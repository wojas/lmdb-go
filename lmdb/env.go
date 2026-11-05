package lmdb

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// success is a value returned from the LMDB API to indicate a successful call.
// The functions in this API this behavior and its use is not required.
const success = 0

// These flags are used exclusively for Env types and are set with Env.Open.
// Some flags may be set/unset later using Env.Set/.Unset methods.  Others will
// produce syscall.EINVAL.  Refer to the C documentation for detailed
// information.
const (
	// Flags for Env.Open.
	//
	// See mdb_env_open

	FixedMap    = 0x01      // Danger zone. Map memory at a fixed address.
	NoSubdir    = 0x4000    // Argument to Open is a file, not a directory.
	Readonly    = 0x20000   // Used in several functions to denote an object as readonly.
	WriteMap    = 0x80000   // Use a writable memory map.
	NoMetaSync  = 0x40000   // Don't fsync metapage after commit.
	NoSync      = 0x10000   // Don't fsync after commit.
	MapAsync    = 0x100000  // Flush asynchronously when using the WriteMap flag.
	NoTLS       = 0x200000  // Danger zone. When unset reader locktable slots are tied to their thread.
	NoLock      = 0x400000  // Danger zone. LMDB does not use any locks.
	NoReadahead = 0x800000  // Disable readahead. Requires OS support.
	NoMemInit   = 0x1000000 // Disable LMDB memory initialization.
)

// These flags are exclusively used in the Env.CopyFlags and Env.CopyFDFlags
// methods.
const (
	// Flags for Env.CopyFlags
	//
	// See mdb_env_copy2

	CopyCompact = 0x01 // Perform compaction while copying
)

// DBI is a handle for a database in an Env.
//
// See MDB_dbi
type DBI uint32

// Env is opaque structure for a database environment.  A DB environment
// supports multiple databases, all residing in the same shared-memory map.
//
// See MDB_env.
type Env struct {
	// eng is the open environment: nil before Open and after Close.
	eng atomic.Pointer[engine.Env]

	// closeLock keeps Close out of Open, the settings and the short Env
	// methods that use the open environment.  It is never held while
	// waiting for a writer lock: transactions (also their finalizers) and
	// copies do not take it (internal/engine/PORTING.md, "Lock order").
	closeLock sync.RWMutex

	// The state below is guarded by closeLock.
	created bool // made by NewEnv (C: mdb_env_create), not a zero Env
	opened  bool // Open succeeded
	closed  bool // Close was called

	// The settings of mdb_env_set_* made before Open (C keeps them in the
	// MDB_env that mdb_env_create returns; here Open hands them to the
	// engine): map size, readers, DBs, page size and the flags of
	// SetFlags.  reqFormat is the format requested with SetFormat.
	opts      engine.Options
	reqFormat Format

	// txns are the engine states of ended read transactions, cursors those
	// of closed read cursors: their memory is reused
	// (engine.Env.BeginRead, engine.Txn.OpenCursorInto), so that a read
	// transaction or cursor costs one small allocation, for its handle.
	txns    sync.Pool
	cursors sync.Pool
}

// NewEnv allocates and initializes a new Env.
//
// See mdb_env_create.
func NewEnv() (*Env, error) {
	// The environment itself is made by Open, with the settings made before
	// it (SetMapSize, SetMaxDBs, SetMaxReaders, SetFlags, SetFormat,
	// SetPageSize).
	env := &Env{created: true}

	runtime.SetFinalizer(env, (*Env).Close)
	return env, nil
}

// Open an environment handle. If this function fails Close() must be called to
// discard the Env handle.  Open passes flags|NoTLS to mdb_env_open.
//
// Open decides the on-disk format of the environment: an existing database
// is opened in its format (see SniffFormat); a new database uses the format
// requested with SetFormat, or the process-wide default (V09 unless changed).
// If the on-disk format conflicts with an explicitly requested format, Open
// fails with ErrFormatConflict.  Without the NoLock flag Open uses the
// environment's lock file, which a Linux or BSD program built without cgo
// cannot: Open returns ErrNoSharedLocking then (see SharedLockingAvailable).
// A failed Open may be retried.
//
// See mdb_env_open.
func (env *Env) Open(path string, flags uint, mode os.FileMode) error {
	env.closeLock.Lock()
	defer env.closeLock.Unlock()
	return env.open(path, NoTLS|flags, mode)
}

var errNotOpen = errors.New("enivornment is not open")
var errNegSize = errors.New("negative size")

