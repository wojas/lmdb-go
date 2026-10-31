// A Go environment opened with NoLock next to C processes that use the
// lock file: Go readers still see C's commits (C's MDB_NOLOCK readers take
// the newest meta page; coordinating the writers is the caller's job).

package lockfiletest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PowerDNS/lmdb-go/lmdb"
)

// TestNoLockSeesCCommits: commits of a C process (between Go's
// transactions, as MDB_NOLOCK requires of the caller) are seen by the next
// Go read transaction, by a plain copy and by the next Go write
// transaction, which builds on them (rlmdb's
// no_lock_handle_sees_commits_of_another_process).
func TestNoLockSeesCCommits(t *testing.T) {
	for _, c := range sides(t) {
		env, _ := c.newEnv(t)
		genv, err := lmdb.NewEnv()
		if err != nil {
			t.Fatal(err)
		}
		if err := genv.Open(env, lmdb.NoLock, 0o644); err != nil {
			t.Fatalf("%s: Open: %v", c.format, err)
		}
		for round := 0; round < 3; round++ {
			mustRun(t, c.peer, "fill", env, "-", "20", "1")
			want := c.lastTxnID(t, env)
			var key, val []byte
			for i := 0; i < 3; i++ { // pooled slots too
				err := genv.View(func(txn *lmdb.Txn) error {
					if id := uint64(txn.ID()); id != want {
						t.Errorf("%s: Go NoLock reader at txnid %d, C committed %d", c.format, id, want)
					}
					cur, err := txn.OpenCursor(lmdb.DBI(1))
					if err != nil {
						return err
					}
					defer cur.Close()
					key, val, err = cur.Get(nil, nil, lmdb.First)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			mustRun(t, c.peer, "fill", env, "-", "20", "1")
			want = c.lastTxnID(t, env)
			dst := filepath.Join(t.TempDir(), "copy")
			if err := os.Mkdir(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := genv.Copy(dst); err != nil {
				t.Fatal(err)
			}
			if got := c.lastTxnID(t, dst); got != want {
				t.Fatalf("%s: plain copy at txnid %d, C committed %d", c.format, got, want)
			}
			mustRun(t, c.peer, "fill", env, "-", "20", "1")
			want = c.lastTxnID(t, env)
			// Rewrite one item as it is: the commit must build on C's.
			err := genv.Update(func(txn *lmdb.Txn) error { return txn.Put(lmdb.DBI(1), key, val, 0) })
			if err != nil {
				t.Fatal(err)
			}
			if got := c.lastTxnID(t, env); got != want+1 {
				t.Fatalf("%s: Go commit at txnid %d after C's %d", c.format, got, want)
			}
			mustRun(t, c.peer, "verify", env, "-", "20", "1")
		}
		genv.Close()
	}
}
