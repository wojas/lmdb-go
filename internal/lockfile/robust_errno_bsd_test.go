//go:build netbsd || dragonfly

package lockfile

import "syscall"

// errNotRecoverable is never returned here: NetBSD and DragonFly lock files
// have no robust mutexes (and package syscall has no ENOTRECOVERABLE
// there), as on Windows.
const errNotRecoverable = syscall.Errno(0)
