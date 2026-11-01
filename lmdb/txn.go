package lmdb

import (
	"log"
	"runtime"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/engine"
)

// This flags are used exclusively for Txn.OpenDBI and Txn.OpenRoot.  The
// Create flag must always be supplied when opening a non-root DBI for the
// first time. When a DBI is created, the other flags will be persisted in the
// LMDB and automatically applied when opening the DBI.
const (
	// Flags for Txn.OpenDBI.

	ReverseKey = 0x02    // Use reverse string keys.
	DupSort    = 0x04    // Use sorted duplicates.
	DupFixed   = 0x10    // Duplicate items have a fixed size (DupSort).
	ReverseDup = 0x40    // Reverse duplicate values (DupSort).
	Create     = 0x40000 // Create DB if not already existing.

	// IntegerKey and IntegerDup should only be used when needed for
	// compatibility with other programs that require them. The keys will be
	// interpreted as in-memory integers and sorted according to the endianness
	// of the system. The keys returned by the API calls will still be
	// []byte slices.
	IntegerKey = 0x08 // Use native integer order.
	IntegerDup = 0x20 // Duplicate integers (DupSort).
)

// Txn is a database transaction in an environment.
//
// WARNING: A writable Txn is not threadsafe and may only be used in the
// goroutine that created it.
//
// After Env.Close a Txn can only end: Abort and Commit (which aborts a write
// transaction and returns EINVAL) end it, every other method returns EINVAL
// (Reset does nothing), and so do the methods of its cursors.
//
// See MDB_txn.
type Txn struct {
	// If RawRead is true []byte values retrieved from Get() calls on the Txn
	// and its cursors will point directly into the memory-mapped structure.
	// Such slices will be readonly and must only be referenced wthin the
	// transaction's lifetime.
	RawRead bool

	// Pooled may be set to true while a Txn is stored in a sync.Pool, after
	// Txn.Reset reset has been called and before Txn.Renew.  This will keep
	// the Txn finalizer from unnecessarily warning the application about
	// finalizations.
	Pooled bool

	managed  bool
	readonly bool

	// pooled: _txn came from env.txns and goes back there when the
	// transaction ends.
	pooled bool

	// gen counts Reset and Renew.  A cursor remembers the generation it was
	// opened or renewed in; after a Reset and Renew without Cursor.Renew it
	// starts over unpositioned, as in rlmdb's C API (in C it would keep
	// pointers into the old snapshot).
	gen uint32

	env *Env
	// _txn is the transaction; nil once it has ended (Commit, Abort, or
	// the end of its parent).
	_txn *engine.Txn
	// tok is _txn's Owner when the transaction began.  The engine reuses
	// the state of ended transactions (the environment's write
	// transaction, as C reuses me_txn0; a parent's next child; pooled read
	// transactions): a handle whose tok is not _txn's Owner any more must
	// not touch it (see ended).
	tok uint64
	// child is the nested transaction begun last in this one.  While it
	// is active it ends with this one (endChildren), and this reference
	// keeps its finalizer from running while this one lives: it would end
	// the child on the finalizer's goroutine, changing this transaction
	// under its user.
	child *Txn

	errLogf func(format string, v ...interface{})
}

