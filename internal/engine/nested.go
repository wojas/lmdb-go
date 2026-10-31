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
	"slices"

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
	c := &Txn{
		env:      t.env,
		pageSize: t.pageSize,
		l:        t.l,
		dmap:     t.dmap,
		txnid:    t.txnid,
		nextPgno: t.nextPgno,
		gen:      t.gen,
		slot:     -1,
	}
	// Copy parent's mt_dbflags, but clear DB_NEW.
	c.dbs = make([]txnDB, len(t.dbs))
	for i := 0; i < t.ndbs; i++ {
		c.dbs[i] = *t.dbe(i)
		c.dbs[i].state &^= dbNew
	}
	c.ndbs = t.ndbs
	cd := newDirtyPages(t.pageSize)
	// The child allocates from the parent's spare buffers.
	cd.pool.merge(&pw.dirty.pool)
	parents := append([]layer{{dirty: pw.dirty, spilled: pw.spilled}}, pw.parents...)
	// mdb_cursor_shadow: the child tracks the parent's cursors.
	backup := a.backup()
	a.depth++
	var pghead format.IDL
	if pw.pghead != nil {
		// The child works on a copy of the reclaimed pages; the parent's
		// stay as they are in case the child aborts.
		pghead = slices.Clone(pw.pghead)
		if pghead == nil {
			pghead = format.IDL{}
		}
	}
	c.w = &wstate{
		flags:        pw.flags,
		dirtyRoom:    pw.dirtyRoom,
		pghead:       pghead,
		pglast:       pw.pglast,
		pgOldest:     pw.pgOldest,
		pbufPad:      pw.pbufPad,
		cursors:      a,
		baseMapsize:  pw.baseMapsize,
		dirty:        cd,
		spilled:      spillSet{},
		parents:      parents,
		parent:       t,
		cursorBackup: backup,
	}
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
		// The child's reclaimed-page state replaces the parent's.
		pw.pghead = w.pghead
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
		// #20).
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
	w.dirty = newDirtyPages(0)
	w.spilled = spillSet{}
	w.freePgs = nil
	w.loose = nil
	w.pghead = nil
	w.parents = nil
	w.flags = txnFinished
}
