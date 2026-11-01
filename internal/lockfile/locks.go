// The pair of shared locks of an environment, created or attached as the
// lock-file version says: the lock part of mdb_env_setup_locks and of
// mdb_env_close0 in mdb.c (0.9.36 lines 4964-5079, 5250-5270; 1.0 lines
// 6064-6165); rlmdb os/shared_lock.rs and os/windows.rs (SharedLocks).
// createLocks and attachLocks are per OS: locks_unix.go (pthread mutexes
// and named semaphores, whose libc calls are in shared_cgo.go and
// sem_darwin.go) and locks_windows.go (named mutexes).

package lockfile

import "syscall"

// lockStatus is the outcome of a successful lock.
type lockStatus int

const (
	// lockAcquired: taken normally.
	lockAcquired lockStatus = iota
	// lockOwnerDied: taken, but the previous owner died holding it
	// (EOWNERDEAD of a robust mutex; WAIT_ABANDONED of a Windows mutex).
	// The protected state must be repaired and the mutex marked consistent
	// before it is unlocked; unlocking a robust mutex without that makes it
	// unusable for everyone (ENOTRECOVERABLE).
	lockOwnerDied
)

// sharedLocks are an environment's reader-table lock (me_rmutex, guards
// slot allocation) and writer lock (me_wmutex, held for a whole write
// transaction).
type sharedLocks struct {
	r, w *sharedMutex
}

// readName reads the NUL-terminated semaphore or mutex name in the n bytes
// at off; EINVAL if there is no NUL.
func readName(m *lockMap, off, n int) (string, error) {
	b := m.readBytes(off, n)
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), nil
		}
	}
	return "", syscall.EINVAL
}

// close releases this process's handles. With semaphores, the last user of
// the environment (lastUser: it could take the exclusive file lock) also
// removes their names, as mdb_env_close0 does (Windows frees a named mutex
// with its last handle).
func (s *sharedLocks) close(lastUser bool) {
	s.r.close(lastUser)
	s.w.close(lastUser)
}
