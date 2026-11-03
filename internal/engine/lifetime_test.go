package engine

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// Read transactions begin (reading the meta pages) while SetMapSize
// replaces and releases the map, over and over: every meta read goes
// through a map the reader holds a reference on (review finding: they
// read the current map without one, racing with its unmap; -race reported
// it, and a released map's empty slice made the read panic). Run with
// -race.
func TestMetaReadsDuringSetMapSize(t *testing.T) {
	for _, nolock := range []bool{false, true} {
		t.Run(fmt.Sprintf("nolock=%v", nolock), func(t *testing.T) {
			opts := Options{}
			if nolock {
				opts.Flags = format.NoLock
			}
			env, _ := openTest(t, format.V09, opts)
			putKV(t, env, "k", "v")
			iters := 20000
			if testing.Short() || raceEnabled {
				iters = 2000
			}
			var wg sync.WaitGroup
			errs := make(chan error, 4)
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					txn := new(Txn)
					for i := 0; i < iters; i++ {
						if err := env.BeginRead(txn); err != nil {
							errs <- err
							return
						}
						v, err := txn.Get(MainDBI, []byte("k"))
						ok := err == nil && string(v) == "v" // before Abort: v is in the map
						txn.Abort()
						if !ok {
							errs <- fmt.Errorf("Get: %v", err)
							return
						}
					}
				}()
			}
			for i := 0; i < iters/4; i++ {
				if err := env.SetMapSize(64 << 20); err != nil {
					t.Fatal(err)
				}
				if _, err := env.Info(); err != nil {
					t.Fatal(err)
				}
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
		})
	}
}

// guardRegion is a LockRegion that records every use after its Close.
type guardRegion struct {
	LockRegion
	mu     sync.Mutex
	closed int
	late   []string
}

func (g *guardRegion) use(what string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed > 0 {
		g.late = append(g.late, what)
	}
}

func (g *guardRegion) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed > 0
}

func (g *guardRegion) AcquireSlot() (int, error) {
	g.use("AcquireSlot")
	return g.LockRegion.AcquireSlot()
}
func (g *guardRegion) SlotIsIdleAndOurs(i int) bool {
	g.use("SlotIsIdleAndOurs")
	return g.LockRegion.SlotIsIdleAndOurs(i)
}
func (g *guardRegion) PublishCurrentTxnID(i int) uint64 {
	g.use("PublishCurrentTxnID")
	return g.LockRegion.PublishCurrentTxnID(i)
}
func (g *guardRegion) SetSlotTxnID(i int, txnid uint64) {
	g.use("SetSlotTxnID")
	g.LockRegion.SetSlotTxnID(i, txnid)
}
func (g *guardRegion) ResetSlot(i int)   { g.use("ResetSlot"); g.LockRegion.ResetSlot(i) }
func (g *guardRegion) ReleaseSlot(i int) { g.use("ReleaseSlot"); g.LockRegion.ReleaseSlot(i) }
func (g *guardRegion) TxnID() uint64     { g.use("TxnID"); return g.LockRegion.TxnID() }
func (g *guardRegion) FindOldest(txnid uint64) uint64 {
	g.use("FindOldest")
	return g.LockRegion.FindOldest(txnid)
}
func (g *guardRegion) LockWriter(f func() (uint64, error)) error {
	g.use("LockWriter")
	return g.LockRegion.LockWriter(f)
}
func (g *guardRegion) UnlockWriter() error { g.use("UnlockWriter"); return g.LockRegion.UnlockWriter() }
func (g *guardRegion) ReaderList() []format.ReaderInfo {
	g.use("ReaderList")
	return g.LockRegion.ReaderList()
}
func (g *guardRegion) Close() error {
	g.mu.Lock()
	g.closed++
	g.mu.Unlock()
	return g.LockRegion.Close()
}

// openGuarded opens an environment on a guardRegion over the in-memory
// lock-file stand-in.
func openGuarded(t *testing.T) (*Env, string, *guardRegion) {
	t.Helper()
	g := new(guardRegion)
	open := func(_ string, _ os.FileMode, n uint32, _ bool, _ format.Format, _ LockClaim) (LockRegion, error) {
		g.LockRegion = NewMemLock(n)
		return g, nil
	}
	env, dir := openTest(t, format.V09, Options{OpenLock: open})
	return env, dir, g
}

