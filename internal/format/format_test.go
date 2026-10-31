package format

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"slices"
	"sort"
	"testing"
)

func TestLayoutNumbers(t *testing.T) {
	l := LayoutV09
	if l.PageHdr != 16 || l.PtrBase != 0 || l.EvenNodes || l.OvpageSize != 8 ||
		l.SubpageFlags != 0x52 || l.MaxPageSize != 0x8000 || l.DBNameNUL || l.LoosensRebalancedRoot() {
		t.Fatalf("V09 layout: %+v", l)
	}
	if l.PadOffset() != 8 || l.FlagsOffset() != 10 || l.LowerOffset() != 12 || l.UpperOffset() != 14 ||
		l.MetaOffset() != 16 {
		t.Fatal("V09 offsets")
	}
	if _, ok := l.TxnIDOffset(); ok {
		t.Fatal("V09 has no txnid")
	}
	check := func(name string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	check("v09 page space", l.PageSpace(4096), 4080)
	check("v09 nodemax 4k", l.NodeMax(4096), 2038)
	check("v09 nodemax 16k", l.NodeMax(16384), 8182)
	check("v09 maxfree", l.MaxFree1Pg(4096), 509)
	check("v09 maxkey", l.MaxKeySize(16384), 511)
	check("v09 ovpages 1", l.OverflowPagesFor(1, 4096), 1)
	check("v09 ovpages 4080", l.OverflowPagesFor(4080, 4096), 1)
	check("v09 ovpages 4081", l.OverflowPagesFor(4081, 4096), 2)
	check("v09 empty lower", l.EmptyLower(), 16)
	check("v09 numkeys 16", l.NumKeys(16), 0)
	check("v09 numkeys 22", l.NumKeys(22), 3)
	check("v09 numkeys 3", l.NumKeys(3), 0)
	check("v09 data offset", l.NodeDataOffset(3), 11)
	check("v09 leaf size", l.LeafSize(3, 5), 16)
	check("v09 branch size", l.BranchSize(5), 13)
	check("v09 stored", l.StoredDataLen(5000, true), 8)
	if l.SubpageDirty() != PDirty {
		t.Error("v09 subpage dirty")
	}

	l = LayoutV10
	if l.PageHdr != 24 || l.PtrBase != 24 || !l.EvenNodes || l.OvpageSize != 24 ||
		l.SubpageFlags != 0x42 || l.MaxPageSize != 0x10000 || !l.DBNameNUL || !l.LoosensRebalancedRoot() {
		t.Fatalf("V10 layout: %+v", l)
	}
	if off, ok := l.TxnIDOffset(); !ok || off != 8 {
		t.Fatal("V10 txnid offset")
	}
	if l.PadOffset() != 16 || l.FlagsOffset() != 18 || l.LowerOffset() != 20 || l.UpperOffset() != 22 ||
		l.MetaOffset() != 24 {
		t.Fatal("V10 offsets")
	}
	check("v10 page space", l.PageSpace(4096), 4072)
	check("v10 nodemax 4k", l.NodeMax(4096), 2034)
	check("v10 nodemax 16k", l.NodeMax(16384), 8178)
	check("v10 nodemax 64k", l.NodeMax(65536), 32754)
	check("v10 maxkey 4k", l.MaxKeySize(4096), 1978)
	check("v10 maxkey 16k", l.MaxKeySize(16384), 8122)
	check("v10 maxkey 64k", l.MaxKeySize(65536), 32698)
	check("v10 maxfree", l.MaxFree1Pg(4096), 508)
	check("v10 ovpages 4072", l.OverflowPagesFor(4072, 4096), 1)
	check("v10 ovpages 4073", l.OverflowPagesFor(4073, 4096), 2)
	check("v10 empty lower", l.EmptyLower(), 0)
	check("v10 numkeys 6", l.NumKeys(6), 3)
	check("v10 data offset 3", l.NodeDataOffset(3), 12)
	check("v10 data offset 4", l.NodeDataOffset(4), 12)
	check("v10 leaf size", l.LeafSize(3, 5), 8+4+6)
	check("v10 stored", l.StoredDataLen(5, false), 6)
	check("v10 stored big", l.StoredDataLen(5000, true), 24)
	if l.SubpageDirty() != 0 {
		t.Error("v10 subpage dirty")
	}
	if MetaTxnID+8 != MetaSize {
		t.Error("meta size")
	}
}

func TestFormatNames(t *testing.T) {
	for _, f := range []Format{V09, V10} {
		if g, ok := FormatFromName(f.String()); !ok || g != f {
			t.Errorf("round trip %v", f)
		}
	}
	for _, bad := range []string{"", "0.9 ", " 1.0", "1", "10", "1.0.2", "0.90", "V10"} {
		if _, ok := FormatFromName(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestOverflowRefs(t *testing.T) {
	r := OverflowRef{Pgno: 0x123456789abc, TxnID: 77, Pages: 3}
	buf := bytes.Repeat([]byte{0xff}, 32)
	if !LayoutV09.WriteOverflowRef(buf, r) {
		t.Fatal("write")
	}
	if !bytes.Equal(buf[8:], bytes.Repeat([]byte{0xff}, 24)) {
		t.Error("0.9 writes 8 bytes")
	}
	if got, _ := LayoutV09.ReadOverflowRef(buf); got != (OverflowRef{Pgno: r.Pgno}) {
		t.Errorf("v09 read %+v", got)
	}
	if _, ok := LayoutV09.ReadOverflowRef(buf[:7]); ok {
		t.Error("short read")
	}
	buf = bytes.Repeat([]byte{0xff}, 32)
	LayoutV10.WriteOverflowRef(buf, r)
	if got, _ := LayoutV10.ReadOverflowRef(buf); got != r {
		t.Errorf("v10 read %+v", got)
	}
	if _, ok := LayoutV10.ReadOverflowRef(buf[:23]); ok {
		t.Error("short v10 read")
	}
	page := bytes.Repeat([]byte{7}, 64)
	LayoutV09.SetTxnID(page, 42)
	if !bytes.Equal(page, bytes.Repeat([]byte{7}, 64)) {
		t.Error("0.9 pages have no txnid")
	}
	LayoutV10.SetTxnID(page, 42)
	if binary.NativeEndian.Uint64(page[8:]) != 42 {
		t.Error("v10 txnid")
	}
}

const ps = 4096

func leafPage(t *testing.T, l *Layout, entries [][2]string) []byte {
	t.Helper()
	buf := make([]byte, ps)
	b := PageBuilder{buf, l}
	b.Init(7, PLeaf)
	for i, e := range entries {
		if !b.AddNode(i, []byte(e[0]), NodeData{Inline: []byte(e[1])}, 0) {
			t.Fatal("add node")
		}
	}
	return buf
}

func TestLeafPageNodes(t *testing.T) {
	buf := leafPage(t, LayoutV09, [][2]string{{"a", "1"}, {"bb", "22"}, {"ccc", "333"}})
	p, _ := NewPage(buf, LayoutV09)
	if p.Pgno() != 7 || !p.IsLeaf() || p.IsBranch() || p.IsLeaf2() || p.NumKeys() != 3 || p.Lower() != 22 {
		t.Fatalf("header %d %v %d", p.Pgno(), p.Flags(), p.NumKeys())
	}
	if off, _ := p.NodeOffset(0); off != ps-10 {
		t.Errorf("node 0 at %d", off)
	}
	if off, _ := p.NodeOffset(1); off != ps-10-12 {
		t.Errorf("node 1 at %d", off)
	}
	for i, kv := range [][2]string{{"a", "1"}, {"bb", "22"}, {"ccc", "333"}} {
		n, ok := p.Node(i)
		if !ok {
			t.Fatal("node")
		}
		k, _ := n.Key()
		d, _ := n.InlineData()
		if string(k) != kv[0] || string(d) != kv[1] {
			t.Errorf("node %d: %q %q", i, k, d)
		}
	}
	if _, ok := p.Node(3); ok {
		t.Error("node 3")
	}
	// Insert in the middle keeps pointer order.
	buf = leafPage(t, LayoutV09, [][2]string{{"a", "1"}, {"c", "3"}})
	PageBuilder{buf, LayoutV09}.AddNode(1, []byte("b"), NodeData{Inline: []byte("2")}, 0)
	p, _ = NewPage(buf, LayoutV09)
	for i, want := range []string{"a", "b", "c"} {
		if k, _ := p.NodeKey(i); string(k) != want {
			t.Errorf("key %d %q", i, k)
		}
	}
}

func TestBranchAndLeaf2(t *testing.T) {
	buf := make([]byte, ps)
	b := PageBuilder{buf, LayoutV09}
	b.Init(3, PBranch)
	b.AddNode(0, nil, NodeData{IsChild: true, Child: 10}, 0)
	b.AddNode(1, []byte("m"), NodeData{IsChild: true, Child: 0x123456789abc}, 0)
	p, _ := NewPage(buf, LayoutV09)
	n0, _ := p.Node(0)
	n1, _ := p.Node(1)
	if !p.IsBranch() || n0.ChildPgno() != 10 || n1.ChildPgno() != 0x123456789abc {
		t.Fatal("branch")
	}
	if off, _ := p.NodeOffset(1); off != ps-8-10 {
		t.Error("branch offsets")
	}

	buf = make([]byte, ps)
	b = PageBuilder{buf, LayoutV09}
	b.Init(9, PLeaf|PLeaf2)
	b.SetPad(4)
	b.AddLeaf2Key(0, []byte("bbbb"))
	b.AddLeaf2Key(0, []byte("aaaa"))
	b.AddLeaf2Key(2, []byte("cccc"))
	p, _ = NewPage(buf, LayoutV09)
	if !p.IsLeaf2() || p.NumKeys() != 3 || p.Pad() != 4 || int(p.Upper()) != ps-3*2 {
		t.Fatal("leaf2 header")
	}
	if k, _ := p.Leaf2Key(2, 4); string(k) != "cccc" {
		t.Error("leaf2 key")
	}
	if _, ok := p.Leaf2Key(3, 4); ok {
		t.Error("leaf2 key 3")
	}
	if k, _ := p.Leaf2Keys(4); string(k) != "aaaabbbbcccc" {
		t.Error("leaf2 keys")
	}
}

func TestOverflowAndSubpage(t *testing.T) {
	for _, l := range []*Layout{LayoutV09, LayoutV10} {
		buf := make([]byte, ps)
		b := PageBuilder{buf, l}
		b.Init(2, PLeaf)
		b.AddNode(0, []byte("big"), NodeData{IsOverflow: true, OverflowPgno: 5, OverflowLen: 10000}, 0)
		p, _ := NewPage(buf, l)
		n, _ := p.Node(0)
		if n.Flags() != FBigData || n.DataSize() != 10000 {
			t.Fatal("bigdata node")
		}
		if pg, _ := n.OverflowPgno(); pg != 5 {
			t.Error("overflow pgno")
		}
		if raw, _ := n.Raw(true); len(raw) != l.NodeDataOffset(3)+l.OvpageSize {
			t.Errorf("raw %d", len(raw))
		}
		if l == LayoutV10 {
			if r, _ := n.OverflowRef(); r.Pages != 3 {
				t.Error("v10 pages")
			}
		}
		run := make([]byte, 3*ps)
		o := PageBuilder{run, l}
		o.InitOverflow(5, 3)
		op, _ := NewPage(run, l)
		if !op.IsOverflow() || op.OverflowPages() != 3 {
			t.Error("overflow run")
		}
		if d, ok := op.OverflowData(10000); !ok || len(d) != 10000 {
			t.Error("overflow data")
		}
		if _, ok := op.OverflowData(3 * ps); ok {
			t.Error("overflow data too long")
		}
	}
	// A P_SUBP sub-page as node data.
	sub := make([]byte, 16+3*(8+2+2))
	s := PageBuilder{sub, LayoutV09}
	s.Init(2, PLeaf|PSubp|PDirty)
	for i, d := range []string{"x1", "x2", "x3"} {
		s.AddNode(i, []byte(d), NodeData{Inline: []byte{}}, 0)
	}
	buf := make([]byte, ps)
	b := PageBuilder{buf, LayoutV09}
	b.Init(2, PLeaf)
	b.AddNode(0, []byte("key"), NodeData{Inline: sub}, FDupData)
	p, _ := NewPage(buf, LayoutV09)
	n, _ := p.Node(0)
	d, _ := n.InlineData()
	sp, _ := NewPage(d, LayoutV09)
	if !n.IsDupData() || n.IsSubData() || !sp.IsSubpage() || sp.NumKeys() != 3 {
		t.Fatal("subpage")
	}
	if k, _ := sp.NodeKey(1); string(k) != "x2" {
		t.Error("subpage key")
	}
}

func TestCorruptPointers(t *testing.T) {
	buf := leafPage(t, LayoutV09, [][2]string{{"a", "1"}})
	Put16(buf, 16, ps-2)
	p, _ := NewPage(buf, LayoutV09)
	if _, ok := p.Node(0); ok {
		t.Error("node past end")
	}
	small := make([]byte, 64)
	Put16(small, 12, 0xfff0)
	p, _ = NewPage(small, LayoutV09)
	if p.NumKeys() < 1000 {
		t.Error("numkeys")
	}
	if _, ok := p.Node(1000); ok {
		t.Error("node 1000")
	}
	if _, ok := p.Leaf2Key(1000, 8); ok {
		t.Error("leaf2 1000")
	}
	if _, ok := NewPage(small[:15], LayoutV09); ok {
		t.Error("short page")
	}
	// Short buffers built with MakePage read zeros.
	if MakePage(small[:3], LayoutV10).NumKeys() != 0 {
		t.Error("short MakePage")
	}
}

func TestV10Page(t *testing.T) {
	buf := make([]byte, ps)
	b := PageBuilder{buf, LayoutV10}
	b.Init(7, PLeaf)
	if b.Page().Lower() != 0 || int(b.Page().Upper()) != ps-24 {
		t.Fatal("v10 init")
	}
	b.AddNode(0, []byte("a"), NodeData{Inline: []byte("1")}, 0)
	b.AddNode(1, []byte("bbb"), NodeData{Inline: []byte("22")}, 0)
	p, _ := NewPage(buf, LayoutV10)
	if txn, ok := p.TxnID(); !ok || txn != 0 {
		t.Error("txnid")
	}
	if p.NumKeys() != 2 || p.Lower() != 4 {
		t.Fatal("v10 numkeys")
	}
	if off, _ := p.NodeOffset(0); off != ps-12 {
		t.Errorf("node 0 at %d", off)
	}
	if off, _ := p.NodeOffset(1); off != ps-12-14 {
		t.Errorf("node 1 at %d", off)
	}
	if v, _ := Get16(buf, 24); int(v) != ps-12-24 {
		t.Error("stored pointers are PAGEBASE-relative")
	}
	n, _ := p.Node(1)
	k, d, _ := n.KeyAndData()
	if string(k) != "bbb" || string(d) != "22" {
		t.Error("v10 node")
	}
	if p09, _ := NewPage(buf, LayoutV09); p09.NumKeys() != 0 {
		t.Error("v10 bytes as v09")
	}
}

func TestNodeV10Padding(t *testing.T) {
	nb := func(lo, hi, flags uint16, key, data []byte) []byte {
		v := make([]byte, NodeSize)
		Put16(v, nodeLo, lo)
		Put16(v, nodeHi, hi)
		Put16(v, nodeFlags, flags)
		Put16(v, nodeKSize, uint16(len(key)))
		return append(append(v, key...), data...)
	}
	b := append(nb(5, 0, 0, []byte("key"), nil), 0)
	b = append(b, "hello"...)
	n, _ := NewNode(b, LayoutV10)
	if d, _ := n.InlineData(); string(d) != "hello" {
		t.Error("v10 data")
	}
	n, _ = NewNode(b, LayoutV09)
	if d, _ := n.InlineData(); string(d) != "\x00hell" {
		t.Error("v09 data")
	}
	n, _ = NewNode(nb(0x3456, 0x9012, 0x0078, nil, nil), LayoutV09)
	if n.ChildPgno() != 0x007890123456 {
		t.Error("child pgno")
	}
	n, _ = NewNode(nb(2, 1, FBigData, []byte("k"), []byte{77, 0, 0, 0, 0, 0, 0, 0}), LayoutV09)
	if n.DataSize() != 0x10002 {
		t.Error("data size")
	}
	if _, ok := n.InlineData(); ok {
		t.Error("inline data past the end")
	}
	// KeyAndData agrees with Key and InlineData for every cut.
	for _, l := range []*Layout{LayoutV09, LayoutV10} {
		for klen := 0; klen < 6; klen++ {
			for dlen := uint16(0); dlen < 6; dlen++ {
				b := append(nb(dlen, 0, 0, []byte("abcde")[:klen], nil), "\x00vwxyz"...)
				for cut := NodeSize; cut <= len(b); cut++ {
					n, _ := NewNode(b[:cut], l)
					k1, ok1 := n.Key()
					d1, ok2 := n.InlineData()
					k2, d2, ok3 := n.KeyAndData()
					if (ok1 && ok2) != ok3 || ok3 && (!bytes.Equal(k1, k2) || !bytes.Equal(d1, d2)) {
						t.Fatalf("%d %d %d", klen, dlen, cut)
					}
				}
			}
		}
	}
}

func TestMeta(t *testing.T) {
	m := NewEnvMeta(LayoutV09, 4096, 1<<20, FixedMap)
	if m.EnvFlags() != 0x09 || m.PageSize() != 4096 {
		t.Fatal("new env meta")
	}
	buf := bytes.Repeat([]byte{0xff}, 8192)
	m.WriteInitialPages(buf, 4096, LayoutV09)
	for i := 0; i < 2; i++ {
		page := buf[i*4096 : (i+1)*4096]
		if v, _ := Get64(page, 0); v != uint64(i) {
			t.Error("pgno")
		}
		got, err := ReadMetaPage(page, LayoutV09)
		if err != nil || got != m || got.DBs[1].Root != PInvalid || got.LastPgno != 1 {
			t.Fatalf("meta %d: %v", i, err)
		}
	}
	bad := slices.Clone(buf[:4096])
	bad[10] = 2
	if _, err := ReadMetaPage(bad, LayoutV09); err != Invalid {
		t.Error("not a meta page")
	}
	bad = slices.Clone(buf[:4096])
	bad[16] ^= 1
	if _, err := ReadMetaPage(bad, LayoutV09); err != Invalid {
		t.Error("bad magic")
	}
	bad = slices.Clone(buf[:4096])
	bad[20] = 2
	if _, err := ReadMetaPage(bad, LayoutV09); err != VersionMismatch {
		t.Error("bad version")
	}

	m10 := NewEnvMeta(LayoutV10, 4096, 1<<20, 0)
	v10 := make([]byte, 8192)
	m10.WriteInitialPages(v10, 4096, LayoutV10)
	if f, _ := Get16(v10, 18); f != PMeta {
		t.Error("v10 flags")
	}
	if got, err := ReadMetaPage(v10[:4096], LayoutV10); err != nil || got != m10 {
		t.Error("v10 meta")
	}
	if _, err := ReadMetaPage(v10[:4096], LayoutV09); err != Invalid {
		t.Error("v10 as v09")
	}
	if _, err := ReadMetaPage(buf[:4096], LayoutV10); err != Invalid {
		t.Error("v09 as v10")
	}

	// Sniffing.
	if f, ok, err := SniffFormat(buf[:SniffLen]); !ok || err != nil || f != V09 {
		t.Error("sniff v09")
	}
	if f, ok, err := SniffFormat(v10[:SniffLen]); !ok || err != nil || f != V10 {
		t.Error("sniff v10")
	}
	if _, ok, err := SniffFormat(nil); ok || err != nil {
		t.Error("sniff empty")
	}
	if _, _, err := SniffFormat(buf[:20]); err != Invalid {
		t.Error("sniff short")
	}
	if _, _, err := SniffFormat(make([]byte, SniffLen)); err != Invalid {
		t.Error("sniff zeros")
	}
	b := slices.Clone(buf[:SniffLen])
	Put32(b, 20, 3)
	if _, _, err := SniffFormat(b); err != Invalid {
		t.Error("0.9 shape with version 3")
	}
	b = slices.Clone(v10[:SniffLen])
	Put32(b, 28, 1)
	if _, _, err := SniffFormat(b); err != Invalid {
		t.Error("1.0 shape with version 1")
	}
	for _, at := range []int{20, 28} {
		src := buf
		if at == 28 {
			src = v10
		}
		b := slices.Clone(src[:SniffLen])
		Put32(b, at, 2)
		if _, _, err := SniffFormat(b); err != VersionMismatch {
			t.Error("prerelease version")
		}
	}
	a, c := m, m
	a.TxnID, c.TxnID = 7, 8
	if PickNewest(&[2]Meta{a, c}) != 1 || PickNewest(&[2]Meta{c, a}) != 0 || PickNewest(&[2]Meta{a, a}) != 0 {
		t.Error("pick newest")
	}
}

func TestComparators(t *testing.T) {
	if Memn.Compare([]byte("abc"), []byte("abd")) >= 0 || Memn.Compare([]byte("ab"), []byte("abc")) >= 0 ||
		Memn.Compare([]byte("abc"), []byte("ab")) <= 0 || Memn.Compare([]byte{0xff}, []byte{1, 0}) <= 0 {
		t.Error("memn")
	}
	if CmpMemnrBytes([]byte("xa"), []byte("ab")) >= 0 || CmpMemnrBytes([]byte("ba"), []byte("aa")) <= 0 ||
		CmpMemnrBytes([]byte("bc"), []byte("abc")) >= 0 || CmpMemnrBytes([]byte("abc"), []byte("bc")) <= 0 ||
		CmpMemnrBytes([]byte("zzzb"), []byte("a")) <= 0 || CmpMemnrBytes([]byte("abc"), []byte("abc")) != 0 {
		t.Error("memnr")
	}
	k := func(v uint32) []byte { return binary.NativeEndian.AppendUint32(nil, v) }
	q := func(v uint64) []byte { return binary.NativeEndian.AppendUint64(nil, v) }
	if Int.Compare(k(1), k(256)) >= 0 || Cint.Compare(k(1), k(256)) >= 0 ||
		Cint.Compare(k(0x10000), k(0xffff)) <= 0 || Long.Compare(q(1<<40), q(1<<39)) <= 0 ||
		Cint.Compare(q(1<<40), q(1<<39)) <= 0 || Cint.Compare(q(^uint64(0)), q(^uint64(0))) != 0 {
		t.Error("integer comparators")
	}
	if Clong.Compare(q(1<<33), q(1)) <= 0 || Clong.Compare(q(5), q(5)) != 0 {
		t.Error("clong")
	}
	if littleEndian {
		// The little-endian C loop, literally.
		cle := func(a, b []byte) int {
			w := func(buf []byte, i int) int { return int(binary.LittleEndian.Uint16(buf[2*i:])) }
			i := len(a) / 2
			for {
				i--
				x := w(a, i) - w(b, i)
				if x != 0 || i == 0 {
					return x
				}
			}
		}
		sign := func(x int) int {
			switch {
			case x < 0:
				return -1
			case x > 0:
				return 1
			}
			return 0
		}
		for _, c := range [][2][]byte{
			{{1, 0, 0, 0, 0, 0}, {0, 0, 0, 0, 0, 1}},
			{{0, 0, 0, 0, 0, 1}, {1, 0, 0, 0, 0, 0}},
			{{9, 9, 2, 0, 7, 7}, {1, 1, 3, 0, 7, 7}},
			{{5, 0}, {4, 0}},
			{{3, 4, 5, 6, 7, 8}, {3, 4, 5, 6, 7, 8}},
		} {
			if CmpCintBytes(c[0], c[1]) != sign(cle(c[0], c[1])) {
				t.Errorf("cint %v %v", c[0], c[1])
			}
		}
		if CmpCintBytes([]byte{1, 0}, []byte{1, 0, 9, 9}) != 0 {
			t.Error("cint b longer")
		}
		if CmpCintBytes([]byte{0, 0, 0, 0, 0, 1}, []byte{5, 0}) <= 0 {
			t.Error("cint b shorter")
		}
		tail := []byte{5, 0, 0, 0, 0, 7}
		if Cint.CompareInPage([]byte{0, 0, 0, 0, 0, 1}, tail[:2], tail) >= 0 {
			t.Error("cint in page")
		}
		if Memn.CompareInPage([]byte("ab"), []byte("a"), []byte("abc")) <= 0 {
			t.Error("memn in page")
		}
		_ = CmpCintBytes([]byte{1, 2, 3}, []byte{1, 2, 4})
		_ = CmpCintBytes(nil, nil)
	}
	if DefaultKeyCmp(0).Kind != CmpMemn || DefaultKeyCmp(IntegerKey).Kind != CmpCint || DefaultKeyCmp(ReverseKey|IntegerKey).Kind != CmpMemnr {
		t.Error("default key")
	}
	if _, ok := DefaultDupCmp(IntegerDup); ok {
		t.Error("dup without dupsort")
	}
	for flags, want := range map[uint32]Comparator{
		DupSort:                           Memn,
		DupSort | IntegerDup:              Cint,
		DupSort | IntegerDup | DupFixed:   Int,
		DupSort | ReverseDup:              Memnr,
		DupSort | ReverseDup | IntegerDup: Cint,
	} {
		if got, _ := DefaultDupCmp(flags); got.Kind != want.Kind {
			t.Errorf("dup %#x: %v", flags, got)
		}
	}
}

func TestIDL(t *testing.T) {
	ids := IDL{20, 15, 10, 9, 3}
	for id := uint64(0); id < 25; id++ {
		// mdb_midl_search, literally (1-based).
		c := append([]uint64{uint64(len(ids))}, ids...)
		base, cursor, val := 0, 1, 0
		n := int(c[0])
		for n > 0 {
			pivot := n >> 1
			cursor = base + pivot + 1
			switch {
			case c[cursor] < id:
				val = -1
			case c[cursor] == id:
				val = 0
			default:
				val = 1
			}
			if val < 0 {
				n = pivot
			} else if val > 0 {
				base = cursor
				n -= pivot + 1
			} else {
				break
			}
		}
		if val > 0 {
			cursor++
		}
		if ids.Search(id)+1 != cursor {
			t.Errorf("search %d", id)
		}
	}
	if !ids.Contains(10) || ids.Contains(11) || (IDL{}).Search(5) != 0 {
		t.Error("contains")
	}
	l := IDL{100}
	l.AppendRange(10, 3)
	if !slices.Equal(l, IDL{100, 12, 11, 10}) || !l.IsSortedDesc() {
		t.Error("append range")
	}
	rng := rand.New(rand.NewSource(1))
	for la := 0; la < 7; la++ {
		for lb := 0; lb < 7; lb++ {
			for r := 0; r < 20; r++ {
				list := func(n int) IDL {
					v := make(IDL, n)
					for i := range v {
						v[i] = uint64(rng.Intn(16))
					}
					v.Sort()
					return v
				}
				a, b := list(la), list(lb)
				want := append(slices.Clone(a), b...)
				want.Sort()
				got := slices.Clone(a)
				got.XMerge(b)
				if !slices.Equal(got, want) {
					t.Fatalf("xmerge %v %v", a, b)
				}
				enc := slices.Clone(a)
				rec := b.Bytes()
				if !enc.XMergeEncoded(rec) || !slices.Equal(enc, want) {
					t.Fatal("xmerge encoded")
				}
				short := slices.Clone(a)
				if len(b) > 0 && (short.XMergeEncoded(rec[:len(rec)-1]) || !slices.Equal(short, a)) {
					t.Fatal("short record")
				}
			}
		}
	}
	// Runs against the literal C loop of mdb_page_alloc.
	cRun := func(mop []uint64, num int) ([]uint64, uint64, bool) {
		n2 := num - 1
		mopLen := int(mop[0])
		if mopLen <= n2 {
			return mop, 0, false
		}
		i := mopLen
		var pgno uint64
		for {
			p := mop[i]
			if mop[i-n2] == p+uint64(n2) {
				pgno = p
				break
			}
			i--
			if i <= n2 {
				return mop, 0, false
			}
		}
		mopLen -= num
		mop[0] = uint64(mopLen)
		j := i - num
		for j < mopLen {
			j++
			i++
			mop[j] = mop[i]
		}
		return mop[:mopLen+1], pgno, true
	}
	for round := 0; round < 300; round++ {
		var ids IDL
		for i := 0; i < 1+round%60; i++ {
			ids = append(ids, 3+uint64(rng.Intn(200)))
		}
		ids.Sort()
		ids = slices.Compact(ids)
		for num := 1; num < 6; num++ {
			idl := slices.Clone(ids)
			c := append([]uint64{uint64(len(ids))}, ids...)
			i, ok := idl.FindRun(num)
			var got uint64
			if ok {
				got = idl.RemoveRun(i, num)
			}
			c, want, cok := cRun(c, num)
			if ok != cok || got != want || !slices.Equal(idl, IDL(c[1:])) {
				t.Fatalf("run %v num %d", ids, num)
			}
			if ok {
				idl.InsertRun(got, num)
				if !slices.Equal(idl, ids) {
					t.Fatal("insert run")
				}
			}
		}
	}
	enc := IDL{9, 7, 3}.Bytes()
	if len(enc) != 32 {
		t.Fatal("encoding length")
	}
	if dec, ok := DecodeIDL(enc); !ok || !slices.Equal(dec, IDL{9, 7, 3}) {
		t.Error("decode")
	}
	if _, ok := DecodeIDL(enc[:31]); ok {
		t.Error("short decode")
	}
	Put64(enc, 0, ^uint64(0))
	if _, ok := DecodeIDL(enc); ok {
		t.Error("huge count")
	}
	if !sort.SliceIsSorted([]int{1}, func(i, j int) bool { return false }) {
		t.Error("unreachable")
	}
}

func TestStrerror(t *testing.T) {
	if NotFound.Error() != "MDB_NOTFOUND: No matching key/data pair found" ||
		BadDBI.Error() != "MDB_BAD_DBI: The specified DBI handle was closed/changed unexpectedly" ||
		EnvEncryption.Error() != "MDB_ENV_ENCRYPTION: Environment encryption mismatch" ||
		Strerror(int(AddrBusy)) != "MDB_ADDR_BUSY: Requested map address is unavailable" ||
		Strerror(int(AddrBusy)+1) != "Invalid error code" || Strerror(0) != "Successful return: 0" ||
		Strerror(-1) != "Invalid error code" {
		t.Error("strerror")
	}
	for c := int(KeyExist); c <= int(LastErrCode10); c++ {
		if Code(FromCode(c)) != c {
			t.Errorf("round trip %d", c)
		}
	}
	if FormatReaderList(nil) != "(no active readers)\n" ||
		FormatReaderList([]ReaderInfo{{PID: 12, Thread: 0xab, TxnID: 7, InTxn: true}, {PID: 3, Thread: 1}}) !=
			"    pid     thread     txnid\n        12 ab 7\n         3 1 -\n" {
		t.Error("reader list")
	}
}
