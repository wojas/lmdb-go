package engine

import (
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// listOrder returns the cursors of l from the head, at most max of them.
func listOrder(l *CursorList, max int) []*Cursor {
	var out []*Cursor
	for c := l.head; c != nil && len(out) < max; c = c.next {
		out = append(out, c)
	}
	return out
}

func sameCursors(t *testing.T, what string, got []*Cursor, want ...*Cursor) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d cursors, want %d", what, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: cursor %d differs", what, i)
		}
	}
}

func TestCursorList(t *testing.T) {
	var l CursorList
	a, b, c := new(Cursor), new(Cursor), new(Cursor)
	l.Add(a)
	l.Add(b)
	l.Add(c)
	sameCursors(t, "added", listOrder(&l, 10), c, b, a)
	for _, x := range []*Cursor{a, b, c} {
		if !l.Has(x) {
			t.Fatal("Has: false for a cursor in the list")
		}
	}
	l.Remove(b) // middle
	sameCursors(t, "middle removed", listOrder(&l, 10), c, a)
	if l.Has(b) || b.next != nil || b.prev != nil {
		t.Fatal("a removed cursor still looks linked")
	}
	l.Remove(b) // not in the list: nothing happens
	sameCursors(t, "removed twice", listOrder(&l, 10), c, a)
	l.Remove(c) // head
	sameCursors(t, "head removed", listOrder(&l, 10), a)
	l.Add(b)
	l.Remove(a) // tail
	sameCursors(t, "tail removed", listOrder(&l, 10), b)
	l.Remove(b)
	if l.head != nil {
		t.Fatal("list not empty")
	}

	// DetachAll empties the list, unlinks every cursor and marks it
	// detached; opening it again clears the mark.
	l.Add(a)
	l.Add(b)
	tok := a.Owner()
	l.DetachAll()
	if l.head != nil || l.Has(a) || l.Has(b) || a.next != nil || b.prev != nil {
		t.Fatal("DetachAll left cursors linked")
	}
	if a.Owner() != tok|OwnerDetached || b.Owner()&OwnerDetached == 0 {
		t.Fatalf("DetachAll: owner %#x, want %#x", a.Owner(), tok|OwnerDetached)
	}
	if a.nextOwner() != tok+1 {
		t.Fatalf("next owner of a detached cursor: %#x, want %#x", a.nextOwner(), tok+1)
	}

	// It ends on a list that misuse left with a cycle.
	l.Add(a)
	l.Add(b)
	l.Add(c)
	a.next = c
	l.DetachAll()
	if l.head != nil || a.next != nil || b.next != nil || c.next != nil {
		t.Fatal("DetachAll left cursors linked")
	}
}

// A cursor opened again (renewed, or opened from package lmdb's pool)
// holds no page of its last use: init only resets the root of the stack,
// and the deeper pages of a taller tree, perhaps of another map, would
// stay. Here the tree shrinks from several levels to one between the
// cursor's two transactions (long keys: few per branch page, also with
// 16 KiB pages).
func TestOpenCursorIntoForgetsLastUse(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 4})
	key := func(i int) []byte {
		k := make([]byte, 400)
		k[0], k[1] = byte(i>>8), byte(i)
		return k
	}
	w := beginW(t, env)
	for i := 0; i < 2000; i++ {
		must(t, w.Put(MainDBI, key(i), []byte("v"), 0))
	}
	must(t, w.Commit())

	r := beginR(t, env)
	c, err := r.OpenCursor(MainDBI)
	must(t, err)
	if _, _, err := c.Get(nil, nil, format.Last); err != nil {
		t.Fatal(err)
	}
	deep := c.mc.snum
	if deep < 3 {
		t.Fatalf("tree of %d levels: the test needs 3 or more", deep)
	}
	r.Abort()

	w = beginW(t, env)
	for i := 1; i < 2000; i++ {
		must(t, w.Del(MainDBI, key(i), nil))
	}
	must(t, w.Commit())

	r = beginR(t, env)
	defer r.Abort()
	tok := c.Owner()
	must(t, c.Renew(r))
	if c.Owner() == tok {
		t.Error("Renew kept the owner token")
	}
	if c.HoldsPages() {
		t.Error("renewed cursor holds pages of its last use")
	}
	if _, _, err := c.Get(nil, nil, format.First); err != nil {
		t.Fatal(err)
	}
	if c.mc.snum != 1 {
		t.Fatalf("tree of %d levels after the deletes, want 1", c.mc.snum)
	}
	for i := 1; i < deep; i++ {
		if c.mc.pg[i] != nil {
			t.Errorf("stack entry %d still holds a page of the last use", i)
		}
	}
}

// Close and Release change the owner token and leave the cursor closed.
func TestCursorReleaseOwner(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 4})
	w := beginW(t, env)
	must(t, w.Put(MainDBI, []byte("a"), []byte("1"), 0))
	must(t, w.Commit())
	r := beginR(t, env)
	defer r.Abort()
	c, err := r.OpenCursor(MainDBI)
	must(t, err)
	if _, _, err := c.Get(nil, nil, format.First); err != nil {
		t.Fatal(err)
	}
	tok := c.Owner()
	c.Release()
	if c.Owner() == tok || c.HoldsPages() {
		t.Fatal("Release kept the owner token or the pages")
	}
	if _, _, err := c.Get(nil, nil, format.First); err == nil {
		t.Fatal("Get on a released cursor succeeded")
	}
	must(t, r.OpenCursorInto(c, MainDBI))
	tok = c.Owner()
	c.Close()
	if c.Owner() == tok {
		t.Fatal("Close kept the owner token")
	}
}
