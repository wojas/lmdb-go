// MDB_FDATASYNC on macOS: fcntl(F_FULLFSYNC), since plain fsync does not
// flush the drive cache there; MDB_DSYNC is O_DSYNC.

package sys

import (
	"os"
	"syscall"
)

// oDSync is MDB_DSYNC (mdb.c 0.9.37 lines 456-461): O_DSYNC.
const oDSync = syscall.O_DSYNC

// Fdatasync flushes the file's data to disk (MDB_FDATASYNC). os.File.Sync
// uses F_FULLFSYNC on macOS, as C's MDB_FDATASYNC does.
func Fdatasync(f *os.File) error { return unwrap(f.Sync()) }

// adviseRandom would be madvise(MADV_RANDOM) (MDB_NORDAHEAD); the syscall
// package has no madvise on macOS, and the advice is only a hint.
func adviseRandom([]byte) {}
