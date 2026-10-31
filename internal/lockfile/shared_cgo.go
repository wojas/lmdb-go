//go:build cgo

// The libc half of the two shared locks: what mdb.c calls for
// LOCK_MUTEX0, UNLOCK_MUTEX and mdb_mutex_consistent, for creating the
// locks in mdb_env_setup_locks and for closing them in mdb_env_close0.
// rlmdb's os/shared_lock.rs makes the same calls through the libc crate.
//
// On Linux (MDB_USE_POSIX_MUTEX with MDB_USE_ROBUST) the locks are
// process-shared robust pthread_mutex_ts inside the lock file; on macOS
// (MDB_USE_POSIX_SEM) they are POSIX named semaphores. Going through libc
// keeps interoperation exact with C processes of the same libc, whatever
// it is (glibc, musl, Apple's): the mutex protocol, the robust-list
// handling that lets the kernel mark a dead owner's mutex, and
// sizeof(pthread_mutex_t), which moves the reader table, are libc's.
//
// A robust mutex belongs to the thread that locked it: the caller keeps the
// goroutine on one OS thread (runtime.LockOSThread) from lock to unlock.
//
// This is the only file of the library that uses cgo; shared_nocgo.go is
// its counterpart for builds without cgo.

package lockfile

/*
#include <errno.h>
#include <pthread.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#ifdef __APPLE__
#include <fcntl.h>
#include <semaphore.h>
#endif

enum { lmdbgo_mutex_align = _Alignof(pthread_mutex_t) };

// mr_tid as mdb_txn_renew0 stores it.
static uint64_t lmdbgo_thread_id(void) { return (uint64_t)(uintptr_t)pthread_self(); }

#ifndef __APPLE__
// mdb_env_setup_locks, exclusive opener: zero the mutex (mdb.c does, for
// Solaris), then pthread_mutex_init with PTHREAD_PROCESS_SHARED and
// PTHREAD_MUTEX_ROBUST.
static int lmdbgo_mutex_init(void *p) {
	pthread_mutex_t *m = p;
	pthread_mutexattr_t mattr;
	int rc;
	memset(m, 0, sizeof(*m));
	if ((rc = pthread_mutexattr_init(&mattr)) != 0)
		return rc;
	rc = pthread_mutexattr_setpshared(&mattr, PTHREAD_PROCESS_SHARED);
	if (!rc) rc = pthread_mutexattr_setrobust(&mattr, PTHREAD_MUTEX_ROBUST);
	if (!rc) rc = pthread_mutex_init(m, &mattr);
	pthread_mutexattr_destroy(&mattr);
	return rc;
}
static int lmdbgo_mutex_lock(void *p) { return pthread_mutex_lock(p); }
static int lmdbgo_mutex_unlock(void *p) { return pthread_mutex_unlock(p); }
static int lmdbgo_mutex_consistent(void *p) { return pthread_mutex_consistent(p); }

static void *lmdbgo_sem_create(const char *name, unsigned int mode) { errno = ENOTSUP; return NULL; }
static void *lmdbgo_sem_open(const char *name) { errno = ENOTSUP; return NULL; }
static int lmdbgo_sem_wait(void *s) { return ENOTSUP; }
static int lmdbgo_sem_post(void *s) { return ENOTSUP; }
static int lmdbgo_sem_close(void *s) { return ENOTSUP; }
static int lmdbgo_sem_unlink(const char *name) { return ENOTSUP; }
#else
// macOS has neither robust nor process-shared mutexes that LMDB uses.
static int lmdbgo_mutex_init(void *p) { return ENOTSUP; }
static int lmdbgo_mutex_lock(void *p) { return ENOTSUP; }
static int lmdbgo_mutex_unlock(void *p) { return ENOTSUP; }
static int lmdbgo_mutex_consistent(void *p) { return ENOTSUP; }

// sem_open is variadic, which cgo cannot call directly.
static void *lmdbgo_sem_create(const char *name, unsigned int mode) {
	sem_t *s = sem_open(name, O_CREAT|O_EXCL, (mode_t)mode, 1u);
	return s == SEM_FAILED ? NULL : s;
}
static void *lmdbgo_sem_open(const char *name) {
	sem_t *s = sem_open(name, 0);
	return s == SEM_FAILED ? NULL : s;
}
// mdb_sem_wait.
static int lmdbgo_sem_wait(void *s) {
	int rc;
	while ((rc = sem_wait(s)) && (rc = errno) == EINTR) ;
	return rc;
}
static int lmdbgo_sem_post(void *s) { return sem_post(s) ? errno : 0; }
static int lmdbgo_sem_close(void *s) { return sem_close(s) ? errno : 0; }
static int lmdbgo_sem_unlink(const char *name) { return sem_unlink(name) ? errno : 0; }
#endif
*/
import "C"

import (
	"syscall"
	"unsafe"
)

