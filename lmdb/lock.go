// The one place where the engine gets the lock file (internal/lockfile):
// the writer lock and reader table that C, Rust and Go processes share.
// In C this is mdb_env_setup_locks, called by mdb_env_open.

package lmdb

import (
	"errors"
	"os"

	"github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/lockfile"
)

// The lock file is the engine's lock region.
var _ engine.LockRegion = (*lockfile.Region)(nil)

// ErrNoSharedLocking is returned by Env.Open for an environment without the
// NoLock flag in a Linux program built without cgo (CGO_ENABLED=0).  The
// writer lock and the reader-table lock that LMDB shares between processes
// are libc objects: on Linux pthread mutexes, which this package reaches
// through cgo, so that without cgo only environments opened with NoLock,
// which have no lock file, can be used.  On macOS they are POSIX semaphores,
// reached through cgo in a cgo build and through libSystem directly in a
// build without cgo, and on Windows named kernel mutexes, reached through
// the system's DLLs without cgo, so macOS and Windows builds never return
// this error.
var ErrNoSharedLocking = errors.New("lmdb: shared locking on Linux needs a cgo build (CGO_ENABLED=1); open the environment with NoLock or build with cgo")

// errNoSharedLocks is what the lock file returns for ErrNoSharedLocking.
var errNoSharedLocks = lockfile.ErrNoSharedLocks

// SharedLockingAvailable reports whether this program can open environments
// with a lock file, i.e. without the NoLock flag: true on macOS and Windows,
// and on Linux if it was built with cgo.  Otherwise Env.Open returns
// ErrNoSharedLocking unless NoLock is given.
func SharedLockingAvailable() bool {
	return lockfile.SharedLocksAvailable()
}

// openLock opens the lock file of a new or existing environment of data
// format f for the engine (engine.OpenLockFunc).
func openLock(path string, mode os.FileMode, maxReaders uint32, readOnly bool, f format.Format) (engine.LockRegion, error) {
	if !lockfile.SharedLocksAvailable() {
		return nil, lockfile.ErrNoSharedLocks
	}
	r, err := lockfile.Open(path, mode, maxReaders, readOnly, lockfile.LayoutFor(f))
	if err != nil {
		return nil, err
	}
	if r == nil {
		// A read-only environment on a read-only file system: no lock
		// file, as in C. The engine wants a nil interface, not a typed nil.
		return nil, nil
	}
	return r, nil
}
