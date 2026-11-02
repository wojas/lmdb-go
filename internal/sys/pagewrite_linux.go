// The writes of mdb_page_flush and mdb_env_write_meta on Linux: pwrite(2)
// for one buffer and pwritev(2) for a batch of contiguous pages (mdb.c
// 0.9.36 mdb_page_flush with MDB_USE_PWRITEV, line 3394), on the file's
// descriptor directly.
//
// os.File.WriteAt takes and releases the file's reference count around
// each call, and a batch would have to be gathered into one buffer first
// (the syscall package has no pwritev); on a commit of many pages that
// copy is a few percent of the commit's time. pagewrite_other.go is the
// version for the other systems.

package sys

import (
	"io"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// maxRW is the most bytes one write call is given, as internal/poll does
// for files (Linux transfers at most 0x7ffff000 bytes per call).
const maxRW = 1 << 30

// maxIOV is the most buffers one pwritev call is given (mdb.c's
// MDB_COMMIT_PAGES; IOV_MAX is 1024).
const maxIOV = 64

// PageWriter writes runs of pages that are contiguous in a file (C's
// pwritev in mdb_page_flush). The zero value is ready to use; its iovec
// array is kept for reuse, so that a commit allocates nothing.
type PageWriter struct {
	iov [maxIOV]syscall.Iovec
}

// WriteAt writes the buffers bufs, which follow each other in the file,
// at off: one pwrite for one buffer, pwritev for several. Short writes are
// resumed; an error is the errno.
func (w *PageWriter) WriteAt(f *os.File, bufs [][]byte, off int64) error {
	for len(bufs) > 1 {
		n := min(len(bufs), maxIOV)
		total := 0
		for i, b := range bufs[:n] {
			total += len(b)
			if len(b) == 0 || total > maxRW {
				// Not a batch of pages (the engine's hold no empty buffers
				// and stay below maxRW): one buffer at a time.
				clear(w.iov[:i])
				return writeFrom(f, bufs, off, 0)
			}
			w.iov[i].Base = &b[0]
			w.iov[i].SetLen(len(b))
		}
		written, err := pwritev(f, w.iov[:n], off)
		clear(w.iov[:n]) // keep no references to the pages
		if err != nil {
			return err
		}
		if written < total {
			// A short write (the disk filling up, a signal): the rest of
			// the batch one buffer at a time.
			if err := writeFrom(f, bufs[:n], off, written); err != nil {
				return err
			}
		}
		bufs = bufs[n:]
		off += int64(total)
	}
	if len(bufs) == 1 {
		return Pwrite(f, bufs[0], off)
	}
	return nil
}

// writeFrom writes the run bufs, which starts at off, from its byte skip
// on, one buffer at a time.
func writeFrom(f *os.File, bufs [][]byte, off int64, skip int) error {
	off += int64(skip)
	for _, b := range bufs {
		if skip >= len(b) {
			skip -= len(b)
			continue
		}
		b, skip = b[skip:], 0
		if err := Pwrite(f, b, off); err != nil {
			return err
		}
		off += int64(len(b))
	}
	return nil
}

// pwritev is one pwritev(2) call, retried on EINTR.
func pwritev(f *os.File, iov []syscall.Iovec, off int64) (int, error) {
	fd := f.Fd()
	for {
		// The iovecs and the pages they point to are Go memory that the
		// caller keeps referenced during the call (PageWriter.iov and the
		// dirty pages); the kernel only reads them.
		r, _, e := syscall.Syscall6(syscall.SYS_PWRITEV, fd, uintptr(unsafe.Pointer(&iov[0])), uintptr(len(iov)),
			uintptr(off), uintptr(uint64(off)>>63>>1), 0)
		if e == syscall.EINTR {
			continue
		}
		runtime.KeepAlive(f)
		if e != 0 {
			return 0, e
		}
		return int(r), nil
	}
}

// Pwrite writes all of b at off with pwrite(2) on the file's descriptor,
// resuming short writes and retrying on EINTR as os.File.WriteAt does.
func Pwrite(f *os.File, b []byte, off int64) error {
	fd := int(f.Fd())
	var err error
	for len(b) > 0 {
		var n int
		n, err = syscall.Pwrite(fd, b[:min(len(b), maxRW)], off)
		if err == syscall.EINTR {
			err = nil
			continue
		}
		if err != nil {
			break
		}
		if n == 0 {
			err = io.ErrUnexpectedEOF
			break
		}
		b = b[n:]
		off += int64(n)
	}
	runtime.KeepAlive(f)
	return err
}
