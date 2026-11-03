package lmdb

// Cursors used after they ended: after Close, after their transaction
// ended (Commit, Abort, the end of a parent), and after Reset without
// Renew; copies of a handle that was closed; and the pages a cursor that
// outlives its transaction keeps. lmdb-go v1 passed NULL or freed C
// cursors to C here: nil pointer panics and segmentation faults, and a
// write cursor of a committed transaction wrote into the next one.
// Not in lmdb-go v1's tests.

import (
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// wantErrno fails t unless err is errno.
func wantErrno(t *testing.T, what string, err error, errno Errno) {
	t.Helper()
	if !IsErrno(err, errno) {
		t.Errorf("%s: %v (want %v)", what, err, errno)
	}
}

// wantSys fails t unless err is the system error errno.
func wantSys(t *testing.T, what string, err error, errno syscall.Errno) {
	t.Helper()
	if !IsErrnoSys(err, errno) {
		t.Errorf("%s: %v (want %v)", what, err, errno)
	}
}

// wantPair fails t unless the cursor result is key and val.
func wantPair(t *testing.T, what string, k, v []byte, err error, key, val string) {
	t.Helper()
	if err != nil || string(k) != key || string(v) != val {
		t.Errorf("%s: %q %q %v (want %q %q)", what, k, v, err, key, val)
	}
}

// checkNoPages fails t if c's engine cursor holds a page (a slice into
// the map, which may be unmapped once its transaction released it).
func checkNoPages(t *testing.T, what string, c *Cursor) {
	t.Helper()
	if c._c != nil && c._c.HoldsPages() {
		t.Errorf("%s: the cursor still holds pages of its transaction", what)
	}
}

// openPositioned opens a cursor on dup in txn, positioned on the first
// value ("k", "v1").
func openPositioned(t *testing.T, txn *Txn, dup DBI) *Cursor {
	t.Helper()
	c, err := txn.OpenCursor(dup)
	check(t, err)
	k, v, err := c.Get(nil, nil, First)
	wantPair(t, "First", k, v, err, "k", "v1")
	return c
}

// Every operation of a closed cursor fails with EINVAL; its transaction
// is not affected.
func TestCursor_useAfterClose(t *testing.T) {
	for _, f := range []Format{V09, V10} {
		t.Run(f.String(), func(t *testing.T) {
			env, _, _, dup := openClosedTest(t, f, 0)
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			for _, flags := range []uint{Readonly, 0} {
				txn, err := env.BeginTxn(nil, flags)
				check(t, err)
				c := openPositioned(t, txn, dup)
				c.Close()
				checkCursorClosed(t, c, txn)
				if c.DBI() != ^DBI(0) || c.Txn() != nil {
					t.Errorf("flags %#x: DBI %d, Txn %p after Close", flags, c.DBI(), c.Txn())
				}
				c.Close()
				c2 := openPositioned(t, txn, dup)
				k, v, err := c2.Get(nil, nil, Next)
				wantPair(t, "Next", k, v, err, "k", "v2")
				c2.Close()
				txn.Abort()
			}
		})
	}
}

// The cursors of a transaction that ended fail with EINVAL and hold no
// pages of it; Close works, and a read cursor can be renewed.
func TestCursor_useAfterTxnEnd(t *testing.T) {
	for _, f := range []Format{V09, V10} {
		t.Run(f.String(), func(t *testing.T) {
			env, _, _, dup := openClosedTest(t, f, 0)
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			ends := []struct {
				name  string
				flags uint
				end   func(*Txn)
			}{
				{"read/Abort", Readonly, (*Txn).Abort},
				{"read/Commit", Readonly, func(txn *Txn) { check(t, txn.Commit()) }},
				{"write/Abort", 0, (*Txn).Abort},
				{"write/Commit", 0, func(txn *Txn) { check(t, txn.Commit()) }},
			}
			for _, e := range ends {
				txn, err := env.BeginTxn(nil, e.flags)
				check(t, err)
				c := openPositioned(t, txn, dup)
				e.end(txn)
				checkNoPages(t, e.name, c)
				checkCursorClosed(t, c, txn)
				if e.flags == Readonly {
					txn2, err := env.BeginTxn(nil, Readonly)
					check(t, err)
					check(t, c.Renew(txn2))
					k, v, err := c.Get(nil, nil, Last)
					wantPair(t, e.name+": Last after Renew", k, v, err, "k", "v2")
					txn2.Abort()
					checkNoPages(t, e.name+" renewed", c)
				}
				c.Close()
				c.Close()
			}

			// Nested transactions: a child that ended, and one that ended
			// with its parent.
			parent, err := env.BeginTxn(nil, 0)
			check(t, err)
			child, err := env.BeginTxn(parent, 0)
			check(t, err)
			c := openPositioned(t, child, dup)
			check(t, child.Commit())
			checkNoPages(t, "child committed", c)
			checkCursorClosed(t, c, child)
			pc := openPositioned(t, parent, dup)
			child, err = env.BeginTxn(parent, 0)
			check(t, err)
			c = openPositioned(t, child, dup)
			parent.Abort()
			checkNoPages(t, "child ended with its parent", c)
			checkNoPages(t, "parent's cursor", pc)
			checkCursorClosed(t, c, child)
			checkCursorClosed(t, pc, parent)
			c.Close()
			pc.Close()
		})
	}
}

// A copy of a handle that was closed never touches the engine cursor,
// which serves later cursors: its operations fail, its Close does not
// put the cursor back into the pool a second time, and the cursor that
// reuses it keeps its position.
func TestCursor_staleCopy(t *testing.T) {
	env, _, db, dup := openClosedTest(t, V09, 0)
	txn, err := env.BeginTxn(nil, Readonly)
	check(t, err)
	defer txn.Abort()

	c := openPositioned(t, txn, dup)
	stale := *c
	c.Close()
	checkCursorClosed(t, &stale, txn)
	if stale.DBI() != ^DBI(0) {
		t.Errorf("DBI of a stale copy: %d", stale.DBI())
	}

	// Find the cursor that reuses the engine cursor (sync.Pool may drop it
	// under -race).
	var fresh *Cursor
	var others []*Cursor
	for i := 0; i < 20 && fresh == nil; i++ {
		x, err := txn.OpenCursor(dup)
		check(t, err)
		if x._c == stale._c {
			fresh = x
		} else {
			others = append(others, x)
		}
	}
	for _, x := range others {
		x.Close()
	}
	if fresh == nil {
		t.Log("the engine cursor was not reused (sync.Pool dropped it)")
		stale.Close()
		return
	}
	k, v, err := fresh.Get(nil, nil, Last)
	wantPair(t, "fresh Last", k, v, err, "k", "v2")
	checkCursorClosed(t, &stale, txn)
	stale.Close()
	k, v, err = fresh.Get(nil, nil, GetCurrent)
	wantPair(t, "fresh GetCurrent after the stale Close", k, v, err, "k", "v2")

	// The stale Close did not pool the engine cursor: two new cursors do
	// not share one.
	a, err := txn.OpenCursor(db)
	check(t, err)
	b, err := txn.OpenCursor(db)
	check(t, err)
	if a._c == fresh._c || b._c == fresh._c || a._c == b._c {
		t.Error("an engine cursor serves two handles")
	}
	a.Close()
	b.Close()
	fresh.Close()

	// A copy of a write cursor.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w, err := env.BeginTxn(nil, 0)
	check(t, err)
	defer w.Abort()
	wc := openPositioned(t, w, dup)
	wstale := *wc
	wc.Close()
	checkCursorClosed(t, &wstale, w)
	wstale.Close()
}

// After Reset and before Renew, a transaction and its cursors fail as in
// C (and lmdb-go v1): EINVAL for the functions that check the database
// handle first, MDB_BAD_TXN or EACCES for cursor operations; the cursors
// hold no pages. After Renew a cursor starts over unpositioned.
func TestCursor_reset(t *testing.T) {
	for _, f := range []Format{V09, V10} {
		t.Run(f.String(), func(t *testing.T) {
			env, _, db, dup := openClosedTest(t, f, 0)
			txn, err := env.BeginTxn(nil, Readonly)
			check(t, err)
			defer txn.Abort()
			// The main DB holds "dup" (the named DB) and "key".
			c, err := txn.OpenCursor(db)
			check(t, err)
			if k, _, err := c.Get(nil, nil, Last); err != nil || string(k) != "key" {
				t.Fatalf("Last: %q %v", k, err)
			}
			d := openPositioned(t, txn, dup)
			txn.Reset()
			checkNoPages(t, "main cursor", c)
			checkNoPages(t, "dup cursor", d)

			_, _, err = c.Get(nil, nil, Next)
			wantErrno(t, "Cursor.Get", err, BadTxn)
			_, err = d.Count()
			wantErrno(t, "Cursor.Count (DupSort)", err, BadTxn)
			_, err = c.Count()
			wantErrno(t, "Cursor.Count", err, Incompatible)
			wantSys(t, "Cursor.Put", c.Put([]byte("x"), []byte("y"), 0), syscall.EACCES)
			wantSys(t, "Cursor.Del", c.Del(0), syscall.EACCES)
			wantEINVAL(t, "Cursor.Renew", d.Renew(txn))
			for _, op := range txnOps(db, dup) {
				err := op.op(txn)
				switch op.name {
				case "OpenDBI", "CreateDBI", "OpenRoot":
					wantErrno(t, "Txn."+op.name, err, BadTxn)
				default:
					wantEINVAL(t, "Txn."+op.name, err)
				}
			}
			if id := txn.ID(); id != 0 {
				t.Errorf("ID %d while reset", id)
			}

			check(t, txn.Renew())
			k, _, err := c.Get(nil, nil, Next)
			if err != nil || !strings.HasPrefix(string(k), "dup") {
				// 1.0 stores the name with its NUL.
				t.Errorf("Next after Renew: %q %v (want the first key, \"dup\")", k, err)
			}
			check(t, d.Renew(txn))
			k, v, err := d.Get(nil, nil, Last)
			wantPair(t, "Last after Cursor.Renew", k, v, err, "k", "v2")

			// A cursor renewed into another transaction is that one's.
			other, err := env.BeginTxn(nil, Readonly)
			check(t, err)
			check(t, d.Renew(other))
			if _, _, err := d.Get(nil, nil, First); err != nil {
				t.Fatal(err)
			}
			txn.Reset()
			k, v, err = d.Get(nil, nil, Next)
			wantPair(t, "Next in the other transaction", k, v, err, "k", "v2")
			other.Abort()
			checkNoPages(t, "dup cursor after the other transaction", d)
			checkNoPages(t, "main cursor after the second Reset", c)
			c.Close()
			d.Close()
		})
	}
}

// Reset does nothing to a write transaction, as mdb_txn_reset: its
// cursors keep their positions.
func TestTxn_resetWrite(t *testing.T) {
	env, _, db, dup := openClosedTest(t, V09, 0)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	txn, err := env.BeginTxn(nil, 0)
	check(t, err)
	c := openPositioned(t, txn, dup)
	txn.Reset()
	k, v, err := c.Get(nil, nil, Next)
	wantPair(t, "Next after Reset", k, v, err, "k", "v2")
	check(t, txn.Put(db, []byte("after"), []byte("reset"), 0))
	check(t, txn.Commit())
	checkKeys(t, env, db, "after", "absent")
}

// A Renew that fails (the transaction is not reset) leaves the cursors
// where they are, as in C.
func TestTxn_renewActive(t *testing.T) {
	env, _, _, dup := openClosedTest(t, V09, 0)
	txn, err := env.BeginTxn(nil, Readonly)
	check(t, err)
	defer txn.Abort()
	c := openPositioned(t, txn, dup)
	wantEINVAL(t, "Renew of an active transaction", txn.Renew())
	k, v, err := c.Get(nil, nil, Next)
	wantPair(t, "Next after the failed Renew", k, v, err, "k", "v2")
	c.Close()
}

// The transaction's list of cursors makes no reference cycle through the
// Txn: an unreachable read transaction with an open cursor is still
// aborted by its finalizer.
func TestTxn_finalizerWithCursor(t *testing.T) {
	env, _, _, dup := openClosedTest(t, V09, 0)
	finalized := make(chan struct{}, 1)
	func() {
		txn, err := env.BeginTxn(nil, Readonly)
		check(t, err)
		txn.errLogf = func(string, ...interface{}) {
			select {
			case finalized <- struct{}{}:
			default:
			}
		}
		openPositioned(t, txn, dup)
	}()
	for i := 0; i < 100; i++ {
		runtime.GC()
		select {
		case <-finalized:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("the finalizer of an unreachable transaction with an open cursor did not run")
}
