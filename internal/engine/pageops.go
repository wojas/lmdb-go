// In-place edits of a page buffer: the byte-shuffling halves of
// mdb_node_add, mdb_node_del, mdb_node_shrink and mdb_update_key (rlmdb
// core/page_ops.rs).
//
// Everything works on one byte slice that is exactly a page (or a P_SUBP
// sub-page inside a leaf node: offsets are relative to its start, so the
// same code applies). The functions mirror C's byte layout exactly,
// because C readers parse these pages. They return ok false when the page
// is malformed (an offset or count outside the buffer) instead of
// panicking; callers turn that into MDB_CORRUPTED.
//
// Offsets passed in and out (lower, upper, ptr, node offsets) are from the
// start of the buffer; the layout's PAGEBASE is added when a stored
// mp_lower, mp_upper or mp_ptrs[i] is read and subtracted when one is
// written, so the algorithms are C's with PAGEBASE folded in.

package engine

import (
	"github.com/PowerDNS/lmdb-go/internal/format"
)

func hdr16(p []byte, off int) uint16 {
	v, _ := format.Get16(p, off)
	return v
}

// pLower is mp_lower + PAGEBASE: the end of the pointer array.
func pLower(l *format.Layout, p []byte) int { return int(hdr16(p, l.LowerOffset())) + l.PtrBase }

// pUpper is mp_upper + PAGEBASE: the start of the node area.
func pUpper(l *format.Layout, p []byte) int { return int(hdr16(p, l.UpperOffset())) + l.PtrBase }

func pFlags(l *format.Layout, p []byte) uint16 { return hdr16(p, l.FlagsOffset()) }

func pPad(l *format.Layout, p []byte) uint16 { return hdr16(p, l.PadOffset()) }

func setLower(l *format.Layout, p []byte, v int) {
	format.Put16(p, l.LowerOffset(), uint16(v-l.PtrBase))
}

func setUpper(l *format.Layout, p []byte, v int) {
	format.Put16(p, l.UpperOffset(), uint16(v-l.PtrBase))
}

func setFlags(l *format.Layout, p []byte, v uint16) { format.Put16(p, l.FlagsOffset(), v) }

func setPad(l *format.Layout, p []byte, v uint16) { format.Put16(p, l.PadOffset(), v) }

func setOverflowPages(l *format.Layout, p []byte, pages uint32) {
	format.Put32(p, l.PagesOffset(), pages)
}

func overflowPages(l *format.Layout, p []byte) (uint32, bool) {
	return format.Get32(p, l.PagesOffset())
}

func setPgno(p []byte, pgno uint64) { format.Put64(p, 0, pgno) }

func pgnoOf(p []byte) uint64 {
	v, _ := format.Get64(p, 0)
	return v
}

func numKeys(l *format.Layout, p []byte) int { return l.NumKeys(hdr16(p, l.LowerOffset())) }

func sizeLeft(l *format.Layout, p []byte) int { return max(pUpper(l, p)-pLower(l, p), 0) }

// ptrAt is mp_ptrs[i] + PAGEBASE: the offset of node i.
func ptrAt(l *format.Layout, p []byte, i int) (int, bool) {
	v, ok := format.Get16(p, l.PageHdr+2*i)
	return int(v) + l.PtrBase, ok
}

// setPtr sets mp_ptrs[i] to v - PAGEBASE.
func setPtr(l *format.Layout, p []byte, i, v int) bool {
	off := l.PageHdr + 2*i
	if v < l.PtrBase || off+2 > len(p) {
		return false
	}
	format.Put16(p, off, uint16(v-l.PtrBase))
	return true
}

// nodeHdr is the header of the node at off: key size, flags, data size.
func nodeHdr(p []byte, off int) (ksize int, flags uint16, dsize int, ok bool) {
	if off < 0 || off > len(p) {
		return 0, 0, 0, false
	}
	n, ok := format.NewNode(p[off:], nil)
	if !ok {
		return 0, 0, 0, false
	}
	return n.KeySize(), n.Flags(), n.DataSize(), true
}

