//go:build unix

// The lock file's OS protocol on Unix: opening it, and one-byte fcntl
// record locks on it for mdb_env_excl_lock, mdb_env_share_locks and
// mdb_reader_pid in mdb.c; rlmdb os/unix.rs lock_byte, probe_lock,
// downgrade_lock, set_pid_lock, pid_alive. The Windows counterpart is
// filelock_windows.go.

package lockfile

import (
	"errors"
	"os"
	"syscall"
)

// openLockFile opens the lock file read/write, creating it with the
// permissions mode if it does not exist (mdb_fopen's MDB_O_LOCKS).
func openLockFile(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, mode.Perm())
}

// isReadOnlyFS reports whether err says the file system is read-only
// (EROFS, C's MDB_ERRCODE_ROFS).
func isReadOnlyFS(err error) bool { return errors.Is(err, syscall.EROFS) }

// lockByte sets a one-byte fcntl lock at off: exclusive (F_WRLCK) or shared
// (F_RDLCK), waiting for conflicting locks to go away (F_SETLKW) if wait is
// set, else failing at once with EAGAIN or EACCES (F_SETLK). EINTR is
// retried, as mdb.c does.
//
// POSIX record locks belong to the process: locking a byte this process
// already holds converts the lock, and closing any descriptor of the file
// drops all of this process's locks on it. That is why LMDB forbids opening
// one environment twice in a process, and why this package never opens the
// lock file a second time.
func lockByte(fd uintptr, off int64, exclusive, wait bool) error {
	lk := syscall.Flock_t{Type: syscall.F_RDLCK, Whence: 0, Start: off, Len: 1}
	if exclusive {
		lk.Type = syscall.F_WRLCK
	}
	cmd := syscall.F_SETLK
	if wait {
		cmd = syscall.F_SETLKW
	}
	for {
		err := syscall.FcntlFlock(fd, cmd, &lk)
		if err != syscall.EINTR {
			return err
		}
	}
}

// probeLock reports whether another process holds a lock on the byte at off
// (F_GETLK with an F_WRLCK request, mdb_reader_pid(Pidcheck)). This
// process's own locks never conflict with its request and read as absent.
func probeLock(fd uintptr, off int64) (bool, error) {
	for {
		lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: off, Len: 1}
		err := syscall.FcntlFlock(fd, syscall.F_GETLK, &lk)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		return lk.Type != syscall.F_UNLCK, nil
	}
}

// downgradeLock is mdb_env_share_locks: it turns this process's exclusive
// lock on byte 0 into a shared one (F_SETLK converts it).
func downgradeLock(fd uintptr) error { return lockByte(fd, 0, false, false) }

// unlockLockFile: nothing to do before the lock file is closed; closing it
// releases the process's record locks.
func unlockLockFile(fd uintptr) {}

// setPIDLock is mdb_reader_pid(Pidset): the exclusive lock on byte pid of
// the lock file that advertises this process as alive.
func setPIDLock(fd uintptr, pid int32) error { return lockByte(fd, int64(pid), true, false) }

// pidAlive is mdb_reader_pid(Pidcheck): false only if process pid is
// certainly dead (nobody holds the lock on byte pid).
func pidAlive(fd uintptr, pid int32) bool {
	held, err := probeLock(fd, int64(pid))
	return held || err != nil
}
