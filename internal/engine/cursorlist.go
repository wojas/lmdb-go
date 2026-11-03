// CursorList: the cursors of one transaction handle that hold pages of its
// snapshot, so that they can drop them (and be marked detached) when the
// transaction ends or is reset. C does not track read-only cursors
// (mdb_cursor_open links only a write transaction's into mt_cursors);
// rlmdb's C API does (rlmdb-capi/src/handles.rs, "Cursor tracking"): a
// read-only transaction that is reset, committed or aborted first drops its
// cursors, which stay alive but detached until mdb_cursor_close or
// mdb_cursor_renew. Package lmdb keeps one CursorList per Txn for the same
// reason. A cursor holds slices into the transaction's map; once the
// transaction ends (or is renewed onto a new map) the old map may be
// unmapped, and a cursor that its user keeps (to renew it later, or by
// mistake) must not keep Go pointers into the address range (see
// rcursor.forget).

package engine

// CursorList is an intrusive, doubly linked list of cursors. Its methods
// are not safe for concurrent use: the transaction the list belongs to is
// used by one goroutine at a time.
type CursorList struct {
	head *Cursor
}

// Add puts c at the head of l. c must not be in a list.
func (l *CursorList) Add(c *Cursor) {
	c.prev = nil
	c.next = l.head
	if l.head != nil {
		l.head.prev = c
	}
	l.head = c
}

// Has reports whether c is in l.
func (l *CursorList) Has(c *Cursor) bool {
	return c.prev != nil || l.head == c
}

// Remove takes c out of l. c must be in l or in no list (a cursor that
// DetachAll detached, or that was never added); then nothing happens.
func (l *CursorList) Remove(c *Cursor) {
	switch {
	case c.prev != nil:
		c.prev.next = c.next
	case l.head == c:
		l.head = c.next
	default:
		return
	}
	if c.next != nil {
		c.next.prev = c.prev
	}
	c.next, c.prev = nil, nil
}

// DetachAll detaches every cursor in l and empties l: each drops its
// pages (Cursor.Forget) and has OwnerDetached set in its Owner until it is
// opened again (OpenCursorInto). Each cursor is unlinked before the walk
// moves on, so the walk ends even on a list that misuse from several
// goroutines left inconsistent.
func (l *CursorList) DetachAll() {
	c := l.head
	l.head = nil
	for c != nil {
		next := c.next
		c.next, c.prev = nil, nil
		c.detach()
		c = next
	}
}
