// Package sys is the operating-system layer of the engine: the read-only
// memory map of the data file, positional I/O, syncing and platform
// queries. It is the Go form of rlmdb's os/unix.rs and os/mod.rs without
// the lock file (the shared mutexes and the lock-file map live in
// internal/lockfile). C counterparts: mdb_env_map, MDB_FDATASYNC,
// GET_PAGESIZE, the pwrite calls of mdb_page_flush and mdb_env_write_meta.
//
// The package needs no unsafe: syscall.Mmap returns the mapping as a
// []byte, and everything else is ordinary system calls.
package sys

import (
	"os"
	"sync/atomic"
	"syscall"
)

// DataMap is a read-only, MAP_SHARED memory map of the data file
// (me_map).
//
// The map may be longer than the file (the map size is the maximum
// database size); touching a page beyond the end of the file would raise
// SIGBUS, so Slice also checks against the file size, re-reading it (one
// fstat) only when an access goes past the last size seen.
//
// Why handing out slices of this map is sound: other processes write the
// file while we read it. LMDB's MVCC invariant, which C relies on just the
// same, is that a page reachable from a snapshot is never modified while a
// reader slot pins that snapshot: writers only reuse pages freed by
// transactions older than every active reader (mdb_find_oldest). Slices
// handed out by Slice are only for such pages (the callers bound every page
// number by their snapshot), and they are used while the read transaction
// that pins the snapshot is alive. The two meta pages are rewritten under
// readers; they are only read with ReadMeta, into a private copy, and Slice
// refuses them. Damage that breaks MVCC itself (a free list naming a live
// page, a forged lock file) makes a writer rewrite a page under a reader,
// as in C; every access is still bounds-checked against the slice.
//
// A DataMap is reference counted: the environment holds one reference
// while the map is current, every transaction one while it runs, and the
// mapping is removed when the last reference goes. That keeps slices of
// live transactions valid when Env.SetMapSize installs a new map (rlmdb
// keeps old maps alive with an Arc for the same reason).
type DataMap struct {
	data    []byte
	metaEnd int
	fd      int
	// knownSize is the last known size of the data file.
	knownSize atomic.Int64
	refs      atomic.Int64
}

// MapReadOnly maps size bytes of f read-only and shared. random applies
// madvise(MADV_RANDOM) where available (MDB_NORDAHEAD). metaEnd is the end
// of the meta pages, which Slice refuses. f must stay open while the map
// is in use. The map starts with one reference, the caller's.
func MapReadOnly(f *os.File, size int, random bool, metaEnd int) (*DataMap, error) {
	if size <= 0 {
		return nil, syscall.EINVAL
	}
	fd := int(f.Fd())
	data, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	if random {
		adviseRandom(data)
	}
	m := &DataMap{data: data, metaEnd: metaEnd, fd: fd}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = syscall.Munmap(data)
		return nil, err
	}
	m.knownSize.Store(st.Size)
	m.refs.Store(1)
	return m, nil
}

// Len is the length of the mapping (me_mapsize).
func (m *DataMap) Len() int { return len(m.data) }

// Slice returns n bytes at off, or ok false if the range is outside the
// map, overlaps the meta pages or is beyond the end of the data file.
// Callers ask only for pages of a snapshot they hold pinned.
func (m *DataMap) Slice(off, n int) ([]byte, bool) {
	end := off + n
	if off < m.metaEnd || n < 0 || end < off || end > len(m.data) {
		return nil, false
	}
	if int64(end) > m.knownSize.Load() && !m.refreshSize(end) {
		return nil, false
	}
	return m.data[off:end:end], true
}

func (m *DataMap) refreshSize(needed int) bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(m.fd, &st); err != nil {
		return false
	}
	for {
		old := m.knownSize.Load()
		if st.Size <= old || m.knownSize.CompareAndSwap(old, st.Size) {
			break
		}
	}
	return st.Size >= int64(needed)
}

// ReadMeta copies len(out) bytes at off, for data other processes may be
// writing concurrently (the meta pages). ok is false if the range is not
// mapped or beyond the file.
func (m *DataMap) ReadMeta(off int, out []byte) bool {
	end := off + len(out)
	if off < 0 || end > len(m.data) {
		return false
	}
	if int64(end) > m.knownSize.Load() && !m.refreshSize(end) {
		return false
	}
	copy(out, m.data[off:end])
	return true
}

// TryAcquire takes a reference if the map is still referenced (it has not
// been unmapped). Transactions take theirs with Env's current map; a false
// result means the map was replaced and released meanwhile.
func (m *DataMap) TryAcquire() bool {
	for {
		r := m.refs.Load()
		if r <= 0 {
			return false
		}
		if m.refs.CompareAndSwap(r, r+1) {
			return true
		}
	}
}

// Release drops a reference; the last one removes the mapping.
func (m *DataMap) Release() {
	if m.refs.Add(-1) == 0 {
		_ = syscall.Munmap(m.data)
		m.data = nil
	}
}

// OSPageSize is the OS memory page size (4096 on x86-64 Linux, 16384 on
// Apple Silicon macOS).
func OSPageSize() int { return os.Getpagesize() }
