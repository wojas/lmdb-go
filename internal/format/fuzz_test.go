package format

// Fuzz targets for the parsers of on-disk structures (go test -fuzz, see
// FUZZING.md at the repository root): pages of both formats and every
// page kind (branch, leaf, P_LEAF2, overflow, P_SUBP sub-pages inside leaf
// nodes, meta), nodes, MDB_meta and MDB_db records, free-list records
// (IDLs) and the comparators that order keys read from pages. The input
// is arbitrary bytes; the accessors must never panic or read out of range
// (rule 4 of AGENTS.md), and what they return must be consistent: slices
// as long as the header fields they decode claim, the same answer from
// the fast and the general accessor, encodings that round-trip. A normal
// go test runs only the seeds below and testdata/fuzz.

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

// fuzzLayout picks the layout from a selector byte.
func fuzzLayout(sel byte) *Layout {
	if sel&1 == 0 {
		return LayoutV09
	}
	return LayoutV10
}

type pageSeed struct {
	sel  byte
	page []byte
}

// fuzzPageSeeds are well-formed pages of every kind for both layouts and a
// few page sizes.
func fuzzPageSeeds() []pageSeed {
	var out []pageSeed
	for sel, l := range []*Layout{LayoutV09, LayoutV10} {
		for _, ps := range []int{256, 1024, 4096} {
			// Leaf with inline, empty, big-data and sub-DB nodes.
			leaf := make([]byte, ps)
			b := PageBuilder{leaf, l}
			b.Init(5, PLeaf)
			b.AddNode(0, []byte("a"), NodeData{Inline: []byte("1")}, 0)
			b.AddNode(1, []byte("bb"), NodeData{Inline: nil}, 0)
			b.AddNode(2, []byte("big"), NodeData{IsOverflow: true, OverflowPgno: 9, OverflowLen: 3 * ps}, 0)
			db := DB{Flags: uint16(DupSort), Depth: 1, LeafPages: 1, Entries: 3, Root: 7}
			rec := db.Bytes()
			b.AddNode(3, []byte("sub"), NodeData{Inline: rec[:]}, FSubData)
			// A sub-page of duplicates as the data of a node.
			sub := make([]byte, l.PageHdr+3*(NodeSize+4))
			s := PageBuilder{sub, l}
			s.Init(0, l.SubpageFlags)
			for i, d := range []string{"x1", "x2", "x3"} {
				s.AddNode(i, []byte(d), NodeData{}, 0)
			}
			b.AddNode(4, []byte("dups"), NodeData{Inline: sub}, FDupData)
			out = append(out, pageSeed{byte(sel), leaf})

			// Branch.
			br := make([]byte, ps)
			b = PageBuilder{br, l}
			b.Init(3, PBranch)
			b.AddNode(0, nil, NodeData{IsChild: true, Child: 10}, 0)
			b.AddNode(1, []byte("m"), NodeData{IsChild: true, Child: 0x123456789a}, 0)
			out = append(out, pageSeed{byte(sel), br})

			// P_LEAF2.
			l2 := make([]byte, ps)
			b = PageBuilder{l2, l}
			b.Init(4, PLeaf|PLeaf2)
			b.SetPad(4)
			for i, k := range []string{"aaaa", "bbbb", "cccc"} {
				b.AddLeaf2Key(i, []byte(k))
			}
			out = append(out, pageSeed{byte(sel), l2})

			// Overflow run of two pages.
			ov := make([]byte, 2*ps)
			b = PageBuilder{ov, l}
			b.InitOverflow(9, 2)
			out = append(out, pageSeed{byte(sel), ov})

			// Meta page.
			meta := make([]byte, 2*ps)
			m := NewEnvMeta(l, uint32(ps), 1<<20, 0)
			m.WriteInitialPages(meta, ps, l)
			out = append(out, pageSeed{byte(sel), meta[:ps]})
		}
	}
	return out
}

// FuzzPage parses one page (or any buffer as a page) with every accessor
// of Page and Node, recursing into sub-pages.
func FuzzPage(f *testing.F) {
	for _, s := range fuzzPageSeeds() {
		f.Add(s.sel, s.page)
	}
	f.Fuzz(func(t *testing.T, sel byte, data []byte) {
		checkPage(t, fuzzLayout(sel), data, 0)
	})
}

// maxFuzzIndex bounds the indexes tried per page (NUMKEYS can claim 32K).
const maxFuzzIndex = 2048

