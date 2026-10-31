package engine

// A file C 1.0 wrote with an F_BIGDATA node in a DUPSORT database, which
// the Go writer repairs (rlmdb KNOWN_C_BUGS.md #38).

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// TestRepairCBigDataDupsort: C 1.0 stores an MDB_CURRENT item too big for
// a node that replaces a DUPSORT key's only item on overflow pages (an
// F_BIGDATA node in a DUPSORT database; C's next duplicate of the key reads
// the node as if the item were inline and leaks the run). The Go writer
// never writes such a node (internal/engine TestDupsortCurrentTooBigForNode);
// on a file that has one it reads the item through its overflow pages when
// the key gets a duplicate, and frees them: the key holds both items, the
// database no overflow page, and the file passes C's page audit and Go's.
func TestRepairCBigDataDupsort(t *testing.T) {
	const ps = 4096
	o := oracle10(t, ps)
	dir := t.TempDir()
	k := append([]byte("k51"), bytes.Repeat([]byte("x"), 100)...)
	// 1.0's largest item at 4 KiB pages (me_nodemax - 56): with the key, too
	// big for a node.
	big := append([]byte("004"), bytes.Repeat([]byte("d"), 1978-3)...)
	c := startC(t, o, dir, format.DupSort, 1, 64<<20)
	for _, op := range []string{
		fmt.Sprintf("p:%s:%s:0", hexs(k), hexs([]byte("213ddd"))),
		fmt.Sprintf("g:0:%d:%s", format.Set, hexs(k)),
		fmt.Sprintf("cp:0:%s:%s:%d", hexs(k), hexs(big), format.Current),
	} {
		line, ok := c.step(op)
		if f := strings.Fields(line); !ok || len(f) < 2 || f[1] != "rc=0" {
			_, errs := c.finish()
			t.Fatalf("C: %s: %q; stderr %s", op[:2], line, errs)
		}
	}
	if commit, errs := c.finish(); commit != "commit rc=0" {
		t.Fatalf("C commit: %q; stderr %s", commit, errs)
	}

	env, err := eng.Open(dir, eng.Options{MaxDBs: 8, Flags: format.NoLock | format.NoSync})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	txn, err := env.BeginTxn(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Abort()
	dbi, err := txn.OpenDBI([]byte("t"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := txn.Stat(dbi); err != nil || st.OverflowPages == 0 {
		t.Fatalf("C's file: %+v %v (want an item on overflow pages)", st, err)
	}
	if v, err := txn.Get(dbi, k); err != nil || !bytes.Equal(v, big) {
		t.Fatalf("C's item: %d bytes, %v", len(v), err)
	}
	if err := txn.Put(dbi, k, []byte("0"), 0); err != nil {
		t.Fatal(err)
	}
	cur, err := txn.OpenCursor(dbi)
	if err != nil {
		t.Fatal(err)
	}
	var items [][]byte
	for _, d, err := cur.Get(k, nil, format.Set); err == nil; _, d, err = cur.Get(nil, nil, format.NextDup) {
		items = append(items, bytes.Clone(d))
	}
	if len(items) != 2 || string(items[0]) != "0" || !bytes.Equal(items[1], big) {
		t.Fatalf("items after the duplicate: %d", len(items))
	}
	if st, err := txn.Stat(dbi); err != nil || st.OverflowPages != 0 {
		t.Fatalf("after the duplicate: %+v %v (want no overflow page)", st, err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := env.Audit(); err != nil {
		t.Fatalf("Go audit: %v", err)
	}
	env.Close()
	if rc, out, errs := run(t, o.peer, "check-freelist", dir); rc != 0 {
		t.Fatalf("C's page audit of the repaired file:\n%s\n%s", out, errs)
	}
	_, dump, _ := run(t, o.dump, "-s", "t", dir)
	if !strings.Contains(dump, " "+hexs([]byte("0"))+"\n") || !strings.Contains(dump, " "+hexs(big)+"\n") {
		t.Fatalf("mdb_dump10 of the repaired file lacks an item:\n%.400s", dump)
	}
}
