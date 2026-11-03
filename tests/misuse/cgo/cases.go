// Package cases holds the misuse cases of tests/misuse, written once for
// both implementations: tests/misuse/cgo runs them on the cgo reference,
// tests/misuse/port (generated from it by tests/gen-port.sh) on the port. A
// case begins transactions and cursors in an environment that Open filled,
// uses one of them after it ended (or after Reset), and returns what
// happened as a string that the test compares: "ok ..." with the result, or
// "err <C function>: <error>".
//
// Many cases are undefined behaviour on the cgo reference: lmdb-go v1
// passed NULL or freed C handles to C, which crashed, panicked in Go, or
// used freed memory. misuse_test.go decides where each case runs on it: in
// the test process, in a subprocess of its own, or not at all.
package cases

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	lmdb "github.com/PowerDNS/lmdb-go/tests/cgolmdb"
)

// Env is an environment with the data the cases read: in the database
// "main" the keys a, b and c with the values va, vb and vc; in the DupSort
// database "dup" the same keys, each with the values 1 and 2.
type Env struct {
	env     *lmdb.Env
	db, dup lmdb.DBI
}

// Open creates the environment in the directory dir. Like the cases, it
// begins write transactions: the calling goroutine must be locked to its
// OS thread.
func Open(dir string) (*Env, error) {
	env, err := lmdb.NewEnv()
	if err != nil {
		return nil, err
	}
	e := &Env{env: env}
	if err := e.open(dir); err != nil {
		env.Close()
		return nil, err
	}
	return e, nil
}

