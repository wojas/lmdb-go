//go:build unix

// The data file map on Unix: mmap(PROT_READ, MAP_SHARED) of the map size
// (mdb_env_map; rlmdb os/unix.rs DataMap::map_readonly).

package sys

import (
	"os"
	"syscall"
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
