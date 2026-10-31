// The dirty pages of a write transaction (mt_u.dirty_list, an MDB_ID2L;
// rlmdb core/dirty.rs).
//
// C keeps malloc'ed page copies in a list sorted by page number and finds
// them with mdb_mid2l_search in mdb_page_get before falling back to the
// map. Here each dirty page (or overflow run) is a buffer of n * psize
// bytes, and the sorted index maps each page number to the slot holding
// its buffer. Txn.page consults it first, so every read sees the
// transaction's own changes.
//
// Slots: the buffers live in a slab that only grows while the transaction
// runs, with a free list of empty slots. A page keeps its slot for as long
// as it stays dirty; only the sorted index shifts. Write cursors remember
// the slot of each page on their stack (a pageSlot). A slot is only ever a
// hint: every access by slot names the page number it expects and checks
// it, falling back to the search on a mismatch, so stale hints cost a
// search and never give the wrong buffer. Slots are per list: a nested
// transaction has its own list, where a parent's slot simply misses.
//
// Pages written to the data file before commit (mdb_page_spill) leave the
// list and are recorded in the transaction's spill set (mt_spill_pgs);
// they are read back from the map.
//
// New buffers are always zeroed (as C's default !MDB_NOMEMINIT, plus the
// parts C leaves uninitialised), which only changes bytes no reader looks
// at.

package engine

import "sort"

// spillSet is mt_spill_pgs: page numbers this transaction wrote to the
// data file before commit.
type spillSet map[uint64]struct{}

func (s spillSet) contains(pgno uint64) bool {
	_, ok := s[pgno]
	return ok
}

// layer is the dirty and spilled pages of one ancestor of a nested
// transaction.
type layer struct {
	dirty   *dirtyPages
	spilled spillSet
}

// hints is the number of remembered positions in dirtyPages.
const hints = 16

// poolMaxBytes is the most memory a pagePool keeps between transactions.
const poolMaxBytes = 256 << 20

// pagePool keeps spare single-page buffers (C's me_dpages). Between write
// transactions it keeps as many as the last transaction used, up to
// poolMaxBytes. Overflow runs are not pooled.
type pagePool struct {
	pageSize int
	max      int
	free     [][]byte
	used     int
}

func newPagePool(pageSize int) pagePool {
	return pagePool{pageSize: pageSize, max: poolMaxBytes / max(pageSize, 1)}
}

func (p *pagePool) take(n int) []byte {
	if n != p.pageSize || p.pageSize == 0 {
		return nil
	}
	p.used++
	if k := len(p.free); k > 0 {
		b := p.free[k-1]
		p.free[k-1] = nil
		p.free = p.free[:k-1]
		return b
	}
	return nil
}

func (p *pagePool) put(b []byte) {
	if len(b) == p.pageSize && p.pageSize != 0 && len(p.free) < p.max {
		p.free = append(p.free, b)
	}
}

// merge adds the buffers of o (and takes its page size if p has none).
func (p *pagePool) merge(o *pagePool) {
	if p.pageSize == 0 {
		p.pageSize, p.max = o.pageSize, o.max
	}
	p.used += o.used
	for _, b := range o.free {
		p.put(b)
	}
	*o = pagePool{}
}

// trim keeps no more spare buffers than the transaction used.
func (p *pagePool) trim() {
	if len(p.free) > p.used {
		clear(p.free[p.used:])
		p.free = p.free[:p.used]
	}
	p.used = 0
}

// pageSlot is where a dirty page's buffer is in its list's slab; only a
// hint.
type pageSlot uint32

// noSlot is no slot known: accesses through it fall back to the search.
const noSlot pageSlot = ^pageSlot(0)

type dirtyEntry struct {
	pgno uint64
	buf  []byte // nil: vacant
}

type dirtyIdx struct {
	pgno uint64
	slot pageSlot
}

// dirtyPages is the dirty-page list of a write transaction.
type dirtyPages struct {
	// index is mt_u.dirty_list: page number to slot, ascending.
	index  []dirtyIdx
	slots  []dirtyEntry
	vacant []pageSlot
	// hint is where pages were last found, by pgno % hints.
	hint [hints]pageSlot
	pool pagePool
}

