package engine

// Nested transaction edge cases (port of parts of rlmdb's
// tests/nested_txn.rs; the lock-step differential test covers nested
// transactions with cursors against C).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func TestChildHandlesAndFailures(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 4})
	p := beginW(t, env)
	a, _ := p.OpenDBI([]byte("a"), format.Create)
	must(t, p.Put(a, []byte("k"), []byte("v"), 0))

	// A DB created in an aborted child: the handle is closed.
	c, err := env.BeginTxn(p, 0)
	must(t, err)
	b, _ := c.OpenDBI([]byte("b"), format.Create)
	must(t, c.Put(b, []byte("x"), []byte("y"), 0))
	c.Abort()
	if _, err := p.Get(b, []byte("x")); err == nil {
		t.Fatal("handle of a DB created in an aborted child works")
	}
	_, err = p.OpenDBI([]byte("b"), 0)
	expectErr(t, err, format.NotFound, "DB of an aborted child")

	// A DB created and written by committed children.
	c, _ = env.BeginTxn(p, 0)
	b, _ = c.OpenDBI([]byte("b"), format.Create)
	must(t, c.Put(b, []byte("x"), []byte("y"), 0))
	g, _ := env.BeginTxn(c, 0)
	must(t, g.Put(a, []byte("k"), []byte("g"), 0))
	must(t, g.Commit())
	must(t, c.Commit())
	if v, _ := p.Get(b, []byte("x")); string(v) != "y" {
		t.Fatal(string(v))
	}
	if v, _ := p.Get(a, []byte("k")); string(v) != "g" {
		t.Fatal(string(v))
	}

	// Errors that do not fail a transaction leave the child usable.
	c, _ = env.BeginTxn(p, 0)
	expectErr(t, c.Put(a, []byte{}, []byte("v"), 0), format.BadValSize, "empty key in child")
	must(t, c.Put(a, []byte("k2"), []byte("v2"), 0))
	c.Abort()
	if _, err := p.Get(a, []byte("k2")); err != format.NotFound {
		t.Fatal(err)
	}
	must(t, p.Commit())
	r := beginR(t, env)
	defer r.Abort()
	b, err = r.OpenDBI([]byte("b"), 0)
	must(t, err)
	if v, _ := r.Get(b, []byte("x")); string(v) != "y" {
		t.Fatal(string(v))
	}
	if err := env.Audit(); err != nil {
		t.Fatal(err)
	}
}

func TestNestedOnFailedParentIsRefused(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MapSize: 1 << 20})
	p := beginW(t, env)
	defer p.Abort()
	var err error
	for i := uint32(0); err == nil; i++ {
		err = p.Put(MainDBI, binary.BigEndian.AppendUint32(nil, i), make([]byte, 1000), 0)
	}
	expectErr(t, err, format.MapFull, "fill")
	_, err = env.BeginTxn(p, 0)
	expectErr(t, err, format.BadTxn, "nested on a failed parent")
}

func TestFailedChildCommitFailsTheParent(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MapSize: 1 << 20})
	p := beginW(t, env)
	must(t, p.Put(MainDBI, []byte("a"), []byte("b"), 0))
	c, _ := env.BeginTxn(p, 0)
	for i := uint32(0); c.Put(MainDBI, binary.BigEndian.AppendUint32(nil, i), make([]byte, 1000), 0) == nil; i++ {
	}
	expectErr(t, c.Commit(), format.BadTxn, "commit of a failed child")
	expectErr(t, p.Put(MainDBI, []byte("c"), []byte("d"), 0), format.BadTxn, "parent after failed child")
	expectErr(t, p.Commit(), format.BadTxn, "commit of the parent")
	r := beginR(t, env)
	defer r.Abort()
	if _, err := r.Get(MainDBI, []byte("a")); err != format.NotFound {
		t.Fatal(err)
	}
}

func TestComparatorSetInChildSurvivesAbort(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2})
	p := beginW(t, env)
	defer p.Abort()
	db, _ := p.OpenDBI([]byte("r"), format.Create)
	c, _ := env.BeginTxn(p, 0)
	must(t, c.SetCompare(db, func(a, b []byte) int { return bytes.Compare(b, a) }))
	c.Abort()
	for _, k := range []string{"a", "c", "b"} {
		must(t, p.Put(db, []byte(k), nil, 0))
	}
	cur, _ := p.OpenCursor(db)
	var keys []string
	for {
		k, _, err := cur.Get(nil, nil, format.Next)
		if err != nil {
			break
		}
		keys = append(keys, string(k))
	}
	if len(keys) != 3 || keys[0] != "c" || keys[1] != "b" || keys[2] != "a" {
		t.Fatal(keys)
	}
}