// nodeDataRange is the byte range [start, end) of the inline data of leaf
// node i (NODEDATA, NODEDSZ bytes).
func nodeDataRange(l *format.Layout, p []byte, i int) (int, int, bool) {
	if i >= numKeys(l, p) {
		return 0, 0, false
	}
	off, ok := ptrAt(l, p, i)
	if !ok {
		return 0, 0, false
	}
	ksize, _, dsize, ok := nodeHdr(p, off)
	if !ok {
		return 0, 0, false
	}
	start := off + l.NodeDataOffset(ksize)
	end := start + dsize
	if end > len(p) {
		return 0, 0, false
	}
	return start, end, true
}

// setNodeOverflowRef replaces the overflow reference of F_BIGDATA leaf
// node i with r.
func setNodeOverflowRef(l *format.Layout, p []byte, i int, r format.OverflowRef) bool {
	if i >= numKeys(l, p) {
		return false
	}
	off, ok := ptrAt(l, p, i)
	if !ok {
		return false
	}
	ksize, _, _, ok := nodeHdr(p, off)
	if !ok {
		return false
	}
	d := off + l.NodeDataOffset(ksize)
	if d > len(p) {
		return false
	}
	return l.WriteOverflowRef(p[d:], r)
}

// putLoHi writes mn_lo, mn_hi (and for a branch node mn_flags) of the node
// at off.
func putLoHi(p []byte, off int, word uint32) {
	lo, hi := nodeLoHiOffsets()
	format.Put16(p, off+lo, uint16(word))
	format.Put16(p, off+hi, uint16(word>>16))
}

var loOff, hiOff = func() (int, int) {
	var b [8]byte
	format.Put16(b[:], 0, 1)
	n, _ := format.NewNode(b[:], nil)
	if n.Lo() == 1 {
		return 0, 2
	}
	return 2, 0
}()

func nodeLoHiOffsets() (int, int) { return loOff, hiOff }

const (
	nodeFlagsOff = 4
	nodeKSizeOff = 6
)

// setNodeDSize is SETDSZ for leaf node i.
func setNodeDSize(l *format.Layout, p []byte, i, size int) bool {
	off, ok := ptrAt(l, p, i)
	if !ok {
		return false
	}
	if _, _, _, ok := nodeHdr(p, off); !ok {
		return false
	}
	putLoHi(p, off, uint32(size))
	return true
}

// setNodePgno is SETPGNO: point branch node i at pgno.
func setNodePgno(l *format.Layout, p []byte, i int, pgno uint64) bool {
	if i >= numKeys(l, p) {
		return false
	}
	off, ok := ptrAt(l, p, i)
	if !ok {
		return false
	}
	if _, _, _, ok := nodeHdr(p, off); !ok {
		return false
	}
	putLoHi(p, off, uint32(pgno))
	format.Put16(p, off+nodeFlagsOff, uint16(pgno>>32))
	return true
}

// nodeBody is what goes into a new node besides its key.
type nodeBody struct {
	kind   int // bodyBranch, bodyInline, bodyReserve, bodyOverflow
	pgno   uint64
	inline []byte
	size   int
	ovref  format.OverflowRef
}

const (
	bodyBranch = iota
	bodyInline
	bodyReserve
	bodyOverflow
)

// insertNode is the update: part of mdb_node_add: make room in the
// pointer array at indx, carve nodeSize bytes (already EVEN) off the top
// of the free space and write header, key and body there. The caller has
// checked that the node fits. Returns the node's offset in the page.
func insertNode(l *format.Layout, p []byte, indx, nodeSize int, key []byte, flags uint16, body nodeBody) (int, bool) {
	nkeys := numKeys(l, p)
	if indx > nkeys {
		return 0, false
	}
	ptrs := l.PageHdr
	if ptrs+2*(nkeys+1) > len(p) {
		return 0, false
	}
	copy(p[ptrs+2*(indx+1):ptrs+2*(nkeys+1)], p[ptrs+2*indx:ptrs+2*nkeys])
	ofs := pUpper(l, p) - nodeSize
	if ofs < pLower(l, p)+2 || ofs+nodeSize > len(p) {
		return 0, false
	}
	setPtr(l, p, indx, ofs)
	setUpper(l, p, ofs)
	setLower(l, p, pLower(l, p)+2)
	format.Put16(p, ofs+nodeKSizeOff, uint16(len(key)))
	var word uint32
	switch body.kind {
	case bodyBranch:
		word = uint32(body.pgno)
		flags = uint16(body.pgno >> 32)
	case bodyInline:
		word = uint32(len(body.inline))
	case bodyReserve:
		word = uint32(body.size)
	case bodyOverflow:
		word = uint32(body.size)
		flags |= format.FBigData
	}
	putLoHi(p, ofs, word)
	format.Put16(p, ofs+nodeFlagsOff, flags)
	k := ofs + format.NodeSize
	if k+len(key) > len(p) {
		return 0, false
	}
	copy(p[k:], key)
	d := ofs + l.NodeDataOffset(len(key))
	switch body.kind {
	case bodyInline:
		if d+len(body.inline) > len(p) {
			return 0, false
		}
		copy(p[d:], body.inline)
	case bodyOverflow:
		if d > len(p) || !l.WriteOverflowRef(p[d:], body.ovref) {
			return 0, false
		}
	}
	return ofs, true
}

