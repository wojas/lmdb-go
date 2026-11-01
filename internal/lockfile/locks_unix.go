//go:build unix

// createLocks and attachLocks on Unix: process-shared robust pthread
// mutexes in the lock file (Linux, FreeBSD), named POSIX semaphores (macOS;
// OpenBSD, NetBSD and DragonFly in version 1) or a SysV semaphore set
// (OpenBSD, NetBSD and DragonFly in version 2), as mdb_env_setup_locks
// makes and finds them (0.9.36 lines 4964-5079, 1.0 lines 6064-6182).

package lockfile

import (
	"os"
	"syscall"
	"unsafe"
)

// createLocks creates both locks for a lock file of layout l that this
// process holds exclusively, and records in it what other openers need to
// find them (mdb_env_setup_locks, *excl > 0): pthread mutexes initialised
// in place, semaphores whose names (version 1) or name hash (version 2)
// are stored, or the SysV set whose id is stored (sysv.go).
func createLocks(m *lockMap, fd uintptr, mode os.FileMode, l *Layout) (*sharedLocks, error) {
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
		if err := syscall.Fstat(int(fd), &st); err != nil {
			return nil, err
		}
		// unsafe.Sizeof only: the width of dev_t, which C hashes.
		id := lockFileID(uint64(st.Dev), int(unsafe.Sizeof(st.Dev)), st.Ino)
		var rname, wname string
		if l.Objects == SemaphoreNames {
			if l.WLock < 0 {
				return nil, syscall.EINVAL
			}
			rname, wname = semNamesV09(id, l.shortSemNames)
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
	case SysVSemaphores:
		return createSysVLocks(m, fd, mode, l)
	}
	return nil, syscall.EINVAL
}

// attachLocks uses the locks another process created: the mutexes in the
// file, the semaphores named in it (version 1) or after the hash stored in
// it (version 2), or the SysV set whose id is stored in it. What is in the
// file is read, never recomputed.
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
	case SysVSemaphores:
		return attachSysVLocks(m, l)
	}
	return nil, syscall.EINVAL
}
