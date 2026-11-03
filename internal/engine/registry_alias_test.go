package engine

// An environment under other names of its files, with a real lock file
// (rlmdb core/registry.rs
// an_environment_under_another_name_is_refused_before_its_lock_file, 4762f54).

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/lockfile"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// openRealLock opens the lock file shared with C (lmdb's openLock).
func openRealLock(path string, mode os.FileMode, maxReaders uint32, readOnly bool, f format.Format, claim LockClaim) (LockRegion, error) {
	r, err := lockfile.OpenClaimed(path, mode, maxReaders, readOnly, lockfile.LayoutFor(f), claim)
	if err != nil || r == nil {
		return nil, err
	}
	return r, nil
}

// envFiles are the path, the data file and the lock file of an environment
// name in dir: a directory, created here, or with noSubdir the data file.
func envFiles(t *testing.T, dir, name string, noSubdir bool) (path, data, lock string) {
	t.Helper()
	path = filepath.Join(dir, name)
	if noSubdir {
		return path, path, path + "-lock"
	}
	must(t, os.Mkdir(path, 0o755))
	return path, filepath.Join(path, "data.mdb"), filepath.Join(path, "lock.mdb")
}

// parkedOf is how many descriptors of the file id refused opens left parked.
func parkedOf(id sys.FileID) int {
	registryMu.Lock()
	defer registryMu.Unlock()
	return len(regParked[id])
}

// TestEnvUnderAnotherNameIsRefusedBeforeItsLockFile: the process has an
// environment open, with a live read transaction, and its files are reachable
// under other names (hard links here; a bind mount looks the same). Opening
// them must fail before the lock file is set up again: the exclusive lock
// would be granted within the process and the table re-initialised (the
// reader slot gone, mti_txnid 0, so the next writer would overwrite the
// reader's pages), and closing a descriptor of the lock file would drop the
// open handle's record locks. Every branch of the open: read-write, read-only,
// NoLock, both formats, with and without NoSubdir; both files under another
// name, and the lock file alone.
func TestEnvUnderAnotherNameIsRefusedBeforeItsLockFile(t *testing.T) {
	if !lockfile.SharedLocksAvailable() {
		t.Skip("needs the shared locks")
	}
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, noSubdir := range []bool{false, true} {
			refuseAliases(t, f, noSubdir)
		}
	}
}

func refuseAliases(t *testing.T, f format.Format, noSubdir bool) {
	dir := t.TempDir()
	var sub uint32
	if noSubdir {
		sub = format.NoSubdir
	}
	open := func(path string, flags uint32) (*Env, error) {
		return Open(path, Options{Flags: sub | flags, OpenLock: openRealLock})
	}
	a, aData, aLock := envFiles(t, dir, "a", noSubdir)
	env, err := Open(a, Options{Format: f, MapSize: 1 << 20, Flags: sub | format.NoSync, OpenLock: openRealLock})
	must(t, err)
	txn := beginW(t, env)
	// On overflow pages, which the next commit would reuse if it did not
	// see the reader.
	before := bytes.Repeat([]byte{11}, 32000)
	must(t, txn.Put(MainDBI, []byte("key"), before, 0))
	must(t, txn.Commit())
	reader := beginR(t, env)
	held, err := reader.Get(MainDBI, []byte("key"))
	must(t, err)
	lockID, err := sys.StatFileID(aLock)
	must(t, err)
	intact := func(what string) {
		t.Helper()
		list := env.ReaderList()
		if len(list) != 1 || int(list[0].PID) != os.Getpid() || !list[0].InTxn || list[0].TxnID != 1 {
			t.Fatalf("%v %s: reader table %+v", f, what, list)
		}
		if n := env.lock.NumReaders(); n != 1 {
			t.Fatalf("%v %s: mti_numreaders %d", f, what, n)
		}
		if id := env.lock.TxnID(); id != 1 {
			t.Fatalf("%v %s: mti_txnid %d", f, what, id)
		}
		if !bytes.Equal(held, before) {
			t.Fatalf("%v %s: the held value changed", f, what)
		}
	}
	refused := func(path string, flags uint32) {
		t.Helper()
		if e, err := open(path, flags); err != syscall.EBUSY {
			if err == nil {
				e.Close()
			}
			t.Fatalf("%v %s flags %#x: %v, want EBUSY", f, path, flags, err)
		}
		intact(path)
	}

	// Both files.
	b, bData, bLock := envFiles(t, dir, "b", noSubdir)
	if err := os.Link(aData, bData); err != nil {
		t.Logf("skipped: the file system refuses hard links: %v", err)
		reader.Abort()
		env.Close()
		return
	}
	must(t, os.Link(aLock, bLock))
	for _, flags := range []uint32{0, format.ReadOnly, format.NoLock, format.NoLock | format.ReadOnly} {
		refused(b, flags)
	}
	// Refused at the data file: the lock file was not opened.
	if n := parkedOf(lockID); n != 0 {
		t.Fatalf("%v: %d descriptors parked", f, n)
	}

	// The lock file alone: the data file missing (the lock file comes first,
	// so the data file is not created), then another file.
	c, cData, cLock := envFiles(t, dir, "c", noSubdir)
	must(t, os.Link(aLock, cLock))
	refused(c, 0)
	if _, err := os.Stat(cData); !os.IsNotExist(err) {
		t.Fatalf("%v: the refused open created the data file: %v", f, err)
	}
	content, err := os.ReadFile(aData)
	must(t, err)
	must(t, os.WriteFile(cData, content, 0o644))
	for _, flags := range []uint32{0, format.ReadOnly} {
		refused(c, flags)
	}
	// Their descriptors of the lock file are kept open, not closed.
	if n := parkedOf(lockID); n != 3 {
		t.Fatalf("%v: %d descriptors parked, want 3", f, n)
	}
	// So is one that names the lock file as a data file, here to sniff its
	// format (it is not a data file).
	if _, _, err := SniffFormat(cLock, format.NoSubdir); err != format.Invalid {
		t.Fatalf("%v: SniffFormat of the lock file: %v", f, err)
	}
	if n := parkedOf(lockID); n != 4 {
		t.Fatalf("%v: %d descriptors parked, want 4", f, n)
	}
	intact("SniffFormat")

	// The handle carries on: its next writer follows the last commit and
	// keeps the reader's pages.
	txn = beginW(t, env)
	if id := txn.ID(); id != 2 {
		t.Fatalf("%v: next txnid %d", f, id)
	}
	after := bytes.Repeat([]byte{22}, 32000)
	must(t, txn.Put(MainDBI, []byte("key"), after, 0))
	must(t, txn.Commit())
	if !bytes.Equal(held, before) {
		t.Fatalf("%v: the held value changed", f)
	}
	reader.Abort()
	mustAudit(t, env)
	env.Close()
	// The parked descriptors went with the handle's claim.
	if n := parkedOf(lockID); n != 0 {
		t.Fatalf("%v: %d descriptors parked after the close", f, n)
	}
	registryMu.Lock()
	_, claimed := regFiles[lockID]
	registryMu.Unlock()
	if claimed {
		t.Fatalf("%v: the lock file is still claimed", f)
	}

	// Without the first handle, the other names open the environment.
	env, err = open(b, 0)
	must(t, err)
	defer env.Close()
	r := beginR(t, env)
	defer r.Abort()
	v, err := r.Get(MainDBI, []byte("key"))
	must(t, err)
	if r.ID() != 2 || !bytes.Equal(v, after) {
		t.Fatalf("%v: through the other name: txnid %d", f, r.ID())
	}
}
