// The lock-file mapping: rlmdb os/unix.rs LockMap; the mmap of
// mdb_env_setup_locks and the munmap of mdb_env_close0 in mdb.c.

package lockfile

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// lockMap is a read/write MAP_SHARED mapping of the lock file (me_txns).
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

// mapLockFile maps the first size bytes of the open file fd read/write and
// shared.
func mapLockFile(fd int, size int) (*lockMap, error) {
	if size <= 0 {
		return nil, syscall.EINVAL
	}
	mem, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return &lockMap{mem: mem}, nil
}

func (m *lockMap) len() int { return len(m.mem) }

// unmap removes the mapping; every pointer into it is invalid afterwards.
func (m *lockMap) unmap() error {
	if m.mem == nil {
		return nil
	}
	err := syscall.Munmap(m.mem)
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
	// mapping does not move, and mmap returns page-aligned memory, so the
	// address has off's alignment.
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
