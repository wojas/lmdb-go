package engine

// Unit tests of the P_LEAF2 bookkeeping of pageops.go (rlmdb
// core/page_ops.rs tests leaf2_one_byte_items and
// leaf2_upper_wraps_on_64k_pages).

import (
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// newLeaf2 is an empty P_LEAF2 page of ps bytes.
func newLeaf2(l *format.Layout, ps int) []byte {
	p := make([]byte, ps)
	format.PageBuilder{B: p, L: l}.Init(3, format.PLeaf|format.PLeaf2)
	return p
}

// TestLeaf2OneByteItems: C's unsigned mp_upper -= ksize - sizeof(indx_t)
// moves mp_upper up by one per 1-byte item, past the page end, while
// SIZELEFT still drops by one; deleting moves it back.
func TestLeaf2OneByteItems(t *testing.T) {
	const ps = 4096
	for _, l := range []*format.Layout{format.LayoutV09, format.LayoutV10} {
		p := newLeaf2(l, ps)
		free := sizeLeft(l, p)
		for i, k := range []byte("bdac") {
			at := []int{0, 1, 0, 2}[i]
			if !insertLeaf2(l, p, at, []byte{k}, 1) {
				t.Fatalf("%v: insert %d", l.Format, i)
			}
		}
		if got := string(p[l.PageHdr : l.PageHdr+4]); got != "abcd" {
			t.Fatalf("%v: items %q", l.Format, got)
		}
		if numKeys(l, p) != 4 || pUpper(l, p) != ps+4 || sizeLeft(l, p) != free-4 {
			t.Fatalf("%v: keys %d upper %d free %d", l.Format, numKeys(l, p), pUpper(l, p), sizeLeft(l, p))
		}
		if !deleteNode(l, p, 0, 1) {
			t.Fatalf("%v: delete", l.Format)
		}
		if got := string(p[l.PageHdr : l.PageHdr+3]); got != "bcd" || numKeys(l, p) != 3 || pUpper(l, p) != ps+3 {
			t.Fatalf("%v: after delete %q keys %d upper %d", l.Format, got, numKeys(l, p), pUpper(l, p))
		}
		// 2-byte items leave mp_upper alone.
		p = newLeaf2(l, ps)
		if !insertLeaf2(l, p, 0, []byte("zz"), 2) || numKeys(l, p) != 1 || pUpper(l, p) != ps {
			t.Fatalf("%v: 2-byte item: keys %d upper %d", l.Format, numKeys(l, p), pUpper(l, p))
		}
	}
}

// TestLeaf2UpperWraps64K: on 1.0's 64 KiB pages the stored mp_upper of a
// P_LEAF2 page of 1-byte items starts at 65,512 and wraps to 0 at the
// 24th item; SIZELEFT is the 16-bit difference, as C's, and stays exact.
// On other pages an mp_upper below mp_lower is damage: no room.
func TestLeaf2UpperWraps64K(t *testing.T) {
	const ps = 0x10000
	l := format.LayoutV10
	p := newLeaf2(l, ps)
	for n := 1; n <= 100; n++ {
		if !insertLeaf2(l, p, n-1, []byte{byte(n)}, 1) {
			t.Fatalf("insert %d", n)
		}
		free := ps - l.PageHdr - n
		if sizeLeft(l, p) != free || format.MakePage(p, l).SizeLeft() != free {
			t.Fatalf("%d items: free %d/%d, want %d", n, sizeLeft(l, p), format.MakePage(p, l).SizeLeft(), free)
		}
	}
	if up := hdr16(p, l.UpperOffset()); up != 100-24 {
		t.Fatalf("stored mp_upper %d, want it wrapped to %d", up, 100-24)
	}
	if numKeys(l, p) != 100 {
		t.Fatalf("keys %d", numKeys(l, p))
	}
	// The compacting copy keeps the items, not the wrapped "free" tail.
	dst := make([]byte, ps)
	if err := copyUsed(l, dst, p, ps); err != nil || string(dst[:l.PageHdr+100]) != string(p[:l.PageHdr+100]) {
		t.Fatalf("copyUsed: %v", err)
	}

	br := make([]byte, ps)
	format.PageBuilder{B: br, L: l}.Init(4, format.PBranch)
	format.Put16(br, l.LowerOffset(), 48)
	format.Put16(br, l.UpperOffset(), 0)
	if sizeLeft(l, br) != 0 || format.MakePage(br, l).SizeLeft() != 0 {
		t.Fatalf("branch page with mp_upper < mp_lower: free %d/%d", sizeLeft(l, br), format.MakePage(br, l).SizeLeft())
	}
}
