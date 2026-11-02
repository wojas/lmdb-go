package engine

// The writer locks and what happens around them: the end of a write
// transaction, the next writer's reuse of the environment's write Txn.

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// nextWriter is a write transaction that begins (began), waits for
// proceed, then puts c=3 and commits; a panic is returned as an error.
func nextWriter(env *Env, began, proceed chan struct{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	w, err := env.BeginTxn(nil, 0)
	if err != nil {
		return err
	}
	close(began)
	<-proceed
	if err := w.Put(MainDBI, []byte("c"), []byte("3"), 0); err != nil {
		w.Abort()
		return err
	}
	return w.Commit()
}

// The next write transaction begins as soon as the last one released the
// writer locks, in the same Txn (the environment reuses it, as C reuses
// me_txn0): the ending one must not touch it any more (review finding,
// round 2, P1-1: it released its map after unlocking, which dropped the
// next writer's map reference and left it without a map, so that it
// panicked, and leaked its own reference, so that the environment was
// never torn down). Deterministic: the next writer begins inside the
// first one's end, after the unlock.
func TestWriterEndLeavesNextWriterAlone(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, flags := range []uint32{0, format.NoLock} {
			t.Run(fmt.Sprintf("%v/flags=%#x", f, flags), func(t *testing.T) {
				env, dir := openTest(t, f, Options{Flags: flags})
				putKV(t, env, "a", "1")
				began, proceed := make(chan struct{}), make(chan struct{})
				done := make(chan error, 1)
				var once sync.Once
				testHookWriterUnlocked = func(e *Env) {
					if e != env {
						return
					}
					once.Do(func() {
						go func() { done <- nextWriter(env, began, proceed) }()
						<-began
					})
				}
				defer func() { testHookWriterUnlocked = nil }()
				putKV(t, env, "b", "2")
				close(proceed)
				if err := <-done; err != nil {
					t.Fatalf("next writer: %v", err)
				}
				r := beginR(t, env)
				for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}, {"c", "3"}} {
					if v, err := r.Get(MainDBI, []byte(kv[0])); err != nil || string(v) != kv[1] {
						t.Fatalf("Get %s: %q, %v", kv[0], v, err)
					}
				}
				r.Abort()
				mustAudit(t, env)
				// Every map reference is gone with the transactions: Close
				// tears the environment down (a NoLock open would succeed
				// anyway, sharing the handle's region), and the path opens
				// again.
				env.Close()
				if n := env.liveMaps.Load(); n != 0 {
					t.Fatalf("%d maps left after Close", n)
				}
				env2, err := Open(dir, Options{Flags: flags | format.NoSync, OpenLock: OpenMemLock})
				if err != nil {
					t.Fatalf("reopen: %v", err)
				}
				env2.Close()
			})
		}
	}
}

// deadOwnerRegion is the in-memory lock-file stand-in with a writer lock
// whose owner dies while a goroutine waits for it (Linux robust mutexes:
// EOWNERDEAD): once armed, the next LockWriter reports that it waits,
// blocks until release, and then runs the repair as lockfile.Region does
// (mdb_mutex_failed): a failed repair leaves the lock unrecoverable.
type deadOwnerRegion struct {
	*MemLock
	waiting, release chan struct{}
	repairTxnID      uint64
	repairErr        error
	repaired         bool
}

func (r *deadOwnerRegion) LockWriter(repair func() (uint64, error)) error {
	if r.release != nil {
		close(r.waiting)
		<-r.release
		r.release = nil
		r.repaired = true
		r.repairTxnID, r.repairErr = repair()
		if r.repairErr != nil {
			return r.repairErr
		}
		r.SetTxnID(r.repairTxnID)
	}
	return r.MemLock.LockWriter(repair)
}

// A writer, or a plain copy, waits for the writer lock; the environment is
// closed; the lock's owner dies. The waiting goroutine must repair the
// lock (with the newest txnid, read through the map it pinned) before it
// fails with EINVAL for the closed environment, or the lock is lost for
// every process (review finding, round 2, P1-3: the repair read the meta
// pages through the current map, which Close had cleared, failed with
// EINVAL, and left the mutex ENOTRECOVERABLE). The multi-process version
// with real robust mutexes is lmdb's TestCloseDuringOwnerDeathRecovery
// (Linux).
func TestOwnerDeathRepairAfterClose(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, copying := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/copy=%v", f, copying), func(t *testing.T) {
				r := &deadOwnerRegion{}
				open := func(_ string, _ os.FileMode, n uint32, _ bool, _ format.Format, _ LockClaim) (LockRegion, error) {
					r.MemLock = NewMemLock(n)
					return r, nil
				}
				env, dir := openTest(t, f, Options{OpenLock: open})
				putKV(t, env, "a", "1")
				putKV(t, env, "b", "2")
				newest := env.lock.TxnID()
				r.waiting, r.release = make(chan struct{}), make(chan struct{})
				done := make(chan error, 1)
				go func() {
					if copying {
						done <- env.CopyTo(io.Discard, 0)
						return
					}
					w, err := env.BeginTxn(nil, 0)
					if err == nil {
						w.Abort()
					}
					done <- err
				}()
				<-r.waiting
				env.Close()
				close(r.release)
				if err := <-done; err != syscall.EINVAL {
					t.Fatalf("after Close: %v, want EINVAL", err)
				}
				if !r.repaired || r.repairErr != nil || r.repairTxnID != newest {
					t.Fatalf("repair ran %v: txnid %d, %v; want %d", r.repaired, r.repairTxnID, r.repairErr, newest)
				}
				select {
				case r.MemLock.writer <- struct{}{}:
					<-r.MemLock.writer
				default:
					t.Fatal("the writer lock is still held")
				}
				env2, err := Open(dir, Options{Flags: format.NoSync, OpenLock: OpenMemLock})
				if err != nil {
					t.Fatalf("reopen: %v", err)
				}
				env2.Close()
			})
		}
	}
}

// NoLock handles of one file share one writer lock (registry.go): a
// writer of one handle waits for another handle's writer, and its handle
// is closed meanwhile. Once it has the lock it fails with EINVAL and
// leaves the lock free, its handle is torn down, and the other handle's
// writers go on.
func TestSharedWriterLockAcrossClose(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			a, dir := openTest(t, f, Options{Flags: format.NoLock})
			b, err := Open(dir, Options{Flags: format.NoLock | format.NoSync, MapSize: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			putKV(t, a, "k", "1")
			wb := beginW(t, b)
			done := make(chan error, 1)
			go func() {
				w, err := a.BeginTxn(nil, 0)
				if err == nil {
					w.Abort()
				}
				done <- err
			}()
			// a's writer holds a's writerMu: it waits for the shared lock.
			for a.writerMu.TryLock() {
				a.writerMu.Unlock()
				runtime.Gosched()
			}
			a.Close()
			if err := wb.Put(MainDBI, []byte("k"), []byte("2"), 0); err != nil {
				t.Fatal(err)
			}
			if err := wb.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != syscall.EINVAL {
				t.Fatalf("writer of the closed handle: %v, want EINVAL", err)
			}
			if n := a.liveMaps.Load(); n != 0 {
				t.Fatalf("%d maps of the closed handle left", n)
			}
			putKV(t, b, "k", "3")
			r := beginR(t, b)
			defer r.Abort()
			if v, err := r.Get(MainDBI, []byte("k")); err != nil || string(v) != "3" {
				t.Fatalf("Get: %q, %v", v, err)
			}
		})
	}
}
