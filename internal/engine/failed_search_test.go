package engine

// Moves after a failed exact search (rlmdb KNOWN_C_BUGS.md row 59; port of
// rlmdb's tests/write_api.rs moves_after_a_failed_search). tests/engine's
// TestCgoMovesAfterAFailedSearch runs the shapes C survives against C 0.9.

import (
	"encoding/binary"
	"fmt"
	"slices"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// failedSearchBefore is what the cursor was on before the failed search.
type failedSearchBefore int

const (
	// beforeSame: "c" itself.
	beforeSame failedSearchBefore = iota
	// beforeOther: "e", with duplicates of 8 bytes (those of "c" have 4).
	beforeOther
	// beforeFresh: nothing, a new cursor.
	beforeFresh
	// beforeDeleted: "b", which mdb_del then deleted, moving the cursor
	// onto "c".
	beforeDeleted
)

// putACE puts "a" and "c" (items 10, 20, ..., 10 n, 4 bytes) and "e" (m
// items 0, 1, ... of 8 bytes if m > 0, else one of 4 bytes) into a new
// database "t".
func putACE(t *testing.T, txn *Txn, flags uint32, n, m int) DBI {
	t.Helper()
	db, err := txn.OpenDBI([]byte("t"), flags|format.Create)
	must(t, err)
	must(t, txn.Put(db, []byte("a"), be32(1), 0))
	for i := 1; i <= n; i++ {
		must(t, txn.Put(db, []byte("c"), be32(uint32(10*i)), 0))
	}
	if m == 0 {
		must(t, txn.Put(db, []byte("e"), be32(1), 0))
	}
	for i := range m {
		must(t, txn.Put(db, []byte("e"), binary.BigEndian.AppendUint64(nil, uint64(i)), 0))
	}
	return db
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// getOK is c.Get with MDB_NOTFOUND as ok false; other errors fail the test.
func getOK(t *testing.T, c *Cursor, key []byte, op format.CursorOp, what string) ([]byte, []byte, bool) {
	t.Helper()
	k, v, err := c.Get(key, nil, op)
	if err == format.NotFound {
		return nil, nil, false
	}
	if err != nil {
		t.Fatalf("%s: op %d: %v", what, op, err)
	}
	return k, v, true
}

// positionFor positions c as before says (for beforeDeleted the caller
// deletes "b").
func positionFor(t *testing.T, c *Cursor, before failedSearchBefore) {
	t.Helper()
	key := map[failedSearchBefore]string{beforeSame: "c", beforeOther: "e", beforeDeleted: "b"}[before]
	if before == beforeFresh {
		return
	}
	if _, _, ok := getOK(t, c, []byte(key), format.Set, "position"); !ok {
		t.Fatalf("position on %q: not found", key)
	}
}

// missB searches "b" and checks what C's argument checks say about the
// cursor now; MDB_GET_CURRENT only if current (C's clears the
// sub-cursor's C_DEL mark, also when it fails).
func missB(t *testing.T, c *Cursor, current bool, what string) {
	t.Helper()
	if _, _, ok := getOK(t, c, []byte("b"), format.Set, what); ok {
		t.Fatalf("%s: b found", what)
	}
	// The sub-cursor is not on an item: C's EINVAL, kept.
	if _, err := c.Count(); err != syscall.EINVAL {
		t.Fatalf("%s: count: %v", what, err)
	}
	if current {
		if _, _, err := c.Get(nil, nil, format.GetCurrent); err != syscall.EINVAL {
			t.Fatalf("%s: current: %v", what, err)
		}
	}
}

// walkC is op and then the same move while on "c" (MDB_NEXT_MULTIPLE:
// pages of items): every item of all once, in order; the count after the
// first move; the key the walk ends on.
func walkC(t *testing.T, c *Cursor, op format.CursorOp, all []uint32, what string) {
	t.Helper()
	back := op == format.Prev || op == format.PrevDup
	var seen []uint32
	k, d, ok := getOK(t, c, nil, op, what)
	if n, err := c.Count(); err != nil || n != uint64(len(all)) {
		t.Fatalf("%s: count %d (%v), want %d", what, n, err, len(all))
	}
	for ok && string(k) == "c" {
		if len(d) == 0 || len(d)%4 != 0 {
			t.Fatalf("%s: item %x", what, d)
		}
		for i := 0; i < len(d); i += 4 {
			seen = append(seen, binary.BigEndian.Uint32(d[i:]))
		}
		k, d, ok = getOK(t, c, nil, op, what)
	}
	if back {
		slices.Reverse(seen)
	}
	if !slices.Equal(seen, all) {
		t.Fatalf("%s: walk %v, want %v", what, seen, all)
	}
	end := ""
	switch op {
	case format.Next:
		end = "e"
	case format.Prev:
		end = "a"
	}
	if got := map[bool]string{true: string(k), false: ""}[ok]; got != end {
		t.Fatalf("%s: after the walk on %q, want %q", what, got, end)
	}
}

// MDB_SET of "b" leaves the cursor on the node of "c", whose duplicates the
// test varies, with the sub-cursor reset. The next MDB_NEXT, MDB_PREV,
// MDB_NEXT_DUP, MDB_PREV_DUP or MDB_NEXT_MULTIPLE enters those duplicates;
// C enters them with the record its sub-cursor last read, which here is
// stale. Either another cursor added duplicates since then (C counts too
// few; after a sub-DB's root split it walks the old root, and a sub-page
// the put moved crashes it), or it is another key's record ("e": C walks
// its duplicates), or there is none (the cursor never was on duplicates,
// row 6: C asserts); a sub-cursor that a delete marked (C_DEL, row 29: "b"
// was deleted under the cursor, moving it onto "c") keeps the mark, and C
// returns the first duplicate twice. The port sets the sub-cursor up from
// the node, so the walk sees every duplicate of "c" once, in order, the
// count is right, and the walk ends on the key next to "c" ("a" or "e") as
// C's walk does with a right record (the cases "the record is right" have
// one: there the port and C agree). The count and MDB_GET_CURRENT right
// after the failed search stay C's EINVAL. Write transactions in both
// formats, 1.0 at 256 B and 1 KiB pages too, MDB_DUPSORT and
// MDB_DUPFIXED; read transactions for the stale records that need no
// other cursor.
func TestMovesAfterAFailedSearch(t *testing.T) {
	shapes := []struct {
		f  format.Format
		ps int
	}{{format.V09, 0}, {format.V10, 0}, {format.V10, 256}, {format.V10, 1024}}
	for _, s := range shapes {
		for _, flags := range []uint32{format.DupSort | format.DupFixed, format.DupSort} {
			t.Run(fmt.Sprintf("%v/ps=%d/flags=%#x", s.f, s.ps, flags), func(t *testing.T) {
				movesAfterFailedSearch(t, s.f, s.ps, flags)
			})
		}
	}
}

// failedSearchCase is one case of movesAfterFailedSearch.
type failedSearchCase struct {
	what     string
	n        int                // items of "c"
	before   failedSearchBefore // the cursor before the search
	m        int                // duplicates of "e" for beforeOther
	add      []uint32           // items another cursor adds
	multiple bool               // ... with MDB_MULTIPLE
}

// subDBThresholds is how many items "c" has (in a database put by putACE)
// when it moves to a sub-DB (s) and when that sub-DB's leaf splits (t).
func subDBThresholds(t *testing.T, env *Env, flags uint32) (s, split int) {
	txn := beginW(t, env)
	defer txn.Abort()
	db := putACE(t, txn, flags, 0, 0)
	for i := 1; ; i++ {
		st, err := txn.Stat(db)
		must(t, err)
		must(t, txn.Put(db, []byte("c"), be32(uint32(10*i)), 0))
		st2, err := txn.Stat(db)
		must(t, err)
		if st2.LeafPages != st.LeafPages {
			if s > 0 {
				return s, i
			}
			s = i
		}
	}
}

// movesAfterFailedSearch is one shape of TestMovesAfterAFailedSearch.
func movesAfterFailedSearch(t *testing.T, f format.Format, ps int, flags uint32) {
	env, _ := openTest(t, f, Options{MaxDBs: 4, PageSize: ps})
	fixed := flags&format.DupFixed != 0
	s, split := subDBThresholds(t, env, flags)
	if s <= 4 || split <= s+1 {
		t.Fatalf("sub-DB at %d, split at %d", s, split)
	}
	// A sub-DB of one leaf.
	sub := s + 1
	odd := func(n int) []uint32 {
		var out []uint32
		for i := range n {
			out = append(out, uint32(10*i+5))
		}
		return out
	}
	cases := []failedSearchCase{
		{what: "sub-page, another cursor adds one", n: 3, before: beforeSame, add: []uint32{25}},
		{what: "sub-page becomes a sub-DB", n: s - 1, before: beforeSame, add: []uint32{5, 15}},
		{what: "sub-DB, another cursor adds one", n: sub, before: beforeSame, add: []uint32{25}},
		{what: "sub-DB's root splits", n: sub, before: beforeSame, add: odd(split - s + 1)},
		{what: "another key's sub-page", n: 3, before: beforeOther, m: 2},
		{what: "another key's sub-DB", n: 3, before: beforeOther, m: s + 1},
		{what: "a sub-DB, another key's sub-page", n: sub, before: beforeOther, m: 2},
		{what: "a sub-page, never on duplicates", n: 3, before: beforeFresh},
		{what: "a sub-DB, never on duplicates", n: sub, before: beforeFresh},
		{what: "sub-page, a delete marked it", n: 3, before: beforeDeleted},
		{what: "sub-DB, a delete marked it", n: sub, before: beforeDeleted},
		{what: "sub-page, the record is right", n: 3, before: beforeSame},
		{what: "sub-DB, the record is right", n: sub, before: beforeSame},
	}
	ops := []format.CursorOp{format.Next, format.Prev, format.NextDup, format.PrevDup}
	if fixed {
		cases = append(cases,
			failedSearchCase{what: "sub-page, MDB_MULTIPLE of two", n: 3, before: beforeSame, add: []uint32{15, 25}, multiple: true},
			failedSearchCase{what: "sub-DB's root splits, MDB_MULTIPLE", n: sub, before: beforeSame, add: odd(split - s + 1), multiple: true})
		ops = append(ops, format.NextMultiple)
	}
	for _, tc := range cases {
		var all []uint32
		for i := 1; i <= tc.n; i++ {
			all = append(all, uint32(10*i))
		}
		all = append(all, tc.add...)
		slices.Sort(all)
		// Each move in a nested transaction (aborted) of one that holds the
		// data, so that the data is put once.
		txn := beginW(t, env)
		db := putACE(t, txn, flags, tc.n, tc.m)
		deleted := tc.before == beforeDeleted
		if deleted {
			must(t, txn.Put(db, []byte("b"), be32(1), 0))
		}
		for _, op := range ops {
			what := fmt.Sprintf("%s, op %d", tc.what, op)
			child, err := env.BeginTxn(txn, 0)
			must(t, err)
			c0, err := child.OpenCursor(db)
			must(t, err)
			c1, err := child.OpenCursor(db)
			must(t, err)
			positionFor(t, c0, tc.before)
			if deleted {
				must(t, child.Del(db, []byte("b"), nil))
			}
			missB(t, c0, !deleted, what)
			if tc.multiple {
				var items []byte
				for _, v := range tc.add {
					items = append(items, be32(v)...)
				}
				stored, err := c1.PutMulti([]byte("c"), items, 4, 0)
				must(t, err)
				if stored != len(tc.add) {
					t.Fatalf("%s: %d items stored", what, stored)
				}
			} else {
				for _, v := range tc.add {
					must(t, c1.Put([]byte("c"), be32(v), 0))
				}
			}
			walkC(t, c0, op, all, what)
			// MDB_PREV_MULTIPLE does not enter the duplicates: C's
			// MDB_NOTFOUND.
			if fixed && op == format.Next {
				what := what + ", then MDB_PREV_MULTIPLE"
				c2, err := child.OpenCursor(db)
				must(t, err)
				if !deleted {
					positionFor(t, c2, tc.before)
				}
				missB(t, c2, true, what)
				if _, _, err := c2.Get(nil, nil, format.PrevMultiple); err != format.NotFound {
					t.Fatalf("%s: %v", what, err)
				}
			}
			child.Abort()
		}
		if len(tc.add) > 0 || deleted {
			txn.Abort()
			continue
		}
		// The stale records that need no other cursor, in read
		// transactions.
		must(t, txn.Commit())
		for _, op := range ops {
			what := fmt.Sprintf("%s, op %d, read transaction", tc.what, op)
			r := beginR(t, env)
			c, err := r.OpenCursor(db)
			must(t, err)
			positionFor(t, c, tc.before)
			missB(t, c, true, what)
			walkC(t, c, op, all, what)
			c.Close()
			r.Abort()
		}
		txn = beginW(t, env)
		must(t, txn.Drop(db, true))
		must(t, txn.Commit())
	}
}