// beginTxn does not lock the OS thread which is a prerequisite for creating a
// write transaction.
func beginTxn(env *Env, parent *Txn, flags uint) (*Txn, error) {
	e := env.eng.Load()
	if parent != nil && (e == nil || parent.ended()) {
		// An ended parent, or one that outlived Env.Close.
		return nil, errInvalid("mdb_txn_begin")
	}
	if e == nil {
		// The environment exists only after a successful Open.
		return nil, errNotOpen
	}
	txn := &Txn{
		readonly: (flags&Readonly != 0),
		env:      env,
	}

	var ptxn *engine.Txn
	if parent == nil {
		if flags&Readonly != 0 {
			// Readonly transaction: the state of an ended one is reused,
			// so that nothing is allocated but the Txn.  It goes back to
			// env.txns in clearTxn(), which is always called at the end of
			// a transaction through Commit() or Abort().
			txn._txn = env.readTxn()
			txn.pooled = true
			if err := e.BeginRead(txn._txn); err != nil {
				txn.clearTxn()
				return nil, operr("mdb_txn_begin", err)
			}
			txn.tok = txn._txn.Owner()
			return txn, nil
		}
	} else {
		ptxn = parent._txn
	}
	_txn, err := e.BeginTxn(ptxn, uint32(flags))
	if err != nil {
		return nil, operr("mdb_txn_begin", err)
	}
	txn._txn = _txn
	txn.tok = _txn.Owner()
	if parent != nil {
		parent.child = txn
	}
	return txn, nil
}

// ID returns the identifier for txn.  A view transaction identifier
// corresponds to the Env snapshot being viewed and may be shared with other
// view transactions.
//
// See mdb_txn_id.
func (txn *Txn) ID() uintptr {
	// It is possible for a txn to legitimately have ID 0 if it a readonly txn
	// created before any updates.  It is 0 after the transaction ended and
	// while a readonly transaction is reset.
	return txn.getID()
}

func (txn *Txn) getID() uintptr {
	if txn.ended() {
		return 0
	}
	return uintptr(txn._txn.ID())
}

// RunOp executes fn with txn as an argument.  During the execution of fn no
// goroutine may call the Commit, Abort, Reset, and Renew methods on txn.
// RunOp returns the result of fn without any further action.  RunOp will not
// abort txn if fn returns an error, unless terminate is true.  If terminate is
// true then RunOp will attempt to commit txn if fn is successful, otherwise
// RunOp will abort txn before returning any failure encountered.
//
// RunOp primarily exists to allow applications and other packages to provide
// variants of the managed transactions provided by lmdb (i.e. View, Update,
// etc).  For example, the lmdbpool package used RunOp to provide an
// Txn-friendly sync.Pool and a function analogous to Env.View that uses
// transactions from that pool.
func (txn *Txn) RunOp(fn TxnOp, terminate bool) error {
	if terminate {
		return txn.runOpTerm(fn)
	}
	return txn.runOp(fn)
}

func (txn *Txn) runOpTerm(fn TxnOp) error {
	if txn.managed {
		panic("managed transaction cannot be terminated directly")
	}
	defer txn.abort()

	// There is no need to restore txn.managed after fn has executed because
	// the Txn will terminate one way or another using methods which don't
	// check txn.managed.
	txn.managed = true

	err := fn(txn)
	if err != nil {
		return err
	}

	return txn.commit()
}

func (txn *Txn) runOp(fn TxnOp) error {
	if !txn.managed {
		// Restoring txn.managed must be done in a deferred call otherwise the
		// caller may not be able to abort the transaction if a runtime panic
		// occurs (attempting to do so would cause another panic).
		txn.managed = true
		defer func() {
			txn.managed = false
		}()
	}
	return fn(txn)
}

// Commit persists all transaction operations to the database and clears the
// finalizer on txn.  A Txn cannot be used again after Commit is called.
//
// A write transaction committed on another OS thread than the one that
// began it is committed, but Commit returns an error wrapping
// ErrTxnWrongThread (and syscall.EPERM, on Windows ERROR_NOT_OWNER) on
// Linux, FreeBSD and Windows, whose writer mutex stays with the thread that
// began the transaction until that thread exits.
//
// See mdb_txn_commit.
func (txn *Txn) Commit() error {
	if txn.managed {
		panic("managed transaction cannot be committed directly")
	}

	runtime.SetFinalizer(txn, nil)
	return txn.commit()
}

