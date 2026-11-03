//go:build darwin || ((openbsd || netbsd || dragonfly) && cgo)

// The SysV semaphore set of a version 2 lock file (LMDB 1.0) on OpenBSD,
// NetBSD and DragonFly: C 1.0's MDB_USE_SYSV_SEM (mdb.c 1.0 lines 192-198
// select it, 452-481 lock and unlock, 6096-6110 and 6165-6182 create and
// attach, 6565-6574 remove); rlmdb os/shared_lock.rs (mod sysv).
//
// The set has two semaphores, the reader lock (0) and the writer lock (1),
// each with value 1 when free. The exclusive opener creates or finds the
// set with semget(ftok(lockfile, 'M'), 2, mode | IPC_CREAT) (SysVKey), sets
// both values to 1 and stores the set's id in mtb_semid; every other opener
// takes the id from there and checks that it may use the set (IPC_STAT,
// then IPC_SET with what that read, which only the set's owner, its
// creator and root pass). Every semop has SEM_UNDO, so the kernel gives
// the semaphore back when its holder dies; a flag in the lock file
// (mti_rlocked, mti_wlocked) is set while the semaphore is held, so the
// next holder finds the flag a dead holder left set and reports it as C's
// MDB_OWNERDEAD (mdb_sem_wait), which LockWriter and lockReaders repair as
// for a robust mutex. The last user removes the set (IPC_RMID). Unlike a
// robust mutex, a semaphore does not belong to a thread.
//
// The system calls are made by sysv_cgo.go (cgo builds), behind these
// functions:
//
//	sysvGet(key, mode) (semid, errno)   semget(key, 2, (mode & 0777) | IPC_CREAT)
//	sysvSetAll(semid) errno             semctl(semid, 0, SETALL, {1, 1})
//	sysvCheck(semid) errno              semctl(IPC_STAT), then semctl(IPC_SET) with what it read
//	sysvHasThird(semid) (bool, errno)   semctl(semid, 2, GETVAL): whether semaphore 2 exists (EINVAL says not)
//	sysvOp(semid, num, op) errno        one semop of op on semaphore num, with SEM_UNDO; EINTR is returned
//	sysvRemove(semid) errno             semctl(semid, 0, IPC_RMID)
//
// This file is also built on macOS, which has the same interface, so that
// the tests run the protocol, and lock files with the version 2 layout of
// these BSDs, on a real kernel there, as rlmdb's tests do; no macOS lock
// file uses it. macOS builds without cgo have no SysV calls
// (sysv_nocgo_darwin.go: ENOTSUP).
//
// Unlike C, the exclusive opener refuses an existing set of the key that is
// not its to reset (checkReusable, rlmdb KNOWN_C_BUGS.md row 46).

package lockfile

import (
	"os"
	"syscall"
)

// The semaphores of the set (me_rmutex->semnum, me_wmutex->semnum).
const (
	sysvReader = 0
	sysvWriter = 1
)

// The calls sysv_cgo.go and sysv_nocgo_darwin.go provide, with their
// signatures.
var (
	_ func(int64, uint32) (int32, syscall.Errno) = sysvGet
	_ func(int32) syscall.Errno                  = sysvSetAll
	_ func(int32) syscall.Errno                  = sysvCheck
	_ func(int32) (bool, syscall.Errno)          = sysvHasThird
	_ func(int32, uint16, int16) syscall.Errno   = sysvOp
	_ func(int32) syscall.Errno                  = sysvRemove
)

// sysvSemaphore is one semaphore of the set used as a mutex (C's struct
// mdb_mutex with MDB_USE_SYSV_SEM): the set's id, the semaphore's number,
// and the offset of its flag in the lock file, 1 while it is held.
type sysvSemaphore struct {
	m      *lockMap
	semid  int32
	semnum uint16
	flag   int
}

// lock is mdb_sem_wait: it takes the semaphore (retried on EINTR), then
// sets the flag. A flag that was set already was left by a holder that
// died (the kernel undid its semop): lockOwnerDied.
func (s *sysvSemaphore) lock() (lockStatus, error) {
	for {
		switch e := sysvOp(s.semid, s.semnum, -1); e {
		case 0:
			st := lockAcquired
			if s.m.loadInt32(s.flag) != 0 {
				st = lockOwnerDied
			}
			s.m.storeInt32(s.flag, 1)
			return st, nil
		case syscall.EINTR:
		default:
			return 0, e
		}
	}
}

