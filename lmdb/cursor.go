package lmdb

import (
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// These flags are used exclusively for Cursor.Get.
const (
	// Flags for Cursor.Get
	//
	// See MDB_cursor_op.

	First        = 0  // The first item.
	FirstDup     = 1  // The first value of current key (DupSort).
	GetBoth      = 2  // Get the key as well as the value (DupSort).
	GetBothRange = 3  // Get the key and the nearsest value (DupSort).
	GetCurrent   = 4  // Get the key and value at the current position.
	GetMultiple  = 5  // Get up to a page dup values for key at current position (DupFixed).
	Last         = 6  // Last item.
	LastDup      = 7  // Position at last value of current key (DupSort).
	Next         = 8  // Next value.
	NextDup      = 9  // Next value of the current key (DupSort).
	NextMultiple = 10 // Get key and up to a page of values from the next cursor position (DupFixed).
	NextNoDup    = 11 // The first value of the next key (DupSort).
	Prev         = 12 // The previous item.
	PrevDup      = 13 // The previous item of the current key (DupSort).
	PrevNoDup    = 14 // The last data item of the previous key (DupSort).
	Set          = 15 // The specified key.
	SetKey       = 16 // Get key and data at the specified key.
	SetRange     = 17 // The first key no less than the specified key.
)

// The MDB_MULTIPLE and MDB_RESERVE flags are special and do not fit the
// calling pattern of other calls to Put.  They are not exported because they
// require special methods, PutMultiple and PutReserve in which the flag is
// implied and does not need to be passed.
const (
	// Flags for Txn.Put and Cursor.Put.
	//
	// See mdb_put and mdb_cursor_put.

	Current     = 0x40    // Replace the item at the current key position (Cursor only)
	NoDupData   = 0x20    // Store the key-value pair only if key is not present (DupSort).
	NoOverwrite = 0x10    // Store a new key-value pair only if key is not present.
	Append      = 0x20000 // Append an item to the database.
	AppendDup   = 0x40000 // Append an item to the database (DupSort).
)

// Cursor operates on data inside a transaction and holds a position in the
// database.
//
// See MDB_cursor.
type Cursor struct {
	txn *Txn
	_c  *engine.Cursor // nil once closed

	// gen is the generation of txn (Txn.gen) _c was positioned in.
	gen uint32
	// pooled: _c belongs to a readonly transaction and goes back to the
	// environment's pool when closed.
	pooled bool
}

func openCursor(txn *Txn, db DBI) (*Cursor, error) {
	if txn._txn == nil {
		return nil, errInvalid("mdb_cursor_open")
	}
	c := &Cursor{txn: txn, gen: txn.gen}
	if txn.readonly {
		// The state of a closed read cursor is reused: nothing is
		// allocated but the Cursor.
		c._c = txn.env.readCursor()
		c.pooled = true
		if err := txn._txn.OpenCursorInto(c._c, engine.DBI(db)); err != nil {
			txn.env.cursors.Put(c._c)
			return nil, operr("mdb_cursor_open", err)
		}
		return c, nil
	}
	_c, err := txn._txn.OpenCursor(engine.DBI(db))
	if err != nil {
		return nil, operr("mdb_cursor_open", err)
	}
	c._c = _c
	return c, nil
}

// Renew associates readonly cursor with txn.
//
// txn must belong to the same Env as the transaction the cursor was opened
// in: renewing it with a transaction from another environment returns
// EINVAL.
//
// See mdb_cursor_renew.
func (c *Cursor) Renew(txn *Txn) error {
	if c._c == nil || txn == nil || txn._txn == nil || !c.pooled || !txn.readonly || txn.env != c.txn.env {
		// Closed cursors and transactions, cursors of write transactions,
		// write transactions, and another environment's transactions (C
		// does not check that one and corrupts memory).
		return errInvalid("mdb_cursor_renew")
	}
	// mdb_cursor_renew is mdb_cursor_open into the cursor; not through
	// the old transaction, whose state may serve another one by now.
	err := operr("mdb_cursor_renew", txn._txn.OpenCursorInto(c._c, c._c.DBI()))
	if err != nil {
		return err
	}
	c.txn = txn
	c.gen = txn.gen
	return nil
}

func (c *Cursor) close() bool {
	if c._c != nil {
		if c.pooled {
			// A read cursor holds nothing but its memory.
			c.txn.env.cursors.Put(c._c)
		} else if c.txn._txn == nil && !c.txn.readonly {
			// the cursor has already been released by LMDB.
		} else {
			c._c.Close()
		}
		c.txn = nil
		c._c = nil
		return true
	}
	return false
}

// Close the cursor handle.  Cursors belonging to
// write transactions are closed automatically when the transaction is
// terminated.
//
// See mdb_cursor_close.
func (c *Cursor) Close() {
	c.close()
}

// Txn returns the cursor's transaction.
func (c *Cursor) Txn() *Txn {
	return c.txn
}

// DBI returns the cursor's database handle.  If c has been closed than an
// invalid DBI is returned.
func (c *Cursor) DBI() DBI {
	// dbiInvalid is an invalid DBI (the max value for the type).  it shouldn't
	// be possible to create a database handle with value dbiInvalid because
	// the process address space would be exhausted.  it is also impractical to
	// have many open databases in an environment.
	const dbiInvalid = ^DBI(0)

	if c._c == nil {
		return dbiInvalid
	}
	return DBI(c._c.DBI())
}

// Get retrieves items from the database. If c.Txn().RawRead is true the slices
// returned by Get reference readonly sections of memory that must not be
// accessed after the transaction has terminated.
//
// In a Txn with RawRead set to true the Set op causes the returned key to
// share its memory with setkey (making it writable memory). In a Txn with
// RawRead set to false the Set op returns key values with memory distinct from
// setkey, as is always the case when using RawRead.
//
// Get ignores setval if setkey is empty.
//
// With GetMultiple on a key that has a single value, val is that value (a
// page of one item); lmdb-go v1 returned an empty slice.  FirstDup and
// LastDup return the current key.
//
// See mdb_cursor_get.
func (c *Cursor) Get(setkey, setval []byte, op uint) (key, val []byte, err error) {
	if err = c.ready("mdb_cursor_get"); err != nil {
		return nil, nil, err
	}
	switch {
	case len(setkey) == 0:
		key, val, err = c.getVal0(op)
	case len(setval) == 0:
		key, val, err = c.getVal1(setkey, op)
	default:
		key, val, err = c.getVal2(setkey, setval, op)
	}
	if err == nil && op == GetMultiple {
		val, err = c.multipleOfOne(val)
	}
	if err != nil {
		return nil, nil, cursorGetErr(err)
	}

	// When MDB_SET is passed to mdb_cursor_get its first argument will be
	// returned unchanged.
	if op == Set {
		key = setkey
	}
	if c.txn.RawRead {
		return key, val, nil
	}
	// Key and value in one allocation.
	key, val = copyPair(key, val)

	return key, val, nil
}

// getVal0 retrieves items from the database without using given key or value
// data for reference (Next, First, Last, etc).
//
// See mdb_cursor_get.
func (c *Cursor) getVal0(op uint) ([]byte, []byte, error) {
	return c._c.Get(eb[:0], eb[:0], format.CursorOp(op))
}

// getVal1 retrieves items from the database using key data for reference
// (Set, SetRange, etc).
//
// See mdb_cursor_get.
func (c *Cursor) getVal1(setkey []byte, op uint) ([]byte, []byte, error) {
	return c._c.Get(setkey, eb[:0], format.CursorOp(op))
}

// getVal2 retrieves items from the database using key and value data for
// reference (GetBoth, GetBothRange, etc).
//
// See mdb_cursor_get.
func (c *Cursor) getVal2(setkey, setval []byte, op uint) ([]byte, []byte, error) {
	return c._c.Get(setkey, setval, format.CursorOp(op))
}

func (c *Cursor) putNilKey(flags uint) error {
	err := c._c.Put(eb[:0], eb[:0], uint32(flags))
	return operr("mdb_cursor_put", err)
}

// Put stores an item in the database.
//
// See mdb_cursor_put.
func (c *Cursor) Put(key, val []byte, flags uint) error {
	if err := c.ready("mdb_cursor_put"); err != nil {
		return err
	}
	kn := len(key)
	if kn == 0 {
		return c.putNilKey(flags)
	}
	vn := len(val)
	if vn == 0 {
		val = eb[:0]
	}
	err := c._c.Put(key, val, uint32(flags))
	return operr("mdb_cursor_put", err)
}

// PutReserve returns a []byte of length n that can be written to, potentially
// avoiding a memcopy.  The returned byte slice is only valid in txn's thread,
// before it has terminated.
func (c *Cursor) PutReserve(key []byte, n int, flags uint) ([]byte, error) {
	if err := c.ready("mdb_cursor_put"); err != nil {
		return nil, err
	}
	if len(key) == 0 {
		return nil, c.putNilKey(flags)
	}
	if n < 0 {
		// A size_t above MAXDATASIZE in C.
		return nil, &OpError{Op: "mdb_cursor_put", Errno: BadValSize}
	}

	b, err := c._c.PutReserve(key, n, uint32(flags))
	if err != nil {
		return nil, operr("mdb_cursor_put", err)
	}
	return b, nil
}

// PutMulti stores a set of contiguous items with stride size under key.
// PutMulti panics if len(page) is not a multiple of stride.  The cursor's
// database must be DupFixed and DupSort.
// An empty page or a stride below 1 is EINVAL.
//
// See mdb_cursor_put.
func (c *Cursor) PutMulti(key []byte, page []byte, stride int, flags uint) error {
	if err := c.ready("mdb_cursor_put"); err != nil {
		return err
	}
	if len(key) == 0 {
		return c.putNilKey(flags)
	}
	if len(page) == 0 || stride < 1 {
		// lmdb-go v1 stored one zero byte (stride 1) or panicked.
		return &OpError{Op: "mdb_cursor_put", Errno: syscall.EINVAL}
	}

	vn := WrapMulti(page, stride).Len()
	_, err := c._c.PutMulti(key, page[:vn*stride], stride, uint32(flags))
	return operr("mdb_cursor_put", err)
}

// Del deletes the item referred to by the cursor from the database.
//
// See mdb_cursor_del.
func (c *Cursor) Del(flags uint) error {
	if err := c.ready("mdb_cursor_del"); err != nil {
		return err
	}
	err := c._c.Del(uint32(flags))
	return operr("mdb_cursor_del", err)
}

// Count returns the number of duplicates for the current key.
//
// See mdb_cursor_count.
func (c *Cursor) Count() (uint64, error) {
	if err := c.ready("mdb_cursor_count"); err != nil {
		return 0, err
	}
	_size, err := c._c.Count()
	if err != nil {
		return 0, operr("mdb_cursor_count", err)
	}
	return _size, nil
}