func (d *dirtyPages) len() int { return len(d.index) }

func (d *dirtyPages) isEmpty() bool { return len(d.index) == 0 }

// at is the buffer of slot if it holds page pgno.
func (d *dirtyPages) at(slot pageSlot, pgno uint64) []byte {
	if int(slot) < len(d.slots) {
		e := &d.slots[slot]
		if e.buf != nil && e.pgno == pgno {
			return e.buf
		}
	}
	return nil
}

// searchIdx is mdb_mid2l_search: the position of the first entry whose
// pgno is >= pgno.
func (d *dirtyPages) searchIdx(pgno uint64) int {
	idx := d.index
	return sort.Search(len(idx), func(i int) bool { return idx[i].pgno >= pgno })
}

// search finds the slot of pgno by binary search and remembers it.
func (d *dirtyPages) search(pgno uint64) (pageSlot, bool) {
	i := d.searchIdx(pgno)
	if i < len(d.index) && d.index[i].pgno == pgno {
		s := d.index[i].slot
		d.hint[pgno%hints] = s
		return s, true
	}
	return noSlot, false
}

// find is the slot of dirty page pgno.
func (d *dirtyPages) find(pgno uint64) (pageSlot, bool) {
	if h := d.hint[pgno%hints]; d.at(h, pgno) != nil {
		return h, true
	}
	return d.search(pgno)
}

// lookup is the slot and buffer of pgno: through hint if it still holds
// the page, else as find finds it.
func (d *dirtyPages) lookup(hint pageSlot, pgno uint64) (pageSlot, []byte) {
	if b := d.at(hint, pgno); b != nil {
		return hint, b
	}
	s, ok := d.find(pgno)
	if !ok {
		return noSlot, nil
	}
	return s, d.at(s, pgno)
}

// get is the buffer of dirty page pgno (the whole run for overflow
// pages), nil if not dirty.
func (d *dirtyPages) get(pgno uint64) []byte {
	if b := d.at(d.hint[pgno%hints], pgno); b != nil {
		return b
	}
	s, ok := d.search(pgno)
	if !ok {
		return nil
	}
	return d.at(s, pgno)
}

// resolve is the slot of pgno: hint if it still holds the page, else by
// search.
func (d *dirtyPages) resolve(hint pageSlot, pgno uint64) (pageSlot, bool) {
	if d.at(hint, pgno) != nil {
		return hint, true
	}
	return d.find(pgno)
}

func (d *dirtyPages) contains(pgno uint64) bool {
	_, ok := d.find(pgno)
	return ok
}

// allocZeroed is mdb_page_malloc for a new page: a zero-filled buffer of n
// bytes, recycled if possible. Hand it to insert.
func (d *dirtyPages) allocZeroed(n int) []byte {
	if b := d.pool.take(n); b != nil {
		clear(b)
		return b
	}
	return make([]byte, n)
}

// allocCopy is a buffer holding a copy of src, recycled if possible.
func (d *dirtyPages) allocCopy(src []byte) []byte {
	b := d.pool.take(len(src))
	if b == nil {
		b = make([]byte, len(src))
	}
	copy(b, src)
	return b
}

// allocData is a buffer of n bytes holding data at offset off and zeros
// elsewhere (a new overflow run); nil if data does not fit.
func (d *dirtyPages) allocData(n, off int, data []byte) []byte {
	if off < 0 || len(data) > n-off {
		return nil
	}
	b := d.allocZeroed(n)
	copy(b[off:], data)
	return b
}

// occupy puts buf into an empty slot for pgno (not yet indexed).
func (d *dirtyPages) occupy(pgno uint64, buf []byte) pageSlot {
	if k := len(d.vacant); k > 0 {
		s := d.vacant[k-1]
		d.vacant = d.vacant[:k-1]
		d.slots[s] = dirtyEntry{pgno, buf}
		return s
	}
	d.slots = append(d.slots, dirtyEntry{pgno, buf})
	return pageSlot(len(d.slots) - 1)
}

