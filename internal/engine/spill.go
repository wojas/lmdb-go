// Spilling dirty pages to disk before commit: mdb_page_spill (mdb.c line
// 2080), mdb_pages_xkeep (line 1987) and mdb_page_unspill (line 2435)
// (rlmdb core/spill.rs).
//
// A write transaction keeps at most MDB_IDL_UM_MAX dirty pages. Before each
// put or delete, the operation's page need is estimated; if the room left
// is not larger, about an eighth of the dirty list is written to the data
// file now, at the pages' final positions, and remembered in the spill set.
// A spilled page is read back from the map; if the transaction writes it
// again it is copied into the dirty list under the same page number
// (mdb_page_unspill). Which pages are spilled follows C exactly (it
// decides what a later free goes to): walking the dirty list from its end,
// skip loose pages, pages under a cursor or the root of a dirty database
// (C's P_KEEP), and pages an ancestor already spilled.

package engine

import (
	"slices"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// pageSpill is mdb_page_spill: make room in the dirty list for an
// operation of cursor m0 estimated to dirty up to need pages, by writing
// at least an eighth of the dirty list's capacity to disk.
func (t *Txn) pageSpill(m0 cref, need int) error {
	err := t.pageSpill0(m0, need)
	if err == nil {
		t.w.flags |= txnSpills
	} else {
		t.w.flags |= txnError
	}
	return err
}

func (t *Txn) pageSpill0(m0 cref, need int) error {
	// Preserve pages which may soon be dirtied again.
	keep := t.pagesToKeep(m0)
	// Less aggressive spill: C spills only 1/8th of the dirty pages.
	need = max(need, int(t.env.dirtyLimit.Load())/8)
	d := t.w.dirty
	var chosen []uint64
	// Flush from the tail forward.
	for i := len(d.index) - 1; i >= 0 && need > 0; i-- {
		pgno := d.index[i].pgno
		var flags uint16
		if p := d.get(pgno); p != nil {
			flags = pFlags(t.l, p)
		}
		if flags&(format.PLoose|format.PKeep) != 0 {
			continue
		}
		if _, found := slices.BinarySearch(keep, pgno); found {
			continue
		}
		// Can't spill twice, make sure it's not already in a parent's
		// spill list.
		inParent := false
		for j := range t.w.parents {
			if t.w.parents[j].spilled.contains(pgno) {
				inParent = true
				break
			}
		}
		if inParent {
			continue
		}
		chosen = append(chosen, pgno)
		need--
	}
	slices.Reverse(chosen)
	// Flush the spilled part of the dirty list.
	if err := t.writePages(chosen); err != nil {
		return err
	}
	for _, p := range chosen {
		t.w.spilled[p] = struct{}{}
	}
	return nil
}

// pagesToKeep is the pages mdb_pages_xkeep(m0, P_DIRTY, 1) marks P_KEEP,
// sorted: this transaction's dirty pages (not loose, not sub-pages) on the
// stack of an initialised cursor (m0 and every tracked cursor) or of its
// sub-cursor when that is in a sub-DB, and the root pages of the databases
// written in this transaction.
func (t *Txn) pagesToKeep(m0 cref) []uint64 {
	const mask = format.PSubp | format.PDirty | format.PLoose | format.PKeep
	var keep []uint64
	d := t.w.dirty
	mark := func(pgno uint64) {
		if p := d.get(pgno); p != nil && pFlags(t.l, p)&mask == format.PDirty {
			keep = append(keep, pgno)
		}
	}
	a := t.w.cursors
	visit := func(i int) {
		s := a.slot(i)
		if s == nil {
			return
		}
		m := &s.main
		if !m.isInit() {
			return
		}
		for _, pgno := range m.pg[:m.snum] {
			if pgno != subpageNo {
				mark(pgno)
			}
		}
		// Proceed to the sub-cursor if it is at a sub-database.
		if !s.hasX || !s.x.c.isInit() {
			return
		}
		top := max(m.snum-1, 0)
		if m.snum == 0 || m.pg[top] == subpageNo {
			return
		}
		leaf, err := t.page(m.pg[top])
		if err != nil || !leaf.IsLeaf() {
			return
		}
		if n, ok := leaf.Node(int(m.ki[top])); ok && n.IsSubData() {
			for _, pgno := range s.x.c.pg[:s.x.c.snum] {
				if pgno != subpageNo {
					mark(pgno)
				}
			}
		}
	}
	visit(m0.slot)
	for i := range a.slots {
		if s := a.slot(i); s != nil && s.tracked {
			visit(i)
		}
	}
	// Mark dirty root pages (this transaction's own dirty pages only).
	for i := 0; i < t.ndbs; i++ {
		if e := t.dbe(i); e.state&dbDirty != 0 && e.db.Root != format.PInvalid {
			mark(e.db.Root)
		}
	}
	slices.Sort(keep)
	return slices.Compact(keep)
}

// pageUnspill is mdb_page_unspill: if pgno (not dirty) was spilled by
// this transaction or an ancestor, copy it back into this transaction's
// dirty list under the same number and return true. It stays in an
// ancestor's spill list until this transaction commits.
func (t *Txn) pageUnspill(pgno uint64) (bool, error) {
	b, level := t.locate(pgno)
	if b != nil || level == 0 {
		return false, nil
	}
	if t.w.dirtyRoom == 0 {
		return false, format.TxnFull
	}
	ps := t.pageSize
	first, err := t.page(pgno)
	if err != nil {
		return false, err
	}
	num := 1
	if first.Flags()&format.POverflow != 0 {
		num = max(int(first.OverflowPages()), 1)
	}
	if pgno > uint64(1<<62)/uint64(ps) {
		return false, format.PageNotFound
	}
	src, ok := t.dmap.Slice(int(pgno)*ps, num*ps)
	if !ok {
		return false, format.PageNotFound
	}
	buf := t.w.dirty.allocCopy(src)
	setFlags(t.l, buf, pFlags(t.l, buf)|format.PDirty)
	if level == 1 {
		// If in current txn, this page is no longer spilled.
		delete(t.w.spilled, pgno)
	}
	// Otherwise, if belonging to a parent txn, the page remains spilled
	// until the child commits.
	if _, ok := t.w.dirty.insert(pgno, buf); !ok {
		return false, format.Corrupted
	}
	t.w.dirtyRoom--
	return true, nil
}
