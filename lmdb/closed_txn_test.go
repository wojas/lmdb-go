package lmdb

// Transactions and cursors that outlive Env.Close (DEVIATIONS.md): every
// operation fails with EINVAL, Abort, Commit and the finalizer end them,
// RawRead slices returned before Close stay valid until then, and the
// environment is torn down when the last one ends. Not in lmdb-go v1's
// tests (C requires every transaction to end before mdb_env_close).

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

// overwriteRounds is how many commits the overwrite helper and
// overwriteInProcess make.
const overwriteRounds = 10

// overwriteValue is the value of the overwrite helper's commit i: big
// enough for overflow pages, which the next commits reuse once no reader
// can see them.
func overwriteValue(i int) []byte {
	return bytes.Repeat([]byte{byte('a' + i)}, 40000)
}

// openClosedTest opens a new environment of format f with flags (plus
// testFlags' and NoSync) and two named databases in a temporary directory,
// and writes the main DB's key "key" (overwriteValue(-1), all '`') and the
// DupSort DB "dup"'s key "k" with two values.
func openClosedTest(t *testing.T, f Format, flags uint) (env *Env, path string, db, dup DBI) {
	t.Helper()
	env, err := NewEnv()
	check(t, err)
	check(t, env.SetMapSize(64<<20))
	check(t, env.SetMaxDBs(4))
	check(t, env.SetFormat(f))
	path = t.TempDir()
	check(t, env.Open(path, testFlags(flags)|NoSync, 0o644))
	t.Cleanup(func() { env.Close() })
	check(t, env.Update(func(txn *Txn) (err error) {
		if db, err = txn.OpenRoot(0); err != nil {
			return err
		}
		if dup, err = txn.OpenDBI("dup", Create|DupSort); err != nil {
			return err
		}
		if err := txn.Put(db, []byte("key"), overwriteValue(-1), 0); err != nil {
			return err
		}
		if err := txn.Put(dup, []byte("k"), []byte("v1"), 0); err != nil {
			return err
		}
		return txn.Put(dup, []byte("k"), []byte("v2"), 0)
	}))
	return env, path, db, dup
}

// openWithLockFile opens path in a new handle with the lock file, and
// returns Open's error; the handle is closed at the end of the test.
func openWithLockFile(t *testing.T, path string) error {
	t.Helper()
	env, err := NewEnv()
	check(t, err)
	check(t, env.SetMaxDBs(4))
	t.Cleanup(func() { env.Close() })
	return env.Open(path, NoSync, 0o644)
}

// txnOps are the operations of a Txn that return an error, each with a
// name for messages.
func txnOps(db, dup DBI) []struct {
	name string
	op   func(*Txn) error
} {
	return []struct {
		name string
		op   func(*Txn) error
	}{
		{"Get", func(txn *Txn) error { _, err := txn.Get(db, []byte("key")); return err }},
		{"OpenDBI", func(txn *Txn) error { _, err := txn.OpenDBI("dup", 0); return err }},
		{"CreateDBI", func(txn *Txn) error { _, err := txn.CreateDBI("new"); return err }},
		{"OpenRoot", func(txn *Txn) error { _, err := txn.OpenRoot(0); return err }},
		{"Flags", func(txn *Txn) error { _, err := txn.Flags(dup); return err }},
		{"Stat", func(txn *Txn) error { _, err := txn.Stat(db); return err }},
		{"Drop", func(txn *Txn) error { return txn.Drop(dup, false) }},
		{"Put", func(txn *Txn) error { return txn.Put(db, []byte("x"), []byte("y"), 0) }},
		{"PutReserve", func(txn *Txn) error { _, err := txn.PutReserve(db, []byte("x"), 1, 0); return err }},
		{"Del", func(txn *Txn) error { return txn.Del(db, []byte("key"), nil) }},
		{"OpenCursor", func(txn *Txn) error { _, err := txn.OpenCursor(db); return err }},
		{"Sub", func(txn *Txn) error { return txn.Sub(func(*Txn) error { return nil }) }},
	}
}

// checkCursorClosed checks that every operation of c, a cursor on a
// DupSort DB of txn, fails with EINVAL.
func checkCursorClosed(t *testing.T, c *Cursor, txn *Txn) {
	t.Helper()
	for _, op := range []uint{First, Next, GetCurrent, Last, NextDup} {
		_, _, err := c.Get(nil, nil, op)
		wantEINVAL(t, fmt.Sprintf("Cursor.Get(%d)", op), err)
	}
	_, _, err := c.Get([]byte("k"), nil, Set)
	wantEINVAL(t, "Cursor.Get(Set)", err)
	_, _, err = c.Get([]byte("k"), []byte("v2"), GetBoth)
	wantEINVAL(t, "Cursor.Get(GetBoth)", err)
	wantEINVAL(t, "Cursor.Put", c.Put([]byte("k"), []byte("v3"), 0))
	_, err = c.PutReserve([]byte("k"), 1, 0)
	wantEINVAL(t, "Cursor.PutReserve", err)
	wantEINVAL(t, "Cursor.PutMulti", c.PutMulti([]byte("k"), []byte("ab"), 1, 0))
	wantEINVAL(t, "Cursor.Del", c.Del(0))
	_, err = c.Count()
	wantEINVAL(t, "Cursor.Count", err)
	wantEINVAL(t, "Cursor.Renew", c.Renew(txn))
}

