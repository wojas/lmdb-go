// The shared locks on Windows: named kernel mutexes, what mdb.c's _WIN32
// paths call for LOCK_MUTEX0 (WaitForSingleObject), UNLOCK_MUTEX
// (ReleaseMutex) and mdb_mutex_consistent (nothing); rlmdb os/windows.rs
// (SharedMutex). No cgo: the calls go through golang.org/x/sys/windows, so
// every Windows build, with or without cgo, shares the locks with C.
//
//   - A mutex whose owner died (its thread ended holding it) is abandoned:
//     the next WaitForSingleObject returns WAIT_ABANDONED and owns it (C's
//     MDB_OWNERDEAD). That is lockOwnerDied, so the region repairs the
//     state as after a dead owner of a robust pthread mutex; marking it
//     consistent is a no-op.
//   - Windows mutexes belong to the locking thread and are recursive. The
//     region locks each on one OS thread and unlocks it there
//     (runtime.LockOSThread, as for the Linux mutexes), and never twice on
//     a thread: a goroutine pinned to a thread is the only one running on
//     it, and a second write transaction of the process waits for the
//     environment's in-process writer lock first. A goroutine that ends
//     holding the writer lock ends its thread, which abandons the mutex.
//   - mr_tid is GetCurrentThreadId(), a DWORD.

package lockfile

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// sharedLocksAvailable: Windows builds take the shared locks with and
	// without cgo.
	sharedLocksAvailable = true
	// mutexSize, mutexAlign: Windows lock files hold no pthread mutexes
	// (layout_windows.go), so a layout with mutexes never matches.
	mutexSize  = 0
	mutexAlign = 1
)

// sharedMutex is one of the two shared locks: an open named mutex.
type sharedMutex struct {
	h windows.Handle
}

// createMutex creates (or opens, if it exists) the mutex name, accessible
// to every account (a NULL DACL, C's mdb_all_sa), not owned.
func createMutex(name string) (*sharedMutex, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	// A present but NULL DACL grants everyone access.
	if err := sd.SetDACL(nil, true, false); err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	// CreateMutex reports ERROR_ALREADY_EXISTS with a valid handle when the
	// mutex exists; C's CreateMutexA treats that as success.
	h, err := windows.CreateMutex(&sa, false, p)
	if h == 0 {
		return nil, err
	}
	return &sharedMutex{h: h}, nil
}

// openMutex opens the existing mutex name (OpenMutex(SYNCHRONIZE), enough
// to wait on and release it).
func openMutex(name string) (*sharedMutex, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, p)
	if err != nil {
		return nil, err
	}
	return &sharedMutex{h: h}, nil
}

// lock takes the mutex, blocking (WaitForSingleObject(INFINITE)).
// lockOwnerDied is WAIT_ABANDONED: the previous owner's thread ended
// holding it.
func (s *sharedMutex) lock() (lockStatus, error) {
	ev, err := windows.WaitForSingleObject(s.h, windows.INFINITE)
	switch {
	case err != nil:
		return 0, err
	case ev == windows.WAIT_OBJECT_0:
		return lockAcquired, nil
	case ev == windows.WAIT_ABANDONED:
		return lockOwnerDied, nil
	}
	return 0, syscall.EINVAL
}

// unlock releases the mutex (ReleaseMutex); ERROR_NOT_OWNER if this
// thread does not hold it.
func (s *sharedMutex) unlock() error { return windows.ReleaseMutex(s.h) }

// consistent: nothing to do, the waiter owns an abandoned mutex normally
// (mdb_mutex_consistent is 0 on Windows).
func (s *sharedMutex) consistent() error { return nil }

// close closes this process's handle. Nothing is removed: the kernel
// destroys a named mutex with its last handle.
func (s *sharedMutex) close(bool) {
	if s.h != 0 {
		windows.CloseHandle(s.h)
		s.h = 0
	}
}

// threadID is GetCurrentThreadId(): mr_tid, which C only ever prints
// (mdb_reader_list).
func threadID() uint64 { return uint64(windows.GetCurrentThreadId()) }
