// The cursor API: Cursor and mdb_cursor_get's dispatch (rlmdb
// core/ro_cursor.rs, and the handle cursors of core/txn_rw.rs).
//
// A Cursor of a read transaction holds its read cursor state (rcursor
// main and sub-cursor). A Cursor of a write transaction names a slot of
// the transaction's cursor arena (wcursor.go), which the write path keeps
// up to date across changes as C keeps mt_cursors; read operations on it
// rebuild read cursors in the Cursor's own scratch space, run the same
// code, and store the positions back.
//
// Differences to the C signature: key/data inputs are plain slices (nil
// for "not given"), and the position is returned. MDB_NOTFOUND is an
// error, as in C. Operations that in C leave key untouched (MDB_SET,
// MDB_GET_BOTH, MDB_GET_MULTIPLE, ...) return the key stored in the
// database.

package engine

import (
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// Cursor is MDB_cursor over one database of a transaction.
type Cursor struct {
	txn *Txn
	dbi DBI
	// mc and mx are the main cursor and the sub-cursor (MDB_DUPSORT only,
	// hasX). For a write transaction they are scratch space for read
	// operations.
	mc   rcursor
	mx   rcursor
	hasX bool
	// dcmp is md_dcmp, the duplicate order.
	dcmp    format.Comparator
	hasDcmp bool
	// id is the arena slot of a write transaction's cursor.
	id     cursorID
	closed bool
}

// OpenCursor is mdb_cursor_open: a cursor over dbi. In a write
// transaction it is tracked: it stays valid across all changes made in
// the transaction (and its nested transactions) until Close or the end of
// the transaction. EINVAL for the free DB in a write transaction, as in C.
func (t *Txn) OpenCursor(dbi DBI) (*Cursor, error) {
	c := new(Cursor)
	if err := t.OpenCursorInto(c, dbi); err != nil {
		return nil, err
	}
	return c, nil
}

// OpenCursorInto is OpenCursor into a caller's Cursor (for pooled
// cursors; nothing is allocated for a read transaction).
func (t *Txn) OpenCursorInto(c *Cursor, dbi DBI) error {
	if t.w != nil {
		id, err := t.openWriteCursor(dbi)
		if err != nil {
			return err
		}
		*c = Cursor{txn: t, dbi: dbi, id: id}
		return nil
	}
	if _, err := t.dbiEntry(dbi, dbValid); err != nil {
		return err
	}
	if err := t.usable(); err != nil {
		return err
	}
	e, err := t.freshDBI(dbi, dbValid)
	if err != nil {
		return err
	}
	c.txn, c.dbi, c.closed, c.id = t, dbi, false, cursorID{}
	c.mc.init(e.db, e.keyCmp, 0, t.l)
	c.hasX = newXCursor(&c.mx, e, t.l)
	c.dcmp, c.hasDcmp = e.dupCmp, e.hasDup
	return nil
}

// Renew is mdb_cursor_renew: bind a cursor of a read transaction to the
// read transaction t (the same or another one), unpositioned. EINVAL for
// write transactions, as in C.
func (c *Cursor) Renew(t *Txn) error {
	if t.w != nil || (c.txn != nil && c.txn.w != nil) {
		return syscall.EINVAL
	}
	return t.OpenCursorInto(c, c.dbi)
}

// Close is mdb_cursor_close. Closing a cursor of an ancestor transaction
// through a nested one does nothing, as in C; it stays the ancestor's.
func (c *Cursor) Close() {
	if c.closed {
		return
	}
	c.closed = true
	if c.txn != nil && c.txn.w != nil {
		c.txn.closeWriteCursor(c.id)
	}
	c.Forget()
}

// Forget drops the cursor's pages (rcursor.forget), which point into its
// transaction's map, before package lmdb keeps a closed read cursor for
// reuse: a kept cursor must not keep pointers into a map that may be
// unmapped. It touches nothing but the cursor (not its transaction, whose
// state may serve another transaction by now). OpenCursorInto starts it
// over.
func (c *Cursor) Forget() {
	c.mc.forget()
	c.mx.forget()
}

// Txn is the cursor's transaction (mdb_cursor_txn): for a write cursor
// used inside a nested transaction, the transaction it was opened in.
func (c *Cursor) Txn() *Txn { return c.txn }

// DBI is mdb_cursor_dbi.
func (c *Cursor) DBI() DBI { return c.dbi }

// Get is mdb_cursor_get: perform op. key is required by Set, SetKey,
// SetRange, GetBoth and GetBothRange; data by GetBoth and GetBothRange
// (EINVAL if missing).
func (c *Cursor) Get(key, data []byte, op format.CursorOp) (k, v []byte, err error) {
	if c.closed {
		return nil, nil, syscall.EINVAL
	}
	t := c.txn
	if t.w != nil {
		return t.writeCursorGet(c, key, data, op)
	}
	if t.state != txnActive {
		return nil, nil, format.BadTxn
	}
	// MDB_NEXT within a page needs none of the dispatch: scans call it
	// once per item.
	if op == format.Next {
		if k, v, ok := stepInLeaf(&c.mc, true); ok {
			return k, v, nil
		}
	}
	return cursorGet(t, &c.mc, c.xc(), c.dcmp, c.hasDcmp, key, data, op)
}

func (c *Cursor) xc() *rcursor {
	if c.hasX {
		return &c.mx
	}
	return nil
}

// Count is mdb_cursor_count: the number of data items of the current key
// (1 for a key without duplicates). MDB_INCOMPATIBLE unless DUPSORT;
// MDB_NOTFOUND if the cursor is not on an item (KNOWN_C_BUGS.md #5).
func (c *Cursor) Count() (uint64, error) {
	if c.closed {
		return 0, syscall.EINVAL
	}
	t := c.txn
	if t.w != nil {
		return t.writeCursorCount(c)
	}
	if t.state != txnActive {
		return 0, format.BadTxn
	}
	return cursorCount(&c.mc, c.xc())
}

// IsInitialized reports whether the cursor is positioned (C_INITIALIZED).
func (c *Cursor) IsInitialized() bool {
	if c.txn != nil && c.txn.w != nil {
		w, err := c.txn.cursorState(c.id)
		return err == nil && w.isInit()
	}
	return c.mc.isInit()
}

// cursorGet is mdb_cursor_get (mdb.c line 6458) on read cursors mc and mx
// (nil unless DUPSORT): validate op against the database type and
// dispatch. The tail clears C_DEL.
func cursorGet(t *Txn, mc, mx *rcursor, dcmp format.Comparator, hasDcmp bool,
	key, data []byte, op format.CursorOp) (k, v []byte, err error) {
	k, v, err = cursorGetInner(t, mc, mx, dcmp, hasDcmp, key, data, op)
	if mc.flags&cDel != 0 {
		mc.flags ^= cDel
	}
	return k, v, err
}

func requireDupFixed(mc, mx *rcursor) error {
	if uint32(mc.db.Flags)&format.DupFixed == 0 || mx == nil {
		return format.Incompatible
	}
	return nil
}

// currentHasDups reports whether the cursor is on an F_DUPDATA node.
func currentHasDups(mc *rcursor) bool {
	if !mc.isInit() || mc.snum == 0 {
		return false
	}
	p := mc.topPage()
	if p.IsLeaf2() {
		return false
	}
	n, ok := p.Node(mc.kiTop())
	return ok && n.IsDupData()
}

// currentIsDB is 1.0's mdb_cursor_is_db: the cursor is on the record of a
// named database (F_SUBDATA without F_DUPDATA).
func currentIsDB(mc *rcursor) bool {
	if !mc.isInit() || mc.snum == 0 {
		return false
	}
	p := mc.topPage()
	if p.IsLeaf2() {
		return false
	}
	n, ok := p.Node(mc.kiTop())
	return ok && n.Flags()&(format.FDupData|format.FSubData) == format.FSubData
}

func cursorGetInner(t *Txn, mc, mx *rcursor, dcmp format.Comparator, hasDcmp bool,
	key, data []byte, op format.CursorOp) ([]byte, []byte, error) {
	switch op {
	case format.GetCurrent:
		return cursorGetCurrent(t, mc, mx)
	case format.GetBoth, format.GetBothRange:
		if data == nil {
			return nil, nil, syscall.EINVAL
		}
		if mx == nil {
			return nil, nil, format.Incompatible
		}
		if key == nil {
			return nil, nil, syscall.EINVAL
		}
		k, d, _, err := cursorSet(t, mc, mx, dcmp, hasDcmp, key, data, true, op, true)
		return k, d, err
	case format.Set, format.SetKey, format.SetRange:
		if key == nil {
			return nil, nil, syscall.EINVAL
		}
		k, d, _, err := cursorSet(t, mc, mx, dcmp, hasDcmp, key, data, data != nil, op, op != format.SetRange)
		return k, d, err
	case format.GetMultiple:
		if !mc.isInit() {
			return nil, nil, syscall.EINVAL
		}
		if err := requireDupFixed(mc, mx); err != nil {
			return nil, nil, err
		}
		var k []byte
		if n, err := mc.currentNode(); err == nil {
			k, _ = n.Key()
		}
		// The sub-cursor stays initialised when the cursor moves from a
		// key with duplicates onto one without (KNOWN_C_BUGS.md #27): only
		// fetch for a key with some. C tests C_EOF (#28): fetch on an item,
		// and past the end of the page only without C_EOF.
		dups := currentHasDups(mc)
		fetch := false
		if mx.snum > 0 {
			nkeys := mx.topPage().NumKeys()
			fetch = mx.kiTop() < nkeys || (nkeys > 0 && mx.flags&cEOF == 0)
		}
		if dups && mx.isInit() && fetch {
			d, err := mx.fetchMultiple()
			if err != nil {
				return nil, nil, err
			}
			return k, d, nil
		}
		// Nothing to fetch: C returns success and leaves data untouched;
		// here the data is empty (KNOWN_C_BUGS.md #8).
		return k, nil, nil
	case format.NextMultiple:
		if err := requireDupFixed(mc, mx); err != nil {
			return nil, nil, err
		}
		k, _, err := cursorNext(t, mc, mx, format.NextDup)
		if err != nil {
			return nil, nil, err
		}
		if !mx.isInit() {
			return nil, nil, format.NotFound
		}
		d, err := mx.fetchMultiple()
		if err != nil {
			return nil, nil, err
		}
		return k, d, nil
	case format.PrevMultiple:
		if err := requireDupFixed(mc, mx); err != nil {
			return nil, nil, err
		}
		if !mc.isInit() {
			if _, _, err := cursorLast(t, mc, mx); err != nil {
				return nil, nil, err
			}
		}
		// C does not read the key here: a cursor left past the end by a
		// failed MDB_SET has an uninitialised sub-cursor and C returns
		// MDB_NOTFOUND. Only read the key on a node with duplicates.
		if !currentHasDups(mc) || !mx.isInit() {
			return nil, nil, format.NotFound
		}
		n, err := mc.currentNode()
		if err != nil {
			return nil, nil, err
		}
		k, ok := n.Key()
		if !ok {
			return nil, nil, format.Corrupted
		}
		if err := mx.sibling(t, false); err != nil {
			return nil, nil, err
		}
		d, err := mx.fetchMultiple()
		if err != nil {
			return nil, nil, err
		}
		return k, d, nil
	case format.Next, format.NextDup, format.NextNoDup:
		return cursorNext(t, mc, mx, op)
	case format.Prev, format.PrevDup, format.PrevNoDup:
		return cursorPrev(t, mc, mx, op)
	case format.First:
		return cursorFirst(t, mc, mx)
	case format.Last:
		return cursorLast(t, mc, mx)
	case format.FirstDup, format.LastDup:
		return moveInDups(t, mc, mx, op == format.FirstDup)
	}
	return nil, nil, syscall.EINVAL
}

// moveInDups is MDB_FIRST_DUP / MDB_LAST_DUP (mmove in mdb_cursor_get).
func moveInDups(t *Txn, mc, mx *rcursor, first bool) ([]byte, []byte, error) {
	if !mc.isInit() {
		return nil, nil, syscall.EINVAL
	}
	if mx == nil {
		return nil, nil, format.Incompatible
	}
	nkeys := mc.topPage().NumKeys()
	if mc.kiTop() >= nkeys {
		mc.setKiTop(nkeys)
		return nil, nil, format.NotFound
	}
	mc.flags &^= cEOF
	leaf, err := mc.currentNode()
	if err != nil {
		return nil, nil, err
	}
	k, ok := leaf.Key()
	if !ok {
		return nil, nil, format.Corrupted
	}
	if !leaf.IsDupData() {
		d, err := nodeRead(t, leaf)
		return k, d, err
	}
	if mx.flags&cInitialized == 0 {
		return nil, nil, syscall.EINVAL
	}
	var d []byte
	if first {
		d, _, err = cursorFirst(t, mx, nil)
	} else {
		d, _, err = cursorLast(t, mx, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	return k, d, nil
}

// cursorCount is mdb_cursor_count on read cursors.
func cursorCount(mc, mx *rcursor) (uint64, error) {
	if mx == nil {
		return 0, format.Incompatible
	}
	if !mc.isInit() {
		return 0, syscall.EINVAL
	}
	if mc.snum == 0 {
		return 0, format.NotFound
	}
	if mc.flags&cEOF != 0 {
		if mc.kiTop() >= mc.topPage().NumKeys() {
			return 0, format.NotFound
		}
		mc.flags ^= cEOF
	}
	// C only checks the index under C_EOF and otherwise reads NODEPTR(mp,
	// ki) unchecked (KNOWN_C_BUGS.md #5).
	if mc.kiTop() >= mc.topPage().NumKeys() {
		return 0, format.NotFound
	}
	leaf, err := mc.currentNode()
	if err != nil {
		return 0, err
	}
	if !leaf.IsDupData() {
		return 1, nil
	}
	if !mx.isInit() {
		return 0, syscall.EINVAL
	}
	return mx.db.Entries, nil
}
