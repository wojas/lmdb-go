// An in-memory LockRegion: the reader table of an environment without a
// lock file (MDB_NOLOCK, or a read-only environment on a read-only file
// system), and an in-process stand-in for the lock file in tests.
//
// AGENTS.md rule 2a: NoLock removes only the cross-process locking. Within
// the process, write transactions are still serialised (Env.writerMu) and
// readers are tracked in this table, which the writer consults through
// FindOldest exactly as it consults the lock file's table, so pages a live
// reader can still see are never reused. C's MDB_NOLOCK leaves both to the
// caller; rlmdb tracks no readers there (its find_oldest returns txnid-1
// without a lock region), which the Go port does not follow. Nothing on
// disk changes. It keeps the reader-table protocol of rlmdb core/reader.rs
// in memory and does not interoperate with other processes.

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

// MemLock is an in-memory LockRegion.
type MemLock struct {
	mu     sync.Mutex // the reader mutex
	writer chan struct{}
	// slots are the reader slots; grown (copy on write, under mu) when
	// max is 0.
	slots      atomic.Pointer[[]*memSlot]
	max        int
	exclusive  atomic.Bool
	txnid      atomic.Uint64
	numReaders atomic.Uint32
	pid        int32
}

// NewMemLock is a fresh in-memory region with maxReaders slots (0: as
// many as needed, for MDB_NOLOCK, which has no reader limit in C), as if
// this process were the exclusive opener.
func NewMemLock(maxReaders uint32) *MemLock {
	m := &MemLock{writer: make(chan struct{}, 1), max: int(maxReaders), pid: int32(os.Getpid())}
	n := max(int(maxReaders), 8)
	s := make([]*memSlot, n)
	for i := range s {
		s[i] = new(memSlot)
	}
	m.slots.Store(&s)
	m.exclusive.Store(true)
	return m
}

// OpenMemLock is an OpenLockFunc that returns a fresh bounded MemLock: an
// in-process stand-in for the lock file in tests.
func OpenMemLock(_ string, _ os.FileMode, maxReaders uint32, _ bool, _ format.Format) (LockRegion, error) {
	return NewMemLock(maxReaders), nil
}

func (m *MemLock) slot(i int) *memSlot { return (*m.slots.Load())[i] }

// IsExclusive implements LockRegion.
func (m *MemLock) IsExclusive() bool { return m.exclusive.Load() }

// Downgrade implements LockRegion.
func (m *MemLock) Downgrade(txnid uint64) error {
	m.txnid.Store(txnid)
	m.exclusive.Store(false)
	return nil
}

// MaxReaders implements LockRegion.
func (m *MemLock) MaxReaders() uint32 { return uint32(m.max) }

// LockWriter implements LockRegion (in-process only).
func (m *MemLock) LockWriter(func() (uint64, error)) error {
	m.writer <- struct{}{}
	return nil
}

// UnlockWriter implements LockRegion. The in-process lock belongs to no
// thread, so it cannot fail.
func (m *MemLock) UnlockWriter() error {
	<-m.writer
	return nil
}

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
	slots := *m.slots.Load()
	nr := int(m.numReaders.Load())
	i := nr
	for j := 0; j < nr; j++ {
		if slots[j].pid.Load() == 0 {
			i = j
			break
		}
	}
	if m.max > 0 && i >= m.max {
		return 0, format.ReadersFull
	}
	if i >= len(slots) {
		grown := make([]*memSlot, 2*len(slots))
		copy(grown, slots)
		for j := len(slots); j < len(grown); j++ {
			grown[j] = new(memSlot)
		}
		m.slots.Store(&grown)
		slots = grown
	}
	s := slots[i]
	s.pid.Store(0)
	s.txnid.Store(^uint64(0))
	if i == nr {
		m.numReaders.Store(uint32(nr + 1))
	}
	s.pid.Store(m.pid)
	return i, nil
}

// SlotIsIdleAndOurs implements LockRegion.
func (m *MemLock) SlotIsIdleAndOurs(i int) bool {
	s := m.slot(i)
	return s.pid.Load() == m.pid && s.txnid.Load() == ^uint64(0)
}

// PublishCurrentTxnID implements LockRegion.
func (m *MemLock) PublishCurrentTxnID(i int) uint64 {
	s := m.slot(i)
	for {
		t := m.txnid.Load()
		s.txnid.Store(t)
		if m.txnid.Load() == t {
			return t
		}
	}
}

// SlotTxnID implements LockRegion.
func (m *MemLock) SlotTxnID(i int) uint64 { return m.slot(i).txnid.Load() }

// SetSlotTxnID implements LockRegion.
func (m *MemLock) SetSlotTxnID(i int, t uint64) { m.slot(i).txnid.Store(t) }

// ResetSlot implements LockRegion.
func (m *MemLock) ResetSlot(i int) { m.slot(i).txnid.Store(^uint64(0)) }

// ReleaseSlot implements LockRegion.
func (m *MemLock) ReleaseSlot(i int) {
	s := m.slot(i)
	s.txnid.Store(^uint64(0))
	s.pid.Store(0)
}

// FindOldest implements LockRegion (mdb_find_oldest).
func (m *MemLock) FindOldest(txnid uint64) uint64 {
	oldest := txnid - 1
	slots := *m.slots.Load()
	for i := min(int(m.numReaders.Load()), len(slots)) - 1; i >= 0; i-- {
		if slots[i].pid.Load() != 0 {
			oldest = min(oldest, slots[i].txnid.Load())
		}
	}
	return oldest
}

// ReaderList implements LockRegion.
func (m *MemLock) ReaderList() []format.ReaderInfo {
	var out []format.ReaderInfo
	slots := *m.slots.Load()
	for i := 0; i < min(int(m.numReaders.Load()), len(slots)); i++ {
		if pid := slots[i].pid.Load(); pid != 0 {
			t := slots[i].txnid.Load()
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
