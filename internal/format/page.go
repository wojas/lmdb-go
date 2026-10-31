// MDB_page: page header accessors, the node index and P_LEAF2 keys
// (rlmdb format/page.rs).
//
// A page starts with a header (16 bytes in 0.9, 24 in 1.0 which inserts
// mp_txnid after mp_pgno); mp_pad, mp_flags, mp_lower and mp_upper are the
// last 8 bytes of it in both formats. Branch and leaf pages continue with
// mp_ptrs[], one u16 offset per node, sorted by key; the nodes are packed
// at the end of the page. In 1.0, mp_lower, mp_upper and the pointers are
// relative to the end of the header (PAGEBASE, ITS#7713). P_LEAF2 pages
// (MDB_DUPFIXED duplicates) have no pointers or node headers: NUMKEYS keys
// of mp_pad bytes each are packed after the header, while mp_lower still
// counts as if there were pointers. Overflow pages hold one big data item;
// only the first page of a run has a header and mp_pages gives the run
// length. P_SUBP sub-pages (duplicates inline in a leaf node) use the same
// layout inside the node's data, so Page works on them unchanged.
//
// All accessors are bounds-checked and report malformed input instead of
// panicking.

package format

// Page flags (mp_flags).
const (
	PBranch   uint16 = 0x01   // P_BRANCH: branch page
	PLeaf     uint16 = 0x02   // P_LEAF: leaf page
	POverflow uint16 = 0x04   // P_OVERFLOW: first page of an overflow run
	PMeta     uint16 = 0x08   // P_META: meta page
	PDirty    uint16 = 0x10   // P_DIRTY: dirty page (in memory; 0.9 sub-pages)
	PLeaf2    uint16 = 0x20   // P_LEAF2: leaf page of a DUPFIXED sub-DB
	PSubp     uint16 = 0x40   // P_SUBP: sub-page of duplicates in a node
	PLoose    uint16 = 0x4000 // P_LOOSE: dirtied then freed (in memory)
	PKeep     uint16 = 0x8000 // P_KEEP: leave alone during spill (in memory)
)

// Offsets of mp_pad, mp_flags, mp_lower and mp_upper within the last 8
// bytes of the page header.
const (
	fieldPad   = 0
	fieldFlags = 2
	fieldLower = 4
	fieldUpper = 6
)

// Page is a read-only view of a page (or sub-page) of a given Layout. B
// may be shorter than a header only for pages built with MakePage; the
// accessors then read zeros, never out of bounds.
type Page struct {
	B []byte
	L *Layout
}

// NewPage wraps a page; ok is false if b cannot hold a header.
func NewPage(b []byte, l *Layout) (Page, bool) {
	if len(b) < l.PageHdr {
		return Page{}, false
	}
	return Page{b, l}, true
}

// MakePage wraps bytes without the length check of NewPage.
func MakePage(b []byte, l *Layout) Page { return Page{b, l} }

func (p Page) hdr16(field int) uint16 {
	off := p.L.PageHdr - 8 + field
	if off+2 > len(p.B) {
		return 0
	}
	return ne.Uint16(p.B[off:])
}

// Pgno is mp_pgno (the first 8 bytes in both formats).
func (p Page) Pgno() uint64 {
	if len(p.B) < 8 {
		return 0
	}
	return ne.Uint64(p.B)
}

// TxnID is mp_txnid, in formats whose header has it (1.0).
func (p Page) TxnID() (uint64, bool) {
	off, ok := p.L.TxnIDOffset()
	if !ok {
		return 0, false
	}
	return Get64(p.B, off)
}

// Pad is mp_pad: the key size on P_LEAF2 pages.
func (p Page) Pad() uint16 { return p.hdr16(fieldPad) }

// Flags is mp_flags.
func (p Page) Flags() uint16 { return p.hdr16(fieldFlags) }

// Lower is mp_lower as stored (PAGEBASE-relative).
func (p Page) Lower() uint16 { return p.hdr16(fieldLower) }

// Upper is mp_upper as stored (PAGEBASE-relative).
func (p Page) Upper() uint16 { return p.hdr16(fieldUpper) }

// OverflowPages is mp_pages: the length of an overflow run.
func (p Page) OverflowPages() uint32 {
	return uint32(p.hdr16(fieldLower)) | uint32(p.hdr16(fieldUpper))<<16
}

// NumKeys is NUMKEYS: the number of nodes (or P_LEAF2 keys).
func (p Page) NumKeys() int { return p.L.NumKeys(p.Lower()) }

// SizeLeft is SIZELEFT: free bytes between pointers and nodes.
//
// C computes (indx_t)(mp_upper - mp_lower), modulo 2^16. That matters for
// P_LEAF2 pages, which only count with the two fields (mp_upper grows past
// the page end, and wraps on 64 KiB pages, with 1-byte items, while the
// difference stays exact; see AddLeaf2Key): their free space is the
// wrapping difference, as in C. On other pages mp_upper is an offset, and
// a corrupt one below mp_lower yields 0.
func (p Page) SizeLeft() int {
	u, l := p.Upper(), p.Lower()
	if p.IsLeaf2() {
		return int(u - l)
	}
	if u < l {
		return 0
	}
	return int(u - l)
}

