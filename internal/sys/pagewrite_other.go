//go:build !linux

// The writes of mdb_page_flush and mdb_env_write_meta outside Linux
// (pagewrite_linux.go has the Linux version): a batch of contiguous pages
// is gathered into one buffer and written with one positional write, where
// C uses lseek and writev (mdb.c 0.9.36 mdb_page_flush, line 3394; Windows:
// WriteFile per batch with the offset in an OVERLAPPED).

package sys

import "os"

// PageWriter writes runs of pages that are contiguous in a file (C's
// writev in mdb_page_flush). The zero value is ready to use; its gather
// buffer is kept for reuse, so that a commit allocates nothing.
type PageWriter struct {
	buf []byte
}

// WriteAt writes the buffers bufs, which follow each other in the file,
// at off, with one write.
func (w *PageWriter) WriteAt(f *os.File, bufs [][]byte, off int64) error {
	switch len(bufs) {
	case 0:
		return nil
	case 1:
		return WriteAt(f, bufs[0], off)
	}
	b := w.buf[:0]
	for _, x := range bufs {
		b = append(b, x...)
	}
	w.buf = b
	return WriteAt(f, b, off)
}

// Pwrite writes all of b at off (WriteAt).
func Pwrite(f *os.File, b []byte, off int64) error { return WriteAt(f, b, off) }