// insertLeaf2 is the IS_LEAF2 branch of mdb_node_add: insert a ksize-byte
// key at indx, shifting the following keys up.
func insertLeaf2(l *format.Layout, p []byte, indx int, key []byte, ksize int) bool {
	nkeys := numKeys(l, p)
	// ksize may be 1: C's mp_upper -= ksize - sizeof(indx_t) then adds 1
	// (indx_t arithmetic), and setUpper stores mod 2^16 as C does.
	if indx > nkeys || len(key) != ksize || ksize < 1 {
		return false
	}
	start := l.PageHdr + indx*ksize
	end := l.PageHdr + nkeys*ksize
	if end+ksize > len(p) {
		return false
	}
	copy(p[start+ksize:end+ksize], p[start:end])
	copy(p[start:start+ksize], key)
	// Just using these for counting.
	setLower(l, p, pLower(l, p)+2)
	up := pUpper(l, p) - (ksize - 2)
	if up < 0 {
		return false
	}
	setUpper(l, p, up)
	return true
}

// nodeSizeAt is the on-page size of the node at off (header, key and
// inline data or overflow reference), EVEN, as mdb_node_del computes it.
func nodeSizeAt(l *format.Layout, p []byte, off int, isLeaf bool) (int, bool) {
	ksize, nflags, dsize, ok := nodeHdr(p, off)
	if !ok {
		return 0, false
	}
	sz := format.NodeSize + l.NodeKeyLen(ksize)
	if isLeaf {
		sz += l.StoredDataLen(dsize, nflags&format.FBigData != 0)
	}
	return format.Even(sz), true
}

// deleteNode is mdb_node_del (mdb.c line 7602): delete node indx, closing
// the gap in the pointer array and in the node area. ksize is the key size
// of a P_LEAF2 page.
func deleteNode(l *format.Layout, p []byte, indx, ksize int) bool {
	numkeys := numKeys(l, p)
	if indx >= numkeys {
		return false
	}
	pflags := pFlags(l, p)
	if pflags&format.PLeaf2 != 0 {
		x := numkeys - 1 - indx
		base := l.PageHdr + indx*ksize
		end := base + ksize + x*ksize
		if end > len(p) || ksize < 1 {
			return false
		}
		copy(p[base:], p[base+ksize:end])
		setLower(l, p, pLower(l, p)-2)
		setUpper(l, p, pUpper(l, p)+ksize-2)
		return true
	}
	nodeOfs, ok := ptrAt(l, p, indx)
	if !ok {
		return false
	}
	sz, ok := nodeSizeAt(l, p, nodeOfs, pflags&format.PLeaf != 0)
	if !ok {
		return false
	}
	first := l.PageHdr
	if first+2*numkeys > len(p) {
		return false
	}
	below := nodeOfs - l.PtrBase
	j := 0
	for i := 0; i < numkeys; i++ {
		if i == indx {
			continue
		}
		v := int(hdr16(p, first+2*i))
		if v < below {
			v += sz
		}
		format.Put16(p, first+2*j, uint16(v))
		j++
	}
	base := pUpper(l, p)
	if base > nodeOfs || nodeOfs+sz > len(p) {
		return false
	}
	copy(p[base+sz:], p[base:nodeOfs])
	setLower(l, p, pLower(l, p)-2)
	setUpper(l, p, base+sz)
	return true
}

