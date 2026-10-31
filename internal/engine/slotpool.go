// Reader slots kept for reuse between read transactions: the per-Env pool
// of idle slots this process owns. Not in mdb.c or rlmdb as such; it
// gives lmdb-go's MDB_NOTLS transactions what C's default (TLS) mode has,
// a slot that outlives the transaction (mdb_txn_renew0's
// pthread_getspecific branch, mdb.c line 2758), without binding it to a
// thread.
//
// With MDB_NOTLS, C claims a slot under the reader mutex at every
// mdb_txn_begin and gives it up (mr_pid = 0) at every end. lmdb-go
// begins a new transaction for every Env.View, so that was two calls into
// libc per read transaction (the reader mutex: a cgo call each way, on
// macOS a semaphore system call) and the contention of every reader of
// the process on one mutex. Here a read transaction that ends leaves its
// slot owned and idle, exactly as C leaves a thread's slot in TLS mode
// (mr_pid = our pid, mr_txnid = (txnid_t)-1), and puts it in the pool;
// the next read transaction takes it from there without the mutex and
// without cgo.
//
// Why that is sound:
//
//   - An idle slot pins no snapshot: mdb_find_oldest (C, rlmdb, this
//     port) takes the minimum of mr_txnid over slots with mr_pid != 0, and
//     an idle slot's (txnid_t)-1 is the largest value there is. A slot is
//     made idle (ResetSlot) before it enters the pool, so no pooled slot
//     ever holds a txnid.
//   - Reusing a slot publishes the snapshot exactly as a new one does
//     (takeSnapshot: mti_txnid stored in the slot and re-read until both
//     agree, ITS#7970; then the meta page read and mti_txnid checked
//     again), the same sequence C runs for a TLS slot or a renewed
//     transaction.
//   - A slot is handed to one transaction at a time: a cell of the pool is
//     emptied with a compare-and-swap, so one taker wins; the transaction
//     owns the slot until it puts it back (or releases it).
//   - Before reuse the slot is checked to be still ours and idle, C's
//     MDB_BAD_RSLOT check for a TLS slot. Other processes leave it alone
//     while this one lives: mdb_reader_check clears only slots whose
//     process holds no lock on its pid's byte of the lock file, and this
//     process holds that lock from its first slot until Env.Close. A slot
//     that fails the check (cleared by something else) is dropped from the
//     pool, not released, since it may be someone else's now.
//   - Env.Close releases every pooled slot (mr_pid = 0) before closing the
//     lock file, which in turn clears any slot of this process left over
//     (mdb_env_close0).
//
// The pool is bounded, so that idle slots of this process never take more
// than a quarter of the reader table from other processes (C processes in
// TLS mode keep one idle slot per thread the same way): at most
// min(MaxReaders/4, 64) slots. 64 covers the begin/end churn of programs
// with many cores: the pool only has to hold the slots that are idle at
// one moment, not one per concurrent reader. A transaction that finds the
// pool empty claims a new slot under the reader mutex, as before; one that
// ends with the pool full releases its slot, as before. And when the table
// gets nearly full, idle slots go back to the table instead: a new slot
// claimed at index MaxReaders*3/4 or above (mdb_txn_renew0 takes the first
// free one, so at least that many are in use), or MDB_READERS_FULL, turns
// pooling off, so that ending transactions release their slots, until a
// claim lands below that mark again. Without a lock file
// (MDB_NOLOCK) the in-memory table has no limit and only the size bound
// applies.
//
// What other processes see: an idle slot of this process in
// mdb_reader_list / mdb_stat -r (txnid "-"), as for a C process in TLS
// mode, and mti_numreaders (Env.Info's NumReaders), the high-water mark of
// the table, counts the pooled slots as the ones in use, as C's does. A
// reused slot keeps the thread id (mr_tid) of the thread that claimed it;
// reading the current one is a cgo call, and the value is informational
// only (mdb_reader_list prints it).

package engine