func (e *Env) open(dir string) error {
	if err := e.env.SetMaxDBs(4); err != nil {
		return err
	}
	if err := e.env.SetMapSize(16 << 20); err != nil {
		return err
	}
	if err := e.env.Open(dir, lmdb.NoSync, 0o644); err != nil {
		return err
	}
	return e.env.UpdateLocked(func(txn *lmdb.Txn) (err error) {
		if e.db, err = txn.OpenDBI("main", lmdb.Create); err != nil {
			return err
		}
		if e.dup, err = txn.OpenDBI("dup", lmdb.Create|lmdb.DupSort); err != nil {
			return err
		}
		for _, k := range []string{"a", "b", "c"} {
			if err := txn.Put(e.db, []byte(k), []byte("v"+k), 0); err != nil {
				return err
			}
			for _, v := range []string{"1", "2"} {
				if err := txn.Put(e.dup, []byte(k), []byte(v), 0); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Close closes the environment.
func (e *Env) Close() { e.env.Close() }

// Case is a misuse case: it returns what happened.
type Case func(e *Env) string

// Cases are the cases by name: <handle>/<operation>, the handle and what
// happened to it before ("closed-ro": a read-only cursor that was closed,
// "rw-commit": a cursor of a write transaction that committed, "txn-ro-reset":
// a read-only transaction that was reset, ...).
var Cases = map[string]Case{
	// Cursors after Close.
	"closed-ro/Get-First": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		c.Close()
		return pair(c.Get(nil, nil, lmdb.First))
	},
	"closed-ro/Get-Set": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		c.Close()
		return pair(c.Get([]byte("a"), nil, lmdb.Set))
	},
	"closed-ro/Count": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.dup)
		c.Close()
		return count(c.Count())
	},
	"closed-ro/Renew": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		c.Close()
		return outcome(c.Renew(t))
	},
	"closed-ro/DBI": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		c.Close()
		return fmt.Sprint(c.DBI())
	},
	"closed-ro/Close": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		c.Close()
		c.Close()
		return "ok"
	},
	"closed-rw/Get-First": func(e *Env) string {
		t := e.rw()
		defer t.Abort()
		c := e.cursor(t, e.db)
		c.Close()
		return pair(c.Get(nil, nil, lmdb.First))
	},
	"closed-rw/Put": func(e *Env) string {
		t := e.rw()
		defer t.Abort()
		c := e.cursor(t, e.db)
		c.Close()
		return outcome(c.Put([]byte("x"), []byte("y"), 0))
	},
	"closed-rw/Del": func(e *Env) string {
		t := e.rw()
		defer t.Abort()
		c := e.positioned(t, e.db)
		c.Close()
		return outcome(c.Del(0))
	},

	// Cursors of a write transaction that ended (C frees them).
	"rw-commit/Get-First": func(e *Env) string {
		t := e.rw()
		c := e.cursor(t, e.db)
		e.commit(t)
		return pair(c.Get(nil, nil, lmdb.First))
	},
	"rw-commit/Put": func(e *Env) string {
		t := e.rw()
		c := e.cursor(t, e.db)
		e.commit(t)
		return outcome(c.Put([]byte("x"), []byte("y"), 0))
	},
	"rw-commit/Del": func(e *Env) string {
		t := e.rw()
		c := e.positioned(t, e.db)
		e.commit(t)
		return outcome(c.Del(0))
	},
	"rw-commit/Count": func(e *Env) string {
		t := e.rw()
		c := e.positioned(t, e.dup)
		e.commit(t)
		return count(c.Count())
	},
	"rw-commit/Close": func(e *Env) string {
		t := e.rw()
		c := e.cursor(t, e.db)
		e.commit(t)
		c.Close()
		return "ok"
	},
	"rw-commit/Put-into-next": func(e *Env) string {
		t := e.rw()
		c := e.cursor(t, e.db)
		e.commit(t)
		next := e.rw()
		defer next.Abort()
		e.cursor(next, e.db)
		r := outcome(c.Put([]byte("stale"), []byte("y"), 0))
		if _, err := next.Get(e.db, []byte("stale")); err == nil {
			r += "; written into the next transaction"
		}
		return r
	},
	"rw-abort/Get-First": func(e *Env) string {
		t := e.rw()
		c := e.cursor(t, e.db)
		t.Abort()
		return pair(c.Get(nil, nil, lmdb.First))
	},
	"rw-abort/Put": func(e *Env) string {
		t := e.rw()
		c := e.cursor(t, e.db)
		t.Abort()
		return outcome(c.Put([]byte("x"), []byte("y"), 0))
	},

	// Cursors of a read-only transaction that ended (C leaves them, to be
	// renewed or closed, but frees the transaction).
	"ro-abort/Get-First": func(e *Env) string {
		t := e.ro()
		c := e.cursor(t, e.db)
		t.Abort()
		return pair(c.Get(nil, nil, lmdb.First))
	},
	"ro-commit/Get-Next": func(e *Env) string {
		t := e.ro()
		c := e.positioned(t, e.db)
		e.commit(t)
		return pair(c.Get(nil, nil, lmdb.Next))
	},
	"ro-abort/Count": func(e *Env) string {
		t := e.ro()
		c := e.positioned(t, e.dup)
		t.Abort()
		return count(c.Count())
	},
	"ro-abort/Get-after-new-txn": func(e *Env) string {
		t := e.ro()
		c := e.cursor(t, e.db)
		t.Abort()
		t2 := e.ro()
		defer t2.Abort()
		return pair(c.Get(nil, nil, lmdb.First))
	},
	"ro-abort/Renew-then-Get": func(e *Env) string {
		t := e.ro()
		c := e.cursor(t, e.db)
		t.Abort()
		t2 := e.ro()
		defer t2.Abort()
		defer c.Close()
		if err := c.Renew(t2); err != nil {
			return outcome(err)
		}
		return pair(c.Get(nil, nil, lmdb.First))
	},
	"ro-abort/Close": func(e *Env) string {
		t := e.ro()
		c := e.cursor(t, e.db)
		t.Abort()
		c.Close()
		return "ok"
	},

	// A copy of a cursor value made before Close, used after another
	// cursor was opened (in the port it takes over the closed cursor's
	// memory, in C malloc may hand it the freed one).
	"ro-copy-closed/Get": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		stale := *c
		c.Close()
		other := e.cursor(t, e.db)
		defer other.Close()
		if _, _, err := other.Get(nil, nil, lmdb.Last); err != nil {
			return outcome(err)
		}
		r := pair(stale.Get(nil, nil, lmdb.First))
		return r + "; other cursor: " + pair(other.Get(nil, nil, lmdb.GetCurrent))
	},

	// Cursors of a read-only transaction that was reset, before Renew.
	"ro-reset/Get-First": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		defer c.Close()
		t.Reset()
		return pair(c.Get(nil, nil, lmdb.First))
	},
	"ro-reset/Count": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.positioned(t, e.dup)
		defer c.Close()
		t.Reset()
		return count(c.Count())
	},
	"ro-reset/Count-nodup": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.positioned(t, e.db)
		defer c.Close()
		t.Reset()
		return count(c.Count())
	},
	"ro-reset/Put": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		defer c.Close()
		t.Reset()
		return outcome(c.Put([]byte("x"), []byte("y"), 0))
	},
	"ro-reset/Del": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.positioned(t, e.db)
		defer c.Close()
		t.Reset()
		return outcome(c.Del(0))
	},
	"ro-reset/Cursor-Renew": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		defer c.Close()
		t.Reset()
		return outcome(c.Renew(t))
	},
	"ro-reset/Renew-other": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		defer c.Close()
		t.Reset()
		t2 := e.ro()
		defer t2.Abort()
		if err := c.Renew(t2); err != nil {
			return outcome(err)
		}
		return pair(c.Get(nil, nil, lmdb.First))
	},
	"ro-reset/Close": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		t.Reset()
		c.Close()
		return "ok"
	},

	// Cursors of a read-only transaction that was reset and renewed.
	"ro-reset-renew/Get-Next": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.positioned(t, e.db)
		defer c.Close()
		t.Reset()
		if err := t.Renew(); err != nil {
			return outcome(err)
		}
		return pair(c.Get(nil, nil, lmdb.Next))
	},
	"ro-reset-renew/Get-Current": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.positioned(t, e.db)
		defer c.Close()
		t.Reset()
		if err := t.Renew(); err != nil {
			return outcome(err)
		}
		return pair(c.Get(nil, nil, lmdb.GetCurrent))
	},
	"ro-reset-renew/Cursor-Renew-Get-Next": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.positioned(t, e.db)
		defer c.Close()
		t.Reset()
		if err := t.Renew(); err != nil {
			return outcome(err)
		}
		if err := c.Renew(t); err != nil {
			return outcome(err)
		}
		return pair(c.Get(nil, nil, lmdb.Next))
	},
	"ro-renew-active/Get-Next": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.positioned(t, e.db)
		defer c.Close()
		return outcome(t.Renew()) + "; " + pair(c.Get(nil, nil, lmdb.Next))
	},
	"ro-active/Put": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		c := e.cursor(t, e.db)
		defer c.Close()
		return outcome(c.Put([]byte("x"), []byte("y"), 0))
	},

	// Reset of a write transaction (mdb_txn_reset does nothing).
	"rw-reset/Get-Next": func(e *Env) string {
		t := e.rw()
		defer t.Abort()
		c := e.positioned(t, e.db)
		t.Reset()
		return pair(c.Get(nil, nil, lmdb.Next))
	},
	"rw-reset/Put": func(e *Env) string {
		t := e.rw()
		defer t.Abort()
		t.Reset()
		return outcome(t.Put(e.db, []byte("x"), []byte("y"), 0))
	},

	// Read-only transactions that ended.
	"txn-ro-abort/Get": func(e *Env) string {
		t := e.ro()
		t.Abort()
		return value(t.Get(e.db, []byte("a")))
	},
	"txn-ro-abort/OpenCursor": func(e *Env) string {
		t := e.ro()
		t.Abort()
		return opened(t.OpenCursor(e.db))
	},
	"txn-ro-abort/Renew": func(e *Env) string {
		t := e.ro()
		t.Abort()
		return outcome(t.Renew())
	},
	"txn-ro-abort/Reset": func(e *Env) string {
		t := e.ro()
		t.Abort()
		t.Reset()
		return "ok"
	},
	"txn-ro-abort/ID": func(e *Env) string {
		t := e.ro()
		t.Abort()
		return fmt.Sprint(t.ID())
	},

	// Read-only transactions that were reset, before Renew.
	"txn-ro-reset/Get": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		return value(t.Get(e.db, []byte("a")))
	},
	"txn-ro-reset/Put": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		return outcome(t.Put(e.db, []byte("x"), []byte("y"), 0))
	},
	"txn-ro-reset/Del": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		return outcome(t.Del(e.db, []byte("a"), nil))
	},
	"txn-ro-reset/Drop": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		return outcome(t.Drop(e.db, false))
	},
	"txn-ro-reset/Stat": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		_, err := t.Stat(e.db)
		return outcome(err)
	},
	"txn-ro-reset/Flags": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		_, err := t.Flags(e.db)
		return outcome(err)
	},
	"txn-ro-reset/OpenCursor": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		return opened(t.OpenCursor(e.db))
	},
	"txn-ro-reset/OpenDBI": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		_, err := t.OpenDBI("main", 0)
		return outcome(err)
	},
	"txn-ro-reset/OpenRoot": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		_, err := t.OpenRoot(0)
		return outcome(err)
	},
	"txn-ro-reset/Commit": func(e *Env) string {
		return outcome(e.reset().Commit())
	},
	"txn-ro-reset/Reset-again": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		t.Reset()
		return outcome(t.Renew())
	},
	"txn-ro-reset/ID": func(e *Env) string {
		t := e.reset()
		defer t.Abort()
		if t.ID() != 0 {
			return "the snapshot's"
		}
		return "0"
	},
	"txn-ro/Renew-active": func(e *Env) string {
		t := e.ro()
		defer t.Abort()
		return outcome(t.Renew())
	},

	// Write transactions that ended.
	"txn-rw-commit/Get": func(e *Env) string {
		return value(e.committed().Get(e.db, []byte("a")))
	},
	"txn-rw-commit/Put": func(e *Env) string {
		return outcome(e.committed().Put(e.db, []byte("x"), []byte("y"), 0))
	},
	"txn-rw-commit/PutReserve": func(e *Env) string {
		_, err := e.committed().PutReserve(e.db, []byte("x"), 4, 0)
		return outcome(err)
	},
	"txn-rw-commit/Del": func(e *Env) string {
		return outcome(e.committed().Del(e.db, []byte("a"), nil))
	},
	"txn-rw-commit/Drop": func(e *Env) string {
		return outcome(e.committed().Drop(e.db, false))
	},
	"txn-rw-commit/Stat": func(e *Env) string {
		_, err := e.committed().Stat(e.db)
		return outcome(err)
	},
	"txn-rw-commit/Flags": func(e *Env) string {
		_, err := e.committed().Flags(e.db)
		return outcome(err)
	},
	"txn-rw-commit/OpenCursor": func(e *Env) string {
		return opened(e.committed().OpenCursor(e.db))
	},
	"txn-rw-commit/OpenDBI": func(e *Env) string {
		_, err := e.committed().OpenDBI("main", 0)
		return outcome(err)
	},
	"txn-rw-commit/Sub": func(e *Env) string {
		return outcome(e.committed().Sub(func(*lmdb.Txn) error { return nil }))
	},
	"txn-rw-commit/Commit": func(e *Env) string {
		return outcome(e.committed().Commit())
	},
	"txn-rw-commit/Abort": func(e *Env) string {
		e.committed().Abort()
		return "ok"
	},
	"txn-rw-abort/Put": func(e *Env) string {
		t := e.rw()
		t.Abort()
		return outcome(t.Put(e.db, []byte("x"), []byte("y"), 0))
	},
}

