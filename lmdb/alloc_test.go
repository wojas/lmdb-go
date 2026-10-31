// Allocation counts of the public API's hot paths, guarded against
// regressions (AGENTS.md, "Allocations (owner)"). Not in lmdb-go v1.
//
// TestAllocs runs each operation with testing.AllocsPerRun and fails
// unless the count equals the case's constant: the measured value, so a
// change that lowers a count lowers its constant in the same commit, and
// one that raises it says why. BenchmarkAllocs runs the same operations
// and reports allocs/op. The counts are skipped under -race, which
// changes them.
//
// AllocsPerRun reports whole allocations per run (rounded down), so
// growth amortised over many operations (a dirty page now and then)
// shows only once it reaches one per operation. Every case first runs
// its operation in a transaction that is thrown away, so that the page
// pool holds what the measured one needs, as it would in a long-running
// program.

package lmdb

import (
	"encoding/binary"
	"runtime"
	"testing"
)

// The expected allocations per operation.
const (
	// Read path: zero-copy reads allocate nothing; lmdb-go's copying Get
	// makes one slice; a transaction begun through View (or BeginTxn) is
	// one *Txn, as in lmdb-go v1.
	allocsView           = 1
	allocsGetRaw         = 0
	allocsGetCopy        = 1
	allocsCursorFirst    = 0
	allocsCursorNext     = 0
	allocsCursorPrev     = 0
	allocsCursorSet      = 0
	allocsCursorSetRange = 0
	allocsCursorGetBoth  = 0
	allocsResetRenew     = 0
	allocsCursorRenew    = 0

	// Write path. lmdb-go v1 makes one allocation (the *Txn) for Update
	// and BeginTxn+Commit, none for the operations inside a transaction.
	allocsUpdatePut    = 1
	allocsCommit       = 1
	allocsPut          = 0
	allocsOverwrite    = 0
	allocsDelete       = 0
	allocsPutReserve   = 0
	allocsPutDup       = 0
	allocsSub          = 14
	allocsUpdateBigPut = 1
)

// allocKeys is how many keys the cases' databases hold.
const allocKeys = 10000

// allocCase is an operation whose allocations are counted. setup
// prepares it (in a fresh environment) and returns the operation and the
// cleanup to run afterwards.
type allocCase struct {
	name  string
	want  float64
	setup func(t testing.TB, env *Env) (op func(), done func())
}

var allocCases = []allocCase{
	{"View", allocsView, setupView},
	{"GetRaw", allocsGetRaw, setupGet(true)},
	{"GetCopy", allocsGetCopy, setupGet(false)},
	{"CursorFirst", allocsCursorFirst, setupCursor(First, false)},
	{"CursorNext", allocsCursorNext, setupCursor(Next, false)},
	{"CursorPrev", allocsCursorPrev, setupCursor(Prev, false)},
	{"CursorSet", allocsCursorSet, setupCursor(Set, false)},
	{"CursorSetRange", allocsCursorSetRange, setupCursor(SetRange, false)},
	{"CursorGetBoth", allocsCursorGetBoth, setupCursor(GetBoth, true)},
	{"ResetRenew", allocsResetRenew, setupRenew(false)},
	{"CursorRenew", allocsCursorRenew, setupRenew(true)},
	{"UpdatePut", allocsUpdatePut, setupUpdatePut(100)},
	{"Commit", allocsCommit, setupCommit},
	{"Put", allocsPut, setupInTxn(opPut)},
	{"Overwrite", allocsOverwrite, setupInTxn(opOverwrite)},
	{"Delete", allocsDelete, setupInTxn(opDelete)},
	{"PutReserve", allocsPutReserve, setupInTxn(opPutReserve)},
	{"PutDup", allocsPutDup, setupInTxn(opPutDup)},
	{"Sub", allocsSub, setupSub},
	{"UpdateBigPut", allocsUpdateBigPut, setupUpdatePut(64 << 10)},
}

func TestAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("-race changes allocation counts")
	}
	for _, c := range allocCases {
		t.Run(c.name, func(t *testing.T) {
			env := allocEnv(t)
			op, done := c.setup(t, env)
			got := testing.AllocsPerRun(1000, op)
			done()
			if got != c.want {
				t.Errorf("%s: %v allocations per operation, want %v", c.name, got, c.want)
			}
		})
	}
}

// BenchmarkAllocs reports allocs/op (whole allocations per operation,
// rounded down like TestAllocs's counts) and mallocs/op, the exact mean,
// which shows allocations made only every so many operations.
func BenchmarkAllocs(b *testing.B) {
	for _, c := range allocCases {
		b.Run(c.name, func(b *testing.B) {
			env := allocEnv(b)
			op, done := c.setup(b, env)
			var before, after runtime.MemStats
			b.ReportAllocs()
			b.ResetTimer()
			runtime.ReadMemStats(&before)
			for i := 0; i < b.N; i++ {
				op()
			}
			runtime.ReadMemStats(&after)
			b.StopTimer()
			b.ReportMetric(float64(after.Mallocs-before.Mallocs)/float64(b.N), "mallocs/op")
			done()
		})
	}
}

