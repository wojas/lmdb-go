// MDB_FDATASYNC on FreeBSD: fdatasync(2), which C LMDB keeps there from
// FreeBSD 11.1 on (mdb.c 0.9.37 lines 135-142); MDB_DSYNC is O_DSYNC
// (FreeBSD 13 and later); rlmdb os/unix.rs (fdatasync, MDB_DSYNC).

package sys

import (
	"os"

	"golang.org/x/sys/unix"
)

// oDSync is MDB_DSYNC (mdb.c 0.9.37 lines 456-461): FreeBSD's O_DSYNC
// (<fcntl.h>, 13.0 and later), which neither package syscall nor
// golang.org/x/sys/unix v0.30.0 defines.
const oDSync = 0x01000000

// Fdatasync flushes the file's data to disk (MDB_FDATASYNC): fdatasync(2)
// by its system call number, since neither package syscall nor
// golang.org/x/sys/unix wraps it on FreeBSD. EINTR is retried.
func Fdatasync(f *os.File) error {
	for {
		_, _, e := unix.Syscall(unix.SYS_FDATASYNC, f.Fd(), 0, 0)
		switch e {
		case 0:
			return nil
		case unix.EINTR:
		default:
			return e
		}
	}
}

// adviseRandom is madvise(MADV_RANDOM) (MDB_NORDAHEAD); failures are
// ignored as in C.
func adviseRandom(b []byte) { _ = unix.Madvise(b, unix.MADV_RANDOM) }
