// Tests of Region: rlmdb core/reader.rs's tests, plus Go-vs-Go
// cross-process tests with helper processes (this test binary re-executed).
// The tests against C peers are in tests/lockfile.

package lockfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("LOCKFILE_TEST_HELPER"); mode != "" {
		os.Exit(runHelper(mode))
	}
	// Keep the main thread to this goroutine, so no test goroutine runs on
	// it: Go never ends the main thread, so a robust mutex that a goroutine
	// holds there when it exits is never released (TestWriterThreadExit).
	runtime.LockOSThread()
	os.Exit(m.Run())
}

func needLocks(t *testing.T) {
	t.Helper()
	if !sharedLocksAvailable {
		t.Skip("shared locks need cgo on Linux and the BSDs")
	}
}

// testLayouts are layouts of other platforms that this host runs too, in
// addition to its own: on macOS the BSDs' (bsd_darwin_test.go).
var testLayouts []*Layout

// layouts are the layouts the tests run: this platform's, then testLayouts.
func layouts() []*Layout { return append([]*Layout{LockV09, LockV10}, testLayouts...) }

func openRegion(t *testing.T, path string, l *Layout) *Region {
	t.Helper()
	r, err := Open(path, 0o644, 4, false, l)
	if err != nil {
		t.Fatalf("Open %s v%d: %v", path, l.Version, err)
	}
	if r == nil {
		t.Fatal("Open returned no region")
	}
	return r
}

func TestOpenWithoutCgo(t *testing.T) {
	if sharedLocksAvailable {
		t.Skip("this build has shared locks")
	}
	l := &Layout{}
	if _, err := Open(filepath.Join(t.TempDir(), "lock.mdb"), 0o644, 4, false, l); err != ErrNoSharedLocks {
		t.Fatalf("Open: %v, want ErrNoSharedLocks", err)
	}
}

func TestOpenArguments(t *testing.T) {
	needLocks(t)
	path := filepath.Join(t.TempDir(), "lock.mdb")
	if _, err := Open(path, 0o644, 4, false, nil); err != syscall.EINVAL {
		t.Errorf("nil layout: %v", err)
	}
	if _, err := Open(path, 0o644, 0, false, LockV09); err != syscall.EINVAL {
		t.Errorf("0 readers: %v", err)
	}
	// ENOENT; on Windows ERROR_PATH_NOT_FOUND.
	if _, err := Open(filepath.Join(path, "x", "lock.mdb"), 0o644, 4, false, LockV09); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing directory: %v", err)
	}
}

