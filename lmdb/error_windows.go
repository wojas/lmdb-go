package lmdb

import "syscall"

func operrno(op string, ret int) error {
	if ret == success {
		return nil
	}
	if minErrno <= int32(ret) && int32(ret) <= maxErrno {
		return &OpError{Op: op, Errno: Errno(ret)}
	}

	// translate C errors into corresponding syscall.Errno values so that
	// IsErrnoSys functions correctly, a kludge unknowning inherited from LMDB.
	// the errno in the returned OpError cannot be passed to C.mdb_strerror.
	// see the implementation of C.mdb_strerror for information about how the
	// following table was generated.
	var errno syscall.Errno
	switch ret {
	case 2: // ENOENT
		errno = syscall.ENOENT /* 2, FILE_NOT_FOUND */
	case 5: // EIO
		errno = syscall.EIO /* 5, ACCESS_DENIED */
	case 12: // ENOMEM
		errno = syscall.ENOMEM /* 12, INVALID_ACCESS */
	case 13: // EACCES
		errno = syscall.EACCES /* 13, INVALID_DATA */
	case 16: // EBUSY
		errno = syscall.EBUSY /* 16, CURRENT_DIRECTORY */
	case 22: // EINVAL
		errno = syscall.EINVAL /* 22, BAD_COMMAND */
	case 28: // ENOSPC
		errno = syscall.ENOSPC /* 28, OUT_OF_PAPER */
	default:
		errno = syscall.Errno(ret)
	}
	return &OpError{Op: op, Errno: errno}
}

// errNotOwner is what releasing another thread's writer mutex returns
// (ReleaseMutex: ERROR_NOT_OWNER).
const errNotOwner = syscall.Errno(288)