// A read transaction and its cursor that outlive Close: every operation
// fails with EINVAL, Reset does nothing, RawRead slices returned before
// Close keep their bytes, and Abort tears the environment down (a handle
// with a lock file opens the path again, EBUSY before).
func TestClosedEnv_readTxn(t *testing.T) {
	for _, f := range []Format{V09, V10} {
		for _, flags := range lockModes() {
			t.Run(fmt.Sprintf("%v/%#x", f, flags), func(t *testing.T) {
				env, path, db, dup := openClosedTest(t, f, flags)
				txn, err := env.BeginTxn(nil, Readonly)
				check(t, err)
				txn.RawRead = true
				val, err := txn.Get(db, []byte("key"))
				check(t, err)
				c, err := txn.OpenCursor(dup)
				check(t, err)
				ck, cv, err := c.Get(nil, nil, Last)
				check(t, err)
				id := txn.ID()

				check(t, env.Close())
				for _, op := range txnOps(db, dup) {
					wantEINVAL(t, "Txn."+op.name, op.op(txn))
				}
				checkCursorClosed(t, c, txn)
				txn.Reset()
				if txn.ID() != id {
					t.Errorf("Reset after Close: ID %d, want %d (no reset)", txn.ID(), id)
				}
				wantEINVAL(t, "Txn.Renew", txn.Renew())
				if !bytes.Equal(val, overwriteValue(-1)) || string(ck) != "k" || string(cv) != "v2" {
					t.Error("RawRead slices changed after Close")
				}
				lockFile := SharedLockingAvailable()
				if lockFile {
					if err := openWithLockFile(t, path); !IsErrnoSys(err, syscall.EBUSY) {
						t.Fatalf("Open while a transaction outlives Close: %v, want EBUSY", err)
					}
				}
				c.Close()
				txn.Abort()
				if lockFile {
					if err := openWithLockFile(t, path); err != nil {
						t.Fatalf("Open after the transaction ended: %v", err)
					}
				}
			})
		}
	}
}

// A write transaction in progress at Close: its operations fail with
// EINVAL, its Commit aborts it (EINVAL), and its writes are gone.
func TestClosedEnv_writeTxn(t *testing.T) {
	for _, flags := range lockModes() {
		t.Run(fmt.Sprintf("%#x", flags), func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			env, path, db, dup := openClosedTest(t, V09, flags)
			txn, err := env.BeginTxn(nil, 0)
			check(t, err)
			check(t, txn.Put(db, []byte("written"), []byte("before Close"), 0))
			c, err := txn.OpenCursor(dup)
			check(t, err)
			_, _, err = c.Get(nil, nil, First)
			check(t, err)

			check(t, env.Close())
			for _, op := range txnOps(db, dup) {
				wantEINVAL(t, "Txn."+op.name, op.op(txn))
			}
			checkCursorClosed(t, c, txn)
			wantEINVAL(t, "Txn.Commit", txn.Commit())

			env2, err := NewEnv()
			check(t, err)
			defer env2.Close()
			check(t, env2.Open(path, testFlags(flags), 0o644))
			check(t, env2.View(func(txn *Txn) error {
				if _, err := txn.Get(db, []byte("written")); !IsNotFound(err) {
					t.Errorf("write of an aborted transaction: %v, want MDB_NOTFOUND", err)
				}
				return nil
			}))
		})
	}
}

