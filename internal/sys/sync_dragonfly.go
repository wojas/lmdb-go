// MDB_FDATASYNC on DragonFly: fsync, as C LMDB picks for every BSD but
// FreeBSD (mdb.c 0.9.37 lines 143-149); MDB_DSYNC is O_SYNC, since
// DragonFly has no O_DSYNC; rlmdb os/unix.rs (fdatasync, MDB_DSYNC).

package sys

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// oDSync is MDB_DSYNC (mdb.c 0.9.37 lines 456-461): O_SYNC, C's choice
// where O_DSYNC is not defined.
const oDSync = syscall.O_SYNC

// Fdatasync flushes the file's data to disk (MDB_FDATASYNC): os.File.Sync,
// which is fsync(2) here, retried on EINTR.
func Fdatasync(f *os.File) error { return unwrap(f.Sync()) }

// adviseRandom is madvise(MADV_RANDOM) (MDB_NORDAHEAD); failures are
// ignored as in C.
func adviseRandom(b []byte) { _ = unix.Madvise(b, unix.MADV_RANDOM) }
