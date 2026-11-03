// Nested write transactions: mdb_txn_begin with a parent (mdb.c line
// 2912), the nested branch of mdb_txn_commit (line 3594), mdb_txn_end for
// a child (line 3064), mdb_cursor_shadow (line 2622) and
// mdb_cursors_close (line 2665) (rlmdb core/nested.rs).
//
// A child sees everything its parent did, and its own changes; when it
// commits they become the parent's, when it aborts they vanish. The parent
// cannot be used while the child runs (MDB_TXN_HAS_CHILD).
//
//   - The parent's dirty and spilled pages become the child's first
//     ancestor layer; reads walk the child's own pages, then the layers
//     (mdb_page_get), and a write to a page dirty in an ancestor first
//     copies it into the child's dirty list under the same page number.
//   - The child shares the parent's cursor arena, so its page moves keep
//     the parent's cursors up to date as C's fixups do on shadowed
//     cursors; a copy of their state (mc_backup) restores them on abort.
//   - The DB table, mt_next_pgno, mt_dirty_room, the flags and a copy of
//     the reclaimed free pages (me_pghead) are copied; the child starts
//     with an empty free list and no loose pages.
//
// A commit merges: the free list is appended to the parent's, the child's
// dirty pages replace the parent's (and leave its spill list), pages the
// child spilled leave the parent's dirty list and join its spill list,
// loose pages are appended, the DB table, next page number, dirty room,
// flags and free-page state replace the parent's (keeping the parent's
// DB_NEW marks). An abort restores the parent's cursors and closes the
// handles of databases the child created.

package engine

