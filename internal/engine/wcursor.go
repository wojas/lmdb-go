// Cursors of a write transaction, and how the write path reaches pages
// (rlmdb core/wcursor.rs).
//
// A C cursor is a stack of page pointers; writes move pages around (a
// clean page is copied to a dirty buffer, a page is split or merged) and C
// keeps every open cursor valid by walking mt_cursors[dbi] after each
// change ("cursor fixups"). rlmdb's write cursors store page numbers
// instead, which identify pages exactly as C's pointers do within one
// transaction, plus the page's slot in the dirty list as a hint; every
// access looks the page up again. The one page without a number is a
// P_SUBP sub-page inside a leaf node: a sub-cursor on it records subpageNo
// and the page is found through its main cursor's current node, which
// makes C's XCURSOR_REFRESH implicit. (The structure is a consequence of
// Rust's borrow rules; the Go port keeps it because it is the tested
// structure whose decisions must be preserved, see PORTING.md.)
//
// All cursors of a write transaction live in an arena owned by the
// transaction; a cursor is named by a cref (slot, main or sub-cursor). A
// slot is tracked when C would link it into mt_cursors[dbi]: user cursors,
// the temporary cursor of mdb_put, and the temporary copies that
// WITH_CURSOR_TRACKING links in during splits and rebalances.
//
// Reading through a write cursor: readOp rebuilds read cursors (rcursor)
// from a write cursor's state, runs the read-path operation and stores the
// positions back. The positioning of every mdb_put and mdb_del on a fresh
// cursor of a tree without duplicates runs on the write cursor's own stack
// instead (seekKey, seekLast), with the same result.

package engine

