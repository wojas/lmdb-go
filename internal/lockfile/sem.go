//go:build darwin || ((openbsd || netbsd || dragonfly) && cgo)

// The shared locks as semaphores. On macOS, and for LMDB 0.9 lock files on
// OpenBSD, NetBSD and DragonFly, they are LMDB's MDB_USE_POSIX_SEM (mdb.c
// 0.9.37 lines 143-149 and 367-379): LOCK_MUTEX0 is mdb_sem_wait (sem_wait,
// retried on EINTR), UNLOCK_MUTEX is sem_post, mdb_env_setup_locks
// (4990-5030) creates the two semaphores with sem_unlink then
// sem_open(name, O_CREAT|O_EXCL, mode, 1) or opens them with sem_open(name,
// 0), and mdb_env_close0 (5265-5280) closes them with sem_close (and
// sem_unlink for the last user). rlmdb os/shared_lock.rs (the
// NamedSemaphore part) makes the same calls. Named semaphores are not
// robust: a holder that dies leaves the lock taken, as with C. For LMDB 1.0
// lock files OpenBSD, NetBSD and DragonFly use a SysV semaphore set
// instead (C 1.0's MDB_USE_SYSV_SEM), which sysv.go implements; a
// sharedMutex is then one semaphore of that set.
//
// The libc calls themselves are made by one of two files, both behind the
// same small set of functions:
//
//   - sem_cgo.go (//go:build cgo): through cgo. The default on macOS, since
//     Go enables cgo for native builds when a C compiler is found, and the
//     only way on the other BSDs (without cgo they have no shared locks:
//     shared_nocgo.go).
//   - sem_libsystem_darwin.go (macOS, //go:build !cgo): libSystem called
//     directly, through assembly trampolines, as golang.org/x/sys/unix
//     does. This makes macOS builds without cgo (CGO_ENABLED=0, and
//     cross-compiles to macOS) fully functional; its file comment explains
//     the mechanism.
//
// Both provide exactly these calls (checked below at compile time), and
// sem_darwin_test.go runs the same tests against whichever is built on
// macOS:
//
//	semCreate(name, mode, value) (h, errno)  sem_open(name, O_CREAT|O_EXCL, mode, value)
//	semOpen(name) (h, errno)                 sem_open(name, 0)
//	semWait(h) errno                         one sem_wait; EINTR is returned, not retried
//	semTryWait(h) errno                      sem_trywait (tests: macOS has no working sem_getvalue)
//	semPost(h) errno                         sem_post
//	semClose(h) errno                        sem_close
//	semUnlink(name) errno                    sem_unlink
//	pthreadSelf() uint64                     pthread_self, for mr_tid
//	semImpl                                  "cgo" or "libSystem", for test logs
//
// A result errno of 0 means success. Names are NUL-free strings (EINVAL
// otherwise); the names LMDB uses never contain NUL (readName stops at the
// first one).
//
// Semaphore handles are uintptr, never a Go pointer type. macOS's sem_t *
// values are not addresses but descriptor numbers (sem_open returns the
// index of a new file descriptor of semaphore type: 0, 3, 4, ...). Stored
// in an unsafe.Pointer or *T, such a value is an invalid pointer that the
// garbage collector finds on a stack or in the heap and crashes on ("invalid
// pointer found on stack"); as a uintptr it is an integer the collector
// ignores. For the same reason a handle of 0 is valid (when descriptor 0 is
// free), so failure is reported by the errno, never by a zero handle. (On
// the other BSDs a sem_t * is a pointer to C memory, which a uintptr holds
// just as well.)

package lockfile

import "syscall"

const (
	// sharedLocksAvailable: macOS builds take the shared locks with and
	// without cgo, OpenBSD, NetBSD and DragonFly builds with cgo.
	sharedLocksAvailable = true
	// mutexSize, mutexAlign: these lock files hold no pthread mutexes
	// (layout_darwin.go, layout_bsd.go), so a layout with mutexes never
	// matches.
	mutexSize  = 0
	mutexAlign = 1
)

