package engine

// Nested transaction edge cases (port of parts of rlmdb's
// tests/nested_txn.rs; the lock-step differential test covers nested
// transactions with cursors against C).

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func TestChildHandlesAndFailures(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 4})
	p := beginW(t, env)
	a, _ := p.OpenDBI([]byte("a"), format.Create)
	must(t, p.Put(a, []byte("k"), []byte("v"), 0))

	// A DB created in an aborted child: the handle is closed.
	c, err := env.BeginTxn(p, 0)
	must(t, err)
	b, _ := c.OpenDBI([]byte("b"), format.Create)
	must(t, c.Put(b, []byte("x"), []byte("y"), 0))
	c.Abort()
	if _, err := p.Get(b, []byte("x")); err == nil {
		t.Fatal("handle of a DB created in an aborted child works")
	}
	_, err = p.OpenDBI([]byte("b"), 0)
	expectErr(t, err, format.NotFound, "DB of an aborted child")

	// A DB created and written by committed children.
	c, _ = env.BeginTxn(p, 0)
	b, _ = c.OpenDBI([]byte("b"), format.Create)
	must(t, c.Put(b, []byte("x"), []byte("y"), 0))
	g, _ := env.BeginTxn(c, 0)
	must(t, g.Put(a, []byte("k"), []byte("g"), 0))
	must(t, g.Commit())
	must(t, c.Commit())
	if v, _ := p.Get(b, []byte("x")); string(v) != "y" {
		t.Fatal(string(v))
	}
	if v, _ := p.Get(a, []byte("k")); string(v) != "g" {
		t.Fatal(string(v))
	}

	// Errors that do not fail a transaction leave the child usable.
	c, _ = env.BeginTxn(p, 0)
	expectErr(t, c.Put(a, []byte{}, []byte("v"), 0), format.BadValSize, "empty key in child")
	must(t, c.Put(a, []byte("k2"), []byte("v2"), 0))
	c.Abort()
	if _, err := p.Get(a, []byte("k2")); err != format.NotFound {
		t.Fatal(err)
	}
	must(t, p.Commit())
	r := beginR(t, env)
	defer r.Abort()
	b, err = r.OpenDBI([]byte("b"), 0)
	must(t, err)
	if v, _ := r.Get(b, []byte("x")); string(v) != "y" {
		t.Fatal(string(v))
	}
	if err := env.Audit(); err != nil {
		t.Fatal(err)
	}
}

func TestNestedOnFailedParentIsRefused(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MapSize: 1 << 20})
	p := beginW(t, env)
	defer p.Abort()
	var err error
	for i := uint32(0); err == nil; i++ {
		err = p.Put(MainDBI, binary.BigEndian.AppendUint32(nil, i), make([]byte, 1000), 0)
	}
	expectErr(t, err, format.MapFull, "fill")
	_, err = env.BeginTxn(p, 0)
	expectErr(t, err, format.BadTxn, "nested on a failed parent")
}

func TestFailedChildCommitFailsTheParent(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MapSize: 1 << 20})
	p := beginW(t, env)
	must(t, p.Put(MainDBI, []byte("a"), []byte("b"), 0))
	c, _ := env.BeginTxn(p, 0)
	for i := uint32(0); c.Put(MainDBI, binary.BigEndian.AppendUint32(nil, i), make([]byte, 1000), 0) == nil; i++ {
	}
	expectErr(t, c.Commit(), format.BadTxn, "commit of a failed child")
	expectErr(t, p.Put(MainDBI, []byte("c"), []byte("d"), 0), format.BadTxn, "parent after failed child")
	expectErr(t, p.Commit(), format.BadTxn, "commit of the parent")
	r := beginR(t, env)
	defer r.Abort()
	if _, err := r.Get(MainDBI, []byte("a")); err != format.NotFound {
		t.Fatal(err)
	}
}

func TestComparatorSetInChildSurvivesAbort(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2})
	p := beginW(t, env)
	defer p.Abort()
	db, _ := p.OpenDBI([]byte("r"), format.Create)
	c, _ := env.BeginTxn(p, 0)
	must(t, c.SetCompare(db, func(a, b []byte) int { return bytes.Compare(b, a) }))
	c.Abort()
	for _, k := range []string{"a", "c", "b"} {
		must(t, p.Put(db, []byte(k), nil, 0))
	}
	cur, _ := p.OpenCursor(db)
	var keys []string
	for {
		k, _, err := cur.Get(nil, nil, format.Next)
		if err != nil {
			break
		}
		keys = append(keys, string(k))
	}
	if len(keys) != 3 || keys[0] != "c" || keys[1] != "b" || keys[2] != "a" {
		t.Fatal(keys)
	}
}

// TestParentCommitCommitsActiveChild: as in C (mdb_txn_commit commits
// mt_child first), committing a parent with an active child commits the
// child; aborting the parent aborts it.
func TestParentCommitCommitsActiveChild(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{})
	p := beginW(t, env)
	must(t, p.Put(MainDBI, []byte("a"), []byte("1"), 0))
	c, _ := env.BeginTxn(p, 0)
	must(t, c.Put(MainDBI, []byte("b"), []byte("2"), 0))
	expectErr(t, p.Put(MainDBI, []byte("x"), []byte("y"), 0), format.BadTxn, "parent with an active child")
	must(t, p.Commit())
	r := beginR(t, env)
	if v, _ := r.Get(MainDBI, []byte("b")); string(v) != "2" {
		t.Fatal(string(v))
	}
	r.Abort()
	p = beginW(t, env)
	c, _ = env.BeginTxn(p, 0)
	must(t, c.Put(MainDBI, []byte("c"), []byte("3"), 0))
	p.Abort()
	r = beginR(t, env)
	defer r.Abort()
	if _, err := r.Get(MainDBI, []byte("c")); err != format.NotFound {
		t.Fatal(err)
	}
}