// TestParentCommitCommitsActiveChild: as in C (mdb_txn_commit commits
// mt_child first), committing a parent with an active child commits the
// child; aborting the parent aborts it.
func TestParentCommitCommitsActiveChild(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{})
	p := beginW(t, env)
	must(t, p.Put(MainDBI, []byte("a"), []byte("1"), 0))
	c, _ := env.BeginTxn(p, 0)
	must(t, c.Put(MainDBI, []byte("b"), []byte("2"), 0))
	expectErr(t, p.Put(MainDBI, []byte("x"), []byte("y"), 0), format.BadTxn, "parent with an active child")
	must(t, p.Commit())
	r := beginR(t, env)
	if v, _ := r.Get(MainDBI, []byte("b")); string(v) != "2" {
		t.Fatal(string(v))
	}
	r.Abort()
	p = beginW(t, env)
	c, _ = env.BeginTxn(p, 0)
	must(t, c.Put(MainDBI, []byte("c"), []byte("3"), 0))
	p.Abort()
	r = beginR(t, env)
	defer r.Abort()
	if _, err := r.Get(MainDBI, []byte("c")); err != format.NotFound {
		t.Fatal(err)
	}
}

func TestBeginTxnParentRules(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{})
	r := beginR(t, env)
	_, err := env.BeginTxn(r, 0)
	expectErr(t, err, syscall.EINVAL, "child of a read-only txn")
	r.Abort()
	p := beginW(t, env)
	_, err = env.BeginTxn(p, format.ReadOnly)
	expectErr(t, err, format.BadTxn, "read-only child")
	c, err := env.BeginTxn(p, 0)
	must(t, err)
	_, err = env.BeginTxn(p, 0)
	expectErr(t, err, format.BadTxn, "second child")
	c.Abort()
	p.Abort()
}

// TestReusedChildKeepsParentPghead: a committed child hands its copy of
// the reclaimed pages (me_pghead) to the parent; the next child, which
// reuses the first one's memory, must work on a copy again, so that its
// abort leaves the parent's list as it was.
func TestReusedChildKeepsParentPghead(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{})
	key := func(i int) []byte { return []byte(fmt.Sprintf("key%06d", i)) }
	val := bytes.Repeat([]byte("v"), 200)
	put := func(txn *Txn, from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			must(t, txn.Put(MainDBI, key(i), val, 0))
		}
	}
	// Free many pages: fill, then delete most of it.
	txn := beginW(t, env)
	put(txn, 0, 3000)
	must(t, txn.Commit())
	txn = beginW(t, env)
	for i := 0; i < 2900; i++ {
		must(t, txn.Del(MainDBI, key(i), nil))
	}
	must(t, txn.Commit())
	// A free DB record is reclaimed once it is older than every snapshot.
	txn = beginW(t, env)
	put(txn, 5000, 5001)
	must(t, txn.Commit())
	// The parent reclaims them.
	p := beginW(t, env)
	put(p, 10000, 10050)
	if len(p.w.pghead) < 10 {
		t.Fatalf("setup: pghead holds %d pages", len(p.w.pghead))
	}
	c, err := env.BeginTxn(p, 0)
	must(t, err)
	put(c, 20000, 20050)
	must(t, c.Commit())
	before := append([]uint64(nil), p.w.pghead...)
	c, err = env.BeginTxn(p, 0)
	must(t, err)
	put(c, 30000, 30400)
	c.Abort()
	if !slices.Equal(before, p.w.pghead) {
		t.Fatalf("an aborted child changed the parent's pghead:\n%v\n%v", before, []uint64(p.w.pghead))
	}
	put(p, 40000, 40400)
	must(t, p.Commit())
	must(t, env.Audit())
}

