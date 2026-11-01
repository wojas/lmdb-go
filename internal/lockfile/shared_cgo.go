//go:build (linux || freebsd) && cgo

// The libc half of the two shared locks on Linux and FreeBSD: what mdb.c
// calls for LOCK_MUTEX0, UNLOCK_MUTEX and mdb_mutex_consistent, for
// creating the locks in mdb_env_setup_locks and for closing them in
// mdb_env_close0. rlmdb's os/shared_lock.rs makes the same calls through
// the libc crate.
//
// On Linux and FreeBSD 11+ (MDB_USE_POSIX_MUTEX with MDB_USE_ROBUST) the
// locks are process-shared robust pthread_mutex_ts inside the lock file.
// Going through libc keeps interoperation exact with C processes of the
// same libc (glibc, musl, FreeBSD's libthr): the mutex protocol, the
// robust-list handling that lets the kernel mark a dead owner's mutex, and
// sizeof(pthread_mutex_t), which moves the reader table, are libc's. On
// FreeBSD the pthread_mutex_t in the file is a pointer-sized word, and
// libthr keeps the process-shared mutex in a kernel object keyed by its
// address.
//
// A robust mutex belongs to the thread that locked it: the caller keeps the
// goroutine on one OS thread (runtime.LockOSThread) from lock to unlock.
//
// shared_nocgo.go is its counterpart for builds without cgo, which have no
// shared locks. The semaphores of macOS and the other BSDs are in sem.go
// and sysv.go.

package lockfile

/*
#include <errno.h>
#include <pthread.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

enum { lmdbgo_mutex_align = _Alignof(pthread_mutex_t) };

// mr_tid as mdb_txn_renew0 stores it.
static uint64_t lmdbgo_thread_id(void) { return (uint64_t)(uintptr_t)pthread_self(); }

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
*/
import "C"

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	// sharedLocksAvailable: this build can take the shared locks.
	sharedLocksAvailable = true
	// mutexSize is sizeof(pthread_mutex_t): MNAME_LEN of a Linux or
	// FreeBSD lock file (glibc 40 bytes on x86-64, 48 on aarch64; musl 40;
	// FreeBSD 8).
	mutexSize = C.sizeof_pthread_mutex_t
	// mutexAlign is alignof(pthread_mutex_t).
	mutexAlign = C.lmdbgo_mutex_align
)

// sharedMutex is one of the two shared locks: a pthread_mutex_t in the lock
// file.
type sharedMutex struct {
	p unsafe.Pointer // the pthread_mutex_t in the lock map
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

// createSemaphore, openSemaphore, unlinkSemaphore, createSysVLocks,
// attachSysVLocks: these lock files have no semaphores.
func createSemaphore(string, uint32) (*sharedMutex, error) { return nil, syscall.ENOTSUP }
func openSemaphore(string) (*sharedMutex, error)           { return nil, syscall.ENOTSUP }
func unlinkSemaphore(string) error                         { return syscall.ENOTSUP }

func createSysVLocks(*lockMap, uintptr, os.FileMode, *Layout) (*sharedLocks, error) {
	return nil, syscall.ENOTSUP
}
func attachSysVLocks(*lockMap, *Layout) (*sharedLocks, error) { return nil, syscall.ENOTSUP }

// lock takes the lock, blocking (pthread_mutex_lock). lockOwnerDied is
// EOWNERDEAD: the previous owner of the robust mutex died holding it.
func (s *sharedMutex) lock() (lockStatus, error) {
	switch rc := C.lmdbgo_mutex_lock(s.p); rc {
	case 0:
		return lockAcquired, nil
	case C.EOWNERDEAD:
		return lockOwnerDied, nil
	default:
		return 0, syscall.Errno(rc)
	}
}

// unlock releases the lock (pthread_mutex_unlock); EPERM if this thread
// does not hold it.
func (s *sharedMutex) unlock() error {
	if rc := C.lmdbgo_mutex_unlock(s.p); rc != 0 {
		return syscall.Errno(rc)
	}
	return nil
}

// consistent marks a mutex taken with lockOwnerDied as repaired
// (pthread_mutex_consistent).
func (s *sharedMutex) consistent() error {
	if rc := C.lmdbgo_mutex_consistent(s.p); rc != 0 {
		return syscall.Errno(rc)
	}
	return nil
}

// close: process-shared mutexes need nothing (C does not destroy them
// either).
func (s *sharedMutex) close(bool) {}

// threadID is pthread_self() as an integer: mr_tid, which C only ever
// prints (mdb_reader_list).
func threadID() uint64 { return uint64(C.lmdbgo_thread_id()) }
