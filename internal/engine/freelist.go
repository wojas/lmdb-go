// Page allocation and the free-page database: mdb_page_alloc (mdb.c line
// 2231), mdb_freelist_save (line 3169), mdb_page_loose (line 1934) and
// mdb_ovpage_free (line 5819) (rlmdb core/freelist.rs).
//
// A write transaction never overwrites a page a snapshot may still read.
// Pages it stops using go on its free list (mt_free_pgs), which commit
// stores in the free DB under the transaction's own id: txnid -> [count,
// pgno...] (descending). A later writer may reuse the pages of record T
// once every reader's snapshot is at least T (mdb_find_oldest). It reads
// such records in key order into me_pghead (wstate.pghead, descending)
// and remembers the last one it consumed in me_pglast; the records are
// only deleted when the free list is saved at commit. Pages allocated and
// freed in the same transaction are loose (P_LOOSE): still dirty, reused
// first.
//
// Writing the free DB allocates and frees pages itself, which changes the
// lists being written; mdb_freelist_save therefore loops until a pass
// makes no more changes, reserving record space first (MDB_RESERVE) and
// filling it in at the end. The port keeps C's exact sequence of
// operations, which determines which pages end up where.

package engine

import (
	"encoding/binary"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// txnidKey reads a free DB key (a native txnid_t).
func txnidKey(key []byte) (uint64, error) {
	v, ok := format.Get64(key, 0)
	if !ok {
		return 0, format.Corrupted
	}
	return v, nil
}

// txnidBytes writes the free DB key of txnid id into buf and returns it.
// The key reaches comparators through function values, so a local array
// would escape: callers pass a buffer of their wstate (reclaimRun's and
// freelistSave's are distinct: a put of freelistSave can call reclaimRun).
func txnidBytes(buf *[8]byte, id uint64) []byte {
	binary.NativeEndian.PutUint64(buf[:], id)
	return buf[:]
}

// How the buffer of a newly allocated page starts.
const (
	initZeroed = iota // zero-filled (a reused loose page keeps its contents, as in C)
	initCopy          // a copy of a whole page (mdb_page_touch)
	initData          // this data right after the header, zeros elsewhere (a new overflow run)
)

type pageInit struct {
	kind int
	src  []byte
}

// pageAlloc is mdb_page_alloc: allocate num contiguous pages and return
// the first page number. The buffer is in the dirty list, zero-filled (a
// reused loose page keeps its old contents, as in C). Order of preference
// as C: a loose page (single pages only); a run of num pages from pghead,
// preferring the tail, reading more free DB records when needed; new
// pages at the end of the file (MDB_MAP_FULL if the map is too small).
func (t *Txn) pageAlloc(num int) (uint64, error) {
	p, _, err := t.pageAllocWith(num, pageInit{})
	return p, err
}

// pageAllocCopy is pageAlloc of one page whose contents start as a copy
// of src (a whole page), for mdb_page_touch. Also returns the slot.
func (t *Txn) pageAllocCopy(src []byte) (uint64, pageSlot, error) {
	return t.pageAllocWith(1, pageInit{kind: initCopy, src: src})
}

// pageAllocWith is pageAlloc with the buffer starting as init says.
// Returns the page number and its slot (noSlot for a reused loose page not
// initialised here).
func (t *Txn) pageAllocWith(num int, init pageInit) (uint64, pageSlot, error) {
	w := t.w
	// If there are any loose pages, just use them.
	if num == 1 && len(w.loose) > 0 {
		pgno := w.loose[len(w.loose)-1]
		w.loose = w.loose[:len(w.loose)-1]
		var at int
		switch init.kind {
		case initZeroed:
			return pgno, noSlot, nil
		case initCopy:
			at = 0
		case initData:
			at = t.l.PageHdr
		}
		slot, ok := w.dirty.find(pgno)
		if !ok {
			return 0, noSlot, format.Corrupted
		}
		page := w.dirty.at(slot, pgno)
		if at+len(init.src) > len(page) {
			return 0, noSlot, format.Corrupted
		}
		if init.kind == initCopy && len(init.src) == len(page) {
			copyPage(t.l, page, init.src)
		} else {
			copy(page[at:], init.src)
		}
		return pgno, slot, nil
	}
	// If our dirty list is already full, we can't do anything.
	if w.dirtyRoom == 0 {
		w.flags |= txnError
		return 0, noSlot, format.TxnFull
	}
	i, found, err := t.reclaimRun(num)
	if err != nil {
		w.flags |= txnError
		return 0, noSlot, err
	}
	var pgno uint64
	if found {
		pgno = w.pghead.RemoveRun(i, num)
	} else {
		// Use new pages from the map when nothing suitable in the free DB.
		pgno = t.nextPgno
		if pgno+uint64(num) >= t.maxPgno() {
			w.flags |= txnError
			return 0, noSlot, format.MapFull
		}
		t.nextPgno = pgno + uint64(num)
	}
	n := t.pageSize * num
	var buf []byte
	switch init.kind {
	case initZeroed:
		buf = w.dirty.allocZeroed(n)
	case initCopy:
		buf = w.dirty.allocPageCopy(t.l, init.src)
	case initData:
		if buf = w.dirty.allocData(n, t.l.PageHdr, init.src); buf == nil {
			w.flags |= txnError
			return 0, noSlot, format.Corrupted
		}
	}
	setPgno(buf, pgno)
	// mdb_page_dirty
	slot, ok := w.dirty.insert(pgno, buf)
	if !ok {
		w.flags |= txnError
		return 0, noSlot, format.Corrupted
	}
	w.dirtyRoom--
	return pgno, slot, nil
}

// reclaimRun is the search loop of mdb_page_alloc: find num contiguous
// pages in pghead, merging in more free DB records as long as they are
// older than every reader. Returns the 1-based C index of the run's lowest
// page in pghead.
func (t *Txn) reclaimRun(num int) (int, bool, error) {
	w := t.w
	n2 := num - 1
	retry := num * 60
	free := t.dbe(format.FreeDBI)
	m2 := &w.reclaimCur
	m2.init(free.db, free.keyCmp, 0, t.l)
	op := format.First
	foundOld := false
	var oldest, last uint64
	mopLen := len(w.pghead)
	for {
		// Seek a big enough contiguous page range. Prefer pages at the
		// tail, just truncating the list.
		if mopLen > n2 {
			if i, ok := w.pghead.FindRun(num); ok {
				return i, true, nil
			}
			retry--
			if retry < 0 {
				break
			}
		}
		if op == format.First {
			// Prepare to fetch more and coalesce.
			last = w.pglast
			oldest = w.pgOldest
			if last != 0 {
				op = format.SetRange // will look up last+1
			}
		}
		last++
		// Do not fetch more if the record will be too recent.
		if oldest <= last {
			if !foundOld {
				oldest = t.findOldest()
				w.pgOldest = oldest
				foundOld = true
			}
			if oldest <= last {
				break
			}
		}
		var k, v []byte
		var err error
		switch op {
		case format.First:
			k, v, err = cursorFirst(t, m2, nil)
		case format.SetRange:
			key := txnidBytes(&w.reclaimKey, last)
			k, v, _, err = cursorSet(t, m2, nil, format.Comparator{}, false, key, nil, false, format.SetRange, false)
		default:
			k, v, err = cursorNext(t, m2, nil, format.Next)
		}
		op = format.Next
		if err == format.NotFound {
			break
		}
		if err != nil {
			return 0, false, err
		}
		if last, err = txnidKey(k); err != nil {
			return 0, false, err
		}
		if oldest <= last {
			if !foundOld {
				oldest = t.findOldest()
				w.pgOldest = oldest
				foundOld = true
			}
			if oldest <= last {
				break
			}
		}
		ids, ok := format.EncodedIDs(v)
		if !ok {
			return 0, false, format.Corrupted
		}
		// C trusts the record. A meta page or a page past the snapshot
		// would be handed out and overwritten.
		for j := 0; j < len(ids); j += 8 {
			p := binary.NativeEndian.Uint64(ids[j:])
			if p < format.NumMetas || p >= t.nextPgno {
				return 0, false, format.Corrupted
			}
		}
		w.pglast = last
		// Merge in descending sorted order (straight from the record).
		if w.pghead == nil {
			w.pghead = w.emptyPghead()
		}
		if !w.pghead.XMergeEncoded(v) {
			return 0, false, format.Corrupted
		}
		mopLen = len(w.pghead)
	}
	return 0, false, nil
}

// findOldest is mdb_find_oldest: the oldest snapshot any reader uses, or
// our txnid minus one if there is none older. Without a lock file the
// in-memory reader table answers (AGENTS.md rule 2a; C and rlmdb return
// txnid-1 there, leaving reader safety to the caller).
func (t *Txn) findOldest() uint64 { return t.env.lock.FindOldest(t.txnid) }

// pageLoose is mdb_page_loose: a page mc's tree no longer uses. If it is
// dirty (allocated in this transaction) and not a free DB page, keep it
// for reuse in this transaction; otherwise put it on the free list.
func (t *Txn) pageLoose(mc cref, pgno uint64) error {
	dirty := false
	if t.w.dirty.contains(pgno) {
		p, err := t.page(pgno)
		if err != nil {
			return err
		}
		dirty = p.Flags()&format.PDirty != 0
	}
	if dirty && t.cur(mc).dbi != format.FreeDBI {
		t.w.loose = append(t.w.loose, pgno)
		page, err := t.dirtyMut(pgno)
		if err != nil {
			return err
		}
		setFlags(t.l, page, pFlags(t.l, page)|format.PLoose)
	} else {
		t.w.freePgs = append(t.w.freePgs, pgno)
	}
	return nil
}

// overflowRunPages is the length of the overflow run at pgno (mp_pages),
// checked against the transaction's pages: a corrupt count must not make
// the free list grow by billions of entries.
func (t *Txn) overflowRunPages(pgno uint64) (int, error) {
	p, err := t.page(pgno)
	if err != nil {
		return 0, err
	}
	n := uint64(p.OverflowPages())
	if !p.IsOverflow() || n == 0 || pgno+n < pgno || pgno+n > t.nextPgno {
		return 0, format.Corrupted
	}
	return int(n), nil
}

// droppedRunPages is the length of the overflow run reference r points to,
// as mdb_drop0 frees it: 0.9 reads mp_pages from the run's first page; 1.0
// trusts op_pages. Either way the run must lie within the transaction's
// pages.
func (t *Txn) droppedRunPages(r format.OverflowRef) (int, error) {
	if t.l.Format == format.V09 {
		return t.overflowRunPages(r.Pgno)
	}
	if r.Pages == 0 || r.Pgno+r.Pages < r.Pgno || r.Pgno+r.Pages > t.nextPgno {
		return 0, format.Corrupted
	}
	return int(r.Pages), nil
}

// ovpageFree is mdb_ovpage_free: free the overflow run at pgno. A run
// allocated in this transaction (dirty, or spilled by it) goes straight
// back to pghead (if the free DB is in use); otherwise on the free list.
// Not in nested transactions: they would need to hide the run in their
// ancestors' lists.
func (t *Txn) ovpageFree(mc cref, pgno uint64) error {
	ovpages, err := t.overflowRunPages(pgno)
	if err != nil {
		return err
	}
	p, err := t.page(pgno)
	if err != nil {
		return err
	}
	dirty := p.Flags()&format.PDirty != 0
	spilled := !dirty && t.w.spilled.contains(pgno)
	if t.w.pghead != nil && !t.hasParent() && (dirty || spilled) {
		if spilled {
			// This page is no longer spilled.
			delete(t.w.spilled, pgno)
		} else {
			// Remove from dirty list.
			if !t.w.dirty.remove(pgno) {
				t.w.flags |= txnError
				return format.Corrupted
			}
			t.w.dirtyRoom++
		}
		// Insert in me_pghead, keeping it descending.
		t.w.pghead.InsertRun(pgno, ovpages)
	} else {
		t.w.freePgs.AppendRange(pgno, ovpages)
	}
	t.dbOf(mc).OverflowPages -= uint64(ovpages)
	return nil
}

// freelistSave is mdb_freelist_save: write the free list of this
// transaction and what is left of pghead into the free DB. Called by
// commit. The cursor is tracked (C uses an untracked one,
// KNOWN_C_BUGS.md #11).
func (t *Txn) freelistSave() error {
	slot := t.openSlot(format.FreeDBI, true)
	err := t.freelistSave0(mainRef(slot))
	t.closeSlot(slot)
	return err
}

func (t *Txn) freelistSave0(mc cref) error {
	w := t.w
	// pghead can grow and shrink during this call; pglast and freePgs can
	// only grow.
	maxfree1pg := t.l.MaxFree1Pg(t.pageSize)
	more := 1
	var pglast, headID uint64
	freecnt := 0
	headRoom, totalRoom := 0, 0
	txnid := t.txnid
	if w.pghead != nil {
		// Make sure first page of freeDB is touched and on freelist.
		if err := t.pageSearchModify(mc, nil, psFirst); err != nil && err != format.NotFound {
			return err
		}
	}
	if w.pghead == nil && len(w.loose) > 0 {
		// Put loose page numbers in freePgs, since we may be unable to
		// return them to pghead. They must also leave the dirty list.
		for i := len(w.loose) - 1; i >= 0; i-- {
			pgno := w.loose[i]
			w.freePgs = append(w.freePgs, pgno)
			w.dirty.remove(pgno)
		}
		w.loose = w.loose[:0]
	}
	for {
		// Come back here after each put in case the free list changed. If
		// using records from freeDB which we have not yet deleted, delete
		// them and any we reserved for pghead.
		for pglast < w.pglast {
			var key uint64
			err := t.readOp(mc, func(c, _ *rcursor) error {
				k, _, err := cursorFirst(t, c, nil)
				if err != nil {
					return err
				}
				key, err = txnidKey(k)
				return err
			})
			if err != nil {
				return err
			}
			pglast, headID = key, key
			totalRoom, headRoom = 0, 0
			if err := t.delAt(mc, 0); err != nil {
				return err
			}
		}
		// Save the IDL of pages freed by this txn, to a single record.
		if freecnt < len(w.freePgs) {
			if freecnt == 0 {
				// Make sure last page of freeDB is touched and on freelist.
				if err := t.pageSearchModify(mc, nil, psLast); err != nil && err != format.NotFound {
					return err
				}
			}
			key := txnidBytes(&w.saveKey, txnid)
			for {
				freecnt = len(w.freePgs)
				size := (freecnt + 1) * 8
				if _, err := t.putAt(mc, key, putReserve(size), format.Reserve); err != nil {
					return err
				}
				// Retry if freePgs grew during the put.
				if freecnt >= len(w.freePgs) {
					break
				}
			}
			w.freePgs.Sort()
			out, err := t.dataLocBytes(mc)
			if err != nil {
				return err
			}
			if !w.freePgs.EncodeInto(out) {
				return format.Corrupted
			}
			continue
		}
		mopLen := len(w.pghead) + len(w.loose)
		// Reserve records for pghead. Split it if multi-page, to avoid
		// searching freeDB for a page range. Use keys in range [1,
		// pglast]: smaller than txnid of oldest reader.
		if totalRoom >= mopLen {
			if totalRoom == mopLen {
				break
			}
			more--
			if more < 0 {
				break
			}
		} else if headRoom >= maxfree1pg && headID > 1 {
			// Keep current record (overflow page), add a new one.
			headID--
			headRoom = 0
		}
		// (Re)write {key = headID, IDL length = headRoom}.
		totalRoom -= headRoom
		headRoom = mopLen - totalRoom
		if headRoom > maxfree1pg && headID > 1 {
			// Overflow multi-page for part of pghead.
			headRoom /= int(headID) // amortize page sizes
			headRoom += maxfree1pg - headRoom%(maxfree1pg+1)
		} else if headRoom < 0 {
			// Rare case, not bothering to delete this record.
			headRoom = 0
		}
		size := (headRoom + 1) * 8
		if _, err := t.putAt(mc, txnidBytes(&w.saveKey, headID), putReserve(size), format.Reserve); err != nil {
			return err
		}
		// The IDL is initially empty.
		out, err := t.dataLocBytes(mc)
		if err != nil {
			return err
		}
		clear(out)
		totalRoom += headRoom
	}
	// Return loose page numbers to pghead, though usually none are left at
	// this point. The pages themselves remain in the dirty list.
	if len(w.loose) > 0 {
		loose := format.IDL(w.loose)
		w.loose = nil
		loose.Sort()
		if w.pghead == nil {
			w.pghead = w.emptyPghead()
		}
		w.pghead.XMerge(loose)
	}
	// Fill in the reserved pghead records.
	mop := append(w.mopBuf[:0], w.pghead...)
	w.mopBuf = mop
	mopLen := len(mop)
	if mopLen == 0 {
		return nil
	}
	var id uint64
	var dsize int
	rec := func(next bool) error {
		return t.readOp(mc, func(c, _ *rcursor) error {
			var k, v []byte
			var err error
			if next {
				k, v, err = cursorNext(t, c, nil, format.Next)
			} else {
				k, v, err = cursorFirst(t, c, nil)
			}
			if err != nil {
				return err
			}
			id, err = txnidKey(k)
			dsize = len(v)
			return err
		})
	}
	err := rec(false)
	for {
		if err != nil {
			return err
		}
		n := dsize/8 - 1
		if n < 0 || id > w.pglast {
			return format.Corrupted
		}
		n = min(n, mopLen)
		// The record holds the n lowest remaining page numbers.
		ids := w.idsBuf[:0]
		if need := 8 * (n + 1); cap(ids) < need {
			ids = make([]byte, need)
		}
		ids = ids[:8*(n+1)]
		w.idsBuf = ids
		format.IDL(mop[mopLen-n : mopLen]).EncodeInto(ids)
		if _, err := t.putAt(mc, txnidBytes(&w.saveKey, id), putBytes(ids), format.Current); err != nil {
			return err
		}
		mopLen -= n
		if mopLen == 0 {
			return nil
		}
		err = rec(true)
	}
}