// TestComparatorOfAbortedDBNotInherited: an abort that closes the handle
// of a database its transaction (or a nested child) created forgets the
// comparators installed for it, so the next database to get the slot
// starts with the default order (rlmdb 774f8d0, capi_write.c
// test_comparator_reuse; C's mdb_dbi_open calls mdb_default_cmp). The
// port keeps the comparators in the environment's DBI slot, which every
// close clears with the rest of it.
func TestComparatorOfAbortedDBNotInherited(t *testing.T) {
	rev := func(a, b []byte) int { return bytes.Compare(b, a) }
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, nested := range []bool{false, true} {
			for _, dup := range []bool{false, true} {
				what := fmt.Sprintf("%v nested=%v dupsort=%v", f, nested, dup)
				env, _ := openTest(t, f, Options{MaxDBs: 2})
				flags := uint32(format.Create)
				if dup {
					flags |= format.DupSort
				}
				p := beginW(t, env)
				creator := p
				if nested {
					var err error
					if creator, err = env.BeginTxn(p, 0); err != nil {
						t.Fatal(err)
					}
				}
				a, err := creator.OpenDBI([]byte("a"), flags)
				must(t, err)
				if dup {
					must(t, creator.SetDupsort(a, rev))
				} else {
					must(t, creator.SetCompare(a, rev))
				}
				creator.Abort()
				if nested {
					must(t, p.Commit())
				}
				w := beginW(t, env)
				b, err := w.OpenDBI([]byte("b"), flags)
				must(t, err)
				if b != a {
					t.Fatalf("%s: b got DBI %d, a had %d", what, b, a)
				}
				key, dcmp, _, err := w.Comparators(b)
				must(t, err)
				c := key
				if dup {
					c = dcmp
				}
				if c.Compare([]byte("a"), []byte("b")) >= 0 {
					t.Errorf("%s: b inherited a's comparator", what)
				}
				for _, v := range []string{"b", "a"} {
					must(t, w.Put(b, []byte("k"), []byte(v), 0))
					must(t, w.Put(b, []byte(v), []byte(v), 0))
				}
				cur, _ := w.OpenCursor(b)
				var got []string
				for k, v, err := cur.Get(nil, nil, format.Next); err == nil; k, v, err = cur.Get(nil, nil, format.Next) {
					got = append(got, string(k)+string(v))
				}
				want := "aa bb ka kb"
				if !dup {
					want = "aa bb ka"
				}
				if strings.Join(got, " ") != want {
					t.Errorf("%s: order %q, want %q", what, got, want)
				}
				w.Abort()
				env.Close()
			}
		}
	}
}

// TestActiveDescendantsAtAnyDepth (rlmdb 3a97557, capi_write.c): a
// transaction with an active child is blocked; in Go nothing can leak a child
// the way mem::forget can in Rust, and as in C the end of an ancestor ends
// its descendants. A commit at any depth commits them first; an abort aborts
// them, with the handles of the databases they created. Both formats, several
// depths.
func TestActiveDescendantsAtAnyDepth(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, depth := range []int{1, 2, 3, 5} {
			for _, keep := range []bool{true, false} {
				env, _ := openTest(t, f, Options{MaxDBs: 16})
				top := beginW(t, env)
				must(t, top.Put(MainDBI, []byte("key"), []byte("original"), 0))
				must(t, top.Commit())

				top = beginW(t, env)
				chain := []*Txn{top}
				var names []string
				for level := 0; level <= depth; level++ {
					cur := chain[level]
					must(t, cur.Put(MainDBI, []byte("key"), []byte(fmt.Sprintf("level %d", level)), 0))
					name := fmt.Sprintf("made at %d", level)
					names = append(names, name)
					_, err := cur.OpenDBI([]byte(name), format.Create)
					must(t, err)
					if level < depth {
						c, err := env.BeginTxn(cur, 0)
						must(t, err)
						chain = append(chain, c)
					}
				}
				// Every ancestor refuses everything.
				for _, a := range chain[:depth] {
					expectErr(t, a.Put(MainDBI, []byte("key"), []byte("x"), 0), format.BadTxn, "put")
					_, err := a.Get(MainDBI, []byte("key"))
					expectErr(t, err, format.BadTxn, "get")
					expectErr(t, a.Del(MainDBI, []byte("key"), nil), format.BadTxn, "del")
					_, err = a.OpenCursor(MainDBI)
					expectErr(t, err, format.BadTxn, "cursor")
					_, err = a.OpenDBI([]byte("late"), format.Create)
					expectErr(t, err, format.BadTxn, "create")
					_, err = env.BeginTxn(a, 0)
					expectErr(t, err, format.BadTxn, "begin")
				}
				want := "original"
				if keep {
					must(t, top.Commit())
					want = fmt.Sprintf("level %d", depth)
				} else {
					top.Abort()
				}
				r := beginR(t, env)
				v, err := r.Get(MainDBI, []byte("key"))
				must(t, err)
				if string(v) != want {
					t.Fatalf("%v depth %d keep %v: %q, want %q", f, depth, keep, v, want)
				}
				for _, name := range names {
					_, err := r.OpenDBI([]byte(name), 0)
					if keep {
						must(t, err)
					} else {
						expectErr(t, err, format.NotFound, name)
					}
				}
				r.Abort()
				// The names are free again, and the environment takes writes.
				txn := beginW(t, env)
				for _, name := range names {
					db, err := txn.OpenDBI([]byte(name), format.Create)
					must(t, err)
					must(t, txn.Put(db, []byte("k"), []byte(name), 0))
				}
				must(t, txn.Commit())
				mustAudit(t, env)
			}
		}
	}
}

