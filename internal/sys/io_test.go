// Tests of the file helpers every platform shares: what counts as a
// missing file (rlmdb os::is_missing), the copy target's exclusive
// creation, FileID, and ExtendFile (rlmdb os::extend_file), which only
// ever grows a file.

package sys

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestIsMissing(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path    string
		missing bool
	}{
		{filepath.Join(dir, "data.mdb"), true},
		{filepath.Join(dir, "none", "data.mdb"), true},
		// A file where a directory should be: ENOTDIR on Unix; Windows says
		// ERROR_PATH_NOT_FOUND, which is not "missing" then either.
		{filepath.Join(file, "data.mdb"), false},
		{filepath.Join(file, "sub", "data.mdb"), false},
	} {
		_, err := os.Open(c.path)
		if err == nil {
			t.Fatalf("%s: opened", c.path)
		}
		if got := IsMissing(err, c.path); got != c.missing {
			t.Errorf("%s: IsMissing(%v) = %v, want %v", c.path, err, got, c.missing)
		}
	}
}

func TestCreateCopyTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.mdb")
	f, err := CreateCopyTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("copy")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// The target must not exist: EEXIST, on Windows ERROR_FILE_EXISTS.
	if _, err := CreateCopyTarget(path); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: %v", err)
	}
}

func TestFileID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.mdb")
	f, err := OpenEnvFile(path, false, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, err := FileIDOf(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := StatFileID(path)
	if err != nil || a != b {
		t.Fatalf("open %+v, stat %+v %v", a, b, err)
	}
	other := filepath.Join(dir, "other.mdb")
	if err := os.WriteFile(other, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := StatFileID(other); err != nil || c == a {
		t.Fatalf("another file has the same id %+v %v", c, err)
	}
	if _, err := StatFileID(filepath.Join(dir, "none")); err == nil {
		t.Fatal("id of a missing file")
	}
	ro, err := OpenEnvFile(path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Write([]byte("x")); err == nil {
		t.Fatal("wrote through a read-only handle")
	}
	if n, err := FileSize(f); n != 0 || err != nil {
		t.Fatalf("size %d %v", n, err)
	}
}

// TestExtendFile: ExtendFile grows a shorter file and leaves a longer one
// as it is, on every system (the engine needs it on Windows only; rlmdb
// tests its os::extend_file the same way everywhere).
func TestExtendFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "data.mdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := WriteAt(f, []byte{1, 2, 3}, 4093); err != nil {
		t.Fatal(err)
	}
	if err := ExtendFile(f, 64<<10); err != nil {
		t.Fatal(err)
	}
	if n, err := FileSize(f); n != 64<<10 || err != nil {
		t.Fatalf("size %d, %v", n, err)
	}
	if err := WriteAt(f, []byte{9}, 128<<10-1); err != nil {
		t.Fatal(err)
	}
	if err := ExtendFile(f, 64<<10); err != nil {
		t.Fatal(err)
	}
	if n, err := FileSize(f); n != 128<<10 || err != nil {
		t.Fatalf("a longer file: size %d, %v", n, err)
	}
	buf := make([]byte, 3)
	if _, err := ReadAt(f, buf, 4093); err != nil || buf[0] != 1 || buf[2] != 3 {
		t.Fatalf("content %v, %v", buf, err)
	}
}
