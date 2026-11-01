package engine

// The registry of open environments (registry.go; rlmdb core/registry.rs
// tests, rust_env.rs and core/reader.rs tests of d3d9239).

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// fileNamesOf are the names of one data file: its directory and the file
// in MDB_NOSUBDIR mode, through symlinks to both and a hard link.
func fileNamesOf(t *testing.T, dir string) []struct {
	path  string
	flags uint32
} {
	t.Helper()
	data := filepath.Join(dir, "data.mdb")
	other := t.TempDir()
	dirLink := filepath.Join(other, "dirlink")
	fileLink := filepath.Join(other, "filelink")
	hardDir := filepath.Join(other, "hard")
	names := []struct {
		path  string
		flags uint32
	}{{dir, 0}, {data, format.NoSubdir}}
	add := func(path string, flags uint32) {
		names = append(names, struct {
			path  string
			flags uint32
		}{path, flags})
	}
	// made reports whether a link could be made. On Windows some file
	// systems have no symlinks or hard links (exFAT, FAT), and NTFS
	// symlinks need a privilege: the names that can be made are checked.
	made := func(what string, err error) bool {
		t.Helper()
		if err == nil {
			return true
		}
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		t.Logf("no %s here: %v", what, err)
		return false
	}
	if made("directory symlink", os.Symlink(dir, dirLink)) {
		add(dirLink, 0)
		add(filepath.Join(dirLink, "data.mdb"), format.NoSubdir)
	}
	if made("file symlink", os.Symlink(data, fileLink)) {
		add(fileLink, format.NoSubdir)
	}
	if err := os.Mkdir(hardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if made("hard link", os.Link(data, filepath.Join(hardDir, "data.mdb"))) {
		add(hardDir, 0)
		add(filepath.Join(hardDir, "data.mdb"), format.NoSubdir)
	}
	return names
}

// openCounting opens path with flags (plus NoLock if noLock) and reports
// whether the lock file was opened.
func openCounting(path string, flags uint32, noLock bool) (*Env, bool, error) {
	opened := false
	opts := Options{Flags: flags, OpenLock: func(string, os.FileMode, uint32, bool, format.Format) (LockRegion, error) {
		opened = true
		return NewMemLock(0), nil
	}}
	if noLock {
		opts.Flags |= format.NoLock
	}
	e, err := Open(path, opts)
	return e, opened, err
}

// TestOpenTwiceWithLockFile: a data file that a handle with a lock file
// has open is open at most once in the process, whatever path names it
// (directory or MDB_NOSUBDIR mode, symlinks, hard links), with or without
// NoLock. The second open fails with EBUSY before it touches the lock file
// (a second handle's close would drop the first one's record locks).
func TestOpenTwiceWithLockFile(t *testing.T) {
	env, dir := openTest(t, format.V09, Options{}) // a lock file (MemLock stand-in)
	data := filepath.Join(dir, "data.mdb")
	names := fileNamesOf(t, dir)
	refused := func() {
		t.Helper()
		for _, n := range names {
			for _, noLock := range []bool{true, false} {
				e, opened, err := openCounting(n.path, n.flags, noLock)
				if err == nil {
					e.Close()
				}
				if err != syscall.EBUSY {
					t.Errorf("open %s (flags %#x, NoLock %v): %v (want EBUSY)", n.path, n.flags, noLock, err)
				}
				if opened {
					t.Errorf("open %s (flags %#x): lock file opened before EBUSY", n.path, n.flags)
				}
			}
		}
	}
	refused()

	// A name that appears after the registration (here while the lock
	// file is opened) is caught once the data file is open.
	late := filepath.Join(t.TempDir(), "late")
	var linkErr error
	e, err := Open(late, Options{Flags: format.NoSubdir, OpenLock: func(string, os.FileMode, uint32, bool, format.Format) (LockRegion, error) {
		if linkErr = os.Link(data, late); linkErr != nil {
			return nil, linkErr
		}
		return NewMemLock(0), nil
	}})
	if err == nil {
		e.Close()
	}
	switch {
	case linkErr != nil && runtime.GOOS == "windows":
		t.Logf("no hard link here: %v", linkErr)
	case err != syscall.EBUSY:
		t.Errorf("open of a hard link made during the open: %v (want EBUSY)", err)
	}

	// The refused opens left the first one's registration in place.
	putOne(t, env, "k", "v")
	refused()
	env.Close()
	// Closing releases every name of the file.
	for _, n := range names {
		e, err := Open(n.path, Options{Flags: n.flags | format.NoLock})
		if err != nil {
			t.Fatalf("open %s after close: %v", n.path, err)
		}
		e.Close()
	}
}

// TestNoLockHandlesShare: any number of NoLock handles may open a data
// file, under any of its names; they share one in-memory region (reader
// table, committed txnid, writer lock). A handle with a lock file is
// refused next to them, before it opens the lock file, and so is a
// read-only handle on a read-only file system (no lock file, no NoLock),
// whose table would be its own.
func TestNoLockHandlesShare(t *testing.T) {
	env, dir := openTest(t, format.V09, Options{Flags: format.NoLock})
	putOne(t, env, "k", "v")
	names := fileNamesOf(t, dir)
	var handles []*Env
	for _, n := range names {
		e, opened, err := openCounting(n.path, n.flags, true)
		if err != nil {
			t.Fatalf("NoLock open %s (flags %#x): %v", n.path, n.flags, err)
		}
		if opened || e.HasLockFile() {
			t.Fatalf("NoLock open %s used a lock file", n.path)
		}
		if e.mem != env.mem {
			t.Fatalf("open %s: a reader table of its own", n.path)
		}
		handles = append(handles, e)
		e, opened, err = openCounting(n.path, n.flags, false)
		if err == nil {
			e.Close()
		}
		if err != syscall.EBUSY || opened {
			t.Errorf("open %s with a lock file next to NoLock handles: %v, lock file opened %v (want EBUSY before it)", n.path, err, opened)
		}
		e, err = Open(n.path, Options{Flags: n.flags | format.ReadOnly, OpenLock: func(string, os.FileMode, uint32, bool, format.Format) (LockRegion, error) {
			return nil, nil // a read-only file system: no lock file
		}})
		if err == nil {
			e.Close()
		}
		if err != syscall.EBUSY {
			t.Errorf("read-only open %s without a lock file and without NoLock: %v (want EBUSY)", n.path, err)
		}
	}
	// Every handle sees the others' commits.
	putOne(t, handles[len(handles)-1], "k3", "v3")
	for i, e := range append(handles, env) {
		r := beginR(t, e)
		if v, err := r.Get(MainDBI, []byte("k3")); err != nil || string(v) != "v3" {
			t.Errorf("handle %d: %q %v", i, v, err)
		}
		r.Abort()
	}
	// The region lives while one handle is open.
	env.Close()
	for _, e := range handles[1:] {
		e.Close()
	}
	if e, err := Open(dir, Options{OpenLock: OpenMemLock}); err != syscall.EBUSY {
		t.Errorf("open with a lock file while one NoLock handle is open: %v", err)
		if err == nil {
			e.Close()
		}
	}
	mem := handles[0].mem
	handles[0].Close()
	e, err := Open(dir, Options{Flags: format.NoLock})
	if err != nil {
		t.Fatal(err)
	}
	if e.mem == mem {
		t.Error("a new handle got the region of handles that are all closed")
	}
	e.Close()
	e, err = Open(dir, Options{OpenLock: OpenMemLock})
	if err != nil {
		t.Fatalf("open with a lock file after the NoLock handles closed: %v", err)
	}
	e.Close()
}

// TestOpenTwiceNew: the same for a data file the first open creates, named
// in the other mode: one file, so NoLock handles share it and a handle
// with a lock file is refused.
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
		e, err := Open(first.other, Options{Flags: first.otherFlags | format.NoLock})
		if err != nil {
			t.Fatalf("%s: %v", first.description, err)
		}
		if e.mem != env.mem {
			t.Errorf("%s: two reader tables for one file", first.description)
		}
		e.Close()
		if e, err := Open(first.other, Options{Flags: first.otherFlags, OpenLock: OpenMemLock}); err != syscall.EBUSY {
			t.Errorf("%s, with a lock file: %v (want EBUSY)", first.description, err)
			if err == nil {
				e.Close()
			}
		}
		env.Close()
	}
}