func checkPage(t *testing.T, l *Layout, b []byte, depth int) {
	if _, ok := NewPage(b, l); ok != (len(b) >= l.PageHdr) {
		t.Fatalf("NewPage of %d bytes: ok %v", len(b), ok)
	}
	p := MakePage(b, l)
	_ = p.Pgno()
	_, _ = p.TxnID()
	_, _, _ = p.Pad(), p.Flags(), p.OverflowPages()
	_, _, _, _, _, _ = p.IsBranch(), p.IsLeaf(), p.IsLeaf2(), p.IsOverflow(), p.IsSubpage(), p.IsMeta()
	n := p.NumKeys()
	if n < 0 || n > 0xffff/2 {
		t.Fatalf("NumKeys %d", n)
	}
	if sl := p.SizeLeft(); sl < 0 || sl > 0xffff {
		t.Fatalf("SizeLeft %d", sl)
	}
	upto := min(n+1, maxFuzzIndex)
	for i := -1; i <= upto; i++ {
		off, okOff := p.NodeOffset(i)
		if okOff && (i < 0 || i >= n || off < l.PtrBase) {
			t.Fatalf("NodeOffset(%d) = %d of %d keys", i, off, n)
		}
		node, okNode := p.Node(i)
		key, okKey := p.NodeKey(i)
		if okNode {
			if !okOff || len(node.B) != len(b)-off || len(node.B) < NodeSize {
				t.Fatalf("Node(%d): %d bytes at %d (ok %v) of %d", i, len(node.B), off, okOff, len(b))
			}
			k2, ok2 := node.Key()
			if ok2 != okKey || !bytes.Equal(k2, key) {
				t.Fatalf("NodeKey(%d) %q %v, Node.Key %q %v", i, key, okKey, k2, ok2)
			}
			checkNode(t, l, node, depth)
		} else if okKey {
			t.Fatalf("NodeKey(%d) ok without a node", i)
		}
	}
	for _, ks := range []int{int(p.Pad()), 0, 1, 8, 1 << 20, -1} {
		keys, okKeys := p.Leaf2Keys(ks)
		if okKeys && len(keys) != n*ks {
			t.Fatalf("Leaf2Keys(%d): %d bytes for %d keys", ks, len(keys), n)
		}
		for i := -1; i <= min(upto, 64); i++ {
			k, ok := p.Leaf2Key(i, ks)
			tail, okTail := p.Leaf2KeyTail(i, ks)
			if ok && (len(k) != ks || !okTail || !bytes.HasPrefix(tail, k)) {
				t.Fatalf("Leaf2Key(%d, %d): %d bytes, tail ok %v", i, ks, len(k), okTail)
			}
			if ok && okKeys && !bytes.Equal(k, keys[i*ks:(i+1)*ks]) {
				t.Fatalf("Leaf2Key(%d, %d) is not in Leaf2Keys", i, ks)
			}
		}
	}
	for _, nn := range []int{-1, 0, 1, len(b) - l.PageHdr, len(b) - l.PageHdr + 1, int(p.OverflowPages())} {
		d, ok := p.OverflowData(nn)
		want := nn >= 0 && l.PageHdr+nn <= len(b)
		if ok != want || (ok && len(d) != nn) {
			t.Fatalf("OverflowData(%d) of %d bytes: %d bytes, ok %v", nn, len(b), len(d), ok)
		}
	}
	if p.IsMeta() {
		if m, err := ReadMetaPage(b, l); err == nil && m.Validate(l) != nil {
			t.Fatal("ReadMetaPage accepted an invalid meta")
		}
	}
}

