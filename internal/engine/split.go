// mdb_page_split (mdb.c line 8813): split a full page and insert a new
// node (rlmdb core/split.rs).
//
// The cursor mc points at the full page and the index where the new node
// belongs. A new right sibling rp is allocated (and a new root above the
// page if it was the root). Then: with MDB_APPEND the new node alone goes
// to rp; on P_LEAF2 pages the upper half of the keys moves to rp;
// otherwise the split point is chosen as C does it (the middle, adjusted
// by node sizes when nodes are large or few, and biased so sequential
// inserts leave the old page full), and the nodes are redistributed
// through a temporary copy of the page, the new node in its place. The
// first key of rp (the separator) is inserted into the parent, splitting
// it recursively if full; during that recursion a copy of the cursor is
// tracked so the nested split's fixups adjust it (WITH_CURSOR_TRACKING).
// Finally every other cursor on the split page moves to where its node
// went.

package engine

import (
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// pageSplit is mdb_page_split: insert newkey (with newdata, or child
// newpgno for a branch page) at mc's position in its full top page.
func (t *Txn) pageSplit(mc cref, newkey []byte, newdata *nodeVal, newpgno uint64, nflags uint32) error {
	mnSlot := -1
	err := t.pageSplit0(mc, newkey, newdata, newpgno, nflags, &mnSlot)
	if mnSlot >= 0 {
		t.closeSlot(mnSlot)
	}
	if err != nil {
		t.w.flags |= txnError
	}
	return err
}

func (t *Txn) pageSplit0(mc cref, newkey []byte, newdata *nodeVal, newpgno uint64, nflags uint32, mnSlot *int) error {
	ps := t.pageSize
	l := t.l
	nodemax := t.env.nodeMax
	mp := t.cur(mc).pgTop()
	newindx := t.cur(mc).kiTop()
	top0, err := t.topPage(mc)
	if err != nil {
		return err
	}
	mpFlags, mpPad, nkeys := top0.Flags(), top0.Pad(), top0.NumKeys()
	isLeaf := mpFlags&format.PLeaf != 0
	isLeaf2 := mpFlags&format.PLeaf2 != 0
	// Create a right sibling.
	rp, err := t.pageNew(mc, mpFlags, 1)
	if err != nil {
		return err
	}
	rpb, err := t.dirtyMut(rp)
	if err != nil {
		return err
	}
	setPad(l, rpb, mpPad)
	// Usually when splitting the root page, the cursor height is 1. But
	// when called from updateKey, the cursor height may be greater because
	// it walks up the stack while finding the branch slot to update.
	newRoot := -1
	var ptop int
	if t.cur(mc).top < 1 {
		pp, err := t.pageNew(mc, format.PBranch, 1)
		if err != nil {
			return err
		}
		// Shift current top to make room for new parent.
		w := t.cur(mc)
		for i := min(w.snum, format.CursorStack-1); i >= 1; i-- {
			w.pg[i] = w.pg[i-1]
			w.ki[i] = w.ki[i-1]
		}
		w.pg[0] = pp
		w.ki[0] = 0
		db := t.dbOf(mc)
		db.Root = pp
		newRoot = int(db.Depth)
		db.Depth++
		// Add left (implicit) pointer.
		if err := t.nodeAdd(mc, 0, nil, nil, mp, 0); err != nil {
			// Undo the pre-push.
			w := t.cur(mc)
			w.pg[0] = w.pg[1]
			w.ki[0] = w.ki[1]
			db := t.dbOf(mc)
			db.Root = mp
			db.Depth--
			return err
		}
		w = t.cur(mc)
		w.snum++
		w.top++
		ptop = 0
	} else {
		ptop = t.cur(mc).top - 1
	}
	mn := t.copyCursor(mc, false)
	*mnSlot = mn.slot
	{
		mcKiPtop := t.cur(mc).ki[ptop]
		w := t.cur(mn)
		w.pg[w.top] = rp
		w.ki[ptop] = mcKiPtop + 1
	}
	// The page as it was (src), the temporary page holding the left half
	// (cp), and the offsets of mp's nodes with a slot for the new one (C
	// keeps those in copy->mp_ptrs).
	var src, cp []byte
	var copyPtrs []int
	var splitIndx int
	var sepkey []byte
	if nflags&format.Append != 0 {
		w := t.cur(mn)
		w.ki[w.top] = 0
		sepkey = newkey
		splitIndx = newindx
		nkeys = 0
	} else {
		tp, err := t.topPage(mc)
		if err != nil {
			return err
		}
		src = append([]byte(nil), tp.B...)
		si := (nkeys + 1) / 2
		if isLeaf2 {
			if sepkey, err = t.splitLeaf2(mc, mp, rp, src, newkey, newindx, si, nkeys); err != nil {
				return err
			}
		} else {
			// Maximum free space in an empty page.
			pmax := l.PageSpace(ps)
			// Threshold number of keys considered "small".
			keythresh := ps >> 7
			var nsize int
			if isLeaf {
				dsize := 0
				if newdata != nil {
					dsize = newdata.size
				}
				nsize = leafSizeOf(l, nodemax, len(newkey), dsize)
			} else {
				nsize = branchSizeOf(l, len(newkey))
			}
			nsize = format.Even(nsize)
			// Grab a page to hold a temporary copy (zero-filled: node
			// padding bytes are not written but end up in mp).
			hdr := l.PageHdr
			cp = make([]byte, ps)
			copy(cp[:hdr], src[:hdr])
			setLower(l, cp, hdr)
			setUpper(l, cp, ps)
			setPad(l, cp, 0)
			// Prepare to insert.
			copyPtrs = make([]int, 0, nkeys+1)
			for i := 0; i < nkeys; i++ {
				if i == newindx {
					copyPtrs = append(copyPtrs, 0)
				}
				p, ok := ptrAt(l, src, i)
				if !ok {
					return format.Corrupted
				}
				copyPtrs = append(copyPtrs, p)
			}
			if newindx >= nkeys {
				copyPtrs = append(copyPtrs, 0)
			}
			// When items are relatively large the split point needs to be
			// checked, because being off-by-one makes the difference
			// between success or failure in nodeAdd. As a final tweak, if
			// the new item goes on the last spot on the page, bias the
			// split so the new page is emptier than the old page.
			if nkeys < keythresh || nsize > pmax/16 || newindx >= nkeys {
				// Find split point.
				psize := 0
				var i, j, k int
				if newindx <= si || newindx >= nkeys {
					k = si + 1
					if isLeaf {
						k++
					}
					if newindx >= nkeys {
						k = nkeys
					}
					i, j = 0, 1
				} else {
					i, j, k = nkeys, -1, si-1
				}
				for i != k {
					if i == newindx {
						psize += nsize
					} else {
						// A corrupt page can be "full" with too few keys
						// for the scan (C reads garbage here).
						if i < 0 || i >= len(copyPtrs) {
							return format.Corrupted
						}
						node, err := nodeAt(l, src, copyPtrs[i])
						if err != nil {
							return err
						}
						psize += format.NodeSize + l.NodeKeyLen(node.KeySize()) + 2
						if isLeaf {
							psize += l.StoredDataLen(node.DataSize(), node.Flags()&format.FBigData != 0)
						}
						psize = format.Even(psize)
					}
					if psize > pmax || i == k-j {
						si = i
						if j < 0 {
							si++
						}
						break
					}
					i += j
				}
			}
			if si == newindx {
				sepkey = newkey
			} else {
				if si < 0 || si >= len(copyPtrs) {
					return format.Corrupted
				}
				node, err := nodeAt(l, src, copyPtrs[si])
				if err != nil {
					return err
				}
				k, ok := node.Key()
				if !ok {
					return format.Corrupted
				}
				sepkey = k
			}
		}
		splitIndx = si
	}
	// Copy separator key to the parent.
	didSplit := false
	pp, err := t.pageOfLevel(mn, ptop)
	if err != nil {
		return err
	}
	if pp.SizeLeft() < branchSizeOf(l, len(sepkey)) {
		snum := t.cur(mc).snum
		w := t.cur(mn)
		w.snum--
		w.top--
		didSplit = true
		// We want other splits to find mn when doing fixups.
		s := t.w.cursors.slot(mn.slot)
		if s == nil {
			return format.Corrupted
		}
		s.tracked = true
		err := t.pageSplit(mn, sepkey, nil, rp, 0)
		if s := t.w.cursors.slot(mn.slot); s != nil {
			s.tracked = false
		}
		if err != nil {
			return err
		}
		// Root split?
		if t.cur(mc).snum > snum {
			ptop++
		}
		// Right page might now have changed parent. Check if left page
		// also changed parent.
		mnPgPtop := t.cur(mn).pg[ptop]
		mcKiPtop := int(t.cur(mc).ki[ptop])
		if mnPgPtop != t.cur(mc).pg[ptop] {
			pl, err := t.pageOfLevel(mc, ptop)
			if err != nil {
				return err
			}
			if mcKiPtop >= pl.NumKeys() {
				m := *t.cur(mn)
				w := t.cur(mc)
				for i := 0; i < ptop; i++ {
					w.pg[i] = m.pg[i]
					w.ki[i] = m.ki[i]
				}
				w.pg[ptop] = m.pg[ptop]
				if m.ki[ptop] != 0 {
					w.ki[ptop] = m.ki[ptop] - 1
				} else {
					// Find right page's left sibling.
					w.ki[ptop] = m.ki[ptop]
					_ = t.readOp(mc, func(c, _ *rcursor) error { return c.sibling(t, false) })
				}
			}
		}
	} else {
		t.cur(mn).top--
		ki := int(t.cur(mn).ki[ptop])
		err := t.nodeAdd(mn, ki, sepkey, nil, rp, 0)
		t.cur(mn).top++
		if err != nil {
			return err
		}
	}
	top := t.cur(mc).top
	switch {
	case nflags&format.Append != 0:
		w := t.cur(mc)
		w.pg[top] = rp
		w.ki[top] = 0
		if err := t.nodeAdd(mc, 0, newkey, newdata, newpgno, nflags); err != nil {
			return err
		}
		m := t.cur(mn)
		copy(t.cur(mc).ki[:top], m.ki[:top])
	case !isLeaf2:
		// Move nodes.
		t.cur(mc).pg[top] = rp
		i, j := splitIndx, 0
		toCopy := false
		for {
			var rkey []byte
			var rdata *nodeVal
			var pgno uint64
			var flags uint32
			if i == newindx {
				rkey, rdata, pgno, flags = newkey, newdata, newpgno, nflags
				// Update index for the new key.
				t.cur(mc).ki[top] = uint16(j)
			} else {
				if i < 0 || i >= len(copyPtrs) {
					return format.Corrupted
				}
				node, err := nodeAt(l, src, copyPtrs[i])
				if err != nil {
					return err
				}
				k, ok := node.Key()
				if !ok {
					return format.Corrupted
				}
				rkey = k
				if isLeaf {
					body, err := nodeBodyOf(node)
					if err != nil {
						return err
					}
					rdata = &nodeVal{size: node.DataSize(), bytes: body}
				} else {
					pgno = node.ChildPgno()
				}
				flags = uint32(node.Flags())
			}
			// First branch index doesn't need key data.
			if !isLeaf && j == 0 {
				rkey = nil
			}
			var err error
			if toCopy {
				err = t.nodeAddTo(mc, cp, j, rkey, rdata, pgno, flags)
			} else {
				err = t.nodeAdd(mc, j, rkey, rdata, pgno, flags)
			}
			if err != nil {
				return err
			}
			if i == nkeys {
				i, j = 0, 0
				toCopy = true
			} else {
				i++
				j++
			}
			if i == splitIndx {
				break
			}
		}
		// Write the left half back into mp: mp_lower, mp_upper, the
		// pointers and the node area.
		n := numKeys(l, cp)
		cu := pUpper(l, cp)
		lo := l.LowerOffset()
		ptrsEnd := l.PageHdr + 2*n
		page, err := t.dirtyMut(mp)
		if err != nil {
			return err
		}
		if ptrsEnd > ps || cu > ps {
			return format.Corrupted
		}
		copy(page[lo:ptrsEnd], cp[lo:ptrsEnd])
		copy(page[cu:ps], cp[cu:ps])
		// Reset back to original page.
		if newindx < splitIndx {
			t.cur(mc).pg[top] = mp
		} else if err := t.moveToRightPage(mc, mn, rp, top, ptop); err != nil {
			return err
		}
	case newindx >= splitIndx:
		if err := t.moveToRightPage(mc, mn, rp, top, ptop); err != nil {
			return err
		}
	}
	return t.splitFixups(mc, mn, mp, rp, newindx, nflags, newRoot, didSplit, ptop)
}

// moveToRightPage: after a split the cursor's node is on the right page:
// move there, and make sure mc_ki is still valid (the parent may have
// split).
func (t *Txn) moveToRightPage(mc, mn cref, rp uint64, top, ptop int) error {
	w := t.cur(mc)
	w.pg[top] = rp
	w.ki[ptop]++
	if t.cur(mn).pg[ptop] != w.pg[ptop] {
		p, err := t.pageOfLevel(mc, ptop)
		if err != nil {
			return err
		}
		if int(w.ki[ptop]) >= p.NumKeys() {
			m := t.cur(mn)
			copy(w.pg[:ptop+1], m.pg[:ptop+1])
			copy(w.ki[:ptop+1], m.ki[:ptop+1])
		}
	}
	return nil
}

// splitLeaf2 is the P_LEAF2 split: move keys splitIndx.. of mp (a copy of
// which is src) to rp, inserting the new key on its side. Returns the
// separator key.
func (t *Txn) splitLeaf2(mc cref, mp, rp uint64, src, newkey []byte, newindx, splitIndx, nkeys int) ([]byte, error) {
	ksize := int(t.dbOf(mc).Pad)
	if len(newkey) != ksize {
		return nil, format.BadValSize
	}
	l := t.l
	hdr := l.PageHdr
	x := newindx - splitIndx
	split := hdr + splitIndx*ksize
	rsize := (nkeys - splitIndx) * ksize
	lsize := (nkeys - splitIndx) * 2
	if split < 0 || rsize < 0 || split+rsize > len(src) || ksize < 2 {
		return nil, format.Corrupted
	}
	keys := src[split : split+rsize]
	var sepkey []byte
	if newindx == splitIndx {
		sepkey = newkey
	} else {
		if len(keys) < ksize {
			return nil, format.Corrupted
		}
		sepkey = keys[:ksize]
	}
	page, err := t.dirtyMut(mp)
	if err != nil {
		return nil, err
	}
	setLower(l, page, pLower(l, page)-lsize)
	setUpper(l, page, pUpper(l, page)+rsize-lsize)
	rpage, err := t.dirtyMut(rp)
	if err != nil {
		return nil, err
	}
	setLower(l, rpage, pLower(l, rpage)+lsize)
	setUpper(l, rpage, pUpper(l, rpage)-(rsize-lsize))
	if x < 0 {
		// The new key goes left: all keys from splitIndx move right.
		copy(rpage[hdr:hdr+rsize], keys)
		ins := hdr + newindx*ksize
		if ins < 0 || split+ksize > len(page) {
			return nil, format.Corrupted
		}
		copy(page[ins+ksize:split+ksize], page[ins:split])
		copy(page[ins:ins+ksize], newkey)
		setLower(l, page, pLower(l, page)+2)
		setUpper(l, page, pUpper(l, page)-(ksize-2))
	} else {
		ins := hdr + x*ksize
		if ins+ksize+rsize-x*ksize > len(rpage) || x*ksize > len(keys) {
			return nil, format.Corrupted
		}
		copy(rpage[hdr:ins], keys[:x*ksize])
		copy(rpage[ins:ins+ksize], newkey)
		copy(rpage[ins+ksize:], keys[x*ksize:])
		setLower(l, rpage, pLower(l, rpage)+2)
		setUpper(l, rpage, pUpper(l, rpage)-(ksize-2))
		w := t.cur(mc)
		w.ki[w.top] = uint16(x)
	}
	return sepkey, nil
}

// splitFixups is the cursor fixup at the end of mdb_page_split: every
// other tracked cursor on the split page mp follows its node (to rp if it
// moved there), and after a root split every cursor on the old root gets
// the new root pushed under it.
func (t *Txn) splitFixups(mc, mn cref, mp, rp uint64, newindx int, nflags uint32, newRoot int, didSplit bool, ptop int) error {
	mpage, err := t.page(mp)
	if err != nil {
		return err
	}
	nkeys := mpage.NumKeys()
	w := t.cur(mc)
	dbi, sub, top := w.dbi, w.isSub(), w.top
	rootPg := w.pg[0]
	mcPgPtop, mcKiPtop := w.pg[ptop], w.ki[ptop]
	m := *t.cur(mn)
	for s := range t.w.cursors.slots {
		m3, ok := t.w.cursors.peer(s, dbi, sub)
		if !ok || m3 == mc {
			continue
		}
		sl := t.w.cursors.slot(s)
		m2Init := sl != nil && sl.main.isInit()
		w3 := t.cur(m3)
		if !(m2Init && w3.flags&cInitialized != 0) {
			continue
		}
		if w3.subp && w3.snum <= 1 {
			// A sub-page cursor: sub-pages are never split.
			continue
		}
		if newRoot >= 0 {
			// Sub cursors may be on a different DB.
			if w3.pg[0] != mp {
				continue
			}
			// Root split.
			for k := min(newRoot, format.CursorStack-2); k >= 0; k-- {
				w3.ki[k+1] = w3.ki[k]
				w3.pg[k+1] = w3.pg[k]
			}
			if int(w3.ki[0]) >= nkeys {
				w3.ki[0] = 1
			} else {
				w3.ki[0] = 0
			}
			w3.pg[0] = rootPg
			w3.snum++
			w3.top++
		}
		if w3.top >= top && w3.pg[top] == mp {
			if int(w3.ki[top]) >= newindx && nflags&splitReplace == 0 {
				w3.ki[top]++
			}
			if int(w3.ki[top]) >= nkeys {
				w3.pg[top] = rp
				w3.ki[top] -= uint16(nkeys)
				copy(w3.ki[:top], m.ki[:top])
				copy(w3.pg[:top], m.pg[:top])
			}
		} else if !didSplit && w3.top >= ptop && w3.pg[ptop] == mcPgPtop && w3.ki[ptop] >= mcKiPtop {
			w3.ki[ptop]++
		}
	}
	return nil
}

// nodeAt is the node at offset off of page bytes src.
func nodeAt(l *format.Layout, src []byte, off int) (format.Node, error) {
	if off < 0 || off > len(src) {
		return format.Node{}, format.Corrupted
	}
	n, ok := format.NewNode(src[off:], l)
	if !ok {
		return n, format.Corrupted
	}
	return n, nil
}

// nodeBodyOf is the stored data part of a leaf node: the inline bytes, or
// the overflow reference of an F_BIGDATA node.
func nodeBodyOf(node format.Node) ([]byte, error) {
	if node.IsBigData() {
		start := node.L.NodeDataOffset(node.KeySize())
		raw, ok := node.Raw(true)
		if !ok || start+node.L.OvpageSize > len(raw) {
			return nil, format.Corrupted
		}
		return raw[start : start+node.L.OvpageSize], nil
	}
	d, ok := node.InlineData()
	if !ok {
		return nil, format.Corrupted
	}
	return d, nil
}
