package engine

// A known bug found by FuzzWriteModel, shared with C LMDB 1.0 and rlmdb
// (FUZZING.md, "Findings"): MDB_CURRENT replacing the only item of a
// DUPSORT key by one too big for a node goes through mdb_node_add, which
// puts it on overflow pages (F_BIGDATA in a DUPSORT database; a new key's
// first item of that size goes to a sub-DB instead). The next put of a
// duplicate of the key reads the F_BIGDATA node's data as if the item
// were inline: the first item becomes the overflow reference followed by
// whatever follows it in the page (or MDB_CORRUPTED when the node is at
// the end of the page), the overflow run leaks and the DB record keeps
// counting it. Reachable through the API in LMDB 1.0 files with any page
// size (MaxKeySize is me_nodemax - 56, so key and item together exceed
// me_nodemax); not in 0.9 files with 4 KiB or larger pages (items and
// keys of at most 511 bytes).

import (
	"bytes"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func TestDupsortCurrentTooBigForNode(t *testing.T) {
	t.Skip("known bug shared with C 1.0 and rlmdb (FUZZING.md, Findings); remove this Skip with the fix")
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
		must(t, txn.Commit())
		mustAudit(t, env)
	}
}