// TestFreshTable is rlmdb's fresh_table, for both versions.
func TestFreshTable(t *testing.T) {
	needLocks(t)
	for _, l := range layouts() {
		path := filepath.Join(t.TempDir(), "lock.mdb")
		r := openRegion(t, path, l)
		if !r.IsExclusive() || r.MaxReaders() != 4 {
			t.Fatalf("v%d: exclusive %v, readers %d", l.Version, r.IsExclusive(), r.MaxReaders())
		}
		if st, err := os.Stat(path); err != nil || st.Size() != l.FileSize(4) {
			t.Fatalf("v%d: size %v %v", l.Version, st.Size(), err)
		}
		if err := r.Downgrade(7); err != nil || r.IsExclusive() || r.TxnID() != 7 {
			t.Fatalf("v%d: downgrade: %v", l.Version, err)
		}
		a, err1 := r.AcquireSlot()
		b, err2 := r.AcquireSlot()
		if a != 0 || b != 1 || err1 != nil || err2 != nil || r.NumReaders() != 2 {
			t.Fatalf("v%d: slots %d %d %v %v", l.Version, a, b, err1, err2)
		}
		if !r.SlotIsIdleAndOurs(a) || r.PublishCurrentTxnID(a) != 7 || r.SlotIsIdleAndOurs(a) {
			t.Fatalf("v%d: publish", l.Version)
		}
		if r.FindOldest(10) != 7 || r.FindOldest(5) != 4 {
			t.Fatalf("v%d: oldest %d", l.Version, r.FindOldest(10))
		}
		list := r.ReaderList()
		if len(list) != 2 || !list[0].InTxn || list[0].TxnID != 7 || list[1].InTxn ||
			list[0].PID != int32(os.Getpid()) || list[0].Thread == 0 {
			t.Fatalf("v%d: reader list %+v", l.Version, list)
		}
		r.ResetSlot(a)
		if !r.SlotIsIdleAndOurs(a) || r.SlotTxnID(a) != IdleTxnID {
			t.Fatalf("v%d: reset", l.Version)
		}
		r.ReleaseSlot(a)
		// A released slot is reused before the table grows.
		if i, err := r.AcquireSlot(); i != 0 || err != nil {
			t.Fatalf("v%d: reuse %d %v", l.Version, i, err)
		}
		r.AcquireSlot()
		r.AcquireSlot()
		if _, err := r.AcquireSlot(); err != format.ReadersFull {
			t.Fatalf("v%d: full: %v", l.Version, err)
		}
		// Our own slots are never stale.
		if n, err := r.ReaderCheck(); n != 0 || err != nil {
			t.Fatalf("v%d: reader check %d %v", l.Version, n, err)
		}
		h := r.Header()
		if h.Magic != Magic || h.Format != l.LockFormat || h.TxnID != 7 || h.NumReaders != 4 ||
			(h.MutexID != 0) != l.Objects.storesHash() {
			t.Fatalf("v%d: header %+v", l.Version, h)
		}
		if err := r.LockWriter(noRepair); err != nil {
			t.Fatalf("v%d: lock writer: %v", l.Version, err)
		}
		if err := r.UnlockWriter(); err != nil {
			t.Fatalf("v%d: unlock writer: %v", l.Version, err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestExclusiveReinitialisesOtherVersion is rlmdb's
// exclusive_opener_reinitialises_other_version.
func TestExclusiveReinitialisesOtherVersion(t *testing.T) {
	needLocks(t)
	path := filepath.Join(t.TempDir(), "lock.mdb")
	for _, l := range []*Layout{LockV09, LockV10, LockV09, LockV10} {
		r := openRegion(t, path, l)
		if !r.IsExclusive() || r.Header().Format != l.LockFormat {
			t.Fatalf("v%d: not reinitialised: %+v", l.Version, r.Header())
		}
		r.Downgrade(5)
		if slot, err := r.AcquireSlot(); slot != 0 || err != nil {
			t.Fatalf("v%d: numreaders not reset: %d %v", l.Version, slot, err)
		}
		if r.PublishCurrentTxnID(0) != 5 || len(r.ReaderList()) != 1 {
			t.Fatalf("v%d: table", l.Version)
		}
		r.Close()
	}
}

// TestLargerFileKeepsItsSlots: an existing table larger than asked for
// gives more slots (mdb_env_setup_locks uses the file's size).
func TestLargerFileKeepsItsSlots(t *testing.T) {
	needLocks(t)
	path := filepath.Join(t.TempDir(), "lock.mdb")
	r, err := Open(path, 0o644, 10, false, LockV09)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	r = openRegion(t, path, LockV09)
	defer r.Close()
	if r.MaxReaders() != 10 {
		t.Fatalf("max readers %d", r.MaxReaders())
	}
}

// --- helper processes -------------------------------------------------------

// runHelper runs a helper process: it opens the lock file of
// LOCKFILE_TEST_PATH with layouts()[LOCKFILE_TEST_LAYOUT], does what mode
// says, prints "READY pid=<pid>" and waits for a line (or EOF) on stdin.
func runHelper(mode string) int {
	i, err := strconv.Atoi(os.Getenv("LOCKFILE_TEST_LAYOUT"))
	if err != nil || i < 0 || i >= len(layouts()) {
		fmt.Fprintln(os.Stderr, "helper: bad LOCKFILE_TEST_LAYOUT")
		return 2
	}
	r, err := Open(os.Getenv("LOCKFILE_TEST_PATH"), 0o644, 8, false, layouts()[i])
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open:", err)
		return 3
	}
	// A helper that is let finish closes the environment, so that the
	// last user removes semaphores (a killed one stands for a crash).
	defer r.Close()
	if r.IsExclusive() {
		r.Downgrade(0)
	}
	switch mode {
	case "hold-env":
	case "hold-reader":
		i, err := r.AcquireSlot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: slot:", err)
			return 3
		}
		r.PublishCurrentTxnID(i)
	case "hold-writer":
		if err := r.LockWriter(func() (uint64, error) { return 0, nil }); err != nil {
			fmt.Fprintln(os.Stderr, "helper: writer:", err)
			return 3
		}
		defer r.UnlockWriter()
	case "hold-rmutex":
		if err := r.lockReaders(); err != nil {
			fmt.Fprintln(os.Stderr, "helper: rmutex:", err)
			return 3
		}
		defer r.unlockReaders()
	default:
		return 2
	}
	fmt.Printf("READY pid=%d\n", os.Getpid())
	bufio.NewReader(os.Stdin).ReadString('\n')
	return 0
}

type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	pid   int
	lines chan string
}

// spawnHelper starts a helper process in mode on the lock file path with
// layout l (one of layouts()), without waiting for it to be ready (see
// ready).
func spawnHelper(t *testing.T, mode, path string, l *Layout) *helper {
	t.Helper()
	i := slices.Index(layouts(), l)
	if i < 0 {
		t.Fatalf("layout %+v is not one of layouts()", l)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LOCKFILE_TEST_HELPER="+mode, "LOCKFILE_TEST_PATH="+path,
		fmt.Sprintf("LOCKFILE_TEST_LAYOUT=%d", i))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helper{cmd: cmd, stdin: stdin, pid: cmd.Process.Pid, lines: make(chan string, 1)}
	t.Cleanup(h.kill)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		h.lines <- line
	}()
	return h
}

