package engine

// Hand-built damaged trees (rlmdb tests/handbuilt.rs): a root split on a
// tree as deep as the cursor stack (KNOWN_C_BUGS.md #36,
// root_split_beyond_the_cursor_stack_fails_the_txn), and compacting
// copies of trees whose pages are shared or cyclic (#44).

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// handbuiltFile is a data file of the format of l with ps-byte pages: the
// two meta pages, then pages (page 2 on), with the main DB record main
// in meta page 1 (txnid 1).
func handbuiltFile(l *format.Layout, ps int, pages [][]byte, main format.DB) []byte {
	m0 := format.NewEnvMeta(l, uint32(ps), 1<<20, 0)
	m0.DBs[0].Root = format.PInvalid
	m1 := m0
	m1.TxnID = 1
	m1.LastPgno = 1 + uint64(len(pages))
	m1.DBs[1] = main
	file := make([]byte, 2*ps)
	m0.WriteInitialPages(file, ps, l)
	m1.Write(file[ps+l.MetaOffset():])
	for _, p := range pages {
		file = append(file, p...)
	}
	return file
}

// TestRootSplitBeyondCursorStack: a tree as deep as a cursor stack
// (CursorStack, 32 levels; a real one would have more than 2^31 pages, so
// this file is damaged: every branch page names one child several times)
// whose pages along the path to its last key are all full. A put after
// that key splits every level, up to the root, and the new root would be
// a 33rd level under the cursors on the old one: C writes past the end of
// its cursor stacks. The put must fail with MDB_CURSOR_FULL, which fails
// the transaction, and leave the file as it was.
//
// Pages of 256 bytes, in both formats: pages 2..32 are branch pages of the
// implicit first child and three 58-byte separators, every child being the
// next page; page 33 is a leaf of three 58-byte keys with 1-byte values.
// The put goes through Txn.Put and through a cursor, with another cursor
// on the leaf.
func TestRootSplitBeyondCursorStack(t *testing.T) {
	const ps = 256
	key := func(c byte) []byte { return bytes.Repeat([]byte{c}, 58) }
	for _, l := range []*format.Layout{format.LayoutV09, format.LayoutV10} {
		var pages [][]byte
		for pgno := uint64(2); pgno <= 32; pgno++ {
			p := make([]byte, ps)
			b := format.PageBuilder{B: p, L: l}
			b.Init(pgno, format.PBranch)
			child := format.NodeData{IsChild: true, Child: pgno + 1}
			b.AddNode(0, nil, child, 0)
			for i, c := range []byte("bcd") {
				if !b.AddNode(i+1, key(c), child, 0) {
					t.Fatalf("%v: branch page %d: separator %d", l.Format, pgno, i)
				}
			}
			if b.AddNode(4, key('e'), child, 0) {
				t.Fatalf("%v: branch page %d is not full", l.Format, pgno)
			}
			pages = append(pages, p)
		}
		p := make([]byte, ps)
		b := format.PageBuilder{B: p, L: l}
		b.Init(33, format.PLeaf)
		for i, c := range []byte("def") {
			if !b.AddNode(i, key(c), format.NodeData{Inline: []byte{c}}, 0) {
				t.Fatalf("%v: leaf key %d", l.Format, i)
			}
		}
		if b.AddNode(3, key('g'), format.NodeData{Inline: []byte("g")}, 0) {
			t.Fatalf("%v: the leaf is not full", l.Format)
		}
		pages = append(pages, p)
		main := format.DB{Depth: 32, BranchPages: 31, LeafPages: 1, Entries: 3, Root: 2}
		file := handbuiltFile(l, ps, pages, main)
		dir := t.TempDir()
		data := filepath.Join(dir, "data.mdb")
		if err := os.WriteFile(data, file, 0o644); err != nil {
			t.Fatal(err)
		}
		env, err := Open(dir, Options{MaxDBs: 2, Flags: format.NoLock | format.NoSync})
		if err != nil {
			t.Fatal(err)
		}
		if env.Format() != l.Format || env.PageSize() != ps {
			t.Fatalf("opened as %v with %d-byte pages", env.Format(), env.PageSize())
		}
		rt, err := env.BeginTxn(nil, format.ReadOnly)
		if err != nil {
			t.Fatal(err)
		}
		if v, err := rt.Get(MainDBI, key('e')); err != nil || string(v) != "e" {
			t.Fatalf("%v: get e: %q %v", l.Format, v, err)
		}
		rt.Abort()

		expect := func(err, want error, how string) {
			t.Helper()
			if !errors.Is(err, want) {
				t.Fatalf("%v: %s: %v, want %v", l.Format, how, err, want)
			}
		}
		txn, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		expect(txn.Put(MainDBI, key('g'), []byte("g"), 0), format.CursorFull, "put")
		expect(txn.Commit(), format.BadTxn, "commit after the put")

		txn, err = env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		c0, err := txn.OpenCursor(MainDBI)
		if err != nil {
			t.Fatal(err)
		}
		c1, err := txn.OpenCursor(MainDBI)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := c1.Get(key('f'), nil, format.Set); err != nil {
			t.Fatalf("%v: cursor on the leaf: %v", l.Format, err)
		}
		expect(c0.Put(key('g'), []byte("g"), 0), format.CursorFull, "cursor put")
		expect(txn.Commit(), format.BadTxn, "commit after the cursor put")
		env.Close()
		if got, _ := os.ReadFile(data); !bytes.Equal(got, file) {
			t.Fatalf("%v: the file changed", l.Format)
		}
	}
}

