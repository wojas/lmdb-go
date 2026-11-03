// Tests of the Windows lock-file primitives, ported from rlmdb
// os/windows.rs (lock_map_atomics, byte_locks_are_per_handle,
// process_liveness, named_mutexes), and of the mutex names a region
// creates.

package lockfile

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// errNotOwner is what releasing another thread's mutex returns.
// errNotRecoverable is never returned on Windows: after a failed repair
// the mutex is released normally (TestWriterRepairFailure checks that).
const (
	errNotOwner       = windows.ERROR_NOT_OWNER
	errNotRecoverable = syscall.Errno(0)
)

// TestLockMapAtomics is rlmdb's lock_map_atomics.
func TestLockMapAtomics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.mdb")
	f, err := openLockFile(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(4096); err != nil {
		t.Fatal(err)
	}
	m, err := mapLockFile(f.Fd(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer m.unmap()
	m.storeUint64(64, 0x1122334455667788)
	m.storeInt32(72, -5)
	if m.loadUint64(64) != 0x1122334455667788 || m.loadInt32(72) != -5 {
		t.Fatal("atomics")
	}
	m.writeBytes(8, []byte("Global"))
	if string(m.readBytes(8, 6)) != "Global" {
		t.Fatal("bytes")
	}
	// The view and the file are one (no lock is held on byte 64).
	back := make([]byte, 8)
	if _, err := f.ReadAt(back, 64); err != nil || ne.Uint64(back) != 0x1122334455667788 {
		t.Fatalf("read back %x %v", back, err)
	}
}

// TestByteLocksArePerHandle is rlmdb's byte_locks_are_per_handle: a second
// handle of the file conflicts, the same handle may add a shared lock to
// its exclusive one, and the downgrade leaves only the shared one.
func TestByteLocksArePerHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.mdb")
	a, err := openLockFile(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := openLockFile(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := lockByte(a.Fd(), 0, true, false); err != nil {
		t.Fatal(err)
	}
	if err := lockByte(b.Fd(), 0, false, false); !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("shared lock next to an exclusive one: %v", err)
	}
	if err := downgradeLock(a.Fd()); err != nil {
		t.Fatal(err)
	}
	if err := lockByte(b.Fd(), 0, false, false); err != nil {
		t.Fatalf("shared lock next to a shared one: %v", err)
	}
	if err := lockByte(b.Fd(), 0, true, false); err == nil {
		t.Fatal("exclusive lock next to a shared one")
	}
	unlockLockFile(a.Fd())
	unlockLockFile(b.Fd())
	if err := lockByte(b.Fd(), 0, true, false); err != nil {
		t.Fatalf("exclusive lock after the unlocks: %v", err)
	}
	unlockLockFile(b.Fd())
	// No sharing for deletion while open.
	if err := os.Remove(path); err == nil {
		t.Fatal("removed an open lock file")
	}
}

// TestProcessLiveness is rlmdb's process_liveness: a process that has
// exited is dead even while a handle to it is open (C would say alive,
// rlmdb KNOWN_C_BUGS.md row 42).
func TestProcessLiveness(t *testing.T) {
	if !pidAlive(0, int32(os.Getpid())) {
		t.Fatal("this process is dead")
	}
	cmd := exec.Command("cmd", "/C", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Our own handle, kept open past the exit (cmd.Wait closes its own).
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if pidAlive(0, int32(pid)) {
		t.Fatal("an exited process with an open handle counts as alive")
	}
}

// TestNamedMutexes is rlmdb's named_mutexes: a mutex created by one handle
// and opened by another; a thread that ends holding it abandons it.
func TestNamedMutexes(t *testing.T) {
	name := fmt.Sprintf(`Global\lmdbgo-test-%d`, os.Getpid())
	a, err := createMutex(name)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close(false)
	b, err := openMutex(name)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close(false)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if st, err := a.lock(); st != lockAcquired || err != nil {
		t.Fatalf("lock: %v %v", st, err)
	}
	if err := a.unlock(); err != nil {
		t.Fatal(err)
	}
	// A goroutine pinned to its thread that ends holding the mutex ends the
	// thread, which abandons it.
	done := make(chan error)
	go func() {
		runtime.LockOSThread()
		_, err := b.lock()
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st, err := a.lock(); st != lockOwnerDied || err != nil {
		t.Fatalf("lock after the owner's thread ended: %v %v", st, err)
	}
	if err := a.consistent(); err != nil {
		t.Fatal(err)
	}
	if err := a.unlock(); err != nil {
		t.Fatal(err)
	}
}

// TestRegionMutexNames: the exclusive opener names the mutexes as C does,
// from the lock file's identity (version 1: names stored; version 2: the
// hash stored), and the kernel removes them with the last handle.
func TestRegionMutexNames(t *testing.T) {
	for _, l := range layouts() {
		path := filepath.Join(t.TempDir(), "lock.mdb")
		r := openRegion(t, path, l)
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(windows.Handle(r.fd), &info); err != nil {
			t.Fatal(err)
		}
		id := lockFileIDWin32(info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
		h := r.Header()
		rname, wname := mutexNamesV09(id)
		switch l.Objects {
		case MutexNames:
			if string(h.RName) != rname || string(h.WName) != wname || h.Format != 0x1 {
				t.Fatalf("v1 header %+v, want names %q %q", h, rname, wname)
			}
		case MutexHash:
			if h.MutexID != hashVal(id, hashInit) {
				t.Fatalf("v2 mutex id %#x", h.MutexID)
			}
			rname, wname = mutexNameV10(h.MutexID, 'r'), mutexNameV10(h.MutexID, 'w')
		default:
			t.Fatalf("objects %d", l.Objects)
		}
		for _, n := range []string{rname, wname} {
			m, err := openMutex(n)
			if err != nil {
				t.Fatalf("v%d: %s not there: %v", l.Version, n, err)
			}
			m.close(false)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := openMutex(rname); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			t.Fatalf("v%d: %s still there: %v", l.Version, rname, err)
		}
	}
}
