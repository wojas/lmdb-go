//go:build unix

// Opening the environment's files, their identity and size on Unix
// (mdb_fopen's MDB_O_RDWR, MDB_O_RDONLY, MDB_O_META and MDB_O_COPY; rlmdb
// os/unix.rs: env_file_options, sync_write_options, open_copy_target,
// is_missing, file_id, file_size).

package sys

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// OpenEnvFile opens the data file of an environment: read-only, or
// read/write and created with the permissions mode if it does not exist.
func OpenEnvFile(path string, readOnly bool, mode os.FileMode) (*os.File, error) {
	if readOnly {
		return os.OpenFile(path, os.O_RDONLY, 0)
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, mode)
}

// OpenDSync opens path for writing with MDB_DSYNC (O_DSYNC, or O_SYNC
// where there is none: oDSync in sync_<os>.go): the descriptor used for
// meta-page writes (me_mfd, C's MDB_O_META).
func OpenDSync(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|oDSync, 0)
	return f, unwrap(err)
}

// CreateCopyTarget creates the target of mdb_env_copy2, which must not
// exist (O_EXCL), with mode 0666 before the umask (MDB_O_COPY without
// O_DIRECT: see the ledger in internal/engine/PORTING.md).
func CreateCopyTarget(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
}

// DupFile returns a new *os.File on a duplicate of the descriptor fd
// (mdb_filehandle_t), which shares its file position, for
// mdb_env_copyfd2; closing it leaves fd open.
func DupFile(fd uintptr) (*os.File, error) {
	nfd, err := syscall.Dup(int(fd))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(nfd), "copy"), nil
}

// IsMissing reports whether err, from opening path, says the file does not
// exist (ENOENT, also for a missing directory; a file where a directory
// should be is ENOTDIR, an error).
func IsMissing(err error, path string) bool { return errors.Is(err, fs.ErrNotExist) }

// FileSize is the current size of the file (fstat().st_size).
func FileSize(f *os.File) (int64, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return st.Size, nil
}

// MapNeedsFileLength reports whether a writable map MapData makes with
// reserve needs the data file to be as long as the map before it is mapped:
// never here (C 0.9 does not extend the data file on Unix, and a map may
// reach past the end of the file). rlmdb os::map_needs_file_length.
func MapNeedsFileLength(reserve bool) bool { return false }

// ExtendFile extends f to size bytes if it is shorter, never shortening it.
// Only Windows needs it before mapping (MapNeedsFileLength); it is the same
// everywhere so that the caller's ordering can be tested on any system.
// rlmdb os::extend_file.
func ExtendFile(f *os.File, size int64) error {
	n, err := FileSize(f)
	if err != nil || n >= size {
		return err
	}
	return f.Truncate(size)
}

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