// ready waits up to d for the helper's READY line; false on timeout.
func (h *helper) ready(t *testing.T, d time.Duration) bool {
	t.Helper()
	select {
	case line := <-h.lines:
		if !strings.HasPrefix(line, "READY") {
			t.Fatalf("helper: %q", line)
		}
		return true
	case <-time.After(d):
		return false
	}
}

// startHelper starts a helper and waits until it is ready.
func startHelper(t *testing.T, mode, path string, l *Layout) *helper {
	t.Helper()
	h := spawnHelper(t, mode, path, l)
	if !h.ready(t, 30*time.Second) {
		t.Fatalf("helper %s not ready", mode)
	}
	return h
}

// release lets the helper finish and waits for it.
func (h *helper) release(t *testing.T) {
	t.Helper()
	h.stdin.Close()
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
}

// kill kills the helper with SIGKILL, as a crash would.
func (h *helper) kill() {
	if h.cmd.ProcessState == nil {
		h.cmd.Process.Kill()
		h.cmd.Wait()
	}
}

// threadOwned reports whether the locks of layout l belong to the thread
// that took them, and tell the next locker when that thread or its process
// ended holding them: the robust mutexes of Linux and FreeBSD and
// Windows's mutexes.
func threadOwned(l *Layout) bool {
	switch l.Objects {
	case PthreadMutexes, MutexNames, MutexHash:
		return true
	}
	return false
}

// tellsOwnerDeath reports whether the locks of layout l tell the next
// locker about a process that died holding them: the thread-owned ones and
// the SysV semaphores of the other BSDs (SEM_UNDO and the flags in the lock
// file), not named semaphores.
func tellsOwnerDeath(l *Layout) bool { return threadOwned(l) || l.Objects == SysVSemaphores }

// ownerDeathLayouts are the layouts of layouts() whose locks tell about a
// dead owner; the test is skipped if there are none.
func ownerDeathLayouts(t *testing.T) []*Layout {
	t.Helper()
	var ls []*Layout
	for _, l := range layouts() {
		if tellsOwnerDeath(l) {
			ls = append(ls, l)
		}
	}
	if len(ls) == 0 {
		t.Skip("no lock here tells about a dead owner: named semaphores are not robust")
	}
	skipUnderQEMU(t)
	return ls
}

// needThreadOwned skips tests of locks owned by a thread.
func needThreadOwned(t *testing.T) {
	t.Helper()
	if !threadOwned(LockV09) {
		t.Skip("the locks are semaphores, which belong to no thread")
	}
	skipUnderQEMU(t)
}