// allocEnv is a new environment (NoSync: commits need not reach the disk
// to count allocations) with a main DB of allocKeys keys and a DupSort DB
// "dup" with allocKeys/10 keys of 10 values each.
func allocEnv(t testing.TB) *Env {
	t.Helper()
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.SetMaxDBs(2); err != nil {
		t.Fatal(err)
	}
	if err := env.SetMapSize(1 << 30); err != nil {
		t.Fatal(err)
	}
	if err := env.Open(t.TempDir(), testFlags(NoSync), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Close() })
	err = env.Update(func(txn *Txn) error {
		db, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		dup, err := txn.OpenDBI("dup", Create|DupSort)
		if err != nil {
			return err
		}
		for i := 0; i < allocKeys; i++ {
			if err := txn.Put(db, allocKey(i), allocVal(i), 0); err != nil {
				return err
			}
			if i%10 == 0 {
				for j := 0; j < 10; j++ {
					if err := txn.Put(dup, allocKey(i), allocVal(j), 0); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// allocKey is key i: 16 bytes, keys in a pseudo-random order.
func allocKey(i int) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b, uint64(i)*0x9e3779b97f4a7c15)
	binary.BigEndian.PutUint64(b[8:], uint64(i))
	return b
}

// allocVal is a 100-byte value.
func allocVal(i int) []byte {
	b := make([]byte, 100)
	binary.BigEndian.PutUint64(b, uint64(i))
	return b
}

// allocKeySet is n keys made in advance, so that making them is not
// counted.
func allocKeySet(n int) [][]byte {
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = allocKey(i)
	}
	return keys
}

func dbis(t testing.TB, txn *Txn) (db, dup DBI) {
	t.Helper()
	db, err := txn.OpenRoot(0)
	if err != nil {
		t.Fatal(err)
	}
	if dup, err = txn.OpenDBI("dup", 0); err != nil {
		t.Fatal(err)
	}
	return db, dup
}

func check(t testing.TB, err error) {
	if err != nil {
		t.Helper()
		t.Fatal(err)
	}
}

// --- read path ------------------------------------------------------------

func setupView(t testing.TB, env *Env) (func(), func()) {
	keys := allocKeySet(allocKeys)
	var db DBI
	check(t, env.View(func(txn *Txn) (err error) { db, _ = dbis(t, txn); return nil }))
	i := 0
	fn := func(txn *Txn) error {
		txn.RawRead = true
		_, err := txn.Get(db, keys[i%allocKeys])
		return err
	}
	return func() {
		i++
		check(t, env.View(fn))
	}, func() {}
}

func beginRead(t testing.TB, env *Env, raw bool) *Txn {
	t.Helper()
	txn, err := env.BeginTxn(nil, Readonly)
	check(t, err)
	txn.RawRead = raw
	return txn
}

func setupGet(raw bool) func(testing.TB, *Env) (func(), func()) {
	return func(t testing.TB, env *Env) (func(), func()) {
		keys := allocKeySet(allocKeys)
		txn := beginRead(t, env, raw)
		db, _ := dbis(t, txn)
		i := 0
		return func() {
			i++
			_, err := txn.Get(db, keys[i%allocKeys])
			check(t, err)
		}, txn.Abort
	}
}

func setupCursor(op uint, dupDB bool) func(testing.TB, *Env) (func(), func()) {
	return func(t testing.TB, env *Env) (func(), func()) {
		keys := allocKeySet(allocKeys)
		vals := make([][]byte, 10)
		for j := range vals {
			vals[j] = allocVal(j)
		}
		txn := beginRead(t, env, true)
		db, dup := dbis(t, txn)
		if dupDB {
			db = dup
		}
		cur, err := txn.OpenCursor(db)
		check(t, err)
		_, _, err = cur.Get(nil, nil, First)
		check(t, err)
		i := 0
		return func() {
			i++
			var err error
			switch op {
			case Next, Prev:
				if _, _, err = cur.Get(nil, nil, op); IsNotFound(err) {
					_, _, err = cur.Get(nil, nil, First)
				}
			case Set, SetRange:
				_, _, err = cur.Get(keys[i%allocKeys], nil, op)
			case GetBoth:
				_, _, err = cur.Get(keys[i%(allocKeys/10)*10], vals[i%10], op)
			default:
				_, _, err = cur.Get(nil, nil, op)
			}
			check(t, err)
		}, func() { cur.Close(); txn.Abort() }
	}
}

func setupRenew(withCursor bool) func(testing.TB, *Env) (func(), func()) {
	return func(t testing.TB, env *Env) (func(), func()) {
		txn := beginRead(t, env, true)
		db, _ := dbis(t, txn)
		cur, err := txn.OpenCursor(db)
		check(t, err)
		return func() {
				txn.Reset()
				check(t, txn.Renew())
				if withCursor {
					check(t, cur.Renew(txn))
				}
			}, func() {
				cur.Close()
				txn.Abort()
			}
	}
}

// --- write path -----------------------------------------------------------

// setupUpdatePut: one overwrite of an existing key with a vlen-byte value
// per Update.
func setupUpdatePut(vlen int) func(testing.TB, *Env) (func(), func()) {
	return func(t testing.TB, env *Env) (func(), func()) {
		keys := allocKeySet(allocKeys)
		val := make([]byte, vlen)
		var db DBI
		check(t, env.View(func(txn *Txn) (err error) { db, _ = dbis(t, txn); return nil }))
		i := 0
		fn := func(txn *Txn) error { return txn.Put(db, keys[i%allocKeys], val, 0) }
		return func() {
			i++
			check(t, env.Update(fn))
		}, func() {}
	}
}

// setupCommit: BeginTxn, one overwrite, Commit.
func setupCommit(t testing.TB, env *Env) (func(), func()) {
	keys := allocKeySet(allocKeys)
	val := allocVal(1)
	var db DBI
	check(t, env.View(func(txn *Txn) (err error) { db, _ = dbis(t, txn); return nil }))
	runtime.LockOSThread()
	i := 0
	return func() {
		i++
		txn, err := env.BeginTxn(nil, 0)
		check(t, err)
		check(t, txn.Put(db, keys[i%allocKeys], val, 0))
		check(t, txn.Commit())
	}, runtime.UnlockOSThread
}

// A write operation of setupInTxn: the i-th of a series in one
// transaction, on the main DB db or the DupSort DB dup.
type writeOp func(t testing.TB, txn *Txn, db, dup DBI, keys [][]byte, val []byte, i int)

func opPut(t testing.TB, txn *Txn, db, dup DBI, keys [][]byte, val []byte, i int) {
	// New keys: after the allocKeys the database holds.
	check(t, txn.Put(db, keys[allocKeys+i%allocKeys], val, 0))
}

func opOverwrite(t testing.TB, txn *Txn, db, dup DBI, keys [][]byte, val []byte, i int) {
	check(t, txn.Put(db, keys[i%allocKeys], val, 0))
}

func opDelete(t testing.TB, txn *Txn, db, dup DBI, keys [][]byte, val []byte, i int) {
	if i/allocKeys%2 == 1 {
		// Every key is gone (only benchmarks get here): put them back.
		check(t, txn.Put(db, keys[i%allocKeys], val, 0))
		return
	}
	check(t, txn.Del(db, keys[i%allocKeys], nil))
}

func opPutDup(t testing.TB, txn *Txn, db, dup DBI, keys [][]byte, val []byte, i int) {
	// A new value for one of the DupSort keys (each holds 10 at first).
	binary.BigEndian.PutUint64(val, uint64(1<<32+i))
	check(t, txn.Put(dup, keys[i%(allocKeys/10)*10], val, 0))
}

func opPutReserve(t testing.TB, txn *Txn, db, dup DBI, keys [][]byte, val []byte, i int) {
	b, err := txn.PutReserve(db, keys[i%allocKeys], len(val), 0)
	check(t, err)
	copy(b, val)
}

// setupInTxn: op, the i-th time in one write transaction. The same series
// runs once first in a transaction that is aborted (its pages fill the
// pool). Delete needs at most allocKeys operations.
func setupInTxn(op writeOp) func(testing.TB, *Env) (func(), func()) {
	return func(t testing.TB, env *Env) (func(), func()) {
		keys := allocKeySet(2 * allocKeys)
		val := allocVal(2)
		runtime.LockOSThread()
		warm, err := env.BeginTxn(nil, 0)
		check(t, err)
		db, dup := dbis(t, warm)
		for i := 0; i < 1001; i++ {
			op(t, warm, db, dup, keys, val, i)
		}
		warm.Abort()
		txn, err := env.BeginTxn(nil, 0)
		check(t, err)
		i := 0
		return func() {
				op(t, txn, db, dup, keys, val, i)
				i++
			}, func() {
				txn.Abort()
				runtime.UnlockOSThread()
			}
	}
}

// setupSub: a nested transaction with one overwrite, in one write
// transaction.
func setupSub(t testing.TB, env *Env) (func(), func()) {
	keys := allocKeySet(allocKeys)
	val := allocVal(3)
	runtime.LockOSThread()
	txn, err := env.BeginTxn(nil, 0)
	check(t, err)
	db, _ := dbis(t, txn)
	i := 0
	fn := func(sub *Txn) error { return sub.Put(db, keys[i%allocKeys], val, 0) }
	return func() {
			i++
			check(t, txn.Sub(fn))
		}, func() {
			txn.Abort()
			runtime.UnlockOSThread()
		}
}
