//go:build !cgo

// The counterpart of shared_cgo.go for builds without cgo: the shared locks
// are libc objects (pthread mutexes, named semaphores), so without cgo
// there are none. The package still builds; Open returns ErrNoSharedLocks,
// and environments without a lock file (MDB_NOLOCK) need nothing from this
// package.

package lockfile

const (
	sharedLocksAvailable = false
	// sizeof(pthread_mutex_t) is unknown without cgo, and with it the
	// Linux lock-file layouts (LayoutFor returns nil there).
	mutexSize  = 0
	mutexAlign = 1
)

type sharedMutex struct{}

func initMutex(*lockMap, int) (*sharedMutex, error)   { return nil, ErrNoSharedLocks }
func attachMutex(*lockMap, int) (*sharedMutex, error) { return nil, ErrNoSharedLocks }

func createSemaphore(string, uint32) (*sharedMutex, error) { return nil, ErrNoSharedLocks }
func openSemaphore(string) (*sharedMutex, error)           { return nil, ErrNoSharedLocks }
func unlinkSemaphore(string) error                         { return ErrNoSharedLocks }

func (*sharedMutex) lock() (lockStatus, error) { return 0, ErrNoSharedLocks }
func (*sharedMutex) unlock() error             { return ErrNoSharedLocks }
func (*sharedMutex) consistent() error         { return ErrNoSharedLocks }
func (*sharedMutex) close(bool)                {}

func threadID() uint64 { return 0 }