// IsBranch is IS_BRANCH.
func (p Page) IsBranch() bool { return p.Flags()&PBranch != 0 }

// IsLeaf is IS_LEAF.
func (p Page) IsLeaf() bool { return p.Flags()&PLeaf != 0 }

// IsLeaf2 is IS_LEAF2.
func (p Page) IsLeaf2() bool { return p.Flags()&PLeaf2 != 0 }

// IsOverflow is IS_OVERFLOW.
func (p Page) IsOverflow() bool { return p.Flags()&POverflow != 0 }

// IsSubpage is IS_SUBP.
func (p Page) IsSubpage() bool { return p.Flags()&PSubp != 0 }

// IsMeta reports a meta page.
func (p Page) IsMeta() bool { return p.Flags()&PMeta != 0 }

// NodeOffset is mp_ptrs[i] + PAGEBASE: the offset of node i from the page
// start.
func (p Page) NodeOffset(i int) (int, bool) {
	if i < 0 || i >= p.NumKeys() {
		return 0, false
	}
	v, ok := Get16(p.B, p.L.PageHdr+2*i)
	if !ok {
		return 0, false
	}
	return int(v) + p.L.PtrBase, true
}

// Node is NODEPTR(p, i): node i; ok is false if i is out of range or the
// pointer leads outside the page.
func (p Page) Node(i int) (Node, bool) {
	off, ok := p.NodeOffset(i)
	if !ok || off > len(p.B)-NodeSize {
		return Node{}, false
	}
	return Node{p.B[off:], p.L}, true
}

// NodeKey is NODEKEY(NODEPTR(p, i)), without building the Node (binary
// searches).
func (p Page) NodeKey(i int) ([]byte, bool) {
	off, ok := p.NodeOffset(i)
	if !ok || off > len(p.B)-NodeSize {
		return nil, false
	}
	ksize := int(ne.Uint16(p.B[off+nodeKSize:]))
	start := off + NodeSize
	if ksize > len(p.B)-start {
		return nil, false
	}
	return p.B[start : start+ksize : start+ksize], true
}

// Leaf2Key is LEAF2KEY(p, i, ksize): key i of a P_LEAF2 page.
func (p Page) Leaf2Key(i, ksize int) ([]byte, bool) {
	if i < 0 || i >= p.NumKeys() {
		return nil, false
	}
	start := p.L.PageHdr + i*ksize
	if ksize < 0 || start > len(p.B) || ksize > len(p.B)-start {
		return nil, false
	}
	return p.B[start : start+ksize : start+ksize], true
}

// Leaf2KeyTail is the bytes from LEAF2KEY(p, i, ksize) to the end of the
// page.
func (p Page) Leaf2KeyTail(i, ksize int) ([]byte, bool) {
	if i < 0 || i >= p.NumKeys() {
		return nil, false
	}
	start := p.L.PageHdr + i*ksize
	if ksize < 0 || start > len(p.B) {
		return nil, false
	}
	return p.B[start:], true
}

// Leaf2Keys is all keys of a P_LEAF2 page as one slice (what
// MDB_GET_MULTIPLE returns).
func (p Page) Leaf2Keys(ksize int) ([]byte, bool) {
	n := p.NumKeys() * ksize
	start := p.L.PageHdr
	if ksize < 0 || start > len(p.B) || n > len(p.B)-start {
		return nil, false
	}
	return p.B[start : start+n : start+n], true
}

// OverflowData is the data of an overflow run: n bytes right after the
// header of the first page (METADATA(omp)).
func (p Page) OverflowData(n int) ([]byte, bool) {
	start := p.L.PageHdr
	if n < 0 || start > len(p.B) || n > len(p.B)-start {
		return nil, false
	}
	return p.B[start : start+n : start+n], true
}

// NodeData is the data of a node built with PageBuilder.AddNode.
type NodeData struct {
	// Child is the child page of a branch node (IsChild).
	Child   uint64
	IsChild bool
	// Inline is the data of a leaf node stored in the page.
	Inline []byte
	// OverflowPgno and OverflowLen describe a leaf node whose data lives on
	// an overflow run (IsOverflow; F_BIGDATA is set automatically).
	OverflowPgno uint64
	OverflowLen  int
	IsOverflow   bool
}

// PageBuilder is a mutable view of a page buffer, for building pages:
// meta pages of a new environment and test fixtures.
type PageBuilder struct {
	B []byte
	L *Layout
}

// Page returns a read-only view of the same bytes.
func (b PageBuilder) Page() Page { return Page{b.B, b.L} }

// Init initialises an empty branch/leaf page as mdb_page_new does: header
// with pgno and flags, no nodes (0.9: mp_lower 16, mp_upper len; 1.0: 0 and
// len - 24).
func (b PageBuilder) Init(pgno uint64, flags uint16) {
	l := b.L
	clear(b.B[:l.PageHdr])
	b.SetPgno(pgno)
	b.SetFlags(flags)
	b.SetLower(uint16(l.EmptyLower()))
	b.SetUpper(uint16(len(b.B) - l.PtrBase))
}

