package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// openPoolTest opens an environment on an in-memory lock region of
// maxReaders slots (the lock file's stand-in) and returns that region.
func openPoolTest(t *testing.T, maxReaders uint32) (*Env, *MemLock) {
	t.Helper()
	return openPoolTestMap(t, maxReaders, 0)
}

func openPoolTestMap(t *testing.T, maxReaders uint32, mapSize int64) (*Env, *MemLock) {
	t.Helper()
	var region *MemLock
	open := func(_ string, _ os.FileMode, n uint32, _ bool, _ format.Format) (LockRegion, error) {
		region = NewMemLock(n)
		return region, nil
	}
	env, _ := openTest(t, format.V09, Options{MaxReaders: maxReaders, OpenLock: open, MapSize: mapSize})
	return env, region
}

// putKV commits key = val.
func putKV(t *testing.T, env *Env, key, val string) {
	t.Helper()
	w := beginW(t, env)
	if err := w.Put(MainDBI, []byte(key), []byte(val), 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}

// readerSlots counts the slots of env's reader table in a transaction and
// the idle ones.
func readerSlots(env *Env) (active, idle int) {
	for _, r := range env.lock.ReaderList() {
		if r.InTxn {
			active++
		} else {
			idle++
		}
	}
	return active, idle
}

func wantSlots(t *testing.T, env *Env, active, idle int) {
	t.Helper()
	if a, i := readerSlots(env); a != active || i != idle {
		t.Fatalf("reader table: %d active, %d idle slots; want %d and %d (%+v)",
			a, i, active, idle, env.lock.ReaderList())
	}
}

// A read transaction that ends leaves its slot idle and owned; the next one
// takes it without claiming a new one. The idle slot pins no snapshot.
func TestSlotPoolReuse(t *testing.T) {
	env, _ := openPoolTest(t, 0)
	putKV(t, env, "k", "v1")
	r := beginR(t, env)
	s := r.slot
	if s < 0 || env.lock.SlotTxnID(s) != r.ID() {
		t.Fatalf("slot %d txnid %d, txn %d", s, env.lock.SlotTxnID(s), r.ID())
	}
	wantSlots(t, env, 1, 0)
	r.Abort()
	wantSlots(t, env, 0, 1)
	if !env.lock.SlotIsIdleAndOurs(s) {
		t.Fatal("ended transaction's slot is not idle and ours")
	}
	if got := env.lock.FindOldest(100); got != 99 {
		t.Fatalf("idle slot pins a snapshot: FindOldest(100) = %d", got)
	}
	putKV(t, env, "k", "v2")
	// The same Txn object (as package lmdb reuses them) and a new one.
	if err := env.BeginRead(r); err != nil {
		t.Fatal(err)
	}
	if r.slot != s || r.ID() != env.lock.TxnID() {
		t.Fatalf("reused: slot %d (want %d), txnid %d (want %d)", r.slot, s, r.ID(), env.lock.TxnID())
	}
	if v, err := r.Get(MainDBI, []byte("k")); err != nil || string(v) != "v2" {
		t.Fatalf("Get: %q, %v", v, err)
	}
	r.Abort()
	r2 := beginR(t, env)
	if r2.slot != s {
		t.Fatalf("new Txn: slot %d, want the pooled %d", r2.slot, s)
	}
	r2.Abort()
	if n := env.lock.NumReaders(); n != 1 {
		t.Fatalf("numreaders %d, want 1", n)
	}
}

// Reset keeps the slot (in a transaction or not, it stays the
// transaction's), Renew reuses it, Abort pools it.
func TestSlotPoolResetRenew(t *testing.T) {
	env, _ := openPoolTest(t, 0)
	r := beginR(t, env)
	s := r.slot
	r.Reset()
	wantSlots(t, env, 0, 1)
	// The reset transaction's slot is not in the pool.
	r2 := beginR(t, env)
	if r2.slot == s {
		t.Fatal("a reset transaction's slot was handed out")
	}
	if err := r.Renew(); err != nil || r.slot != s {
		t.Fatalf("Renew: %v, slot %d", err, r.slot)
	}
	r.Abort()
	r2.Abort()
	wantSlots(t, env, 0, 2)
}

// The pool keeps at most MaxReaders/4 slots; the others are released.
func TestSlotPoolBound(t *testing.T) {
	env, _ := openPoolTest(t, 16)
	if env.slots.n != 4 || env.slots.nearFullAt != 12 {
		t.Fatalf("pool size %d, near-full mark %d", env.slots.n, env.slots.nearFullAt)
	}
	var txns []*Txn
	for i := 0; i < 10; i++ {
		txns = append(txns, beginR(t, env))
	}
	wantSlots(t, env, 10, 0)
	for _, r := range txns {
		r.Abort()
	}
	wantSlots(t, env, 0, 4)
	if n := env.lock.NumReaders(); n != 10 {
		t.Fatalf("numreaders %d, want the high-water mark 10", n)
	}
	// They are reused first.
	txns = txns[:0]
	for i := 0; i < 4; i++ {
		txns = append(txns, beginR(t, env))
	}
	wantSlots(t, env, 4, 0)
	for _, r := range txns {
		r.Abort()
	}
}

// A slot claimed in the top quarter of the table turns pooling off until
// a claim lands below it again.
func TestSlotPoolNearlyFull(t *testing.T) {
	env, _ := openPoolTest(t, 16)
	var txns []*Txn
	for i := 0; i < 13; i++ { // slots 0..12; 12 is the near-full mark
		txns = append(txns, beginR(t, env))
	}
	for _, r := range txns {
		r.Abort()
	}
	wantSlots(t, env, 0, 0)
	r := beginR(t, env) // slot 0: below the mark
	r.Abort()
	wantSlots(t, env, 0, 1)
}

// MDB_READERS_FULL as in C when every slot is in a transaction; a slot
// that ends is available at once.
func TestSlotPoolReadersFull(t *testing.T) {
	env, _ := openPoolTest(t, 4)
	var txns []*Txn
	for i := 0; i < 4; i++ {
		txns = append(txns, beginR(t, env))
	}
	if _, err := env.BeginTxn(nil, format.ReadOnly); err != format.ReadersFull {
		t.Fatalf("fifth reader: %v", err)
	}
	// Pooling is off after the full table: the slot goes back to it.
	txns[2].Abort()
	wantSlots(t, env, 3, 0)
	// Claimed again (slot 2, below the mark of 3): pooling is on again.
	r := beginR(t, env)
	r.Abort()
	wantSlots(t, env, 3, 1)
	for _, r := range txns {
		r.Abort()
	}
	// The pool holds one slot (4/4).
	wantSlots(t, env, 0, 1)
}

// A pooled slot that is no longer ours (cleared and claimed by another
// process) is dropped, not used and not released.
func TestSlotPoolStaleSlot(t *testing.T) {
	env, region := openPoolTest(t, 0)
	r := beginR(t, env)
	s := r.slot
	r.Abort()
	region.slot(s).pid.Store(region.pid + 1) // someone else's now
	r = beginR(t, env)
	if r.slot == s {
		t.Fatal("got a slot that is not ours")
	}
	r.Abort()
	if pid := region.slot(s).pid.Load(); pid != region.pid+1 {
		t.Fatalf("foreign slot touched: pid %d", pid)
	}
	region.slot(s).pid.Store(0)
}

// Close releases the pooled slots (mr_pid = 0).
func TestSlotPoolClose(t *testing.T) {
	env, region := openPoolTest(t, 0)
	a, b := beginR(t, env), beginR(t, env)
	a.Abort()
	b.Abort()
	wantSlots(t, env, 0, 2)
	env.Close()
	if list := region.ReaderList(); len(list) != 0 {
		t.Fatalf("slots left after Close: %+v", list)
	}
	// A transaction ending after Close leaves the region alone.
	if err := env.BeginRead(a); err == nil {
		t.Fatal("BeginRead after Close")
	}
}

// NoLock: the in-memory table pools the same way; the environment still
// reports no readers, as C without a lock file.
func TestSlotPoolNoLock(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{Flags: format.NoLock})
	if env.HasLockFile() || env.slots.n != int(format.DefaultReaders/4) || env.slots.nearFullAt != 0 {
		t.Fatalf("lock file %v, pool size %d, near-full mark %d", env.HasLockFile(), env.slots.n, env.slots.nearFullAt)
	}
	r := beginR(t, env)
	s := r.slot
	r.Abort()
	r = beginR(t, env)
	if r.slot != s {
		t.Fatalf("slot %d, want the pooled %d", r.slot, s)
	}
	r.Abort()
	wantSlots(t, env, 0, 1)
	if in, err := env.Info(); err != nil || in.NumReaders != 0 || env.ReaderList() != nil {
		t.Fatal("NoLock reports readers")
	}
}