func (txn *Txn) commit() error {
	if txn.ended() {
		// mdb_txn_commit(NULL)
		return errInvalid("mdb_txn_commit")
	}
	var err error
	if txn.readonly {
		// Ends the transaction, which is safe after Env.Close (v1 did not
		// lock here either).
		err = txn._txn.Commit()
	} else if txn.env.eng.Load() == nil {
		// After Env.Close (DEVIATIONS.md).  No closeLock: a commit never
		// waits for Close, which may be queued behind a copy that waits
		// for this writer.  A Close during the commit is safe: the
		// transaction keeps the environment until it ends.
		txn._txn.Abort()
		err = syscall.EINVAL
	} else {
		err = txn._txn.Commit()
	}
	txn.clearTxn()
	return operr("mdb_txn_commit", err)
}

// Abort discards pending writes in the transaction and clears the finalizer on
// txn.  A Txn cannot be used again after Abort is called.
//
// Abort reports no error, as mdb_txn_abort: a write transaction aborted on
// another OS thread than the one that began it is aborted, and on Linux,
// FreeBSD and Windows its writer mutex stays with the beginning thread
// until that thread exits (see ErrTxnWrongThread).
//
// See mdb_txn_abort.
func (txn *Txn) Abort() {
	if txn.managed {
		panic("managed transaction cannot be aborted directly")
	}

	runtime.SetFinalizer(txn, nil)
	txn.abort()
}

func (txn *Txn) abort() {
	if txn.ended() {
		return
	}

	// Without closeLock, as commit: the engine keeps the environment for
	// the transaction until it ends, also after Close.
	txn._txn.Abort()

	txn.clearTxn()
}

func (txn *Txn) clearTxn() {
	// Clear the transaction to prevent any potential future use of it, which
	// fails cleanly from now on.
	t := txn._txn
	txn._txn = nil
	txn.endChildren()

	// Return the state of a readonly transaction for reuse.
	if txn.pooled {
		txn.pooled = false
		txn.env.txns.Put(t)
	}
}

// Reset aborts the transaction clears internal state so the transaction may be
// reused by calling Renew.  If txn is not going to be reused txn.Abort() must
// be called to release its slot in the lock table and free its memory.  Reset
// panics if txn is managed by Update, View, etc.
//
// See mdb_txn_reset.
func (txn *Txn) Reset() {
	if txn.managed {
		panic("managed transaction cannot be reset directly")
	}

	txn.reset()
}

func (txn *Txn) reset() {
	if !txn.invalid() {
		txn._txn.Reset()
		txn.gen++
	}
}

// Renew reuses a transaction that was previously reset by calling txn.Reset().
// Renew panics if txn is managed by Update, View, etc.
//
// See mdb_txn_renew.
func (txn *Txn) Renew() error {
	if txn.managed {
		panic("managed transaction cannot be renewed directly")
	}

	return txn.renew()
}

func (txn *Txn) renew() error {
	if txn.invalid() {
		return errInvalid("mdb_txn_renew")
	}
	err := txn._txn.Renew()
	txn.gen++

	// mdb_txn_renew causes txn._txn to pick up a new transaction ID.  It's
	// slightly confusing in the LMDB docs.  Txn ID corresponds to database
	// snapshot the reader is holding, which is good because renewed
	// transactions can see updates which happened since they were created (or
	// since they were last renewed).  It should follow that renewing a Txn
	// results in the freeing of stale pages the Txn has been holding, though
	// this has not been confirmed in any way by bmatsuo as of 2017-02-15.

	return operr("mdb_txn_renew", err)
}

// OpenDBI opens a named database in the environment.  An error is returned if
// name is empty.  The DBI returned by OpenDBI can be used in other
// transactions but not before Txn has terminated.
//
// OpenDBI can only be called after env.SetMaxDBs() has been called to set the
// maximum number of named databases.
//
// The C API uses null terminated strings for database names.  A consequence is
// that names cannot contain null bytes themselves. OpenDBI does not check for
// null bytes in the name argument.
//
// See mdb_dbi_open.
func (txn *Txn) OpenDBI(name string, flags uint) (DBI, error) {
	cname := cString(name)
	dbi, err := txn.openDBI(cname, flags)
	return dbi, err
}

