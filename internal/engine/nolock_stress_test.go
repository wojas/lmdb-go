package engine

// Without a lock file, a read transaction reads the snapshot whose txnid
// it pins, also when it begins while commits replace the meta page it
// copies (rlmdb tests/rust_env.rs, from its commit ade31f0, with the
// 1 GiB map of 9155a5d: no_lock_readers_pin_the_snapshot_they_read).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// TestNoLockReadersPinTheSnapshotTheyRead: one writer stores each
// transaction's txnid as the value of "counter", and readers on other
// goroutines check it against the transaction's ID. Readers that pinned a
// newer snapshot than they read would let the writer reuse their pages, so
// some readers also hold their value across many commits and check that
// it does not change. Both formats, with readers in the writer's handle
// and in another handle of the file. The map is 1 GiB, reserved, not
// written: a reader descheduled under load while the writer runs its
// commits must not let them fill it (rlmdb 9155a5d).
func TestNoLockReadersPinTheSnapshotTheyRead(t *testing.T) {
	commits := uint64(4000)
	if testing.Short() || raceEnabled {
		commits = 1000
	}
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, twoHandles := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/two-handles=%v", f, twoHandles), func(t *testing.T) {
				noLockSnapshotMatchesItsID(t, f, twoHandles, commits)
			})
		}
	}
}

// counterValue is the value of "counter" in transaction id: the id, then
// a filler that changes with it.
func counterValue(id uint64) []byte {
	v := bytes.Repeat([]byte{byte(id)}, 64)
	binary.LittleEndian.PutUint64(v, id)
	return v
}

func noLockSnapshotMatchesItsID(t *testing.T, f format.Format, twoHandles bool, commits uint64) {
	const heldCommits = 20
	writer, dir := openTest(t, f, Options{Flags: format.NoLock, MapSize: 1 << 30})
	readerEnv := writer
	if twoHandles {
		other, err := Open(dir, Options{Format: f, Flags: format.NoLock | format.NoSync, MapSize: 1 << 30})
		must(t, err)
		defer other.Close()
		readerEnv = other
	}
	// check reports a value v that is not the one of the snapshot of txn.
	check := func(txn *Txn, v []byte, what string) error {
		id := binary.LittleEndian.Uint64(v)
		if txn.ID() != id || !bytes.Equal(v, counterValue(id)) {
			return fmt.Errorf("%s: snapshot %d, value of %d", what, txn.ID(), id)
		}
		return nil
	}
	var committed atomic.Uint64
	var done atomic.Bool
	errs := make(chan error, 6)
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				txn, err := readerEnv.BeginTxn(nil, format.ReadOnly)
				if err != nil {
					errs <- err
					return
				}
				err = func() error {
					defer txn.Abort()
					held, err := txn.Get(MainDBI, []byte("counter"))
					if err == format.NotFound {
						if txn.ID() != 0 {
							return fmt.Errorf("no counter in snapshot %d", txn.ID())
						}
						return nil
					}
					if err != nil {
						return err
					}
					if err := check(txn, held, "snapshot and value differ"); err != nil {
						return err
					}
					if g%2 == 0 {
						return nil
					}
					// Hold the value while the writer commits.
					cp := bytes.Clone(held)
					start := committed.Load()
					for committed.Load() < start+heldCommits && !done.Load() {
						runtime.Gosched()
					}
					if !bytes.Equal(held, cp) {
						return fmt.Errorf("a held value changed")
					}
					return check(txn, held, "a held value changed")
				}()
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := uint64(0); i < commits && len(errs) == 0; i++ {
		w, err := writer.BeginTxn(nil, 0)
		if err == nil {
			id := w.ID()
			if err = w.Put(MainDBI, []byte("counter"), counterValue(id), 0); err != nil {
				w.Abort()
			} else if err = w.Commit(); err == nil {
				committed.Store(id)
			}
		}
		if err != nil {
			done.Store(true)
			wg.Wait()
			t.Fatalf("commit %d: %v", i+1, err)
		}
	}
	done.Store(true)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if id := snapshotID(t, readerEnv); id != commits {
		t.Fatalf("the last snapshot is %d, want %d", id, commits)
	}
}
