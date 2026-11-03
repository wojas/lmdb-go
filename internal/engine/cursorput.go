// Inserting and replacing items: mdb_cursor_put (mdb.c line 7244, here
// putAt), mdb_node_add (line 7475), mdb_page_new (line 7382),
// mdb_leaf_size / mdb_branch_size and mdb_update_key (line 7962) (rlmdb
// core/cursor_put.rs).
//
// mdb_cursor_put is one long C function with gotos between its stages;
// here each stage is a function and the jumps are return values:
//
//  1. position the cursor (MDB_SET, MDB_APPEND -> last key, or the current
//     position for MDB_CURRENT); an empty tree gets a root leaf;
//  2. touch the pages on the cursor's path (cursorTouch);
//  3. for an existing key: overwrite in place where possible (putCurrent),
//     or for MDB_DUPSORT prepare the node that will hold the duplicates
//     (dupPrepare): convert a single item into a sub-page, grow a
//     sub-page, or promote it to a sub-DB; where C's in-place replacement
//     goes wrong, delete the item and insert the new one instead
//     (replaceCurrent);
//  4. insert the node, splitting the leaf if it is full (insertLeafNode),
//     and fix up the other cursors;
//  5. for duplicates, store the item(s) through the sub-cursor (putSub)
//     and write the sub-DB record back into the node.

package engine

