// Making and removing the lock-file mapping on Windows (mdb.c 0.9.36 line
// 4937; rlmdb os/windows.rs LockMap::map): CreateFileMapping with
// PAGE_READWRITE over the first size bytes and MapViewOfFile with
// FILE_MAP_WRITE; the mapping handle is closed at once, the view keeps the
// section alive. Views start on the 64 KiB allocation granularity, so the
// shared words are naturally aligned as on Unix.

package lockfile

import (
	"golang.org/x/sys/windows"

	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// mapLockFile maps the first size bytes of the open file fd (a HANDLE),
// which must be at least that long, read/write and shared.
func mapLockFile(fd uintptr, size int) (*lockMap, error) {
	mem, err := sys.MapView(windows.Handle(fd), size, true)
	if err != nil {
		return nil, err
	}
	return &lockMap{mem: mem}, nil
}

// unmapMem removes a mapping made by mapLockFile (UnmapViewOfFile).
func unmapMem(mem []byte) error { return sys.UnmapView(mem) }
