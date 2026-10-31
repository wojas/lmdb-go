// MDB_FDATASYNC on Linux: fdatasync(2).

package sys

import (
	"os"
	"syscall"
)

// Fdatasync flushes the file's data to disk (MDB_FDATASYNC).
func Fdatasync(f *os.File) error {
	for {
		err := syscall.Fdatasync(int(f.Fd()))
		if err != syscall.EINTR {
			return err
		}
	}
}

// adviseRandom is madvise(MADV_RANDOM) (MDB_NORDAHEAD); failures are
// ignored as in C.
func adviseRandom(b []byte) { _ = syscall.Madvise(b, syscall.MADV_RANDOM) }