// noRepair is a LockWriter callback for tests in which no writer dies.
func noRepair() (uint64, error) { return 0, errors.New("unexpected dead owner") }

// blocks reports whether f is still running after d.
func blocks(f func(), d time.Duration) (done chan struct{}, blocked bool) {
	done = make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	select {
	case <-done:
		return done, false
	case <-time.After(d):
		return done, true
	}
}

// TestOpenWithItsSemaphoresRemoved: a lock file that names semaphores which
// do not exist while another process holds it open (damage): the open tries
// again a bounded number of times, then fails with ENOENT (rlmdb
// KNOWN_C_BUGS.md row 47; the race it is for: lmdb TestOpenCloseRace).
func TestOpenWithItsSemaphoresRemoved(t *testing.T) {
	needLocks(t)
	for _, l := range layouts() {
		if !l.Objects.removedByLastUser() {
			continue
		}
		path := filepath.Join(t.TempDir(), "lock.mdb")
		h := startHelper(t, "hold-env", path, l)
		r := openRegion(t, path, l)
		// Remove them, keeping the helper's lock on byte 0.
		r.locks.close(true)
		start := time.Now()
		if got, err := Open(path, 0o644, 4, false, l); err != syscall.ENOENT {
			if err == nil {
				got.Close()
			}
			t.Fatalf("v%d: %v, want ENOENT", l.Version, err)
		}
		if d := time.Since(start); d > 30*time.Second {
			t.Fatalf("v%d: gave up after %v", l.Version, d)
		}
		r.m.unmap()
		r.file.Close()
		h.kill()
	}
}

func TestVersionMismatchWhileHeld(t *testing.T) {
	needLocks(t)
	for _, l := range layouts() {
		other := LockV10
		if l == LockV10 {
			other = LockV09
		}
		path := filepath.Join(t.TempDir(), "lock.mdb")
		h := startHelper(t, "hold-env", path, l)
		if _, err := Open(path, 0o644, 4, false, other); err != format.VersionMismatch {
			t.Fatalf("v%d held, v%d open: %v", l.Version, other.Version, err)
		}
		r := openRegion(t, path, l)
		if r.IsExclusive() {
			t.Fatal("exclusive while the helper holds the file")
		}
		r.Close()

		// A wrong magic is MDB_INVALID (this process holds no record locks
		// on the file, so writing it through another descriptor is fine).
		// Not on Windows: byte 0, the lock byte, cannot be written there
		// while a process holds the environment (locks are mandatory).
		// The magic is cleared through a shared mapping, as the table is
		// always written: on OpenBSD, which has no unified buffer cache,
		// a write(2) is not seen by the mapping the helper keeps (and so
		// not by Open's, which shares its pages), in C as here.
		if runtime.GOOS != "windows" {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			m, err := mapLockFile(f.Fd(), MagicOffset+4)
			if err != nil {
				t.Fatal(err)
			}
			m.storeUint32(MagicOffset, 0)
			m.unmap()
			f.Close()
			if _, err := Open(path, 0o644, 4, false, l); err != format.Invalid {
				t.Fatalf("v%d bad magic: %v", l.Version, err)
			}
		}
		h.release(t)
	}
}

func TestWritersExcludeEachOther(t *testing.T) {
	needLocks(t)
	for _, l := range layouts() {
		path := filepath.Join(t.TempDir(), "lock.mdb")
		r := openRegion(t, path, l)
		r.Downgrade(0)
		h := startHelper(t, "hold-writer", path, l)
		done, blocked := blocks(func() {
			if err := r.LockWriter(noRepair); err != nil {
				t.Error(err)
				return
			}
			if err := r.UnlockWriter(); err != nil {
				t.Error(err)
			}
		}, 300*time.Millisecond)
		if !blocked {
			t.Fatalf("v%d: got the writer lock while another process holds it", l.Version)
		}
		h.release(t)
		<-done

		// And the other way round.
		if err := r.LockWriter(noRepair); err != nil {
			t.Fatal(err)
		}
		h2 := spawnHelper(t, "hold-writer", path, l)
		if h2.ready(t, 300*time.Millisecond) {
			t.Fatalf("v%d: helper got the writer lock while we hold it", l.Version)
		}
		if err := r.UnlockWriter(); err != nil {
			t.Fatal(err)
		}
		if !h2.ready(t, 30*time.Second) {
			t.Fatalf("v%d: helper did not get the writer lock", l.Version)
		}
		h2.release(t)
		r.Close()
	}
}