// TestFailedDescendantFailsTheCommit (rlmdb 3a97557, capi_write.c): a
// grandchild that failed and is still active: committing the top-level
// transaction commits it first, which fails with MDB_BAD_TXN, and everything
// is aborted.
func TestFailedDescendantFailsTheCommit(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MapSize: 1 << 20})
	top := beginW(t, env)
	must(t, top.Put(MainDBI, []byte("parent"), []byte("lost"), 0))
	child, err := env.BeginTxn(top, 0)
	must(t, err)
	grand, err := env.BeginTxn(child, 0)
	must(t, err)
	big := bytes.Repeat([]byte("y"), 3000)
	for i := 0; ; i++ {
		err := grand.Put(MainDBI, []byte(fmt.Sprintf("%08d", i)), big, 0)
		if err == format.MapFull {
			break
		}
		must(t, err)
	}
	expectErr(t, top.Commit(), format.BadTxn, "commit of the top-level transaction")
	r := beginR(t, env)
	defer r.Abort()
	_, err = r.Get(MainDBI, []byte("parent"))
	expectErr(t, err, format.NotFound, "the parent's key")
}

// TestComparatorsFollowAReopenedHandle (rlmdb 54a8645): the abort of a nested
// transaction that dropped a database reopens its handle (KNOWN_C_BUGS.md
// row 20) with its comparators, which belong to the handle: Comparators (what
// mdb_cmp and mdb_dcmp call) and the tree's order stay the custom ones.
func TestComparatorsFollowAReopenedHandle(t *testing.T) {
	reverse := func(a, b []byte) int { return bytes.Compare(b, a) }
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2})
	p := beginW(t, env)
	defer p.Abort()
	db, err := p.OpenDBI([]byte("r"), format.Create|format.DupSort)
	must(t, err)
	must(t, p.SetCompare(db, reverse))
	must(t, p.SetDupsort(db, reverse))
	for _, k := range []string{"a", "c", "b"} {
		must(t, p.Put(db, []byte(k), []byte("1"), 0))
		must(t, p.Put(db, []byte(k), []byte("2"), 0))
	}
	c, err := env.BeginTxn(p, 0)
	must(t, err)
	must(t, c.Drop(db, true))
	c.Abort()
	key, dup, hasDup, err := p.Comparators(db)
	must(t, err)
	if !hasDup || key.Compare([]byte("a"), []byte("b")) <= 0 || dup.Compare([]byte("a"), []byte("b")) <= 0 {
		t.Fatalf("the comparators after the abort: %+v, %+v, %v", key, dup, hasDup)
	}
	must(t, p.Put(db, []byte("d"), []byte("1"), 0))
	cur, err := p.OpenCursor(db)
	must(t, err)
	var got []string
	for {
		k, v, err := cur.Get(nil, nil, format.Next)
		if err != nil {
			break
		}
		got = append(got, string(k)+string(v))
	}
	if want := []string{"d1", "c2", "c1", "b2", "b1", "a2", "a1"}; !slices.Equal(got, want) {
		t.Fatalf("order after the abort: %q, want %q", got, want)
	}
}

// foldCase is ASCII case-insensitive order.
func foldCase(a, b []byte) int { return bytes.Compare(bytes.ToLower(a), bytes.ToLower(b)) }

