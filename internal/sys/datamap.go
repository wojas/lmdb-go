// Package sys is the operating-system layer of the engine: the read-only
// memory map of the data file, positional I/O, syncing and platform
// queries. It is the Go form of rlmdb's os/unix.rs, os/windows.rs and
// os/mod.rs without the lock file (the shared mutexes and the lock-file
// map live in internal/lockfile). C counterparts: mdb_env_map,
// MDB_FDATASYNC, GET_PAGESIZE, mdb_fopen, the pwrite calls of
// mdb_page_flush and mdb_env_write_meta.
//
// Everything the engine needs is behind the same functions on every
// platform; the files ending in _unix.go and _windows.go make the system
// calls. On Unix the package needs no unsafe: syscall.Mmap returns the
// mapping as a []byte. On Windows the map is made with the Win32 and NT
// calls C uses there (datamap_windows.go, whose comment lists every
// Windows decision), and the view becomes a []byte with unsafe.Slice.
package sys

import (
	"runtime"
	"sync/atomic"
)

// DataMap is a read-only, shared memory map of the data file (me_map).
//
// The map may be longer than the file (the map size is the maximum
// database size); touching a page beyond the end of the file would raise
// SIGBUS (an access violation on Windows), so Slice also checks against
// the file size, re-reading it only when an access goes past the last size
// seen.
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
//
// On Unix the map is always as long as the map size asked for. On Windows
// a map made through a read-only handle covers the file as it is (C maps
// "whatever exists" for MDB_RDONLY), so Len may be smaller; the engine maps
// the file again when a snapshot needs more (Env.renewShortMap).
type DataMap struct {
	data    []byte
	metaEnd int
	// knownSize is the last known size of the data file (on Windows, with
	// a reserved view, also the size the view is known to show).
	knownSize atomic.Int64
	refs      atomic.Int64
	// onFree runs after the mapping was removed (SetOnFree).
	onFree func()
	// osMap holds what the platform needs to refresh the size and to
	// remove the mapping (datamap_unix.go, datamap_windows.go).
	osMap
}

// MapNeedsInvalidate reports whether data written to the data file through
// a descriptor shows in a shared map of it only after the map's copy is
// invalidated (DataMap.Invalidate, C's MDB_Invalidate): on OpenBSD, which
// has no unified buffer cache (mdb.c 0.9.37 lines 86-93). It is a
// constant, so the engine's calls compile away elsewhere.
const MapNeedsInvalidate = runtime.GOOS == "openbsd"

// Len is the length of the mapping (me_mapsize, or on Windows through a
// read-only handle the file's size when it was mapped).
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

// refreshSize re-reads the size the map can show (the file's, see
// viewSize) and reports whether it reaches needed.
func (m *DataMap) refreshSize(needed int) bool {
	size, err := m.viewSize()
	if err != nil {
		return false
	}
	for {
		old := m.knownSize.Load()
		if size <= old || m.knownSize.CompareAndSwap(old, size) {
			break
		}
	}
	return size >= int64(needed)
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

// SetOnFree sets f to run once the last reference is released and the
// mapping removed, on the goroutine that released it. It must be called
// before the map is shared.
func (m *DataMap) SetOnFree(f func()) { m.onFree = f }

// Release drops a reference; the last one removes the mapping.
func (m *DataMap) Release() {
	if m.refs.Add(-1) == 0 {
		m.unmap()
		m.data = nil
		if m.onFree != nil {
			m.onFree()
		}
	}
}
