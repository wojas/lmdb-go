// MDB_node: a key (and on leaf pages data) inside a branch or leaf page
// (rlmdb format/node.rs).
//
// Layout (mdb.c line 918, NODESIZE 8): mn_lo, mn_hi (u16 each, swapped on
// big-endian hosts), mn_flags (u16; branch nodes: pgno bits 32..48),
// mn_ksize (u16), then the key, then the data (1.0 pads the key to an even
// length first). A leaf node's data is plain bytes, an overflow reference
// (F_BIGDATA), an MDB_db record (F_SUBDATA, also with F_DUPDATA for a
// sub-DB of duplicates), or a P_SUBP sub-page (F_DUPDATA alone).

package format

// Node flags (mn_flags of leaf nodes).
const (
	FBigData uint16 = 0x01 // F_BIGDATA: data is on overflow pages
	FSubData uint16 = 0x02 // F_SUBDATA: data is an MDB_db
	FDupData uint16 = 0x04 // F_DUPDATA: data holds duplicates
)

// Node is a read-only view of one node: B runs from the node header to the
// end of its page, so every accessor checks against the page.
type Node struct {
	B []byte
	L *Layout
}

// NewNode wraps the bytes from a node header to the end of its page; ok is
// false if there is no room for the header.
func NewNode(b []byte, l *Layout) (Node, bool) {
	if len(b) < NodeSize {
		return Node{}, false
	}
	return Node{b, l}, true
}

func (n Node) field(off int) uint16 {
	if len(n.B) < NodeSize {
		return 0
	}
	return ne.Uint16(n.B[off:])
}

// Lo is mn_lo.
func (n Node) Lo() uint16 { return n.field(nodeLo) }

// Hi is mn_hi.
func (n Node) Hi() uint16 { return n.field(nodeHi) }

// Flags is mn_flags (pgno bits on branch nodes).
func (n Node) Flags() uint16 { return n.field(nodeFlags) }

// KeySize is mn_ksize.
func (n Node) KeySize() int { return int(n.field(nodeKSize)) }

// DataSize is NODEDSZ: mn_lo | mn_hi << 16.
func (n Node) DataSize() int { return int(uint32(n.Lo()) | uint32(n.Hi())<<16) }

// ChildPgno is NODEPGNO: mn_lo | mn_hi << 16 | mn_flags << 32.
func (n Node) ChildPgno() uint64 {
	return uint64(n.Lo()) | uint64(n.Hi())<<16 | uint64(n.Flags())<<32
}

// IsBigData reports F_BIGDATA.
func (n Node) IsBigData() bool { return n.Flags()&FBigData != 0 }

// IsSubData reports F_SUBDATA.
func (n Node) IsSubData() bool { return n.Flags()&FSubData != 0 }

// IsDupData reports F_DUPDATA.
func (n Node) IsDupData() bool { return n.Flags()&FDupData != 0 }

// Key is the key bytes (NODEKEY).
func (n Node) Key() ([]byte, bool) {
	k := n.KeySize()
	if len(n.B) < NodeSize || k > len(n.B)-NodeSize {
		return nil, false
	}
	return n.B[NodeSize : NodeSize+k : NodeSize+k], true
}

// KeyTail is the bytes from the key to the end of the page (C's integer
// comparators may read past a short stored key).
func (n Node) KeyTail() []byte {
	if len(n.B) < NodeSize {
		return nil
	}
	return n.B[NodeSize:]
}

// DataOffset is NODEDATA - node.
func (n Node) DataOffset() int { return n.L.NodeDataOffset(n.KeySize()) }

// DataTail is the bytes from the data part to the end of the page.
func (n Node) DataTail() []byte {
	off := n.DataOffset()
	if off > len(n.B) {
		return nil
	}
	return n.B[off:]
}

// InlineData is the data (NODEDATA, NODEDSZ bytes) of a leaf node without
// F_BIGDATA: plain data, a sub-page or an MDB_db.
func (n Node) InlineData() ([]byte, bool) {
	off := n.DataOffset()
	sz := n.DataSize()
	if off > len(n.B) || sz > len(n.B)-off {
		return nil, false
	}
	return n.B[off : off+sz : off+sz], true
}

// KeyAndData is Key and InlineData with one bounds check: the key ends
// where the data starts (1.0: at most one padding byte before).
func (n Node) KeyAndData() (key, data []byte, ok bool) {
	k := n.KeySize()
	off := n.DataOffset()
	sz := n.DataSize()
	if off > len(n.B) || sz > len(n.B)-off {
		return nil, nil, false
	}
	return n.B[NodeSize : NodeSize+k : NodeSize+k], n.B[off : off+sz : off+sz], true
}

// OverflowRef is the overflow reference stored as the data of an
// F_BIGDATA node (in 0.9 only its page number).
func (n Node) OverflowRef() (OverflowRef, bool) {
	off := n.DataOffset()
	if off > len(n.B) {
		return OverflowRef{}, false
	}
	return n.L.ReadOverflowRef(n.B[off:])
}

// OverflowPgno is the first overflow page of an F_BIGDATA node.
func (n Node) OverflowPgno() (uint64, bool) {
	r, ok := n.OverflowRef()
	return r.Pgno, ok
}

// Raw is the whole node (header, key, data) of a leaf node as stored;
// branch nodes have no data part. The data part of an F_BIGDATA node is
// its overflow reference. Padding after the data is not included.
func (n Node) Raw(isLeaf bool) ([]byte, bool) {
	size := n.DataOffset()
	if isLeaf {
		if n.IsBigData() {
			size += n.L.OvpageSize
		} else {
			size += n.DataSize()
		}
	}
	if size > len(n.B) {
		return nil, false
	}
	return n.B[:size], true
}
