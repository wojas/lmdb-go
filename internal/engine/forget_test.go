package engine

import (
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// noPages fails t if cursor c still holds a page (rcursor.forget).
func noPages(t *testing.T, what string, c *rcursor) {
	t.Helper()
	for i, p := range c.pg {
		if p != nil {
			t.Errorf("%s: page %d of the stack is still set (%d bytes)", what, i, len(p))
		}
	}
	if c.subpage != nil {
		t.Errorf("%s: the sub-page is still set", what)
	}
}

// Objects that outlive their transaction's map hold no pages of it: the
// scratch cursors of an ended read, write or nested transaction (package
// lmdb pools read Txns; the write Txn and a spare child live as long as
// the environment) and a closed or forgotten cursor (package lmdb pools
// read cursors). A page is a slice into the map, which is unmapped once
// the last transaction releases it; a stale slice into an address range
// that the Go heap takes over afterwards (FreeBSD places the heap in such
// holes) corrupts the heap. Found by FuzzEnvFile on FreeBSD 15.1 arm64.
func TestEndedObjectsHoldNoPages(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 4})
	w := beginW(t, env)
	dbi, err := w.OpenDBI([]byte("d"), format.Create|format.DupSort)
	must(t, err)
	for _, v := range []string{"1", "2", "3"} {
		must(t, w.Put(dbi, []byte("k"), []byte(v), 0))
	}
	must(t, w.Put(MainDBI, []byte("a"), []byte("1"), 0))
	if _, err := w.Get(dbi, []byte("k")); err != nil {
		t.Fatal(err)
	}
	ww := w.w
	must(t, w.Commit())
	noPages(t, "write txn getCur", &w.getCur)
	noPages(t, "write txn getX", &w.getX)
	noPages(t, "write txn nameCur", &w.nameCur)
	noPages(t, "write txn rc", &ww.rc)
	noPages(t, "write txn rx", &ww.rx)
	noPages(t, "write txn reclaimCur", &ww.reclaimCur)

	// A nested transaction's object becomes its parent's spare child.
	p := beginW(t, env)
	c, err := env.BeginTxn(p, 0)
	must(t, err)
	if _, err := c.Get(dbi, []byte("k")); err != nil {
		t.Fatal(err)
	}
	cur, err := c.OpenCursor(dbi)
	must(t, err)
	if _, _, err := cur.Get(nil, nil, format.First); err != nil {
		t.Fatal(err)
	}
	must(t, c.Commit())
	noPages(t, "child getCur", &c.getCur)
	noPages(t, "child getX", &c.getX)
	noPages(t, "child rc", &c.w.rc)
	noPages(t, "child rx", &c.w.rx)
	p.Abort()

	r := new(Txn)
	must(t, env.BeginRead(r))
	rdbi, err := r.OpenDBI([]byte("d"), 0)
	must(t, err)
	if _, err := r.Get(rdbi, []byte("k")); err != nil {
		t.Fatal(err)
	}
	if r.getX.subpage == nil {
		t.Fatal("the duplicates are not in a sub-page: the test misses it")
	}
	rc, err := r.OpenCursor(rdbi)
	must(t, err)
	if _, _, err := rc.Get(nil, nil, format.First); err != nil {
		t.Fatal(err)
	}
	rc2, err := r.OpenCursor(MainDBI)
	must(t, err)
	if _, _, err := rc2.Get(nil, nil, format.Last); err != nil {
		t.Fatal(err)
	}
	rc.Forget()
	noPages(t, "forgotten cursor mc", &rc.mc)
	noPages(t, "forgotten cursor mx", &rc.mx)
	rc2.Close()
	noPages(t, "closed cursor mc", &rc2.mc)
	r.Abort()
	noPages(t, "read txn getCur", &r.getCur)
	noPages(t, "read txn getX", &r.getX)
	noPages(t, "read txn nameCur", &r.nameCur)
}