// CreateDBI is a shorthand for OpenDBI that passed the flag lmdb.Create.
func (txn *Txn) CreateDBI(name string) (DBI, error) {
	return txn.OpenDBI(name, Create)
}

// Flags returns the database flags for handle dbi.
func (txn *Txn) Flags(dbi DBI) (uint, error) {
	if txn.invalid() {
		return 0, errInvalid("mdb_dbi_flags")
	}
	cflags, err := txn._txn.DBIFlags(engine.DBI(dbi))
	return uint(cflags), operr("mdb_dbi_flags", err)
}

// OpenRoot opens the root database.  OpenRoot behaves similarly to OpenDBI but
// does not require env.SetMaxDBs() to be called beforehand.  And, OpenRoot can
// be called without flags in a View transaction.
func (txn *Txn) OpenRoot(flags uint) (DBI, error) {
	return txn.openDBI(nil, flags)
}

// openDBI returns returns whatever DBI value was set by mdb_open_dbi.  In an
// error case, LMDB does not currently set DBI in case of failure, so zero is
// returned in those cases.  This is not a big deal for now because
// applications are expected to handle any error encountered opening a
// database.
func (txn *Txn) openDBI(cname []byte, flags uint) (DBI, error) {
	if txn.invalid() {
		return 0, errInvalid("mdb_dbi_open")
	}
	dbi, err := txn._txn.OpenDBI(cname, uint32(flags))
	if err != nil {
		dbi = 0
	}
	return DBI(dbi), operr("mdb_dbi_open", err)
}

// Stat returns a Stat describing the database dbi.
//
// See mdb_stat.
func (txn *Txn) Stat(dbi DBI) (*Stat, error) {
	if txn.invalid() {
		return nil, errInvalid("mdb_stat")
	}
	_stat, err := txn._txn.Stat(engine.DBI(dbi))
	if err != nil {
		return nil, operr("mdb_stat", err)
	}
	stat := Stat{
		PSize:         uint(_stat.PageSize),
		Depth:         uint(_stat.Depth),
		BranchPages:   _stat.BranchPages,
		LeafPages:     _stat.LeafPages,
		OverflowPages: _stat.OverflowPages,
		Entries:       _stat.Entries,
	}
	return &stat, nil
}

// Drop empties the database if del is false.  Drop deletes and closes the
// database if del is true.
//
// See mdb_drop.
func (txn *Txn) Drop(dbi DBI, del bool) error {
	if txn.invalid() {
		return errInvalid("mdb_drop")
	}
	return operr("mdb_drop", txn._txn.Drop(engine.DBI(dbi), del))
}

// Sub executes fn in a subtransaction.  Sub commits the subtransaction iff a
// nil error is returned by fn and otherwise aborts it.  Sub returns any error
// it encounters.
//
// Sub may only be called on an Update Txn (one created without the Readonly
// flag).  Calling Sub on a View transaction will return an error.  Sub assumes
// the calling goroutine is locked to an OS thread and will not call
// runtime.LockOSThread.
//
// Any call to Abort, Commit, Renew, or Reset on a Txn created by Sub will
// panic.
func (txn *Txn) Sub(fn TxnOp) error {
	// As of 0.9.14 Readonly is the only Txn flag and readonly subtransactions
	// don't make sense.
	return txn.subFlag(0, fn)
}

func (txn *Txn) subFlag(flags uint, fn TxnOp) error {
	sub, err := beginTxn(txn.env, txn, flags)
	if err != nil {
		return err
	}
	sub.managed = true
	defer sub.abort()
	err = fn(sub)
	if err != nil {
		return err
	}
	return sub.commit()
}

func (txn *Txn) bytes(val []byte) []byte {
	if txn.RawRead {
		return val
	}
	return getBytesCopy(val)
}