// unlock is UNLOCK_MUTEX: it clears the flag, then releases the semaphore.
func (s *sysvSemaphore) unlock() error {
	s.m.storeInt32(s.flag, 0)
	return errOf(sysvOp(s.semid, s.semnum, 1))
}

// close: with remove (the last user of the environment), the reader
// semaphore's close removes the whole set, once per environment as
// mdb_env_close0 does; failure (the set is gone already) is ignored, as in
// C. There is no handle to release.
func (s *sysvSemaphore) close(remove bool) {
	if remove && s.semnum == sysvReader {
		sysvRemove(s.semid)
	}
}

// sysvLayout reports whether l has room for the set: mtb_semid and
// mtb_rlocked at RLock, mt2_wlocked at WLock.
func sysvLayout(l *Layout) bool { return l.RLockSize == 2*cIntSize && l.WLock >= 0 }

// sysvLocks is the pair of locks on set semid, with their flags at RLock +
// sizeof(int) (mti_rlocked) and WLock (mti_wlocked), as
// mdb_env_setup_locks points them.
func sysvLocks(m *lockMap, semid int32, l *Layout) *sharedLocks {
	return &sharedLocks{
		r: &sharedMutex{sysv: &sysvSemaphore{m: m, semid: semid, semnum: sysvReader, flag: l.RLock + cIntSize}},
		w: &sharedMutex{sysv: &sysvSemaphore{m: m, semid: semid, semnum: sysvWriter, flag: l.WLock}},
	}
}

// checkReusable checks that set semid, which semget returned for the lock
// file's key, is one createSysVLocks may reset. C 1.0 resets whatever set it
// gets (rlmdb KNOWN_C_BUGS.md row 46); the key is ftok's, which keeps 8 bits of
// the device and 16 of the inode, so another program's set can have it.
//
//   - sysvCheck passes, which every other opener must pass too
//     (attachSysVLocks). If it does not, the set is another user's: its
//     error (EACCES, EPERM).
//   - The set has exactly two semaphores, else EEXIST. semget with nsems 2
//     returns an existing set only if it has at least two, and semaphore
//     number 2 exists only in a larger one: GETVAL of it fails with EINVAL in
//     a set of two (POSIX). SETALL of a larger set would read one value per
//     semaphore from a two-element array and overwrite the other program's
//     semaphores.
//
// A refused set is left as it is.
func checkReusable(semid int32) error {
	if e := sysvCheck(semid); e != 0 {
		return e
	}
	third, e := sysvHasThird(semid)
	if e != 0 {
		return e
	}
	if third {
		return syscall.EEXIST
	}
	return nil
}

// createSysVLocks creates (or finds again) the set of a lock file of
// layout l that this process holds exclusively, sets both semaphores to 1,
// and stores the set's id and cleared flags (mdb_env_setup_locks, *excl >
// 0). An existing set of the key that is not ours to reset is refused
// (checkReusable).
func createSysVLocks(m *lockMap, fd uintptr, mode os.FileMode, l *Layout) (*sharedLocks, error) {
	if !sysvLayout(l) {
		return nil, syscall.EINVAL
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(fd), &st); err != nil {
		return nil, err
	}
	semid, e := sysvGet(SysVKey(uint64(st.Dev), st.Ino), uint32(mode.Perm()))
	if e != 0 {
		return nil, e
	}
	if err := checkReusable(semid); err != nil {
		return nil, err
	}
	if e := sysvSetAll(semid); e != 0 {
		return nil, e
	}
	m.storeInt32(l.RLock, semid)
	m.storeInt32(l.RLock+cIntSize, 0)
	m.storeInt32(l.WLock, 0)
	return sysvLocks(m, semid, l), nil
}

// attachSysVLocks uses the set whose id another process stored, after
// checking, as C does, that this process may read it and change it.
// errLocksRemoved if the id names no set (EINVAL; some systems say EIDRM
// for a removed one): the last user removed it after this opener's exclusive
// probe failed, or the lock file holds garbage.
func attachSysVLocks(m *lockMap, l *Layout) (*sharedLocks, error) {
	if !sysvLayout(l) {
		return nil, syscall.EINVAL
	}
	semid := m.loadInt32(l.RLock)
	if e := sysvCheck(semid); e != 0 {
		if e == syscall.EINVAL || e == syscall.EIDRM {
			return nil, errLocksRemoved
		}
		return nil, e
	}
	return sysvLocks(m, semid, l), nil
}