// TestNoLockReaderPinsAcrossHandles: a reader of one NoLock handle keeps
// its pages from the writers of another, which leave the page numbers and
// free list that one handle doing both would (rlmdb core::reader test).
func TestNoLockReaderPinsAcrossHandles(t *testing.T) {
	big := bytes.Repeat([]byte("A"), 5000)
	run := func(two bool) []byte {
		env, dir := openTest(t, format.V09, Options{Flags: format.NoLock})
		defer env.Close()
		writer := env
		if two {
			var err error
			if writer, err = Open(dir, Options{Flags: format.NoLock | format.NoSync, MapSize: 64 << 20}); err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
		}
		w := beginW(t, writer)
		must(t, w.Put(MainDBI, []byte("k"), big, 0))
		must(t, w.Commit())
		r := beginR(t, env)
		v, err := r.Get(MainDBI, []byte("k"))
		must(t, err)
		for i := 0; i < 20; i++ {
			putKV(t, writer, "k", fmt.Sprint(i, bytes.Repeat([]byte("B"), 3000+i)))
		}
		if !bytes.Equal(v, big) {
			t.Fatalf("two handles %v: the reader's value changed", two)
		}
		r.Abort()
		putKV(t, writer, "z", "after")
		got, err := os.ReadFile(filepath.Join(dir, "data.mdb"))
		must(t, err)
		return got
	}
	if one, two := run(false), run(true); !bytes.Equal(one, two) {
		t.Fatal("the writer of another handle wrote another file than the reader's own handle")
	}
}

