package engine

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// TestOpenTwice: one data file is open at most once per process, whatever
// path names it: directory or MDB_NOSUBDIR mode, symlinks, hard links.
// The second open fails with EBUSY before it touches the lock file (a
// second handle's close would drop the first one's record locks).
func TestOpenTwice(t *testing.T) {
	noLock := Options{Flags: format.NoLock}
	env, dir := openTest(t, format.V09, noLock) // creates data.mdb
	data := filepath.Join(dir, "data.mdb")
	other := t.TempDir()
	dirLink := filepath.Join(other, "dirlink")
	fileLink := filepath.Join(other, "filelink")
	hardDir := filepath.Join(other, "hard")
	for _, err := range []error{
		os.Symlink(dir, dirLink),
		os.Symlink(data, fileLink),
		os.Mkdir(hardDir, 0o755),
		os.Link(data, filepath.Join(hardDir, "data.mdb")),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	refused := func(path string, flags uint32) {
		t.Helper()
		opened := false
		opts := Options{Flags: flags, OpenLock: func(string, os.FileMode, uint32, bool, format.Format) (LockRegion, error) {
			opened = true
			return NewMemLock(0), nil
		}}
		for _, nolock := range []bool{true, false} {
			o := opts
			if nolock {
				o.Flags |= format.NoLock
			}
			e, err := Open(path, o)
			if err == nil {
				e.Close()
			}
			if err != syscall.EBUSY {
				t.Errorf("open %s (flags %#x, NoLock %v): %v (want EBUSY)", path, flags, nolock, err)
			}
			if opened {
				t.Errorf("open %s (flags %#x): lock file opened before EBUSY", path, flags)
			}
		}
	}
	refused(dir, 0)
	refused(data, format.NoSubdir)
	refused(dirLink, 0)
	refused(filepath.Join(dirLink, "data.mdb"), format.NoSubdir)
	refused(fileLink, format.NoSubdir)
	refused(hardDir, 0)
	refused(filepath.Join(hardDir, "data.mdb"), format.NoSubdir)

	// A name that appears after the registration (here while the lock
	// file is opened) is caught once the data file is open.
	late := filepath.Join(other, "late")
	e, err := Open(late, Options{Flags: format.NoSubdir, OpenLock: func(string, os.FileMode, uint32, bool, format.Format) (LockRegion, error) {
		if err := os.Link(data, late); err != nil {
			return nil, err
		}
		return NewMemLock(0), nil
	}})
	if err == nil {
		e.Close()
	}
	if err != syscall.EBUSY {
		t.Errorf("open of a hard link made during the open: %v (want EBUSY)", err)
	}

	// The refused opens left the first one's registration in place.
	putOne(t, env, "k", "v")
	refused(data, format.NoSubdir)
	env.Close()
	// Closing releases every name of the file.
	for _, p := range []struct {
		path  string
		flags uint32
	}{{data, format.NoSubdir}, {hardDir, 0}, {dir, 0}} {
		e, err := Open(p.path, Options{Flags: p.flags | format.NoLock})
		if err != nil {
			t.Fatalf("open %s after close: %v", p.path, err)
		}
		e.Close()
	}
}

// TestOpenTwiceNew: the same for a data file the first open creates, named
// in the other mode.
func TestOpenTwiceNew(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data.mdb")
	for _, first := range []struct {
		path        string
		flags       uint32
		other       string
		otherFlags  uint32
		description string
	}{
		{data, format.NoSubdir, dir, 0, "NoSubdir, then the directory"},
		{dir, 0, data, format.NoSubdir, "directory, then NoSubdir"},
	} {
		os.Remove(data)
		env, err := Open(first.path, Options{Flags: first.flags | format.NoLock})
		if err != nil {
			t.Fatal(err)
		}
		if e, err := Open(first.other, Options{Flags: first.otherFlags | format.NoLock}); err != syscall.EBUSY {
			t.Errorf("%s: %v (want EBUSY)", first.description, err)
			if err == nil {
				e.Close()
			}
		}
		env.Close()
	}
}

func putOne(t *testing.T, env *Env, k, v string) {
	t.Helper()
	txn, err := env.BeginTxn(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.Put(format.MainDBI, []byte(k), []byte(v), 0); err != nil {
		txn.Abort()
		t.Fatal(err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
}