import (
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// mdbNoSpill is MDB_NOSPILL: do not spill pages if the transaction is
// getting full (internal put/del flag).
const mdbNoSpill uint32 = 0x8000

// splitReplace is MDB_SPLIT_REPLACE: the key being split in is not new.
const splitReplace = format.AppendDup

// nodeAddFlags are the put flags mdb_node_add and mdb_page_split use.
const nodeAddFlags = uint32(format.FDupData) | uint32(format.FSubData) | format.Reserve | format.Append

// Results of the positioning step.
const (
	posFound = iota
	posNotFound
	posNoRoot
)

// putData is the data argument of a put (MDB_val *data, or the data[2]
// array of MDB_MULTIPLE).
type putData struct {
	// bytes are the bytes (all items for MDB_MULTIPLE; nil for
	// MDB_RESERVE).
	bytes []byte
	// size is data[0].mv_size: the size of one item.
	size int
	// count is data[1].mv_size for MDB_MULTIPLE.
	count int
}

func putBytes(b []byte) putData { return putData{bytes: b, size: len(b), count: 1} }
func putReserve(n int) putData  { return putData{size: n, count: 1} }
func putEmpty() putData         { return putData{bytes: []byte{}, count: 1} }
func putMultiple(b []byte, size, count int) putData {
	return putData{bytes: b, size: size, count: count}
}

// item is item k as node data.
func (d putData) item(k int) nodeVal {
	b := d.bytes
	if len(b) != 0 {
		b = b[k*d.size : (k+1)*d.size]
	}
	return nodeVal{size: d.size, bytes: b}
}

// nodeVal is node data for nodeAdd: size bytes given by bytes (empty for
// MDB_RESERVE, the stored overflow reference for F_BIGDATA).
type nodeVal struct {
	size  int
	bytes []byte
}

// What the DUPSORT stage decided (C's goto targets).
const (
	dupCurrent = iota // goto current: overwrite the single item
	dupPutSub         // goto put_sub: the node has room for the item
	dupReplace        // an item that does not fit where it would replace one: delete, then put
	dupNewSub         // goto new_sub
)

type dupStep struct {
	kind int
	// For dupNewSub: (re)insert the node with xdata; then store the old
	// single item dkey (if converting) and the new item in the sub-tree
	// whose new root is subRoot (if promoted).
	xdata      []byte
	subRoot    uint64
	hasSubRoot bool
	dkey       []byte
	hasDkey    bool
}

// leafSizeOf is mdb_leaf_size: bytes a leaf node takes (data on overflow
// pages counts as an overflow reference), including its pointer, EVEN.
func leafSizeOf(l *format.Layout, nodemax, ksize, dsize int) int {
	sz := l.LeafSize(ksize, dsize)
	if sz > nodemax {
		// put on overflow page
		sz -= dsize - l.OvpageSize
	}
	return format.Even(sz + 2)
}

// branchSizeOf is mdb_branch_size: bytes a branch node takes, including
// its pointer. 0.9 does not round it up, 1.0 does (mdb.c 1.0 line 9334).
func branchSizeOf(l *format.Layout, ksize int) int {
	sz := l.BranchSize(ksize) + 2
	if l.EvenNodes {
		return format.Even(sz)
	}
	return sz
}

// overflowsNode: does mdb_node_add put dsize bytes of leaf data under a
// key of ksize bytes on overflow pages (F_BIGDATA)? Header, key (1.0:
// padded to an even length) and data, the data not padded, larger than
// nodemax. (LEAFSIZE, which decides whether a new MDB_DUPSORT key's first
// item goes straight to a sub-DB, also pads the data in 1.0, so an item
// that overflows a node always goes there.) rlmdb's overflows_node.
func overflowsNode(l *format.Layout, nodemax, ksize, dsize int) bool {
	return format.NodeSize+l.NodeKeyLen(ksize)+dsize > nodemax
}

// currentSingleOverflows: for MDB_CURRENT on a MDB_DUPSORT key, is the
// cursor on the key's only item (a node without duplicates), and would an
// item of dsize bytes under a key of ksize bytes go to overflow pages if
// it replaced it there (overflowsNode)?
func (t *Txn) currentSingleOverflows(mc cref, ksize, dsize int) (bool, error) {
	if !overflowsNode(t.l, t.env.nodeMax, ksize, dsize) {
		return false, nil
	}
	page, err := t.topPage(mc)
	if err != nil {
		return false, err
	}
	if page.IsLeaf2() {
		return false, nil
	}
	node, ok := page.Node(t.cur(mc).kiTop())
	if !ok {
		return false, format.Corrupted
	}
	return !node.IsDupData(), nil
}

// replaceCurrent is MDB_CURRENT as lmdb.h describes it for an item of
// another size: delete the item under mc, then put data under key through
// mc (which ends on it), as mdb_cursor_del + mdb_cursor_put would. For the
// replacements C gets wrong in place (KNOWN_C_BUGS.md rows 15, 38 and 56).
// Once the delete is done a failed insert leaves the transaction half
// changed, so it fails. rlmdb's RwTxn::replace_current.
func (t *Txn) replaceCurrent(mc cref, key []byte, data putData) error {
	if err := t.delAt(mc, 0); err != nil {
		return err
	}
	if _, err := t.putAt(mc, key, data, 0); err != nil {
		t.w.flags |= txnError
		return err
	}
	return nil
}

// currentDupFits: for MDB_CURRENT on a duplicate in sub-page fp, does the
// new item of size bytes fit where the sub-cursor's current item is?
func (t *Txn) currentDupFits(mc cref, fp []byte, size int) bool {
	l := t.l
	x := t.cur(subRef(mc.slot))
	if !x.isInit() || !x.subp || pFlags(l, fp)&format.PLeaf2 != 0 {
		return true // C's own checks apply
	}
	fpr, ok := format.NewPage(fp, l)
	if !ok {
		return true
	}
	node, ok := fpr.Node(x.kiTop())
	if !ok {
		return true
	}
	oldk := node.KeySize()
	return oldk == size || sizeLeft(l, fp)+format.Even(l.LeafSize(oldk, 0))+2 >= format.Even(l.LeafSize(size, 0)+2)
}

// currentDupOutsideLeaf: for MDB_CURRENT on a duplicate in a sub-DB tree,
// would C's replacement leave the new item where searches do not look for
// it (rlmdb KNOWN_C_BUGS.md row 56)? C keeps the new item in the old one's
// leaf, at the old one's index (_mdb_cursor_put, current:): in place for
// an item of the same size, then fix_parent, which makes it the branch key
// above the leaf if it is the leaf's first; for one of another size (0.9's
// ITS#8447 path, new_ksize) by mdb_node_del and mdb_node_add at the same
// index, without fix_parent. The branch keys around a leaf need not be its
// items (deleting a leaf's first item leaves the key above it), so the new
// item can sort before the branch key above its leaf (the leaf's first
// item replaced by one of another size), or at or after the next leaf's
// branch key (the leaf's last item, any size, MDB_DUPFIXED too). Searches
// then go to the neighbouring leaf: MDB_GET_BOTH says MDB_NOTFOUND, and
// later puts of items between the new one and that key land there, out of
// order. The keys are compared as mdb_node_search compares them, with the
// sub-DB's (duplicate) comparator. rlmdb's RwTxn::current_dup_outside_leaf.
func (t *Txn) currentDupOutsideLeaf(mc cref, item []byte) (bool, error) {
	if mc.sub {
		return false, nil
	}
	main, err := t.topPage(mc)
	if err != nil {
		return false, err
	}
	if main.IsLeaf2() {
		return false, nil
	}
	node, ok := main.Node(t.cur(mc).kiTop())
	if !ok || !node.IsDupData() || !node.IsSubData() {
		return false, nil // a single item or a sub-page: one leaf
	}
	mx := subRef(mc.slot)
	x := t.cur(mx)
	if !x.isInit() || x.subp || x.top == 0 {
		return false, nil // a single leaf: no branch keys
	}
	leaf, err := t.topPage(mx)
	if err != nil {
		return false, err
	}
	ki, nkeys := int(x.ki[x.top]), leaf.NumKeys()
	if ki >= nkeys {
		return false, nil
	}
	cmp := t.cmpOf(mx)
	if ki == 0 && !leaf.IsLeaf2() {
		old, ok := leaf.Node(0)
		if !ok {
			return false, format.Corrupted
		}
		if old.KeySize() != len(item) {
			// The branch key above the leaf: slot 0 of a branch page has
			// none, so it is that of the nearest level not on slot 0 (as
			// fix_parent finds it).
			for level := x.top - 1; level >= 0; level-- {
				if x.ki[level] == 0 {
					continue
				}
				rc, err := t.cmpBranchKey(mx, level, int(x.ki[level]), item, cmp)
				if err != nil {
					return false, err
				}
				if rc < 0 {
					return true, nil
				}
				break
			}
		}
	}
	if ki == nkeys-1 {
		// The branch key of the next leaf: the nearest one right of the
		// cursor's path.
		for level := x.top - 1; level >= 0; level-- {
			branch, err := t.pageOfLevel(mx, level)
			if err != nil {
				return false, err
			}
			if next := int(x.ki[level]) + 1; next < branch.NumKeys() {
				rc, err := t.cmpBranchKey(mx, level, next, item, cmp)
				if err != nil {
					return false, err
				}
				return rc >= 0, nil
			}
		}
	}
	return false, nil
}

// cmpBranchKey compares key with the key of node i of c's branch page at
// level, as mdb_node_search does (the integer comparators read past a
// shorter stored key, rlmdb KNOWN_C_BUGS.md row 7). rlmdb's
// RwTxn::cmp_branch_key.
func (t *Txn) cmpBranchKey(c cref, level, i int, key []byte, cmp format.Comparator) (int, error) {
	branch, err := t.pageOfLevel(c, level)
	if err != nil {
		return 0, err
	}
	if !branch.IsBranch() {
		return 0, format.Corrupted
	}
	node, ok := branch.Node(i)
	if !ok {
		return 0, format.Corrupted
	}
	k, ok := node.Key()
	if !ok {
		return 0, format.Corrupted
	}
	return cmp.CompareInPage(key, k, node.KeyTail()), nil
}

// putAt is _mdb_cursor_put: store key/data at cursor mc. Returns the
// number of items stored for MDB_MULTIPLE (data[1].mv_size).
func (t *Txn) putAt(mc cref, key []byte, data putData, flags uint32) (n int, err error) {
	defer func() {
		if r := recover(); r != nil {
			// A user comparator panicked: the tree may be half changed.
			t.w.flags |= txnError
			panic(r)
		}
	}()
	n, err = t.putAt0(mc, key, data, flags)
	if err != nil {
		t.failUnlessClean(err)
	}
	return n, err
}

func (t *Txn) putAt0(mc cref, key []byte, data putData, flags uint32) (int, error) {
	dcount := 0
	// Check this first so counter will always be zero on any early
	// failures.
	if flags&format.Multiple != 0 {
		if data.count == 0 {
			return 0, syscall.EINVAL
		}
		dcount = data.count
		if uint32(t.dbOf(mc).Flags)&format.DupFixed == 0 {
			return 0, format.Incompatible
		}
		if data.size != 0 && dcount > (1<<62)/data.size {
			return 0, format.BadValSize
		}
		if data.size*dcount > len(data.bytes) {
			return 0, syscall.EINVAL
		}
	}
	nospill := flags&mdbNoSpill != 0
	flags &^= mdbNoSpill
	if err := t.checkUsable(); err != nil {
		return 0, err
	}
	// ENV_MAXKEY(env)
	maxKey := t.env.maxKey
	if len(key) == 0 || len(key) > maxKey {
		return 0, format.BadValSize
	}
	db := *t.dbOf(mc)
	dupsort := uint32(db.Flags)&format.DupSort != 0
	if !t.cur(mc).isSub() && uint32(db.Flags)&format.DupFixed != 0 && !dupsort {
		// DUPFIXED needs DUPSORT (lmdb.h); C builds P_LEAF2 pages with a
		// key size of 0 and corrupts them (KNOWN_C_BUGS.md row 17).
		return 0, format.Incompatible
	}
	if dupsort && (data.size == 0 || data.size > maxKey) {
		return 0, format.BadValSize
	}
	if data.size > format.MaxDataSize {
		return 0, format.BadValSize
	}
	pos, err := t.putPosition(mc, key, &db, flags)
	if err != nil {
		return 0, err
	}
	if w := t.cur(mc); w.flags&cDel != 0 {
		w.flags ^= cDel
	}
	if dupsort && uint32(db.Flags)&format.DupFixed != 0 && pos == posFound {
		if err := t.checkDupFixedSize(mc, data.size); err != nil {
			return 0, err
		}
	}
	if dupsort && flags == format.Current {
		over, err := t.currentSingleOverflows(mc, len(key), data.size)
		if err != nil {
			return 0, err
		}
		if over {
			// C overwrites the key's only item in place (goto current) and
			// stores the new one on overflow pages: an F_BIGDATA node in a
			// DUPSORT DB, which no other put makes and which C's next
			// duplicate put reads as inline data. Delete and insert
			// instead, before anything else, as mdb_cursor_del and
			// mdb_cursor_put would: the item goes to a sub-DB, as a new
			// key's first item of that size does (KNOWN_C_BUGS.md row 38).
			return 0, t.replaceCurrent(mc, key, data)
		}
	}
	// C's sub-cursor put with MDB_CURRENT, of one item (MDB_MULTIPLE with
	// one item too; not MDB_RESERVE, which has no bytes).
	current := dupsort && flags&(format.Current|format.AppendDup) == format.Current
	if current && data.count == 1 && len(data.bytes) >= data.size {
		item := data.bytes[:data.size]
		outside, err := t.currentDupOutsideLeaf(mc, item)
		if err != nil {
			return 0, err
		}
		if outside {
			// C would keep the new duplicate in a leaf of the sub-DB where
			// searches do not look for it: MDB_GET_BOTH would not find it,
			// and later puts would land out of order. Delete and insert
			// instead, before anything else, as mdb_cursor_del and
			// mdb_cursor_put would (rlmdb KNOWN_C_BUGS.md row 56).
			if err := t.replaceCurrent(mc, key, putBytes(item)); err != nil {
				return 0, err
			}
			if flags&format.Multiple != 0 {
				return 1, nil
			}
			return 0, nil
		}
	}
	// Cursor is positioned, check for room in the dirty list.
	if !nospill {
		rsize := data.size
		if flags&format.Multiple != 0 {
			rsize = data.size * dcount
		}
		if err := t.spillCheck(mc, db.Depth, true, len(key), rsize); err != nil {
			return 0, err
		}
	}
	if pos == posNoRoot {
		// New database, write a root leaf page.
		np, err := t.pageNew(mc, format.PLeaf, 1)
		if err != nil {
			return 0, err
		}
		if err := t.cur(mc).push(np); err != nil {
			return 0, err
		}
		d := t.dbOf(mc)
		d.Root = np
		d.Depth++
		leaf2 := uint32(d.Flags)&(format.DupSort|format.DupFixed) == format.DupFixed
		t.setDBFlag(mc, dbDirty)
		if leaf2 {
			p, err := t.dirtyMut(np)
			if err != nil {
				return 0, err
			}
			setFlags(t.l, p, pFlags(t.l, p)|format.PLeaf2)
		}
		t.cur(mc).flags |= cInitialized
	} else if err := t.cursorTouch(mc); err != nil {
		return 0, err
	}
	insertKey := pos != posFound
	insertData := insertKey
	mcount := 0
	first := true
	for {
		item := data.item(mcount)
		done, rc, did, err := t.putItem(mc, &db, key, item, &flags, insertKey, insertData, first)
		if err != nil {
			return 0, err
		}
		insertData = did
		if done {
			return mcount, rc
		}
		if insertData {
			t.dbOf(mc).Entries++
		}
		if insertKey {
			t.cur(mc).flags |= cInitialized
		}
		if flags&format.Multiple != 0 {
			mcount++
			if mcount < dcount {
				insertKey, insertData, first = false, false, false
				continue
			}
		}
		return mcount, nil
	}
}

// putPosition is the positioning step of mdb_cursor_put. The cursor of
// mdb_put is positioned on its own stack (seekKey, seekLast), others
// through the read path.
func (t *Txn) putPosition(mc cref, key []byte, db *format.DB, flags uint32) (int, error) {
	if flags&format.Current != 0 {
		if !t.cur(mc).isInit() {
			return 0, syscall.EINVAL
		}
		// C does not check that the cursor is on an item
		// (KNOWN_C_BUGS.md row 13).
		p, err := t.topPage(mc)
		if err != nil {
			return 0, err
		}
		if t.cur(mc).kiTop() >= p.NumKeys() {
			return 0, format.NotFound
		}
		return posFound, nil
	}
	if db.Root == format.PInvalid {
		// New database, cursor has nothing to point to.
		w := t.cur(mc)
		w.snum, w.top = 0, 0
		w.flags &^= cInitialized
		return posNoRoot, nil
	}
	cmp := t.cmpOf(mc)
	inPlace := t.seeksInPlace(mc)
	var found bool
	switch {
	case flags&format.Append != 0:
		// ok false: an empty tree (see cursorLastForAppend); the cursor is
		// before its first slot, append there.
		var last []byte
		var ok bool
		var err error
		if inPlace {
			last, ok, err = t.seekLast(mc, db, cmp)
		} else {
			err = t.readOp(mc, func(c, x *rcursor) error {
				var e error
				last, _, ok, e = cursorLastForAppend(t, c, x)
				return e
			})
		}
		if err != nil {
			return 0, err
		}
		if ok && cmp.Compare(key, last) <= 0 {
			// New key is <= last key.
			return 0, format.KeyExist
		}
		w := t.cur(mc)
		w.ki[w.top]++
		found = false
	case inPlace:
		var err error
		if found, err = t.seekKey(mc, db, cmp, key); err != nil {
			return 0, err
		}
	default:
		var dcmp format.Comparator
		hasDcmp := false
		if !t.cur(mc).isSub() {
			e := t.dbe(t.cur(mc).dbi)
			dcmp, hasDcmp = e.dupCmp, e.hasDup
		}
		err := t.readOp(mc, func(c, x *rcursor) error {
			_, _, _, e := cursorSet(t, c, x, dcmp, hasDcmp, key, nil, false, format.Set, true)
			return e
		})
		switch {
		case err == nil:
			found = true
		case err == format.NotFound:
		default:
			return 0, err
		}
	}
	switch {
	case found && flags&format.NoOverwrite != 0:
		return 0, format.KeyExist
	case found:
		return posFound, nil
	}
	return posNotFound, nil
}

// checkDupFixedSize: MDB_DUPFIXED items must all have one size. C copies
// md_pad bytes regardless; a mismatch fails with MDB_BAD_VALSIZE before
// anything changes (KNOWN_C_BUGS.md row 12).
func (t *Txn) checkDupFixedSize(mc cref, size int) error {
	if t.cur(mc).isSub() {
		return nil
	}
	page, err := t.topPage(mc)
	if err != nil {
		return err
	}
	node, ok := page.Node(t.cur(mc).kiTop())
	if !ok {
		return nil
	}
	existing := node.DataSize()
	if node.IsDupData() {
		data, ok := node.InlineData()
		if !ok {
			return format.Corrupted
		}
		if node.IsSubData() {
			d, ok := format.ReadDB(data)
			if !ok {
				return format.Corrupted
			}
			existing = int(d.Pad)
		} else {
			sp, ok := format.NewPage(data, node.L)
			if !ok {
				return format.Corrupted
			}
			existing = int(sp.Pad())
		}
	}
	if existing != size {
		return format.BadValSize
	}
	return nil
}

// spillCheck is the start of mdb_page_spill: estimate how many pages the
// operation of mc may dirty (depth: its tree's md_depth; for puts the key
// and data size) and spill dirty pages if the dirty list lacks room.
func (t *Txn) spillCheck(mc cref, depth uint16, isPut bool, ksize, dsize int) error {
	if t.cur(mc).isSub() {
		return nil
	}
	ps := t.pageSize
	i := int(depth)
	// Named DBs also dirty the main DB.
	if t.cur(mc).dbi >= format.CoreDBs {
		i += int(t.dbe(format.MainDBI).db.Depth)
	}
	// For puts, roughly factor in the key+data size.
	if isPut {
		i += (t.l.LeafSize(ksize, dsize) + ps) / ps
	}
	i += i // double it for good measure
	if t.w.dirtyRoom > i {
		return nil
	}
	return t.pageSpill(mc, i)
}

// putItem is one pass of mdb_cursor_put from the insert/overwrite decision
// on (the body MDB_MULTIPLE repeats via goto more). done reports the put
// finished with rc; otherwise did is insert_data for the caller.
func (t *Txn) putItem(mc cref, db *format.DB, key []byte, item nodeVal, flags *uint32,
	insertKey, insertData, first bool) (done bool, rc error, did bool, err error) {
	dupsort := uint32(db.Flags)&format.DupSort != 0
	var sub *dupStep
	if insertKey {
		// The key does not exist.
		if dupsort && t.l.LeafSize(len(key), item.size) > t.env.nodeMax {
			// Too big for a node, insert in sub-DB.
			st, err := t.dupPrepare(mc, item, flags, true, true)
			if err != nil {
				return false, nil, false, err
			}
			if st.kind != dupNewSub {
				return false, nil, false, format.Corrupted
			}
			sub = &st
		}
	} else {
		top, err := t.topPage(mc)
		if err != nil {
			return false, nil, false, err
		}
		if first && top.IsLeaf2() {
			// There's only a key anyway, so this is a no-op.
			ksize := int(db.Pad)
			if len(key) != ksize {
				return false, nil, false, format.BadValSize
			}
			ki := t.cur(mc).kiTop()
			start := t.l.PageHdr + ki*ksize
			page, err := t.topPageMut(mc)
			if err != nil {
				return false, nil, false, err
			}
			if start+ksize > len(page) {
				return false, nil, false, format.Corrupted
			}
			copy(page[start:], key)
			return true, t.fixParent(mc, key), insertData, nil
		}
		goCurrent := !dupsort
		if dupsort {
			st, err := t.dupPrepare(mc, item, flags, false, false)
			if err != nil {
				return false, nil, false, err
			}
			switch st.kind {
			case dupCurrent:
				if overflowsNode(t.l, t.env.nodeMax, len(key), item.size) {
					// Overwriting the key's only item with one the
					// comparator calls equal (a C-made F_BIGDATA item, or
					// one of another size) that does not fit a node: C
					// would leave it on overflow pages (KNOWN_C_BUGS.md
					// row 38). Delete and insert (putAt counts the item); an
					// MDB_MULTIPLE batch goes on with its next item.
					if err := t.replaceCurrent(mc, key, putBytes(item.bytes)); err != nil {
						return false, nil, false, err
					}
					return *flags&format.Multiple == 0, nil, false, nil
				}
				goCurrent = true
			case dupPutSub:
				rc, ins := t.putSub(mc, item, *flags, nil, false, 0, false, false)
				return t.afterSub(mc, rc, ins, false)
			case dupReplace:
				// putAt counts the item itself. (Never MDB_MULTIPLE: its
				// MDB_DUPFIXED items have the size of the key's only one.)
				if err := t.replaceCurrent(mc, key, putBytes(item.bytes)); err != nil {
					return false, nil, false, err
				}
				return true, nil, false, nil
			case dupNewSub:
				sub = &st
			}
		}
		if goCurrent {
			reinsert, err := t.putCurrent(mc, key, item, *flags)
			if err != nil {
				return false, nil, false, err
			}
			if !reinsert {
				// An item equal to the key's only one: C returns here even
				// for MDB_MULTIPLE (KNOWN_C_BUGS.md row 30: here the batch
				// goes on).
				if *flags&format.Multiple != 0 {
					return false, nil, false, nil
				}
				return true, nil, insertData, nil
			}
		}
	}
	// new_sub:
	nv := item
	if sub != nil {
		nv = nodeVal{size: len(sub.xdata), bytes: sub.xdata}
	}
	if err := t.insertLeafNode(mc, key, nv, *flags, insertKey); err != nil {
		t.w.flags |= txnError
		return true, err, insertData, nil
	}
	if sub == nil {
		return false, nil, insertData, nil
	}
	rc, ins := t.putSub(mc, item, *flags, sub.dkey, sub.hasDkey, sub.subRoot, sub.hasSubRoot, insertKey)
	return t.afterSub(mc, rc, ins, insertKey)
}

// afterSub is the tail of mdb_cursor_put after storing into the sub-tree:
// count the item, handle a failure that left an empty sub-DB.
func (t *Txn) afterSub(mc cref, rc error, insertData, insertKey bool) (bool, error, bool, error) {
	if rc == nil {
		return false, nil, insertData, nil
	}
	if insertData {
		t.dbOf(mc).Entries++
	}
	if insertKey {
		// bad_sub: invalidate txn if we created an empty sub-DB.
		if rc == format.KeyExist {
			rc = format.Corrupted
		}
		t.w.flags |= txnError
	}
	return true, rc, false, nil
}

// fixParent is fix_parent: in mdb_cursor_put: after overwriting slot 0 of
// a leaf, update the branch key above.
func (t *Txn) fixParent(mc cref, key []byte) error {
	w := t.cur(mc)
	if w.top > 0 && w.ki[w.top] == 0 {
		dtop := 1
		top := w.top - 1
		for top > 0 && w.ki[top] == 0 {
			top--
			dtop++
		}
		doUpdate := w.ki[top] != 0
		w.top = top
		var err error
		if doUpdate {
			err = t.updateKey(mc, key)
		}
		t.cur(mc).top += dtop
		return err
	}
	return nil
}

// putCurrent is the current: stage: replace the data of the existing node
// in place if possible. reinsert reports the old node was deleted and the
// new one must be inserted (new_ksize).
func (t *Txn) putCurrent(mc cref, key []byte, item nodeVal, flags uint32) (reinsert bool, err error) {
	ps := t.pageSize
	l := t.l
	ki := t.cur(mc).kiTop()
	// The common case: inline data replaced by data of the same size
	// through a main cursor.
	if flags&format.Reserve == 0 && !t.cur(mc).isSub() {
		page, err := t.topPageMut(mc)
		if err != nil {
			return false, err
		}
		p := format.Page{B: page, L: l}
		if !p.IsLeaf2() {
			if n, ok := p.Node(ki); ok && (uint32(n.Flags())^flags)&uint32(format.FSubData) == 0 &&
				n.Flags()&format.FBigData == 0 && n.DataSize() == item.size {
				s, e, ok := nodeDataRange(l, page, ki)
				if !ok {
					return false, format.Corrupted
				}
				copy(page[s:e], item.bytes)
				return false, nil
			}
		}
	}
	page, err := t.topPage(mc)
	if err != nil {
		return false, err
	}
	n, ok := page.Node(ki)
	if !ok {
		return false, format.Corrupted
	}
	nflags, ksize, olddataSize := n.Flags(), n.KeySize(), n.DataSize()
	ovref, ovok := n.OverflowRef()
	// LMDB passes F_SUBDATA in 'flags' to write a DB record.
	if (uint32(nflags)^flags)&uint32(format.FSubData) != 0 {
		return false, format.Incompatible
	}
	switch {
	case nflags&format.FBigData != 0:
		// Overflow page overwrites need special handling.
		if !ovok {
			return false, format.Corrupted
		}
		pg := ovref.Pgno
		dpages := l.OverflowPagesFor(item.size, ps)
		omp, err := t.page(pg)
		if err != nil {
			return false, err
		}
		// 1.0 takes the run length from the node's op_pages, 0.9 from the
		// page (the same number in a sound file).
		ovpages := int(omp.OverflowPages())
		if l.Format == format.V10 {
			ovpages = int(min(ovref.Pages, 1<<40))
		}
		dirty := omp.Flags()&format.PDirty != 0
		level := t.pageLevel(pg)
		// Is the ov page large enough?
		if ovpages >= dpages && !dirty && level != 0 {
			// Spilled by this txn or an ancestor: make it dirty again.
			if dirty, err = t.pageUnspill(pg); err != nil {
				return false, err
			}
			level = 0 // dirty in this txn or clean
		}
		// Is it dirty? Yes, overwrite it.
		if ovpages >= dpages && dirty {
			if level > 1 {
				// Writable only in a parent txn: copy it.
				if _, err := t.copyAncestorPage(pg); err != nil {
					return false, err
				}
			}
			page, err := t.topPageMut(mc)
			if err != nil {
				return false, err
			}
			if !setNodeDSize(l, page, ki, item.size) {
				return false, format.Corrupted
			}
			if flags&format.Reserve == 0 {
				run, err := t.dirtyMut(pg)
				if err != nil {
					return false, err
				}
				if l.PageHdr+item.size > len(run) {
					return false, format.Corrupted
				}
				copy(run[l.PageHdr:], item.bytes)
			}
			return false, nil
		}
		if err := t.ovpageFree(mc, pg); err != nil {
			return false, err
		}
	case item.size == olddataSize:
		// Same size, just replace it.
		if flags&format.Reserve != 0 {
			return false, nil
		}
		if !t.cur(mc).isSub() {
			page, err := t.topPageMut(mc)
			if err != nil {
				return false, err
			}
			s, e, ok := nodeDataRange(l, page, ki)
			if !ok {
				return false, format.Corrupted
			}
			copy(page[s:e], item.bytes)
			return false, nil
		}
		if len(key) == ksize {
			page, err := t.topPageMut(mc)
			if err != nil {
				return false, err
			}
			off, ok := ptrAt(l, page, ki)
			off += format.NodeSize
			if !ok || off+ksize > len(page) {
				return false, format.Corrupted
			}
			copy(page[off:off+ksize], key)
			return false, t.fixParent(mc, key)
		}
	}
	// new_ksize:
	if err := t.nodeDel(mc, 0); err != nil {
		return false, err
	}
	return true, nil
}

// dupPrepare is the DUPSORT stage of mdb_cursor_put ("DB has dups?"):
// prepare the node that will hold the duplicates. With bigNew, a new key
// whose item does not fit a node goes straight to a sub-DB.
func (t *Txn) dupPrepare(mc cref, item nodeVal, flags *uint32, insertKey, bigNew bool) (dupStep, error) {
	ps := t.pageSize
	l := t.l
	nodemax := t.env.nodeMax
	dbflags := uint32(t.dbOf(mc).Flags)
	dupfixed := dbflags&format.DupFixed != 0
	leafPgno := t.cur(mc).pgTop()
	ki := t.cur(mc).kiTop()
	var st dupStep
	var fp []byte
	offset := 0
	toSubDB := bigNew
	xsize := 0
	if bigNew {
		// Set up an empty "old sub-page" for prep_subDB to expand to a
		// full page (P_DIRTY: the sub-DB root built from it is a dirty
		// page). C writes the size into its scratch page (me_pbuf), where
		// it stays for later sub-pages.
		t.w.pbufPad = uint16(item.size)
		fp = t.w.dupScratch(&t.w.dupSub, l.PageHdr, ps)
		setFlags(l, fp, format.PLeaf|format.PDirty)
		setPad(l, fp, t.w.pbufPad)
		setLower(l, fp, l.PageHdr)
		setUpper(l, fp, l.PageHdr)
	} else {
		page, err := t.topPage(mc)
		if err != nil {
			return st, err
		}
		n, ok := page.Node(ki)
		if !ok {
			return st, format.Corrupted
		}
		nflags, ksize := n.Flags(), n.KeySize()
		var od []byte
		// bigPgno is the overflow run of a lone item on overflow pages.
		var bigPgno uint64
		hasBig := false
		if nflags&(format.FBigData|format.FDupData) == format.FBigData {
			// A single item on overflow pages, which only C's MDB_CURRENT
			// stores in a DUPSORT DB (KNOWN_C_BUGS.md row 38). C reads the
			// overflow reference and what follows it in the page as the
			// item; read the item.
			if bigPgno, ok = n.OverflowPgno(); !ok {
				return st, format.Corrupted
			}
			hasBig = true
			if od, err = t.overflowData(bigPgno, n.DataSize()); err != nil {
				return st, err
			}
		} else if od, ok = n.InlineData(); !ok {
			return st, format.Corrupted
		}
		olddata := copyInto(&t.w.dupOld, od)
		switch {
		case nflags&format.FDupData == 0:
			// Was a single item before, must convert now.
			if *flags == format.Current {
				return dupStep{kind: dupCurrent}, nil
			}
			e := t.dbe(t.cur(mc).dbi)
			dcmp := format.Memn
			if e.hasDup {
				dcmp = e.dupCmp
			}
			if dcmp.IsInt() && len(olddata) == 8 {
				dcmp = format.Clong
			}
			// Does data match?
			if dcmp.Compare(item.bytes, olddata) == 0 {
				if *flags&(format.NoDupData|format.AppendDup) != 0 {
					return st, format.KeyExist
				}
				// Overwrite it.
				return dupStep{kind: dupCurrent}, nil
			}
			if hasBig {
				// The item becomes the first duplicate (a key of the
				// sub-tree, so at most the maximum key size, as C's
				// MDB_CURRENT checked): its overflow pages are free. C's
				// mdb_node_del leaves them allocated and counted: a leak.
				if len(olddata) == 0 || len(olddata) > t.env.maxKey {
					return st, format.Corrupted
				}
				if err := t.ovpageFree(mc, bigPgno); err != nil {
					return st, err
				}
			}
			// Make sub-page header for the dup items, with dummy body.
			size := l.PageHdr + len(olddata) + item.size
			pflags := l.SubpageFlags
			if dupfixed {
				pflags |= format.PLeaf2
				t.w.pbufPad = uint16(item.size)
				size += 2 * item.size // leave space for 2 more
			} else {
				size += 2*(2+format.NodeSize) + (len(olddata) & 1) + (item.size & 1)
			}
			h := t.w.dupScratch(&t.w.dupSub, size, ps)
			setPgno(h, leafPgno)
			// 1.0 also stamps the sub-page with the current txnid.
			l.SetTxnID(h, t.txnid)
			// Without MDB_DUPFIXED C leaves mp_pad as its scratch page has
			// it: whatever an earlier put left there.
			setPad(l, h, t.w.pbufPad)
			setFlags(l, h, pflags)
			setLower(l, h, l.PageHdr)
			setUpper(l, h, size)
			fp = h
			xsize = size
			st.dkey, st.hasDkey = olddata, true
			// mp == fp here: the fake page is the new node data.
			if format.NodeSize+ksize+xsize > nodemax {
				toSubDB = true
			}
		case nflags&format.FSubData != 0:
			// Data is on sub-DB, just store it.
			*flags |= uint32(format.FDupData | format.FSubData)
			return dupStep{kind: dupPutSub}, nil
		default:
			// Data is on sub-page.
			fits := *flags == format.Current
			if fits && !t.currentDupFits(mc, olddata, item.size) {
				// C writes in place and, if the item does not fit, splits
				// the sub-page as if it were a page; delete and insert
				// instead (KNOWN_C_BUGS.md row 15).
				return dupStep{kind: dupReplace}, nil
			}
			if !fits {
				if !dupfixed {
					left := sizeLeft(l, olddata)
					offset = format.Even(format.NodeSize + 2 + item.size)
					// If there's enough space, just use it, else grow by
					// whatever we need.
					offset = max(offset-left, 0)
				} else {
					offset = int(pPad(l, olddata))
					if sizeLeft(l, olddata) < offset {
						offset *= 4 // space for 4 more
					} else {
						// Big enough MDB_DUPFIXED sub-page.
						fits = true
					}
				}
			}
			if fits {
				page, err := t.topPageMut(mc)
				if err != nil {
					return st, err
				}
				s, e, ok := nodeDataRange(l, page, ki)
				if !ok {
					return st, format.Corrupted
				}
				sp := page[s:e]
				setFlags(l, sp, pFlags(l, sp)|l.SubpageDirty())
				setPgno(sp, leafPgno)
				x := t.cur(subRef(mc.slot))
				x.pg[0] = subpageNo
				x.subp = true
				*flags |= uint32(format.FDupData)
				return dupStep{kind: dupPutSub}, nil
			}
			xsize = len(olddata) + offset
			if format.NodeSize+ksize+xsize > nodemax {
				toSubDB = true
			}
			fp = olddata
		}
	}
	olddataSize := len(fp)
	if bigNew {
		olddataSize = l.PageHdr
	}
	fpFlags := pFlags(l, fp)
	fpPad := pPad(l, fp)
	st.kind = dupNewSub
	switch {
	case toSubDB:
		// Too big for a sub-page, convert to sub-DB (prep_subDB).
		fpFlags &^= format.PSubp
		var dummy format.DB
		if dupfixed {
			fpFlags |= format.PLeaf2
			dummy.Pad = uint32(fpPad)
			dummy.Flags = uint16(format.DupFixed)
			if dbflags&format.IntegerDup != 0 {
				dummy.Flags |= uint16(format.IntegerKey)
			}
		}
		dummy.Depth = 1
		dummy.LeafPages = 1
		dummy.Entries = uint64(numKeys(l, fp))
		mp, err := t.pageAlloc(1)
		if err != nil {
			return st, err
		}
		// me_pagespace + PAGEHDRSZ - olddata.mv_size (0.9: me_psize -
		// olddata.mv_size); negative only for large DUPFIXED items on 1.0's
		// small pages, where C's unsigned arithmetic wraps back.
		subOffset := ps - olddataSize
		*flags |= uint32(format.FDupData | format.FSubData)
		dummy.Root = mp
		t.dbOf(mc).LeafPages++
		page, err := t.dirtyMut(mp)
		if err != nil {
			return st, err
		}
		// A regular dirty page: P_DIRTY in every format.
		if !buildSubpage(l, page[:ps], fp, olddataSize, fpFlags, fpPad, subOffset, format.PDirty) {
			return st, format.Corrupted
		}
		t.w.dupRec = dummy.Bytes()
		st.xdata = t.w.dupRec[:]
		st.subRoot, st.hasSubRoot = mp, true
	case st.hasDkey:
		// Converting a single item: the fake sub-page is the data.
		st.xdata = fp
	default:
		// Grow the sub-page into a new buffer (C builds it in its scratch
		// page, which keeps the pad).
		t.w.pbufPad = fpPad
		mp := t.w.dupScratch(&t.w.dupSub, xsize, ps)
		setPgno(mp, leafPgno)
		l.SetTxnID(mp, t.txnid)
		if !buildSubpage(l, mp, fp, olddataSize, fpFlags, fpPad, offset, l.SubpageDirty()) {
			return st, format.Corrupted
		}
		st.xdata = mp
	}
	*flags |= uint32(format.FDupData)
	if !insertKey {
		if err := t.nodeDel(mc, 0); err != nil {
			return st, err
		}
	}
	return st, nil
}

// insertLeafNode is the new_sub: stage: add the leaf node for key at the
// cursor position, splitting the page if it has no room, and move other
// cursors on the page past the new node.
func (t *Txn) insertLeafNode(mc cref, key []byte, rdata nodeVal, flags uint32, insertKey bool) error {
	nflags := flags & nodeAddFlags
	p, err := t.topPage(mc)
	if err != nil {
		return err
	}
	isLeaf2, room := p.IsLeaf2(), p.SizeLeft()
	nsize := len(key)
	if !isLeaf2 {
		nsize = leafSizeOf(t.l, t.env.nodeMax, len(key), rdata.size)
	}
	if room < nsize {
		if flags&uint32(format.FDupData|format.FSubData) == uint32(format.FDupData) {
			nflags &^= format.Append // sub-page may need room to grow
		}
		if !insertKey {
			nflags |= splitReplace
		}
		return t.pageSplit(mc, key, &rdata, format.PInvalid, nflags)
	}
	// There is room already in this leaf page.
	ki := t.cur(mc).kiTop()
	if err := t.nodeAdd(mc, ki, key, &rdata, 0, nflags); err != nil {
		return err
	}
	// Adjust other cursors pointing to mp.
	w := t.cur(mc)
	i, snum, dbi, sub := w.top, w.snum, w.dbi, w.isSub()
	mp := t.pageIDOf(mc, i)
	mcKi := w.ki[i]
	for s := range t.w.cursors.slots {
		m3, ok := t.w.cursors.peer(s, dbi, sub)
		if !ok || m3 == mc || t.cur(m3).snum < snum || t.pageIDOf(m3, i) != mp {
			continue
		}
		w3 := t.cur(m3)
		if w3.ki[i] >= mcKi && insertKey {
			w3.ki[i]++
		}
	}
	return nil
}

// putSub is the put_sub: stage: store the item(s) in the duplicates'
// sub-tree through the sub-cursor, then write the sub-DB record back into
// the node. Returns the result and whether an item was added.
func (t *Txn) putSub(mc cref, item nodeVal, flags uint32, dkey []byte, hasDkey bool,
	subRoot uint64, hasSubRoot, insertKey bool) (error, bool) {
	rc, ins, err := t.putSub0(mc, item, flags, dkey, hasDkey, subRoot, hasSubRoot)
	if err != nil {
		return err, false
	}
	return rc, ins
}

func (t *Txn) putSub0(mc cref, item nodeVal, flags uint32, dkey []byte, hasDkey bool,
	subRoot uint64, hasSubRoot bool) (error, bool, error) {
	mx := subRef(mc.slot)
	p, err := t.topPage(mc)
	if err != nil {
		return nil, false, err
	}
	leaf, ok := p.Node(t.cur(mc).kiTop())
	if !ok {
		return nil, false, format.Corrupted
	}
	leafFlags := leaf.Flags()
	var xflags uint32
	if flags&(format.Current|format.AppendDup) == format.Current {
		// The sub-cursor keeps its position; make sure its record is this
		// key's (KNOWN_C_BUGS.md row 14).
		if err := t.refreshXDB(mc); err != nil {
			return nil, false, err
		}
		xflags = format.Current | mdbNoSpill
	} else {
		if err := t.xcursorInit1(mc); err != nil {
			return nil, false, err
		}
		xflags = mdbNoSpill
		if flags&format.NoDupData != 0 {
			xflags = format.NoOverwrite | mdbNoSpill
		}
	}
	if hasSubRoot {
		x := t.cur(mx)
		x.pg[0] = subRoot
		x.subp = false
	}
	// Converted, write the original data first.
	if hasDkey {
		if _, err := t.putAt(mx, dkey, putEmpty(), xflags); err != nil {
			if err == format.KeyExist {
				err = format.Corrupted
			}
			t.w.flags |= txnError
			return nil, false, err
		}
	}
	if leafFlags&format.FSubData == 0 || hasSubRoot {
		// Adjust other cursors pointing to mp.
		w := t.cur(mc)
		i, snum, dbi, mcKi := w.top, w.snum, w.dbi, w.ki[w.top]
		mp := w.pg[i]
		for s := range t.w.cursors.slots {
			m2, ok := t.w.cursors.peer(s, dbi, false)
			if !ok {
				continue
			}
			w2 := t.cur(m2)
			if m2 == mc || w2.snum < snum || !w2.isInit() {
				continue
			}
			if w2.pg[i] == mp && w2.ki[i] == mcKi {
				t.xcursorInit2(s, mc.slot, hasDkey)
			}
		}
	}
	ecount := t.dbOf(mx).Entries
	if flags&format.AppendDup != 0 {
		xflags |= format.Append
	}
	if len(item.bytes) != item.size {
		return nil, false, syscall.EINVAL
	}
	_, rc := t.putAt(mx, item.bytes, putEmpty(), xflags)
	if flags&uint32(format.FSubData) != 0 {
		if err := t.storeSubDBRecord(mc); err != nil {
			return nil, false, err
		}
	}
	insertData := t.dbOf(mx).Entries != ecount
	t.shareXDB(mc)
	return rc, insertData, nil
}

// storeSubDBRecord is mdb_subdb_adjust + memcpy(NODEDATA(leaf), &mx_db):
// write the sub-cursor's MDB_db into the node at mc, adding the change of
// its page counts to the main DB's (ITS#9388).
func (t *Txn) storeSubDBRecord(mc cref) error {
	nw := *t.dbOf(subRef(mc.slot))
	ki := t.cur(mc).kiTop()
	p, err := t.topPage(mc)
	if err != nil {
		return err
	}
	n, ok := p.Node(ki)
	if !ok {
		return format.Corrupted
	}
	d, ok := n.InlineData()
	if !ok {
		return format.Corrupted
	}
	old, ok := format.ReadDB(d)
	if !ok {
		return format.Corrupted
	}
	db := t.dbOf(mc)
	db.BranchPages += nw.BranchPages - old.BranchPages
	db.LeafPages += nw.LeafPages - old.LeafPages
	page, err := t.topPageMut(mc)
	if err != nil {
		return err
	}
	s, e, ok := nodeDataRange(t.l, page, ki)
	if !ok || e-s < format.DBSize {
		return format.Corrupted
	}
	nw.Write(page[s:e])
	return nil
}

// --- nodes and pages -----------------------------------------------------------

// pageNew is mdb_page_new: allocate num pages and initialise them as an
// empty page of type flags, counting them in the cursor's DB record.
func (t *Txn) pageNew(mc cref, flags uint16, num int) (uint64, error) {
	return t.pageNewWith(mc, flags, num, pageInit{})
}

// pageNewWith is pageNew with the buffer starting as init says.
func (t *Txn) pageNewWith(mc cref, flags uint16, num int, init pageInit) (uint64, error) {
	np, slot, err := t.pageAllocWith(num, init)
	if err != nil {
		return 0, err
	}
	l := t.l
	s, ok := t.w.dirty.resolve(slot, np)
	if !ok {
		return 0, format.Corrupted
	}
	page := t.w.dirty.at(s, np)
	setFlags(l, page, flags|format.PDirty)
	setLower(l, page, l.PageHdr)
	setUpper(l, page, t.pageSize)
	if flags&format.POverflow != 0 {
		setOverflowPages(l, page, uint32(num))
	}
	db := t.dbOf(mc)
	switch {
	case flags&format.PBranch != 0:
		db.BranchPages++
	case flags&format.PLeaf != 0:
		db.LeafPages++
	case flags&format.POverflow != 0:
		db.OverflowPages += uint64(num)
	}
	return np, nil
}

// nodeAdd is mdb_node_add on the top page of mc.
func (t *Txn) nodeAdd(mc cref, indx int, key []byte, data *nodeVal, pgno uint64, flags uint32) error {
	return t.nodeAddTo(mc, nil, indx, key, data, pgno, flags)
}

// nodeAddTo is mdb_node_add (mdb.c line 7475): add a node at indx of the
// top page of mc, or of scratch (the temporary page of a split). Leaf
// data too big for a node goes to newly allocated overflow pages.
// MDB_PAGE_FULL (and MDB_TXN_ERROR) if the node does not fit.
func (t *Txn) nodeAddTo(mc cref, scratch []byte, indx int, key []byte, data *nodeVal, pgno uint64, flags uint32) error {
	ps := t.pageSize
	l := t.l
	nodemax := t.env.nodeMax
	page := scratch
	if page == nil {
		var err error
		if page, err = t.topPageMut(mc); err != nil {
			return err
		}
	}
	p, ok := format.NewPage(page, l)
	if !ok {
		return format.Corrupted
	}
	isLeaf2, isLeaf, room := p.IsLeaf2(), p.IsLeaf(), p.SizeLeft()-2
	if isLeaf2 {
		// Move higher keys up one slot and insert the new key.
		ksize := int(t.dbOf(mc).Pad)
		if len(key) != ksize {
			t.w.flags |= txnError
			return format.BadValSize
		}
		if !insertLeaf2(l, page, indx, key, ksize) {
			return format.PageFull
		}
		return nil
	}
	nodeSize := format.NodeSize + l.NodeKeyLen(len(key))
	body := nodeBody{kind: bodyBranch, pgno: pgno}
	if isLeaf {
		if data == nil {
			return format.Corrupted
		}
		d := *data
		switch {
		case flags&uint32(format.FBigData) != 0:
			// Data already on overflow page.
			nodeSize += l.OvpageSize
			r, ok := l.ReadOverflowRef(d.bytes)
			if !ok {
				return format.Corrupted
			}
			body = nodeBody{kind: bodyOverflow, ovref: r, size: d.size}
		case overflowsNode(l, nodemax, len(key), d.size):
			// Put data on overflow page.
			ovpages := l.OverflowPagesFor(d.size, ps)
			nodeSize = format.Even(nodeSize + l.OvpageSize)
			if nodeSize > room {
				return t.pageFull()
			}
			// The run is created with the data in it (MDB_RESERVE:
			// zero-filled, for the caller to fill in).
			init := pageInit{kind: initData, src: d.bytes}
			if flags&format.Reserve != 0 {
				init = pageInit{}
			}
			ofp, err := t.pageNewWith(mc, format.POverflow, ovpages, init)
			if err != nil {
				return err
			}
			flags |= uint32(format.FBigData)
			body = nodeBody{kind: bodyOverflow, size: d.size,
				ovref: format.OverflowRef{Pgno: ofp, TxnID: t.txnid, Pages: uint64(ovpages)}}
			if scratch == nil {
				if page, err = t.topPageMut(mc); err != nil {
					return err
				}
			}
		default:
			nodeSize += l.NodeDataLen(d.size)
			if flags&format.Reserve != 0 {
				body = nodeBody{kind: bodyReserve, size: d.size}
			} else {
				body = nodeBody{kind: bodyInline, inline: d.bytes}
			}
		}
	}
	nodeSize = format.Even(nodeSize)
	if nodeSize > room {
		return t.pageFull()
	}
	if _, ok := insertNode(l, page, indx, nodeSize, key, uint16(flags), body); !ok {
		return format.Corrupted
	}
	return nil
}

func (t *Txn) pageFull() error {
	t.w.flags |= txnError
	return format.PageFull
}

// nodeDel is mdb_node_del on the top page of mc.
func (t *Txn) nodeDel(mc cref, ksize int) error {
	ki := t.cur(mc).kiTop()
	page, err := t.topPageMut(mc)
	if err != nil {
		return err
	}
	if !deleteNode(t.l, page, ki, ksize) {
		return format.Corrupted
	}
	return nil
}

// updateKey is mdb_update_key (mdb.c line 7962): replace the key of the
// branch node at mc's position; without room for a longer key, delete the
// node and re-insert it with a split.
func (t *Txn) updateKey(mc cref, key []byte) error {
	ki := t.cur(mc).kiTop()
	page, err := t.topPageMut(mc)
	if err != nil {
		return err
	}
	needsSplit, ok := updateKey(t.l, page, ki, key)
	if !ok {
		return format.Corrupted
	}
	if !needsSplit {
		return nil
	}
	// Not enough space left, do a delete and split.
	p, err := t.topPage(mc)
	if err != nil {
		return err
	}
	n, ok := p.Node(ki)
	if !ok {
		return format.Corrupted
	}
	pgno := n.ChildPgno()
	if err := t.nodeDel(mc, 0); err != nil {
		return err
	}
	return t.pageSplit(mc, key, nil, pgno, splitReplace)
}

// buildSubpage builds a (sub-)page mp from the old sub-page fp (whose first
// olddataSize bytes are the sub-page) grown by offset bytes (the
// "if (mp != fp)" block of mdb_cursor_put): same header with mp_upper +=
// offset, the nodes moved to the end and their pointers shifted, or the
// P_LEAF2 keys copied. offset is negative when a P_LEAF2 fake sub-page
// larger than a page becomes a sub-DB root, whose mp_upper is then
// computed in 16 bits, as C does. dirty is the P_DIRTY bit the page gets.
// ok is false if a result is out of range (corrupt input).
func buildSubpage(l *format.Layout, mp, fp []byte, olddataSize int, fpFlags, fpPad uint16, offset int, dirty uint16) bool {
	nkeys := numKeys(l, fp)
	fpUpper := pUpper(l, fp)
	setFlags(l, mp, fpFlags|dirty)
	setPad(l, mp, fpPad)
	setLower(l, mp, pLower(l, fp))
	mpUpper := fpUpper + offset
	if fpFlags&format.PLeaf2 != 0 {
		// MP_UPPER(mp) = MP_UPPER(fp) + offset in C's 16 bits: the fake
		// sub-page of a single item of more than 16 KiB (64 KiB pages) is
		// larger than 64 KiB, so its mp_upper wrapped, and the sum wraps
		// back to the new page's.
		mpUpper = l.PtrBase + (fpUpper-l.PtrBase+offset)&0xffff
	} else if mpUpper < 0 {
		return false
	}
	setUpper(l, mp, mpUpper)
	if fpFlags&format.PLeaf2 != 0 {
		n := nkeys * int(fpPad)
		hdr := l.PageHdr
		if hdr+n > len(mp) || hdr+n > len(fp) {
			return false
		}
		copy(mp[hdr:hdr+n], fp[hdr:hdr+n])
		return true
	}
	if fpUpper > olddataSize || olddataSize > len(fp) {
		return false
	}
	tail := fp[fpUpper:olddataSize]
	if mpUpper+len(tail) > len(mp) {
		return false
	}
	copy(mp[mpUpper:], tail)
	for i := 0; i < nkeys; i++ {
		v, ok := ptrAt(l, fp, i)
		if !ok || v+offset < 0 || !setPtr(l, mp, i, v+offset) {
			return false
		}
	}
	return true
}
