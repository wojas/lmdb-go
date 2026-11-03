package lmdb

// What rlmdb's C API review fixed, through this API, where lmdb-go's
// behaviour comes from the same C checks: a failed transaction before a
// cursor operation (rlmdb 16feee5), the ignored value of Del (fe9fd67), the
// descriptor of a copy (a59627f), and a transaction with active descendants
// (3a97557).

import (
	"bytes"
	"fmt"
	"runtime"
	"syscall"
	"testing"
)

// TestCursor_Get_failedTxn: C's mdb_cursor_get returns MDB_BAD_TXN for a
// blocked transaction before its switch on the operation: after a put failed
// with MDB_MAP_FULL an unknown operation, and MDB_GET_BOTH or
// MDB_GET_MULTIPLE without data, give MDB_BAD_TXN rather than EINVAL.
func TestCursor_Get_failedTxn(t *testing.T) {
	env, err := NewEnv()
	must(t, err)
	defer env.Close()
	must(t, env.SetMapSize(1<<20))
	must(t, env.Open(t.TempDir(), testFlags(NoSync), 0o644))
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	txn, err := env.BeginTxn(nil, 0)
	must(t, err)
	defer txn.Abort()
	c, err := txn.OpenCursor(1)
	must(t, err)
	big := bytes.Repeat([]byte("y"), 3000)
	for i := 0; ; i++ {
		err := txn.Put(1, []byte(fmt.Sprintf("%08d", i)), big, 0)
		if IsMapFull(err) {
			break
		}
		must(t, err)
	}
	for _, op := range []uint{9999, GetBoth, GetBothRange, GetMultiple, First} {
		if _, _, err := c.Get(nil, nil, op); !IsErrno(err, BadTxn) {
			t.Errorf("op %d: %v, want MDB_BAD_TXN", op, err)
		}
	}
}

// TestTxn_Del_ignoresVal: lmdb.h: mdb_del ignores data unless the database
// has MDB_DUPSORT: a value that is not the key's, or that could not be
// stored, deletes the key all the same.
func TestTxn_Del_ignoresVal(t *testing.T) {
	env := setup(t)
	defer clean(env, t)
	big := make([]byte, 1<<26)
	must(t, env.Update(func(txn *Txn) error {
		db, err := txn.OpenDBI("plain", Create)
		if err != nil {
			return err
		}
		for _, k := range []string{"a", "b", "c"} {
			if err := txn.Put(db, []byte(k), []byte("v"), 0); err != nil {
				return err
			}
		}
		for _, d := range []struct {
			k string
			v []byte
		}{{"a", []byte("not its value")}, {"b", big}, {"c", nil}} {
			if err := txn.Del(db, []byte(d.k), d.v); err != nil {
				return fmt.Errorf("del %q: %w", d.k, err)
			}
			if _, err := txn.Get(db, []byte(d.k)); !IsNotFound(err) {
				return fmt.Errorf("get %q after the del: %v", d.k, err)
			}
		}
		return nil
	}))
}

// TestEnv_CopyFD_badDescriptor: C's mdb_env_copyfd writes to the descriptor
// and returns what the first write says: EBADF for -1, plain and compacting
// (on Windows ERROR_INVALID_HANDLE, WriteFile's error).
func TestEnv_CopyFD_badDescriptor(t *testing.T) {
	env := setup(t)
	defer clean(env, t)
	want := syscall.Errno(syscall.EBADF)
	if runtime.GOOS == "windows" {
		want = 6 // ERROR_INVALID_HANDLE
	}
	bad := ^uintptr(0)
	if err := env.CopyFD(bad); !IsErrnoSys(err, want) {
		t.Errorf("CopyFD: %v, want %v", err, want)
	}
	if err := env.CopyFDFlag(bad, CopyCompact); !IsErrnoSys(err, want) {
		t.Errorf("CopyFDFlag: %v, want %v", err, want)
	}
}

// TestTxn_Commit_descendants: mdb_txn_commit commits active descendants
// first, at any depth, and fails with MDB_BAD_TXN when one of them failed, in
// which case nothing is written.
func TestTxn_Commit_descendants(t *testing.T) {
	env, err := NewEnv()
	must(t, err)
	defer env.Close()
	must(t, env.SetMapSize(1<<20))
	must(t, env.Open(t.TempDir(), testFlags(NoSync), 0o644))
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	begin := func(parent *Txn) *Txn {
		t.Helper()
		txn, err := env.BeginTxn(parent, 0)
		must(t, err)
		return txn
	}
	parent := begin(nil)
	child := begin(parent)
	grand := begin(child)
	must(t, grand.Put(1, []byte("e2"), []byte("from the grandchild"), 0))
	must(t, parent.Commit())
	must(t, env.View(func(txn *Txn) error {
		v, err := txn.Get(1, []byte("e2"))
		if err == nil && string(v) != "from the grandchild" {
			err = fmt.Errorf("e2: %q", v)
		}
		return err
	}))

	parent = begin(nil)
	must(t, parent.Put(1, []byte("parent"), []byte("lost"), 0))
	child = begin(parent)
	grand = begin(child)
	big := bytes.Repeat([]byte("y"), 3000)
	for i := 0; ; i++ {
		err := grand.Put(1, []byte(fmt.Sprintf("%08d", i)), big, 0)
		if IsMapFull(err) {
			break
		}
		must(t, err)
	}
	if err := parent.Commit(); !IsErrno(err, BadTxn) {
		t.Fatalf("commit: %v, want MDB_BAD_TXN", err)
	}
	must(t, env.View(func(txn *Txn) error {
		if _, err := txn.Get(1, []byte("parent")); !IsNotFound(err) {
			return fmt.Errorf("parent's key: %v", err)
		}
		return nil
	}))
}