func checkNode(t *testing.T, l *Layout, node Node, depth int) {
	_, _, _ = node.Lo(), node.Hi(), node.ChildPgno()
	_, _, _ = node.IsBigData(), node.IsSubData(), node.IsDupData()
	ksize, dsize := node.KeySize(), node.DataSize()
	if ksize < 0 || dsize < 0 {
		t.Fatalf("sizes %d %d", ksize, dsize)
	}
	k, okKey := node.Key()
	if okKey && (len(k) != ksize || !bytes.HasPrefix(node.KeyTail(), k)) {
		t.Fatalf("Key: %d bytes for ksize %d", len(k), ksize)
	}
	off := node.DataOffset()
	if off < NodeSize+ksize || off > NodeSize+ksize+1 {
		t.Fatalf("DataOffset %d for ksize %d", off, ksize)
	}
	tail := node.DataTail()
	d, okData := node.InlineData()
	if okData && (len(d) != dsize || !bytes.HasPrefix(tail, d)) {
		t.Fatalf("InlineData: %d bytes for dsize %d", len(d), dsize)
	}
	k2, d2, ok := node.KeyAndData()
	if ok != okData || (ok && (!okKey || !bytes.Equal(k2, k) || !bytes.Equal(d2, d))) {
		t.Fatalf("KeyAndData disagrees with Key/InlineData (ok %v %v %v)", ok, okKey, okData)
	}
	r, okRef := node.OverflowRef()
	if want := off <= len(node.B) && len(node.B)-off >= l.OvpageSize; okRef != want {
		t.Fatalf("OverflowRef ok %v, want %v", okRef, want)
	}
	if pg, okPg := node.OverflowPgno(); okPg != okRef || pg != r.Pgno {
		t.Fatal("OverflowPgno disagrees with OverflowRef")
	}
	for _, isLeaf := range []bool{false, true} {
		raw, ok := node.Raw(isLeaf)
		want := off
		switch {
		case isLeaf && node.IsBigData():
			want += l.OvpageSize
		case isLeaf:
			want += dsize // without the padding
		}
		if ok != (want <= len(node.B)) || (ok && len(raw) != want) {
			t.Fatalf("Raw(%v): %d bytes, want %d of %d", isLeaf, len(raw), want, len(node.B))
		}
	}
	if !okData {
		return
	}
	if node.IsSubData() {
		db, ok := ReadDB(d)
		if ok != (len(d) >= DBSize) {
			t.Fatalf("ReadDB of %d bytes: %v", len(d), ok)
		}
		if ok {
			if rec := db.Bytes(); !bytes.Equal(rec[:], d[:DBSize]) {
				t.Fatal("MDB_db does not round-trip")
			}
		}
	} else if node.IsDupData() && depth < 2 {
		// A P_SUBP sub-page has the layout of the page it is in.
		checkPage(t, l, d, depth+1)
	}
}

// FuzzMeta decodes meta pages, MDB_db records and the format sniffer's
// view of a file head.
func FuzzMeta(f *testing.F) {
	for _, l := range []*Layout{LayoutV09, LayoutV10} {
		for _, ps := range []int{512, 4096} {
			buf := make([]byte, 2*ps)
			m := NewEnvMeta(l, uint32(ps), 1<<20, 0)
			m.TxnID, m.LastPgno = 7, 40
			m.DBs[1] = DB{Depth: 2, BranchPages: 1, LeafPages: 5, Entries: 99, Root: 39}
			m.WriteInitialPages(buf, ps, l)
			f.Add(buf[:l.MetaOffset()+MetaSize+8])
		}
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, l := range []*Layout{LayoutV09, LayoutV10} {
			m, ok := ReadMeta(data)
			if ok != (len(data) >= MetaSize) {
				t.Fatalf("ReadMeta of %d bytes: %v", len(data), ok)
			}
			if ok {
				var buf [MetaSize]byte
				m.Write(buf[:])
				if !bytes.Equal(buf[:], data[:MetaSize]) {
					t.Fatal("MDB_meta does not round-trip")
				}
				_, _ = m.PageSize(), m.EnvFlags()
				_ = m.Validate(l)
			}
			mp, err := ReadMetaPage(data, l)
			if err == nil {
				if mp.Validate(l) != nil {
					t.Fatal("ReadMetaPage accepted an invalid meta")
				}
				want, _ := ReadMeta(data[l.MetaOffset():])
				if mp != want {
					t.Fatal("ReadMetaPage disagrees with ReadMeta")
				}
				if fl, _ := Get16(data, l.FlagsOffset()); fl&PMeta == 0 {
					t.Fatal("ReadMetaPage accepted a page without P_META")
				}
			} else if err != Invalid && err != VersionMismatch {
				t.Fatalf("ReadMetaPage: %v", err)
			}
		}
		db, ok := ReadDB(data)
		if ok != (len(data) >= DBSize) {
			t.Fatalf("ReadDB of %d bytes: %v", len(data), ok)
		}
		if ok {
			if rec := db.Bytes(); !bytes.Equal(rec[:], data[:DBSize]) {
				t.Fatal("MDB_db does not round-trip")
			}
		}
		fm, ok, err := SniffFormat(data)
		switch {
		case len(data) == 0:
			if ok || err != nil {
				t.Fatal("SniffFormat of nothing")
			}
		case ok:
			l := fm.Layout()
			if err != nil || l == nil {
				t.Fatalf("SniffFormat: %v %v", fm, err)
			}
			if v, _ := Get32(data, l.MetaOffset()); v != Magic {
				t.Fatal("SniffFormat without the magic")
			}
		case err != Invalid && err != VersionMismatch:
			t.Fatalf("SniffFormat: %v", err)
		}
		if len(data) >= 2*MetaSize {
			var metas [2]Meta
			metas[0], _ = ReadMeta(data)
			metas[1], _ = ReadMeta(data[MetaSize:])
			i := PickNewest(&metas)
			if i < 0 || i > 1 || metas[i].TxnID < metas[1-i].TxnID {
				t.Fatalf("PickNewest %d", i)
			}
		}
	})
}

