//go:build unix

// Region tests of the Unix lock objects.

package lockfile

import (
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// errNotOwner is what unlocking another thread's robust mutex returns;
// errNotRecoverable what locking one returns after a failed repair.
const (
	errNotOwner       = syscall.EPERM
	errNotRecoverable = syscall.ENOTRECOVERABLE
)

// TestSemaphoresRemovedByLastUser: on macOS the last user's Close unlinks
// the semaphores (mdb_env_close0).
func TestSemaphoresRemovedByLastUser(t *testing.T) {
	needLocks(t)
	if runtime.GOOS != "darwin" {
		t.Skip("named semaphores are macOS's")
	}
	for _, l := range layouts() {
		r := openRegion(t, filepath.Join(t.TempDir(), "lock.mdb"), l)
		r.Downgrade(0)
		h := r.Header()
		rname, wname := string(h.RName), string(h.WName)
		if l.Objects == SemaphoreHash {
			rname, wname = semNameV10(h.MutexID, 'r'), semNameV10(h.MutexID, 'w')
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
}