// InitOverflow initialises the first page of an overflow run of pages
// pages.
func (b PageBuilder) InitOverflow(pgno uint64, pages uint32) {
	clear(b.B[:b.L.PageHdr])
	b.SetPgno(pgno)
	b.SetFlags(POverflow)
	Put32(b.B, b.L.PagesOffset(), pages)
}

// SetPgno sets mp_pgno.
func (b PageBuilder) SetPgno(pgno uint64) { Put64(b.B, 0, pgno) }

// SetPad sets mp_pad.
func (b PageBuilder) SetPad(pad uint16) { Put16(b.B, b.L.PadOffset(), pad) }

// SetFlags sets mp_flags.
func (b PageBuilder) SetFlags(flags uint16) { Put16(b.B, b.L.FlagsOffset(), flags) }

// SetLower sets mp_lower (the stored, PAGEBASE-relative value).
func (b PageBuilder) SetLower(v uint16) { Put16(b.B, b.L.LowerOffset(), v) }

// SetUpper sets mp_upper (the stored, PAGEBASE-relative value).
func (b PageBuilder) SetUpper(v uint16) { Put16(b.B, b.L.UpperOffset(), v) }

// Body is the bytes after the header (METADATA(p)).
func (b PageBuilder) Body() []byte { return b.B[b.L.PageHdr:] }

// AddNode inserts a node at index indx, laid out exactly as mdb_node_add
// does: pointers above indx move up one slot, the node goes at
// mp_upper - EVEN(size), mp_lower grows by 2. flags are node flags
// (F_SUBDATA, F_DUPDATA). It never allocates overflow pages. ok is false
// (MDB_PAGE_FULL) if the node does not fit.
func (b PageBuilder) AddNode(indx int, key []byte, data NodeData, flags uint16) bool {
	pg := b.Page()
	nkeys, lower, upper, sizeLeft := pg.NumKeys(), pg.Lower(), pg.Upper(), pg.SizeLeft()
	if indx < 0 || indx > nkeys {
		return false
	}
	l := b.L
	bodyLen := 0
	switch {
	case data.IsChild:
	case data.IsOverflow:
		bodyLen = l.OvpageSize
		flags |= FBigData
	default:
		bodyLen = l.NodeDataLen(len(data.Inline))
	}
	nodeSize := Even(NodeSize + l.NodeKeyLen(len(key)) + bodyLen)
	if nodeSize > sizeLeft-2 {
		return false
	}
	ptrs := l.PageHdr
	for i := nkeys - 1; i >= indx; i-- {
		v, _ := Get16(b.B, ptrs+2*i)
		Put16(b.B, ptrs+2*(i+1), v)
	}
	stored := int(upper) - nodeSize
	Put16(b.B, ptrs+2*indx, uint16(stored))
	b.SetUpper(uint16(stored))
	b.SetLower(lower + 2)

	ofs := stored + l.PtrBase
	node := b.B[ofs : ofs+nodeSize]
	clear(node)
	Put16(node, nodeKSize, uint16(len(key)))
	var loHi uint32
	flagsWord := flags
	switch {
	case data.IsChild:
		loHi = uint32(data.Child)
		flagsWord = uint16(data.Child >> 32)
	case data.IsOverflow:
		loHi = uint32(data.OverflowLen)
	default:
		loHi = uint32(len(data.Inline))
	}
	Put16(node, nodeLo, uint16(loHi))
	Put16(node, nodeHi, uint16(loHi>>16))
	Put16(node, nodeFlags, flagsWord)
	copy(node[NodeSize:], key)
	d := l.NodeDataOffset(len(key))
	switch {
	case data.IsChild:
	case data.IsOverflow:
		r := OverflowRef{
			Pgno:  data.OverflowPgno,
			Pages: uint64(l.OverflowPagesFor(data.OverflowLen, len(b.B))),
		}
		if !l.WriteOverflowRef(node[d:], r) {
			return false
		}
	default:
		copy(node[d:], data.Inline)
	}
	return true
}

// AddLeaf2Key inserts a key into a P_LEAF2 page at index indx, as the
// IS_LEAF2 branch of mdb_node_add does (keys shift up, mp_lower += 2,
// mp_upper -= ksize - 2, modulo 2^16 as in C: mp_upper grows for 1-byte
// keys).
func (b PageBuilder) AddLeaf2Key(indx int, key []byte) bool {
	pg := b.Page()
	nkeys, lower, upper, sizeLeft := pg.NumKeys(), pg.Lower(), pg.Upper(), pg.SizeLeft()
	ksize := len(key)
	if indx < 0 || indx > nkeys || sizeLeft < ksize {
		return false
	}
	hdr := b.L.PageHdr
	start := hdr + indx*ksize
	end := hdr + nkeys*ksize
	if end+ksize > len(b.B) {
		return false
	}
	copy(b.B[start+ksize:end+ksize], b.B[start:end])
	copy(b.B[start:], key)
	b.SetLower(lower + 2)
	b.SetUpper(upper - uint16(ksize-2))
	return true
}
