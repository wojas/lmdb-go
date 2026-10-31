// Tests of macOS's semaphore calls (sem_darwin.go), against the
// implementation the build has: cgo (sem_cgo_darwin.go) by default, the
// libSystem stubs (sem_libsystem_darwin.go) with CGO_ENABLED=0. Both must
// pass the same tests. The cross-process tests against C are in
// tests/lockfile.

package lockfile

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// semValueMax is macOS's SEM_VALUE_MAX.
const semValueMax = 32767

// init runs the helper processes of TestSemWaitEINTR and TestSemHandleZero
// (this test binary re-executed). Package initialisation runs on the main
// thread, and LockOSThread keeps the helper there: macOS delivers a signal
// sent to the process to its first thread that does not block it, the main
// thread, so the helper's sem_wait is the call the signal interrupts.
func init() {
	mode := os.Getenv("LOCKFILE_SEM_HELPER")
	if mode == "" {
		return
	}
	runtime.LockOSThread()
	os.Exit(semHelper(mode, os.Getenv("LOCKFILE_SEM_NAME")))
}

var semSeq atomic.Int32

// semName returns a fresh semaphore name, removed when the test ends.
func semName(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("/lgo%d.%d", os.Getpid(), semSeq.Add(1))
	semUnlink(name)
	t.Cleanup(func() { semUnlink(name) })
	return name
}

// mustCreate creates name with mode and value; the handle is closed when
// the test ends.
func mustCreate(t *testing.T, name string, mode, value uint32) uintptr {
	t.Helper()
	h, e := semCreate(name, mode, value)
	if e != 0 {
		t.Fatalf("sem_open(%s, O_CREAT|O_EXCL, %#o, %d): %v", name, mode, value, e)
	}
	t.Cleanup(func() { semClose(h) })
	return h
}

// takeAll takes h with sem_trywait until it fails, at most max+1 times,
// and returns how often it succeeded: the value, since macOS has no
// working sem_getvalue. The failure must be EAGAIN.
func takeAll(t *testing.T, h uintptr, max int) int {
	t.Helper()
	for n := 0; n <= max; n++ {
		if e := semTryWait(h); e != 0 {
			if e != syscall.EAGAIN {
				t.Fatalf("sem_trywait after %d: %v, want EAGAIN", n, e)
			}
			return n
		}
	}
	return max + 1
}

// TestSemCreateValue: the initial value reaches sem_open (a variadic
// argument; on arm64 a stack slot of the libSystem trampoline).
func TestSemCreateValue(t *testing.T) {
	t.Logf("implementation: %s, %s", semImpl, runtime.GOARCH)
	for _, v := range []uint32{0, 1, 2, 5, 100, semValueMax} {
		h := mustCreate(t, semName(t), 0o600, v)
		if n := takeAll(t, h, int(v)); n != int(v) {
			t.Errorf("created with value %d: sem_trywait succeeded %d times", v, n)
		}
	}
	if _, e := semCreate(semName(t), 0o600, semValueMax+1); e != syscall.EINVAL {
		t.Errorf("value SEM_VALUE_MAX+1: %v, want EINVAL", e)
	}
}

// TestCreateSemaphoreValueOne: LMDB's semaphores start at 1, so the first
// lock is free and a second one has to wait (mdb_env_setup_locks).
func TestCreateSemaphoreValueOne(t *testing.T) {
	name := semName(t)
	s, err := createSemaphore(name, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if n := takeAll(t, s.h, 5); n != 1 {
		t.Fatalf("value %d, want 1", n)
	}
	if err := s.unlock(); err != nil {
		t.Fatal(err)
	}
	if st, err := s.lock(); err != nil || st != lockAcquired {
		t.Fatalf("lock: %v %v", st, err)
	}
	if e := semTryWait(s.h); e != syscall.EAGAIN {
		t.Fatalf("taken twice: %v", e)
	}
	s.close(true)
	s.close(true) // a second close does nothing
	if _, e := semOpen(name); e != syscall.ENOENT {
		t.Fatalf("after close with unlink: %v, want ENOENT", e)
	}
}

// TestSemCreateMode: the mode reaches sem_open (the other variadic
// argument). macOS checks it when a semaphore is opened again:
// sem_open(name, 0) needs read permission, which XNU (posix_cred_access)
// grants the creator if the owner bits allow it or, since the creator is
// in the semaphore's group, the group bits; the other bits do not count
// for a group member. (Root passes every check.)
func TestSemCreateMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not subject to the mode")
	}
	for _, c := range []struct {
		mode uint32
		want syscall.Errno
	}{
		{0o600, 0}, {0o644, 0}, {0o400, 0}, {0o040, 0},
		{0o000, syscall.EACCES}, {0o001, syscall.EACCES}, {0o004, syscall.EACCES}, {0o200, syscall.EACCES},
		{0o300, syscall.EACCES}, {0o022, syscall.EACCES}, {0o333, syscall.EACCES},
	} {
		name := semName(t)
		mustCreate(t, name, c.mode, 1)
		h, e := semOpen(name)
		if e != c.want {
			t.Errorf("mode %#o: sem_open(name, 0): %v, want %v", c.mode, e, c.want)
		}
		if e == 0 {
			semClose(h)
		}
	}
}

