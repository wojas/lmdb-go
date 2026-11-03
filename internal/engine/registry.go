// The process-wide registry of open environments (rlmdb core/registry.rs,
// as of its commit 4762f54).
//
// C lets a process open one environment any number of times, but lmdb.h
// forbids it with a lock file: POSIX record locks belong to the process,
// so closing any descriptor of the lock file drops every lock the process
// holds on it, the other handle's too. Without a lock file (MDB_NOLOCK)
// there are no record locks to lose, and programs do open one environment
// several times (for example once per embedded interpreter). The registry
// enforces the first and makes the second safe:
//
//   - With a lock file, an environment is open at most once per process,
//     under any name; a second open fails with EBUSY before it touches the
//     lock file.
//   - Without one (NoLock), any number of handles may open a data file.
//     They share one in-memory region (MemLock): the writer lock that
//     serialises their write transactions, the last committed txnid and the
//     reader table in which their read transactions pin snapshots, so a
//     writer of any handle keeps the pages of readers of every handle (C
//     coordinates nothing there; AGENTS.md rule 2a). It exists while one of
//     them is open, that is until the teardown of the last (Env.Close, or
//     the end of the last transaction that outlived it).
//   - A handle with a lock file and a NoLock handle never share a file
//     (their readers would be invisible to each other's writers): whichever
//     comes second gets EBUSY. So does a read-only handle without a lock
//     file and without NoLock (a read-only file system), which has a table
//     of its own.
//
// An environment is its data file, whatever path names it: directory or
// MDB_NOSUBDIR mode, symlinks, hard links, bind mounts. It is claimed by the
// data file's resolved path (it may not exist yet) before anything is
// opened, and by its device and inode once it exists (again from the opened
// descriptor), which happens before the lock file is touched; the file's
// claim holds the shared region. rlmdb claims the path given (the directory,
// or the file with MDB_NOSUBDIR), so for it the two modes are two paths of
// one file, which its file claim then recognises.
//
// The lock file is claimed by device and inode as well, once it is open and
// before any record lock is taken on it (lockfile.OpenClaimed), so that a
// lock file reachable under another name is refused too. Setting up a lock
// file this process holds again would be the worst outcome: record locks
// never conflict within one process, so the exclusive lock would be granted
// and the table re-initialised under the readers of the open handle (on
// Windows locks belong to the handle; the claims apply there as well).
//
// A refused open must not close a descriptor of a lock file in use either:
// closing any descriptor of a file drops the process's record locks on it.
// The registry keeps such descriptors open ("parked") until the handle that
// holds the lock file is gone, and Dispose does the same for a file opened by
// name for another purpose (SniffFormat). Descriptors of any other file are
// closed.

package engine

