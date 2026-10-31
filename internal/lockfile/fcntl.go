// One-byte fcntl record locks on the lock file: mdb_env_excl_lock,
// mdb_env_share_locks and mdb_reader_pid in mdb.c; rlmdb os/unix.rs
// lock_byte and probe_lock.

package lockfile

import "syscall"

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
func lockByte(fd int, off int64, exclusive, wait bool) error {
	lk := syscall.Flock_t{Type: syscall.F_RDLCK, Whence: 0, Start: off, Len: 1}
	if exclusive {
		lk.Type = syscall.F_WRLCK
	}
	cmd := syscall.F_SETLK
	if wait {
		cmd = syscall.F_SETLKW
	}
	for {
		err := syscall.FcntlFlock(uintptr(fd), cmd, &lk)
		if err != syscall.EINTR {
			return err
		}
	}
}

// probeLock reports whether another process holds a lock on the byte at off
// (F_GETLK with an F_WRLCK request, mdb_reader_pid(Pidcheck)). This
// process's own locks never conflict with its request and read as absent.
func probeLock(fd int, off int64) (bool, error) {
	for {
		lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: off, Len: 1}
		err := syscall.FcntlFlock(uintptr(fd), syscall.F_GETLK, &lk)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		return lk.Type != syscall.F_UNLCK, nil
	}
}