// FD returns the open file descriptor (or Windows file handle) for the given
// environment.  An error is returned if the environment has not been
// successfully Opened (where C API just retruns an invalid handle).
//
// See mdb_env_get_fd.
func (env *Env) FD() (uintptr, error) {
	// fdInvalid is the value -1 as a uintptr, which is used by LMDB in the
	// case that env has not been opened yet.  the strange construction is done
	// to avoid constant value overflow errors at compile time.
	const fdInvalid = ^uintptr(0)

	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	fd := fdInvalid
	if e := env.eng.Load(); e != nil {
		fd = e.DataFile().Fd()
	}

	if fd == fdInvalid {
		return 0, errNotOpen
	}
	return fd, nil
}

// ReaderList dumps the contents of the reader lock table as text.  Readers
// start on the second line as space-delimited fields described by the first
// line.
//
// See mdb_reader_list.
func (env *Env) ReaderList(fn func(string) error) error {
	if fn == nil {
		return &OpError{Op: "mdb_reader_list", Errno: syscall.EINVAL}
	}
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	return env.readerList(fn)
}

// ReaderCheck clears stale entries from the reader lock table and returns the
// number of entries cleared.
//
// See mdb_reader_check()
func (env *Env) ReaderCheck() (int, error) {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	e, err := env.engine()
	if err != nil {
		return 0, err
	}
	dead, err := e.ReaderCheck()
	return dead, operr("mdb_reader_check", err)
}

func (env *Env) close() bool {
	env.closeLock.Lock()
	defer env.closeLock.Unlock()
	if env.closed {
		return false
	}
	env.closed = true
	env.opened = false
	if e := env.eng.Swap(nil); e != nil {
		e.Close()
	}
	return true
}

// Close shuts down the environment, releases the memory map, and clears the
// finalizer on env.
//
// Transactions should end before Close, as C requires.  One that outlives it
// can only end: its other methods and those of its cursors return EINVAL.
// The environment is torn down (map, reader slots, files) when the last of
// them ends (Abort, Commit or its finalizer); until then the slices a read
// transaction returned with RawRead before Close stay valid.
//
// See mdb_env_close.
func (env *Env) Close() error {
	if env.close() {
		runtime.SetFinalizer(env, nil)
		return nil
	}
	return errClosed
}

// CopyFD copies env to the the file descriptor fd.
//
// See mdb_env_copyfd.
func (env *Env) CopyFD(fd uintptr) error {
	return env.copyFD("mdb_env_copyfd", fd, 0)
}

// CopyFDFlag copies env to the file descriptor fd, with options.
//
// See mdb_env_copyfd2.
func (env *Env) CopyFDFlag(fd uintptr, flags uint) error {
	return env.copyFD("mdb_env_copyfd2", fd, flags)
}

// Copy copies the data in env to an environment at path.
//
// See mdb_env_copy.
func (env *Env) Copy(path string) error {
	return env.copy("mdb_env_copy", path, 0)
}

// CopyFlag copies the data in env to an environment at path created with flags.
//
// See mdb_env_copy2.
func (env *Env) CopyFlag(path string, flags uint) error {
	return env.copy("mdb_env_copy2", path, flags)
}

// Stat contains database status information.
//
// See MDB_stat.
type Stat struct {
	PSize         uint   // Size of a database page. This is currently the same for all databases.
	Depth         uint   // Depth (height) of the B-tree
	BranchPages   uint64 // Number of internal (non-leaf) pages
	LeafPages     uint64 // Number of leaf pages
	OverflowPages uint64 // Number of overflow pages
	Entries       uint64 // Number of data items
}

// Stat returns statistics about the environment.
//
// See mdb_env_stat.
func (env *Env) Stat() (*Stat, error) {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	e, err := env.engine()
	if err != nil {
		return nil, err
	}
	_stat, err := e.Stat()
	if err != nil {
		return nil, operr("mdb_env_stat", err)
	}
	stat := Stat{PSize: uint(_stat.PageSize),
		Depth:         uint(_stat.Depth),
		BranchPages:   _stat.BranchPages,
		LeafPages:     _stat.LeafPages,
		OverflowPages: _stat.OverflowPages,
		Entries:       _stat.Entries}
	return &stat, nil
}

// EnvInfo contains information an environment.
//
// See MDB_envinfo.
type EnvInfo struct {
	MapSize    int64 // Size of the data memory map
	LastPNO    int64 // ID of the last used page
	LastTxnID  int64 // ID of the last committed transaction
	MaxReaders uint  // maximum number of threads for the environment
	NumReaders uint  // maximum number of threads used in the environment
}

// Info returns information about the environment.
//
// See mdb_env_info.
func (env *Env) Info() (*EnvInfo, error) {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	e, err := env.engine()
	if err != nil {
		return nil, err
	}
	_info, err := e.Info()
	if err != nil {
		return nil, operr("mdb_env_info", err)
	}
	info := EnvInfo{
		MapSize:    _info.MapSize,
		LastPNO:    int64(_info.LastPgno),
		LastTxnID:  int64(_info.LastTxnID),
		MaxReaders: uint(_info.MaxReaders),
		NumReaders: uint(_info.NumReaders),
	}
	return &info, nil
}

