package lmdb

// Handles of transactions that have ended never act on the engine state
// their transaction used: that memory serves later transactions (the
// environment's write transaction, a parent's next child, pooled read
// transactions).  Not in lmdb-go v1, whose handles pointed to C memory
// that mdb_txn_commit and mdb_txn_abort freed.

import (
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/PowerDNS/lmdb-go/internal/engine"
)

// forFormats runs fn on a new environment of each format.
func forFormats(t *testing.T, fn func(t *testing.T, env *Env, db DBI)) {
	for _, f := range []Format{V09, V10} {
		t.Run(f.String(), func(t *testing.T) {
			env, err := NewEnv()
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			if err := env.SetFormat(f); err != nil {
				t.Fatal(err)
			}
			if err := env.Open(t.TempDir(), testFlags(NoSync), 0o644); err != nil {
				t.Fatal(err)
			}
			db, err := openRoot(env, Create)
			if err != nil {
				t.Fatal(err)
			}
			fn(t, env, db)
		})
	}
}

// waitFinalizers gives finalizers of objects that became unreachable a
// chance to run.
func waitFinalizers() {
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
	}
}

func wantEINVAL(t *testing.T, what string, err error) {
	t.Helper()
	if !IsErrnoSys(err, syscall.EINVAL) {
		t.Errorf("%s: %v (want EINVAL)", what, err)
	}
}

