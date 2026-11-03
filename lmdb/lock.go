// The one place where the engine gets the lock file (internal/lockfile):
// the writer lock and reader table that C, Rust and Go processes share.
// In C this is mdb_env_setup_locks, called by mdb_env_open.

package lmdb

import (
	"errors"
	"os"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/lockfile"
)

// The lock file is the engine's lock region.
var _ engine.LockRegion = (*lockfile.Region)(nil)

// ErrNoSharedLocking is returned by Env.Open for an environment without the
// NoLock flag in a Linux or BSD program built without cgo (CGO_ENABLED=0).
// The writer lock and the reader-table lock that LMDB shares between
// processes are libc objects: on Linux and FreeBSD pthread mutexes, on
// OpenBSD, NetBSD and DragonFly POSIX and SysV semaphores, which this
// package reaches through cgo, so that without cgo only environments opened
// with NoLock, which have no lock file, can be used.  On macOS they are
// POSIX semaphores, reached through cgo in a cgo build and through
// libSystem directly in a build without cgo, and on Windows named kernel
// mutexes, reached through the system's DLLs without cgo, so macOS and
// Windows builds never return this error.
var ErrNoSharedLocking = errors.New("lmdb: shared locking on Linux and the BSDs needs a cgo build (CGO_ENABLED=1); open the environment with NoLock or build with cgo")

// errNoSharedLocks is what the lock file returns for ErrNoSharedLocking.
var errNoSharedLocks = lockfile.ErrNoSharedLocks

// SharedLockingAvailable reports whether this program can open environments
// with a lock file, i.e. without the NoLock flag: true on macOS and Windows,
// and on Linux and the BSDs if it was built with cgo.  Otherwise Env.Open
// returns ErrNoSharedLocking unless NoLock is given.
func SharedLockingAvailable() bool {
	return lockfile.SharedLocksAvailable()
}

// openLock opens the lock file of a new or existing environment of data
// format f for the engine (engine.OpenLockFunc).
func openLock(path string, mode os.FileMode, maxReaders uint32, readOnly bool, f format.Format, claim engine.LockClaim) (engine.LockRegion, error) {
	if !lockfile.SharedLocksAvailable() {
		return nil, lockfile.ErrNoSharedLocks
	}
	r, err := lockfile.OpenClaimed(path, mode, maxReaders, readOnly, lockfile.LayoutFor(f), claim)
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

// ErrTxnWrongThread is the error of Txn.Commit (and Env.Update) for a write
// transaction committed on another OS thread than the one that began it,
// a program error (see Env.BeginTxn and runtime.LockOSThread).  The
// changes are committed, but the lock file's writer mutex, which belongs
// to the thread that took it on Linux, FreeBSD and Windows, stays held
// until that thread exits; then the next writer recovers it as from a
// writer that died.  The error returned wraps ErrTxnWrongThread and the
// mutex's error, syscall.EPERM (Windows: ERROR_NOT_OWNER), so
// errors.Is(err, ErrTxnWrongThread), errors.Is(err, syscall.EPERM) and
// IsErrnoSys(err, syscall.EPERM) all hold.  It is not an *OpError.  C
// LMDB (and lmdb-go v1) ignores the failed unlock; there is no error on
// macOS and the other BSDs, whose locks are semaphores.
var ErrTxnWrongThread = errors.New("lmdb: write transaction committed on another OS thread than the one that began it")

// txnThreadError is the error ErrTxnWrongThread describes: it wraps that
// and the writer mutex's errno.
type txnThreadError struct {
	errno syscall.Errno
}

func (e *txnThreadError) Error() string {
	return ErrTxnWrongThread.Error() + ": " + e.errno.Error()
}

// Unwrap makes errors.Is and errors.As see ErrTxnWrongThread and the errno.
func (e *txnThreadError) Unwrap() []error { return []error{ErrTxnWrongThread, e.errno} }
