package engine

// MDB_CURRENT replacing a duplicate in a sub-DB by an item that sorts where
// the old one is (found by FuzzWriteModel, FUZZING.md "Findings"). C keeps
// the new item in the old one's leaf, at its index, and so does not look
// at the branch keys around the leaf, which need not be its items
// (deleting a leaf's first item leaves the key above it):
//
//   - the first item of a leaf other than the leftmost, replaced by an item
//     of another size that sorts before the branch key above the leaf (C
//     0.9's ITS#8447 path, new_ksize, which lacks the fix_parent of an item
//     of the same size);
//   - the last item of a leaf other than the rightmost, replaced by an item
//     at or after the next leaf's branch key (any size, C 0.9 and 1.0).
//
// MDB_GET_BOTH then cannot find the new item, and a later put of an item
// between it and that key lands in the other leaf, out of order. The Go
// writer deletes the item and puts the new one there; tests/engine
// (TestDifferential*CurrentDupOutsideLeaf) compares the files with C's
// mdb_cursor_del + mdb_cursor_put.

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// TestDupsortCurrentOutsideLeaf replaces every item of a key whose
// duplicates span several leaves, in turn, by items between it and its
// neighbours, also after deleting the item after it (which leaves a stale
// branch key where that was the first of its leaf), then puts an item
// between the new one and the old ones: every item must be found and the
// duplicates stay in order.
func TestDupsortCurrentOutsideLeaf(t *testing.T) {
	shapes := []struct {
		f         format.Format
		ps, n, sz int
	}{
		{format.V09, 0, 160, min(os.Getpagesize()/24, 509)},
		{format.V10, 256, 60, 10},
		{format.V10, 1024, 60, 42},
	}
	for _, sh := range shapes {
		for _, flags := range []uint32{format.DupSort, format.DupSort | format.ReverseDup, format.DupSort | format.DupFixed} {
			t.Run(fmt.Sprintf("%v/%d/%#x", sh.f, sh.ps, flags), func(t *testing.T) {
				currentOutsideLeaf(t, sh.f, sh.ps, sh.n, sh.sz, flags)
			})
		}
	}
}

func currentOutsideLeaf(t *testing.T, f format.Format, ps, n, sz int, flags uint32) {
	env, _ := openTest(t, f, Options{PageSize: ps, MaxDBs: 4})
	dcmp, _ := format.DefaultDupCmp(flags)
	fixed := flags&format.DupFixed != 0
	// enc stores s so that the duplicate order is s's byte order.
	enc := func(s string) []byte {
		b := []byte(s)
		if flags&format.ReverseDup != 0 {
			slices.Reverse(b)
		}
		return b
	}
	item := func(j int) string {
		s := fmt.Sprintf("b%04d", j)
		return s + string(bytes.Repeat([]byte("p"), sz-len(s)))
	}
	// last replaces the last byte of s.
	last := func(s string, c byte) string { return s[:len(s)-1] + string(c) }
	key := []byte("k")
	txn, err := env.BeginTxn(nil, 0)
	must(t, err)
	dbi, err := txn.OpenDBI([]byte("d"), format.Create|flags)
	must(t, err)
	for j := 0; j < n; j++ {
		must(t, txn.Put(dbi, key, enc(item(j)), 0))
	}
	must(t, txn.Commit())
	count := n // items committed
	// try replaces item j by nw (after deleting item del if del >= 0),
	// puts between (if any), and checks the duplicates; committed if
	// commit.
	try := func(j, del int, nw, between string, commit bool) {
		t.Helper()
		what := fmt.Sprintf("item %d -> %q (deleted %d)", j, nw, del)
		txn, err := env.BeginTxn(nil, 0)
		must(t, err)
		defer txn.Abort()
		c, err := txn.OpenCursor(dbi)
		must(t, err)
		defer c.Close()
		want := count
		if del >= 0 {
			_, _, err = c.Get(key, enc(item(del)), format.GetBoth)
			must(t, err)
			must(t, c.Del(0))
			want--
		}
		_, _, err = c.Get(key, enc(item(j)), format.GetBoth)
		must(t, err)
		must(t, c.Put(key, enc(nw), format.Current))
		if _, d, err := c.Get(nil, nil, format.GetCurrent); err != nil || !bytes.Equal(d, enc(nw)) {
			t.Fatalf("%s: cursor on %q, %v", what, d, err)
		}
		if _, _, err := c.Get(key, enc(nw), format.GetBoth); err != nil {
			t.Fatalf("%s: MDB_GET_BOTH: %v", what, err)
		}
		if between != "" {
			must(t, txn.Put(dbi, key, enc(between), 0))
			want++
		}
		var got [][]byte
		for _, d, err := c.Get(key, nil, format.Set); err == nil; _, d, err = c.Get(nil, nil, format.NextDup) {
			got = append(got, bytes.Clone(d))
		}
		if len(got) != want || !slices.IsSortedFunc(got, dcmp.Compare) {
			t.Fatalf("%s: %d items (want %d), sorted %v", what, len(got), want, slices.IsSortedFunc(got, dcmp.Compare))
		}
		if commit {
			c.Close()
			must(t, txn.Commit())
			mustAudit(t, env)
			count = want
		}
	}
	for j := 1; j < n-1; j++ {
		prev := item(j - 1)
		// Between item j-1 and item j: the same size, and (without
		// MDB_DUPFIXED) shorter and longer.
		try(j, -1, last(item(j), 'o'), "", false)
		if !fixed {
			try(j, -1, prev[:5]+"q", prev[:5]+"r", false)
			try(j, -1, prev+"q", prev+"r", false)
		}
		// Item j-1 after deleting item j, by items between item j-2 and
		// item j+1, at or after the old item j (the stale branch key where
		// item j was the first of its leaf); then item j put back.
		try(j-1, j, item(j), "", false)
		try(j-1, j, last(item(j), 'q'), item(j), false)
		if !fixed {
			try(j-1, j, item(j)[:5]+"q", item(j), false)
			try(j-1, j, item(j)+"q", item(j), false)
		}
	}
	// One of each, committed and audited.
	try(n/2, n/2+1, item(n/2+1), "", true)
	if !fixed {
		try(n/3, -1, item(n/3 - 1)[:5]+"q", "", true)
	}
}