// checkKeys checks which keys the environment holds.
func checkKeys(t *testing.T, env *Env, db DBI, present, absent string) {
	t.Helper()
	err := env.View(func(txn *Txn) error {
		if _, err := txn.Get(db, []byte(present)); err != nil {
			t.Errorf("Get(%q): %v", present, err)
		}
		if _, err := txn.Get(db, []byte(absent)); !IsNotFound(err) {
			t.Errorf("Get(%q): %v (want MDB_NOTFOUND)", absent, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A nested transaction ends with its parent.  Its handle fails from then
// on, and neither it, its cursors nor its finalizer touch the parent's
// next child, which reuses the engine state.
func TestTxn_nestedEndsWithParent(t *testing.T) {
	forFormats(t, func(t *testing.T, env *Env, db DBI) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		p, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		old, err := env.BeginTxn(p, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := old.Put(db, []byte("old"), []byte("aborted"), 0); err != nil {
			t.Fatal(err)
		}
		cur, err := old.OpenCursor(db)
		if err != nil {
			t.Fatal(err)
		}
		p.Abort() // aborts old as well, as in C
		err = env.UpdateLocked(func(txn *Txn) error {
			return txn.Sub(func(child *Txn) error {
				ncur, err := child.OpenCursor(db)
				if err != nil {
					return err
				}
				if err := child.Put(db, []byte("new"), []byte("valid"), 0); err != nil {
					return err
				}
				_, err = old.Get(db, []byte("new"))
				wantEINVAL(t, "Get", err)
				wantEINVAL(t, "Put", old.Put(db, []byte("x"), []byte("y"), 0))
				_, err = old.OpenCursor(db)
				wantEINVAL(t, "OpenCursor", err)
				_, err = env.BeginTxn(old, 0)
				wantEINVAL(t, "BeginTxn", err)
				_, _, err = cur.Get(nil, nil, First)
				wantEINVAL(t, "cursor Get", err)
				cur.Close()
				if id := old.ID(); id != 0 {
					t.Errorf("ID: %d", id)
				}
				old.finalize() // what its finalizer would do
				wantEINVAL(t, "Commit", old.Commit())
				old.Abort()
				if _, _, err := ncur.Get([]byte("new"), nil, Set); err != nil {
					t.Errorf("new child's cursor: %v", err)
				}
				return child.Put(db, []byte("new2"), []byte("valid"), 0)
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		checkKeys(t, env, db, "new2", "old")
	})
}

// The finalizer of an unreachable nested transaction that ended with its
// parent does not abort the parent's next child (the review's reproducer).
func TestTxn_nestedFinalizerAfterParent(t *testing.T) {
	forFormats(t, func(t *testing.T, env *Env, db DBI) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		p, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		old, err := env.BeginTxn(p, 0)
		if err != nil {
			t.Fatal(err)
		}
		old.errLogf = t.Logf
		if err := old.Put(db, []byte("old"), []byte("aborted"), 0); err != nil {
			t.Fatal(err)
		}
		p.Abort()
		err = env.UpdateLocked(func(txn *Txn) error {
			return txn.Sub(func(child *Txn) error {
				old = nil
				waitFinalizers()
				return child.Put(db, []byte("new"), []byte("valid"), 0)
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		checkKeys(t, env, db, "new", "old")
	})
}

// An unreachable nested transaction that is still active is not finalized
// while its parent lives: its finalizer would end it on another goroutine,
// changing the parent under its user.  It ends with the parent.
func TestTxn_nestedUnreachableActive(t *testing.T) {
	forFormats(t, func(t *testing.T, env *Env, db DBI) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		p, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		child, err := env.BeginTxn(p, 0)
		if err != nil {
			t.Fatal(err)
		}
		child.errLogf = t.Logf
		if err := child.Put(db, []byte("child"), []byte("v"), 0); err != nil {
			t.Fatal(err)
		}
		child = nil
		waitFinalizers()
		if err := p.Put(db, []byte("parent"), []byte("v"), 0); !IsErrno(err, BadTxn) {
			t.Errorf("parent with an active child: %v (want MDB_BAD_TXN)", err)
		}
		// Committing the parent commits the child first, as in C.
		if err := p.Commit(); err != nil {
			t.Fatal(err)
		}
		checkKeys(t, env, db, "child", "parent")
	})
}

// A handle whose engine state serves another transaction fails, whichever
// object is reused: the environment's write transaction, a parent's spare
// child, a pooled read transaction.  Package lmdb drops the engine state
// of every handle that ends; a copy of a handle stands for one it missed.
func TestTxn_staleHandle(t *testing.T) {
	forFormats(t, func(t *testing.T, env *Env, db DBI) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		check := func(what string, stale *Txn) {
			t.Helper()
			_, err := stale.Get(db, []byte("k"))
			wantEINVAL(t, what+": Get", err)
			wantEINVAL(t, what+": Put", stale.Put(db, []byte("stale"), []byte("v"), 0))
			_, err = stale.OpenCursor(db)
			wantEINVAL(t, what+": OpenCursor", err)
			if id := stale.ID(); id != 0 {
				t.Errorf("%s: ID %d", what, id)
			}
			stale.finalize()
			stale.Reset()
			wantEINVAL(t, what+": Renew", stale.Renew())
			wantEINVAL(t, what+": Commit", stale.Commit())
			stale.Abort()
		}

		// The write transaction.
		w, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		stale := *w
		w.Abort()
		if w, err = env.BeginTxn(nil, 0); err != nil {
			t.Fatal(err)
		}
		check("write", &stale)

		// The spare child.
		child, err := env.BeginTxn(w, 0)
		if err != nil {
			t.Fatal(err)
		}
		stale = *child
		child.Abort()
		if child, err = env.BeginTxn(w, 0); err != nil {
			t.Fatal(err)
		}
		check("child", &stale)
		if err := child.Put(db, []byte("k"), []byte("v"), 0); err != nil {
			t.Fatal(err)
		}
		if err := child.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit(); err != nil {
			t.Fatal(err)
		}
		checkKeys(t, env, db, "k", "stale")

		// A read transaction whose engine state was pooled and taken by
		// another one.
		r, err := env.BeginTxn(nil, Readonly)
		if err != nil {
			t.Fatal(err)
		}
		stale = *r
		r.Abort()
		e := env.eng.Load()
		other := stale._txn
		if err := e.BeginRead(other); err != nil {
			t.Fatal(err)
		}
		check("read", &stale)
		if v, err := other.Get(engine.DBI(db), []byte("k")); err != nil || string(v) != "v" {
			t.Errorf("other read transaction: %q %v", v, err)
		}
		other.Abort()
	})
}
