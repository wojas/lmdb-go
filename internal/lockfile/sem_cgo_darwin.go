//go:build cgo

// macOS's named semaphores through cgo: the semaphore calls listed in
// sem_darwin.go, made in C. This is the default on macOS, because Go
// enables cgo for native builds when a C compiler is found; builds without
// cgo use sem_libsystem_darwin.go, which makes the same calls in libSystem
// directly. Keeping this path means that an ordinary build does not depend
// on the runtime internals the libSystem stubs use; if a Go release ever
// breaks those, cgo builds keep working.

package lockfile

/*
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <semaphore.h>
#include <stdint.h>
#include <stdlib.h>

// Handles cross to Go as integers, never as pointers (sem_darwin.go), and
// through *h, so that failure is told by the return value alone: a handle
// of 0 is valid. sem_open is variadic, which cgo cannot call directly.
static int lmdbgo_sem_create(const char *name, unsigned int mode, unsigned int value, uintptr_t *h) {
	sem_t *s = sem_open(name, O_CREAT|O_EXCL, (mode_t)mode, value);
	if (s == SEM_FAILED) return errno;
	*h = (uintptr_t)s;
	return 0;
}
static int lmdbgo_sem_open(const char *name, uintptr_t *h) {
	sem_t *s = sem_open(name, 0);
	if (s == SEM_FAILED) return errno;
	*h = (uintptr_t)s;
	return 0;
}
static int lmdbgo_sem_wait(uintptr_t s) { return sem_wait((sem_t *)s) ? errno : 0; }
static int lmdbgo_sem_trywait(uintptr_t s) { return sem_trywait((sem_t *)s) ? errno : 0; }
static int lmdbgo_sem_post(uintptr_t s) { return sem_post((sem_t *)s) ? errno : 0; }
static int lmdbgo_sem_close(uintptr_t s) { return sem_close((sem_t *)s) ? errno : 0; }
static int lmdbgo_sem_unlink(const char *name) { return sem_unlink(name) ? errno : 0; }

// mr_tid as mdb_txn_renew0 stores it.
static uint64_t lmdbgo_thread_id(void) { return (uint64_t)(uintptr_t)pthread_self(); }
*/
import "C"

import (
	"strings"
	"syscall"
	"unsafe"
)

// cString is name as a C string, to be freed with C.free, or EINVAL if it
// contains a NUL (which C.CString would silently cut at), as in the
// libSystem path.
func cString(name string) (*C.char, syscall.Errno) {
	if strings.IndexByte(name, 0) >= 0 {
		return nil, syscall.EINVAL
	}
	return C.CString(name), 0
}

// semImpl names this implementation, for test logs.
const semImpl = "cgo"

func semCreate(name string, mode, value uint32) (uintptr, syscall.Errno) {
	cname, e := cString(name)
	if e != 0 {
		return 0, e
	}
	defer C.free(unsafe.Pointer(cname))
	var h C.uintptr_t
	if rc := C.lmdbgo_sem_create(cname, C.uint(mode), C.uint(value), &h); rc != 0 {
		return 0, syscall.Errno(rc)
	}
	return uintptr(h), 0
}

func semOpen(name string) (uintptr, syscall.Errno) {
	cname, e := cString(name)
	if e != 0 {
		return 0, e
	}
	defer C.free(unsafe.Pointer(cname))
	var h C.uintptr_t
	if rc := C.lmdbgo_sem_open(cname, &h); rc != 0 {
		return 0, syscall.Errno(rc)
	}
	return uintptr(h), 0
}

func semWait(h uintptr) syscall.Errno    { return syscall.Errno(C.lmdbgo_sem_wait(C.uintptr_t(h))) }
func semTryWait(h uintptr) syscall.Errno { return syscall.Errno(C.lmdbgo_sem_trywait(C.uintptr_t(h))) }
func semPost(h uintptr) syscall.Errno    { return syscall.Errno(C.lmdbgo_sem_post(C.uintptr_t(h))) }
func semClose(h uintptr) syscall.Errno   { return syscall.Errno(C.lmdbgo_sem_close(C.uintptr_t(h))) }

func semUnlink(name string) syscall.Errno {
	cname, e := cString(name)
	if e != 0 {
		return e
	}
	defer C.free(unsafe.Pointer(cname))
	return syscall.Errno(C.lmdbgo_sem_unlink(cname))
}

func pthreadSelf() uint64 { return uint64(C.lmdbgo_thread_id()) }
