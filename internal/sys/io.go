// Positional I/O (rlmdb os/unix.rs and os/windows.rs: read_at,
// write_all_at) and the error conversion every platform shares. Opening
// files, their identity and their size are in io_unix.go and
// io_windows.go.

package sys

import (
	"io"
	"os"
	"syscall"
)

// ReadAt reads len(buf) bytes at off with pread (on Windows ReadFile with
// the offset in an OVERLAPPED), retrying on EINTR and short reads; it
// returns how many bytes were read (fewer only at the end of the file).
func ReadAt(f *os.File, buf []byte, off int64) (int, error) {
	n, err := f.ReadAt(buf, off)
	if err == io.EOF {
		err = nil
	}
	return n, unwrap(err)
}

// WriteAt writes all of buf at off with pwrite (on Windows WriteFile with
// the offset in an OVERLAPPED).
func WriteAt(f *os.File, buf []byte, off int64) error {
	_, err := f.WriteAt(buf, off)
	return unwrap(err)
}

// FileID identifies a file whatever path names it (symlinks, hard
// links): its device and inode numbers (on Windows the volume serial
// number and the 64-bit file index).
type FileID struct{ Dev, Ino uint64 }

// unwrap returns the syscall.Errno inside an *os.PathError or
// *os.SyscallError, so callers see the C error number as C would return
// it (on Windows the system error code); other errors are returned as
// they are.
func unwrap(err error) error {
	switch e := err.(type) {
	case *os.PathError:
		if errno, ok := e.Err.(syscall.Errno); ok {
			return errno
		}
	case *os.SyscallError:
		if errno, ok := e.Err.(syscall.Errno); ok {
			return errno
		}
	}
	return err
}

// Errno converts an error from the os package into the syscall.Errno it
// wraps, if any (see unwrap).
func Errno(err error) error { return unwrap(err) }
