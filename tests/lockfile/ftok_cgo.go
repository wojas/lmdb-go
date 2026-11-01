//go:build unix && cgo

// The C library's ftok, for ftok_test.go: lockfile.SysVKey computes the
// key of the SysV semaphore set of a version 2 lock file on OpenBSD,
// NetBSD and DragonFly (C 1.0's ftok(lockfile, 'M')) from the file's
// device and inode numbers, and must agree with libc.

package lockfiletest

/*
#include <stdlib.h>
#include <sys/types.h>
#include <sys/ipc.h>

static long long lmdbgo_ftok(const char *path, int id) { return (long long)ftok(path, id); }
*/
import "C"

import "unsafe"

// libcFtok is ftok(path, id) of the C library; -1 on failure.
func libcFtok(path string, id byte) int64 {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	return int64(C.lmdbgo_ftok(cpath, C.int(id)))
}
