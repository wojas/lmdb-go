// Cursor internals: B-tree search and movement (rlmdb core/cursor.rs,
// ported from mdb.c).
//
// An rcursor is MDB_cursor without its sub-cursor: a stack of pages from
// the root to a leaf (mc_pg[]), the index taken at each level (mc_ki[]) and
// state flags. A MDB_DUPSORT database pairs the main cursor with a second
// rcursor over the duplicates of the current key (MDB_xcursor); duplicates
// live either in a sub-page stored inline in the leaf node (F_DUPDATA) or
// in a sub-DB whose MDB_db record is the node's data (F_DUPDATA|F_SUBDATA).
// Keys of a sub-DB are the duplicate data items; with MDB_DUPFIXED they are
// packed in P_LEAF2 pages.
//
// The functions are faithful ports, including C's quirks, because cursor
// positions after edge cases are observable behaviour:
//
//	pageSearch, pageSearchRoot  mdb_page_search, mdb_page_search_root
//	nodeSearch, searchPage      mdb_node_search
//	sibling                     mdb_cursor_sibling
//	cursorFirst, cursorLast     mdb_cursor_first, mdb_cursor_last
//	cursorNext, cursorPrev      mdb_cursor_next, mdb_cursor_prev
//	cursorSet                   mdb_cursor_set
//	xcursorInit1                mdb_xcursor_init1
//	nodeRead                    mdb_node_read
//
// For a sub-cursor the "key" is the duplicate data item and the data part
// is empty, like C's recursive calls with data == NULL. Page lookups go
// through Txn.page, which consults a write transaction's dirty pages first.

package engine

