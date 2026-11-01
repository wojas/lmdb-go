//go:build unix

// The data file map on Unix: mmap(PROT_READ, MAP_SHARED) of the map size
// (mdb_env_map; rlmdb os/unix.rs DataMap::map_readonly).

package sys

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// osMap: a duplicate of the data file's descriptor, to refresh the file
// size; closed with the mapping.
type osMap struct {
	fd int
}

// MapData maps size bytes of the data file f read-only and shared, for
// either data format (on Unix both are mapped alike; reserve is used on
// Windows only, see datamap_windows.go). random applies
// madvise(MADV_RANDOM) where available (MDB_NORDAHEAD). metaEnd is the end
// of the meta pages, which Slice refuses. The map keeps a duplicate of f's
// descriptor to refresh the file size, closed with the mapping. It starts
// with one reference, the caller's.
func MapData(f *os.File, size int, random bool, metaEnd int, reserve bool) (*DataMap, error) {
	if size <= 0 {
		return nil, syscall.EINVAL
	}
	fd, err := dupCloexec(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	data, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if random {
		adviseRandom(data)
	}
	m := &DataMap{data: data, metaEnd: metaEnd, osMap: osMap{fd: fd}}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = syscall.Munmap(data)
		syscall.Close(fd)
		return nil, err
	}
	m.knownSize.Store(st.Size)
	m.refs.Store(1)
	return m, nil
}

// viewSize is the size of the data file (fstat): the map shows all of it.
func (m *DataMap) viewSize() (int64, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(m.fd, &st); err != nil {
		return 0, err
	}
	return st.Size, nil
}

// unmap removes the mapping and closes the descriptor.
func (m *DataMap) unmap() {
	_ = syscall.Munmap(m.data)
	syscall.Close(m.fd)
}

// Invalidate is MDB_Invalidate, called only where MapNeedsInvalidate
// (OpenBSD): after n bytes at off of the file were written through a
// descriptor, msync(MS_INVALIDATE) drops the map's stale copy of them, so
// the next access reads them from the file. C passes its range as is,
// which msync refuses unless it starts on an OS page (pages of LMDB 1.0
// environments may be smaller, and so may those of a 0.9 environment made
// on a system with smaller pages); here it is widened to whole OS pages
// (rlmdb's DataMap::invalidate, KNOWN_C_BUGS.md #39) and clamped to the
// map. Errors are ignored, as in C. The bytes of every page a snapshot can
// reach do not change (they are read again as they were), so slices
// handed out stay valid.
func (m *DataMap) Invalidate(off uint64, n int) {
	if n < 0 {
		return
	}
	size := uint64(len(m.data))
	start := off - off%uint64(OSPageSize())
	end := off + uint64(n)
	if end < off || end > size {
		end = size
	}
	if start >= end {
		return
	}
	_ = unix.Msync(m.data[start:end], unix.MS_INVALIDATE)
}

// CommitPages does nothing on Unix: the map always covers the map size,
// and pages written past the end of the file show in every process's map
// (on Windows, LMDB 1.0's reserved view needs them committed, see
// datamap_windows.go).
func (m *DataMap) CommitPages(off, n int) error { return nil }

// OSPageSize is the OS memory page size (4096 on x86-64 Linux, 16384 on
// Apple Silicon macOS).
func OSPageSize() int { return os.Getpagesize() }

// dupCloexec duplicates fd with the close-on-exec flag set.
func dupCloexec(fd int) (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	nfd, err := syscall.Dup(fd)
	if err != nil {
		return -1, err
	}
	syscall.CloseOnExec(nfd)
	return nfd, nil
}
