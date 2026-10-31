package engine

// Cursors of ended write transactions (rlmdb tests/write_review.rs
// cursor_ids_of_an_ended_transaction_are_refused, 1ade420). rlmdb's
// CursorId named an arena slot and its generation, both numbered from 0
// in every arena, so an id of another environment's transaction, or of an
// earlier transaction after the environment replaced its arena, named a
// cursor here. In the port a Cursor holds its own *Txn (no API takes a
// cursor of one transaction and applies it to another), the environment
// keeps one arena for good, and the arena's reset at the end of a
// top-level transaction keeps the slots' generations: a cursor of an ended
// transaction never matches a slot again.

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// mainItems are the items of the main DB as txn sees them.
func mainItems(t *testing.T, txn *Txn) string {
	t.Helper()
	c, err := txn.OpenCursor(MainDBI)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var s string
	for k, v, err := c.Get(nil, nil, format.Next); err == nil; k, v, err = c.Get(nil, nil, format.Next) {
		s += fmt.Sprintf("%s=%s;", k, v)
	}
	return s
}

// assertRefused: c names no cursor of its transaction any more; every
// operation fails with EINVAL, Close does nothing, the data is unchanged.
func assertRefused(t *testing.T, txn *Txn, c *Cursor, what string) {
	t.Helper()
	before := mainItems(t, txn)
	_, _, errGet := c.Get(nil, nil, format.First)
	_, errReserve := c.PutReserve([]byte("y"), 4, 0)
	_, errMulti := c.PutMulti([]byte("z"), []byte("ab"), 1, 0)
	_, errCount := c.Count()
	_, errIsDB := c.IsDB()
	for op, err := range map[string]error{
		"get": errGet, "put": c.Put([]byte("x"), []byte("foreign"), 0), "reserve": errReserve,
		"put multiple": errMulti, "del": c.Del(0), "count": errCount, "is db": errIsDB,
	} {
		if !errors.Is(err, syscall.EINVAL) {
			t.Errorf("%s: %s: %v, want EINVAL", what, op, err)
		}
	}
	c.Close()
	if after := mainItems(t, txn); after != before {
		t.Errorf("%s: data changed: %q -> %q", what, before, after)
	}
}

func TestCursorsOfEndedTxnsRefused(t *testing.T) {
	env, err := Open(t.TempDir(), Options{MapSize: 16 << 20, MaxDBs: 2, Flags: format.NoLock | format.NoSync})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	for _, tc := range []struct {
		n   int
		end string
	}{{17, "commit"}, {1, "commit"}, {17, "abort"}, {1, "abort"}} {
		what := fmt.Sprintf("%d cursors, %s", tc.n, tc.end)
		txn, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		old := make([]*Cursor, tc.n)
		for i := range old {
			if old[i], err = txn.OpenCursor(MainDBI); err != nil {
				t.Fatal(err)
			}
		}
		if tc.end == "commit" {
			must(t, txn.Commit())
		} else {
			txn.Abort()
		}
		// The new cursor first: a put's temporary cursor would take (and
		// free) the first slot before it.
		if txn, err = env.BeginTxn(nil, 0); err != nil {
			t.Fatal(err)
		}
		c, err := txn.OpenCursor(MainDBI)
		if err != nil {
			t.Fatal(err)
		}
		must(t, txn.Put(MainDBI, []byte("k"), []byte(what), 0))
		assertRefused(t, txn, old[0], what)
		if k, v, err := c.Get(nil, nil, format.First); err != nil || string(k) != "k" || string(v) != what {
			t.Fatalf("%s: the new cursor: %q %q %v", what, k, v, err)
		}

		// A nested transaction's own cursor ends with it; the parent's
		// works through it.
		child, err := env.BeginTxn(txn, 0)
		if err != nil {
			t.Fatal(err)
		}
		own, err := child.OpenCursor(MainDBI)
		if err != nil {
			t.Fatal(err)
		}
		for _, cc := range []*Cursor{own, c} {
			if _, v, err := cc.Get(nil, nil, format.Last); err != nil || string(v) != what {
				t.Fatalf("%s: in the child: %q %v", what, v, err)
			}
		}
		must(t, child.Commit())
		assertRefused(t, txn, own, what+": child's cursor")
		must(t, txn.Commit())
	}
}