// ro begins a read-only transaction.
func (e *Env) ro() *lmdb.Txn {
	t, err := e.env.BeginTxn(nil, lmdb.Readonly)
	check(err)
	return t
}

// rw begins a write transaction.
func (e *Env) rw() *lmdb.Txn {
	t, err := e.env.BeginTxn(nil, 0)
	check(err)
	return t
}

// reset begins a read-only transaction and resets it.
func (e *Env) reset() *lmdb.Txn {
	t := e.ro()
	t.Reset()
	return t
}

// commit commits t.
func (e *Env) commit(t *lmdb.Txn) { check(t.Commit()) }

// committed is a write transaction that committed.
func (e *Env) committed() *lmdb.Txn {
	t := e.rw()
	e.commit(t)
	return t
}

// cursor opens a cursor on db in t.
func (e *Env) cursor(t *lmdb.Txn, db lmdb.DBI) *lmdb.Cursor {
	c, err := t.OpenCursor(db)
	check(err)
	return c
}

// positioned opens a cursor on db in t, on its first item.
func (e *Env) positioned(t *lmdb.Txn, db lmdb.DBI) *lmdb.Cursor {
	c := e.cursor(t, db)
	_, _, err := c.Get(nil, nil, lmdb.First)
	check(err)
	return c
}