// TestCompactingCopyOfSharedPagesStops: trees whose branch pages share
// their children or point back up (rlmdb tests/handbuilt.rs
// compacting_copy_of_shared_pages_stops_at_the_source_size,
// KNOWN_C_BUGS.md #44). A compacting copy writes a page once per path to
// it, which C's mdb_env_cwalk does without bound (4^12 pages for a file of
// 15 here). The copy stops with MDB_CORRUPTED as soon as it would hold
// more pages than the file; a plain copy is unaffected.
//
// "shared": branch pages 2..13 with four children each, all the next
// page, and a leaf of three keys at page 14. "cycle": branch page 2 whose
// first three children are the leaf at page 3 and whose fourth is page 2
// itself.
func TestCompactingCopyOfSharedPagesStops(t *testing.T) {
	const ps, levels = 4096, 12
	l := format.LayoutV09
	leafKV := [][2]string{{"a", "1"}, {"m", "2"}, {"z", "3"}}
	branch := func(pgno, child uint64, back bool) []byte {
		p := make([]byte, ps)
		b := format.PageBuilder{B: p, L: l}
		b.Init(pgno, format.PBranch)
		for i, k := range []string{"", "g", "n", "t"} {
			to := child
			if back && i == 3 {
				to = pgno
			}
			if !b.AddNode(i, []byte(k), format.NodeData{IsChild: true, Child: to}, 0) {
				t.Fatalf("branch page %d: node %d", pgno, i)
			}
		}
		return p
	}
	leaf := func(pgno uint64) []byte {
		p := make([]byte, ps)
		b := format.PageBuilder{B: p, L: l}
		b.Init(pgno, format.PLeaf)
		for i, kv := range leafKV {
			if !b.AddNode(i, []byte(kv[0]), format.NodeData{Inline: []byte(kv[1])}, 0) {
				t.Fatalf("leaf page %d: node %d", pgno, i)
			}
		}
		return p
	}
	var shared [][]byte
	for lv := uint64(0); lv < levels; lv++ {
		shared = append(shared, branch(2+lv, 3+lv, false))
	}
	shared = append(shared, leaf(2+levels))
	cycle := [][]byte{branch(2, 3, true), leaf(3)}
	for _, c := range []struct {
		what  string
		pages [][]byte
	}{{"shared", shared}, {"cycle", cycle}} {
		dir := t.TempDir()
		main := format.DB{Depth: uint16(len(c.pages)), BranchPages: uint64(len(c.pages) - 1), LeafPages: 1,
			Entries: uint64(len(leafKV)), Root: 2}
		file := handbuiltFile(l, ps, c.pages, main)
		if err := os.WriteFile(filepath.Join(dir, "data.mdb"), file, 0o644); err != nil {
			t.Fatal(err)
		}
		env, err := Open(dir, Options{MaxDBs: 2, Flags: format.NoLock | format.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		compact := t.TempDir()
		if err := env.Copy(compact, format.CopyCompact); !errors.Is(err, format.Corrupted) {
			t.Fatalf("%s: compacting copy: %v, want MDB_CORRUPTED", c.what, err)
		}
		if st, err := os.Stat(filepath.Join(compact, "data.mdb")); err != nil || st.Size() > int64(len(file)) {
			t.Fatalf("%s: the compacting copy wrote %d bytes (%v), more than the file's %d", c.what, st.Size(), err, len(file))
		}
		plain := t.TempDir()
		if err := env.Copy(plain, 0); err != nil {
			t.Fatalf("%s: plain copy: %v", c.what, err)
		}
		if got, err := os.ReadFile(filepath.Join(plain, "data.mdb")); err != nil || !bytes.Equal(got, file) {
			t.Fatalf("%s: the plain copy differs from the file (%v)", c.what, err)
		}
		env.Close()
	}
}
