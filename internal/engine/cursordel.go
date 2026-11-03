// Deleting items and keeping the tree balanced: mdb_cursor_del (mdb.c line
// 7257), mdb_cursor_del0 (line 8631), mdb_rebalance (line 8447),
// mdb_page_merge (line 8281), mdb_node_move (line 8052), mdb_del0 (line
// 8759) and mdb_drop0 (line 10126) (rlmdb core/cursor_del.rs).
//
// After a delete, a page that is less than a quarter full
// (FILL_THRESHOLD, or with too few keys) borrows a node from a neighbour
// that can spare one, or is merged into it; a branch root left with a
// single child is replaced by that child. All tracked cursors are adjusted
// exactly as C adjusts them.

package engine

import (
	"bytes"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// fillThreshold is FILL_THRESHOLD: pages emptier than 25% (in tenths of a
// percent) are candidates for merging.
const fillThreshold = 250

// pageFill is PAGEFILL: how full a page is, in tenths of a percent of the
// space after the header.
func pageFill(ps int, p format.Page) int {
	space := p.L.PageSpace(ps)
	return 1000 * max(space-p.SizeLeft(), 0) / space
}

// firstKey is the key of the first item of a page, copied into *buf (a
// scratch buffer of the write transaction, grown if needed).
func firstKey(buf *[]byte, p format.Page, ksize int) ([]byte, error) {
	var k []byte
	var ok bool
	if p.IsLeaf2() {
		k, ok = p.Leaf2Key(0, ksize)
	} else {
		var n format.Node
		if n, ok = p.Node(0); ok {
			k, ok = n.Key()
		}
	}
	if !ok {
		return nil, format.Corrupted
	}
	return copyInto(buf, k), nil
}

// copyInto copies b into the scratch buffer *buf and returns the copy.
func copyInto(buf *[]byte, b []byte) []byte {
	*buf = append((*buf)[:0], b...)
	return *buf
}

// delAt is _mdb_cursor_del: delete the item at mc; with MDB_NODUPDATA all
// duplicates of its key.
func (t *Txn) delAt(mc cref, flags uint32) (err error) {
	defer func() {
		if r := recover(); r != nil {
			t.w.flags |= txnError
			panic(r)
		}
	}()
	err = t.delAt0(mc, flags)
	if err != nil {
		t.failUnlessClean(err)
	}
	return err
}

func (t *Txn) delAt0(mc cref, flags uint32) error {
	if err := t.checkUsable(); err != nil {
		return err
	}
	if !t.cur(mc).isInit() {
		return syscall.EINVAL
	}
	p, err := t.topPage(mc)
	if err != nil {
		return err
	}
	if t.cur(mc).kiTop() >= p.NumKeys() {
		return format.NotFound
	}
	if flags&mdbNoSpill == 0 {
		if err := t.spillCheck(mc, t.dbOf(mc).Depth, false, 0, 0); err != nil {
			return err
		}
	}
	if err := t.cursorTouch(mc); err != nil {
		return err
	}
	if p, err = t.topPage(mc); err != nil {
		return err
	}
	if !p.IsLeaf() {
		return format.Corrupted
	}
	if !p.IsLeaf2() {
		stays, err := t.delNodePayload(mc, flags)
		if err != nil {
			return err
		}
		if stays {
			return nil
		}
	}
	// del_key:
	return t.cursorDel0(mc)
}

// delNodePayload is the part of mdb_cursor_del before del_key: for a node
// page: delete duplicates and overflow pages. stays reports a duplicate was
// removed and others remain.
func (t *Txn) delNodePayload(mc cref, flags uint32) (bool, error) {
	fail := func(err error) (bool, error) {
		t.w.flags |= txnError
		return false, err
	}
	ki := t.cur(mc).kiTop()
	p, err := t.topPage(mc)
	if err != nil {
		return false, err
	}
	n, ok := p.Node(ki)
	if !ok {
		return false, format.Corrupted
	}
	nflags := n.Flags()
	ovpgno, ovok := n.OverflowPgno()
	var leafDB format.DB
	leafDBok := false
	if n.IsSubData() {
		if d, ok := n.InlineData(); ok {
			leafDB, leafDBok = format.ReadDB(d)
		}
	}
	if nflags&format.FDupData != 0 {
		mx := subRef(mc.slot)
		if s := t.w.cursors.slot(mc.slot); mc.sub || s == nil || !s.hasX {
			// F_DUPDATA in a database without MDB_DUPSORT, or among
			// duplicates (C would follow a NULL sub-cursor).
			return fail(format.Corrupted)
		}
		if flags&format.NoDupData != 0 {
			// C uses the sub-cursor's record as it is, which is stale if the
			// sub-cursor was never set up for this key; read this node's
			// (KNOWN_C_BUGS.md row 14). cursorDel0 subtracts the final entry.
			if err := t.xcursorInit1(mc); err != nil {
				return false, err
			}
			cnt := t.dbOf(mx).Entries
			t.dbOf(mc).Entries -= cnt - 1
			t.cur(mx).flags &^= cInitialized
		} else {
			if nflags&format.FSubData == 0 {
				x := t.cur(mx)
				x.pg[0] = subpageNo
				x.subp = true
			}
			// The sub-cursor keeps its position; make sure its record is
			// this key's (KNOWN_C_BUGS.md row 14).
			if err := t.refreshXDB(mc); err != nil {
				return false, err
			}
			if err := t.delAt(mx, mdbNoSpill); err != nil {
				return false, err
			}
			// If sub-DB still has entries, we're done.
			if t.dbOf(mx).Entries != 0 {
				if nflags&format.FSubData != 0 {
					// Update sub-DB info.
					if err := t.storeSubDBRecord(mc); err != nil {
						return false, err
					}
				} else {
					// Shrink fake page.
					pgno := t.cur(mc).pgTop()
					page, err := t.topPageMut(mc)
					if err != nil {
						return false, err
					}
					if !shrinkNode(t.l, page, ki, pgno) {
						return false, format.Corrupted
					}
				}
				t.dbOf(mc).Entries--
				t.shareXDB(mc)
				return true, nil
			}
			t.cur(mx).flags &^= cInitialized
			// Otherwise fall thru and delete the sub-DB.
		}
		if nflags&format.FSubData != 0 {
			// Add all the child DB's pages to the free list.
			if err := t.drop0(mx, false); err != nil {
				return fail(err)
			}
			if !leafDBok {
				return false, format.Corrupted
			}
			db := t.dbOf(mc)
			db.BranchPages -= leafDB.BranchPages
			db.LeafPages -= leafDB.LeafPages
		}
	} else if (uint32(nflags)^flags)&uint32(format.FSubData) != 0 {
		// LMDB passes F_SUBDATA in 'flags' to delete a DB record.
		return fail(format.Incompatible)
	}
	// Add overflow pages to free list.
	if nflags&format.FBigData != 0 {
		if !ovok {
			return false, format.Corrupted
		}
		if err := t.ovpageFree(mc, ovpgno); err != nil {
			return fail(err)
		}
	}
	return false, nil
}

// cursorDel0 is mdb_cursor_del0: remove the node at mc, adjust the other
// cursors, rebalance, and leave every cursor that was on the deleted item
// on its successor with C_DEL set.
func (t *Txn) cursorDel0(mc cref) error {
	err := t.cursorDel0Inner(mc)
	if err != nil {
		t.w.flags |= txnError
	}
	return err
}

// bothInit is m2->mc_flags & m3->mc_flags & C_INITIALIZED.
func (t *Txn) bothInit(slot int, m3 cref) bool {
	s := t.w.cursors.slot(slot)
	return s != nil && s.main.isInit() && t.cur(m3).isInit()
}

func (t *Txn) cursorDel0Inner(mc cref) error {
	w := t.cur(mc)
	top, ki, snum, dbi, sub := w.top, w.ki[w.top], w.snum, w.dbi, w.isSub()
	mp := t.pageIDOf(mc, top)
	pad := int(t.dbOf(mc).Pad)
	if err := t.nodeDel(mc, pad); err != nil {
		return err
	}
	dupsort := uint32(t.dbOf(mc).Flags)&format.DupSort != 0
	t.dbOf(mc).Entries--
	// Adjust other cursors pointing to mp.
	for s := range t.w.cursors.slots {
		m3, ok := t.w.cursors.peer(s, dbi, sub)
		if !ok || !t.bothInit(s, m3) || m3 == mc || t.cur(m3).snum < snum {
			continue
		}
		if t.pageIDOf(m3, top) != mp {
			continue
		}
		w3 := t.cur(m3)
		if w3.ki[top] == ki {
			w3.flags |= cDel
			if dupsort {
				// Sub-cursor referred into dataset which is gone.
				if sl := t.w.cursors.slot(s); sl != nil && sl.hasX {
					sl.x.c.flags &^= cInitialized | cEOF
				}
			}
			continue
		} else if w3.ki[top] > ki {
			w3.ki[top]--
		}
	}
	if err := t.rebalance(mc); err != nil {
		return err
	}
	// DB is totally empty now, just bail out. Other cursors adjustments
	// were already done by rebalance and aren't needed here.
	if t.cur(mc).snum == 0 {
		t.cur(mc).flags |= cEOF
		return nil
	}
	top = t.cur(mc).top
	mp = t.pageIDOf(mc, top)
	tp, err := t.topPage(mc)
	if err != nil {
		return err
	}
	nkeys := tp.NumKeys()
	mcKi := t.cur(mc).ki[top]
	snum = t.cur(mc).snum
	// Adjust other cursors pointing to mp.
	for s := range t.w.cursors.slots {
		m3, ok := t.w.cursors.peer(s, dbi, sub)
		if !ok || !t.bothInit(s, m3) || t.cur(m3).snum < snum {
			continue
		}
		if t.pageIDOf(m3, top) != mp || t.cur(m3).ki[top] < mcKi {
			continue
		}
		// Did the delete put m3 on this node (it was on the deleted one, or
		// past the end of the page)?
		moved := m3 == mc || t.cur(m3).flags&cDel != 0
		// If m3 points past last node in page, find next sibling.
		if int(t.cur(m3).ki[top]) >= nkeys {
			err := t.readOp(m3, func(c, _ *rcursor) error { return c.sibling(t, true) })
			switch {
			case err == nil:
				moved = true
			case err == format.NotFound:
				t.cur(m3).flags |= cEOF
				continue
			default:
				return err
			}
		}
		// (C also skips cursors with C_EOF here; m3 is on an item now, and
		// C_EOF may be left from before: KNOWN_C_BUGS.md row 28.)
		if sl := t.w.cursors.slot(s); !m3.sub && sl != nil && sl.hasX {
			p, err := t.topPage(m3)
			if err != nil {
				return err
			}
			n, ok := p.Node(t.cur(m3).kiTop())
			if !ok {
				return format.Corrupted
			}
			// If this node has dupdata, it may need to be reinited because
			// its data has moved. If the xcursor was not initd it must be
			// reinited (a cursor the delete moved here starts at the first
			// duplicate: KNOWN_C_BUGS.md row 18).
			if n.Flags()&format.FDupData != 0 {
				mx := subRef(s)
				if moved || !t.cur(mx).isInit() {
					if err := t.xcursorInit1(m3); err != nil {
						return err
					}
					if err := t.readOp(mx, func(c, _ *rcursor) error {
						_, _, err := cursorFirst(t, c, nil)
						return err
					}); err != nil {
						return err
					}
				}
			}
			// Its next MDB_NEXT must not skip the node's first duplicate
			// (KNOWN_C_BUGS.md row 29).
			if moved {
				t.cur(subRef(s)).flags |= cDel
			}
		}
	}
	t.cur(mc).flags |= cDel
	return nil
}

// rebalance is mdb_rebalance: after a delete at mc, keep its page at least
// a quarter full (branch pages: at least two keys).
func (t *Txn) rebalance(mc cref) error {
	p, err := t.topPage(mc)
	if err != nil {
		return err
	}
	minkeys, thresh := 1, fillThreshold
	if p.IsBranch() {
		minkeys, thresh = 2, 1
	}
	if pageFill(t.pageSize, p) >= thresh && p.NumKeys() >= minkeys {
		// No need to rebalance, above fill threshold.
		return nil
	}
	if t.cur(mc).snum < 2 {
		return t.rebalanceRoot(mc)
	}
	// The parent (branch page) must have at least 2 pointers, otherwise the
	// tree is invalid.
	ptop := t.cur(mc).top - 1
	pp, err := t.pageOfLevel(mc, ptop)
	if err != nil {
		return err
	}
	if pp.NumKeys() < 2 {
		return format.Corrupted
	}
	// Leaf page fill factor is below the threshold. Try to move keys from
	// left or right neighbor, or merge with a neighbor page.
	mn := t.copyCursor(mc, false)
	err = t.rebalanceWith(mc, mn, ptop, thresh, minkeys)
	t.closeSlot(mn.slot)
	return err
}

func (t *Txn) rebalanceWith(mc, mn cref, ptop, thresh, minkeys int) error {
	top := t.cur(mc).top
	oldki := t.cur(mc).ki[top]
	mcKiPtop := int(t.cur(mc).ki[ptop])
	fromleft := mcKiPtop != 0
	pp, err := t.pageOfLevel(mc, ptop)
	if err != nil {
		return err
	}
	if !fromleft {
		// We're the leftmost leaf in our parent: read right neighbor.
		nk := mcKiPtop + 1
		n, ok := pp.Node(nk)
		if !ok {
			return format.Corrupted
		}
		child := n.ChildPgno()
		if _, err := t.page(child); err != nil {
			return err
		}
		w := t.cur(mn)
		w.ki[ptop] = uint16(nk)
		w.pg[top] = child
		w.ki[top] = 0
		tp, err := t.topPage(mc)
		if err != nil {
			return err
		}
		t.cur(mc).ki[top] = uint16(tp.NumKeys())
	} else {
		// There is at least one neighbor to the left.
		nk := mcKiPtop - 1
		n, ok := pp.Node(nk)
		if !ok {
			return format.Corrupted
		}
		child := n.ChildPgno()
		cp, err := t.page(child)
		if err != nil {
			return err
		}
		w := t.cur(mn)
		w.ki[ptop] = uint16(nk)
		w.pg[top] = child
		w.ki[top] = uint16(cp.NumKeys() - 1)
		t.cur(mc).ki[top] = 0
	}
	// If the neighbor page is above threshold and has enough keys, move
	// one key from it. Otherwise we should try to merge them.
	np, err := t.topPage(mn)
	if err != nil {
		return err
	}
	nfill, nkeys := pageFill(t.pageSize, np), np.NumKeys()
	var rc error
	if nfill >= thresh && nkeys > minkeys {
		rc = t.nodeMove(mn, mc, fromleft)
		if fromleft {
			// If we inserted on left, bump position up.
			oldki++
		}
	} else {
		if !fromleft {
			rc = t.pageMerge(mn, mc)
		} else {
			// indx_t arithmetic, as in C.
			oldki += uint16(nkeys)
			wt := t.cur(mn).top
			add := t.cur(mc).ki[wt] + 1
			t.cur(mn).ki[wt] += add
			// We want rebalance to find mn when doing fixups.
			s := t.w.cursors.slot(mn.slot)
			if s == nil {
				return format.Corrupted
			}
			s.tracked = true
			rc = t.pageMerge(mc, mn)
			if s := t.w.cursors.slot(mn.slot); s != nil {
				s.tracked = false
			}
			t.copyState(mn, mc)
		}
		t.cur(mc).flags &^= cEOF
	}
	w := t.cur(mc)
	w.ki[w.top] = oldki
	return rc
}

// freeRebalancedRoot gives up the root page mp that mdb_rebalance removed:
// 0.9 puts it on the free list, 1.0 calls mdb_page_loose.
func (t *Txn) freeRebalancedRoot(mc cref, mp uint64) error {
	if t.l.LoosensRebalancedRoot() {
		return t.pageLoose(mc, mp)
	}
	t.w.freePgs = append(t.w.freePgs, mp)
	return nil
}

// rebalanceRoot is the root branch of mdb_rebalance: an empty root empties
// the tree; a branch root with one child is replaced by the child.
func (t *Txn) rebalanceRoot(mc cref) error {
	w := t.cur(mc)
	mp, subp := w.pg[0], w.subp
	p, err := t.pageOfLevel(mc, 0)
	if err != nil {
		return err
	}
	isSubp := subp || p.Flags()&format.PSubp != 0
	nkeys, isBranch := p.NumKeys(), p.IsBranch()
	var onlyChild uint64
	n0, hasChild := p.Node(0)
	if hasChild {
		onlyChild = n0.ChildPgno()
	}
	if isSubp {
		// Can't rebalance a subpage, ignoring.
		return nil
	}
	dbi, sub := w.dbi, w.isSub()
	switch {
	case nkeys == 0:
		// Tree is completely empty.
		db := t.dbOf(mc)
		db.Root = format.PInvalid
		db.Depth = 0
		db.LeafPages = 0
		if err := t.freeRebalancedRoot(mc, mp); err != nil {
			return err
		}
		// Adjust cursors pointing to mp.
		w := t.cur(mc)
		w.snum, w.top = 0, 0
		w.flags &^= cInitialized
		for s := range t.w.cursors.slots {
			m3, ok := t.w.cursors.peer(s, dbi, sub)
			if !ok {
				continue
			}
			w3 := t.cur(m3)
			if !w3.isInit() {
				continue
			}
			if w3.pg[0] == mp && !w3.subp {
				w3.snum, w3.top = 0, 0
				w3.flags &^= cInitialized
			}
		}
	case isBranch && nkeys == 1:
		// Collapsing root page.
		if err := t.freeRebalancedRoot(mc, mp); err != nil {
			return err
		}
		if !hasChild {
			return format.Corrupted
		}
		root := onlyChild
		if _, err := t.page(root); err != nil {
			return err
		}
		db := t.dbOf(mc)
		db.Root = root
		db.Depth--
		db.BranchPages--
		depth := int(db.Depth)
		w := t.cur(mc)
		w.pg[0] = root
		w.ki[0] = w.ki[1]
		for i := 1; i < min(depth, format.CursorStack-1); i++ {
			w.pg[i] = w.pg[i+1]
			w.ki[i] = w.ki[i+1]
		}
		// Adjust other cursors pointing to mp.
		for s := range t.w.cursors.slots {
			m3, ok := t.w.cursors.peer(s, dbi, sub)
			if !ok || m3 == mc {
				continue
			}
			w3 := t.cur(m3)
			if !w3.isInit() || w3.subp {
				continue
			}
			if w3.pg[0] == mp {
				for i := 0; i < min(depth, format.CursorStack-1); i++ {
					w3.pg[i] = w3.pg[i+1]
					w3.ki[i] = w3.ki[i+1]
				}
				w3.snum--
				w3.top--
			}
		}
	}
	// else: root page doesn't need rebalancing.
	return nil
}

// pageSearchLowest is mdb_page_search_lowest: from the branch page on top
// of mc, descend to the leftmost leaf below it.
func (t *Txn) pageSearchLowest(mc cref) error {
	return t.readOp(mc, func(c, _ *rcursor) error {
		node, ok := c.topPage().Node(0)
		if !ok {
			return format.Corrupted
		}
		child, err := t.page(node.ChildPgno())
		if err != nil {
			return err
		}
		c.setKiTop(0)
		if err := c.push(child.B); err != nil {
			return err
		}
		return c.pageSearchRoot(t, nil, psFirst)
	})
}

// lowestKeyBelow is the lowest key below mc's current branch page, found
// with a temporary copy of the cursor (returned; the caller closes it).
func (t *Txn) lowestKeyBelow(mc cref) ([]byte, cref, error) {
	mn := t.copyCursor(mc, false)
	if err := t.pageSearchLowest(mn); err != nil {
		t.closeSlot(mn.slot)
		return nil, mn, err
	}
	p, err := t.topPage(mn)
	if err != nil {
		t.closeSlot(mn.slot)
		return nil, mn, err
	}
	k, err := firstKey(&t.w.lowKey, p, int(t.dbOf(mn).Pad))
	if err != nil {
		t.closeSlot(mn.slot)
		return nil, mn, err
	}
	return k, mn, nil
}

// pageMerge is mdb_page_merge: move all nodes of csrc's page to cdst's
// page, unlink and free the source page, then rebalance the parent.
func (t *Txn) pageMerge(csrc, cdst cref) error {
	mark := t.w.scratch.mark()
	err := t.pageMerge0(csrc, cdst)
	t.w.scratch.release(mark)
	return err
}

func (t *Txn) pageMerge0(csrc, cdst cref) error {
	if t.cur(csrc).snum < 2 || t.cur(cdst).snum < 2 {
		return format.Corrupted // can't merge root page
	}
	// Mark dst as dirty.
	if err := t.pageTouch(cdst); err != nil {
		return err
	}
	psrc := t.cur(csrc).pgTop()
	sp, err := t.topPage(csrc)
	if err != nil {
		return err
	}
	// A copy, as rlmdb takes (C reads the source page itself).
	src := t.w.scratch.take(len(sp.B))
	copy(src, sp.B)
	srcp, ok := format.NewPage(src, t.l)
	if !ok {
		return format.Corrupted
	}
	pdst := t.cur(cdst).pgTop()
	dp, err := t.topPage(cdst)
	if err != nil {
		return err
	}
	nkeys := dp.NumKeys()
	j := nkeys
	// Move all nodes from src to dst.
	if srcp.Flags()&format.PLeaf2 != 0 {
		ksize := int(t.dbOf(csrc).Pad)
		for i := 0; i < srcp.NumKeys(); i++ {
			key, ok := srcp.Leaf2Key(i, ksize)
			if !ok {
				return format.Corrupted
			}
			if err := t.nodeAdd(cdst, j, key, nil, 0, 0); err != nil {
				return err
			}
			j++
		}
	} else {
		for i := 0; i < srcp.NumKeys(); i++ {
			srcnode, ok := srcp.Node(i)
			if !ok {
				return format.Corrupted
			}
			var key []byte
			if i == 0 && srcp.IsBranch() {
				// Must find the lowest key below src.
				k, mn, err := t.lowestKeyBelow(csrc)
				if err != nil {
					return err
				}
				t.closeSlot(mn.slot)
				key = k
			} else if key, ok = srcnode.Key(); !ok {
				return format.Corrupted
			}
			var data *nodeVal
			if srcp.IsLeaf() {
				body, err := nodeBodyOf(srcnode)
				if err != nil {
					return err
				}
				data = &nodeVal{size: srcnode.DataSize(), bytes: body}
			}
			if err := t.nodeAdd(cdst, j, key, data, srcnode.ChildPgno(), uint32(srcnode.Flags())); err != nil {
				return err
			}
			j++
		}
	}
	// Unlink the src page from parent and add to free list.
	t.cur(csrc).top--
	err = t.nodeDel(csrc, 0)
	if err == nil && t.cur(csrc).kiTop() == 0 {
		err = t.updateKey(csrc, nil)
	}
	t.cur(csrc).top++
	if err != nil {
		return err
	}
	// If not operating on FreeDB, allow this page to be reused in this
	// txn. Otherwise just add to free list.
	srcIsLeaf := srcp.IsLeaf()
	if err := t.pageLoose(csrc, psrc); err != nil {
		return err
	}
	if db := t.dbOf(csrc); srcIsLeaf {
		db.LeafPages--
	} else {
		db.BranchPages--
	}
	// Adjust other cursors pointing to mp.
	ws := t.cur(csrc)
	dbi, sub, top := ws.dbi, ws.isSub(), ws.top
	csrcSnum, csrcPgUp, csrcKiUp := ws.snum, ws.pg[top-1], ws.ki[top-1]
	cdstKiUp := t.cur(cdst).ki[top-1]
	for s := range t.w.cursors.slots {
		m3, ok := t.w.cursors.peer(s, dbi, sub)
		if !ok || m3 == csrc {
			continue
		}
		w3 := t.cur(m3)
		if w3.snum < csrcSnum || (w3.subp && top == 0) {
			continue
		}
		if w3.pg[top] == psrc {
			w3.pg[top] = pdst
			w3.ki[top] += uint16(nkeys)
			w3.ki[top-1] = cdstKiUp
		} else if w3.pg[top-1] == csrcPgUp && w3.ki[top-1] > csrcKiUp {
			w3.ki[top-1]--
		}
	}
	snum := t.cur(cdst).snum
	depth := t.dbOf(cdst).Depth
	t.cur(cdst).pop()
	rc := t.rebalance(cdst)
	// Did the tree height change?
	newDepth := t.dbOf(cdst).Depth
	snum += int(int16(newDepth) - int16(depth))
	w := t.cur(cdst)
	w.snum = snum
	w.top = max(snum-1, 0)
	return rc
}

// nodeMove is mdb_node_move: move the node at csrc to cdst's position in
// the neighbouring page, updating the separator keys in the parents.
func (t *Txn) nodeMove(csrc, cdst cref, fromleft bool) error {
	// Mark src and dst as dirty.
	if err := t.pageTouch(csrc); err != nil {
		return err
	}
	if err := t.pageTouch(cdst); err != nil {
		return err
	}
	ksizePad := int(t.dbOf(csrc).Pad)
	var key []byte
	var data *nodeVal
	var srcpg uint64
	var flags uint32
	sp, err := t.topPage(csrc)
	if err != nil {
		return err
	}
	srcLeaf2 := sp.IsLeaf2()
	if srcLeaf2 {
		k, ok := sp.Leaf2Key(t.cur(csrc).kiTop(), ksizePad)
		if !ok {
			return format.Corrupted
		}
		key = copyInto(&t.w.moveKey, k)
	} else {
		ki := t.cur(csrc).kiTop()
		srcIsBranch := sp.IsBranch()
		n, ok := sp.Node(ki)
		if !ok {
			return format.Corrupted
		}
		nk, ok := n.Key()
		if !ok {
			return format.Corrupted
		}
		nkey := nk
		if sp.IsLeaf() {
			body, err := nodeBodyOf(n)
			if err != nil {
				return err
			}
			data = &nodeVal{size: n.DataSize(), bytes: copyInto(&t.w.moveData, body)}
		} else {
			data = &nodeVal{bytes: []byte{}}
		}
		srcpg = n.ChildPgno()
		flags = uint32(n.Flags())
		if ki == 0 && srcIsBranch {
			// Must find the lowest key below src.
			w := t.cur(csrc)
			snum, top := w.snum, w.top
			if err := t.pageSearchLowest(csrc); err != nil {
				return err
			}
			lp, err := t.topPage(csrc)
			if err != nil {
				return err
			}
			k, err := firstKey(&t.w.moveKey, lp, ksizePad)
			if err != nil {
				return err
			}
			w = t.cur(csrc)
			w.snum, w.top = snum, top
			key = k
		} else {
			key = copyInto(&t.w.moveKey, nkey)
		}
	}
	dp, err := t.topPage(cdst)
	if err != nil {
		return err
	}
	if dp.IsBranch() && t.cur(cdst).kiTop() == 0 {
		// Must find the lowest key below dst.
		snum := t.cur(cdst).snum
		bkey, mn, err := t.lowestKeyBelow(cdst)
		if err != nil {
			return err
		}
		w := t.cur(mn)
		w.snum = snum
		w.top = snum - 1
		w.ki[snum-1] = 0
		err = t.updateKey(mn, bkey)
		t.closeSlot(mn.slot)
		if err != nil {
			return err
		}
	}
	// Add the node to the destination page.
	if err := t.nodeAdd(cdst, t.cur(cdst).kiTop(), key, data, srcpg, flags); err != nil {
		return err
	}
	// Delete the node from the source page.
	if err := t.nodeDel(csrc, len(key)); err != nil {
		return err
	}
	t.nodeMoveFixups(csrc, cdst, fromleft)
	// Update the parent separators.
	if t.cur(csrc).kiTop() == 0 {
		top := t.cur(csrc).top
		if t.cur(csrc).ki[top-1] != 0 {
			p, err := t.topPage(csrc)
			if err != nil {
				return err
			}
			k, err := firstKey(&t.w.sepKey, p, ksizePad)
			if err != nil {
				return err
			}
			if err := t.updateSeparator(csrc, k); err != nil {
				return err
			}
		}
		p, err := t.topPage(csrc)
		if err != nil {
			return err
		}
		if p.IsBranch() {
			ix := t.cur(csrc).ki[top]
			t.cur(csrc).ki[top] = 0
			err := t.updateKey(csrc, nil)
			t.cur(csrc).ki[top] = ix
			if err != nil {
				return err
			}
		}
	}
	if t.cur(cdst).kiTop() == 0 {
		top := t.cur(cdst).top
		if t.cur(cdst).ki[top-1] != 0 {
			ks := 0
			if srcLeaf2 {
				ks = len(key)
			}
			p, err := t.topPage(cdst)
			if err != nil {
				return err
			}
			k, err := firstKey(&t.w.sepKey, p, ks)
			if err != nil {
				return err
			}
			if err := t.updateSeparator(cdst, k); err != nil {
				return err
			}
		}
		p, err := t.topPage(cdst)
		if err != nil {
			return err
		}
		if p.IsBranch() {
			ix := t.cur(cdst).ki[top]
			t.cur(cdst).ki[top] = 0
			err := t.updateKey(cdst, nil)
			t.cur(cdst).ki[top] = ix
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// updateSeparator updates the separator key for c's page in its parent,
// through a tracked copy one level up (WITH_CURSOR_TRACKING(mn,
// mdb_update_key(&mn, &key))).
func (t *Txn) updateSeparator(c cref, key []byte) error {
	mn := t.copyCursor(c, true)
	w := t.cur(mn)
	w.snum--
	w.top--
	err := t.updateKey(mn, key)
	t.closeSlot(mn.slot)
	return err
}

// nodeMoveFixups is the cursor fixup of mdb_node_move.
func (t *Txn) nodeMoveFixups(csrc, cdst cref, fromleft bool) {
	ws := t.cur(csrc)
	dbi, sub, top := ws.dbi, ws.isSub(), ws.top
	mps := ws.pg[top]
	csrcKi := ws.ki[top]
	wd := t.cur(cdst)
	cdstPg, cdstKi := wd.pg[wd.top], wd.ki[wd.top]
	for s := range t.w.cursors.slots {
		m3, ok := t.w.cursors.peer(s, dbi, sub)
		if !ok {
			continue
		}
		if fromleft {
			// If we're adding on the left, bump others up.
			mpd := t.cur(cdst).pg[top]
			w3 := t.cur(m3)
			if !w3.isInit() || w3.top < top || w3.subp {
				continue
			}
			if m3 != cdst && w3.pg[top] == mpd && w3.ki[top] >= cdstKi {
				w3.ki[top]++
			}
			if m3 != csrc && w3.pg[top] == mps && w3.ki[top] == csrcKi {
				w3.pg[top] = cdstPg
				w3.ki[top] = cdstKi
				w3.ki[top-1]++
			}
		} else {
			// Adding on the right, bump others down.
			if m3 == csrc {
				continue
			}
			w3 := t.cur(m3)
			if !w3.isInit() || w3.top < top || w3.subp {
				continue
			}
			if w3.pg[top] == mps {
				if w3.ki[top] == 0 {
					w3.pg[top] = cdstPg
					w3.ki[top] = cdstKi
					w3.ki[top-1]--
				} else {
					w3.ki[top]--
				}
			}
		}
	}
}

// del0 is mdb_del0: delete key (and data for DUPSORT, else all its items)
// through a temporary cursor.
func (t *Txn) del0(dbi int, key, data []byte, hasData bool, flags uint32) (err error) {
	defer func() {
		if r := recover(); r != nil {
			t.w.flags |= txnError
			panic(r)
		}
	}()
	slot := t.openSlot(dbi, false)
	err = t.del0With(mainRef(slot), key, data, hasData, flags)
	t.closeSlot(slot)
	return err
}

func (t *Txn) del0With(mc cref, key, data []byte, hasData bool, flags uint32) error {
	op := format.GetBoth
	if !hasData {
		flags |= format.NoDupData
		op = format.Set
	}
	if !hasData && t.seeksInPlace(mc) {
		// The cursor of mdb_del in a tree without duplicates: on its own
		// stack.
		db := *t.dbOf(mc)
		found, err := t.seekKey(mc, &db, t.cmpOf(mc), key)
		if err != nil {
			return err
		}
		if !found {
			return format.NotFound
		}
	} else {
		e := t.dbe(t.cur(mc).dbi)
		dcmp, hasDcmp := e.dupCmp, e.hasDup
		if err := t.readOp(mc, func(c, x *rcursor) error {
			_, _, _, err := cursorSet(t, c, x, dcmp, hasDcmp, key, data, hasData, op, true)
			return err
		}); err != nil {
			return err
		}
	}
	// Let pageSplit know about this cursor if needed: a delete triggers a
	// rebalance, which may update separators and split the parent; the
	// cursor must stay consistent until the end of the rebalance.
	s := t.w.cursors.slot(mc.slot)
	if s == nil {
		return format.Corrupted
	}
	s.tracked = true
	err := t.delAt(mc, flags)
	if s := t.w.cursors.slot(mc.slot); s != nil {
		s.tracked = false
	}
	return err
}

// mdbDrop is mdb_drop (mdb.c line 10219): free every page of dbi
// (including overflow pages and, for MDB_DUPSORT, the sub-DBs of its keys)
// and invalidate the cursors on it. With del, also delete the named DB's
// record from the main DB and close the handle (the main DB is only
// emptied); otherwise reset its record to an empty tree. The handle is
// closed for every transaction at once, as in C; a nested transaction that
// aborts reopens it (KNOWN_C_BUGS.md row 20).
func (t *Txn) mdbDrop(dbi int, del bool) error {
	if dbi == format.MainDBI && t.l.DropsNamedDBsWithMain() {
		// Named databases cannot coexist with these main-DB flags.
		if uint32(t.dbe(dbi).db.Flags)&(format.DupSort|format.IntegerKey) == 0 {
			return t.dropMainWithNamedDBs(del)
		}
	}
	return t.mdbDrop0(dbi, del)
}

// dropMainWithNamedDBs is LMDB 1.0's mdb_drop of the main DB when it may
// record named databases: refuse while a named database is open
// (MDB_DBIS_BUSY); while the first record is a named database's, delete
// that database; then empty the main DB. As in C, only the first record
// is checked.
func (t *Txn) dropMainWithNamedDBs(del bool) error {
	if t.namedDBIOpen() {
		return format.DBIsBusy
	}
	id, err := t.openWriteCursor(MainDBI)
	if err != nil {
		return err
	}
	more, err := t.dropNamedDBs(id)
	if err == nil && more {
		err = t.mdbDrop0(format.MainDBI, del)
	}
	t.closeWriteCursor(id)
	return err
}

// dropNamedDBs is the loop of dropMainWithNamedDBs: false if the main DB
// was empty to begin with, true when it is time to empty it.
func (t *Txn) dropNamedDBs(id cursorID) (bool, error) {
	c := &Cursor{txn: t, dbi: MainDBI, id: id}
	k, _, err := t.writeCursorGet(c, nil, nil, format.First)
	if err == format.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	key := append([]byte(nil), k...)
	isDB, err := t.writeCursorIsDB(id)
	if err != nil {
		return false, err
	}
	if !isDB {
		// Not a named database: just a normal drop.
		return true, nil
	}
	for {
		// mdb_dbi_open(txn, key.mv_data, 0, &i): the key as a C string.
		name := key
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		db, err := t.createDB(name, 0)
		if err == format.NotFound {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if err := t.mdbDrop(int(db), true); err != nil {
			return false, err
		}
		// C: any failure to find a first record ends the loop.
		k, _, err := t.writeCursorGet(c, nil, nil, format.First)
		if err != nil {
			return true, nil
		}
		key = append(key[:0], k...)
	}
}

// namedDBIOpen reports whether a named database is open in this
// transaction (C: a DBI >= CORE_DBS with DB_VALID): opened by it, or open
// in the environment when it began and not yet looked at.
func (t *Txn) namedDBIOpen() bool {
	r := &t.env.dbis
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := format.CoreDBs; i < t.ndbs; i++ {
		if st := t.dbe(i).state; st != 0 {
			if st&dbValid != 0 {
				return true
			}
		} else if i < len(r.slots) && r.slots[i].valid {
			return true
		}
	}
	return false
}

func (t *Txn) mdbDrop0(dbi int, del bool) error {
	slot := t.openSlot(dbi, true)
	mc := mainRef(slot)
	subs := uint32(t.dbOf(mc).Flags)&format.DupSort != 0
	err := t.drop0(mc, subs)
	// Invalidate the dropped DB's cursors.
	for i := range t.w.cursors.slots {
		if m2, ok := t.w.cursors.peer(i, dbi, false); ok {
			t.cur(m2).flags &^= cInitialized | cEOF
		}
	}
	if err == nil {
		// Can't delete the main DB.
		if del && dbi >= format.CoreDBs {
			name := t.env.dbis.nameOf(dbi)
			if name == nil {
				err = format.BadDBI
			} else if err = t.del0(format.MainDBI, name, nil, false, uint32(format.FSubData)); err != nil {
				t.w.flags |= txnError
			} else {
				t.dbe(dbi).state = dbStale
				seq := t.env.dbis.seqOf(dbi)
				r := &t.env.dbis
				r.mu.RLock()
				if dbi < len(r.slots) {
					t.w.dropped = append(t.w.dropped, droppedDBI{dbi, r.slots[dbi], seq})
				}
				r.mu.RUnlock()
				r.close(DBI(dbi))
			}
		} else {
			// Reset the DB record, mark it dirty.
			e := t.dbe(dbi)
			e.state |= dbDirty
			e.db.Depth = 0
			e.db.BranchPages = 0
			e.db.LeafPages = 0
			e.db.OverflowPages = 0
			e.db.Entries = 0
			e.db.Root = format.PInvalid
			t.w.flags |= txnDirty
		}
	}
	t.closeSlot(slot)
	return err
}

// drop0 is mdb_drop0: add all pages of the (sub-)DB under mc to the free
// list (with subs, also the sub-DBs of its F_SUBDATA nodes).
func (t *Txn) drop0(mc cref, subs bool) error {
	err := t.readOp(mc, func(c, _ *rcursor) error { return c.pageSearch(t, nil, psFirst) })
	if err != nil {
		t.cur(mc).flags &^= cInitialized
		if err == format.NotFound {
			return nil
		}
		return err
	}
	err = t.drop0Walk(mc, subs)
	if err != nil {
		t.w.flags |= txnError
	}
	t.cur(mc).flags &^= cInitialized
	return err
}

func (t *Txn) drop0Walk(mc cref, subs bool) error {
	// DUPSORT sub-DBs have no ovpages/DBs. Omit scanning leaves. This also
	// avoids any P_LEAF2 pages, which have no nodes. Also if the DB doesn't
	// have sub-DBs and has no overflow pages, omit scanning leaves.
	if t.cur(mc).isSub() || (!subs && t.dbOf(mc).OverflowPages == 0) {
		t.cur(mc).pop()
	}
	mx := *t.cur(mc)
	for t.cur(mc).snum > 0 {
		top := t.cur(mc).top
		p, err := t.topPage(mc)
		if err != nil {
			return err
		}
		isLeaf, n := p.IsLeaf(), p.NumKeys()
		i := 0
		pop := false
		if isLeaf {
			for ; i < n; i++ {
				p, err := t.topPage(mc)
				if err != nil {
					return err
				}
				node, ok := p.Node(i)
				if !ok {
					return format.Corrupted
				}
				nflags := node.Flags()
				if nflags&format.FBigData != 0 {
					ov, ok := node.OverflowRef()
					if !ok {
						return format.Corrupted
					}
					pages, err := t.droppedRunPages(ov)
					if err != nil {
						return err
					}
					t.w.freePgs.AppendRange(ov.Pgno, pages)
					db := t.dbOf(mc)
					db.OverflowPages -= uint64(pages)
					if db.OverflowPages == 0 && !subs {
						break
					}
				} else if subs && nflags&format.FSubData != 0 {
					w := t.cur(mc)
					ki := w.ki[top]
					w.ki[top] = uint16(i)
					err := t.xcursorInit1(mc)
					t.cur(mc).ki[top] = ki
					if err != nil {
						return err
					}
					if err := t.drop0(subRef(mc.slot), false); err != nil {
						return err
					}
				}
			}
			if !subs && t.dbOf(mc).OverflowPages == 0 {
				pop = true
			}
		} else {
			for i2 := 0; i2 < n; i2++ {
				node, ok := p.Node(i2)
				if !ok {
					return format.Corrupted
				}
				t.w.freePgs = append(t.w.freePgs, node.ChildPgno())
			}
			i = n
		}
		if !pop {
			if top == 0 {
				break
			}
			t.cur(mc).ki[top] = uint16(i)
			err := t.readOp(mc, func(c, _ *rcursor) error { return c.sibling(t, true) })
			switch {
			case err == nil:
				continue
			case err == format.NotFound:
				pop = true
			default:
				return err
			}
		}
		if pop {
			// No more siblings, go back to beginning of previous level.
			w := t.cur(mc)
			w.pop()
			w.ki[0] = 0
			for i := 1; i < w.snum; i++ {
				w.ki[i] = 0
				w.pg[i] = mx.pg[i]
			}
		}
	}
	// Free it.
	t.w.freePgs = append(t.w.freePgs, t.dbOf(mc).Root)
	return nil
}
