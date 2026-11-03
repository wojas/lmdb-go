// Where things are in a page: the byte offsets of the structures shared
// by both formats, and the per-environment Layout of the two data-file
// formats (rlmdb format/layout.rs and format/mod.rs).
//
// LMDB 0.9 (MDB_DATA_VERSION 1) and LMDB 1.0 (MDB_DATA_VERSION 3) share
// every on-disk structure except a few page-level details:
//
//	item                 0.9                   1.0
//	PAGEHDRSZ            16                    24 (mp_txnid after mp_pgno)
//	PAGEBASE             0                     24: lower, upper, ptrs header-relative
//	NODEDATA             mn_data + ksize       mn_data + EVEN(ksize)
//	F_BIGDATA data       8-byte pgno           24-byte MDB_ovpage {pgno, txnid, pages}
//	P_SUBP flags         P_LEAF|P_DIRTY|P_SUBP P_LEAF|P_SUBP
//	MAX_PAGESIZE         32 KiB                64 KiB
//	named DB key         the name              the name and its NUL
//
// The engine reads all of these from the Layout the environment picks at
// open.

package format

import "encoding/binary"

var ne = binary.NativeEndian

// MDB_node: the same in both formats.
const (
	// NodeSize is NODESIZE, the size of the node header; the key starts
	// here.
	NodeSize = 8
	// nodeFlags is the offset of mn_flags (on branch nodes the top 16 bits
	// of the child pgno).
	nodeFlags = 4
	// nodeKSize is the offset of mn_ksize.
	nodeKSize = 6
)

// mn_lo and mn_hi swap places on big-endian hosts so that the pair reads
// as one native 32-bit word.
var nodeLo, nodeHi = func() (int, int) {
	var b [2]byte
	ne.PutUint16(b[:], 1)
	if b[0] == 1 {
		return 0, 2
	}
	return 2, 0
}()

// MDB_db: the same in both formats.
const (
	DBSize          = 48 // sizeof(MDB_db)
	dbPad           = 0
	dbFlags         = 4
	dbDepth         = 6
	dbBranchPages   = 8
	dbLeafPages     = 16
	dbOverflowPages = 24
	dbEntries       = 32
	dbRoot          = 40
)

// MDB_meta: the same struct in both formats, at Layout.MetaOffset in the
// page.
const (
	MetaSize     = 136 // sizeof(MDB_meta)
	metaMagic    = 0
	metaVersion  = 4
	metaAddress  = 8
	MetaMapSize  = 16 // mdb_env_write_meta rewrites the meta from here on
	metaFreeDB   = 24
	metaMainDB   = 24 + DBSize
	MetaLastPgno = 24 + 2*DBSize
	MetaTxnID    = 24 + 2*DBSize + 8
)

// OverflowRef is the data of an F_BIGDATA node: just the page number in
// 0.9, MDB_ovpage in 1.0.
type OverflowRef struct {
	Pgno  uint64 // op_pgno: first page of the overflow run
	TxnID uint64 // op_txnid (1.0 only): the transaction that wrote the run
	Pages uint64 // op_pages (1.0 only): the length of the run
}

// Layout describes what differs between the data-file formats. There is
// one per format (LayoutV09, LayoutV10); pages and nodes carry a pointer
// to their environment's.
type Layout struct {
	// Format is the format this describes.
	Format Format
	// DataVersion is MDB_DATA_VERSION, mm_version of the meta pages.
	DataVersion uint32
	// PageHdr is PAGEHDRSZ. The fields mp_pad, mp_flags, mp_lower/mp_upper
	// (or mp_pages) are its last 8 bytes in both formats.
	PageHdr int
	// PtrBase is PAGEBASE: what mp_ptrs[], mp_lower and mp_upper are
	// relative to (0: the page start; 1.0: the end of the header).
	PtrBase int
	// EvenNodes: nodes store key and data padded to even lengths (1.0).
	EvenNodes bool
	// OvpageSize is the size of an F_BIGDATA node's overflow reference.
	OvpageSize int
	// SubpageFlags is the flags word C writes into a new P_SUBP sub-page.
	SubpageFlags uint16
	// MaxPageSize is MAX_PAGESIZE.
	MaxPageSize int
	// MinPageSize is the smallest page size of an environment of the format
	// that is opened. 1.0: that of mdb_env_set_pagesize, which scales the
	// largest key with the page. 0.9 creates environments with the OS page
	// size only (4 KiB or more everywhere) but opens any page size, and its
	// largest key is the constant MDB_MAXKEYSIZE (511): below 2 KiB a node
	// with such a key no longer fits me_nodemax and a P_LEAF2 page holds a
	// single such item, so C writes past its pages (rlmdb KNOWN_C_BUGS.md
	// row 48). Smaller 0.9 pages are refused.
	MinPageSize int
	// DBNameNUL: the key of a named database includes the name's
	// terminating NUL (1.0).
	DBNameNUL bool
}

