//go:build (linux || freebsd || openbsd || netbsd || dragonfly) && !cgo

// The counterpart of shared_cgo.go and, on OpenBSD, NetBSD and DragonFly,
// of sem.go and sysv.go, for builds without cgo: the shared locks are
// pthread mutexes (Linux, FreeBSD) or POSIX and SysV semaphores (the other
// BSDs), which only cgo reaches (their size and protocol are the C
// library's), so without cgo there are none. The package still builds;
// Open returns ErrNoSharedLocks, and environments without a lock file
// (MDB_NOLOCK) need nothing from this package. (macOS builds without cgo
// have shared locks: sem_libsystem_darwin.go.)

package lockfile

import "os"

const (
	sharedLocksAvailable = false
	// sizeof(pthread_mutex_t) is unknown without cgo, and with it the
	// Linux and FreeBSD lock-file layouts (LayoutFor returns nil there).
	mutexSize  = 0
	mutexAlign = 1
)

type sharedMutex struct{}

func initMutex(*lockMap, int) (*sharedMutex, error)   { return nil, ErrNoSharedLocks }
func attachMutex(*lockMap, int) (*sharedMutex, error) { return nil, ErrNoSharedLocks }

func createSemaphore(string, uint32) (*sharedMutex, error) { return nil, ErrNoSharedLocks }
func openSemaphore(string) (*sharedMutex, error)           { return nil, ErrNoSharedLocks }
func unlinkSemaphore(string) error                         { return ErrNoSharedLocks }

func createSysVLocks(*lockMap, uintptr, os.FileMode, *Layout) (*sharedLocks, error) {
	return nil, ErrNoSharedLocks
}
func attachSysVLocks(*lockMap, *Layout) (*sharedLocks, error) { return nil, ErrNoSharedLocks }

func (*sharedMutex) lock() (lockStatus, error) { return 0, ErrNoSharedLocks }
func (*sharedMutex) unlock() error             { return ErrNoSharedLocks }
func (*sharedMutex) consistent() error         { return ErrNoSharedLocks }
func (*sharedMutex) close(bool)                {}

func threadID() uint64 { return 0 }
