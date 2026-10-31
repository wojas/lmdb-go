package engine

// Fuzz target for the engine's own code that works on raw page bytes
// (go test -fuzz, see FUZZING.md at the repository root): the node search
// of a page (searchPage, keyAt) and the in-place page edits of pageops.go
// and the DUPSORT sub-page builder, which run on pages copied from the
// data file and therefore on whatever a damaged file holds. They must
// report malformed pages (ok false, MDB_CORRUPTED) instead of panicking
// or indexing out of range, and leave a page that the readers can still
// parse without panicking. Each edit is called the way its caller calls
// it (cursorput.go, cursordel.go): an index the cursor could hold, node
// sizes computed as mdb_node_add computes them, and the room checks the
// callers make first.

import (
	"bytes"
	"slices"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func fuzzEngineLayout(sel byte) *format.Layout {
	if sel&1 == 0 {
		return format.LayoutV09
	}
	return format.LayoutV10
}

// readAll runs the read side over a page: a search with every comparator
// and keyAt at each index.
func readAll(l *format.Layout, page []byte, key []byte) {
	mp := format.MakePage(page, l)
	for _, cmp := range []format.Comparator{format.Memn, format.Memnr, format.Long, format.Int,
		format.Clong, format.Cint, format.Custom(bytes.Compare)} {
		for _, ksize := range []int{int(mp.Pad()), len(key), 8} {
			i, _, _, err := searchPage(mp, cmp, ksize, key)
			if err == nil && (i < 0 || i > mp.NumKeys()) {
				panic("searchPage: index out of range")
			}
		}
	}
	for i := 0; i < min(mp.NumKeys(), maxFuzzKeys); i++ {
		_, _, _ = keyAt(mp, int(mp.Pad()), i)
		_, _, _ = nodeDataRange(l, page, i)
	}
}

const maxFuzzKeys = 1024

// FuzzPageOps searches a page and applies one edit to it.
func FuzzPageOps(f *testing.F) {
	for sel, l := range []*format.Layout{format.LayoutV09, format.LayoutV10} {
		for _, ps := range []int{256, 1024} {
			leaf := make([]byte, ps)
			b := format.PageBuilder{B: leaf, L: l}
			b.Init(5, format.PLeaf)
			b.AddNode(0, []byte("a"), format.NodeData{Inline: []byte("1")}, 0)
			b.AddNode(1, []byte("bbb"), format.NodeData{Inline: []byte("22")}, 0)
			sub := make([]byte, l.PageHdr+2*(format.NodeSize+4))
			s := format.PageBuilder{B: sub, L: l}
			s.Init(0, l.SubpageFlags)
			s.AddNode(0, []byte("x1"), format.NodeData{}, 0)
			s.AddNode(1, []byte("x2"), format.NodeData{}, 0)
			// Room in the sub-page, as mdb_cursor_put leaves it.
			sub = append(sub, make([]byte, 16)...)
			format.Put16(sub, l.UpperOffset(), uint16(len(sub)-l.PtrBase-2*(format.NodeSize+2)))
			b.AddNode(2, []byte("dups"), format.NodeData{Inline: sub}, format.FDupData)
			br := make([]byte, ps)
			b = format.PageBuilder{B: br, L: l}
			b.Init(3, format.PBranch)
			b.AddNode(0, nil, format.NodeData{IsChild: true, Child: 10}, 0)
			b.AddNode(1, []byte("mm"), format.NodeData{IsChild: true, Child: 11}, 0)
			l2 := make([]byte, ps)
			b = format.PageBuilder{B: l2, L: l}
			b.Init(4, format.PLeaf|format.PLeaf2)
			b.SetPad(4)
			b.AddLeaf2Key(0, []byte("aaaa"))
			b.AddLeaf2Key(1, []byte("bbbb"))
			for op := byte(0); op < 8; op++ {
				f.Add(byte(sel), op, uint16(1), []byte("key"), leaf)
				f.Add(byte(sel), op, uint16(0), []byte("k"), br)
				f.Add(byte(sel), op, uint16(1), []byte("cccc"), l2)
			}
			f.Add(byte(sel), byte(3), uint16(2), []byte{}, leaf)
			f.Add(byte(sel), byte(6), uint16(0), []byte{}, sub)
		}
	}
	f.Fuzz(func(t *testing.T, sel, op byte, idx uint16, key, page []byte) {
		l := fuzzEngineLayout(sel)
		if len(key) > 4096 {
			key = key[:4096]
		}
		p := slices.Clone(page)
		readAll(l, p, key)
		nkeys := numKeys(l, p)
		indx := int(idx)
		switch op % 8 {
		case 0: // mdb_node_add of a node: the caller checks the room first.
			kind := int(idx>>12) % 4
			if indx > nkeys {
				indx = nkeys
			}
			nodeSize := format.NodeSize + l.NodeKeyLen(len(key))
			body := nodeBody{kind: kind, pgno: uint64(len(key)) << 33}
			switch kind {
			case bodyInline:
				body.inline = key
				nodeSize += l.NodeDataLen(len(key))
			case bodyReserve:
				body.size = len(key)
				nodeSize += l.NodeDataLen(len(key))
			case bodyOverflow:
				body.size = 1 << 20
				body.ovref = format.OverflowRef{Pgno: 7, TxnID: 1, Pages: 3}
				nodeSize += l.OvpageSize
			}
			nodeSize = format.Even(nodeSize)
			if nodeSize <= sizeLeft(l, p)-2 && len(p) >= l.PageHdr {
				insertNode(l, p, indx, nodeSize, key, 0, body)
			}
		case 1: // a P_LEAF2 key of the DB's item size
			if len(key) > 0 && len(p) >= l.PageHdr {
				insertLeaf2(l, p, min(indx, nkeys), key, len(key))
			}
		case 2: // the item size of a P_LEAF2 page comes from a DB record
			ksize := int(pPad(l, p))
			if op&8 != 0 {
				ksize = int(idx >> 4)
			}
			if indx < nkeys {
				deleteNode(l, p, indx, ksize)
			}
		case 3:
			// A node whose data the sub-cursor parsed as a sub-page
			// (xcursorInit1) before the delete that shrinks it.
			if s, e, ok := nodeDataRange(l, p, indx); ok && e-s >= l.PageHdr {
				shrinkNode(l, p, indx, 99)
			}
		case 4:
			if indx < nkeys {
				updateKey(l, p, indx, key)
			}
		case 5:
			dst := make([]byte, len(p))
			copyPage(l, dst, p)
			readAll(l, dst, key)
			_ = copyUsed(l, dst, p, len(p))
		case 6: // grow a sub-page into a page or a bigger sub-page
			if len(p) < l.PageHdr {
				break
			}
			ps := 256 << (idx % 4)
			mp := make([]byte, ps)
			if buildSubpage(l, mp, p, len(p), pFlags(l, p), pPad(l, p), ps-len(p), format.PDirty) {
				readAll(l, mp, key)
			}
			grow := int(idx>>8) * 2
			mp = make([]byte, len(p)+grow)
			if buildSubpage(l, mp, p, len(p), pFlags(l, p), pPad(l, p), grow, l.SubpageDirty()) {
				readAll(l, mp, key)
			}
		case 7:
			for i := 0; i < min(nkeys, maxFuzzKeys); i++ {
				setNodeDSize(l, p, i, len(key))
				setNodePgno(l, p, i, 1<<40)
				setNodeOverflowRef(l, p, i, format.OverflowRef{Pgno: 3})
			}
		}
		readAll(l, p, key)
	})
}
