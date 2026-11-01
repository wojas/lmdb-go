//go:build unix && !netbsd && !dragonfly

package lockfile

import "syscall"

// errNotRecoverable is what locking a robust mutex returns after a failed
// repair (ENOTRECOVERABLE; robust mutexes are Linux's and FreeBSD's).
const errNotRecoverable = syscall.ENOTRECOVERABLE
