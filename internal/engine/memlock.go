// An in-process LockRegion, for tests of the engine without the lock file
// (rlmdb's tests use the real lock file; the Go engine is tested against
// this until internal/lockfile is wired in). It keeps the reader-table
// protocol of core/reader.rs in memory, so it does not interoperate with
// other processes: use it only for environments no other process opens.

package engine

import (
	"os"
	"sync"
	"sync/atomic"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

type memSlot struct {
	pid   atomic.Int32
	txnid atomic.Uint64
}

// MemLock is an in-process LockRegion.
type MemLock struct {
	mu         sync.Mutex // the reader mutex
	writer     chan struct{}
	exclusive  atomic.Bool
	txnid      atomic.Uint64
	numReaders atomic.Uint32
	slots      []memSlot
	pid        int32
}

// NewMemLock is a fresh in-process lock region with maxReaders slots, as
// if this process were the exclusive opener.
func NewMemLock(maxReaders uint32) *MemLock {
	m := &MemLock{
		writer: make(chan struct{}, 1),
		slots:  make([]memSlot, maxReaders),
		pid:    int32(os.Getpid()),
	}
	m.exclusive.Store(true)
	return m
}

// OpenMemLock is an OpenLockFunc that returns a fresh MemLock.
func OpenMemLock(_ string, _ os.FileMode, maxReaders uint32, _ bool, _ format.Format) (LockRegion, error) {
	return NewMemLock(maxReaders), nil
}

// IsExclusive implements LockRegion.
func (m *MemLock) IsExclusive() bool { return m.exclusive.Load() }

// Downgrade implements LockRegion.
func (m *MemLock) Downgrade(txnid uint64) error {
	m.txnid.Store(txnid)
	m.exclusive.Store(false)
	return nil
}

// MaxReaders implements LockRegion.
func (m *MemLock) MaxReaders() uint32 { return uint32(len(m.slots)) }

// LockWriter implements LockRegion.
func (m *MemLock) LockWriter(func() (uint64, error)) error {
	m.writer <- struct{}{}
	return nil
}

// UnlockWriter implements LockRegion.
func (m *MemLock) UnlockWriter() { <-m.writer }

// TxnID implements LockRegion.
func (m *MemLock) TxnID() uint64 { return m.txnid.Load() }

// SetTxnID implements LockRegion.
func (m *MemLock) SetTxnID(t uint64) { m.txnid.Store(t) }

// NumReaders implements LockRegion.
func (m *MemLock) NumReaders() uint32 { return m.numReaders.Load() }

// AcquireSlot implements LockRegion.
func (m *MemLock) AcquireSlot() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nr := int(m.numReaders.Load())
	i := nr
	for j := 0; j < nr; j++ {
		if m.slots[j].pid.Load() == 0 {
			i = j
			break
		}
	}
	if i >= len(m.slots) {
		return 0, format.ReadersFull
	}
	m.slots[i].pid.Store(0)
	m.slots[i].txnid.Store(^uint64(0))
	if i == nr {
		m.numReaders.Store(uint32(nr + 1))
	}
	m.slots[i].pid.Store(m.pid)
	return i, nil
}

// SlotIsIdleAndOurs implements LockRegion.
func (m *MemLock) SlotIsIdleAndOurs(i int) bool {
	return m.slots[i].pid.Load() == m.pid && m.slots[i].txnid.Load() == ^uint64(0)
}

// PublishCurrentTxnID implements LockRegion.
func (m *MemLock) PublishCurrentTxnID(i int) uint64 {
	for {
		t := m.txnid.Load()
		m.slots[i].txnid.Store(t)
		if m.txnid.Load() == t {
			return t
		}
	}
}

// SlotTxnID implements LockRegion.
func (m *MemLock) SlotTxnID(i int) uint64 { return m.slots[i].txnid.Load() }

// SetSlotTxnID implements LockRegion.
func (m *MemLock) SetSlotTxnID(i int, t uint64) { m.slots[i].txnid.Store(t) }

// ResetSlot implements LockRegion.
func (m *MemLock) ResetSlot(i int) { m.slots[i].txnid.Store(^uint64(0)) }

// ReleaseSlot implements LockRegion.
func (m *MemLock) ReleaseSlot(i int) {
	m.slots[i].txnid.Store(^uint64(0))
	m.slots[i].pid.Store(0)
}

// FindOldest implements LockRegion.
func (m *MemLock) FindOldest(txnid uint64) uint64 {
	oldest := txnid - 1
	for i := int(m.numReaders.Load()) - 1; i >= 0; i-- {
		if m.slots[i].pid.Load() != 0 {
			oldest = min(oldest, m.slots[i].txnid.Load())
		}
	}
	return oldest
}

// ReaderList implements LockRegion.
func (m *MemLock) ReaderList() []format.ReaderInfo {
	var out []format.ReaderInfo
	for i := 0; i < int(m.numReaders.Load()); i++ {
		if pid := m.slots[i].pid.Load(); pid != 0 {
			t := m.slots[i].txnid.Load()
			out = append(out, format.ReaderInfo{PID: pid, TxnID: t, InTxn: t != ^uint64(0)})
		}
	}
	return out
}

// ReaderCheck implements LockRegion: an in-process table has no dead
// readers.
func (m *MemLock) ReaderCheck() (int, error) { return 0, nil }

// Close implements LockRegion.
func (m *MemLock) Close() error { return nil }
