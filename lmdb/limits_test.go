package lmdb

// The limits the port has where C LMDB has none (rlmdb's
// rlmdb-capi/tests/c/capi_limits.c and rlmdb/tests/rust_env.rs): more than
// 1,048,576 named databases or reader slots is EINVAL (rlmdb
// KNOWN_C_BUGS.md rows 54 and 60), and an existing lock file with a larger
// table is still attached to, as in C.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const limit = 1 << 20 // rlmdb MAX_DBS_LIMIT and MAX_READERS_LIMIT

// TestSetMaxDBsLimit: SetMaxDBs beyond the limit is EINVAL, up to it the
// setting is taken, and a large count within it opens and works.
func TestSetMaxDBsLimit(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	for _, n := range []int{limit + 1, 1<<32 - 1, 1 << 32} {
		if err := env.SetMaxDBs(n); !errIsEINVAL(err) {
			t.Errorf("SetMaxDBs(%d): %v, want EINVAL", n, err)
		}
	}
	for _, n := range []int{limit, 100000} {
		if err := env.SetMaxDBs(n); err != nil {
			t.Errorf("SetMaxDBs(%d): %v", n, err)
		}
	}
	if err := env.Open(t.TempDir(), testFlags(0), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.SetMaxDBs(1); !errIsEINVAL(err) {
		t.Errorf("SetMaxDBs after Open: %v, want EINVAL", err)
	}
	err = env.Update(func(txn *Txn) error {
		dbi, err := txn.OpenDBI("one", Create)
		if err != nil {
			return err
		}
		return txn.Put(dbi, []byte("k"), []byte("v"), 0)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSetMaxReadersLimit: SetMaxReaders beyond the limit is EINVAL (0 too,
// as in C), up to it the setting is taken, and a large count within it
// makes a lock file of that many slots.
func TestSetMaxReadersLimit(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	for _, n := range []int{limit + 1, 1<<32 - 1, 1 << 32, 0} {
		if err := env.SetMaxReaders(n); !errIsEINVAL(err) {
			t.Errorf("SetMaxReaders(%d): %v, want EINVAL", n, err)
		}
	}
	for _, n := range []int{limit, 100000} {
		if err := env.SetMaxReaders(n); err != nil {
			t.Errorf("SetMaxReaders(%d): %v", n, err)
		}
	}
	if err := env.Open(t.TempDir(), testFlags(0), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.SetMaxReaders(1); !errIsEINVAL(err) {
		t.Errorf("SetMaxReaders after Open: %v, want EINVAL", err)
	}
	if n, err := env.MaxReaders(); err != nil || n != 100000 {
		t.Errorf("MaxReaders: %d, %v (want 100000)", n, err)
	}
	err = env.View(func(txn *Txn) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
}

// TestLockFileBeyondTheReaderLimit: the limit applies to the table a new
// lock file is made with. An existing lock file with more slots (as C makes
// it for a larger count) is attached to with its own table, as in C
// (mdb_env_setup_locks takes the count from the file's size), in both
// lock-file versions, and is not resized.
func TestLockFileBeyondTheReaderLimit(t *testing.T) {
	requireLockFile(t)
	for _, f := range []Format{V09, V10} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			dir := t.TempDir()
			lock := filepath.Join(dir, "lock.mdb")
			open := func() *Env {
				env, err := NewEnv()
				if err != nil {
					t.Fatal(err)
				}
				if err := env.SetFormat(f); err != nil {
					t.Fatal(err)
				}
				if err := env.Open(dir, 0, 0o644); err != nil {
					env.Close()
					t.Fatal(err)
				}
				return env
			}
			env := open()
			n, err := env.MaxReaders()
			if err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(lock)
			if err != nil {
				t.Fatal(err)
			}
			env.Close()
			header := st.Size() - int64(n)*64
			// A sparse file of 2^20 + 10 slots (64 MiB and a bit).
			const slots = limit + 10
			if err := os.Truncate(lock, header+slots*64); err != nil {
				t.Fatal(err)
			}
			env = open()
			if n, err := env.MaxReaders(); err != nil || n != slots {
				t.Errorf("MaxReaders: %d, %v (want %d)", n, err, slots)
			}
			txn, err := env.BeginTxn(nil, Readonly)
			if err != nil {
				t.Fatal(err)
			}
			var readers int
			err = env.ReaderList(func(line string) error {
				if !strings.HasPrefix(strings.TrimSpace(line), "pid") {
					readers++
				}
				return nil
			})
			if err != nil || readers != 1 {
				t.Errorf("ReaderList: %d readers, %v (want 1)", readers, err)
			}
			txn.Abort()
			env.Close()
			if st, err := os.Stat(lock); err != nil || st.Size() != header+slots*64 {
				t.Errorf("the lock file was resized: %v, %v", st.Size(), err)
			}
		})
	}
}