// FuzzIDL decodes free-DB records and runs the ID-list operations the
// free list applies to them: merges (with sorted and unsorted input, as a
// damaged file may hold), searches, run searches and removal.
func FuzzIDL(f *testing.F) {
	f.Add(IDL{9, 8, 7, 5, 4, 3, 2}.Bytes(), IDL{10, 6, 1}.Bytes(), uint8(3))
	f.Add(IDL{}.Bytes(), IDL{5}.Bytes(), uint8(1))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, []byte{}, uint8(0))
	f.Fuzz(func(t *testing.T, data, other []byte, num uint8) {
		ids, okIDs := EncodedIDs(data)
		cnt, okLen := EncodedLen(data)
		l, ok := DecodeIDL(data)
		if ok != okIDs || okLen != (len(data) >= 8) || (ok && !okLen) {
			t.Fatalf("decode ok %v, ids ok %v, len ok %v", ok, okIDs, okLen)
		}
		if !ok {
			var m IDL
			if m.XMergeEncoded(data) || len(m) != 0 {
				t.Fatal("XMergeEncoded merged a short record")
			}
			return
		}
		if uint64(len(l)) != cnt || len(ids) != 8*len(l) {
			t.Fatalf("%d IDs, count %d, %d bytes", len(l), cnt, len(ids))
		}
		if enc := l.Bytes(); !bytes.Equal(enc, data[:len(enc)]) {
			t.Fatal("IDL does not round-trip")
		}
		if l.EncodeInto(make([]byte, 8*len(l))) {
			t.Fatal("EncodeInto accepted a wrong length")
		}
		base, _ := DecodeIDL(other)
		base.Sort()

		// XMergeEncoded keeps every ID; with a sorted record the result is
		// sorted (C's precondition), with any record it stays in bounds.
		m := slices.Clone(base)
		if !m.XMergeEncoded(data) || len(m) != len(base)+len(l) {
			t.Fatalf("XMergeEncoded: %d IDs from %d + %d", len(m), len(base), len(l))
		}
		sorted := slices.Clone(l)
		sorted.Sort()
		m2 := slices.Clone(base)
		m2.XMerge(sorted)
		want := append(slices.Clone(base), l...)
		want.Sort()
		if !slices.Equal(m2, want) {
			t.Fatal("XMerge of sorted lists is not their sorted union")
		}
		if slices.Equal(sorted, l) && !slices.Equal(m, want) {
			t.Fatal("XMergeEncoded of a sorted record is not the sorted union")
		}
		ms := slices.Clone(m)
		ms.Sort()
		if !slices.Equal(ms, want) {
			t.Fatal("XMergeEncoded lost or duplicated IDs")
		}

		// Search on the sorted list.
		for _, x := range sorted[:min(len(sorted), 64)] {
			for _, id := range []uint64{x - 1, x, x + 1} {
				i := sorted.Search(id)
				if i < 0 || i > len(sorted) || (i < len(sorted) && sorted[i] > id) || (i > 0 && sorted[i-1] <= id) {
					t.Fatalf("Search(%d) = %d", id, i)
				}
				if sorted.Contains(id) != slices.Contains(sorted, id) {
					t.Fatalf("Contains(%d)", id)
				}
			}
		}

		// Runs, on a strictly descending list as the free list keeps.
		uniq := slices.Compact(slices.Clone(sorted))
		n := int(num % 8)
		if i, ok := uniq.FindRun(n); ok {
			if i < n || i > len(uniq) {
				t.Fatalf("FindRun(%d) = %d of %d", n, i, len(uniq))
			}
			lo := uniq[i-1]
			for j := 0; j < n; j++ {
				if uniq[i-1-j] != lo+uint64(j) {
					t.Fatalf("FindRun(%d) = %d is not a run", n, i)
				}
			}
			r := slices.Clone(uniq)
			if pg := r.RemoveRun(i, n); pg != lo || len(r) != len(uniq)-n || !r.IsSortedDesc() {
				t.Fatalf("RemoveRun(%d, %d)", i, n)
			}
		} else if n > 0 && n <= len(uniq) {
			// No run: check by brute force.
			for i := n; i <= len(uniq); i++ {
				if uniq[i-n] == uniq[i-1]+uint64(n-1) {
					t.Fatalf("FindRun(%d) missed the run ending at %d", n, i)
				}
			}
		}
		if len(uniq) > 0 {
			pg := uniq[len(uniq)-1]
			if pg > uint64(n) {
				r := slices.Clone(uniq)
				r.InsertRun(pg-uint64(n)-1, n)
				if len(r) != len(uniq)+n || !r.IsSortedDesc() {
					t.Fatalf("InsertRun below the lowest ID: %v", r[max(0, len(r)-n-2):])
				}
			}
		}
		var a IDL
		a.AppendRange(uint64(num), n)
		if len(a) != n || (n > 1 && !a.IsSortedDesc()) {
			t.Fatal("AppendRange")
		}
	})
}