import (
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// subpageNo is the page number in pg[0] of a sub-cursor whose tree is a
// P_SUBP sub-page: the page lives in the main cursor's current leaf node.
const subpageNo = format.PInvalid - 1

// dbRef is where a cursor's MDB_db record lives (mc_db): the
// transaction's table (sub false, index dbi), or the mx_db of slot's
// sub-cursor (sub true).
type dbRef struct {
	sub bool
	i   int
}

// wcursor is MDB_cursor with page numbers instead of page pointers.
type wcursor struct {
	dbi    int
	db     dbRef
	pg     [format.CursorStack]uint64
	pgSlot [format.CursorStack]pageSlot
	ki     [format.CursorStack]uint16
	snum   int
	top    int
	flags  uint8
	// subp: the tree is a P_SUBP sub-page (sub-cursors only).
	subp bool
}

func (w *wcursor) reset(dbi int, db dbRef, flags uint8) {
	w.dbi = dbi
	w.db = db
	for i := range w.pg {
		w.pg[i] = format.PInvalid
	}
	w.ki = [format.CursorStack]uint16{}
	w.snum, w.top = 0, 0
	w.flags = flags
	w.subp = false
}

func newWCursor(dbi int, db dbRef, flags uint8) wcursor {
	var w wcursor
	w.reset(dbi, db, flags)
	for i := range w.pgSlot {
		w.pgSlot[i] = noSlot
	}
	return w
}

func (w *wcursor) isInit() bool { return w.flags&cInitialized != 0 }

func (w *wcursor) isSub() bool { return w.flags&cSub != 0 }

func (w *wcursor) kiTop() int { return int(w.ki[w.top]) }

func (w *wcursor) pgTop() uint64 { return w.pg[w.top] }

// push is mdb_cursor_push for a page number.
func (w *wcursor) push(pgno uint64) error {
	if w.snum >= format.CursorStack {
		return format.CursorFull
	}
	w.top = w.snum
	w.snum++
	w.pg[w.top] = pgno
	w.ki[w.top] = 0
	return nil
}

// pop is mdb_cursor_pop.
func (w *wcursor) pop() {
	if w.snum > 0 {
		w.snum--
		if w.snum > 0 {
			w.top--
		} else {
			w.flags &^= cInitialized
		}
	}
}

// xstate is the sub-cursor of a MDB_DUPSORT cursor (MDB_xcursor).
type xstate struct {
	c      wcursor           // mx_cursor
	db     format.DB         // mx_db
	dbflag uint8             // mx_dbflag
	cmp    format.Comparator // mx_dbx.md_cmp: the duplicate order
}

func (x *xstate) reset(dbi, slot int, cmp format.Comparator) {
	x.c.reset(dbi, dbRef{sub: true, i: slot}, cSub)
	x.db = format.EmptyDB(0)
	x.dbflag = 0
	x.cmp = cmp
}

// aslot is one cursor of the arena: a main cursor and, for MDB_DUPSORT
// databases, its sub-cursor.
type aslot struct {
	main    wcursor
	x       xstate
	hasX    bool
	tracked bool // linked into mt_cursors[dbi]
	live    bool
	// gen is bumped whenever the slot is freed (cursorID validation).
	gen uint32
	// owner is the nesting depth of the transaction that opened it.
	owner uint32
}

// cref names a cursor in the arena: the main cursor of a slot or its
// sub-cursor (&mc->mc_xcursor->mx_cursor).
type cref struct {
	slot int
	sub  bool
}

func mainRef(slot int) cref { return cref{slot, false} }
func subRef(slot int) cref  { return cref{slot, true} }

// arena holds the cursors of a write transaction. A nested transaction
// takes over its parent's arena while it runs (mdb_cursor_shadow) and gives
// it back when it ends.
type arena struct {
	slots []aslot
	free  []int
	depth uint32
}

// open is mdb_cursor_init: a slot holding an unpositioned cursor on dbi
// and, for a DUPSORT database (hasDup, dup order dupCmp), its sub-cursor.
func (a *arena) open(dbi int, dupCmp format.Comparator, hasDup bool, tracked bool) int {
	var i int
	if k := len(a.free); k > 0 {
		i = a.free[k-1]
		a.free = a.free[:k-1]
		a.slots[i].main.reset(dbi, dbRef{i: dbi}, 0)
	} else {
		a.slots = append(a.slots, aslot{main: newWCursor(dbi, dbRef{i: dbi}, 0), x: xstate{c: newWCursor(dbi, dbRef{}, 0)}})
		i = len(a.slots) - 1
	}
	s := &a.slots[i]
	s.hasX = hasDup
	if hasDup {
		s.x.reset(dbi, i, dupCmp)
	}
	s.tracked = tracked
	s.live = true
	s.owner = a.depth
	return i
}

// alloc stores a new cursor and returns its slot.
func (a *arena) alloc(main wcursor, x *xstate, tracked bool) int {
	var i int
	if k := len(a.free); k > 0 {
		i = a.free[k-1]
		a.free = a.free[:k-1]
	} else {
		a.slots = append(a.slots, aslot{})
		i = len(a.slots) - 1
	}
	s := &a.slots[i]
	s.main = main
	s.hasX = x != nil
	if x != nil {
		s.x = *x
	}
	s.tracked = tracked
	s.live = true
	s.owner = a.depth
	return i
}

// backupSlot is a copy of a live slot (mc_backup of mdb_cursor_shadow).
type backupSlot struct {
	i int
	s aslot
}

// backup copies the live slots, to put back with restore if a nested
// transaction aborts.
func (a *arena) backup() []backupSlot {
	var out []backupSlot
	for i := range a.slots {
		if a.slots[i].live {
			out = append(out, backupSlot{i, a.slots[i]})
		}
	}
	return out
}

// restore puts slots saved by backup back (*mc = *bk in mdb_cursors_close
// without merge). Their generation numbers have not changed: a slot is
// only freed by the transaction that owns it.
func (a *arena) restore(b []backupSlot) {
	for _, e := range b {
		if e.i < len(a.slots) {
			a.slots[e.i] = e.s
		}
	}
}

// releaseOwned frees every slot opened at nesting depth depth.
func (a *arena) releaseOwned(depth uint32) {
	for i := range a.slots {
		if a.slots[i].owner == depth {
			a.release(i)
		}
	}
}

// release frees a slot (mdb_cursor_close, or the end of a temporary
// cursor).
func (a *arena) release(i int) {
	if i < len(a.slots) && a.slots[i].live {
		s := &a.slots[i]
		s.live = false
		s.tracked = false
		s.gen++
		a.free = append(a.free, i)
	}
}

// clear frees every slot (mdb_cursors_close).
func (a *arena) clear() {
	for i := range a.slots {
		a.release(i)
	}
}

// reset frees every slot for the next transaction, which then gets the
// slots in the order a new arena would hand them out (0, 1, ...). The
// generation numbers stay: a cursor handle of an ended transaction never
// matches a slot again.
func (a *arena) reset() {
	a.clear()
	a.free = a.free[:0]
	for i := len(a.slots) - 1; i >= 0; i-- {
		a.free = append(a.free, i)
	}
	a.depth = 0
}

// slot is slot i if it is live.
func (a *arena) slot(i int) *aslot {
	if i < len(a.slots) && a.slots[i].live {
		return &a.slots[i]
	}
	return nil
}

// cur is the cursor c.
func (a *arena) cur(c cref) *wcursor {
	s := &a.slots[c.slot]
	if c.sub {
		return &s.x.c
	}
	return &s.main
}

// peer is, for a fixup loop over mt_cursors[dbi] on behalf of a cursor
// with (sub) or without C_SUB, the cursor of tracked slot i to adjust.
func (a *arena) peer(i, dbi int, sub bool) (cref, bool) {
	s := a.slot(i)
	if s == nil || !s.tracked || s.main.dbi != dbi {
		return cref{}, false
	}
	if sub {
		if !s.hasX {
			return cref{}, false
		}
		return subRef(i), true
	}
	return mainRef(i), true
}

// pageID is the identity of a cursor's page at one level, for C's
// mc_pg[i] == mp comparisons: a page number, or the sub-page in leaf leaf
// at node idx.
type pageID struct {
	pg   uint64
	sub  bool
	leaf uint64
	idx  uint16
}

// subpageOf is the sub-page the sub-cursor of slot walks: the data of the
// main cursor's current node, or nil if that node holds no sub-page.
func (t *Txn) subpageOf(slot int) ([]byte, error) {
	m := &t.w.cursors.slots[slot].main
	if m.snum == 0 || m.pg[m.top] == subpageNo {
		return nil, nil
	}
	leaf, err := t.pageBySlot(m.pg[m.top], m.pgSlot[m.top])
	if err != nil {
		return nil, err
	}
	n, ok := leaf.Node(int(m.ki[m.top]))
	if !ok || n.Flags()&(format.FDupData|format.FSubData) != format.FDupData {
		return nil, nil
	}
	data, ok := n.InlineData()
	if !ok || len(data) < t.l.PageHdr {
		return nil, format.Corrupted
	}
	return data, nil
}

// cleanPage is the bytes of clean (mapped) page pgno.
func (t *Txn) cleanPage(pgno uint64) ([]byte, error) {
	if pgno > uint64(1<<62)/uint64(t.pageSize) {
		return nil, format.PageNotFound
	}
	b, ok := t.dmap.Slice(int(pgno)*t.pageSize, t.pageSize)
	if !ok {
		return nil, format.PageNotFound
	}
	return b, nil
}

// pageBySlot is Txn.page for a page of a write cursor, whose slot in this
// transaction's dirty list may be known (a hint checked against pgno). A
// page found there is the one mdb_page_get returns: a page in a
// transaction's own dirty list is never in its spill set.
func (t *Txn) pageBySlot(pgno uint64, slot pageSlot) (format.Page, error) {
	if b := t.w.dirty.at(slot, pgno); b != nil {
		return pageOf(b[:t.pageSize], t.l)
	}
	return t.page(pgno)
}

// pageAndSlot is pageBySlot that also returns the page's slot in this
// transaction's own dirty list (noSlot if not there).
func (t *Txn) pageAndSlot(pgno uint64, hint pageSlot) (format.Page, pageSlot, error) {
	s, b := t.w.dirty.lookup(hint, pgno)
	if b != nil {
		p, err := pageOf(b[:t.pageSize], t.l)
		return p, s, err
	}
	p, err := t.pageNotDirtyHere(pgno)
	return p, noSlot, err
}

// loadRead loads the state of write cursor c into the fresh read cursor
// out (out.init has run).
func (t *Txn) loadRead(c cref, out *rcursor) error {
	w := t.w.cursors.cur(c)
	var sub []byte
	if w.subp {
		var err error
		if sub, err = t.subpageOf(c.slot); err != nil {
			return err
		}
		if sub == nil {
			sub = emptyPage
		}
		out.subpage = sub
	}
	out.flags = w.flags
	out.ki = w.ki
	out.snum = min(w.snum, format.CursorStack)
	out.top = w.top
	for i := 0; i < out.snum; i++ {
		if i == 0 && sub != nil {
			out.pg[0] = sub
			continue
		}
		p, err := t.pageBySlot(w.pg[i], w.pgSlot[i])
		if err != nil {
			return err
		}
		out.pg[i] = p.B
	}
	return nil
}

// storeBack stores a read cursor's position back into w.
func storeBack(w *wcursor, rc *rcursor) {
	w.flags = rc.flags
	w.snum = rc.snum
	w.top = rc.top
	w.ki = rc.ki
	w.subp = rc.subpage != nil
	for i := 0; i < w.snum; i++ {
		if i == 0 && w.subp {
			w.pg[i] = subpageNo
		} else {
			w.pg[i] = pgnoOf(rc.pg[i])
		}
	}
}

// dbAndCmp is the record and key order of cursor c.
func (t *Txn) dbAndCmp(c cref) (format.DB, format.Comparator) {
	w := t.w.cursors.cur(c)
	if !w.db.sub {
		e := t.dbe(w.db.i)
		return e.db, e.keyCmp
	}
	x := &t.w.cursors.slots[w.db.i].x
	return x.db, x.cmp
}

// readCursors builds the read cursors of slot c.slot (main and
// sub-cursor) into mc and mx, for an operation on the main cursor c, or
// only the sub-cursor if c is one. hasX tells whether mx was built.
func (t *Txn) readCursors(c cref, mc, mx *rcursor) (hasX bool, err error) {
	db, cmp := t.dbAndCmp(c)
	mc.init(db, cmp, 0, t.l)
	if err := t.loadRead(c, mc); err != nil {
		return false, err
	}
	s := &t.w.cursors.slots[c.slot]
	if s.hasX && !c.sub {
		mx.init(s.x.db, s.x.cmp, 0, t.l)
		if err := t.loadRead(subRef(c.slot), mx); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// writeBack stores the state of read cursors built by readCursors back.
func (t *Txn) writeBack(c cref, mc, mx *rcursor, hasX bool) {
	storeBack(t.w.cursors.cur(c), mc)
	s := &t.w.cursors.slots[c.slot]
	if hasX && s.hasX {
		storeBack(&s.x.c, mx)
		s.x.db = mx.db
		s.x.cmp = mx.cmp
	}
}

// readOp runs a read-path operation on cursor c (and, for a main cursor,
// its sub-cursor), then stores the positions back, also if it fails (C
// cursors keep the position a failed search left them at; mdb_cursor_put
// relies on that).
func (t *Txn) readOp(c cref, f func(mc, mx *rcursor) error) error {
	mc, mx := &t.w.rc, &t.w.rx
	hasX, err := t.readCursors(c, mc, mx)
	if err != nil {
		return err
	}
	var x *rcursor
	if hasX {
		x = mx
	}
	err = f(mc, x)
	t.writeBack(c, mc, mx, hasX)
	return err
}

// descend is mdb_page_search and mdb_page_search_root on the stack of the
// unpositioned write cursor w itself (see seeksInPlace): put the root of
// the tree db (not P_INVALID) on the stack and descend to the leaf for key
// (nil: the last leaf). Returns the leaf. The steps, state and errors are
// those of rcursor.pageSearch for an unpositioned cursor.
func (t *Txn) descend(w *wcursor, db *format.DB, cmp format.Comparator, key []byte, last bool) (format.Page, error) {
	mp, slot, err := t.pageAndSlot(db.Root, w.pgSlot[0])
	if err != nil {
		return mp, err
	}
	w.pg[0] = mp.Pgno()
	w.pgSlot[0] = slot
	w.snum = 1
	w.top = 0
	for mp.IsBranch() {
		nkeys := mp.NumKeys()
		if nkeys == 0 {
			return mp, format.Corrupted
		}
		var i int
		if last {
			i = nkeys - 1
		} else {
			idx, found, exact, err := searchPage(mp, cmp, int(db.Pad), key)
			if err != nil {
				return mp, err
			}
			w.ki[w.top] = uint16(idx)
			i = w.kiTop()
			switch {
			case !found:
				i = nkeys - 1
			case !exact:
				if i == 0 {
					return mp, format.Corrupted
				}
				i--
			}
		}
		node, ok := mp.Node(i)
		if !ok {
			return mp, format.Corrupted
		}
		level := w.snum
		hint := noSlot
		if level < format.CursorStack {
			hint = w.pgSlot[level]
		}
		child, cslot, err := t.pageAndSlot(node.ChildPgno(), hint)
		if err != nil {
			return mp, err
		}
		w.ki[w.top] = uint16(i)
		if level >= format.CursorStack {
			return mp, format.CursorFull
		}
		w.pg[level] = child.Pgno()
		w.pgSlot[level] = cslot
		w.ki[level] = 0
		w.top = level
		w.snum = level + 1
		mp = child
	}
	if !mp.IsLeaf() {
		return mp, format.Corrupted
	}
	w.flags |= cInitialized
	w.flags &^= cEOF
	return mp, nil
}

// readItem is the key of item ki of leaf mp of a tree without duplicates,
// read as readPosition and cursorSet read it for a main cursor without a
// sub-cursor (a node with duplicates is corrupt there; the data is read
// too, following F_BIGDATA, for its checks).
func (t *Txn) readItem(mp format.Page, ki, ksize int) ([]byte, error) {
	if !mp.IsLeaf() {
		return nil, format.Corrupted
	}
	if mp.IsLeaf2() {
		k, ok := mp.Leaf2Key(ki, ksize)
		if !ok {
			return nil, format.Corrupted
		}
		return k, nil
	}
	leaf, ok := mp.Node(ki)
	if !ok || leaf.IsDupData() {
		return nil, format.Corrupted
	}
	if _, err := nodeRead(t, leaf); err != nil {
		return nil, err
	}
	k, ok := leaf.Key()
	if !ok {
		return nil, format.Corrupted
	}
	return k, nil
}

// --- cursor state -------------------------------------------------------------

func (t *Txn) cur(c cref) *wcursor { return t.w.cursors.cur(c) }

// dbOf is *mc->mc_db.
func (t *Txn) dbOf(c cref) *format.DB {
	w := t.cur(c)
	if !w.db.sub {
		return &t.dbe(w.db.i).db
	}
	return &t.w.cursors.slots[w.db.i].x.db
}

// cmpOf is mc->mc_dbx->md_cmp.
func (t *Txn) cmpOf(c cref) format.Comparator {
	w := t.cur(c)
	if !w.db.sub {
		return t.dbe(w.db.i).keyCmp
	}
	return t.w.cursors.slots[w.db.i].x.cmp
}

// dbflag is *mc->mc_dbflag.
func (t *Txn) dbflag(c cref) uint8 {
	if c.sub {
		if s := t.w.cursors.slot(c.slot); s != nil && s.hasX {
			return s.x.dbflag
		}
		return 0
	}
	return t.dbe(t.cur(c).dbi).state
}

// setDBFlag is *mc->mc_dbflag |= bits for a main cursor.
func (t *Txn) setDBFlag(c cref, bits uint8) {
	if !c.sub {
		t.dbe(t.cur(c).dbi).state |= bits
	}
}

// openSlot is mdb_cursor_init: a new cursor slot on dbi, tracked if
// tracked. The DBI's record must be fresh.
func (t *Txn) openSlot(dbi int, tracked bool) int {
	e := t.dbe(dbi)
	return t.w.cursors.open(dbi, e.dupOrder(), uint32(e.db.Flags)&format.DupSort != 0, tracked)
}

func (t *Txn) closeSlot(slot int) { t.w.cursors.release(slot) }

// copyCursor is mdb_cursor_copy(mc, &mn) with mn.mc_xcursor = NULL: a
// temporary copy of mc, tracked if tracked (WITH_CURSOR_TRACKING). A copy
// of a sub-cursor is reached through a dummy main cursor, as in C.
func (t *Txn) copyCursor(mc cref, tracked bool) cref {
	cp := *t.cur(mc)
	if cp.isSub() {
		dummy := newWCursor(cp.dbi, dbRef{i: cp.dbi}, cInitialized)
		x := xstate{c: cp, db: format.EmptyDB(0), cmp: format.Memn}
		return subRef(t.w.cursors.alloc(dummy, &x, tracked))
	}
	return mainRef(t.w.cursors.alloc(cp, nil, tracked))
}

// copyState copies the state of src into dst (mdb_cursor_copy(src, dst)).
func (t *Txn) copyState(src, dst cref) {
	s := *t.cur(src)
	d := t.cur(dst)
	d.dbi = s.dbi
	d.db = s.db
	d.snum = s.snum
	d.top = s.top
	d.flags = s.flags
	d.subp = s.subp
	copy(d.pg[:s.snum], s.pg[:s.snum])
	copy(d.pgSlot[:s.snum], s.pgSlot[:s.snum])
	copy(d.ki[:s.snum], s.ki[:s.snum])
}

// --- pages ----------------------------------------------------------------------

// pageIDOf is the identity of the page of c at level (mc->mc_pg[level]).
func (t *Txn) pageIDOf(c cref, level int) pageID {
	w := t.cur(c)
	if level == 0 && w.subp {
		if s := t.w.cursors.slot(c.slot); s != nil && s.main.snum > 0 {
			return pageID{sub: true, leaf: s.main.pg[s.main.top], idx: s.main.ki[s.main.top]}
		}
		return pageID{sub: true, leaf: format.PInvalid}
	}
	return pageID{pg: w.pg[level]}
}

// pageOfLevel is the page of c at level, for reading.
func (t *Txn) pageOfLevel(c cref, level int) (format.Page, error) {
	w := t.cur(c)
	if level == 0 && w.subp {
		sp, err := t.subpageOf(c.slot)
		if err != nil {
			return format.Page{}, err
		}
		if sp == nil {
			return format.Page{}, format.Corrupted
		}
		return format.Page{B: sp, L: t.l}, nil
	}
	return t.pageBySlot(w.pg[level], w.pgSlot[level])
}

// topPage is the top page of c (mc->mc_pg[mc->mc_top]), for reading.
func (t *Txn) topPage(c cref) (format.Page, error) { return t.pageOfLevel(c, t.cur(c).top) }

// pageMutOf is the bytes of the page of c at level, for writing. The page
// must be dirty; a sub-page is written inside its dirty leaf. The slot of
// the page is stored back into the cursor.
func (t *Txn) pageMutOf(c cref, level int) ([]byte, error) {
	ps := t.pageSize
	w := t.cur(c)
	if level == 0 && w.subp {
		s := t.w.cursors.slot(c.slot)
		if s == nil {
			return nil, format.Corrupted
		}
		m := &s.main
		leaf, idx := m.pg[m.top], int(m.ki[m.top])
		slot, ok := t.w.dirty.resolve(m.pgSlot[m.top], leaf)
		if !ok {
			return nil, format.Corrupted
		}
		m.pgSlot[m.top] = slot
		buf := t.w.dirty.at(slot, leaf)[:ps]
		start, end, ok := nodeDataRange(t.l, buf, idx)
		if !ok {
			return nil, format.Corrupted
		}
		return buf[start:end:end], nil
	}
	pgno := w.pg[level]
	slot, ok := t.w.dirty.resolve(w.pgSlot[level], pgno)
	if !ok {
		return nil, format.Corrupted
	}
	w.pgSlot[level] = slot
	return t.w.dirty.at(slot, pgno)[:ps:ps], nil
}

// topPageMut is the top page of c, for writing.
func (t *Txn) topPageMut(c cref) ([]byte, error) { return t.pageMutOf(c, t.cur(c).top) }

// dirtyMut is a dirty page by number, for writing (psize bytes, or the
// whole run).
func (t *Txn) dirtyMut(pgno uint64) ([]byte, error) {
	b := t.w.dirty.get(pgno)
	if b == nil {
		return nil, format.Corrupted
	}
	return b, nil
}

// --- positioning on a write cursor's own stack -----------------------------------

// seeksInPlace reports whether mc can be positioned on its own stack: an
// unpositioned main cursor of a tree without duplicates (the cursor
// mdb_put and mdb_del open for every call).
func (t *Txn) seeksInPlace(mc cref) bool {
	s := &t.w.cursors.slots[mc.slot]
	return !mc.sub && !s.hasX && !s.main.isInit() && !s.main.db.sub
}

// seekKey is mdb_cursor_set with MDB_SET for a cursor that seeksInPlace,
// in a tree db with key order cmp: what cursorSet(.., Set, true) through
// readOp does for such a cursor, step for step, with the same errors and
// state. Returns false where that returns MDB_NOTFOUND (the cursor then
// sits where key would go).
func (t *Txn) seekKey(mc cref, db *format.DB, cmp format.Comparator, key []byte) (bool, error) {
	if len(key) == 0 {
		return false, format.BadValSize
	}
	if db.Root == format.PInvalid {
		return false, nil
	}
	w := t.cur(mc)
	mp, err := t.descend(w, db, cmp, key, false)
	if err != nil {
		return false, err
	}
	ksize := int(db.Pad)
	i, _, exact, err := searchPage(mp, cmp, ksize, key)
	if err != nil {
		return false, err
	}
	w.ki[w.top] = uint16(i)
	if !exact {
		return false, nil
	}
	// set1: read the item as cursorSet does, for its checks.
	if _, err := t.readItem(mp, w.kiTop(), ksize); err != nil {
		return false, err
	}
	return true, nil
}

// seekLast is mdb_cursor_last as mdb_cursor_put uses it for MDB_APPEND,
// for a cursor that seeksInPlace: what cursorLastForAppend through readOp
// does. Returns the last key, or ok false for an empty leaf (the cursor is
// then before its first slot, mc_ki 0xffff).
func (t *Txn) seekLast(mc cref, db *format.DB, cmp format.Comparator) ([]byte, bool, error) {
	if db.Root == format.PInvalid {
		return nil, false, format.NotFound
	}
	w := t.cur(mc)
	mp, err := t.descend(w, db, cmp, nil, true)
	if err != nil {
		return nil, false, err
	}
	nkeys := mp.NumKeys()
	if nkeys == 0 {
		if !mp.IsLeaf() {
			return nil, false, format.Corrupted
		}
		w.ki[w.top] = 0xffff
		w.flags |= cInitialized | cEOF
		return nil, false, nil
	}
	w.ki[w.top] = uint16(nkeys - 1)
	w.flags |= cInitialized | cEOF
	k, err := t.readItem(mp, w.kiTop(), int(db.Pad))
	if err != nil {
		return nil, false, err
	}
	return k, true, nil
}

// pageSearchModify is mdb_page_search with MDB_PS_MODIFY: descend to the
// leaf for key (or the first/last leaf with psFirst/psLast) and touch
// every page on the way, root first.
func (t *Txn) pageSearchModify(mc cref, key []byte, flags uint8) error {
	if t.w.flags&txnBlocked != 0 {
		return format.BadTxn
	}
	if err := t.readOp(mc, func(c, _ *rcursor) error { return c.pageSearch(t, key, flags) }); err != nil {
		return err
	}
	snum := t.cur(mc).snum
	for level := 0; level < snum; level++ {
		t.cur(mc).top = level
		if err := t.pageTouch(mc); err != nil {
			return err
		}
	}
	t.cur(mc).top = max(snum-1, 0)
	return nil
}

// --- touching -----------------------------------------------------------------

// pageTouch is mdb_page_touch (mdb.c line 2494): make the top page of mc
// writable. A clean page is copied to a newly allocated page number, the
// old number goes on the free list, the parent branch node (or the DB
// root) is pointed at the copy, and every other cursor on the old page
// moves to the copy. Sub-pages live inside a dirty leaf, so touching them
// is a no-op. A page this transaction (or an ancestor) spilled is copied
// back under its own number (mdb_page_unspill); in a nested transaction a
// page dirty in an ancestor is copied into this transaction's list under
// its own number.
func (t *Txn) pageTouch(mc cref) error {
	top := t.cur(mc).top
	inList, needed := t.touchNeeded(mc, top)
	if !needed {
		return nil
	}
	return t.pageTouchSlow(mc, inList)
}

// touchNeeded is the common case of mdb_page_touch for the page of mc at
// level: needed false if it is a sub-page or already dirty in this
// transaction's own list (its slot is then stored); otherwise whether the
// page is in that list at all.
func (t *Txn) touchNeeded(mc cref, level int) (inList, needed bool) {
	w := t.cur(mc)
	if level == 0 && w.subp {
		return false, false
	}
	slot, page := t.w.dirty.lookup(w.pgSlot[level], w.pg[level])
	if page != nil && pFlags(t.l, page)&format.PDirty != 0 {
		w.pgSlot[level] = slot
		return false, false
	}
	return page != nil, true
}

// pageTouchSlow is pageTouch for a page that is not dirty in this
// transaction's own list yet.
func (t *Txn) pageTouchSlow(mc cref, inList bool) error {
	w := t.cur(mc)
	if w.top == 0 && w.subp {
		return nil
	}
	top, mp, snum, isSub := w.top, w.pg[w.top], w.snum, w.isSub()
	var page format.Page
	var err error
	if inList {
		page, err = t.page(mp)
	} else {
		page, err = t.pageNotDirtyHere(mp)
	}
	if err != nil {
		return err
	}
	if page.Flags()&format.PDirty != 0 {
		if t.hasParent() && !inList {
			// If txn has a parent, make sure the page is in our dirty
			// list. No - copy it (it is already counted in the parent's
			// dirty room).
			slot, err := t.copyAncestorPage(mp)
			if err != nil {
				return err
			}
			t.cur(mc).pgSlot[top] = slot
		}
		return nil
	}
	if t.w.flags&txnSpills != 0 {
		done, err := t.pageUnspill(mp)
		if err != nil {
			t.w.flags |= txnError
			return err
		}
		if done {
			return nil
		}
	}
	// The new page starts as a copy of the clean one (mdb_page_copy, done
	// by the allocation).
	src, err := t.cleanPage(mp)
	if err != nil {
		return err
	}
	np, npSlot, err := t.pageAllocCopy(src)
	if err != nil {
		t.w.flags |= txnError
		return err
	}
	// A corrupt file can hand us the page we are copying (a free-DB record
	// naming a live page, or a root on the free list). C goes on and later
	// reuses the live page; refuse instead.
	if np == mp {
		t.w.flags |= txnError
		return format.Corrupted
	}
	t.w.freePgs = append(t.w.freePgs, mp)
	// Update the parent page, if any, to point to the new page.
	if top > 0 {
		pki := int(t.cur(mc).ki[top-1])
		parent, err := t.pageMutOf(mc, top-1)
		if err != nil {
			return err
		}
		if !setNodePgno(t.l, parent, pki, np) {
			return format.Corrupted
		}
	} else {
		t.dbOf(mc).Root = np
	}
	if err := t.ownCopiedPage(np, npSlot); err != nil {
		return err
	}
	// Adjust cursors pointing to mp.
	w = t.cur(mc)
	w.pg[top] = np
	w.pgSlot[top] = npSlot
	dbi := w.dbi
	for i := range t.w.cursors.slots {
		m3, ok := t.w.cursors.peer(i, dbi, isSub)
		if !ok || (!isSub && m3 == mc) {
			continue
		}
		w3 := t.cur(m3)
		if w3.snum < snum {
			continue
		}
		if w3.pg[top] == mp && !(top == 0 && w3.subp) {
			w3.pg[top] = np
			w3.pgSlot[top] = npSlot
		}
	}
	return nil
}

// copyAncestorPage is the nested branch of mdb_page_touch: copy the page
// (or overflow run) an ancestor has dirty into this transaction's dirty
// list under the same number. Returns its slot.
func (t *Txn) copyAncestorPage(pgno uint64) (pageSlot, error) {
	src, level := t.locate(pgno)
	if src == nil || level < 2 {
		return noSlot, format.Corrupted
	}
	buf := t.w.dirty.allocCopy(src)
	s, ok := t.w.dirty.insert(pgno, buf)
	if !ok {
		return noSlot, format.Corrupted
	}
	return s, nil
}

// ownCopiedPage is the tail of mdb_page_touch for the new dirty page dst
// (in slot), which holds a copy of the clean page: np->mp_pgno = pgno;
// np->mp_flags |= P_DIRTY.
func (t *Txn) ownCopiedPage(dst uint64, slot pageSlot) error {
	to := t.w.dirty.at(slot, dst)
	if to == nil {
		return format.Corrupted
	}
	setPgno(to, dst)
	setFlags(t.l, to, pFlags(t.l, to)|format.PDirty)
	return nil
}

// cursorTouch is mdb_cursor_touch (mdb.c line 6650): touch every page on
// the cursor's stack, root first. For a named DB not yet written in this
// transaction, first touch the path to its record in the main DB (so the
// record can be updated at commit) and mark it DB_DIRTY.
func (t *Txn) cursorTouch(mc cref) error {
	dbi := t.cur(mc).dbi
	if dbi >= format.CoreDBs && t.dbflag(mc)&(dbDirty|dbDupData) == 0 {
		// Touch DB record of named DB.
		e := t.dbe(dbi)
		if e.seq != t.env.dbis.seqOf(dbi) {
			return format.BadDBI
		}
		name := t.env.dbis.nameOf(dbi)
		if name == nil {
			return format.BadDBI
		}
		slot := t.openSlot(format.MainDBI, false)
		err := t.pageSearchModify(mainRef(slot), name, 0)
		t.closeSlot(slot)
		if err != nil {
			return err
		}
		t.setDBFlag(mc, dbDirty)
	}
	snum := t.cur(mc).snum
	t.cur(mc).top = 0
	if snum > 0 {
		// Most of the time every page on the stack is already dirty in
		// this transaction's list and the cursor's slots say where.
		first := snum
		w := t.cur(mc)
		for level := 0; level < min(snum, format.CursorStack); level++ {
			if level == 0 && w.subp {
				continue
			}
			p := t.w.dirty.at(w.pgSlot[level], w.pg[level])
			if p == nil || pFlags(t.l, p)&format.PDirty == 0 {
				first = level
				break
			}
		}
		var err error
		for level := first; level < snum; level++ {
			inList, needed := t.touchNeeded(mc, level)
			if !needed {
				continue
			}
			t.cur(mc).top = level
			if err = t.pageTouchSlow(mc, inList); err != nil {
				break
			}
		}
		t.cur(mc).top = snum - 1
		if err != nil {
			return err
		}
	}
	return nil
}

// subDB is the record of the duplicates in node: the MDB_db of a sub-DB,
// or one describing a sub-page, as mdb_xcursor_init1 builds it.
func subDB(node format.Node, parentFlags uint32) (format.DB, bool, error) {
	data, ok := node.InlineData()
	if !ok {
		return format.DB{}, false, format.Corrupted
	}
	if node.IsSubData() {
		db, ok := format.ReadDB(data)
		if !ok {
			return db, false, format.Corrupted
		}
		return db, false, nil
	}
	// A sub-page has the layout of the page it is stored in.
	fp, ok := format.NewPage(data, node.L)
	if !ok {
		return format.DB{}, false, format.Corrupted
	}
	db := format.DB{Depth: 1, LeafPages: 1, Entries: uint64(fp.NumKeys()), Root: fp.Pgno()}
	if parentFlags&format.DupFixed != 0 {
		db.Flags = uint16(format.DupFixed)
		db.Pad = uint32(fp.Pad())
		if parentFlags&format.IntegerDup != 0 {
			db.Flags |= uint16(format.IntegerKey)
		}
	}
	return db, true, nil
}

// xcursorInit1 is mdb_xcursor_init1 (mdb.c line 7735) for the write path:
// point the sub-cursor of main cursor mc at the duplicates in its current
// node.
func (t *Txn) xcursorInit1(mc cref) error {
	parentFlags := uint32(t.dbOf(mc).Flags)
	leaf, err := t.topPage(mc)
	if err != nil {
		return err
	}
	node, ok := leaf.Node(t.cur(mc).kiTop())
	if !ok {
		return format.Corrupted
	}
	db, subpage, err := subDB(node, parentFlags)
	if err != nil {
		return err
	}
	s := t.w.cursors.slot(mc.slot)
	if s == nil || !s.hasX {
		return format.Corrupted
	}
	x := &s.x
	x.db = db
	x.c.snum, x.c.top = 0, 0
	x.c.flags = cSub
	x.c.subp = false
	if subpage {
		x.c.snum = 1
		x.c.flags = cInitialized | cSub
		x.c.pg[0] = subpageNo
		x.c.ki[0] = 0
		x.c.subp = true
	}
	x.dbflag = dbValid | dbUsrValid | dbDupData
	// #if UINT_MAX < SIZE_MAX: 8-byte integer duplicates.
	if x.cmp.IsInt() && x.db.Pad == 8 {
		x.cmp = format.Clong
	}
	return nil
}

// refreshXDB re-reads the duplicates' record of the node at main cursor mc
// into its sub-cursor (mx_db), keeping the sub-cursor's position. C trusts
// the cached copy, which goes stale when another cursor changes the same
// key's duplicates (KNOWN_C_BUGS.md #14).
func (t *Txn) refreshXDB(mc cref) error {
	parentFlags := uint32(t.dbOf(mc).Flags)
	leaf, err := t.topPage(mc)
	if err != nil {
		return err
	}
	node, ok := leaf.Node(t.cur(mc).kiTop())
	if !ok || !node.IsDupData() {
		return nil
	}
	db, _, err := subDB(node, parentFlags)
	if err != nil {
		return err
	}
	if s := t.w.cursors.slot(mc.slot); s != nil && s.hasX {
		s.x.db = db
	}
	return nil
}

// shareXDB: after the duplicates of mc's current key changed, give every
// other tracked cursor on the same key whose sub-cursor is set up the new
// record (C leaves their mx_db stale, KNOWN_C_BUGS.md #14).
func (t *Txn) shareXDB(mc cref) {
	w := t.cur(mc)
	dbi, top, pg, ki := w.dbi, w.top, w.pg[w.top], w.ki[w.top]
	s := t.w.cursors.slot(mc.slot)
	if s == nil || !s.hasX {
		return
	}
	db := s.x.db
	for i := range t.w.cursors.slots {
		if i == mc.slot {
			continue
		}
		m2, ok := t.w.cursors.peer(i, dbi, false)
		if !ok {
			continue
		}
		w2 := t.cur(m2)
		if !w2.isInit() || w2.top != top || w2.pg[top] != pg || w2.ki[top] != ki {
			continue
		}
		if s2 := t.w.cursors.slot(i); s2 != nil && s2.hasX && s2.x.c.isInit() {
			s2.x.db = db
		}
	}
}

// xcursorInit2 is mdb_xcursor_init2 (mdb.c line 7786): bring the
// sub-cursor of slot m2 up to date with src's after src changed the
// duplicates both cursors are on.
func (t *Txn) xcursorInit2(m2, src int, newDupData bool) {
	ss := t.w.cursors.slot(src)
	if ss == nil || !ss.hasX {
		return
	}
	sx := ss.x
	s := t.w.cursors.slot(m2)
	if s == nil || !s.hasX {
		return
	}
	mainFlags := s.main.flags
	x := &s.x
	if newDupData {
		x.c.snum = 1
		x.c.top = 0
		// On the key's first item now. C ORs in C_INITIALIZED, so a C_DEL
		// or C_EOF left from earlier makes the next MDB_NEXT return the
		// item again (KNOWN_C_BUGS.md #29); only a main cursor that sits
		// before the item (after a delete, C_DEL) must not skip it.
		x.c.flags = cSub | cInitialized | (mainFlags & cDel)
		x.c.ki[0] = 0
		x.dbflag = dbValid | dbUsrValid | dbDupData
		x.cmp = sx.cmp
	} else if !x.c.isInit() {
		return
	}
	x.db = sx.db
	x.c.pg[0] = sx.c.pg[0]
	x.c.pgSlot[0] = sx.c.pgSlot[0]
	x.c.subp = sx.c.subp
}
