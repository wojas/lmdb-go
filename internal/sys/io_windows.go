// Opening the environment's files, their identity and size on Windows:
// mdb_fopen's _WIN32 branch (0.9.37 mdb.c line 4405) and rlmdb's
// os/windows.rs (env_file_options, sync_write_options, open_copy_target,
// is_missing, file_id, file_size).
//
//   - Environment files are opened with FILE_SHARE_READ | FILE_SHARE_WRITE
//     and no FILE_SHARE_DELETE, as mdb_fopen does: the files of an open
//     environment cannot be deleted or renamed. The handles are not
//     inheritable. The Unix permission mode is ignored, as in C. The files
//     are opened with CreateFile here, not os.OpenFile, so that the sharing
//     is C's whatever the Go release does.
//   - The meta pages are written through a second handle opened with
//     FILE_FLAG_WRITE_THROUGH (C's MDB_O_META, where Unix uses O_DSYNC).
//   - A copy target (mdb_env_copy2) is created with CREATE_NEW, no sharing
//     and FILE_FLAG_WRITE_THROUGH (MDB_O_COPY). C also asks for
//     FILE_FLAG_NO_BUFFERING, which needs sector-aligned buffers and
//     writes; it is not used, as O_DIRECT is not on Unix (see the ledger in
//     internal/engine/PORTING.md): it changes no byte of the copy.
//   - fdatasync and fsync are FlushFileBuffers (sync_windows.go).
//   - A read-only file system is ERROR_WRITE_PROTECT (C's
//     MDB_ERRCODE_ROFS; internal/lockfile checks it).
//   - The file identity (the registry's "device and inode") is the volume
//     serial number and the 64-bit file index.
//   - Errors are the system's error codes, as syscall.Errno values (C
//     returns GetLastError()).

package sys

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// shareEnv is mdb_fopen's sharing for the environment's files.
const shareEnv = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE

// createFile is CreateFileW(path, access, share, NULL, disp, attrs, NULL)
// as an *os.File.
func createFile(path string, access, share, disp, attrs uint32) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, access, share, nil, disp, attrs, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// OpenEnvFile opens the data file of an environment: read-only
// (MDB_O_RDONLY: GENERIC_READ, OPEN_EXISTING), or read/write and created
// if it does not exist (MDB_O_RDWR: OPEN_ALWAYS). mode is ignored, as in
// C.
func OpenEnvFile(path string, readOnly bool, mode os.FileMode) (*os.File, error) {
	if readOnly {
		return createFile(path, windows.GENERIC_READ, shareEnv, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL)
	}
	return createFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, shareEnv, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL)
}

// OpenDSync opens path for writing through to the disk: the handle used
// for meta-page writes (me_mfd, C's MDB_O_META: FILE_FLAG_WRITE_THROUGH,
// where Unix uses O_DSYNC).
func OpenDSync(path string) (*os.File, error) {
	return createFile(path, windows.GENERIC_WRITE, shareEnv, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_WRITE_THROUGH)
}

// CreateCopyTarget creates the target of mdb_env_copy2, which must not
// exist (CREATE_NEW: ERROR_FILE_EXISTS otherwise), unshared and written
// through (MDB_O_COPY without FILE_FLAG_NO_BUFFERING, see the file
// comment).
func CreateCopyTarget(path string) (*os.File, error) {
	return createFile(path, windows.GENERIC_WRITE, 0, windows.CREATE_NEW, windows.FILE_FLAG_WRITE_THROUGH)
}

// DupFile returns a new *os.File on a duplicate of the handle fd
// (mdb_filehandle_t is a HANDLE on Windows), which shares its file
// position, for mdb_env_copyfd2; closing it leaves fd open.
func DupFile(fd uintptr) (*os.File, error) {
	h, err := dupHandle(windows.Handle(fd))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), "copy"), nil
}

// IsMissing reports whether err, from opening path, says the file does not
// exist, as Unix's ENOENT does: ERROR_FILE_NOT_FOUND, or
// ERROR_PATH_NOT_FOUND for a directory that does not exist. Windows says
// the latter also when a file stands where a directory should be, which
// Unix reports as ENOTDIR, an error: that is not "missing".
func IsMissing(err error, path string) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return errors.Is(err, fs.ErrNotExist)
	}
	switch errno {
	case windows.ERROR_FILE_NOT_FOUND:
		return true
	case windows.ERROR_PATH_NOT_FOUND:
		for dir := filepath.Dir(path); ; {
			if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
				return false
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return true
			}
			dir = parent
		}
	}
	return errors.Is(err, fs.ErrNotExist)
}

// FileSize is the current size of the file.
func FileSize(f *os.File) (int64, error) {
	return handleSize(windows.Handle(f.Fd()))
}

// StatFileID is the FileID of the file path names (following symlinks).
func StatFileID(path string) (FileID, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FileID{}, err
	}
	// No access rights are needed to read the identity; full sharing so
	// that nobody else's open is disturbed.
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return FileID{}, err
	}
	defer windows.CloseHandle(h)
	return handleFileID(h)
}

// FileIDOf is the FileID of an open file.
func FileIDOf(f *os.File) (FileID, error) {
	return handleFileID(windows.Handle(f.Fd()))
}

// handleFileID is the volume serial number and file index of h
// (GetFileInformationByHandle).
func handleFileID(h windows.Handle) (FileID, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return FileID{}, err
	}
	return FileID{
		Dev: uint64(info.VolumeSerialNumber),
		Ino: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}