// Get retrieves items from database dbi.  If txn.RawRead is true the slice
// returned by Get references a readonly section of memory that must not be
// accessed after txn has terminated.
//
// See mdb_get.
func (txn *Txn) Get(dbi DBI, key []byte) ([]byte, error) {
	if txn.invalid() {
		return nil, errInvalid("mdb_get")
	}
	kdata, kn := valBytes(key)
	val, err := txn._txn.Get(engine.DBI(dbi), kdata[:kn])
	if err != nil {
		return nil, getErr(err)
	}
	b := txn.bytes(val)
	return b, nil
}

func (txn *Txn) putNilKey(dbi DBI, flags uint) error {
	// mdb_put with an empty key will always fail
	err := txn._txn.Put(engine.DBI(dbi), eb[:0], eb[:0], uint32(flags))
	return operr("mdb_put", err)
}

// Put stores an item in database dbi.
//
// See mdb_put.
func (txn *Txn) Put(dbi DBI, key []byte, val []byte, flags uint) error {
	if txn.invalid() {
		return errInvalid("mdb_put")
	}
	kn := len(key)
	if kn == 0 {
		return txn.putNilKey(dbi, flags)
	}
	vn := len(val)
	if vn == 0 {
		val = eb[:0]
	}

	err := txn._txn.Put(engine.DBI(dbi), key, val, uint32(flags))
	return operr("mdb_put", err)
}

// PutReserve returns a []byte of length n that can be written to, potentially
// avoiding a memcopy.  The returned byte slice is only valid in txn's thread,
// before it has terminated.
func (txn *Txn) PutReserve(dbi DBI, key []byte, n int, flags uint) ([]byte, error) {
	if txn.invalid() {
		return nil, errInvalid("mdb_put")
	}
	if len(key) == 0 {
		return nil, txn.putNilKey(dbi, flags)
	}
	if n < 0 {
		// A size_t above MAXDATASIZE in C.
		return nil, &OpError{Op: "mdb_put", Errno: BadValSize}
	}
	b, err := txn._txn.PutReserve(engine.DBI(dbi), key, n, uint32(flags))
	if err != nil {
		return nil, operr("mdb_put", err)
	}
	return b, nil
}

// Del deletes an item from database dbi.  Del ignores val unless dbi has the
// DupSort flag.
// For a DupSort database a nil val deletes all values of key (mdb_del with
// NULL data); lmdb-go v1 passed an empty value instead, which C looked for.
//
// See mdb_del.
func (txn *Txn) Del(dbi DBI, key, val []byte) error {
	if txn.invalid() {
		return errInvalid("mdb_del")
	}
	kdata, kn := valBytes(key)
	vdata := val // nil is NULL
	err := txn._txn.Del(engine.DBI(dbi), kdata[:kn], vdata)
	return operr("mdb_del", err)
}

// OpenCursor allocates and initializes a Cursor to database dbi.
//
// See mdb_cursor_open.
func (txn *Txn) OpenCursor(dbi DBI) (*Cursor, error) {
	// A cursor holds nothing that needs a finalizer (v1 freed the C cursor
	// of a readonly transaction in one).
	cur, err := openCursor(txn, dbi)
	return cur, err
}

func (txn *Txn) errf(format string, v ...interface{}) {
	if txn.errLogf != nil {
		txn.errLogf(format, v...)
		return
	}
	log.Printf(format, v...)
}

func (txn *Txn) finalize() {
	if !txn.ended() {
		if !txn.Pooled {
			txn.errf("lmdb: aborting unreachable transaction %p", txn)
		}

		txn.abort()
	}
}

// TxnOp is an operation applied to a managed transaction.  The Txn passed to a
// TxnOp is managed and the operation must not call Commit, Abort, Renew, or
// Reset on it.
//
// IMPORTANT:
// TxnOps that write to the database (those passed to Env.Update or Txn.Sub)
// must not use the Txn in another goroutine (passing it directly or otherwise
// through closure).  Doing so has undefined results.
type TxnOp func(txn *Txn) error