// TestSemErrors: errno comes back from each call.
func TestSemErrors(t *testing.T) {
	name := semName(t)
	h, e := semCreate(name, 0o600, 1) // closed below, not at cleanup
	if e != 0 {
		t.Fatal(e)
	}
	if _, e := semCreate(name, 0o600, 1); e != syscall.EEXIST {
		t.Errorf("create existing: %v, want EEXIST", e)
	}
	missing := semName(t)
	if _, e := semOpen(missing); e != syscall.ENOENT {
		t.Errorf("open missing: %v, want ENOENT", e)
	}
	if e := semUnlink(missing); e != syscall.ENOENT {
		t.Errorf("unlink missing: %v, want ENOENT", e)
	}
	long := "/" + strings.Repeat("x", 40) // PSEMNAMLEN is 31
	if _, e := semCreate(long, 0o600, 1); e != syscall.ENAMETOOLONG {
		t.Errorf("create long name: %v, want ENAMETOOLONG", e)
	}
	if _, e := semOpen(long); e != syscall.ENAMETOOLONG {
		t.Errorf("open long name: %v, want ENAMETOOLONG", e)
	}
	if e := semUnlink(long); e != syscall.ENAMETOOLONG {
		t.Errorf("unlink long name: %v, want ENAMETOOLONG", e)
	}
	for _, f := range []func() syscall.Errno{
		func() syscall.Errno { _, e := semCreate("/a\x00b", 0o600, 1); return e },
		func() syscall.Errno { _, e := semOpen("/a\x00b"); return e },
		func() syscall.Errno { return semUnlink("/a\x00b") },
	} {
		if e := f(); e != syscall.EINVAL {
			t.Errorf("name with NUL: %v, want EINVAL", e)
		}
	}
	if e := semClose(h); e != 0 {
		t.Fatalf("close: %v", e)
	}
	// h is closed, so its descriptor number is no handle (nothing can
	// have taken the number since: the test opens no files).
	if e := semClose(h); e != syscall.EBADF {
		t.Errorf("close closed handle: %v, want EBADF", e)
	}
	if e := semPost(h); e != syscall.EBADF {
		t.Errorf("post closed handle: %v, want EBADF", e)
	}
	if e := semTryWait(h); e != syscall.EBADF {
		t.Errorf("trywait closed handle: %v, want EBADF", e)
	}
	if e := semWait(h); e != syscall.EBADF {
		t.Errorf("wait closed handle: %v, want EBADF", e)
	}
}

// TestSemSharedByName: handles of one name are one semaphore; unlinking
// removes the name, not the semaphore, and the name can then be created
// afresh (what mdb_env_setup_locks's sem_unlink before sem_open relies on).
func TestSemSharedByName(t *testing.T) {
	name := semName(t)
	h1 := mustCreate(t, name, 0o600, 0)
	h2, e := semOpen(name)
	if e != 0 {
		t.Fatal(e)
	}
	defer semClose(h2)
	if h1 == h2 {
		t.Fatalf("two opens gave one handle %d", h1)
	}
	if e := semPost(h1); e != 0 {
		t.Fatal(e)
	}
	if n := takeAll(t, h2, 3); n != 1 {
		t.Fatalf("post on one handle, %d taken on the other", n)
	}
	if e := semUnlink(name); e != 0 {
		t.Fatal(e)
	}
	if e := semPost(h2); e != 0 {
		t.Fatalf("post after unlink: %v", e)
	}
	if n := takeAll(t, h1, 3); n != 1 {
		t.Fatalf("after unlink: %d taken", n)
	}
	if _, e := semOpen(name); e != syscall.ENOENT {
		t.Fatalf("open after unlink: %v, want ENOENT", e)
	}
	h3 := mustCreate(t, name, 0o600, 2)
	semPost(h1)
	if n := takeAll(t, h3, 5); n != 2 {
		t.Fatalf("new semaphore of the old name: %d taken, want 2", n)
	}
}

// TestSemLockBlocks: lock waits for an unlock through another handle.
func TestSemLockBlocks(t *testing.T) {
	name := semName(t)
	a, err := createSemaphore(name, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close(false)
	b, err := openSemaphore(name)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close(false)
	if _, err := a.lock(); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		_, err := b.lock()
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("second lock did not wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := a.unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lock not granted after unlock")
	}
	if err := b.unlock(); err != nil {
		t.Fatal(err)
	}
}

// semProc is a helper process started by startSemHelper.
type semProc struct {
	cmd   *exec.Cmd
	lines chan string
}

func startSemHelper(t *testing.T, mode, name string) *semProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LOCKFILE_SEM_HELPER="+mode, "LOCKFILE_SEM_NAME="+name)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &semProc{cmd: cmd, lines: make(chan string, 16)}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return p
}