// Many goroutines begin and end read transactions while a writer commits:
// no slot is in two transactions at once, every snapshot is consistent
// (the writer never reused a page a reader could see), and a goroutine's
// snapshots never go back. The small table makes claims, the near-full
// rule and the bound race with the pool.
func TestSlotPoolConcurrent(t *testing.T) {
	for _, maxReaders := range []uint32{126, 20} {
		t.Run(fmt.Sprint(maxReaders), func(t *testing.T) {
			testSlotPoolConcurrent(t, maxReaders)
		})
	}
}

func testSlotPoolConcurrent(t *testing.T, maxReaders uint32) {
	const readers = 16
	// A reader that the scheduler leaves alone for a while pins its
	// snapshot while the writer commits freely: room for that.
	env, _ := openPoolTestMap(t, maxReaders, 1<<30)
	iters := 20000
	if testing.Short() || raceEnabled {
		iters = 2000
	}
	// Two keys the writer always sets together: a counter and a value
	// derived from it, rewritten with every commit (so freed pages get
	// reused as soon as no reader pins them).
	value := func(n uint64, key byte) []byte {
		v := make([]byte, 300)
		binary.BigEndian.PutUint64(v, n)
		for i := 8; i < len(v); i++ {
			v[i] = byte(n) ^ byte(i) ^ key
		}
		return v
	}
	commit := func(n uint64) error {
		w, err := env.BeginTxn(nil, 0)
		if err != nil {
			return err
		}
		for _, k := range []byte{'a', 'b'} {
			if err := w.Put(MainDBI, []byte{k}, value(n, k), 0); err != nil {
				w.Abort()
				return err
			}
		}
		return w.Commit()
	}
	if err := commit(0); err != nil {
		t.Fatal(err)
	}
	var inUse [256]atomic.Bool
	var stop atomic.Bool
	var full atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, readers+1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := uint64(1); !stop.Load(); n++ {
			if err := commit(n); err != nil {
				errs <- fmt.Errorf("writer: %w (oldest snapshot %d of %d, readers %+v)",
					err, env.lock.FindOldest(n), n-1, env.lock.ReaderList())
				return
			}
		}
	}()
	var rw sync.WaitGroup
	for g := 0; g < readers; g++ {
		rw.Add(1)
		go func() {
			defer rw.Done()
			txn := new(Txn)
			var last uint64
			for i := 0; i < iters; i++ {
				if err := env.BeginRead(txn); err != nil {
					if errors.Is(err, format.ReadersFull) && int(maxReaders) < readers+env.slots.n {
						full.Add(1) // transient: idle slots in flight
						continue
					}
					errs <- err
					return
				}
				if !inUse[txn.slot].CompareAndSwap(false, true) {
					errs <- fmt.Errorf("slot %d in two transactions", txn.slot)
					return
				}
				if id := txn.ID(); id < last {
					errs <- fmt.Errorf("snapshot went back: %d after %d", id, last)
					return
				} else {
					last = id
				}
				a, err1 := txn.Get(MainDBI, []byte{'a'})
				b, err2 := txn.Get(MainDBI, []byte{'b'})
				if err1 != nil || err2 != nil {
					errs <- fmt.Errorf("get: %v, %v", err1, err2)
					return
				}
				n := binary.BigEndian.Uint64(a)
				if !bytes.Equal(a, value(n, 'a')) || !bytes.Equal(b, value(n, 'b')) {
					errs <- fmt.Errorf("inconsistent snapshot %d: a=%d b=%d", txn.ID(), n, binary.BigEndian.Uint64(b))
					return
				}
				inUse[txn.slot].Store(false)
				txn.Abort()
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
	active, idle := readerSlots(env)
	if active != 0 || idle > env.slots.n {
		t.Fatalf("after the run: %d active, %d idle slots (pool size %d)", active, idle, env.slots.n)
	}
	// Pages were reused (each commit writes at least 3 new ones).
	in, err := env.Info()
	if err != nil || in.LastPgno >= 3*in.LastTxnID {
		t.Fatalf("no page reuse? last page %d after %d commits (%v)", in.LastPgno, in.LastTxnID, err)
	}
	t.Logf("%d commits, last page %d, %d transient MDB_READERS_FULL", in.LastTxnID, in.LastPgno, full.Load())
	mustAudit(t, env)
}
