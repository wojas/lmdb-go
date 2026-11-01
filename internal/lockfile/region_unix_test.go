//go:build unix

// Region tests of the Unix lock objects.

package lockfile

import (
	"path/filepath"
	"syscall"
	"testing"
)

// errNotOwner is what unlocking another thread's robust mutex returns
// (errNotRecoverable, what locking one returns after a failed repair, is
// in robust_errno_test.go).
const errNotOwner = syscall.EPERM

// TestSemaphoresRemovedByLastUser: with named semaphores (macOS; version 1
// on OpenBSD, NetBSD and DragonFly) the last user's Close unlinks them
// (mdb_env_close0).
func TestSemaphoresRemovedByLastUser(t *testing.T) {
	needLocks(t)
	ran := false
	for _, l := range layouts() {
		if l.Objects != SemaphoreNames && l.Objects != SemaphoreHash {
			continue
		}
		ran = true
		r := openRegion(t, filepath.Join(t.TempDir(), "lock.mdb"), l)
		r.Downgrade(0)
		h := r.Header()
		rname, wname := string(h.RName), string(h.WName)
		if l.Objects == SemaphoreHash {
			rname, wname = semNameV10(h.MutexID, 'r'), semNameV10(h.MutexID, 'w')
		} else if want := 15 - b2i(l.shortSemNames); len(rname) != want || len(wname) != want {
			// "/MDBr" and ten characters of the hash, nine on NetBSD.
			t.Fatalf("v%d: names %q %q, want %d characters", l.Version, rname, wname, want)
		}
		for _, n := range []string{rname, wname} {
			s, err := openSemaphore(n)
			if err != nil {
				t.Fatalf("v%d: %s not there: %v", l.Version, n, err)
			}
			s.close(false)
		}
		r.Close()
		if _, err := openSemaphore(rname); err != syscall.ENOENT {
			t.Fatalf("v%d: %s still there: %v", l.Version, rname, err)
		}
	}
	if !ran {
		t.Skip("no named semaphores here")
	}
}
