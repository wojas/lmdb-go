package lmdb

// Tests of what this implementation does differently from lmdb-go v1 on C
// LMDB, or newly (DEVIATIONS.md).

import (
	"bytes"
	"errors"
	"os"
	"strings"
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