// check panics on a failure of the setup, which is no outcome of a case.
func check(err error) {
	if err != nil {
		panic(err)
	}
}

// outcome is "ok" or the error, as errname names it.
func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	var op *lmdb.OpError
	if errors.As(err, &op) {
		return "err " + op.Op + ": " + errname(op.Errno)
	}
	return "err " + err.Error()
}

// errname names the errors the cases expect, the same for both
// implementations on every platform.
func errname(err error) string {
	switch err {
	case syscall.EINVAL:
		return "EINVAL"
	case syscall.EACCES:
		return "EACCES"
	case lmdb.BadTxn:
		return "MDB_BAD_TXN"
	case lmdb.Incompatible:
		return "MDB_INCOMPATIBLE"
	case lmdb.NotFound:
		return "MDB_NOTFOUND"
	}
	return err.Error()
}

// pair is the outcome of a Cursor.Get.
func pair(k, v []byte, err error) string {
	if err != nil {
		return outcome(err)
	}
	return fmt.Sprintf("ok %s=%s", k, v)
}

// value is the outcome of a Txn.Get.
func value(v []byte, err error) string {
	if err != nil {
		return outcome(err)
	}
	return "ok " + string(v)
}

// count is the outcome of a Cursor.Count.
func count(n uint64, err error) string {
	if err != nil {
		return outcome(err)
	}
	return fmt.Sprint("ok ", n)
}

// opened is the outcome of a Txn.OpenCursor (the cursor is closed).
func opened(c *lmdb.Cursor, err error) string {
	if err != nil {
		return outcome(err)
	}
	c.Close()
	return "ok"
}

// SetupFailed is the exit status of Run when the case is unknown or its
// environment cannot be created: not an outcome of the case (a Go panic
// exits with 2, as does a signal in C code).
const SetupFailed = 3

// Run runs the case name in a new environment in dir and prints its
// outcome: the second process of a case whose misuse may crash the
// process (misuse_test.go). The calling goroutine must be locked to its OS
// thread.
func Run(dir, name string) int {
	c, ok := Cases[name]
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown case", name)
		return SetupFailed
	}
	e, err := Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		return SetupFailed
	}
	fmt.Println(c(e))
	return 0
}