// Sync flushes buffers to disk.  If force is true a synchronous flush occurs
// and ignores any NoSync or MapAsync flag on the environment.
//
// See mdb_env_sync.
func (env *Env) Sync(force bool) error {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	e, err := env.engine()
	if err != nil {
		return err
	}
	return operr("mdb_env_sync", e.Sync(force))
}

// SetFlags sets flags in the environment.
//
// See mdb_env_set_flags.
func (env *Env) SetFlags(flags uint) error {
	return env.setFlags(flags, true)
}

// UnsetFlags clears flags in the environment.
//
// See mdb_env_set_flags.
func (env *Env) UnsetFlags(flags uint) error {
	return env.setFlags(flags, false)
}

// Flags returns the flags set in the environment.
//
// See mdb_env_get_flags.
func (env *Env) Flags() (uint, error) {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	if e := env.eng.Load(); e != nil {
		return uint(e.Flags()), nil
	}
	if env.closed {
		return 0, errClosed
	}
	// Before Open: the flags given to SetFlags.
	return uint(env.opts.Flags), nil
}

// Path returns the path argument passed to Open.  Path returns a non-nil error
// if env.Open() was not previously called.
//
// See mdb_env_get_path.
func (env *Env) Path() (string, error) {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	e := env.eng.Load()
	if e == nil {
		return "", errNotOpen
	}
	return e.Path(), nil
}

// SetMapSize sets the size of the environment memory map.
//
// See mdb_env_set_mapsize.
func (env *Env) SetMapSize(size int64) error {
	if size < 0 {
		return errNegSize
	}
	env.closeLock.Lock()
	defer env.closeLock.Unlock()
	if e := env.eng.Load(); e != nil {
		// Read transactions may be active (they keep the map they began
		// with); EINVAL with a write transaction of this process, as in C.
		return operr("mdb_env_set_mapsize", e.SetMapSize(size))
	}
	if env.closed {
		return errClosed
	}
	env.opts.MapSize = size
	return nil
}

// SetMaxReaders sets the maximum number of reader slots in the environment.
//
// See mdb_env_set_maxreaders.
func (env *Env) SetMaxReaders(size int) error {
	if size < 0 {
		return errNegSize
	}
	env.closeLock.Lock()
	defer env.closeLock.Unlock()
	if env.closed {
		return errClosed
	}
	// As C: EINVAL once open, and for fewer than one reader. Not as C:
	// EINVAL for more than 1,048,576 readers, a 64 MiB lock file; C takes
	// any count and makes a 256 GiB lock file for four billion (rlmdb
	// KNOWN_C_BUGS.md row 60).
	if env.opened || size < 1 || size > format.MaxReadersLimit {
		return &OpError{Op: "mdb_env_set_maxreaders", Errno: syscall.EINVAL}
	}
	env.opts.MaxReaders = uint32(size)
	return nil
}

// MaxReaders returns the maximum number of reader slots for the environment.
//
// See mdb_env_get_maxreaders.
func (env *Env) MaxReaders() (int, error) {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	if e := env.eng.Load(); e != nil {
		return int(e.MaxReaders()), nil
	}
	if env.closed {
		return 0, errClosed
	}
	if env.opts.MaxReaders != 0 {
		return int(env.opts.MaxReaders), nil
	}
	return format.DefaultReaders, nil
}

// MaxKeySize returns the maximum allowed length for a key.
//
// See mdb_env_get_maxkeysize.
func (env *Env) MaxKeySize() int {
	if env == nil {
		return format.MaxKeySize09
	}
	return env.maxKeySize()
}

// SetMaxDBs sets the maximum number of named databases for the environment.
//
// See mdb_env_set_maxdbs.
func (env *Env) SetMaxDBs(size int) error {
	if size < 0 {
		return errNegSize
	}
	env.closeLock.Lock()
	defer env.closeLock.Unlock()
	if env.closed {
		return errClosed
	}
	// As C: EINVAL once open. Not as C: EINVAL for more than 1,048,576
	// databases; C takes any count and breaks above about 35 million
	// (rlmdb KNOWN_C_BUGS.md row 54).
	if env.opened || size > format.MaxDBsLimit {
		return &OpError{Op: "mdb_env_set_maxdbs", Errno: syscall.EINVAL}
	}
	env.opts.MaxDBs = uint32(size)
	return nil
}