// next returns the helper's next line, failing on EOF or timeout.
func (p *semProc) next(t *testing.T) string {
	t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			t.Fatal("helper exited")
		}
		return line
	case <-time.After(10 * time.Second):
		t.Fatal("helper timed out")
	}
	return ""
}

func (p *semProc) expect(t *testing.T, want string) {
	t.Helper()
	if line := p.next(t); line != want {
		t.Fatalf("helper said %q, want %q", line, want)
	}
}

func (p *semProc) signal(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
}

// TestSemWaitEINTR: a signal handled while sem_wait waits makes it fail
// with EINTR (Go installs its handlers with SA_RESTART, which does not
// restart sem_wait on macOS), and errno arrives intact; lock retries, as
// mdb_sem_wait does, and only returns once the semaphore is posted.
func TestSemWaitEINTR(t *testing.T) {
	name := semName(t)
	h := mustCreate(t, name, 0o600, 0)
	p := startSemHelper(t, "eintr", name)
	p.expect(t, "WAIT-ONCE")
	// A signal that comes before the helper is in sem_wait is just
	// handled: repeat until one interrupts the wait.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(10 * time.Second)
wait:
	for {
		select {
		case line, ok := <-p.lines:
			if !ok || line != "EINTR" {
				t.Fatalf("helper: %q (open %v), want EINTR", line, ok)
			}
			break wait
		case <-tick.C:
			p.signal(t)
		case <-timeout:
			t.Fatal("sem_wait was never interrupted")
		}
	}
	p.expect(t, "WAIT-LOCK")
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 10; i++ {
		p.signal(t)
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case line := <-p.lines:
		t.Fatalf("lock returned before the post: %q", line)
	default:
	}
	if e := semPost(h); e != 0 {
		t.Fatal(e)
	}
	if line := p.next(t); !strings.HasPrefix(line, "ACQUIRED ") {
		t.Fatalf("helper: %q, want ACQUIRED", line)
	} else {
		t.Logf("helper: %s", line)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

// TestSemHandleZero: a semaphore handle is a descriptor number, so with
// descriptor 0 closed sem_open returns 0, a valid handle; failure is told
// by errno alone.
func TestSemHandleZero(t *testing.T) {
	p := startSemHelper(t, "fd0", semName(t))
	line := p.next(t)
	if line != "HANDLE 0" {
		t.Fatalf("helper: %q, want HANDLE 0", line)
	}
	p.expect(t, "OK")
	if err := p.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

// semHelper is the helper process's side; it prints its progress and
// returns the exit code.
func semHelper(mode, name string) int {
	fail := func(format string, args ...any) int {
		fmt.Printf("ERR "+format+"\n", args...)
		return 1
	}
	switch mode {
	case "eintr":
		sig := make(chan os.Signal, 64)
		signal.Notify(sig, syscall.SIGUSR1)
		h, e := semOpen(name)
		if e != 0 {
			return fail("open: %v", e)
		}
		fmt.Println("WAIT-ONCE")
		if e := semWait(h); e != syscall.EINTR {
			return fail("first sem_wait: %d (%v), want EINTR", int(e), e)
		}
		fmt.Println("EINTR")
		fmt.Println("WAIT-LOCK")
		s := &sharedMutex{h: h, open: true, name: name}
		if _, err := s.lock(); err != nil {
			return fail("lock: %v", err)
		}
		fmt.Printf("ACQUIRED signals=%d\n", len(sig))
		return 0
	case "fd0":
		if err := syscall.Close(0); err != nil {
			return fail("close 0: %v", err)
		}
		s, err := createSemaphore(name, 0o600)
		if err != nil {
			return fail("create: %v", err)
		}
		fmt.Printf("HANDLE %d\n", s.h)
		if st, err := s.lock(); err != nil || st != lockAcquired {
			return fail("lock: %v %v", st, err)
		}
		if e := semTryWait(s.h); e != syscall.EAGAIN {
			return fail("taken twice: %v", e)
		}
		if err := s.unlock(); err != nil {
			return fail("unlock: %v", err)
		}
		s.close(true)
		if _, e := semOpen(name); e != syscall.ENOENT {
			return fail("not removed by close: %v", e)
		}
		fmt.Println("OK")
		return 0
	}
	return fail("unknown mode %q", mode)
}

// TestThreadID: mr_tid is pthread_self(): the same on one thread, not 0,
// different on two threads.
func TestThreadID(t *testing.T) {
	ids := make(chan [2]uint64, 2)
	release := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			ids <- [2]uint64{threadID(), threadID()}
			<-release // both threads exist at once
		}()
	}
	a, b := <-ids, <-ids
	close(release)
	if a[0] == 0 || a[0] != a[1] || b[0] != b[1] {
		t.Fatalf("thread ids %x %x", a, b)
	}
	if a[0] == b[0] {
		t.Fatalf("two threads, one id %x", a[0])
	}
}
