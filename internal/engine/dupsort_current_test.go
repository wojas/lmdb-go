package engine

// MDB_CURRENT replacing the only item of a DUPSORT key by one too big for
// a node (rlmdb KNOWN_C_BUGS.md #38; found by FuzzWriteModel, FUZZING.md
// "Findings"). C goes through mdb_node_add, which puts the item on
// overflow pages: an F_BIGDATA node in a DUPSORT database (a new key's
// first item of that size goes to a sub-DB instead). The next put of a
// duplicate of the key reads the node's data as if the item were inline:
// the first item becomes the overflow reference followed by whatever
// follows it in the page (or MDB_CORRUPTED when the node is at the end of
// the page), the overflow run leaks and the DB record keeps counting it.
// Reachable through the API in LMDB 1.0 files with any page size
// (MaxKeySize is me_nodemax - 56, so key and item together exceed
// me_nodemax); not in 0.9 files with 4 KiB or larger pages (items and
// keys of at most 511 bytes).
//
// rlmdb's decision, ported: the writer never creates an F_BIGDATA node in
// a DUPSORT database. Such a put deletes the item and puts the new one as
// a fresh put would (a sub-DB, or MDB_BAD_VALSIZE), as for a sub-page item
// that does not fit (#15); and a lone F_BIGDATA item that C 1.0 wrote is
// read through its overflow pages when the key gets a duplicate, and the
// pages are freed (tests/engine TestRepairCBigDataDupsort).

import (
	"bytes"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func TestDupsortCurrentTooBigForNode(t *testing.T) {
	for _, others := range []int{0, 2} {
		env, _ := openTest(t, format.V10, Options{PageSize: 4096, MaxDBs: 4})
		txn, err := env.BeginTxn(nil, 0)
		must(t, err)
		dbi, err := txn.OpenDBI([]byte("t"), format.Create|format.DupSort)
		must(t, err)
		// Inline items after the key's node in the page (others == 2) make
		// the bad read succeed with garbage instead of MDB_CORRUPTED.
		for i := 0; i < others; i++ {
			must(t, txn.Put(dbi, []byte{'z', byte(i)}, bytes.Repeat([]byte{'o'}, 1000), 0))
		}
		k := append([]byte("k51"), bytes.Repeat([]byte("x"), 100)...)
		must(t, txn.Put(dbi, k, []byte("213ddd"), 0))
		c, err := txn.OpenCursor(dbi)
		must(t, err)
		_, _, err = c.Get(k, nil, format.Set)
		must(t, err)
		big := append([]byte("004"), bytes.Repeat([]byte("d"), env.MaxKeySize()-3)...)
		must(t, c.Put(k, big, format.Current))
		must(t, txn.Put(dbi, k, []byte("0"), 0))
		_, d, err := c.Get(k, nil, format.Set)
		if err != nil || !bytes.Equal(d, []byte("0")) {
			t.Fatalf("others %d: first item %q, %v", others, d[:min(len(d), 12)], err)
		}
		_, d, err = c.Get(nil, nil, format.NextDup)
		if err != nil || !bytes.Equal(d, big) {
			t.Fatalf("others %d: second item %d bytes %q..., %v", others, len(d), d[:min(len(d), 12)], err)
		}
		noOverflow(t, txn, dbi)
		must(t, txn.Commit())
		mustAudit(t, env)
	}
}

// noOverflow: dbi holds no overflow pages, so no F_BIGDATA node.
func noOverflow(t *testing.T, txn *Txn, dbi DBI) {
	t.Helper()
	st, err := txn.Stat(dbi)
	must(t, err)
	if st.OverflowPages != 0 {
		t.Fatalf("%d overflow pages in a DUPSORT database", st.OverflowPages)
	}
}

// TestDupsortTooBigReplacements: the other way an item too big for a node
// would replace a key's only one: a put (no MDB_CURRENT) of an item that a
// custom duplicate order calls equal to it, which C overwrites as with
// MDB_CURRENT. It goes to a sub-DB too (Audit counts overflow pages).
func TestDupsortTooBigReplacements(t *testing.T) {
	firstByte := func(a, b []byte) int { return bytes.Compare(a[:min(len(a), 1)], b[:min(len(b), 1)]) }
	t.Run("equal by the dup order", func(t *testing.T) {
		env, _ := openTest(t, format.V10, Options{PageSize: 1024, MaxDBs: 4})
		txn, err := env.BeginTxn(nil, 0)
		must(t, err)
		dbi, err := txn.OpenDBI([]byte("t"), format.Create|format.DupSort)
		must(t, err)
		must(t, txn.SetDupsort(dbi, firstByte))
		k := bytes.Repeat([]byte("k"), 300)
		must(t, txn.Put(dbi, k, []byte("a"), 0))
		big := append([]byte("a"), bytes.Repeat([]byte("x"), 300)...)
		must(t, txn.Put(dbi, k, big, 0)) // equal to "a": replaces it
		must(t, txn.Put(dbi, k, []byte("b"), 0))
		c, err := txn.OpenCursor(dbi)
		must(t, err)
		var got [][]byte
		for _, d, err := c.Get(k, nil, format.Set); err == nil; _, d, err = c.Get(nil, nil, format.NextDup) {
			got = append(got, bytes.Clone(d))
		}
		if len(got) != 2 || !bytes.Equal(got[0], big) || string(got[1]) != "b" {
			t.Fatalf("items %q", got)
		}
		noOverflow(t, txn, dbi)
		must(t, txn.Commit())
		mustAudit(t, env)
	})
}