import (
	"bytes"
	"encoding/binary"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// Cursor state flags (mc_flags).
const (
	cInitialized uint8 = 0x01 // C_INITIALIZED: positioned
	cEOF         uint8 = 0x02 // C_EOF: past the last item
	cSub         uint8 = 0x04 // C_SUB: sub-cursor of a DUPSORT cursor
	cDel         uint8 = 0x08 // C_DEL: the last operation was a delete
)

// Page search flags.
const (
	psModify   uint8 = 1 // MDB_PS_MODIFY
	psRootOnly uint8 = 2 // MDB_PS_ROOTONLY
	psFirst    uint8 = 4 // MDB_PS_FIRST
	psLast     uint8 = 8 // MDB_PS_LAST
)

// emptyPage is a header-only page of either layout, filling unused stack
// slots: it has no keys.
var emptyPage = make([]byte, 24)

// rcursor is MDB_cursor without the sub-cursor, with the bytes of each
// page on its stack.
type rcursor struct {
	pg   [format.CursorStack][]byte // mc_pg, root first
	ki   [format.CursorStack]uint16 // mc_ki
	snum int                        // mc_snum
	top  int                        // mc_top
	// flags is mc_flags (C_*).
	flags uint8
	// db is *mc_db: the tree's record (for a sub-cursor, mx_db).
	db format.DB
	// cmp is mc_dbx->md_cmp (for a sub-cursor, the dup order).
	cmp format.Comparator
	// subpage is, for a sub-cursor over a P_SUBP sub-page, that page (the
	// root of its one-page tree); nil otherwise.
	subpage []byte
	l       *format.Layout
}

// init is mdb_cursor_init / mdb_xcursor_init0: an unpositioned cursor.
func (c *rcursor) init(db format.DB, cmp format.Comparator, flags uint8, l *format.Layout) {
	c.snum, c.top = 0, 0
	c.flags = flags
	c.db = db
	c.cmp = cmp
	c.subpage = nil
	c.l = l
	c.pg[0] = emptyPage
}

func (c *rcursor) page(i int) format.Page { return format.Page{B: c.pg[i], L: c.l} }

func (c *rcursor) topPage() format.Page { return format.Page{B: c.pg[c.top], L: c.l} }

func (c *rcursor) isInit() bool { return c.flags&cInitialized != 0 }

func (c *rcursor) kiTop() int { return int(c.ki[c.top]) }

// setKiTop sets mc_ki[mc_top] (an indx_t, truncated to 16 bits as in C).
func (c *rcursor) setKiTop(i int) { c.ki[c.top] = uint16(i) }

func (c *rcursor) leaf2KSize() int { return int(c.db.Pad) }

// currentNode is NODEPTR(mc_pg[top], mc_ki[top]).
func (c *rcursor) currentNode() (format.Node, error) {
	n, ok := c.topPage().Node(c.kiTop())
	if !ok {
		return n, format.Corrupted
	}
	return n, nil
}

// push is mdb_cursor_push.
func (c *rcursor) push(mp []byte) error {
	level := c.snum
	if level >= format.CursorStack {
		return format.CursorFull
	}
	c.pg[level] = mp
	c.ki[level] = 0
	c.top = level
	c.snum = level + 1
	return nil
}

// pop is mdb_cursor_pop.
func (c *rcursor) pop() {
	if c.snum > 0 {
		c.snum--
		if c.snum > 0 {
			c.top--
		} else {
			c.flags &^= cInitialized
		}
	}
}

// nodeSearch is mdb_node_search on the top page: sets mc_ki[top] to the
// smallest entry >= key.
func (c *rcursor) nodeSearch(key []byte) (found, exact bool, err error) {
	i, found, exact, err := searchPage(c.topPage(), c.cmp, c.leaf2KSize(), key)
	if err != nil {
		return false, false, err
	}
	c.setKiTop(i)
	return found, exact, nil
}

// pageSearch is mdb_page_search: put the root on the stack and descend to
// the leaf for key (or the first/last leaf with psFirst/psLast).
// MDB_NOTFOUND for an empty tree.
func (c *rcursor) pageSearch(t *Txn, key []byte, flags uint8) error {
	var root []byte
	if c.subpage != nil {
		root = c.subpage
	} else {
		if c.db.Root == format.PInvalid {
			return format.NotFound
		}
		p, err := t.page(c.db.Root)
		if err != nil {
			return err
		}
		root = p.B
	}
	c.pg[0] = root
	c.snum = 1
	c.top = 0
	if flags&psRootOnly != 0 {
		return nil
	}
	return c.pageSearchRoot(t, key, flags)
}

// pageSearchRoot is mdb_page_search_root: descend from the top page.
func (c *rcursor) pageSearchRoot(t *Txn, key []byte, flags uint8) error {
	mp := c.topPage()
	for mp.IsBranch() {
		nkeys := mp.NumKeys()
		if nkeys == 0 {
			return format.Corrupted
		}
		i := 0
		if flags&(psFirst|psLast) != 0 {
			if flags&psLast != 0 {
				i = nkeys - 1
				// If already initialised, see if we are already in the
				// right place: the child we descended into last time is
				// still on the stack (snapshots never change).
				if c.isInit() && c.kiTop() == i {
					if c.snum >= format.CursorStack {
						return format.CursorFull
					}
					c.top = c.snum
					c.snum++
					mp = c.topPage()
					continue
				}
			}
		} else {
			found, exact, err := c.nodeSearch(key)
			if err != nil {
				return err
			}
			if !found {
				i = nkeys - 1
			} else {
				i = c.kiTop()
				if !exact {
					if i == 0 {
						return format.Corrupted
					}
					i--
				}
			}
		}
		node, ok := mp.Node(i)
		if !ok {
			return format.Corrupted
		}
		child, err := t.page(node.ChildPgno())
		if err != nil {
			return err
		}
		c.setKiTop(i)
		if err := c.push(child.B); err != nil {
			return err
		}
		mp = child
	}
	if !mp.IsLeaf() {
		return format.Corrupted
	}
	c.flags |= cInitialized
	c.flags &^= cEOF
	return nil
}

// sibling is mdb_cursor_sibling: replace the top page with its right (or
// left) neighbour, climbing as far as needed. MDB_NOTFOUND at the edge of
// the tree, leaving the stack as it was.
func (c *rcursor) sibling(t *Txn, right bool) error {
	if c.snum < 2 {
		return format.NotFound // root has no siblings
	}
	c.pop()
	parent := c.topPage()
	ki := c.kiTop()
	var atEdge bool
	if right {
		atEdge = ki+1 >= parent.NumKeys()
	} else {
		atEdge = ki == 0
	}
	if atEdge {
		if err := c.sibling(t, right); err != nil {
			// Undo the pop before returning.
			c.top++
			c.snum++
			return err
		}
	} else if right {
		c.setKiTop(ki + 1)
	} else {
		c.setKiTop(ki - 1)
	}
	node, err := c.currentNode()
	var mp format.Page
	if err == nil {
		mp, err = t.page(node.ChildPgno())
	}
	if err != nil {
		c.flags &^= cInitialized | cEOF
		return err
	}
	if err := c.push(mp.B); err != nil {
		return err
	}
	if !right {
		c.setKiTop(max(mp.NumKeys()-1, 0))
	}
	return nil
}

// fetchMultiple is MDB_GET_MULTIPLE's fetchm: all items of the
// sub-cursor's current P_LEAF2 page, leaving it on the last of them.
func (c *rcursor) fetchMultiple() ([]byte, error) {
	mp := c.topPage()
	if !mp.IsLeaf2() {
		return nil, format.Corrupted
	}
	data, ok := mp.Leaf2Keys(c.leaf2KSize())
	if !ok {
		return nil, format.Corrupted
	}
	c.setKiTop(max(mp.NumKeys()-1, 0))
	return data, nil
}

// keyAt is the key at index i of mp, for P_LEAF2 (ksize-byte keys) and
// node pages alike, plus the bytes from the key to the end of the page.
func keyAt(mp format.Page, ksize, i int) (key, tail []byte, err error) {
	if mp.IsLeaf2() {
		k, ok1 := mp.Leaf2Key(i, ksize)
		tl, ok2 := mp.Leaf2KeyTail(i, ksize)
		if !ok1 || !ok2 {
			return nil, nil, format.Corrupted
		}
		return k, tl, nil
	}
	n, ok := mp.Node(i)
	if !ok {
		return nil, nil, format.Corrupted
	}
	k, ok := n.Key()
	if !ok {
		return nil, nil, format.Corrupted
	}
	return k, n.KeyTail(), nil
}

// cmpAt compares key with the stored key at index i of mp.
func cmpAt(cmp format.Comparator, key []byte, mp format.Page, ksize, i int) (int, error) {
	k, tail, err := keyAt(mp, ksize, i)
	if err != nil {
		return 0, err
	}
	return cmp.CompareInPage(key, k, tail), nil
}

// searchPage is mdb_node_search (mdb.c line 5422) on page mp of a tree
// ordered by cmp (ksize: its P_LEAF2 key size): binary search of key.
// Returns the index of the smallest entry >= key, found (false if every
// entry is smaller) and exact. Branch pages start at index 1 (the first
// branch key is implicit).
func searchPage(mp format.Page, cmp format.Comparator, ksize int, key []byte) (int, bool, bool, error) {
	nkeys := mp.NumKeys()
	low := 1
	if mp.IsLeaf() {
		low = 0
	}
	high := nkeys - 1
	// Branch pages have no data, so with integer keys alignment is
	// guaranteed and C uses the faster mdb_cmp_long/mdb_cmp_int, chosen by
	// the size of the second key on the page.
	if cmp.IsCint() && mp.IsBranch() {
		if n, ok := mp.Node(1); ok && n.KeySize() == 8 {
			cmp = format.Long
		} else {
			cmp = format.Int
		}
	}
	i, rc := 0, 0
	if !mp.IsLeaf2() && cmp.Kind == format.CmpMemn {
		// The default order gets its own loop, reading node i's key as
		// Page.NodeKey does (the same checks, NUMKEYS read once).
		b, base, pb := mp.B, mp.L.PageHdr, mp.L.PtrBase
		for low <= high {
			i = (low + high) >> 1
			p := base + 2*i
			if p+2 > len(b) {
				return 0, false, false, format.Corrupted
			}
			off := int(binary.NativeEndian.Uint16(b[p:])) + pb
			if off+format.NodeSize > len(b) {
				return 0, false, false, format.Corrupted
			}
			s := off + format.NodeSize
			ks := int(binary.NativeEndian.Uint16(b[off+6:]))
			if ks > len(b)-s {
				return 0, false, false, format.Corrupted
			}
			rc = bytes.Compare(key, b[s:s+ks])
			if rc == 0 {
				break
			}
			if rc > 0 {
				low = i + 1
			} else {
				high = i - 1
			}
		}
	} else {
		for low <= high {
			i = (low + high) >> 1
			var err error
			if rc, err = cmpAt(cmp, key, mp, ksize, i); err != nil {
				return 0, false, false, err
			}
			if rc == 0 {
				break
			}
			if rc > 0 {
				low = i + 1
			} else {
				high = i - 1
			}
		}
	}
	if rc > 0 {
		// Found entry is less than the key: skip to the next one.
		i++
	}
	exact := rc == 0 && nkeys > 0
	return i, i < nkeys, exact, nil
}

// newXCursor sets up x as the sub-cursor for entry (mdb_xcursor_init0);
// false if the database is not MDB_DUPSORT. Until xcursorInit1 runs the
// sub-tree is empty (C leaves mx_db uninitialised, KNOWN_C_BUGS.md #6).
func newXCursor(x *rcursor, e *txnDB, l *format.Layout) bool {
	if uint32(e.db.Flags)&format.DupSort == 0 {
		return false
	}
	x.init(format.EmptyDB(0), e.dupOrder(), cSub, l)
	return true
}

// nodeRead is mdb_node_read: the data of a leaf node, following
// F_BIGDATA to its overflow pages.
func nodeRead(t *Txn, leaf format.Node) ([]byte, error) {
	if !leaf.IsBigData() {
		d, ok := leaf.InlineData()
		if !ok {
			return nil, format.Corrupted
		}
		return d, nil
	}
	pgno, ok := leaf.OverflowPgno()
	if !ok {
		return nil, format.Corrupted
	}
	return t.overflowData(pgno, leaf.DataSize())
}

// xcursorInit1 is mdb_xcursor_init1: point the sub-cursor mx at the
// duplicates in node (a sub-DB record or a sub-page).
func xcursorInit1(mc, mx *rcursor, node format.Node) error {
	data, ok := node.InlineData()
	if !ok {
		return format.Corrupted
	}
	if node.IsSubData() {
		db, ok := format.ReadDB(data)
		if !ok {
			return format.Corrupted
		}
		mx.db = db
		mx.subpage = nil
		mx.pg[0] = emptyPage
		mx.snum = 0
		mx.top = 0
		mx.flags = cSub
	} else {
		// A sub-page has the layout of the page it is stored in.
		fp, ok := format.NewPage(data, node.L)
		if !ok {
			return format.Corrupted
		}
		mx.db = format.DB{Depth: 1, LeafPages: 1, Entries: uint64(fp.NumKeys()), Root: fp.Pgno()}
		mx.subpage = data
		mx.snum = 1
		mx.top = 0
		mx.flags = cInitialized | cSub
		mx.pg[0] = data
		mx.ki[0] = 0
		parentFlags := uint32(mc.db.Flags)
		if parentFlags&format.DupFixed != 0 {
			mx.db.Flags = uint16(format.DupFixed)
			mx.db.Pad = uint32(fp.Pad())
			if parentFlags&format.IntegerDup != 0 {
				mx.db.Flags |= uint16(format.IntegerKey)
			}
		}
	}
	// #if UINT_MAX < SIZE_MAX: 8-byte integer duplicates are size_t's.
	if mx.cmp.IsInt() && mx.db.Pad == 8 {
		mx.cmp = format.Clong
	}
	return nil
}

// readPosition is the common tail of first/last/next/prev: read the entry
// at the cursor position; for a node with duplicates set up the sub-cursor
// on its first (or last) duplicate and return that as the data.
func readPosition(t *Txn, mc, mx *rcursor, last bool) (key, data []byte, err error) {
	mp := mc.topPage()
	if !mp.IsLeaf() {
		return nil, nil, format.Corrupted
	}
	ki := mc.kiTop()
	if mp.IsLeaf2() {
		k, ok := mp.Leaf2Key(ki, mc.leaf2KSize())
		if !ok {
			return nil, nil, format.Corrupted
		}
		return k, nil, nil
	}
	leaf, ok := mp.Node(ki)
	if !ok {
		return nil, nil, format.Corrupted
	}
	switch {
	case leaf.IsDupData():
		if mx == nil {
			return nil, nil, format.Corrupted
		}
		if err := xcursorInit1(mc, mx, leaf); err != nil {
			return nil, nil, err
		}
		if last {
			data, _, err = cursorLast(t, mx, nil)
		} else {
			data, _, err = cursorFirst(t, mx, nil)
		}
		if err != nil {
			return nil, nil, err
		}
	case mc.flags&cSub == 0:
		if data, err = nodeRead(t, leaf); err != nil {
			return nil, nil, err
		}
	}
	k, ok := leaf.Key()
	if !ok {
		return nil, nil, format.Corrupted
	}
	return k, data, nil
}

func resetSub(mx *rcursor) {
	if mx != nil {
		mx.flags &^= cInitialized | cEOF
	}
}

// cursorFirst is mdb_cursor_first.
func cursorFirst(t *Txn, mc, mx *rcursor) ([]byte, []byte, error) {
	resetSub(mx)
	if !mc.isInit() || mc.top > 0 {
		if err := mc.pageSearch(t, nil, psFirst); err != nil {
			return nil, nil, err
		}
	}
	mc.flags |= cInitialized
	mc.flags &^= cEOF
	mc.setKiTop(0)
	return readPosition(t, mc, mx, false)
}

// cursorLast is mdb_cursor_last. Leaves C_EOF set, as C does.
func cursorLast(t *Txn, mc, mx *rcursor) ([]byte, []byte, error) {
	resetSub(mx)
	if !mc.isInit() || mc.top > 0 {
		if err := mc.pageSearch(t, nil, psLast); err != nil {
			return nil, nil, err
		}
	}
	nkeys := mc.topPage().NumKeys()
	if nkeys == 0 {
		return nil, nil, format.Corrupted
	}
	mc.setKiTop(nkeys - 1)
	mc.flags |= cInitialized | cEOF
	return readPosition(t, mc, mx, true)
}

// cursorLastForAppend is mdb_cursor_last as mdb_cursor_put uses it for
// MDB_APPEND, where the tree may be empty: a new key whose first duplicate
// does not fit a node goes straight to a new sub-DB whose root leaf has no
// items yet. C sets mc_ki to NUMKEYS - 1 (0xffff) there and compares with a
// node read out of bounds (KNOWN_C_BUGS.md #32); here an empty leaf yields
// ok false with the cursor before its first slot.
func cursorLastForAppend(t *Txn, mc, mx *rcursor) (key, data []byte, ok bool, err error) {
	resetSub(mx)
	if !mc.isInit() || mc.top > 0 {
		if err := mc.pageSearch(t, nil, psLast); err != nil {
			return nil, nil, false, err
		}
	}
	top := mc.topPage()
	nkeys := top.NumKeys()
	if nkeys == 0 {
		if !top.IsLeaf() {
			return nil, nil, false, format.Corrupted
		}
		mc.setKiTop(0xffff)
		mc.flags |= cInitialized | cEOF
		return nil, nil, false, nil
	}
	mc.setKiTop(nkeys - 1)
	mc.flags |= cInitialized | cEOF
	key, data, err = readPosition(t, mc, mx, true)
	return key, data, err == nil, err
}

// itemInLeaf is what readPosition returns for item ki of mp (the top page
// of mc), if that needs neither the sub-cursor nor overflow pages; ok is
// false if it does, and also where the general path would fail.
func itemInLeaf(mc *rcursor, mp format.Page, ki int) (key, data []byte, ok bool) {
	if !mp.IsLeaf() {
		return nil, nil, false
	}
	if mp.IsLeaf2() {
		k, ok := mp.Leaf2Key(ki, mc.leaf2KSize())
		return k, nil, ok
	}
	leaf, ok := mp.Node(ki)
	if !ok || leaf.Flags()&(format.FDupData|format.FBigData) != 0 {
		return nil, nil, false
	}
	if mc.flags&cSub != 0 {
		k, ok := leaf.Key()
		return k, nil, ok
	}
	return leaf.KeyAndData()
}

// stepInLeaf is the common case of cursorNext and cursorPrev in one pass:
// the cursor is on a leaf of a tree without duplicates (neither past its
// end nor after a delete), and its neighbour on the same page has key and
// data in the page; the cursor moves there. ok is false, with the cursor
// unchanged, in every other case, and the caller takes the general path
// from the same state, which gives the same result.
func stepInLeaf(mc *rcursor, forward bool) (key, data []byte, ok bool) {
	if mc.flags&(cInitialized|cEOF|cDel) != cInitialized || uint32(mc.db.Flags)&format.DupSort != 0 {
		return nil, nil, false
	}
	ki := mc.kiTop()
	if forward {
		ki++
	} else {
		if ki == 0 {
			return nil, nil, false
		}
		ki--
	}
	key, data, ok = itemInLeaf(mc, mc.topPage(), ki)
	if ok {
		mc.setKiTop(ki)
	}
	return key, data, ok
}

// cursorNext is mdb_cursor_next for op in Next, NextDup, NextNoDup.
func cursorNext(t *Txn, mc, mx *rcursor, op format.CursorOp) ([]byte, []byte, error) {
	if k, d, ok := stepInLeaf(mc, true); ok {
		return k, d, nil
	}
	return cursorNextAny(t, mc, mx, op)
}

// cursorNextAny is cursorNext in every case.
func cursorNextAny(t *Txn, mc, mx *rcursor, op format.CursorOp) ([]byte, []byte, error) {
	if mc.flags&cDel != 0 && op == format.NextDup {
		return nil, nil, format.NotFound
	}
	if !mc.isInit() {
		return cursorFirst(t, mc, mx)
	}
	mp := mc.topPage()
	if mc.flags&cEOF != 0 {
		// C returns MDB_NOTFOUND at once for a cursor on the last item of
		// its page. In a write transaction the key may have got more
		// duplicates since, or the tree a page to the right, so look
		// (KNOWN_C_BUGS.md #28). Only a cursor past its page's end stops.
		if mc.kiTop() >= mp.NumKeys() {
			return nil, nil, format.NotFound
		}
		mc.flags ^= cEOF
	}
	if uint32(mc.db.Flags)&format.DupSort != 0 {
		// C reads NODEPTR(mp, ki) here without checking ki < NUMKEYS; a
		// position past the end counts as an item without duplicates
		// (KNOWN_C_BUGS.md #4).
		leaf, ok := mp.Node(mc.kiTop())
		if ok && leaf.IsDupData() {
			if op == format.Next || op == format.NextDup {
				if mx == nil {
					return nil, nil, format.Corrupted
				}
				dup, _, err := cursorNext(t, mx, nil, format.Next)
				if op != format.Next || err != format.NotFound {
					if err != nil {
						return nil, nil, err
					}
					k, ok := leaf.Key()
					if !ok {
						return nil, nil, format.Corrupted
					}
					return k, dup, nil
				}
			}
		} else {
			resetSub(mx)
			if op == format.NextDup {
				return nil, nil, format.NotFound
			}
		}
	}
	if mc.flags&cDel != 0 {
		mc.flags ^= cDel
	} else if mc.kiTop()+1 >= mp.NumKeys() {
		if err := mc.sibling(t, true); err != nil {
			mc.flags |= cEOF
			return nil, nil, err
		}
	} else {
		mc.ki[mc.top]++
	}
	return readPosition(t, mc, mx, false)
}

// cursorPrev is mdb_cursor_prev for op in Prev, PrevDup, PrevNoDup.
func cursorPrev(t *Txn, mc, mx *rcursor, op format.CursorOp) ([]byte, []byte, error) {
	if k, d, ok := stepInLeaf(mc, false); ok {
		return k, d, nil
	}
	return cursorPrevAny(t, mc, mx, op)
}

// cursorPrevAny is cursorPrev in every case.
func cursorPrevAny(t *Txn, mc, mx *rcursor, op format.CursorOp) ([]byte, []byte, error) {
	if !mc.isInit() {
		if _, _, err := cursorLast(t, mc, mx); err != nil {
			return nil, nil, err
		}
		mc.ki[mc.top]++
	}
	mp := mc.topPage()
	if uint32(mc.db.Flags)&format.DupSort != 0 && mc.kiTop() < mp.NumKeys() {
		leaf, ok := mp.Node(mc.kiTop())
		if !ok {
			return nil, nil, format.Corrupted
		}
		if leaf.IsDupData() {
			if op == format.Prev || op == format.PrevDup {
				if mx == nil {
					return nil, nil, format.Corrupted
				}
				dup, _, err := cursorPrev(t, mx, nil, format.Prev)
				if op != format.Prev || err != format.NotFound {
					if err != nil {
						return nil, nil, err
					}
					mc.flags &^= cEOF
					k, ok := leaf.Key()
					if !ok {
						return nil, nil, format.Corrupted
					}
					return k, dup, nil
				}
			}
		} else {
			resetSub(mx)
			if op == format.PrevDup {
				return nil, nil, format.NotFound
			}
		}
	}
	mc.flags &^= cEOF | cDel
	if mc.kiTop() == 0 {
		if err := mc.sibling(t, false); err != nil {
			return nil, nil, err
		}
		mc.setKiTop(max(mc.topPage().NumKeys()-1, 0))
	} else {
		mc.ki[mc.top]--
	}
	return readPosition(t, mc, mx, true)
}

// cursorSet is mdb_cursor_set for op in Set, SetKey, SetRange, GetBoth,
// GetBothRange. wantExact is C's non-NULL exactp (all but SetRange).
// Returns the position and whether the key matched exactly. hasData tells
// whether data was given (C's data != NULL for the GET_BOTH ops).
func cursorSet(t *Txn, mc, mx *rcursor, dcmp format.Comparator, hasDcmp bool,
	key, data []byte, hasData bool, op format.CursorOp, wantExact bool) (k, d []byte, exact bool, err error) {
	const (
		stepSearch = iota
		stepSearchLeaf
		stepPositioned
	)
	if len(key) == 0 {
		return nil, nil, false, format.BadValSize
	}
	resetSub(mx)
	step := stepSearch
	// See if we're already on the right page.
	if mc.isInit() {
		mp := mc.topPage()
		nkeys := mp.NumKeys()
		if nkeys == 0 {
			mc.setKiTop(0)
			return nil, nil, false, format.NotFound
		}
		rc, err := cmpAt(mc.cmp, key, mp, mc.leaf2KSize(), 0)
		if err != nil {
			return nil, nil, false, err
		}
		if rc == 0 {
			// The first node on the page was the one we wanted.
			mc.setKiTop(0)
			exact = true
			step = stepPositioned
		} else if rc > 0 {
			if nkeys > 1 {
				rc, err := cmpAt(mc.cmp, key, mp, mc.leaf2KSize(), nkeys-1)
				if err != nil {
					return nil, nil, false, err
				}
				if rc == 0 {
					// The last node was the one we wanted.
					mc.setKiTop(nkeys - 1)
					exact = true
					step = stepPositioned
				} else if rc < 0 {
					// This is definitely the right page, skip the tree search.
					step = stepSearchLeaf
					ki := mc.kiTop()
					if ki < nkeys {
						r, err := cmpAt(mc.cmp, key, mp, mc.leaf2KSize(), ki)
						if err != nil {
							return nil, nil, false, err
						}
						if r == 0 {
							// The current node was the one we wanted.
							exact = true
							step = stepPositioned
						}
					}
					if step != stepPositioned {
						mc.flags &^= cEOF
					}
				}
			}
			if step == stepSearch {
				// If any parents have right siblings, search; otherwise
				// there's nothing further.
				hasRight := false
				for i := 0; i < mc.top; i++ {
					if int(mc.ki[i])+1 < mc.page(i).NumKeys() {
						hasRight = true
						break
					}
				}
				if !hasRight {
					mc.setKiTop(nkeys)
					return nil, nil, false, format.NotFound
				}
			}
		}
		if step == stepSearch && mc.top == 0 {
			// There are no other pages.
			mc.setKiTop(0)
			if op == format.SetRange && !wantExact {
				step = stepPositioned
			} else {
				return nil, nil, false, format.NotFound
			}
		}
	}
	if step == stepSearch {
		if err := mc.pageSearch(t, key, 0); err != nil {
			return nil, nil, false, err
		}
		step = stepSearchLeaf
	}
	if step == stepSearchLeaf {
		found, ex, err := mc.nodeSearch(key)
		if err != nil {
			return nil, nil, false, err
		}
		exact = ex
		if wantExact && !exact {
			// MDB_SET specified and not an exact match.
			return nil, nil, false, format.NotFound
		}
		if !found {
			// Inexact leaf not found: the key is past this page; the
			// position is the first entry of the next page.
			if err := mc.sibling(t, true); err != nil {
				mc.flags |= cEOF
				return nil, nil, false, err
			}
		}
	}
	// set1:
	mc.flags |= cInitialized
	mc.flags &^= cEOF
	mp := mc.topPage()
	ki := mc.kiTop()
	if mp.IsLeaf2() {
		k, ok := mp.Leaf2Key(ki, mc.leaf2KSize())
		if !ok {
			return nil, nil, false, format.Corrupted
		}
		return k, nil, exact, nil
	}
	leaf, ok := mp.Node(ki)
	if !ok {
		return nil, nil, false, format.Corrupted
	}
	switch {
	case leaf.IsDupData():
		if mx == nil {
			return nil, nil, false, format.Corrupted
		}
		if err := xcursorInit1(mc, mx, leaf); err != nil {
			return nil, nil, false, err
		}
		if op == format.Set || op == format.SetKey || op == format.SetRange {
			if d, _, err = cursorFirst(t, mx, nil); err != nil {
				return nil, nil, false, err
			}
		} else {
			if !hasData {
				return nil, nil, false, syscall.EINVAL
			}
			if d, _, _, err = cursorSet(t, mx, nil, format.Comparator{}, false, data, nil, false,
				format.SetRange, op == format.GetBoth); err != nil {
				return nil, nil, false, err
			}
		}
	case mc.flags&cSub == 0:
		if op == format.GetBoth || op == format.GetBothRange {
			olddata, err := nodeRead(t, leaf)
			if err != nil {
				return nil, nil, false, err
			}
			if !hasData {
				return nil, nil, false, syscall.EINVAL
			}
			if !hasDcmp {
				return nil, nil, false, format.Incompatible
			}
			if dcmp.IsInt() && len(olddata) == 8 {
				dcmp = format.Clong
			}
			tail := olddata
			if !leaf.IsBigData() {
				tail = leaf.DataTail()
			}
			if rc := dcmp.CompareInPage(data, olddata, tail); rc != 0 {
				if op == format.GetBoth || rc > 0 {
					return nil, nil, false, format.NotFound
				}
			}
			d = olddata
		} else {
			resetSub(mx)
			if d, err = nodeRead(t, leaf); err != nil {
				return nil, nil, false, err
			}
		}
	}
	k, ok = leaf.Key()
	if !ok {
		return nil, nil, false, format.Corrupted
	}
	return k, d, exact, nil
}

// cursorGetCurrent is MDB_GET_CURRENT.
func cursorGetCurrent(t *Txn, mc, mx *rcursor) ([]byte, []byte, error) {
	if !mc.isInit() {
		return nil, nil, syscall.EINVAL
	}
	mp := mc.topPage()
	nkeys := mp.NumKeys()
	if nkeys == 0 || mc.kiTop() >= nkeys {
		mc.setKiTop(nkeys)
		return nil, nil, format.NotFound
	}
	if mp.IsLeaf2() {
		k, ok := mp.Leaf2Key(mc.kiTop(), mc.leaf2KSize())
		if !ok {
			return nil, nil, format.Corrupted
		}
		return k, nil, nil
	}
	leaf, ok := mp.Node(mc.kiTop())
	if !ok {
		return nil, nil, format.Corrupted
	}
	var data []byte
	switch {
	case leaf.IsDupData():
		if mx == nil {
			return nil, nil, format.Corrupted
		}
		dup, _, err := cursorGetCurrent(t, mx, nil)
		if err != nil {
			return nil, nil, err
		}
		mx.flags &^= cDel
		data = dup
	case mc.flags&cSub == 0:
		var err error
		if data, err = nodeRead(t, leaf); err != nil {
			return nil, nil, err
		}
	}
	k, ok := leaf.Key()
	if !ok {
		return nil, nil, format.Corrupted
	}
	return k, data, nil
}