// TestComparatorsSetAfterAReadCursorOpened (rlmdb 769f352, core_api_c2.rs): a
// read cursor opened before SetCompare / SetDupsort searches with the new key
// order and the new order of a key's single data item, as C's, which reaches
// both through the handle; for a key's duplicates it keeps the order it was
// opened with, as C's sub-cursor does (mdb_xcursor_init0). Both formats.
func TestComparatorsSetAfterAReadCursorOpened(t *testing.T) {
	reverse := func(a, b []byte) int { return bytes.Compare(b, a) }
	for _, f := range []format.Format{format.V09, format.V10} {
		dir := t.TempDir()
		open := func() *Env {
			env, err := Open(dir, Options{Format: f, MapSize: 16 << 20, MaxDBs: 4, Flags: format.NoSync, OpenLock: OpenMemLock})
			must(t, err)
			return env
		}
		env := open()
		txn := beginW(t, env)
		db, err := txn.OpenDBI([]byte("r"), format.Create)
		must(t, err)
		dups, err := txn.OpenDBI([]byte("d"), format.Create|format.DupSort)
		must(t, err)
		must(t, txn.Put(MainDBI, []byte("A"), []byte("value"), 0))
		must(t, txn.Put(dups, []byte("single"), []byte("A"), 0))
		// Stored in reverse order.
		must(t, txn.SetCompare(db, reverse))
		must(t, txn.SetDupsort(dups, reverse))
		for _, k := range []string{"a", "b", "c", "d", "e"} {
			must(t, txn.Put(db, []byte(k), []byte(k), 0))
			must(t, txn.Put(dups, []byte("many"), []byte(k), 0))
		}
		must(t, txn.Put(dups, []byte("one"), []byte("c"), 0))
		must(t, txn.Commit())
		env.Close()
		// A new environment: the handles start with the default orders.
		env = open()
		defer env.Close()
		r := beginR(t, env)
		defer r.Abort()
		main, err := r.OpenCursor(MainDBI)
		must(t, err)
		db2, err := r.OpenDBI([]byte("r"), 0)
		must(t, err)
		dups2, err := r.OpenDBI([]byte("d"), 0)
		must(t, err)
		if db2 != db || dups2 != dups {
			t.Fatalf("handles %d, %d, want %d, %d", db2, dups2, db, dups)
		}
		c, err := r.OpenCursor(db)
		must(t, err)
		dc, err := r.OpenCursor(dups)
		must(t, err)
		must(t, r.SetCompare(MainDBI, foldCase))
		must(t, r.SetCompare(db, reverse))
		must(t, r.SetDupsort(dups, foldCase))
		get := func(c *Cursor, key, data string, op format.CursorOp) (string, string, error) {
			var d []byte
			if data != "" {
				d = []byte(data)
			}
			k, v, err := c.Get([]byte(key), d, op)
			return string(k), string(v), err
		}
		// The key order.
		if k, _, err := get(main, "a", "", format.SetKey); err != nil || k != "A" {
			t.Fatalf("%v: main SetKey: %q, %v", f, k, err)
		}
		if k, v, err := get(c, "a", "", format.Set); err != nil || k != "a" || v != "a" {
			t.Fatalf("%v: Set: %q, %q, %v", f, k, v, err)
		}
		if k, _, err := get(c, "bb", "", format.SetRange); err != nil || k != "b" {
			t.Fatalf("%v: SetRange: %q, %v", f, k, err)
		}
		if k, _, err := c.Get(nil, nil, format.Next); err != nil || string(k) != "a" {
			t.Fatalf("%v: Next: %q, %v", f, k, err)
		}
		// A key's single item: "a" is "A" now, and sorts before "c".
		if _, v, err := get(dc, "single", "a", format.GetBoth); err != nil || v != "A" {
			t.Fatalf("%v: GetBoth single: %q, %v", f, v, err)
		}
		if _, _, err := get(dc, "one", "D", format.GetBothRange); err != format.NotFound {
			t.Fatalf("%v: GetBothRange D: %v", f, err)
		}
		if _, v, err := get(dc, "one", "B", format.GetBothRange); err != nil || v != "c" {
			t.Fatalf("%v: GetBothRange B: %q, %v", f, v, err)
		}
		// A key's duplicates are searched in the order the cursor was opened
		// with (the default), which they are not stored in (C: MDB_NOTFOUND
		// too); a new cursor uses the new order.
		must(t, r.SetDupsort(dups, reverse))
		if _, _, err := get(dc, "many", "b", format.GetBoth); err != format.NotFound {
			t.Fatalf("%v: GetBoth through the earlier cursor: %v", f, err)
		}
		dc2, err := r.OpenCursor(dups)
		must(t, err)
		if _, v, err := get(dc2, "many", "b", format.GetBoth); err != nil || v != "b" {
			t.Fatalf("%v: GetBoth through a new cursor: %q, %v", f, v, err)
		}
	}
}