// LayoutV09 is the LMDB 0.9 layout.
var LayoutV09 = &Layout{
	Format:       V09,
	DataVersion:  1,
	PageHdr:      16,
	PtrBase:      0,
	EvenNodes:    false,
	OvpageSize:   8,
	SubpageFlags: PLeaf | PDirty | PSubp, // mdb.c line 6920
	MaxPageSize:  0x8000,
	MinPageSize:  0x800,
	DBNameNUL:    false,
}

// LayoutV10 is the LMDB 1.0 layout (mdb.RE/1.0).
var LayoutV10 = &Layout{
	Format:       V10,
	DataVersion:  3,
	PageHdr:      24,
	PtrBase:      24,
	EvenNodes:    true,
	OvpageSize:   24,
	SubpageFlags: PLeaf | PSubp, // mdb.c 1.0 line 8806: no P_DIRTY in 1.0
	MaxPageSize:  0x10000,
	MinPageSize:  0x100,
	DBNameNUL:    true,
}

// Layout returns the page layout of the format (nil for an unknown one).
func (f Format) Layout() *Layout {
	switch f {
	case V09:
		return LayoutV09
	case V10:
		return LayoutV10
	}
	return nil
}

// Even is EVEN(n): n rounded up to an even number.
func Even(n int) int { return (n + 1) &^ 1 }

// TxnIDOffset is the offset of mp_txnid, and whether the header has one
// (1.0 only).
func (l *Layout) TxnIDOffset() (int, bool) {
	if l.Format == V10 {
		return 8, true
	}
	return 0, false
}

// PadOffset is the offset of mp_pad (the key size on P_LEAF2 pages).
func (l *Layout) PadOffset() int { return l.PageHdr - 8 }

// FlagsOffset is the offset of mp_flags.
func (l *Layout) FlagsOffset() int { return l.PageHdr - 6 }

// LowerOffset is the offset of mp_lower.
func (l *Layout) LowerOffset() int { return l.PageHdr - 4 }

// UpperOffset is the offset of mp_upper.
func (l *Layout) UpperOffset() int { return l.PageHdr - 2 }

// PagesOffset is the offset of mp_pages (u32, an overflow run's length,
// overlaying mp_lower/mp_upper).
func (l *Layout) PagesOffset() int { return l.PageHdr - 4 }

// MetaOffset is METADATA(p) of a meta page: where the MDB_meta starts.
func (l *Layout) MetaOffset() int { return l.PageHdr }

// EmptyLower is the stored mp_lower of an empty page,
// PAGEHDRSZ - PAGEBASE (mdb_page_new); also what NUMKEYS subtracts.
func (l *Layout) EmptyLower() int { return l.PageHdr - l.PtrBase }

// NumKeys is NUMKEYS(p) from a stored mp_lower:
// (MP_LOWER(p) - (PAGEHDRSZ - PAGEBASE)) >> 1. A corrupt mp_lower below the
// header yields 0.
func (l *Layout) NumKeys(lower uint16) int {
	n := int(lower) - l.EmptyLower()
	if n < 0 {
		return 0
	}
	return n >> 1
}

// SetTxnID sets mp_txnid of the page (or sub-page) header in page in
// formats that have it (1.0); a no-op for 0.9 or a too short buffer. C 1.0
// stamps every page it writes (mdb_page_flush) and every sub-page it builds
// with the writing transaction's id.
func (l *Layout) SetTxnID(page []byte, txnid uint64) {
	if off, ok := l.TxnIDOffset(); ok && len(page) >= off+8 {
		ne.PutUint64(page[off:], txnid)
	}
}

// LoosensRebalancedRoot reports whether mdb_rebalance hands an emptied or
// collapsed root to mdb_page_loose (1.0) instead of the free list (0.9).
func (l *Layout) LoosensRebalancedRoot() bool { return l.Format == V10 }

// DropsNamedDBsWithMain reports whether mdb_drop of the main DB first
// deletes the named databases recorded in it and refuses with
// MDB_DBIS_BUSY while one is open (1.0), instead of leaking their pages
// (0.9, rlmdb KNOWN_C_BUGS.md row 21).
func (l *Layout) DropsNamedDBsWithMain() bool { return l.Format == V10 }

// NodeKeyLen is the bytes a key of ksize bytes takes in a node.
func (l *Layout) NodeKeyLen(ksize int) int {
	if l.EvenNodes {
		return Even(ksize)
	}
	return ksize
}

// NodeDataLen is the bytes inline data of dsize bytes takes in a node.
func (l *Layout) NodeDataLen(dsize int) int {
	if l.EvenNodes {
		return Even(dsize)
	}
	return dsize
}