// Transactions that outlive Close keep the environment alive (review
// finding P1-4: Close cleared their reader slots and closed the lock file
// while they still read their snapshots, so writers of this process
// after a reopen, or of another process, reused their pages; finding
// P1-5: a transaction ending after Close used the torn-down lock region).
// The lock region, files and registry entry stay until the last of them
// ends, nothing uses the region after its Close, and its Close comes
// exactly once, when the last transaction ends.
func TestCloseWaitsForTransactions(t *testing.T) {
	env, dir, g := openGuarded(t)
	putKV(t, env, "k", "v1")
	active := beginR(t, env)
	reset := beginR(t, env)
	reset.Reset()
	pooled := beginR(t, env)
	pooled.Abort() // an idle slot in the pool
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w := beginW(t, env)
	if err := w.Put(MainDBI, []byte("w"), []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	slot := active.slot

	env.Close()
	if g.isClosed() {
		t.Fatal("the lock region was closed under live transactions")
	}
	if !g.LockRegion.SlotIsIdleAndOurs(reset.slot) || g.LockRegion.SlotTxnID(slot) != active.ID() {
		t.Fatal("Close dropped the slots of live transactions")
	}
	if _, err := Open(dir, Options{OpenLock: OpenMemLock}); err != syscall.EBUSY {
		t.Fatalf("reopen while transactions live: %v, want EBUSY", err)
	}
	// New transactions and Renew fail; the live ones work.
	if err := env.BeginRead(new(Txn)); err != syscall.EINVAL {
		t.Fatalf("BeginRead after Close: %v", err)
	}
	if err := reset.Renew(); err != syscall.EINVAL {
		t.Fatalf("Renew after Close: %v", err)
	}
	if v, err := active.Get(MainDBI, []byte("k")); err != nil || string(v) != "v1" {
		t.Fatalf("Get after Close: %q, %v", v, err)
	}
	w.Abort()
	active.Reset()
	active.Abort()
	if g.isClosed() {
		t.Fatal("the lock region was closed while a reset transaction lives")
	}
	reset.Abort()
	if !g.isClosed() {
		t.Fatal("the lock region was not closed after the last transaction ended")
	}
	if g.closed != 1 || len(g.late) != 0 {
		t.Fatalf("lock region closed %d times; used after Close: %v", g.closed, g.late)
	}
	if list := g.LockRegion.ReaderList(); len(list) != 0 {
		t.Fatalf("slots left: %+v", list)
	}
	env2, err := Open(dir, Options{OpenLock: OpenMemLock})
	if err != nil {
		t.Fatalf("reopen after the last transaction: %v", err)
	}
	env2.Close()
}

// Close with no transaction left tears the environment down at once.
func TestCloseTearsDownAtOnce(t *testing.T) {
	env, dir, g := openGuarded(t)
	r := beginR(t, env)
	r.Abort()
	env.Close()
	if !g.isClosed() || len(g.late) != 0 {
		t.Fatalf("closed %v, used after Close: %v", g.isClosed(), g.late)
	}
	env2, err := Open(dir, Options{OpenLock: OpenMemLock})
	if err != nil {
		t.Fatal(err)
	}
	env2.Close()
}

// NoLock: a snapshot that outlives Close keeps its pages (review finding
// P1-4: Close left the registry, so the path could be opened again with a
// reader table of its own, whose writers reused the snapshot's pages).
// The closed handle keeps its claim of the file until the snapshot ends,
// so a new NoLock handle of the file shares its reader table, and the new
// handle's writers keep the snapshot's pages.
func TestNoLockCloseKeepsSnapshot(t *testing.T) {
	env, dir := openTest(t, format.V09, Options{Flags: format.NoLock})
	big := bytes.Repeat([]byte("A"), 5000)
	w := beginW(t, env)
	if err := w.Put(MainDBI, []byte("k"), big, 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	r := beginR(t, env)
	v, err := r.Get(MainDBI, []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	env.Close()
	if _, err := Open(dir, Options{OpenLock: OpenMemLock}); err != syscall.EBUSY {
		t.Fatalf("reopen with a lock file under a live snapshot: %v, want EBUSY", err)
	}
	env2, err := Open(dir, Options{Flags: format.NoLock | format.NoSync, MapSize: 64 << 20})
	if err != nil {
		t.Fatalf("NoLock reopen under a live snapshot: %v", err)
	}
	defer env2.Close()
	if env2.mem != env.mem {
		t.Fatal("the new handle does not share the closed one's reader table")
	}
	for i := 0; i < 5; i++ {
		putKV(t, env2, "k", fmt.Sprint(i))
	}
	if !bytes.Equal(v, big) {
		t.Fatal("snapshot changed")
	}
	r.Abort()
	for i := 0; i < 5; i++ {
		putKV(t, env2, "k", fmt.Sprint(i))
	}
}

// Reset, Renew, Abort and begins race with Close (review finding P1-5:
// -race reported Close unmapping the lock table under ResetSlot). Run with
// -race.
func TestCloseRacesWithTransactions(t *testing.T) {
	rounds := 50
	if testing.Short() || raceEnabled {
		rounds = 10
	}
	for i := 0; i < rounds; i++ {
		env, _, g := openGuarded(t)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for j := 0; j < 32; j++ {
			r := beginR(t, env)
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				<-start
				switch j % 4 {
				case 0:
					r.Reset()
					r.Abort()
				case 1:
					r.Reset()
					_ = r.Renew()
					r.Abort()
				case 2:
					_ = r.Commit()
				default:
					r.Abort()
					if err := env.BeginRead(r); err == nil {
						r.Abort()
					}
				}
			}(j)
		}
		close(start)
		env.Close()
		wg.Wait()
		if !g.isClosed() || len(g.late) != 0 {
			t.Fatalf("closed %v, used after Close: %v", g.isClosed(), g.late)
		}
	}
}
