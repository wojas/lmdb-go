package engine

import (
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// mdb_cursor_count of a cursor on a key after another cursor (or a plain
// put, or MDB_MULTIPLE) put duplicates of that key: converting the key's
// single item, into its sub-page, growing it, promoting it to a sub-DB,
// into the sub-DB, splitting the sub-DB's leaf; before and after the
// cursor's item. The count is the key's (C's is the one before the put,
// KNOWN_C_BUGS.md row 14; tests/engine's TestCgoCountAfterAnotherCursorsPut
// runs the same against C 0.9), and the cursor stays on its item. Both
// formats, 1.0 at 256 B and 1 KiB pages too, MDB_DUPSORT and
// MDB_DUPFIXED. Port of rlmdb's tests/write_api.rs
// count_after_another_cursors_put (a508e1b).
func TestCountAfterAnotherCursorsPut(t *testing.T) {
	shapes := []struct {
		f  format.Format
		ps int
	}{{format.V09, 0}, {format.V10, 0}, {format.V10, 256}, {format.V10, 1024}}
	for _, s := range shapes {
		for _, flags := range []uint32{format.DupSort | format.DupFixed, format.DupSort} {
			t.Run(fmt.Sprintf("%v/ps=%d/flags=%#x", s.f, s.ps, flags), func(t *testing.T) {
				countAfterPut(t, s.f, s.ps, flags)
			})
		}
	}
}

// countAfterPut is one shape of TestCountAfterAnotherCursorsPut.
func countAfterPut(t *testing.T, f format.Format, ps int, flags uint32) {
	env, _ := openTest(t, f, Options{MaxDBs: 200, PageSize: ps})
	fixed := flags&format.DupFixed != 0
	txn := beginW(t, env)
	defer txn.Abort()
	seq := 0
	// A new DB with "a" and "z" around "k", which gets items 10, 20, ...:
	// n of them, or (n == 0) until a put adds a leaf page for the from-th
	// time (1: "k" moves to a sub-DB; 2: its leaf splits); the DB and the
	// number of items.
	fill := func(n, from int) (DBI, int) {
		seq++
		db, err := txn.OpenDBI(fmt.Appendf(nil, "count%d", seq), flags|format.Create)
		must(t, err)
		for _, k := range []string{"a", "z"} {
			must(t, txn.Put(db, []byte(k), be32(1), 0))
		}
		added := 0
		for i := 1; ; i++ {
			st, err := txn.Stat(db)
			must(t, err)
			must(t, txn.Put(db, []byte("k"), be32(uint32(10*i)), 0))
			if n == 0 {
				st2, err := txn.Stat(db)
				must(t, err)
				if st2.LeafPages != st.LeafPages {
					if added++; added == from {
						return db, i
					}
				}
			}
			if i == n {
				return db, n
			}
		}
	}
	_, s := fill(0, 1)
	_, split := fill(0, 2)
	if s <= 4 || split <= s {
		t.Fatalf("sub-DB at %d, split at %d", s, split)
	}
	cases := []struct {
		what  string
		n, on int
		v     uint32
	}{
		{"single item, after", 1, 10, 15},
		{"single item, before", 1, 10, 5},
		{"sub-page, after", 3, 20, 25},
		{"sub-page, before", 3, 20, 15},
		{"sub-page grows", 4, 20, 45},
		{"to a sub-DB, after", s - 1, 20, uint32(10*s + 5)},
		{"to a sub-DB, before", s - 1, 20, 5},
		{"sub-DB, after", s + 5, 20, 25},
		{"sub-DB, before", s + 5, 20, 15},
		{"sub-DB leaf splits, after", split - 1, 10, uint32(10*split + 5)},
		{"sub-DB leaf splits, before", split - 1, 10 * (split - 1), 5},
	}
	hows := 2
	if fixed {
		hows = 3
	}
	dup := func(d []byte, err error) (uint32, bool) {
		if err == format.NotFound {
			return 0, false
		}
		must(t, err)
		return binary.BigEndian.Uint32(d), true
	}
	for how := range hows {
		for _, tc := range cases {
			what := fmt.Sprintf("%s, put %d", tc.what, how)
			db, _ := fill(tc.n, 0)
			a, err := txn.OpenCursor(db)
			must(t, err)
			b, err := txn.OpenCursor(db)
			must(t, err)
			_, _, err = a.Get([]byte("k"), be32(uint32(tc.on)), format.GetBoth)
			must(t, err)
			news := []uint32{tc.v}
			switch how {
			case 0:
				must(t, b.Put([]byte("k"), be32(tc.v), 0))
			case 1:
				must(t, txn.Put(db, []byte("k"), be32(tc.v), 0))
			default:
				news = []uint32{tc.v, tc.v + 1, tc.v + 2}
				var items []byte
				for _, x := range news {
					items = append(items, be32(x)...)
				}
				stored, err := b.PutMulti([]byte("k"), items, 4, 0)
				must(t, err)
				if stored != 3 {
					t.Fatalf("%s: %d stored", what, stored)
				}
			}
			var all []uint32
			for i := 1; i <= tc.n; i++ {
				all = append(all, uint32(10*i))
			}
			all = append(all, news...)
			slices.Sort(all)
			if n, err := a.Count(); err != nil || n != uint64(len(all)) {
				t.Fatalf("%s: count %d (%v), want %d", what, n, err, len(all))
			}
			_, d, err := a.Get(nil, nil, format.GetCurrent)
			if cur, ok := dup(d, err); !ok || cur != uint32(tc.on) {
				t.Fatalf("%s: current %d (%v)", what, cur, ok)
			}
			at := slices.Index(all, uint32(tc.on))
			_, d, err = a.Get(nil, nil, format.NextDup)
			next, ok := dup(d, err)
			if want := at+1 < len(all); ok != want || (ok && next != all[at+1]) {
				t.Fatalf("%s: next dup %d (%v)", what, next, ok)
			}
			if n, err := a.Count(); err != nil || n != uint64(len(all)) {
				t.Fatalf("%s: count after next dup %d (%v), want %d", what, n, err, len(all))
			}
			_, d, err = a.Get(nil, nil, format.PrevDup)
			prev, ok := dup(d, err)
			var want uint32
			wantOK := true
			switch {
			case at+1 < len(all):
				want = uint32(tc.on)
			case at > 0:
				want = all[at-1]
			default:
				wantOK = false
			}
			if ok != wantOK || prev != want {
				t.Fatalf("%s: previous dup %d (%v), want %d (%v)", what, prev, ok, want, wantOK)
			}
			a.Close()
			b.Close()
		}
	}
	must(t, txn.Commit())
}
