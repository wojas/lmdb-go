package engine

// A main DB that a caller put out of order: a cursor put with MDB_CURRENT
// and a key other than the item's replaces the item's key, without a check
// in C, the port or rlmdb. Searches can then miss keys that are there. The
// writer must not depend on such a search where C does not, so that it
// writes what C writes. Found by FuzzAPI.

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// unsortedMain begins a write transaction with the database "dups" (one
// put) and the empty databases extra, then breaks the main DB's order: an
// item 00 01 02 becomes 89 8a 8b 8c (a key after every DB name) with an
// MDB_CURRENT put. Returns the transaction and the handle of "dups".
func unsortedMain(t *testing.T, env *Env, extra ...string) (*Txn, DBI) {
	t.Helper()
	p := beginW(t, env)
	d, err := p.OpenDBI([]byte("dups"), format.Create)
	must(t, err)
	must(t, p.Put(d, []byte("k"), []byte("v"), 0))
	for _, name := range extra {
		_, err := p.OpenDBI([]byte(name), format.Create)
		must(t, err)
	}
	c, err := p.OpenCursor(MainDBI)
	must(t, err)
	defer c.Close()
	must(t, c.Put([]byte{0, 1, 2}, nil, 0))
	must(t, c.Put([]byte{0x89, 0x8a, 0x8b, 0x8c}, bytes.Repeat([]byte("v"), 10), format.Current))
	return p, d
}

// A child's abort keeps the handles of the databases its parent has a view
// of. C closes only DB_NEW handles and writes the record of every changed
// database at commit (here a second "dups" record). The check for vanished
// handles (KNOWN_C_BUGS.md row 19) looked "dups" up in the parent's main
// DB, 89 8a 8b 8c, dups, missed it and closed the handle: the commit failed
// with MDB_BAD_DBI, or, once a new database had taken the slot, lost the
// put and leaked its leaf page.
func TestChildAbortKeepsParentViewsInUnsortedMain(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, reuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/reuse=%v", f, reuse), func(t *testing.T) {
				env, _ := openTest(t, f, Options{MaxDBs: 4})
				p, d := unsortedMain(t, env)
				child, err := env.BeginTxn(p, 0)
				must(t, err)
				child.Abort()
				if reuse {
					xa, err := p.OpenDBI([]byte("xa"), format.Create)
					must(t, err)
					if xa == d {
						t.Fatal("a new database took the slot of an open one")
					}
				}
				if v, err := p.Get(d, []byte("k")); err != nil || string(v) != "v" {
					t.Fatalf("get after the child's abort: %q %v", v, err)
				}
				must(t, p.Commit())
				mustAudit(t, env)
			})
		}
	}
}