import (
	"os"
	"slices"
	"sync"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// regEntry is how a path or a data file is open in this process.
type regEntry struct {
	// handles is the number of NoLock handles that share it; 0 for the
	// one handle of an exclusive claim.
	handles int
	// shared is the region the NoLock handles of a data file share (file
	// claims only; made by the first handle that has the file open).
	shared *MemLock
	// lock: the file is the lock file of one handle, which holds (or is
	// about to take) record locks on it, so no other descriptor of the file
	// may be closed.
	lock bool
}

var (
	registryMu sync.Mutex
	regPaths   = map[string]*regEntry{}
	regFiles   = map[sys.FileID]*regEntry{}
	// regParked holds the descriptors of lock files in use that a refused
	// open or Dispose could not close, until the file's claim is given back
	// (after the handle closed its own descriptor).
	regParked = map[sys.FileID][]*os.File{}
	// regLeaked holds descriptors whose identity could not be read: they
	// may be of a lock file in use, so they stay open for good.
	regLeaked []*os.File
)

// claim claims key in m, exclusively or shared with other shared claims.
func claim[K comparable](m map[K]*regEntry, key K, shared bool) (*regEntry, error) {
	e, ok := m[key]
	switch {
	case !ok:
		e = &regEntry{}
		if shared {
			e.handles = 1
		}
		m[key] = e
		return e, nil
	case shared && e.handles > 0:
		e.handles++
		return e, nil
	}
	return nil, syscall.EBUSY
}

// release gives back one claim of key, forgetting it with the last one.
func release[K comparable](m map[K]*regEntry, key K) {
	if e, ok := m[key]; ok {
		if e.handles > 1 {
			e.handles--
			return
		}
		delete(m, key)
	}
}

// parkOrClose gets rid of f, a descriptor of the file id that the caller
// may not keep: parks it if the file is a lock file in use, else closes it.
// Either happens with registryMu held, so the file cannot become a lock file
// in use (which takes a claim first) before it is closed.
func parkOrClose(id sys.FileID, f *os.File) {
	if e, ok := regFiles[id]; ok && e.lock {
		regParked[id] = append(regParked[id], f)
		return
	}
	f.Close()
}

// identify is the identity of f. If it cannot be read, f is kept open for
// good rather than closed: it may be a lock file in use.
func identify(f *os.File) (sys.FileID, error) {
	id, err := sys.FileIDOf(f)
	if err != nil {
		registryMu.Lock()
		regLeaked = append(regLeaked, f)
		registryMu.Unlock()
	}
	return id, err
}

// Dispose closes f, which the caller opened by name, unless it is the lock
// file of an environment open in this process: closing it would drop that
// environment's record locks, so it is parked until the environment is gone.
func Dispose(f *os.File) {
	id, err := identify(f)
	if err != nil {
		return
	}
	registryMu.Lock()
	parkOrClose(id, f)
	registryMu.Unlock()
}

// envID is what an environment claimed.
type envID struct {
	path string
	// files are the data file's claims: normally one, a second one if its
	// name led to another file when it was opened again.
	files   []sys.FileID
	lock    sys.FileID
	hasLock bool
	shared  bool
}

// LockClaim is an open's way to claim its lock file in the registry before
// it locks it (lockfile.FileClaim): OpenLockFunc gets one. ClaimLockFile
// fails with EBUSY if the process has the file open in any role, the
// descriptor then being closed, or parked if it is a lock file in use.
// ReleaseLockFile gives the claim back, for an open that closed the lock file
// to start again.
type LockClaim interface {
	ClaimLockFile(f *os.File) error
	ReleaseLockFile()
}

// ClaimLockFile implements LockClaim.
func (id *envID) ClaimLockFile(f *os.File) error {
	file, err := identify(f)
	if err != nil {
		return err
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := regFiles[file]; ok {
		parkOrClose(file, f)
		return syscall.EBUSY
	}
	regFiles[file] = &regEntry{lock: true}
	id.lock, id.hasLock = file, true
	return nil
}

// ReleaseLockFile implements LockClaim.
func (id *envID) ReleaseLockFile() {
	if !id.hasLock {
		return
	}
	registryMu.Lock()
	releaseFile(id.lock)
	registryMu.Unlock()
	id.hasLock = false
}

// releaseFile gives back one claim of file, and closes what refused opens
// left parked once nobody has the file open any more (a lock file's handle
// closed its descriptor before it got here).
func releaseFile(file sys.FileID) {
	release(regFiles, file)
	if _, ok := regFiles[file]; ok {
		return
	}
	for _, f := range regParked[file] {
		f.Close()
	}
	delete(regParked, file)
}

// register claims the data file of the environment at path, before
// anything of the environment is opened, shared for a NoLock open: EBUSY
// if this process has it open with a lock file, or at all and this open
// would use a lock file.
func register(path string, noSubdir, shared bool) (envID, error) {
	data, _ := fileNames(path, noSubdir)
	p, err := canonicalPath(data)
	if err != nil {
		return envID{}, err
	}
	id := envID{path: p, shared: shared}
	f, statErr := sys.StatFileID(p)
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, err := claim(regPaths, id.path, shared); err != nil {
		return envID{}, err
	}
	if statErr == nil {
		if _, err := claim(regFiles, f, shared); err != nil {
			release(regPaths, id.path)
			return envID{}, err
		}
		id.files = append(id.files, f)
	}
	return id, nil
}

// registerFile claims the data file f once it is open, if register could
// not (it did not exist yet, or another file took its name since), and
// returns the region its NoLock handles share (nil for an exclusive claim).
// On EBUSY the descriptor is closed, or parked if it is a lock file in use:
// it is not the caller's any more.
func registerFile(id *envID, f *os.File) (*MemLock, error) {
	file, err := identify(f)
	if err != nil {
		return nil, err
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if !slices.Contains(id.files, file) {
		if _, err := claim(regFiles, file, id.shared); err != nil {
			parkOrClose(file, f)
			return nil, err
		}
		id.files = append(id.files, file)
	}
	if !id.shared {
		return nil, nil
	}
	e := regFiles[file]
	if e.shared == nil {
		e.shared = NewMemLock(0)
	}
	return e.shared, nil
}

// unregister gives back the claims of id.
func unregister(id envID) {
	registryMu.Lock()
	release(regPaths, id.path)
	for _, file := range id.files {
		releaseFile(file)
	}
	if id.hasLock {
		releaseFile(id.lock)
	}
	registryMu.Unlock()
}