// TestNoLockWritersOfHandlesSerialised: write transactions of several
// NoLock handles of one file exclude each other (no lost update), while
// read-only handles open, read and close.
func TestNoLockWritersOfHandlesSerialised(t *testing.T) {
	env, dir := openTest(t, format.V09, Options{Flags: format.NoLock})
	defer env.Close()
	putKV(t, env, "n", "0")
	const writers, rounds = 4, 50
	var wg sync.WaitGroup
	errs := make(chan error, 2*writers)
	for i := 0; i < writers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			e, err := Open(dir, Options{Flags: format.NoLock | format.NoSync, MapSize: 64 << 20})
			if err != nil {
				errs <- err
				return
			}
			defer e.Close()
			for j := 0; j < rounds; j++ {
				w, err := e.BeginTxn(nil, 0)
				if err != nil {
					errs <- err
					return
				}
				v, err := w.Get(MainDBI, []byte("n"))
				var n int
				if err == nil {
					_, err = fmt.Sscan(string(v), &n)
				}
				if err == nil {
					err = w.Put(MainDBI, []byte("n"), []byte(fmt.Sprint(n+1)), 0)
				}
				if err == nil {
					err = w.Commit()
				} else {
					w.Abort()
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < rounds/5; j++ {
				e, err := Open(dir, Options{Flags: format.NoLock | format.ReadOnly})
				if err != nil {
					errs <- err
					return
				}
				r, err := e.BeginTxn(nil, format.ReadOnly)
				if err == nil {
					_, err = r.Get(MainDBI, []byte("n"))
					r.Abort()
				}
				e.Close()
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	r := beginR(t, env)
	defer r.Abort()
	if v, err := r.Get(MainDBI, []byte("n")); err != nil || string(v) != fmt.Sprint(writers*rounds) {
		t.Fatalf("counter %q %v, want %d", v, err, writers*rounds)
	}
}

// TestNoLockNewFileFromManyGoroutines: NoLock handles opening one new file
// at once all see one header (one of them writes the meta pages, under the
// file's writer lock), and none fails with MDB_INVALID.
func TestNoLockNewFileFromManyGoroutines(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		for round := 0; round < 10; round++ {
			dir := t.TempDir()
			const n = 8
			envs := make([]*Env, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range envs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					envs[i], errs[i] = Open(dir, Options{Format: f, Flags: format.NoLock | format.NoSync, MapSize: 1 << 20})
				}()
			}
			close(start)
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("%v round %d: open %d: %v", f, round, i, err)
				}
			}
			putKV(t, envs[0], "k", "v")
			for i, e := range envs {
				if e.mem != envs[0].mem {
					t.Fatalf("%v: handle %d has a table of its own", f, i)
				}
				r := beginR(t, e)
				if v, err := r.Get(MainDBI, []byte("k")); err != nil || string(v) != "v" {
					t.Fatalf("%v: handle %d: %q %v", f, i, v, err)
				}
				r.Abort()
			}
			for _, e := range envs {
				e.Close()
			}
		}
	}
}

// TestNoLockHandleClosedUnderOthersReader: closing a handle (on another
// goroutine) while another handle's reader is live leaves that reader and
// its slot alone; the closed handle gives back only its own pooled slots.
func TestNoLockHandleClosedUnderOthersReader(t *testing.T) {
	env, dir := openTest(t, format.V09, Options{Flags: format.NoLock})
	defer env.Close()
	putKV(t, env, "k", "v1")
	other, err := Open(dir, Options{Flags: format.NoLock | format.NoSync, MapSize: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	pooled := beginR(t, other)
	pooled.Abort() // an idle slot in other's pool
	r := beginR(t, env)
	busy := func() int {
		n := 0
		for _, ri := range env.mem.ReaderList() {
			if ri.InTxn {
				n++
			}
		}
		return len(env.mem.ReaderList())*1000 + n
	}
	if got := busy(); got != 2001 {
		t.Fatalf("slots before the close: %d, want 2 of which 1 busy", got)
	}
	done := make(chan struct{})
	go func() { other.Close(); close(done) }()
	<-done
	if got := busy(); got != 1001 {
		t.Fatalf("slots after the close: %d, want 1, busy", got)
	}
	putKV(t, env, "k", "v2")
	if v, err := r.Get(MainDBI, []byte("k")); err != nil || string(v) != "v1" {
		t.Fatalf("the reader after the other handle closed: %q %v", v, err)
	}
	r.Abort()
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
