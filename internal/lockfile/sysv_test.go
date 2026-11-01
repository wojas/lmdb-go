//go:build (darwin || openbsd || netbsd || dragonfly) && cgo

// Tests of the SysV semaphore set (sysv.go), on OpenBSD, NetBSD and
// DragonFly and on macOS: rlmdb os/shared_lock.rs sysv::tests (protocol).
// The cross-process cases (a holder that dies, the last user removing the
// set) run in region_test.go and below with the version 2 layout of these
// BSDs, which layouts() includes on all four systems.

package lockfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// sysvTestLayout is the version 2 layout of OpenBSD, NetBSD and DragonFly:
// mtb_semid at 20, mtb_rlocked at 24, mt2_wlocked at 64.
func sysvTestLayout(t *testing.T) *Layout {
	t.Helper()
	_, l := semBSD.layouts()
	if l.Objects != SysVSemaphores || l.RLock != 20 || l.WLock != 64 {
		t.Fatalf("layout %+v", l)
	}
	return l
}

// mapTestFile maps a new file of size bytes in the test's directory.
func mapTestFile(t *testing.T, size int) (*os.File, *lockMap) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "lock.mdb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := f.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	m, err := mapLockFile(f.Fd(), size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.unmap() })
	return f, m
}

// TestSysVProtocol: create, lock and unlock with the flags, a second
// opener through the stored id, exclusion, removal.
func TestSysVProtocol(t *testing.T) {
	l := sysvTestLayout(t)
	f, m := mapTestFile(t, int(l.FileSize(1)))
	// Values a previous table left.
	m.storeInt32(l.RLock+cIntSize, 1)
	m.storeInt32(l.WLock, 1)
	locks, err := createSysVLocks(m, f.Fd(), 0o600, l)
	if err != nil {
		t.Fatal(err)
	}
	r, w := locks.r.sysv, locks.w.sysv
	t.Cleanup(func() { sysvRemove(r.semid) })
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	t.Logf("key %#x, semid %d", SysVKey(uint64(st.Dev), st.Ino), r.semid)
	if m.loadInt32(l.RLock) != r.semid || w.semid != r.semid || r.semnum != sysvReader || w.semnum != sysvWriter {
		t.Fatalf("set %d stored as %d; semaphores %d, %d", r.semid, m.loadInt32(l.RLock), r.semnum, w.semnum)
	}
	if m.loadInt32(l.RLock+cIntSize) != 0 || m.loadInt32(l.WLock) != 0 {
		t.Fatal("flags not cleared")
	}

	if st, err := locks.r.lock(); st != lockAcquired || err != nil {
		t.Fatalf("lock: %v %v", st, err)
	}
	if m.loadInt32(l.RLock+cIntSize) != 1 {
		t.Fatal("rlocked not set")
	}
	if err := locks.r.unlock(); err != nil || m.loadInt32(l.RLock+cIntSize) != 0 {
		t.Fatalf("unlock: %v, rlocked %d", err, m.loadInt32(l.RLock+cIntSize))
	}

	// Another opener finds the set through the stored id.
	other, err := attachSysVLocks(m, l)
	if err != nil {
		t.Fatal(err)
	}
	if other.w.sysv.semid != w.semid {
		t.Fatalf("attached to %d, not %d", other.w.sysv.semid, w.semid)
	}
	if st, err := other.w.lock(); st != lockAcquired || err != nil || m.loadInt32(l.WLock) != 1 {
		t.Fatalf("lock: %v %v, wlocked %d", st, err, m.loadInt32(l.WLock))
	}
	// The writer semaphore excludes; the reader one is separate.
	done, blocked := blocks(func() {
		if _, err := locks.w.lock(); err != nil {
			t.Error(err)
		}
	}, 200*time.Millisecond)
	if !blocked {
		t.Fatal("got the writer semaphore while it is held")
	}
	if st, err := locks.r.lock(); st != lockAcquired || err != nil {
		t.Fatalf("reader lock: %v %v", st, err)
	}
	locks.r.unlock()
	if err := other.w.unlock(); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := locks.w.unlock(); err != nil {
		t.Fatal(err)
	}

	// A flag left set is a dead holder's (mdb_sem_wait).
	m.storeInt32(l.WLock, 1)
	if st, err := locks.w.lock(); st != lockOwnerDied || err != nil {
		t.Fatalf("lock after a dead holder: %v %v", st, err)
	}
	locks.w.unlock()
	if st, err := locks.w.lock(); st != lockAcquired || err != nil {
		t.Fatalf("lock after the repair: %v %v", st, err)
	}
	locks.w.unlock()

	// The last user removes the set, once, through the reader semaphore.
	locks.w.close(true)
	if e := sysvCheck(r.semid); e != 0 {
		t.Fatalf("the writer's close removed the set: %v", e)
	}
	locks.r.close(true)
	if e := sysvCheck(r.semid); e == 0 {
		t.Fatal("the set is still there")
	}
	if _, err := attachSysVLocks(m, l); err == nil {
		t.Fatal("attached to a removed set")
	}
}

// TestSysVBadLayout: a layout without room for the id and the flags is
// refused.
func TestSysVBadLayout(t *testing.T) {
	l := *sysvTestLayout(t)
	f, m := mapTestFile(t, int(l.FileSize(1)))
	l.WLock = -1
	if _, err := createSysVLocks(m, f.Fd(), 0o600, &l); err != syscall.EINVAL {
		t.Fatalf("create: %v", err)
	}
	if _, err := attachSysVLocks(m, &l); err != syscall.EINVAL {
		t.Fatalf("attach: %v", err)
	}
}

// TestSysVSetRemovedByLastUser: Close removes the set only when no other
// process has the environment open (mdb_env_close0), as with named
// semaphores.
func TestSysVSetRemovedByLastUser(t *testing.T) {
	needLocks(t)
	for _, l := range layouts() {
		if l.Objects != SysVSemaphores {
			continue
		}
		path := filepath.Join(t.TempDir(), "lock.mdb")
		r := openRegion(t, path, l)
		r.Downgrade(0)
		semid := r.m.loadInt32(l.RLock)
		h := startHelper(t, "hold-env", path, l)
		r.Close()
		if e := sysvCheck(semid); e != 0 {
			t.Fatalf("set removed while another process uses it: %v", e)
		}
		r = openRegion(t, path, l)
		if r.IsExclusive() || r.m.loadInt32(l.RLock) != semid {
			t.Fatal("not attached to the helper's set")
		}
		h.release(t)
		r.Close()
		if e := sysvCheck(semid); e == 0 {
			sysvRemove(semid)
			t.Fatal("the last user left the set")
		}
	}
}
