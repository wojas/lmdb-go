package engine

// A main DB out of order: a comparator changed between opens, a damaged
// file, or the leaf a C writer leaves after an MDB_CURRENT put with another
// key (KNOWN_C_BUGS.md row 58, which the Go writer no longer writes).
// Searches can then miss keys that are there. The writer must not depend
// on such a search where C does not, so that it writes what C writes
// (tests/engine's TestCgoUnsortedMainChildAbortAndOpen compares the pages
// with C 0.9's; rlmdb's capi_unsorted_main.c traces C 0.9 and 1.0, whose
// records these tests expect). Found by FuzzAPI. Port of rlmdb's
// tests/nested_txn.rs unsorted_main_run and the tests that use it.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// hibitFirst is the default key order with the top bit of every byte
// flipped: bytes from 0x80 sort before the others.
func hibitFirst(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if d := int(a[i]^0x80) - int(b[i]^0x80); d != 0 {
			return d
		}
	}
	return len(a) - len(b)
}

// nameKey is the key of database name in the main DB (with its NUL in
// LMDB 1.0).
func nameKey(f format.Format, name string) []byte {
	if f == format.V10 {
		return append([]byte(name), 0)
	}
	return []byte(name)
}

// unsortedMain begins a write transaction whose main DB is out of order, as
// rlmdb's capi_unsorted_main.c leaves it: "dups" (one item) and the empty
// databases "fixed", "ints" and "plain", then the key 89 8a 8b 8c put in
// an order that sorts it first. A comparator changed within the
// transaction is misuse C does not detect, as it cannot detect one changed
// between opens or a damaged file. In the default order a search for
// "dups" turns left at "fixed", then at 89 8a 8b 8c, and misses it.
// Returns the transaction and the handle of "dups".
func unsortedMain(t *testing.T, env *Env, f format.Format) (*Txn, DBI) {
	t.Helper()
	p := beginW(t, env)
	d, err := p.OpenDBI([]byte("dups"), format.Create)
	must(t, err)
	must(t, p.Put(d, []byte("k"), []byte("v"), 0))
	for _, name := range []string{"fixed", "ints", "plain"} {
		_, err := p.OpenDBI([]byte(name), format.Create)
		must(t, err)
	}
	must(t, p.SetCompare(MainDBI, hibitFirst))
	must(t, p.Put(MainDBI, []byte{0x89, 0x8a, 0x8b, 0x8c}, bytes.Repeat([]byte("v"), 10), format.NoOverwrite))
	must(t, p.SetCompare(MainDBI, nil))
	if _, err := p.Get(MainDBI, nameKey(f, "dups")); err != format.NotFound {
		t.Fatalf("a search for dups: %v; the main DB is in order", err)
	}
	return p, d
}

// mainRecords is the committed main DB's records in page order: the key
// and, for a database's record (48 bytes), its entries and root.
func mainRecords(t *testing.T, env *Env) []string {
	t.Helper()
	r := beginR(t, env)
	defer r.Abort()
	c, err := r.OpenCursor(MainDBI)
	must(t, err)
	defer c.Close()
	var out []string
	for {
		k, v, err := c.Get(nil, nil, format.Next)
		if err == format.NotFound {
			return out
		}
		must(t, err)
		rec := fmt.Sprintf("%q", bytes.TrimSuffix(k, []byte{0}))
		if len(v) == 48 {
			root := binary.NativeEndian.Uint64(v[40:])
			if root == format.PInvalid {
				rec += " empty"
			} else {
				rec += fmt.Sprintf(" %d@%d", binary.NativeEndian.Uint64(v[32:]), root)
			}
		}
		out = append(out, rec)
	}
}

// unsortedMainRun is one of the three runs of rlmdb's
// capi_unsorted_main.c: with child, a nested transaction begins and
// aborts; with reuse, "xa" is created then; the transaction commits. The
// main DB's records must be C's, in C's order (at commit C puts the record
// of every database the transaction changed with one cursor, so each
// search starts on the leaf the last put left it on: a name the search
// misses gets a second record where the search ended).
func unsortedMainRun(t *testing.T, f format.Format, child, reuse bool) {
	env, _ := openTest(t, f, Options{MaxDBs: 8})
	p, d := unsortedMain(t, env, f)
	if child {
		c, err := env.BeginTxn(p, 0)
		must(t, err)
		c.Abort()
	}
	if reuse {
		xa, err := p.OpenDBI([]byte("xa"), format.Create)
		must(t, err)
		// A new database takes a free slot, not the slot of an open one.
		if xa == d {
			t.Fatal("a new database took the slot of an open one")
		}
	}
	// The handle is the parent's still, with its changes.
	if v, err := p.Get(d, []byte("k")); err != nil || string(v) != "v" {
		t.Fatalf("get from dups: %q %v", v, err)
	}
	must(t, p.Commit())
	unsorted := `"\x89\x8a\x8b\x8c"`
	var want []string
	if reuse {
		// "xa" was put at the front (C's search for it ended on the only
		// leaf, and "xa" sorts before its first key); at commit "dups" is
		// found, "fixed" goes to the front and "xa" to the end.
		want = []string{`"fixed" empty`, `"xa" empty`, unsorted, `"dups" 1@3`,
			`"fixed" empty`, `"ints" empty`, `"plain" empty`, `"xa" empty`}
	} else {
		// At commit "dups" is missed and goes to the front.
		want = []string{`"dups" 1@3`, unsorted, `"dups" empty`, `"fixed" empty`,
			`"ints" empty`, `"plain" empty`}
	}
	if got := mainRecords(t, env); !slices.Equal(got, want) {
		t.Fatalf("main DB %q, want %q", got, want)
	}
	if info, err := env.Info(); err != nil || info.LastPgno != 3 {
		t.Fatalf("last page %d, want 3 (%v)", info.LastPgno, err)
	}
	mustAudit(t, env)
}

// A child's abort keeps the handles of the databases its parent has a view
// of, as C does (it closes only the handles of databases the child
// created, mdb_dbis_update): the parent commits, with its changes to them.
// The check for vanished handles (KNOWN_C_BUGS.md row 19) looked "dups" up
// in the parent's main DB, missed it and closed the handle: the commit
// failed with MDB_BAD_DBI, or, once "xa" had taken the slot, lost the put
// and leaked its leaf page.
func TestChildAbortKeepsParentViewsInUnsortedMain(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, reuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/reuse=%v", f, reuse), func(t *testing.T) {
				unsortedMainRun(t, f, true, reuse)
			})
		}
	}
}

// A new database's record goes where C's mdb_dbi_open puts it: its put
// starts from the cursor its lookup left positioned, and mdb_cursor_set
// compares the name with the first key of the page, 89 8a 8b 8c, so "xa"
// goes to the front. A fresh search turns right at "fixed" and puts it
// last.
func TestCreateInUnsortedMainWhereCPutsIt(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		t.Run(f.String(), func(t *testing.T) {
			unsortedMainRun(t, f, false, true)
		})
	}
}