func TestReaderCheckClearsDeadProcess(t *testing.T) {
	needLocks(t)
	for _, l := range layouts() {
		path := filepath.Join(t.TempDir(), "lock.mdb")
		r := openRegion(t, path, l)
		r.Downgrade(3)
		h := startHelper(t, "hold-reader", path, l)
		list := r.ReaderList()
		if len(list) != 1 || list[0].PID != int32(h.pid) || !list[0].InTxn || list[0].TxnID != 3 {
			t.Fatalf("v%d: reader list %+v", l.Version, list)
		}
		if r.FindOldest(10) != 3 {
			t.Fatalf("v%d: oldest %d", l.Version, r.FindOldest(10))
		}
		// Alive: not cleared.
		if n, err := r.ReaderCheck(); n != 0 || err != nil {
			t.Fatalf("v%d: live reader cleared: %d %v", l.Version, n, err)
		}
		h.kill()
		if n, err := r.ReaderCheck(); n != 1 || err != nil {
			t.Fatalf("v%d: dead reader: %d %v", l.Version, n, err)
		}
		if len(r.ReaderList()) != 0 || r.FindOldest(10) != 9 {
			t.Fatalf("v%d: slot not cleared", l.Version)
		}
		r.Close()
	}
}

// TestWriterOwnerDied: a process killed while holding the writer mutex
// leaves it to the next writer, which repairs as mdb_mutex_failed does
// (robust mutexes, Windows's abandoned mutexes, SysV semaphores; named
// semaphores are not robust).
func TestWriterOwnerDied(t *testing.T) {
	needLocks(t)
	for _, l := range ownerDeathLayouts(t) {
		path := filepath.Join(t.TempDir(), "lock.mdb")
		r := openRegion(t, path, l)
		r.Downgrade(1)
		reader := startHelper(t, "hold-reader", path, l)
		reader.kill()
		h := startHelper(t, "hold-writer", path, l)
		h.kill()
		called := false
		err := r.LockWriter(func() (uint64, error) { called = true; return 9, nil })
		if err != nil || !called || r.TxnID() != 9 {
			t.Fatalf("v%d: lock after dead owner: %v, repair called %v, txnid %d", l.Version, err, called, r.TxnID())
		}
		// The repair also cleared the dead reader.
		if len(r.ReaderList()) != 0 {
			t.Fatalf("v%d: dead reader kept: %+v", l.Version, r.ReaderList())
		}
		if err := r.UnlockWriter(); err != nil {
			t.Fatal(err)
		}
		called = false
		if err := r.LockWriter(func() (uint64, error) { called = true; return 0, nil }); err != nil || called {
			t.Fatalf("v%d: second lock: %v %v", l.Version, err, called)
		}
		r.UnlockWriter()
		r.Close()
	}
}

// TestWriterRepairFailure: a failed repair leaves a robust mutex not
// recoverable, rather than letting writers work on unrepaired state. A
// Windows mutex or a SysV semaphore is released normally, as C releases
// it, so the next writer is not told about the dead owner (rlmdb
// KNOWN_C_BUGS.md row 40).
func TestWriterRepairFailure(t *testing.T) {
	needLocks(t)
	for _, l := range ownerDeathLayouts(t) {
		path := filepath.Join(t.TempDir(), "lock.mdb")
		r := openRegion(t, path, l)
		r.Downgrade(1)
		startHelper(t, "hold-writer", path, l).kill()
		broken := errors.New("cannot read the meta pages")
		if err := r.LockWriter(func() (uint64, error) { return 0, broken }); err != broken {
			t.Fatalf("v%d: repair error: %v", l.Version, err)
		}
		if l.Objects != PthreadMutexes {
			called := false
			if err := r.LockWriter(func() (uint64, error) { called = true; return 0, nil }); err != nil || called {
				t.Fatalf("v%d: after a failed repair: %v, repair called %v", l.Version, err, called)
			}
			r.UnlockWriter()
		} else if err := r.LockWriter(func() (uint64, error) { return 0, nil }); err != errNotRecoverable {
			t.Fatalf("v%d: after a failed repair: %v", l.Version, err)
		}
		r.Close()
	}
}