// vacate empties slot s, recycling its buffer.
func (d *dirtyPages) vacate(s pageSlot) {
	if b := d.slots[s].buf; b != nil {
		d.pool.put(b)
		d.slots[s].buf = nil
		d.vacant = append(d.vacant, s)
	}
}

// insert is mdb_page_dirty: add a page buffer and return its slot; ok is
// false (and the buffer recycled) if the page is already dirty.
func (d *dirtyPages) insert(pgno uint64, buf []byte) (pageSlot, bool) {
	i := d.searchIdx(pgno)
	if i < len(d.index) && d.index[i].pgno == pgno {
		d.pool.put(buf)
		return noSlot, false
	}
	s := d.occupy(pgno, buf)
	d.index = append(d.index, dirtyIdx{})
	copy(d.index[i+1:], d.index[i:])
	d.index[i] = dirtyIdx{pgno, s}
	d.hint[pgno%hints] = s
	return s, true
}

// remove removes a page (its buffer is recycled); false if not there.
func (d *dirtyPages) remove(pgno uint64) bool {
	i := d.searchIdx(pgno)
	if i >= len(d.index) || d.index[i].pgno != pgno {
		return false
	}
	d.vacate(d.index[i].slot)
	d.index = append(d.index[:i], d.index[i+1:]...)
	return true
}

// clear is mdb_dlist_free: removes all pages (buffers recycled).
func (d *dirtyPages) clear() {
	for _, e := range d.index {
		if b := d.slots[e.slot].buf; b != nil {
			d.pool.put(b)
		}
	}
	d.index = d.index[:0]
	clear(d.slots)
	d.slots = d.slots[:0]
	d.vacant = d.vacant[:0]
	d.hint = [hints]pageSlot{}
	for i := range d.hint {
		d.hint[i] = noSlot
	}
}

// recycle empties the list for the next write transaction and trims the
// pool to what this one used.
func (d *dirtyPages) recycle() {
	d.clear()
	d.pool.trim()
}

// forEach calls f with every page (number, slot, buffer) in page order.
func (d *dirtyPages) forEach(f func(pgno uint64, slot pageSlot, buf []byte)) {
	for _, e := range d.index {
		if b := d.slots[e.slot].buf; b != nil {
			f(e.pgno, e.slot, b)
		}
	}
}

// retain keeps the pages for which keep returns true (one pass, in page
// order); the others' buffers are recycled.
func (d *dirtyPages) retain(keep func(pgno uint64, buf []byte) bool) {
	out := d.index[:0]
	for _, e := range d.index {
		b := d.slots[e.slot].buf
		if b == nil {
			continue
		}
		if keep(e.pgno, b) {
			out = append(out, e)
			continue
		}
		d.vacate(e.slot)
	}
	d.index = out
}

// merge merges o (a committing child's list) in: its buffers replace ours
// for the same page, in the slot that page has here (ours are recycled),
// its other pages get slots here, and its spare buffers join ours.
func (d *dirtyPages) merge(o *dirtyPages) {
	d.pool.merge(&o.pool)
	if len(o.index) == 0 {
		return
	}
	mine := d.index
	merged := make([]dirtyIdx, 0, len(mine)+len(o.index))
	j := 0
	for _, e := range o.index {
		for j < len(mine) && mine[j].pgno < e.pgno {
			merged = append(merged, mine[j])
			j++
		}
		buf := o.slots[e.slot].buf
		if buf == nil {
			continue
		}
		var s pageSlot
		if j < len(mine) && mine[j].pgno == e.pgno {
			s = mine[j].slot
			if old := d.slots[s].buf; old != nil {
				d.pool.put(old)
			}
			d.slots[s].buf = buf
			j++
		} else {
			s = d.occupy(e.pgno, buf)
		}
		merged = append(merged, dirtyIdx{e.pgno, s})
	}
	merged = append(merged, mine[j:]...)
	d.index = merged
	*o = dirtyPages{}
}

// newDirtyPages is an empty list with a pool for pages of pageSize bytes.
func newDirtyPages(pageSize int) *dirtyPages {
	d := &dirtyPages{pool: newPagePool(pageSize)}
	for i := range d.hint {
		d.hint[i] = noSlot
	}
	return d
}
