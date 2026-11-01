// The lock-file mapping: rlmdb os/unix.rs and os/windows.rs LockMap; the
// mmap (MapViewOfFile on Windows) of mdb_env_setup_locks and the munmap of
// mdb_env_close0 in mdb.c. How the mapping is made and removed is in
// lockmap_unix.go and lockmap_windows.go.

package lockfile

import (
	"fmt"
	"sync/atomic"
	"unsafe"
)

// lockMap is a read/write shared mapping of the lock file (me_txns).
//
// Other processes read and write the same memory concurrently, so every
// access to a shared field is an atomic operation on a naturally aligned
// word: that is how C's volatile fields behave on the CPUs LMDB supports,
// and it is well defined for memory shared between processes. Offsets come
// from the layouts and from slot indexes checked against the mapping's
// size; an offset outside the map or misaligned is a programming error and
// panics, as in rlmdb. Nothing read from the file is used as an offset
// without such a check.
type lockMap struct {
	mem []byte
}

func (m *lockMap) len() int { return len(m.mem) }

// unmap removes the mapping; every pointer into it is invalid afterwards.
func (m *lockMap) unmap() error {
	if m.mem == nil {
		return nil
	}
	err := unmapMem(m.mem)
	m.mem = nil
	return err
}

// at returns a pointer to size bytes at off, checking bounds and that off
// is a multiple of align.
func (m *lockMap) at(off, size, align int) unsafe.Pointer {
	if off < 0 || size < 0 || off > len(m.mem)-size || off%align != 0 {
		panic(fmt.Sprintf("lockfile: lock file access out of bounds or misaligned: %d+%d of %d", off, size, len(m.mem)))
	}
	// unsafe: &m.mem[off] is inside the live mapping (checked above). The
	// mapping does not move, and it starts on a page boundary (on Windows
	// on the 64 KiB allocation granularity), so the address has off's
	// alignment.
	return unsafe.Pointer(&m.mem[off])
}

func (m *lockMap) loadUint32(off int) uint32 { return atomic.LoadUint32((*uint32)(m.at(off, 4, 4))) }

func (m *lockMap) storeUint32(off int, v uint32) {
	atomic.StoreUint32((*uint32)(m.at(off, 4, 4)), v)
}

func (m *lockMap) loadInt32(off int) int32 { return atomic.LoadInt32((*int32)(m.at(off, 4, 4))) }

func (m *lockMap) storeInt32(off int, v int32) { atomic.StoreInt32((*int32)(m.at(off, 4, 4)), v) }

func (m *lockMap) loadUint64(off int) uint64 { return atomic.LoadUint64((*uint64)(m.at(off, 8, 8))) }

func (m *lockMap) storeUint64(off int, v uint64) {
	atomic.StoreUint64((*uint64)(m.at(off, 8, 8)), v)
}

// readBytes copies n bytes at off: a snapshot of fields that the
// initialising process writes once (semaphore names) or that are only read
// for diagnostics.
func (m *lockMap) readBytes(off, n int) []byte {
	m.at(off, n, 1)
	return append([]byte(nil), m.mem[off:off+n]...)
}

// writeBytes copies b to off. Only for the process that initialises the
// table while it holds the exclusive file lock, so nobody else reads it.
func (m *lockMap) writeBytes(off int, b []byte) {
	m.at(off, len(b), 1)
	copy(m.mem[off:], b)
}