// BeginTxn is an unsafe, low-level method to initialize a new transaction on
// env.  The Txn returned by BeginTxn is unmanaged and must be terminated by
// calling either its Abort or Commit methods to ensure that its resources are
// released.
//
// BeginTxn does not call runtime.LockOSThread.  Unless the Readonly flag is
// passed goroutines must call runtime.LockOSThread before calling BeginTxn and
// the returned Txn must not have its methods called from another goroutine.
// Failure to meet these restrictions can have undefined results that may
// include deadlocking your application.
//
// Instead of calling BeginTxn users should prefer calling the View and Update
// methods, which assist in management of Txn objects and provide OS thread
// locking required for write transactions.
//
// A finalizer detects unreachable, live transactions and logs thems to
// standard error.  The transactions are aborted, but their presence should be
// interpreted as an application error which should be patched so transactions
// are terminated explicitly.  Unterminated transactions can adversly effect
// database performance and cause the database to grow until the map is full.
//
// See mdb_txn_begin.
func (env *Env) BeginTxn(parent *Txn, flags uint) (*Txn, error) {
	txn, err := beginTxn(env, parent, flags)
	if txn != nil {
		runtime.SetFinalizer(txn, func(v interface{}) { v.(*Txn).finalize() })
	}
	return txn, err
}

// RunTxn creates a new Txn and calls fn with it as an argument.  Run commits
// the transaction if fn returns nil otherwise the transaction is aborted.
// Because RunTxn terminates the transaction goroutines should not retain
// references to it or its data after fn returns.
//
// RunTxn does not call runtime.LockOSThread.  Unless the Readonly flag is
// passed the calling goroutine should ensure it is locked to its thread and
// any goroutines started by fn must not call methods on the Txn object it is
// passed.
//
// See mdb_txn_begin.
func (env *Env) RunTxn(flags uint, fn TxnOp) error {
	return env.run(false, flags, fn)
}

// View creates a readonly transaction with a consistent view of the
// environment and passes it to fn.  View terminates its transaction after fn
// returns.  Any error encountered by View is returned.
//
// Unlike with Update transactions, goroutines created by fn are free to call
// methods on the Txn passed to fn provided they are synchronized in their
// accesses (e.g. using a mutex or channel).
//
// Any call to Commit, Abort, Reset or Renew on a Txn created by View will
// panic.
func (env *Env) View(fn TxnOp) error {
	return env.run(false, Readonly, fn)
}

// Update calls fn with a writable transaction.  Update commits the transaction
// if fn returns a nil error otherwise Update aborts the transaction and
// returns the error.
//
// Update calls runtime.LockOSThread to lock the calling goroutine to its
// thread and until fn returns and the transaction has been terminated, at
// which point runtime.UnlockOSThread is called.  If the calling goroutine is
// already known to be locked to a thread, use UpdateLocked instead to avoid
// premature unlocking of the goroutine.
//
// Neither Update nor UpdateLocked cannot be called safely from a goroutine
// where it isn't known if runtime.LockOSThread has been called.  In such
// situations writes must either be done in a newly created goroutine which can
// be safely locked, or through a worker goroutine that accepts updates to
// apply and delivers transaction results using channels.  See the package
// documentation and examples for more details.
//
// Goroutines created by the operation fn must not use methods on the Txn
// object that fn is passed.  Doing so would have undefined and unpredictable
// results for your program (likely including data loss, deadlock, etc).
//
// Any call to Commit, Abort, Reset or Renew on a Txn created by Update will
// panic.
func (env *Env) Update(fn TxnOp) error {
	return env.run(true, 0, fn)
}

// UpdateLocked behaves like Update but does not lock the calling goroutine to
// its thread.  UpdateLocked should be used if the calling goroutine is already
// locked to its thread for another purpose.
//
// Neither Update nor UpdateLocked cannot be called safely from a goroutine
// where it isn't known if runtime.LockOSThread has been called.  In such
// situations writes must either be done in a newly created goroutine which can
// be safely locked, or through a worker goroutine that accepts updates to
// apply and delivers transaction results using channels.  See the package
// documentation and examples for more details.
//
// Goroutines created by the operation fn must not use methods on the Txn
// object that fn is passed.  Doing so would have undefined and unpredictable
// results for your program (likely including data loss, deadlock, etc).
//
// Any call to Commit, Abort, Reset or Renew on a Txn created by UpdateLocked
// will panic.
func (env *Env) UpdateLocked(fn TxnOp) error {
	return env.run(false, 0, fn)
}

func (env *Env) run(lock bool, flags uint, fn TxnOp) error {
	if lock {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	txn, err := beginTxn(env, nil, flags)
	if err != nil {
		return err
	}
	return txn.runOpTerm(fn)
}

// CloseDBI closes the database handle, db.  Normally calling CloseDBI
// explicitly is not necessary.
//
// It is the caller's responsibility to serialize calls to CloseDBI.
//
// See mdb_dbi_close.
func (env *Env) CloseDBI(db DBI) {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	if e := env.eng.Load(); e != nil {
		e.CloseDBI(engine.DBI(db))
	}
}