// StoredDataLen is the bytes the data part of a leaf node takes: the
// overflow reference for an F_BIGDATA node, else NodeDataLen.
func (l *Layout) StoredDataLen(dsize int, bigdata bool) int {
	if bigdata {
		return l.OvpageSize
	}
	return l.NodeDataLen(dsize)
}

// NodeDataOffset is NODEDATA(node) - node: NODESIZE + ksize (1.0:
// NODESIZE + EVEN(ksize)).
func (l *Layout) NodeDataOffset(ksize int) int { return NodeSize + l.NodeKeyLen(ksize) }

// LeafSize is LEAFSIZE(k, d) without the pointer.
func (l *Layout) LeafSize(ksize, dsize int) int {
	return NodeSize + l.NodeKeyLen(ksize) + l.NodeDataLen(dsize)
}

// BranchSize is INDXSIZE(k) without the pointer, NODESIZE + ksize in both
// formats (1.0 rounds the key when the node is added).
func (l *Layout) BranchSize(ksize int) int { return NodeSize + ksize }

// ReadOverflowRef reads the overflow reference at the start of data.
func (l *Layout) ReadOverflowRef(data []byte) (OverflowRef, bool) {
	if len(data) < l.OvpageSize {
		return OverflowRef{}, false
	}
	r := OverflowRef{Pgno: ne.Uint64(data)}
	if l.Format == V10 {
		r.TxnID = ne.Uint64(data[8:])
		r.Pages = ne.Uint64(data[16:])
	}
	return r, true
}

// WriteOverflowRef writes r at the start of data (OvpageSize bytes; 0.9
// stores only the page number).
func (l *Layout) WriteOverflowRef(data []byte, r OverflowRef) bool {
	if len(data) < l.OvpageSize {
		return false
	}
	ne.PutUint64(data, r.Pgno)
	if l.Format == V10 {
		ne.PutUint64(data[8:], r.TxnID)
		ne.PutUint64(data[16:], r.Pages)
	}
	return true
}

// PageSpace is me_pagespace, psize - PAGEHDRSZ.
func (l *Layout) PageSpace(psize int) int { return psize - l.PageHdr }

// NodeMax is me_nodemax: ((me_pagespace / MDB_MINKEYS) & -2) -
// sizeof(indx_t).
func (l *Layout) NodeMax(psize int) int { return ((l.PageSpace(psize) / MinKeys) &^ 1) - 2 }

// MaxKeySize is mdb_env_get_maxkeysize: 511 for 0.9; 1.0 computes
// me_nodemax - (NODESIZE + sizeof(MDB_db)).
func (l *Layout) MaxKeySize(psize int) int {
	if l.Format == V09 {
		return MaxKeySize09
	}
	return l.NodeMax(psize) - (NodeSize + DBSize)
}

// MaxFree1Pg is me_maxfree_1pg: how many page numbers fit in one
// free-list overflow page.
func (l *Layout) MaxFree1Pg(psize int) int { return l.PageSpace(psize)/8 - 1 }

// OverflowPagesFor is OVPAGES(size, psize): the overflow pages needed for
// size bytes of data, (PAGEHDRSZ - 1 + size) / psize + 1.
func (l *Layout) OverflowPagesFor(size, psize int) int { return (l.PageHdr-1+size)/psize + 1 }

// SubpageDirty is the P_DIRTY bit if the format keeps it in sub-pages
// (0.9), else 0.
func (l *Layout) SubpageDirty() uint16 { return l.SubpageFlags & PDirty }

// Get16 reads a native-endian uint16 at off of b.
func Get16(b []byte, off int) (uint16, bool) {
	if off < 0 || off > len(b)-2 {
		return 0, false
	}
	return ne.Uint16(b[off:]), true
}

// Get32 reads a native-endian uint32 at off of b.
func Get32(b []byte, off int) (uint32, bool) {
	if off < 0 || off > len(b)-4 {
		return 0, false
	}
	return ne.Uint32(b[off:]), true
}

// Get64 reads a native-endian uint64 at off of b.
func Get64(b []byte, off int) (uint64, bool) {
	if off < 0 || off > len(b)-8 {
		return 0, false
	}
	return ne.Uint64(b[off:]), true
}

// Put16 writes a native-endian uint16 at off of b (which must hold it).
func Put16(b []byte, off int, v uint16) { ne.PutUint16(b[off:], v) }

// Put32 writes a native-endian uint32 at off of b.
func Put32(b []byte, off int, v uint32) { ne.PutUint32(b[off:], v) }

// Put64 writes a native-endian uint64 at off of b.
func Put64(b []byte, off int, v uint64) { ne.PutUint64(b[off:], v) }