// FuzzCompare runs every built-in comparator on arbitrary keys and checks
// what can be checked: the sign convention, antisymmetry where the
// comparator reads both operands alike, the byte-order comparators against
// their definitions, and mdb_cmp_cint against C's loop where C stays in
// bounds.
func FuzzCompare(f *testing.F) {
	q := func(v uint64) []byte { return binary.NativeEndian.AppendUint64(nil, v) }
	f.Add([]byte("abc"), []byte("abd"), []byte("x"), uint32(0))
	f.Add(q(1<<40), q(1<<39), []byte{}, uint32(IntegerKey))
	f.Add([]byte{1, 0, 0}, []byte{1, 0}, []byte{9, 9}, uint32(DupSort|IntegerDup))
	f.Fuzz(func(t *testing.T, a, b, extra []byte, flags uint32) {
		sign := func(x int) int {
			switch {
			case x < 0:
				return -1
			case x > 0:
				return 1
			}
			return 0
		}
		tail := append(slices.Clone(b), extra...)
		for _, c := range []Comparator{Memn, Memnr, Long, Int, Clong, Cint} {
			r := c.Compare(a, b)
			if r < -1 || r > 1 {
				t.Fatalf("%v: %d", c, r)
			}
			_ = c.CompareInPage(a, b, tail)
			_ = c.CompareInPage(a, b, b)
			symmetric := c.Kind != CmpCint && c.Kind != CmpClong
			if c.Kind == CmpCint || c.Kind == CmpClong {
				symmetric = len(a) == len(b)
			}
			if symmetric && c.Compare(b, a) != -r {
				t.Fatalf("%v is not antisymmetric on %x, %x", c, a, b)
			}
		}
		if Memn.Compare(a, b) != bytes.Compare(a, b) || Memn.CompareInPage(a, b, tail) != bytes.Compare(a, b) {
			t.Fatal("memn")
		}
		ra, rb := slices.Clone(a), slices.Clone(b)
		slices.Reverse(ra)
		slices.Reverse(rb)
		if CmpMemnrBytes(a, b) != bytes.Compare(ra, rb) {
			t.Fatalf("memnr on %x, %x", a, b)
		}
		if littleEndian && len(a) >= 2 && len(a)%2 == 0 && len(b) >= len(a) {
			// mdb_cmp_cint, literally (little-endian).
			w := func(buf []byte, i int) int { return int(binary.LittleEndian.Uint16(buf[2*i:])) }
			i, x := len(a)/2, 0
			for {
				i--
				x = w(a, i) - w(b, i)
				if x != 0 || i == 0 {
					break
				}
			}
			if CmpCintBytes(a, b) != sign(x) {
				t.Fatalf("cint on %x, %x: %d, C %d", a, b, CmpCintBytes(a, b), x)
			}
		}
		_ = DefaultKeyCmp(flags)
		_, _ = DefaultDupCmp(flags)
	})
}
