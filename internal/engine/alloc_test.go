package engine

// The hot read path allocates nothing: read transaction begin (reusing a
// Txn), reset, renew and abort, Get, and cursor Next/Prev/Set/SetRange.

import (
	"encoding/binary"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func allocEnv(t testing.TB, lock bool) *Env {
	opts := Options{MaxDBs: 4}
	if !lock {
		opts.Flags = format.NoLock
	}
	env, _ := openTest(t, format.V09, opts)
	txn, _ := env.BeginTxn(nil, 0)
	db, _ := txn.OpenDBI([]byte("d"), format.Create)
	for i := uint32(0); i < 20000; i++ {
		k := binary.BigEndian.AppendUint32(nil, i)
		must(t, txn.Put(db, k, k, 0))
		must(t, txn.Put(MainDBI, k, k, 0))
	}
	must(t, txn.Commit())
	return env
}

func TestHotReadPathAllocations(t *testing.T) {
	for _, lock := range []bool{true, false} {
		env := allocEnv(t, lock)
		var txn Txn
		must(t, env.BeginRead(&txn))
		dbi, err := txn.OpenDBI([]byte("d"), 0)
		must(t, err)
		txn.Abort()
		key := binary.BigEndian.AppendUint32(nil, 1234)
		check := func(name string, f func()) {
			t.Helper()
			if n := testing.AllocsPerRun(200, f); n != 0 {
				t.Errorf("lock=%v %s: %v allocations", lock, name, n)
			}
		}
		check("begin/get/abort", func() {
			if err := env.BeginRead(&txn); err != nil {
				t.Fatal(err)
			}
			if _, err := txn.Get(dbi, key); err != nil {
				t.Fatal(err)
			}
			if _, err := txn.Get(MainDBI, key); err != nil {
				t.Fatal(err)
			}
			txn.Abort()
		})
		must(t, env.BeginRead(&txn))
		check("reset/renew", func() {
			txn.Reset()
			if err := txn.Renew(); err != nil {
				t.Fatal(err)
			}
		})
		c, err := txn.OpenCursor(dbi)
		must(t, err)
		check("cursor", func() {
			if _, _, err := c.Get(key, nil, format.Set); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 50; i++ {
				if _, _, err := c.Get(nil, nil, format.Next); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 50; i++ {
				if _, _, err := c.Get(nil, nil, format.Prev); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := c.Get(key, nil, format.SetRange); err != nil {
				t.Fatal(err)
			}
		})
		check("cursor renew", func() {
			txn.Reset()
			if err := txn.Renew(); err != nil {
				t.Fatal(err)
			}
			if err := c.Renew(&txn); err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.Get(nil, nil, format.First); err != nil {
				t.Fatal(err)
			}
		})
		txn.Abort()
		env.Close()
	}
}

func BenchmarkGet(b *testing.B) {
	env := allocEnv(b, true)
	var txn Txn
	must(b, env.BeginRead(&txn))
	defer txn.Abort()
	dbi, _ := txn.OpenDBI([]byte("d"), 0)
	keys := make([][]byte, 20000)
	for i := range keys {
		keys[i] = binary.BigEndian.AppendUint32(nil, uint32(i*7919%20000))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := txn.Get(dbi, keys[i%len(keys)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCursorNext(b *testing.B) {
	env := allocEnv(b, true)
	var txn Txn
	must(b, env.BeginRead(&txn))
	defer txn.Abort()
	dbi, _ := txn.OpenDBI([]byte("d"), 0)
	c, _ := txn.OpenCursor(dbi)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := c.Get(nil, nil, format.Next); err != nil {
			if _, _, err := c.Get(nil, nil, format.First); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkReadTxn(b *testing.B) {
	env := allocEnv(b, true)
	var txn Txn
	for i := 0; i < b.N; i++ {
		if err := env.BeginRead(&txn); err != nil {
			b.Fatal(err)
		}
		txn.Abort()
	}
}

func BenchmarkPut(b *testing.B) {
	env := allocEnv(b, true)
	txn, _ := env.BeginTxn(nil, 0)
	k := make([]byte, 8)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		binary.BigEndian.PutUint64(k, uint64(i%100000*7919))
		if err := txn.Put(MainDBI, k, k, 0); err != nil {
			b.Fatal(err)
		}
		if i%10000 == 9999 {
			must(b, txn.Commit())
			txn, _ = env.BeginTxn(nil, 0)
		}
	}
	must(b, txn.Commit())
}
