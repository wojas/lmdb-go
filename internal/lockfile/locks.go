// The pair of shared locks of an environment, created or attached as the
// lock-file version says: the lock part of mdb_env_setup_locks and of
// mdb_env_close0 in mdb.c (0.9.36 lines 4964-5079, 5250-5270; 1.0 lines
// 6064-6165); rlmdb os/shared_lock.rs (SharedLocks). The libc calls are in
// shared_cgo.go.

package lockfile

import (
	"os"
	"syscall"
)

// lockStatus is the outcome of a successful lock.
type lockStatus int

const (
	// lockAcquired: taken normally.
	lockAcquired lockStatus = iota
	// lockOwnerDied: taken, but the previous owner died holding it
	// (EOWNERDEAD; robust mutexes only). The protected state must be
	// repaired and the mutex marked consistent before it is unlocked;
	// unlocking it without that makes it unusable for everyone
	// (ENOTRECOVERABLE).
	lockOwnerDied
)

// sharedLocks are an environment's reader-table lock (me_rmutex, guards
// slot allocation) and writer lock (me_wmutex, held for a whole write
// transaction).
type sharedLocks struct {
	r, w *sharedMutex
}

// createLocks creates both locks for a lock file of layout l that this
// process holds exclusively, and records in it what other openers need to
// find them (mdb_env_setup_locks, *excl > 0): pthread mutexes initialised
// in place (Linux), or semaphores whose names (version 1) or name hash
// (version 2) are stored (macOS).
func createLocks(m *lockMap, fd int, mode os.FileMode, l *Layout) (*sharedLocks, error) {
	switch l.Objects {
	case PthreadMutexes:
		if l.WLock < 0 || l.RLockSize != mutexSize {
			return nil, syscall.EINVAL
		}
		r, err := initMutex(m, l.RLock)
		if err != nil {
			return nil, err
		}
		w, err := initMutex(m, l.WLock)
		if err != nil {
			return nil, err
		}
		return &sharedLocks{r: r, w: w}, nil
	case SemaphoreNames, SemaphoreHash:
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			return nil, err
		}
		id := lockFileID(uint64(st.Dev), st.Ino)
		var rname, wname string
		if l.Objects == SemaphoreNames {
			if l.WLock < 0 {
				return nil, syscall.EINVAL
			}
			rname, wname = semNamesV09(id)
			if len(rname)+1 > l.RLockSize {
				return nil, syscall.EINVAL
			}
			m.writeBytes(l.RLock, append([]byte(rname), 0))
			m.writeBytes(l.WLock, append([]byte(wname), 0))
		} else {
			if l.RLockSize != 8 {
				return nil, syscall.EINVAL
			}
			mutexID := hashVal(id, hashInit)
			m.storeUint64(l.RLock, mutexID)
			rname, wname = semNameV10(mutexID, 'r'), semNameV10(mutexID, 'w')
		}
		// Clean up after a previous run, if needed: remove both
		// semaphores before anything else (failure is fine).
		_ = unlinkSemaphore(rname)
		_ = unlinkSemaphore(wname)
		r, err := createSemaphore(rname, uint32(mode.Perm()))
		if err != nil {
			return nil, err
		}
		w, err := createSemaphore(wname, uint32(mode.Perm()))
		if err != nil {
			r.close(false)
			return nil, err
		}
		return &sharedLocks{r: r, w: w}, nil
	}
	return nil, syscall.EINVAL
}

// attachLocks uses the locks another process created: the mutexes in the
// file, or the semaphores named in it (version 1) or after the hash stored
// in it (version 2). What is in the file is read, never recomputed.
func attachLocks(m *lockMap, l *Layout) (*sharedLocks, error) {
	switch l.Objects {
	case PthreadMutexes:
		if l.WLock < 0 || l.RLockSize != mutexSize {
			return nil, syscall.EINVAL
		}
		r, err := attachMutex(m, l.RLock)
		if err != nil {
			return nil, err
		}
		w, err := attachMutex(m, l.WLock)
		if err != nil {
			return nil, err
		}
		return &sharedLocks{r: r, w: w}, nil
	case SemaphoreNames, SemaphoreHash:
		var rname, wname string
		if l.Objects == SemaphoreNames {
			if l.WLock < 0 {
				return nil, syscall.EINVAL
			}
			var err error
			if rname, err = readName(m, l.RLock, l.RLockSize); err != nil {
				return nil, err
			}
			if wname, err = readName(m, l.WLock, l.RLockSize); err != nil {
				return nil, err
			}
		} else {
			if l.RLockSize != 8 {
				return nil, syscall.EINVAL
			}
			mutexID := m.loadUint64(l.RLock)
			rname, wname = semNameV10(mutexID, 'r'), semNameV10(mutexID, 'w')
		}
		r, err := openSemaphore(rname)
		if err != nil {
			return nil, err
		}
		w, err := openSemaphore(wname)
		if err != nil {
			r.close(false)
			return nil, err
		}
		return &sharedLocks{r: r, w: w}, nil
	}
	return nil, syscall.EINVAL
}

// readName reads the NUL-terminated semaphore name in the n bytes at off;
// EINVAL if there is no NUL.
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
// removes their names, as mdb_env_close0 does.
func (s *sharedLocks) close(lastUser bool) {
	s.r.close(lastUser)
	s.w.close(lastUser)
}