import (
	"sync/atomic"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// maxPooledSlots bounds the pool whatever MaxReaders is.
const maxPooledSlots = 64

// slotPool holds idle reader slots this process owns. Its zero value is
// an empty pool of size 0 (no pooling).
type slotPool struct {
	// cells hold a slot index + 1, or 0 for an empty cell. cells[n:] are
	// unused.
	cells [maxPooledSlots]atomic.Int32
	n     int
	// nearFullAt is the slot index from which a new claim means the table
	// is nearly full (0: never, for a table without a limit).
	nearFullAt int
	// nearFull is set while the table is nearly full: put refuses slots,
	// so that ending transactions release them.
	nearFull atomic.Bool
}

// init sizes the pool for a reader table of maxReaders slots, of which
// limit is the hard limit (0: none, for the in-memory table of NoLock).
func (p *slotPool) init(maxReaders, limit uint32) {
	p.n = int(min(maxReaders/4, maxPooledSlots))
	if limit > 0 {
		p.nearFullAt = int(limit - limit/4)
	}
}

// get takes an idle slot out of the pool, looking first at cell hint (the
// one the caller used last, so that goroutines working with their own
// transaction objects keep to their own cells). It returns the slot (-1
// if the pool is empty) and the cell it came from.
func (p *slotPool) get(hint int) (slot, cell int) {
	n := p.n
	if hint >= n {
		hint = 0
	}
	for k := 0; k < n; k++ {
		c := hint + k
		if c >= n {
			c -= n
		}
		cl := &p.cells[c]
		if v := cl.Load(); v != 0 && cl.CompareAndSwap(v, 0) {
			return int(v - 1), c
		}
	}
	return -1, hint
}

// put adds the idle slot to the pool, at cell hint if that is empty. It
// returns false if the slot must be released instead: the pool is full,
// or the reader table is nearly full.
func (p *slotPool) put(slot, hint int) (cell int, ok bool) {
	if p.nearFull.Load() {
		return hint, false
	}
	n := p.n
	if hint >= n {
		hint = 0
	}
	for k := 0; k < n; k++ {
		c := hint + k
		if c >= n {
			c -= n
		}
		cl := &p.cells[c]
		if cl.Load() == 0 && cl.CompareAndSwap(0, int32(slot+1)) {
			return c, true
		}
	}
	return hint, false
}

// claimed notes that slot was claimed from the table (mdb_txn_renew0 takes
// the first free slot, so every slot below it was in use).
func (p *slotPool) claimed(slot int) {
	if p.nearFullAt == 0 {
		return
	}
	if full := slot >= p.nearFullAt; full != p.nearFull.Load() {
		p.nearFull.Store(full)
	}
}

// drain empties the pool, calling release for every slot it held.
func (p *slotPool) drain(release func(slot int)) {
	for c := range p.cells[:p.n] {
		if v := p.cells[c].Swap(0); v != 0 {
			release(int(v - 1))
		}
	}
}

// acquireSlot returns a reader slot for a new read transaction: an idle
// slot of the pool if there is one, else a new one claimed under the
// reader mutex (LockRegion.AcquireSlot). hint is the pool cell the caller
// used last; the cell used is returned for the next time.
func (e *Env) acquireSlot(hint int) (slot, cell int, err error) {
	for {
		if slot, cell = e.slots.get(hint); slot < 0 {
			break
		}
		if e.lock.SlotIsIdleAndOurs(slot) {
			return slot, cell, nil
		}
		// Not ours any more (C: MDB_BAD_RSLOT): forget it.
	}
	slot, err = e.lock.AcquireSlot()
	if err == nil {
		e.slots.claimed(slot)
		return slot, cell, nil
	}
	if err == format.ReadersFull {
		// A transaction may have ended meanwhile and pooled its slot.
		e.slots.nearFull.Store(e.slots.nearFullAt > 0)
		for {
			if slot, cell = e.slots.get(hint); slot < 0 {
				break
			}
			if e.lock.SlotIsIdleAndOurs(slot) {
				return slot, cell, nil
			}
		}
	}
	return -1, cell, err
}

// releaseSlot ends a read transaction's use of slot: the slot becomes idle
// (it pins no snapshot from here on) and goes to the pool, or back to the
// table if the pool does not take it. It returns the pool cell for the
// next acquireSlot.
func (e *Env) releaseSlot(slot, hint int) int {
	e.lock.ResetSlot(slot)
	cell, ok := e.slots.put(slot, hint)
	if !ok {
		e.lock.ReleaseSlot(slot)
	}
	return cell
}
