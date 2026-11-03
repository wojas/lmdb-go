package engine

// MDB_CURRENT on a duplicate in a sub-DB, replaced by an item that sorts
// where the old one is, where C keeps the new item in a leaf that searches
// do not look in (found by the Go port's FuzzWriteModel): the first item of
// a leaf other than the leftmost, replaced by one of another size that
// sorts before the branch key above the leaf (C 0.9's ITS#8447 path,
// new_ksize, without fix_parent), and the last item of a leaf other than
// the rightmost, replaced by one (of any size) at or after the next leaf's
// branch key, which deleting that leaf's first item leaves behind (C 0.9
// and 1.0). C's MDB_GET_BOTH then says MDB_NOTFOUND, and later puts land
// out of order (rlmdb KNOWN_C_BUGS.md row 56; rlmdb
// tests/common/differential.rs current_dup_outside_leaf_script,
// write_differential.rs diff_current_dup_outside_leaf,
// interop/tests/write_differential10.rs diff10_current_dup_outside_leaf).
// The Go writer, as rlmdb, deletes the item and puts the new one there,
// which must leave the file C's mdb_cursor_del + mdb_cursor_put leave (the
// script's "cr" op); elsewhere both sides put with MDB_CURRENT. C 1.0 lacks
// ITS#8447 and writes an item of another size over the old one's key
// (rlmdb KNOWN_C_BUGS.md, ITS#8447), so its in-place cases of another size
// are left out; its "cr" ops and in-place cases of the same size are
// compared.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// subDBLeafFirsts runs the puts of items under key in a fresh environment
// of o's shape, commits, and returns the index of the first item of every
// leaf of key's sub-DB but the leftmost whose leaf and the one before it
// hold as many items as the leftmost (not the rightmost, which is fuller or
// emptier: a delete there may rebalance), read from the data file.
func subDBLeafFirsts(t *testing.T, o *oracle, dbflags uint32, key []byte, items [][]byte) []int {
	dir := t.TempDir()
	if o.dataFile != nil {
		if err := os.WriteFile(filepath.Join(dir, "data.mdb"), o.dataFile, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env, err := eng.Open(dir, eng.Options{Format: o.f, PageSize: o.pageSize, MapSize: 64 << 20, MaxDBs: 8,
		Flags: format.NoSync | format.NoLock})
	if err != nil {
		t.Fatal(err)
	}
	txn, err := env.BeginTxn(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	dbi, err := txn.OpenDBI([]byte("t"), dbflags|format.Create)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range items {
		if err := txn.Put(dbi, key, d, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
	ps := env.PageSize()
	env.Close()
	file, err := os.ReadFile(filepath.Join(dir, "data.mdb"))
	if err != nil {
		t.Fatal(err)
	}
	l := o.f.Layout()
	page := func(pgno uint64) format.Page {
		if (pgno+1)*uint64(ps) > uint64(len(file)) {
			t.Fatalf("page %d out of range", pgno)
		}
		return format.MakePage(file[pgno*uint64(ps):(pgno+1)*uint64(ps)], l)
	}
	var metas [2]format.Meta
	for i := range metas {
		if metas[i], err = format.ReadMetaPage(page(uint64(i)).B, l); err != nil {
			t.Fatal(err)
		}
	}
	// find is the data of key's node in the tree at root.
	find := func(root uint64, key []byte) format.Node {
		p := page(root)
		for p.IsBranch() {
			next := uint64(format.PInvalid)
			for i := 0; i < p.NumKeys(); i++ {
				n, _ := p.Node(i)
				k, _ := n.Key()
				if i > 0 && strings.Compare(string(k), string(key)) > 0 {
					break
				}
				next = n.ChildPgno()
			}
			p = page(next)
		}
		for i := 0; i < p.NumKeys(); i++ {
			n, _ := p.Node(i)
			if k, _ := n.Key(); string(k) == string(key) {
				return n
			}
		}
		t.Fatalf("key %q not found", key)
		return format.Node{}
	}
	subDB := func(n format.Node) format.DB {
		data, _ := n.InlineData()
		db, ok := format.ReadDB(data)
		if !ok || !n.IsSubData() {
			t.Fatal("not a DB record")
		}
		return db
	}
	name := []byte("t")
	if l.DBNameNUL {
		name = append(name, 0)
	}
	named := subDB(find(metas[format.PickNewest(&metas)].DBs[1].Root, name))
	sub := subDB(find(named.Root, key))
	index := map[string]int{}
	for i, d := range items {
		index[string(d)] = i
	}
	var firsts, counts []int
	var walk func(pgno uint64)
	walk = func(pgno uint64) {
		p := page(pgno)
		if p.IsBranch() {
			for i := 0; i < p.NumKeys(); i++ {
				n, _ := p.Node(i)
				walk(n.ChildPgno())
			}
			return
		}
		var k []byte
		if p.IsLeaf2() {
			k, _ = p.Leaf2Key(0, int(p.Pad()))
		} else {
			n, _ := p.Node(0)
			k, _ = n.Key()
		}
		firsts = append(firsts, index[string(k)])
		counts = append(counts, p.NumKeys())
	}
	walk(sub.Root)
	var out []int
	for i := 1; i < len(firsts); i++ {
		if counts[i] == counts[0] && counts[i-1] == counts[0] {
			out = append(out, firsts[i])
		}
	}
	if len(out) < 4 {
		t.Fatalf("the sub-DB has %d leaves (%v)", len(firsts), counts)
	}
	return out
}

// currentOutsideScript is a script (two cursors, a MDB_DUPSORT DB with
// dbflags) that puts n items of itemLen bytes under one key, then, from
// the right, one boundary between two leaves of the sub-DB at a time, the
// other cursor on the item after the next one, or on the item itself where
// it is replaced in place (the two rightmost boundaries in a committed and
// an aborted nested transaction), replaces with MDB_CURRENT:
// the leaf's first item by a shorter and a longer item that sort before
// it ("cr"), by one of the same size before it and a shorter one after it,
// or the leaf's second item ("cp"); or, after deleting the leaf's first
// item, the previous leaf's last item by the deleted item itself, by items
// after it of the same size and shorter ("cr"), or by one of the same size
// before it ("cp"). Without MDB_DUPFIXED only items of the same size. Then
// the leftmost leaf's first item, shorter ("cp"). Then every new item is
// looked up, items between new ones and the old ones are put, and the
// database is walked. inPlaceOtherSize keeps the "cp" replacements by
// items of another size (C 1.0 writes those over the old key).
//
// The other cursor on the replaced item makes a replacement that should
// stay in place but is deleted and inserted visible: the delete moves the
// cursor off the item, the in-place put does not.
func currentOutsideScript(t *testing.T, o *oracle, dbflags uint32, itemLen, n int, inPlaceOtherSize bool) []string {
	fixed := dbflags&format.DupFixed != 0
	key := []byte("k")
	item := func(j int) []byte {
		d := []byte(fmt.Sprintf("b%04d", j))
		for len(d) < itemLen {
			d = append(d, 'p')
		}
		return d
	}
	items := make([][]byte, n)
	for j := range items {
		items[j] = item(j)
	}
	cat := func(b []byte, s string) []byte { return append(append([]byte(nil), b...), s...) }
	last := func(b []byte, c byte) []byte {
		d := cat(b, "")
		d[len(d)-1] = c
		return d
	}
	firsts := subDBLeafFirsts(t, o, dbflags, key, items)
	k := hexs(key)
	var ops []string
	add := func(format string, args ...any) { ops = append(ops, fmt.Sprintf(format, args...)) }
	for _, d := range items {
		add("p:%s:%s:0", k, hexs(d))
	}
	var added, between [][]byte
	replace := func(j int, d []byte, routed bool) {
		if !routed && !inPlaceOtherSize && len(d) != itemLen {
			return
		}
		add("g:0:%d:%s:%s", format.GetBoth, k, hexs(items[j]))
		// The other cursor: on the item itself for a replacement in place,
		// which a delete and insert would move it off.
		other := j + 2
		if !routed {
			other = j
		}
		if other < n {
			add("g:1:%d:%s:%s", format.GetBoth, k, hexs(items[other]))
		}
		if routed {
			add("cr:0:%s:%s", k, hexs(d))
		} else {
			add("cp:0:%s:%s:%d", k, hexs(d), format.Current)
		}
		add("g:0:%d", format.GetCurrent)
		added = append(added, d)
	}
	del := func(j int) {
		add("g:0:%d:%s:%s", format.GetBoth, k, hexs(items[j]))
		add("cd:0:0")
	}
	cases := []int{0, 1, 2, 3, 4, 5, 6, 7, 8}
	if fixed {
		cases = []int{2, 5, 6, 8}
	}
	for c := len(firsts) - 1; c >= 0; c-- {
		f := firsts[c]
		prev := items[f-1]
		end := ""
		switch len(firsts) - 1 - c {
		case 0:
			end = "nc"
		case 1:
			end = "na"
		}
		if end != "" {
			add("nb")
		}
		switch cases[c%len(cases)] {
		case 0: // the first item, by a shorter one before it: below the key
			replace(f, cat(prev[:5], "q"), true)
			between = append(between, cat(prev[:5], "r"))
		case 1: // the first item, by a longer one before it: below the key
			replace(f, cat(prev, "q"), true)
		case 2: // the first item, by one of the same size before it
			replace(f, last(items[f], 'o'), false)
		case 3: // the first item, by a shorter one after it
			replace(f, cat(items[f][:5], "q"), false)
		case 4: // the second item, shorter
			if f+2 < n {
				replace(f+1, cat(items[f][:5], "q"), false)
			}
		case 5: // the previous leaf's last item, by the deleted first item
			del(f)
			replace(f-1, items[f], true)
		case 6: // ... by one of the same size after it
			del(f)
			replace(f-1, last(items[f], 'q'), true)
			between = append(between, items[f])
		case 7: // ... by a shorter one after it
			del(f)
			replace(f-1, cat(items[f][:5], "q"), true)
		case 8: // ... by one of the same size before it
			del(f)
			replace(f-1, last(prev, 'q'), false)
		}
		if end != "" {
			add("%s", end)
			add("g:0:%d", format.GetCurrent)
		}
	}
	if !fixed {
		// The leftmost leaf's first item, shorter.
		replace(0, items[0][:4], false)
	}
	for _, d := range added {
		add("g:1:%d:%s:%s", format.GetBoth, k, hexs(d))
	}
	for _, d := range between {
		add("p:%s:%s:0", k, hexs(d))
	}
	add("g:0:%d", format.First)
	for i := 0; i < n+len(between); i++ {
		add("g:0:%d", format.Next)
	}
	return ops
}

// countCR is the number of "cr" ops.
func countCR(ops []string) int {
	n := 0
	for _, op := range ops {
		if strings.HasPrefix(op, "cr:") {
			n++
		}
	}
	return n
}

// TestDifferential09CurrentDupOutsideLeaf: 0.9 at the OS page size, also
// with MDB_DUPFIXED and with a dirty limit of 16 pages on the Go side
// (spilling must not change what is written).
func TestDifferential09CurrentDupOutsideLeaf(t *testing.T) {
	o := oracle09(t)
	itemLen := min(os.Getpagesize()/24, maxKey09-1)
	for _, dbflags := range []uint32{format.DupSort, format.DupSort | format.DupFixed} {
		p := &profile{dbflags: dbflags}
		ops := currentOutsideScript(t, o, dbflags, itemLen, 320, true)
		if countCR(ops) < 3 {
			t.Fatalf("flags %#x: %d cr ops", dbflags, countCR(ops))
		}
		for _, limit := range []int{0, 16} {
			o.dirtyLimit = limit
			if !runRound(t, o, p, 2, nil, 0, ops, fmt.Sprintf("current-outside-%#x-dirty%d", dbflags, limit), 64<<20) {
				t.Fatal("C stopped")
			}
		}
	}
}

// TestDifferential10CurrentDupOutsideLeaf: 1.0 at small and large page
// sizes, also with MDB_DUPFIXED and a dirty limit of 16 pages; in place
// only items of the same size (C 1.0 writes others over the old key,
// ITS#8447).
func TestDifferential10CurrentDupOutsideLeaf(t *testing.T) {
	tool(t, "writeops10")
	for _, ps := range pageSizes10Small {
		o := oracle10(t, ps)
		maxkey := nodemax10(ps) - 56
		itemLen := max(8, min(ps/24, maxkey-1))
		for _, dbflags := range []uint32{format.DupSort, format.DupSort | format.DupFixed} {
			p := &profile{dbflags: dbflags}
			ops := currentOutsideScript(t, o, dbflags, itemLen, 320, false)
			if countCR(ops) < 3 {
				t.Fatalf("page size %d, flags %#x: %d cr ops", ps, dbflags, countCR(ops))
			}
			for _, limit := range []int{0, 16} {
				o.dirtyLimit = limit
				tag := fmt.Sprintf("current-outside-%d-%#x-dirty%d", ps, dbflags, limit)
				if !runRound(t, o, p, 2, nil, 0, ops, tag, 64<<20) {
					t.Fatalf("%s: C stopped", tag)
				}
			}
		}
	}
}
