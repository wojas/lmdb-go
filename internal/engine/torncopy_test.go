package engine

// Copies of a read transaction's meta page that fail validation (rlmdb
// 3811bea, core/txn.rs tests): retried if the published txnid moved, a
// bounded number of times; an error if it did not. In both reader paths:
// the lock file (here the in-memory stand-in) and NoLock.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// tornEnv is an environment with a lock file (the stand-in) or without
// one, after one commit (txnid 1).
func tornEnv(t *testing.T, noLock bool) (*Env, string) {
	var flags uint32
	if noLock {
		flags = format.NoLock
	}
	env, dir := openTest(t, format.V09, Options{Flags: flags})
	putKV(t, env, "n", "1")
	return env, dir
}

// withSnapshotMeta runs f with hook copying the meta pages of snapshots.
func withSnapshotMeta(hook func(e *Env, m *sys.DataMap, i int) (format.Meta, error), f func()) {
	testHookSnapshotMeta = hook
	defer func() { testHookSnapshotMeta = nil }()
	f()
}

// noLiveReaders: no slot of env's reader table pins a snapshot.
func noLiveReaders(t *testing.T, env *Env) {
	t.Helper()
	for _, r := range env.lock.ReaderList() {
		if r.InTxn {
			t.Fatalf("a reader slot still pins txnid %d", r.TxnID)
		}
	}
}

// A copy that fails validation while a commit lands (the published
// txnid moves) is torn, not bad: begin copies again and reads the new
// snapshot.
func TestTornMetaCopyIsRetriedWhenTheTxnIDMoved(t *testing.T) {
	for _, noLock := range []bool{false, true} {
		env, _ := tornEnv(t, noLock)
		copies := 0
		var r *Txn
		withSnapshotMeta(func(e *Env, m *sys.DataMap, i int) (format.Meta, error) {
			copies++
			if copies == 1 {
				// A commit lands during the copy, which comes out torn.
				putKV(t, e, "n", "2")
				return format.Meta{}, format.Corrupted
			}
			return e.readMetaIn(m, i)
		}, func() { r = beginR(t, env) })
		defer r.Abort()
		v, err := r.Get(MainDBI, []byte("n"))
		if copies != 2 || r.ID() != 2 || err != nil || string(v) != "2" {
			t.Fatalf("noLock %v: %d copies, txnid %d, value %q, %v", noLock, copies, r.ID(), v, err)
		}
	}
}

// A copy that fails validation while the published txnid stays is of a
// bad page: begin fails after that one copy, with the validation error,
// and gives its reader slot back. The same with a real bad page.
func TestBadMetaCopyFailsWhenTheTxnIDDidNotMove(t *testing.T) {
	for _, noLock := range []bool{false, true} {
		env, dir := tornEnv(t, noLock)
		copies := 0
		var err error
		withSnapshotMeta(func(*Env, *sys.DataMap, int) (format.Meta, error) {
			copies++
			return format.Meta{}, format.Corrupted
		}, func() { _, err = env.BeginTxn(nil, format.ReadOnly) })
		if err != format.Corrupted || copies != 1 {
			t.Fatalf("noLock %v: %v after %d copies, want MDB_CORRUPTED after 1", noLock, err, copies)
		}
		noLiveReaders(t, env)

		// The magic of meta page 1 (the page of txnid 1; mm_magic starts
		// MDB_meta), overwritten.
		f, err := os.OpenFile(filepath.Join(dir, "data.mdb"), os.O_WRONLY, 0)
		must(t, err)
		_, err = f.WriteAt(make([]byte, 4), int64(env.PageSize()+env.layout.MetaOffset()))
		f.Close()
		must(t, err)
		if _, err := env.BeginTxn(nil, format.ReadOnly); err != format.Corrupted {
			t.Fatalf("noLock %v: a bad meta page: %v, want MDB_CORRUPTED", noLock, err)
		}
		noLiveReaders(t, env)
	}
}

// Torn copies are retried a bounded number of times: if every copy fails
// while a commit lands, begin gives up after maxTornCopies copies with
// the validation error, and the next begin reads the last commit.
func TestTornMetaCopiesAreRetriedABoundedNumberOfTimes(t *testing.T) {
	for _, noLock := range []bool{false, true} {
		env, _ := tornEnv(t, noLock)
		copies := 0
		var err error
		withSnapshotMeta(func(e *Env, _ *sys.DataMap, _ int) (format.Meta, error) {
			copies++
			putKV(t, e, "n", fmt.Sprint(copies+1))
			return format.Meta{}, format.Corrupted
		}, func() { _, err = env.BeginTxn(nil, format.ReadOnly) })
		if err != format.Corrupted || copies != maxTornCopies {
			t.Fatalf("noLock %v: %v after %d copies, want MDB_CORRUPTED after %d", noLock, err, copies, maxTornCopies)
		}
		if id := snapshotID(t, env); id != maxTornCopies+1 {
			t.Fatalf("noLock %v: the next begin reads txnid %d, want %d", noLock, id, maxTornCopies+1)
		}
	}
}
