// MDB_FDATASYNC on Windows: FlushFileBuffers (mdb.c's
// `#define MDB_FDATASYNC(fd) (!FlushFileBuffers(fd))`).

package sys

import "os"

// Fdatasync flushes the file's data to disk (MDB_FDATASYNC). os.File.Sync
// is FlushFileBuffers on Windows.
func Fdatasync(f *os.File) error { return unwrap(f.Sync()) }