// RawRead slices returned before Close hold their bytes while another
// writer overwrites their key: a NoLock handle opened after Close in this
// process (sharing the closed handle's reader table), or, with a lock
// file, a writer process (which sees the closed handle's reader slot).
// The transaction's Abort releases the snapshot: the other writer's last
// value is the current one.
func TestClosedEnv_snapshotKept(t *testing.T) {
	for _, flags := range lockModes() {
		t.Run(fmt.Sprintf("%#x", flags), func(t *testing.T) {
			env, path, db, _ := openClosedTest(t, V09, flags)
			txn, err := env.BeginTxn(nil, Readonly)
			check(t, err)
			txn.RawRead = true
			val, err := txn.Get(db, []byte("key"))
			check(t, err)
			check(t, env.Close())

			if flags&NoLock != 0 {
				env2, err := NewEnv()
				check(t, err)
				defer env2.Close()
				check(t, env2.Open(path, testFlags(NoLock)|NoSync, 0o644))
				for i := 0; i < overwriteRounds; i++ {
					check(t, env2.Update(func(txn *Txn) error { return txn.Put(db, []byte("key"), overwriteValue(i), 0) }))
				}
			} else {
				h := startHelper(t, "overwrite", path)
				fmt.Fprintln(h.stdin, "go")
				if line := h.line(t, 60*time.Second); line != "RESULT <nil>" {
					t.Fatalf("writer process: %q", line)
				}
			}
			if !bytes.Equal(val, overwriteValue(-1)) {
				t.Fatal("RawRead slice changed while another writer committed")
			}
			_, err = txn.Get(db, []byte("key"))
			wantEINVAL(t, "Txn.Get", err)
			txn.Abort()

			env3, err := NewEnv()
			check(t, err)
			defer env3.Close()
			check(t, env3.Open(path, testFlags(flags)|Readonly, 0o644))
			check(t, env3.View(func(txn *Txn) error {
				v, err := txn.Get(db, []byte("key"))
				if err == nil && !bytes.Equal(v, overwriteValue(overwriteRounds-1)) {
					err = errors.New("not the other writer's last value")
				}
				return err
			}))
		})
	}
}

// The finalizer of an unreachable read transaction that outlived Close
// aborts it, which tears the environment down.
func TestClosedEnv_finalizer(t *testing.T) {
	requireLockFile(t)
	env, path, db, _ := openClosedTest(t, V09, 0)
	func() {
		txn, err := env.BeginTxn(nil, Readonly)
		check(t, err)
		txn.errLogf = func(string, ...interface{}) {}
		_, err = txn.Get(db, []byte("key"))
		check(t, err)
	}()
	check(t, env.Close())
	deadline := time.Now().Add(20 * time.Second)
	for {
		runtime.GC()
		err := openWithLockFile(t, path)
		if err == nil {
			return
		}
		if !IsErrnoSys(err, syscall.EBUSY) || time.Now().After(deadline) {
			t.Fatalf("Open after the transaction became unreachable: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Readers in their own goroutines (transactions with cursors, and View)
// while another goroutine closes the environment: every reader ends with
// EINVAL (or, beginning after Close, the error of a closed Env), its
// RawRead slices hold their bytes, and once all have aborted the
// environment is torn down. Run with -race.
func TestClosedEnv_concurrentReaders(t *testing.T) {
	rounds := 10
	if testing.Short() || raceEnabled {
		rounds = 4
	}
	for _, flags := range lockModes() {
		t.Run(fmt.Sprintf("%#x", flags), func(t *testing.T) {
			for r := 0; r < rounds; r++ {
				testClosedConcurrentReaders(t, flags)
			}
		})
	}
}

func testClosedConcurrentReaders(t *testing.T, flags uint) {
	env, path, db, dup := openClosedTest(t, V09, flags)
	want := overwriteValue(-1)
	// Readers that still read this long after Close fail instead of
	// looping on.
	deadline := time.Now().Add(20 * time.Second)
	errStillReading := errors.New("reads still succeed after Close")
	var wg sync.WaitGroup
	start := make(chan struct{})
	reader := func() error {
		txn, err := env.BeginTxn(nil, Readonly)
		if err != nil {
			return err
		}
		defer txn.Abort()
		txn.RawRead = true
		c, err := txn.OpenCursor(dup)
		if err != nil {
			return err
		}
		defer c.Close()
		<-start
		var vals [][]byte
		for {
			v, err := txn.Get(db, []byte("key"))
			if err != nil {
				for _, v := range vals {
					if !bytes.Equal(v, want) {
						return errors.New("RawRead slice changed")
					}
				}
				return err
			}
			if len(vals) < 100 {
				vals = append(vals, v)
			} else if time.Now().After(deadline) {
				return errStillReading
			}
			if _, _, err := c.Get(nil, nil, Next); err != nil && !IsNotFound(err) {
				return err
			}
		}
	}
	viewer := func() error {
		<-start
		for {
			err := env.View(func(txn *Txn) error {
				_, err := txn.Get(db, []byte("key"))
				return err
			})
			if err != nil {
				return err
			}
			if time.Now().After(deadline) {
				return errStillReading
			}
		}
	}
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		fn := reader
		if i%4 == 3 {
			fn = viewer
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- fn()
		}()
	}
	close(start)
	time.Sleep(5 * time.Millisecond)
	check(t, env.Close())
	wg.Wait()
	close(errs)
	for err := range errs {
		if !IsErrnoSys(err, syscall.EINVAL) && err != errNotOpen {
			t.Errorf("reader: %v, want EINVAL", err)
		}
	}
	if SharedLockingAvailable() {
		if err := openWithLockFile(t, path); err != nil {
			t.Fatalf("Open after the readers ended: %v", err)
		}
	}
}