// shrinkNode is mdb_node_shrink (mdb.c line 7658): give the free space of
// the sub-page in leaf node indx back to the main page p.
func shrinkNode(l *format.Layout, p []byte, indx int, pagePgno uint64) bool {
	nodeOfs, ok := ptrAt(l, p, indx)
	if !ok {
		return false
	}
	ksize, _, dsize, ok := nodeHdr(p, nodeOfs)
	if !ok {
		return false
	}
	sp := nodeOfs + l.NodeDataOffset(ksize)
	if sp+dsize > len(p) {
		return false
	}
	sub := p[sp : sp+dsize]
	delta := sizeLeft(l, sub)
	nsize := dsize - delta
	if nsize < 0 {
		return false
	}
	subKeys := numKeys(l, sub)
	subLeaf2 := pFlags(l, sub)&format.PLeaf2 != 0
	var n int
	if subLeaf2 {
		if nsize&1 != 0 {
			return true // do not make the node uneven-sized
		}
		n = nsize
	} else {
		// The pointer array moves up by delta (into the destination
		// sub-page); its entries shrink by delta because the sub-page now
		// starts delta bytes later.
		xp := sp + delta
		for i := subKeys - 1; i >= 0; i-- {
			v, ok := ptrAt(l, p[sp:], i)
			if !ok || v < delta {
				return false
			}
			if !setPtr(l, p[xp:], i, v-delta) {
				return false
			}
		}
		n = l.PageHdr
	}
	subLower := pLower(l, p[sp:])
	setUpper(l, p[sp:], subLower)
	setPgno(p[sp:], pagePgno)
	if !setNodeDSize(l, p, indx, nsize) {
		return false
	}
	// Shift <lower nodes...initial part of subpage> upward.
	base := pUpper(l, p)
	end := sp + n
	if base > end || end+delta > len(p) {
		return false
	}
	copy(p[base+delta:], p[base:end])
	nk := numKeys(l, p)
	for i := 0; i < nk; i++ {
		v, ok := ptrAt(l, p, i)
		if !ok {
			return false
		}
		if v <= nodeOfs && !setPtr(l, p, i, v+delta) {
			return false
		}
	}
	setUpper(l, p, base+delta)
	return true
}

// updateKey is the in-place part of mdb_update_key (mdb.c line 7962):
// replace the key of branch node indx, shifting the node area when
// EVEN(key length) changes. needsSplit: the page has no room for the
// longer key (the caller deletes and re-inserts with a split).
func updateKey(l *format.Layout, p []byte, indx int, key []byte) (needsSplit, ok bool) {
	nodeOfs, ok := ptrAt(l, p, indx)
	if !ok {
		return false, false
	}
	oksize, _, _, ok := nodeHdr(p, nodeOfs)
	if !ok {
		return false, false
	}
	ksize := format.Even(len(key))
	oksizeEven := format.Even(oksize)
	if ksize != oksizeEven {
		if ksize > oksizeEven && sizeLeft(l, p) < ksize-oksizeEven {
			return true, true
		}
		numkeys := numKeys(l, p)
		base := pUpper(l, p)
		if nodeOfs < base {
			return false, false
		}
		n := nodeOfs - base + format.NodeSize
		if ksize > oksizeEven {
			// Grow: everything from upper to this node's header moves down.
			delta := ksize - oksizeEven
			for i := 0; i < numkeys; i++ {
				v, ok := ptrAt(l, p, i)
				if !ok {
					return false, false
				}
				if v <= nodeOfs && (v < delta || !setPtr(l, p, i, v-delta)) {
					return false, false
				}
			}
			dst := base - delta
			if dst < 0 || base+n > len(p) {
				return false, false
			}
			copy(p[dst:], p[base:base+n])
			setUpper(l, p, dst)
			nodeOfs -= delta
		} else {
			// Shrink: it moves up.
			delta := oksizeEven - ksize
			for i := 0; i < numkeys; i++ {
				v, ok := ptrAt(l, p, i)
				if !ok {
					return false, false
				}
				if v <= nodeOfs && !setPtr(l, p, i, v+delta) {
					return false, false
				}
			}
			if base+n+delta > len(p) {
				return false, false
			}
			copy(p[base+delta:], p[base:base+n])
			setUpper(l, p, base+delta)
			nodeOfs += delta
		}
	}
	// But even if no shift was needed, update ksize.
	format.Put16(p, nodeOfs+nodeKSizeOff, uint16(len(key)))
	k := nodeOfs + format.NodeSize
	if k+len(key) > len(p) {
		return false, false
	}
	copy(p[k:], key)
	return false, true
}
