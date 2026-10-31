// A Go environment opened with NoLock next to C processes that use the
// lock file: Go readers still see C's commits (C's MDB_NOLOCK readers take
// the newest meta page; coordinating the writers is the caller's job).

package lockfiletest

import (
	"testing"

	"github.com/PowerDNS/lmdb-go/lmdb"
)

// TestNoLockSeesCCommits: commits of a C process (between Go's
// transactions, as MDB_NOLOCK requires of the caller) are seen by the next
// Go read transaction, whose snapshot is the newest meta page.
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
			mustRun(t, c.peer, "fill", env, "-", "20", "5")
			want := c.lastTxnID(t, env)
			for i := 0; i < 3; i++ { // pooled slots too
				err := genv.View(func(txn *lmdb.Txn) error {
					if id := uint64(txn.ID()); id != want {
						t.Errorf("%s: Go NoLock reader at txnid %d, C committed %d", c.format, id, want)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		genv.Close()
	}
}