// The calls both implementations provide, with their signatures: a build
// fails here if one of them drifts.
var (
	_ func(string, uint32, uint32) (uintptr, syscall.Errno) = semCreate
	_ func(string) (uintptr, syscall.Errno)                 = semOpen
	_ func(uintptr) syscall.Errno                           = semWait
	_ func(uintptr) syscall.Errno                           = semTryWait
	_ func(uintptr) syscall.Errno                           = semPost
	_ func(uintptr) syscall.Errno                           = semClose
	_ func(string) syscall.Errno                            = semUnlink
	_ func() uint64                                         = pthreadSelf
	_ string                                                = semImpl
)

// sharedMutex is one of the two shared locks: an open named semaphore, or
// one semaphore of a SysV set (sysv).
type sharedMutex struct {
	h    uintptr // the sem_t *, as an integer (see the file comment)
	open bool    // h is a handle this process has not closed
	name string
	// sysv, if set, is the lock instead of a named semaphore: one
	// semaphore of the SysV set of a version 2 lock file on OpenBSD,
	// NetBSD and DragonFly (sysv.go).
	sysv *sysvSemaphore
}

// errOf turns a call's errno into an error, nil for 0.
func errOf(e syscall.Errno) error {
	if e != 0 {
		return e
	}
	return nil
}

// initMutex and attachMutex: these lock files have no mutexes (as in C,
// which uses no process-shared mutexes there).
func initMutex(*lockMap, int) (*sharedMutex, error)   { return nil, syscall.ENOTSUP }
func attachMutex(*lockMap, int) (*sharedMutex, error) { return nil, syscall.ENOTSUP }

// createSemaphore creates the semaphore name with value 1 and permissions
// mode (sem_open with O_CREAT|O_EXCL, as mdb_env_setup_locks); EEXIST if it
// exists.
func createSemaphore(name string, mode uint32) (*sharedMutex, error) {
	h, e := semCreate(name, mode, 1)
	if e != 0 {
		return nil, e
	}
	return &sharedMutex{h: h, open: true, name: name}, nil
}

// openSemaphore opens the existing semaphore name (sem_open(name, 0)).
func openSemaphore(name string) (*sharedMutex, error) {
	h, e := semOpen(name)
	if e != 0 {
		return nil, e
	}
	return &sharedMutex{h: h, open: true, name: name}, nil
}

// unlinkSemaphore removes the name (sem_unlink); open handles stay valid.
func unlinkSemaphore(name string) error { return errOf(semUnlink(name)) }

// lock takes the lock, blocking: mdb_sem_wait, sem_wait retried while it
// fails with EINTR. macOS's sem_wait returns EINTR when a signal handler
// runs on the waiting thread, even for handlers installed with SA_RESTART,
// which is how Go installs all of its own. A SysV semaphore tells about a
// dead previous holder (lockOwnerDied); a named one cannot.
func (s *sharedMutex) lock() (lockStatus, error) {
	if s.sysv != nil {
		return s.sysv.lock()
	}
	for {
		switch e := semWait(s.h); e {
		case 0:
			return lockAcquired, nil
		case syscall.EINTR:
		default:
			return 0, e
		}
	}
}

// unlock releases the lock (sem_post).
func (s *sharedMutex) unlock() error {
	if s.sysv != nil {
		return s.sysv.unlock()
	}
	return errOf(semPost(s.h))
}

// consistent: named semaphores have no owner-died state, and a SysV
// semaphore's flag is set again by the new holder already
// (mdb_mutex_consistent is 0 with MDB_USE_SYSV_SEM).
func (s *sharedMutex) consistent() error { return nil }

// close releases this process's handle (sem_close), then, with unlink,
// removes the name (sem_unlink), as mdb_env_close0 does for the last user.
// A SysV semaphore has no handle; the last user removes the set.
func (s *sharedMutex) close(unlink bool) {
	if s.sysv != nil {
		s.sysv.close(unlink)
		return
	}
	if !s.open {
		return
	}
	semClose(s.h)
	s.open = false
	if unlink {
		_ = unlinkSemaphore(s.name)
	}
}

// threadID is pthread_self() as an integer: mr_tid, which C only ever
// prints (mdb_reader_list).
func threadID() uint64 { return pthreadSelf() }
