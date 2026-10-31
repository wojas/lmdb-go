// Positional I/O and syncing (rlmdb os/unix.rs: read_at, write_all_at,
// fdatasync, file_size).

package sys

import (
	"io"
	"os"
	"syscall"
)

// ReadAt reads len(buf) bytes at off with pread, retrying on EINTR and
// short reads; it returns how many bytes were read (fewer only at the end
// of the file).
func ReadAt(f *os.File, buf []byte, off int64) (int, error) {
	n, err := f.ReadAt(buf, off)
	if err == io.EOF {
		err = nil
	}
	return n, unwrap(err)
}

// WriteAt writes all of buf at off with pwrite.
func WriteAt(f *os.File, buf []byte, off int64) error {
	_, err := f.WriteAt(buf, off)
	return unwrap(err)
}

// FileSize is the current size of the file (fstat().st_size).
func FileSize(f *os.File) (int64, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return st.Size, nil
}

// FileID identifies a file whatever path names it (symlinks, hard
// links): its device and inode numbers.
type FileID struct{ Dev, Ino uint64 }

// StatFileID is the FileID of the file path names (stat, following
// symlinks).
func StatFileID(path string) (FileID, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return FileID{}, err
	}
	return FileID{uint64(st.Dev), uint64(st.Ino)}, nil
}

// FileIDOf is the FileID of an open file (fstat).
func FileIDOf(f *os.File) (FileID, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return FileID{}, err
	}
	return FileID{uint64(st.Dev), uint64(st.Ino)}, nil
}

// OpenDSync opens path for writing with O_DSYNC: the descriptor used for
// meta-page writes (me_mfd).
func OpenDSync(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_DSYNC, 0)
	return f, unwrap(err)
}

// unwrap returns the syscall.Errno inside an *os.PathError or
// *os.SyscallError, so callers see the C error number as C would return
// it; other errors are returned as they are.
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
