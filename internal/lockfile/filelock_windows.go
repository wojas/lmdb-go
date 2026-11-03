// The lock file's OS protocol on Windows: mdb_fopen (MDB_O_LOCKS),
// mdb_env_excl_lock, mdb_env_share_locks, mdb_reader_pid and the unlock in
// mdb_env_close0, as mdb.c's _WIN32 paths do them (0.9.36 lines 2718,
// 4698, 4741, 5239); rlmdb os/windows.rs lock_byte, downgrade_lock,
// unlock_lock_file, set_pid_lock, pid_alive. The Unix counterpart is
// fcntl.go.
//
//   - Exclusive and shared use of the environment is a lock on byte 0 of
//     the lock file, as on Unix, with LockFileEx. Windows locks belong to a
//     handle, not to the process, and are mandatory for ReadFile and
//     WriteFile through other handles (not for mapped views, which is all
//     LMDB uses). mdb_env_excl_lock: an exclusive lock without waiting,
//     else a shared one, waiting. mdb_env_share_locks: a shared lock
//     through the same handle, which may overlap our own exclusive one,
//     then UnlockFile, which removes the exclusive one first. Before the
//     lock file is closed its lock is released explicitly: "Windows would
//     have unlocked it after closing anyway, but not necessarily at once"
//     (mdb_env_close0).
//   - Readers take no pid locks (MDB_PIDLOCK is 0, so version 1's
//     mti_format is 0x1). mdb_reader_pid(Pidcheck) asks whether the
//     process exists: it is dead when OpenProcess fails with
//     ERROR_INVALID_PARAMETER or when the process has exited. C opens the
//     process with PROCESS_QUERY_LIMITED_INFORMATION alone, so its
//     WaitForSingleObject always fails and a process that exited while
//     someone still holds a handle to it counts as alive; this asks for
//     SYNCHRONIZE too, as rlmdb does (rlmdb KNOWN_C_BUGS.md row 42).
//   - The lock file is opened with FILE_SHARE_READ | FILE_SHARE_WRITE, as
//     mdb_fopen does (it cannot be deleted while open); the permission mode
//     is ignored, as in C. A read-only file system is ERROR_WRITE_PROTECT.
//   - The lock file's size is computed in 64 bits. C passes it through 32
//     bits on Windows (rlmdb KNOWN_C_BUGS.md row 43), which only matters for
//     lock files of 2 GiB and more (some 33 million readers).

package lockfile

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// openLockFile opens the lock file read/write, creating it if it does not
// exist (MDB_O_LOCKS: GENERIC_READ | GENERIC_WRITE, OPEN_ALWAYS), shared
// for reading and writing only. mode is ignored, as in C.
func openLockFile(path string, mode os.FileMode) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// isReadOnlyFS reports whether err says the file system is read-only
// (ERROR_WRITE_PROTECT, C's MDB_ERRCODE_ROFS).
func isReadOnlyFS(err error) bool { return errors.Is(err, windows.ERROR_WRITE_PROTECT) }

// lockByte locks the byte at off with LockFileEx: exclusive or shared,
// waiting for it if wait is set, else failing with ERROR_LOCK_VIOLATION if
// another handle holds a conflicting lock.
//
// Windows locks belong to the handle: another handle of the same file in
// this process conflicts as another process's would. A handle may add a
// shared lock to its own exclusive one, not the reverse.
func lockByte(fd uintptr, off int64, exclusive, wait bool) error {
	var flags uint32
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	ov := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
	// The handle is synchronous (not opened with FILE_FLAG_OVERLAPPED), so
	// LockFileEx returns only once it is done.
	return windows.LockFileEx(windows.Handle(fd), flags, 0, 1, 0, &ov)
}

// unlockByte is UnlockFile of the byte at off: it removes one lock of this
// handle there, the exclusive one first if it holds both.
func unlockByte(fd uintptr, off int64) error {
	ov := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
	return windows.UnlockFileEx(windows.Handle(fd), 0, 1, 0, &ov)
}

// downgradeLock is mdb_env_share_locks: it turns our exclusive lock on byte
// 0 into a shared one. First a shared lock, which may overlap our own
// exclusive lock; the unlock then removes the exclusive one (C ignores its
// result too).
func downgradeLock(fd uintptr) error {
	if err := lockByte(fd, 0, false, true); err != nil {
		return err
	}
	_ = unlockByte(fd, 0)
	return nil
}

// unlockLockFile releases our lock on byte 0 of the lock file before it is
// closed, as mdb_env_close0 does: Windows would release it with the
// handle, "but not necessarily at once".
func unlockLockFile(fd uintptr) { _ = unlockByte(fd, 0) }

// setPIDLock is mdb_reader_pid(Pidset): nothing on Windows (MDB_PIDLOCK is
// 0).
func setPIDLock(fd uintptr, pid int32) error { return nil }

// pidAlive is mdb_reader_pid(Pidcheck): false only if process pid is
// certainly gone: OpenProcess says there is no such process
// (ERROR_INVALID_PARAMETER), or the process has exited. Any other failure
// (access denied, ...) counts as alive. See the file comment for the
// SYNCHRONIZE right C does not ask for.
func pidAlive(fd uintptr, pid int32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return err != windows.ERROR_INVALID_PARAMETER
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err != nil || ev != windows.WAIT_OBJECT_0
}