// TestReaderMutexOwnerDied: a process killed while holding the reader-table
// mutex; the next slot allocation repairs and goes on.
func TestReaderMutexOwnerDied(t *testing.T) {
	needLocks(t)
	for _, l := range ownerDeathLayouts(t) {
		path := filepath.Join(t.TempDir(), "lock.mdb")
		r := openRegion(t, path, l)
		r.Downgrade(1)
		startHelper(t, "hold-reader", path, l).kill()
		startHelper(t, "hold-rmutex", path, l).kill()
		i, err := r.AcquireSlot()
		if err != nil {
			t.Fatalf("v%d: slot after dead owner: %v", l.Version, err)
		}
		// mdb_mutex_failed's reader check freed the dead reader's slot 0.
		if i != 0 || len(r.ReaderList()) != 1 {
			t.Fatalf("v%d: slot %d, readers %+v", l.Version, i, r.ReaderList())
		}
		r.Close()
	}
}

// TestWriterThreadExit: a goroutine that ends while holding the writer lock
// ends its OS thread (it is pinned), and the kernel hands the robust mutex
// (Windows: the abandoned mutex) to the next locker as from a dead
// process, also in this process.
func TestWriterThreadExit(t *testing.T) {
	needLocks(t)
	needThreadOwned(t)
	r := openRegion(t, filepath.Join(t.TempDir(), "lock.mdb"), LockV09)
	r.Downgrade(1)
	done := make(chan error)
	go func() { done <- r.LockWriter(func() (uint64, error) { return 1, nil }) }()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	called := false
	go func() {
		err := r.LockWriter(func() (uint64, error) { called = true; return 2, nil })
		if err == nil {
			err = r.UnlockWriter()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil || !called {
			t.Fatalf("lock after the holder's thread ended: %v, repair called %v", err, called)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the writer lock of an ended thread was not released")
	}
	// The dead goroutine's LockWriter is never matched: the mapping stays.
	r.writers.Add(-1)
	r.Close()
}

// TestUnlockWriterFromAnotherThread: a robust mutex (a Windows mutex)
// belongs to its thread.
func TestUnlockWriterFromAnotherThread(t *testing.T) {
	needLocks(t)
	needThreadOwned(t)
	r := openRegion(t, filepath.Join(t.TempDir(), "lock.mdb"), LockV09)
	defer r.Close()
	r.Downgrade(1)
	if err := r.LockWriter(noRepair); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		errc <- r.UnlockWriter()
	}()
	if err := <-errc; err != errNotOwner {
		t.Fatalf("unlock from another thread: %v", err)
	}
	if err := r.UnlockWriter(); err != nil {
		t.Fatal(err)
	}
}

// TestCloseWhileWriterHeld: Close while this process's writer lock is
// still held (a write transaction ended on another thread: EPERM on
// Linux, ERROR_NOT_OWNER on Windows, and the lock stays with its thread)
// removes the mapping unless the lock is a robust mutex in it, whose
// thread's robust list the kernel reads when the thread exits. On Windows
// the lock file can then be removed (a mapped file cannot).
func TestCloseWhileWriterHeld(t *testing.T) {
	needLocks(t)
	for _, l := range layouts() {
		path := filepath.Join(t.TempDir(), "lock.mdb")
		r := openRegion(t, path, l)
		r.Downgrade(1)
		if err := r.LockWriter(noRepair); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		runtime.UnlockOSThread() // LockWriter's pin
		if kept := r.m.mem != nil; kept != (l.Objects == PthreadMutexes) {
			t.Errorf("v%d (objects %d): mapping kept %v", l.Version, l.Objects, kept)
		}
		if err := os.Remove(path); err != nil {
			t.Errorf("v%d: remove the lock file: %v", l.Version, err)
		}
	}
}