const (
	// sharedLocksAvailable: this build can take the shared locks.
	sharedLocksAvailable = true
	// mutexSize is sizeof(pthread_mutex_t): MNAME_LEN of a Linux lock file
	// (glibc 40 bytes on x86-64, 48 on aarch64; musl 40).
	mutexSize = C.sizeof_pthread_mutex_t
	// mutexAlign is alignof(pthread_mutex_t).
	mutexAlign = C.lmdbgo_mutex_align
)

// sharedMutex is one of the two shared locks: a pthread_mutex_t in the lock
// file or a named semaphore.
type sharedMutex struct {
	p    unsafe.Pointer // the pthread_mutex_t in the lock map, or the sem_t *
	sem  bool
	name string // the semaphore's name
}

func errnoOr(rc C.int, err error) error {
	if rc != 0 {
		return syscall.Errno(rc)
	}
	if err != nil {
		return err
	}
	return syscall.EINVAL
}

// initMutex initialises the pthread_mutex_t at off; the caller holds the
// lock file exclusively.
func initMutex(m *lockMap, off int) (*sharedMutex, error) {
	// unsafe: m.at checks that the mutex lies inside the mapping, which is
	// memory outside the Go heap, so C may keep using it.
	p := m.at(off, mutexSize, mutexAlign)
	if rc := C.lmdbgo_mutex_init(p); rc != 0 {
		return nil, syscall.Errno(rc)
	}
	return &sharedMutex{p: p}, nil
}

// attachMutex uses the pthread_mutex_t at off that another process (or
// initMutex) initialised.
func attachMutex(m *lockMap, off int) (*sharedMutex, error) {
	// unsafe: as in initMutex.
	return &sharedMutex{p: m.at(off, mutexSize, mutexAlign)}, nil
}

// createSemaphore creates the semaphore name with value 1 and permissions
// mode (sem_open with O_CREAT|O_EXCL); EEXIST if it exists.
func createSemaphore(name string, mode uint32) (*sharedMutex, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	p, err := C.lmdbgo_sem_create(cname, C.uint(mode))
	if p == nil {
		return nil, errnoOr(0, err)
	}
	return &sharedMutex{p: p, sem: true, name: name}, nil
}

// openSemaphore opens the existing semaphore name (sem_open(name, 0)).
func openSemaphore(name string) (*sharedMutex, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	p, err := C.lmdbgo_sem_open(cname)
	if p == nil {
		return nil, errnoOr(0, err)
	}
	return &sharedMutex{p: p, sem: true, name: name}, nil
}

// unlinkSemaphore removes the name (sem_unlink); open handles stay valid.
func unlinkSemaphore(name string) error {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	if rc := C.lmdbgo_sem_unlink(cname); rc != 0 {
		return syscall.Errno(rc)
	}
	return nil
}

// lock takes the lock, blocking (pthread_mutex_lock, or sem_wait retried
// on EINTR). lockOwnerDied is EOWNERDEAD: the previous owner of a robust
// mutex died holding it.
func (s *sharedMutex) lock() (lockStatus, error) {
	var rc C.int
	if s.sem {
		rc = C.lmdbgo_sem_wait(s.p)
	} else {
		rc = C.lmdbgo_mutex_lock(s.p)
	}
	switch {
	case rc == 0:
		return lockAcquired, nil
	case rc == C.EOWNERDEAD && !s.sem:
		return lockOwnerDied, nil
	}
	return 0, syscall.Errno(rc)
}

// unlock releases the lock (pthread_mutex_unlock, or sem_post). For a
// mutex, EPERM if this thread does not hold it.
func (s *sharedMutex) unlock() error {
	var rc C.int
	if s.sem {
		rc = C.lmdbgo_sem_post(s.p)
	} else {
		rc = C.lmdbgo_mutex_unlock(s.p)
	}
	if rc != 0 {
		return syscall.Errno(rc)
	}
	return nil
}

// consistent marks a mutex taken with lockOwnerDied as repaired
// (pthread_mutex_consistent). Semaphores have no such state.
func (s *sharedMutex) consistent() error {
	if s.sem {
		return nil
	}
	if rc := C.lmdbgo_mutex_consistent(s.p); rc != 0 {
		return syscall.Errno(rc)
	}
	return nil
}

// close releases this process's handle: sem_close for a semaphore, then,
// with unlink, sem_unlink of its name. Process-shared mutexes need nothing
// (C does not destroy them either).
func (s *sharedMutex) close(unlink bool) {
	if !s.sem || s.p == nil {
		return
	}
	C.lmdbgo_sem_close(s.p)
	s.p = nil
	if unlink {
		_ = unlinkSemaphore(s.name)
	}
}

// threadID is pthread_self() as an integer: mr_tid, which C only ever
// prints (mdb_reader_list).
func threadID() uint64 { return uint64(C.lmdbgo_thread_id()) }
