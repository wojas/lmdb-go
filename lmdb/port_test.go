package lmdb

// Tests of what this implementation does differently from lmdb-go v1 on C
// LMDB, or newly (DEVIATIONS.md).

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// putDups stores key with the values vals in a new DupSort|DupFixed
// database and returns it.
func putDups(t *testing.T, env *Env, name string, key []byte, vals ...string) DBI {
	t.Helper()
	var db DBI
	err := env.Update(func(txn *Txn) (err error) {
		db, err = txn.OpenDBI(name, Create|DupSort|DupFixed)
		if err != nil {
			return err
		}
		for _, v := range vals {
			if err := txn.Put(db, key, []byte(v), 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// lmdb-go v1 passed an empty value for a nil val, so C looked for that
// value: MDB_NOTFOUND, or MDB_BAD_VALSIZE with a sub-page or sub-DB.
func TestTxn_Del_dupNil(t *testing.T) {
	env := setup(t)
	defer clean(env, t)
	key := []byte("k")
	db := putDups(t, env, "dups", key, "a", "b", "c")
	err := env.Update(func(txn *Txn) error {
		return txn.Del(db, key, nil)
	})
	if err != nil {
		t.Fatalf("Del(key, nil): %v", err)
	}
	err = env.View(func(txn *Txn) error {
		_, err := txn.Get(db, key)
		return err
	})
	if !IsNotFound(err) {
		t.Errorf("key still there after Del(key, nil): %v", err)
	}
	// A given value deletes only that pair.
	db = putDups(t, env, "dups", key, "a", "b")
	err = env.Update(func(txn *Txn) error {
		return txn.Del(db, key, []byte("a"))
	})
	if err != nil {
		t.Fatal(err)
	}
	err = env.View(func(txn *Txn) error {
		v, err := txn.Get(db, key)
		if err == nil && string(v) != "b" {
			t.Errorf("Get after deleting one value: %q", v)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// lmdb-go v1 returned an empty page for MDB_GET_MULTIPLE on a key with one
// value.
func TestCursor_GetMultiple_single(t *testing.T) {
	env := setup(t)
	defer clean(env, t)
	db := putDups(t, env, "one", []byte("k1"), "ab")
	putDups(t, env, "one", []byte("k2"), "cd", "ef", "gh")
	err := env.View(func(txn *Txn) error {
		for _, raw := range []bool{false, true} {
			txn.RawRead = raw
			cur, err := txn.OpenCursor(db)
			if err != nil {
				return err
			}
			defer cur.Close()
			if _, _, err := cur.Get([]byte("k1"), nil, Set); err != nil {
				return err
			}
			_, page, err := cur.Get(nil, nil, GetMultiple)
			if err != nil {
				return err
			}
			if string(page) != "ab" {
				t.Errorf("RawRead=%v: GetMultiple on a single value: %q", raw, page)
			}
			k, page, err := cur.Get([]byte("k2"), nil, Set)
			if err != nil {
				return err
			}
			if _, page, err = cur.Get(nil, nil, GetMultiple); err != nil {
				return err
			}
			if string(k) != "k2" || string(page) != "cdefgh" {
				t.Errorf("RawRead=%v: GetMultiple: %q %q", raw, k, page)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// lmdb-go v1 panicked with RawRead after MDB_FIRST_DUP and MDB_LAST_DUP,
// for which C leaves the key unset.
func TestCursor_FirstDup_rawRead(t *testing.T) {
	env := setup(t)
	defer clean(env, t)
	db := putDups(t, env, "dups", []byte("k"), "aa", "bb", "cc")
	err := env.View(func(txn *Txn) error {
		txn.RawRead = true
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}
		defer cur.Close()
		if _, _, err := cur.Get(nil, nil, First); err != nil {
			return err
		}
		for op, want := range map[uint]string{LastDup: "cc", FirstDup: "aa"} {
			k, v, err := cur.Get(nil, nil, op)
			if err != nil {
				return err
			}
			if string(k) != "k" || string(v) != want {
				t.Errorf("op %d: %q %q", op, k, v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A cursor of a reset and renewed transaction starts over unpositioned
// unless renewed (C keeps pointers into the old snapshot).
func TestCursor_txnRenewed(t *testing.T) {
	env := setup(t)
	defer clean(env, t)
	db, err := openRoot(env, Create)
	if err != nil {
		t.Fatal(err)
	}
	err = env.Update(func(txn *Txn) error {
		return txn.Put(db, []byte("a"), []byte("1"), 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	txn, err := env.BeginTxn(nil, Readonly)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Abort()
	cur, err := txn.OpenCursor(db)
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	if _, _, err := cur.Get(nil, nil, First); err != nil {
		t.Fatal(err)
	}
	txn.Reset()
	if _, _, err := cur.Get(nil, nil, GetCurrent); err == nil {
		t.Error("cursor of a reset transaction works")
	}
	if err := txn.Renew(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cur.Get(nil, nil, GetCurrent); !IsErrnoSys(err, syscall.EINVAL) {
		t.Errorf("GetCurrent on a restarted cursor: %v (want EINVAL: unpositioned)", err)
	}
	if k, _, err := cur.Get(nil, nil, Next); err != nil || string(k) != "a" {
		t.Errorf("Next on a restarted cursor: %q %v", k, err)
	}
}

// Transactions and cursors fail cleanly once ended or closed, and a read
// transaction may end after its environment was closed (v1: its finalizer).
func TestTxn_afterEnd(t *testing.T) {
	env := setup(t)
	path, _ := env.Path()
	db, err := openRoot(env, Create)
	if err != nil {
		t.Fatal(err)
	}
	txn, err := env.BeginTxn(nil, Readonly)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := txn.OpenCursor(db)
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.BeginTxn(nil, Readonly)
	if err != nil {
		t.Fatal(err)
	}
	txn.Abort()
	if _, err := txn.Get(db, []byte("a")); !IsErrnoSys(err, syscall.EINVAL) {
		t.Errorf("Get after Abort: %v", err)
	}
	if _, _, err := cur.Get(nil, nil, First); !IsErrnoSys(err, syscall.EINVAL) {
		t.Errorf("cursor Get after Abort: %v", err)
	}
	if err := txn.Commit(); !IsErrnoSys(err, syscall.EINVAL) {
		t.Errorf("Commit after Abort: %v", err)
	}
	cur.Close()
	cur.Close()
	if err := env.Close(); err != nil {
		t.Fatal(err)
	}
	other.Reset()
	if err := other.Renew(); err == nil {
		t.Error("Renew after Close succeeded")
	}
	other.Abort()
	if _, err := env.BeginTxn(nil, Readonly); err == nil {
		t.Error("BeginTxn after Close succeeded")
	}
	os.RemoveAll(path)
}

// ReaderList lists the readers as mdb_reader_list does, one line per call.
func TestEnv_ReaderList_lines(t *testing.T) {
	requireLockFile(t)
	env := setup(t)
	defer clean(env, t)
	var lines []string
	collect := func(s string) error { lines = append(lines, s); return nil }
	if err := env.ReaderList(collect); err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "") != "(no active readers)\n" {
		t.Errorf("no readers: %q", lines)
	}
	txn, err := env.BeginTxn(nil, Readonly)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Abort()
	lines = nil
	if err := env.ReaderList(collect); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "    pid     thread     txnid\n" || !strings.HasSuffix(lines[1], " 0\n") {
		t.Errorf("one reader: %q", lines)
	}
}

// Without cgo, Open without NoLock fails with ErrNoSharedLocking before it
// touches the file system.
func TestEnv_Open_noSharedLocking(t *testing.T) {
	if SharedLockingAvailable() {
		t.Skip("a cgo build has shared locking")
	}
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	dir := t.TempDir()
	if err := env.Open(dir, 0, 0644); !errors.Is(err, ErrNoSharedLocking) {
		t.Fatalf("Open: %v", err)
	}
	if err := env.Open(dir, NoLock, 0644); err != nil {
		t.Fatalf("Open with NoLock: %v", err)
	}
}

// Values copied out of the map (RawRead false) are independent of it.
func TestCursor_Get_copies(t *testing.T) {
	env := setup(t)
	defer clean(env, t)
	db, err := openRoot(env, Create)
	if err != nil {
		t.Fatal(err)
	}
	err = env.Update(func(txn *Txn) error {
		if err := txn.Put(db, []byte("key"), []byte("value"), 0); err != nil {
			return err
		}
		return txn.Put(db, []byte("empty"), nil, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	err = env.View(func(txn *Txn) error {
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}
		defer cur.Close()
		k, v, err := cur.Get([]byte("key"), nil, SetKey)
		if err != nil {
			return err
		}
		k = append(k, "xx"...)
		if !bytes.Equal(v, []byte("value")) {
			t.Errorf("value changed by appending to the key: %q", v)
		}
		k, v, err = cur.Get([]byte("empty"), nil, SetKey)
		if err != nil {
			return err
		}
		if string(k) != "empty" || v == nil || len(v) != 0 {
			t.Errorf("empty value: %q %#v", k, v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A read transaction that ends leaves its reader slot idle and owned by
// the process, as C's default (TLS) mode leaves a thread's slot, and the
// next one reuses it (slot pool, DEVIATIONS.md): ReaderList shows the idle
// slot as C's mdb_reader_list does ("-"), and no second slot appears.
func TestEnv_ReaderList_idleSlot(t *testing.T) {
	requireLockFile(t)
	env := setup(t)
	defer clean(env, t)
	var lines []string
	collect := func(s string) error { lines = append(lines, s); return nil }
	for i := 0; i < 3; i++ {
		if err := env.View(func(*Txn) error { return nil }); err != nil {
			t.Fatal(err)
		}
		lines = nil
		if err := env.ReaderList(collect); err != nil {
			t.Fatal(err)
		}
		if len(lines) != 2 || !strings.HasSuffix(lines[1], " -\n") ||
			strings.Fields(lines[1])[0] != strconv.Itoa(os.Getpid()) {
			t.Fatalf("after View %d: %q", i, lines)
		}
	}
	info, err := env.Info()
	if err != nil || info.NumReaders != 1 {
		t.Fatalf("Info: %+v, %v", info, err)
	}
}

// Many goroutines run View while a writer runs Update: every snapshot is
// consistent, and the reader table ends with idle slots only, at most the
// pool's bound (MaxReaders/4). Run with -race.
func TestEnv_View_concurrentSlots(t *testing.T) {
	env := setup(t)
	defer clean(env, t)
	var db DBI
	val := func(n uint64, k byte) []byte {
		v := make([]byte, 200)
		binary.BigEndian.PutUint64(v, n)
		for i := 8; i < len(v); i++ {
			v[i] = byte(n) ^ byte(i) ^ k
		}
		return v
	}
	if err := env.View(func(txn *Txn) (err error) { db, err = txn.OpenRoot(0); return err }); err != nil {
		t.Fatal(err)
	}
	update := func(n uint64) error {
		return env.Update(func(txn *Txn) (err error) {
			if err := txn.Put(db, []byte("a"), val(n, 'a'), 0); err != nil {
				return err
			}
			return txn.Put(db, []byte("b"), val(n, 'b'), 0)
		})
	}
	if err := update(0); err != nil {
		t.Fatal(err)
	}
	iters := 5000
	if testing.Short() || raceEnabled {
		iters = 500
	}
	var stop atomic.Bool
	errs := make(chan error, 17)
	var wg, rw sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := uint64(1); !stop.Load(); n++ {
			if err := update(n); err != nil {
				errs <- err
				return
			}
		}
	}()
	for g := 0; g < 16; g++ {
		rw.Add(1)
		go func() {
			defer rw.Done()
			for i := 0; i < iters; i++ {
				err := env.View(func(txn *Txn) error {
					txn.RawRead = true
					a, err := txn.Get(db, []byte("a"))
					if err != nil {
						return err
					}
					b, err := txn.Get(db, []byte("b"))
					if err != nil {
						return err
					}
					n := binary.BigEndian.Uint64(a)
					if !bytes.Equal(a, val(n, 'a')) || !bytes.Equal(b, val(n, 'b')) {
						return fmt.Errorf("inconsistent snapshot %d: a=%d b=%d", txn.ID(), n, binary.BigEndian.Uint64(b))
					}
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	rw.Wait()
	stop.Store(true)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if testFlags(0)&NoLock != 0 {
		return
	}
	idle := 0
	err := env.ReaderList(func(s string) error {
		if f := strings.Fields(s); len(f) == 3 && f[0] != "pid" {
			if f[2] != "-" {
				return fmt.Errorf("a reader is still in a transaction: %q", s)
			}
			idle++
		}
		return nil
	})
	if err != nil || idle > 126/4 {
		t.Fatalf("reader table after the run: %d idle slots, %v", idle, err)
	}
}

// Views run while SetMapSize replaces the map (review finding: the meta
// pages were read through the current map without a reference, racing
// with its unmap). Run with -race.
func TestEnv_SetMapSize_concurrentViews(t *testing.T) {
	for _, flags := range []uint{0, NoLock} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			if flags == 0 {
				requireLockFile(t)
			}
			env := setupFlags(t, flags)
			defer clean(env, t)
			iters := 10000
			if testing.Short() || raceEnabled {
				iters = 1000
			}
			var wg sync.WaitGroup
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < iters; i++ {
						if err := env.View(func(*Txn) error { return nil }); err != nil {
							t.Error(err)
							return
						}
					}
				}()
			}
			for i := 0; i < iters/3; i++ {
				if err := env.SetMapSize(16 << 20); err != nil {
					t.Fatal(err)
				}
			}
			wg.Wait()
		})
	}
}

// NoLock: every snapshot's data is the data of its txnid while a writer
// commits (review finding P1-1, the reviewer's reproducer: a reader could
// take a torn meta copy, publish the newer txnid and read older pages).
func TestNoLock_snapshotMatchesID(t *testing.T) {
	env := setupFlags(t, NoLock|NoSync)
	defer clean(env, t)
	if err := env.SetMapSize(1 << 30); err != nil {
		t.Fatal(err)
	}
	var db DBI
	put := func(txn *Txn) error { return txn.Put(db, []byte("key"), []byte(fmt.Sprint(txn.ID())), 0) }
	err := env.Update(func(txn *Txn) (err error) {
		if db, err = txn.OpenRoot(0); err != nil {
			return err
		}
		return put(txn)
	})
	if err != nil {
		t.Fatal(err)
	}
	commits := 20000
	if testing.Short() || raceEnabled {
		commits = 2000
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				err := env.View(func(txn *Txn) error {
					txn.RawRead = true
					v, err := txn.Get(db, []byte("key"))
					if err != nil {
						return err
					}
					if string(v) != fmt.Sprint(txn.ID()) {
						return fmt.Errorf("snapshot %d holds the data of %s", txn.ID(), v)
					}
					return nil
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for i := 0; i < commits && !t.Failed(); i++ {
		if err := env.Update(put); err != nil {
			t.Error(err)
		}
	}
	stop.Store(true)
	wg.Wait()
}
