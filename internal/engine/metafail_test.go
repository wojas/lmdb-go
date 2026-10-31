package engine

// A meta page write that fails after its bytes reached the file, and
// plain copies of a file holding such a meta page.

import (
	"bytes"
	"fmt"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// putBig commits a value of n bytes under k.
func putBig(t *testing.T, env *Env, k string, n int) error {
	t.Helper()
	w := beginW(t, env)
	if err := w.Put(MainDBI, []byte(k), bytes.Repeat([]byte("B"), n), 0); err != nil {
		w.Abort()
		t.Fatal(err)
	}
	return w.Commit()
}

// copyAndRead copies env (plain copy) into a new directory and returns
// the value of k in the copy, and the copy's audit.
func copyAndRead(t *testing.T, env *Env, k string) ([]byte, error, error) {
	t.Helper()
	dir := t.TempDir()
	if err := env.Copy(dir, 0); err != nil {
		return nil, err, nil
	}
	cp, err := Open(dir, Options{Flags: format.NoLock | format.ReadOnly})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer cp.Close()
	r := beginR(t, cp)
	defer r.Abort()
	v, err := r.Get(MainDBI, []byte(k))
	return bytes.Clone(v), err, cp.Audit()
}

// The meta page of a commit was written, but its write or the sync after
// it failed (macOS syncs after the O_DSYNC write: F_FULLFSYNC; Linux's
// O_DSYNC write itself can fail after the page cache took the bytes): the
// environment is unusable (MDB_FATAL_ERROR: transactions and copies fail
// with MDB_PANIC) and the old txnid is written back, as C does when the
// write fails (review finding, round 2, P2-4: after a failed sync nothing
// of that happened, so the file held meta N+1 while N was published, and a
// plain copy pinned N but copied meta N+1, naming pages it left out).
func TestMetaWriteFailureIsFatal(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, flags := range []uint32{0, format.NoLock} {
			t.Run(fmt.Sprintf("%v/flags=%#x", f, flags), func(t *testing.T) {
				env, dir := openTest(t, f, Options{Flags: flags})
				putKV(t, env, "k", "old")
				testHookMetaWritten = func(e *Env) error {
					if e == env {
						return syscall.EIO
					}
					return nil
				}
				err := putBig(t, env, "k", 100000)
				testHookMetaWritten = nil
				if err != syscall.EIO {
					t.Fatalf("commit: %v, want EIO", err)
				}
				if err := env.BeginRead(new(Txn)); err != format.Panic {
					t.Fatalf("read transaction: %v, want MDB_PANIC", err)
				}
				if _, err := env.BeginTxn(nil, 0); err != format.Panic {
					t.Fatalf("write transaction: %v, want MDB_PANIC", err)
				}
				if _, err, _ := copyAndRead(t, env, "k"); err != format.Panic {
					t.Fatalf("copy: %v, want MDB_PANIC", err)
				}
				// The failed commit's meta page has the old txnid again.
				if info, err := env.Info(); err != nil || info.LastTxnID != 1 {
					t.Fatalf("newest meta page: txnid %d, %v; want 1", info.LastTxnID, err)
				}
				env.Close()
				env2, err := Open(dir, Options{Flags: flags | format.NoSync, OpenLock: OpenMemLock})
				if err != nil {
					t.Fatal(err)
				}
				defer env2.Close()
				if v, err, aerr := copyAndRead(t, env2, "k"); err != nil || string(v) != "old" || aerr != nil {
					t.Fatalf("copy after reopen: %q, %v, audit %v", v, err, aerr)
				}
			})
		}
	}
}

// A plain copy writes no meta page newer than the snapshot it copies: one
// that names pages past the snapshot's end gets the snapshot's meta
// instead. Such a meta page is on the file after a commit whose meta page
// write or sync failed in another process (C and rlmdb leave it there,
// see TestMetaWriteFailureIsFatal; here it is forged by publishing the
// older txnid again). Before, the copy named pages it did not contain
// (MDB_PAGE_NOTFOUND reading it). C's mdb_copy copies both meta pages as
// they are.
func TestCopyMetasNotNewerThanSnapshot(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			env, _ := openTest(t, f, Options{})
			putKV(t, env, "k", "old")
			if err := putBig(t, env, "k", 100000); err != nil {
				t.Fatal(err)
			}
			env.lock.SetTxnID(1)
			env.writer.hasLastMeta = false
			v, err, aerr := copyAndRead(t, env, "k")
			if err != nil || string(v) != "old" || aerr != nil {
				t.Fatalf("copy: %q, %v, audit %v", v, err, aerr)
			}
		})
	}
}