import (
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// How a nested transaction ends.
const (
	endCommit = iota // successful commit: merge into the parent
	endAbort         // abort: discard
	endFailed        // commit of a failed child: discard, the parent fails too
)

// beginNested is mdb_txn_begin(env, parent, 0): begin a transaction nested
// in t. Children can be nested further. MDB_BAD_TXN if t has failed or has
// a child.
func (t *Txn) beginNested() (*Txn, error) {
	if t.w == nil {
		return nil, format.BadTxn
	}
	if err := t.checkUsable(); err != nil {
		return nil, err
	}
	pw := t.w
	a := pw.cursors
	// The child's memory is the one t's last child used, as the top-level
	// write transaction reuses its own (writerState.txn): t keeps it in
	// spareChild between children.
	c := pw.spareChild
	pw.spareChild = nil
	if c == nil {
		c = &Txn{w: &wstate{dirty: newDirtyPages(t.pageSize), spilled: spillSet{}}}
	}
	cw := c.w
	c.env, c.pageSize, c.l, c.dmap = t.env, t.pageSize, t.l, t.dmap
	c.txnid, c.nextPgno, c.gen = t.txnid, t.nextPgno, t.gen
	c.slot, c.state = -1, txnActive
	c.owner++
	// Copy parent's mt_dbflags, but clear DB_NEW. Entries the previous
	// child looked at (below its ndbs) are cleared; the rest are zero.
	if cap(c.dbs) < len(t.dbs) {
		c.dbs = make([]txnDB, len(t.dbs))
	} else {
		clear(c.dbs[:min(c.ndbs, len(c.dbs))])
		c.dbs = c.dbs[:len(t.dbs)]
	}
	for i := 0; i < t.ndbs; i++ {
		c.dbs[i] = *t.dbe(i)
		c.dbs[i].state &^= dbNew
	}
	c.ndbs = t.ndbs
	// The child allocates from the parent's spare buffers.
	cw.dirty.pool.merge(&pw.dirty.pool)
	parents := append(cw.parents[:0], layer{dirty: pw.dirty, spilled: pw.spilled})
	parents = append(parents, pw.parents...)
	// mdb_cursor_shadow: the child tracks the parent's cursors.
	backup := a.backupInto(cw.cursorBackup[:0])
	a.depth++
	var pghead format.IDL
	if pw.pghead != nil {
		// The child works on a copy of the reclaimed pages; the parent's
		// stay as they are in case the child aborts.
		pghead = append(cw.emptyPghead(), pw.pghead...)
	}
	cw.flags = pw.flags
	cw.freePgs = cw.freePgs[:0]
	cw.loose = cw.loose[:0]
	cw.dirtyRoom = pw.dirtyRoom
	cw.pghead, cw.pglast = pghead, pw.pglast
	cw.pgOldest = pw.pgOldest
	cw.pbufPad = pw.pbufPad
	cw.cursors = a
	cw.baseMain, cw.hasBaseMain = format.DB{}, false
	cw.baseMapsize = pw.baseMapsize
	clear(cw.dropped)
	cw.dropped = cw.dropped[:0]
	cw.parents = parents
	cw.parent, cw.child = t, nil
	cw.cursorBackup = backup
	cw.locked = false
	pw.flags |= txnHasChild
	pw.child = c
	return c, nil
}

// endNested ends a nested transaction: merge into the parent or discard.
func (t *Txn) endNested(how int) {
	w := t.w
	p := w.parent
	if p == nil || w.flags&txnFinished != 0 {
		return
	}
	if w.child != nil {
		// End the grandchild first (C aborts it).
		w.child.endNested(endAbort)
	}
	pw := p.w
	r := &t.env.dbis
	if how != endCommit {
		// Reads now see the parent's state: drop our own pages first.
		w.dirty.clear()
		clear(w.spilled)
		t.closeVanishedHandles(p.dbe(format.MainDBI).db)
	}
	// The spare buffers go back first, so that the pages the parent drops
	// below are recycled.
	pw.dirty.pool.merge(&w.dirty.pool)
	a := w.cursors
	depth := a.depth
	// Only the parent's cursors survive the child.
	a.releaseOwned(depth)
	if how == endCommit {
		// Append our free list to parent's.
		pw.freePgs = append(pw.freePgs, w.freePgs...)
		p.nextPgno = t.nextPgno
		pw.flags = w.flags
		// Update parent's DB table, preserving parent's DB_NEW status.
		for i, n := 0, max(p.ndbs, t.ndbs); i < n; i++ {
			pe, ce := p.dbe(i), t.dbe(i)
			var keepNew uint8
			if i >= format.CoreDBs {
				keepNew = pe.state & dbNew
			}
			*pe = *ce
			pe.state |= keepNew
		}
		childDirty, childSpilled := w.dirty, w.spilled
		// Remove anything in our dirty list from parent's spill list.
		if len(pw.spilled) != 0 {
			for _, e := range childDirty.index {
				delete(pw.spilled, e.pgno)
			}
		}
		// Remove anything in our spill list from parent's dirty list: our
		// newer version is on disk.
		if len(childSpilled) != 0 {
			pw.dirty.retain(func(pgno uint64, _ []byte) bool { return !childSpilled.contains(pgno) })
		}
		// Merge our dirty list with parent's.
		pw.dirty.merge(childDirty)
		pw.dirtyRoom = w.dirtyRoom
		for pg := range childSpilled {
			pw.spilled[pg] = struct{}{}
		}
		// Append our loose page list to parent's (the child's pages come
		// after the parent's in C's order).
		pw.loose = append(w.loose, pw.loose...)
		// The child's reclaimed-page state replaces the parent's. The list
		// is the parent's now: the child must not keep it as its spare.
		pw.pghead = w.pghead
		w.pghead = nil
		pw.pglast = w.pglast
		pw.dropped = append(pw.dropped, w.dropped...)
	} else {
		// Close the handles of databases created in this child.
		for i := format.CoreDBs; i < t.ndbs; i++ {
			if e := t.dbe(i); e.state&dbNew != 0 {
				r.finishNew(i, false, e.db.Flags)
			}
		}
		// Reopen the handles the child closed with Drop (KNOWN_C_BUGS.md
		// row 20).
		for _, d := range w.dropped {
			pe := p.dbe(d.i)
			// Only a handle of the database the parent has a view of.
			same := d.slot.name != nil && nameID(d.slot.name) == pe.nameID
			if pe.state&dbValid == 0 || !same {
				continue
			}
			slot := d.slot
			slot.valid = pe.state&dbNew == 0
			if seq, ok := r.reopen(d.i, slot); ok {
				pe.seq = seq
			}
		}
		for i, n := 0, max(p.ndbs, t.ndbs); i < n; i++ {
			pe, ce := p.dbe(i), t.dbe(i)
			if pe.state == 0 {
				continue
			}
			seq := r.seqOf(i)
			name := r.nameOf(i)
			if pe.state&dbValid != 0 && pe.seq != seq && name != nil && nameID(name) == pe.nameID {
				// The child opened the parent's database again in a new
				// generation of the slot; as in C the handle stays usable.
				pe.seq = seq
			} else if ce.state != 0 && ce.seq == pe.seq {
				// Comparators set during the child are environment-wide in
				// C and survive the abort.
				pe.keyCmp, pe.dupCmp, pe.hasDup = ce.keyCmp, ce.dupCmp, ce.hasDup
			}
		}
		a.restore(w.cursorBackup)
		if how == endFailed {
			pw.flags |= txnError
		}
	}
	// me_pgoldest lives in the environment in C.
	pw.pgOldest = w.pgOldest
	pw.pbufPad = w.pbufPad
	pw.flags &^= txnHasChild
	pw.child = nil
	a.depth = depth - 1
	// The dirty list is empty now (merged into the parent's, or cleared).
	clear(w.spilled)
	w.freePgs = w.freePgs[:0]
	// loose went to the parent on commit (pghead too, see above); an
	// aborted child's own copy of pghead is kept for the next child.
	w.loose = nil
	w.dropPghead()
	clear(w.parents)
	w.parents = w.parents[:0]
	clear(w.cursorBackup)
	w.cursorBackup = w.cursorBackup[:0]
	w.parent = nil
	w.flags = txnFinished
	// The map is the parent's (the child holds no reference of its own);
	// the spare child must not keep it past the parent's end, nor pages of
	// it in its scratch cursors.
	t.forgetPages()
	t.dmap = nil
	// The parent's next child reuses this one's memory (beginNested): the
	// caller must not use t any more (package lmdb drops it, and Owner
	// tells a handle that missed the end).
	if pw.spareChild == nil {
		pw.spareChild = t
	}
}
