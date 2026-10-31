// The process-wide registry of open environments (rlmdb core/registry.rs,
// as of its commit d3d9239).
//
// C lets a process open one environment any number of times, but lmdb.h
// forbids it with a lock file: POSIX record locks belong to the process,
// so closing any descriptor of the lock file drops every lock the process
// holds on it, the other handle's too. Without a lock file (MDB_NOLOCK)
// there are no record locks to lose, and programs do open one environment
// several times (for example once per embedded interpreter). The registry
// enforces the first and makes the second safe:
//
//   - With a lock file, an environment is open at most once per process; a
//     second open fails with EBUSY before it touches the lock file.
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
// MDB_NOSUBDIR mode, symlinks, hard links. It is claimed by the data file's
// resolved path (it may not exist yet) before anything is opened, and by
// its device and inode once it exists (again from the opened descriptor);
// the file's claim holds the shared region. rlmdb claims the path given
// (the directory, or the file with MDB_NOSUBDIR), so for it the two modes
// are two paths of one file, which its file claim then recognises.

package engine

import (
	"os"
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
}

var (
	registryMu sync.Mutex
	regPaths   = map[string]*regEntry{}
	regFiles   = map[sys.FileID]*regEntry{}
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

// envID is what an environment claimed.
type envID struct {
	path    string
	file    sys.FileID
	hasFile bool
	shared  bool
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
		id.file, id.hasFile = f, true
	}
	return id, nil
}

// registerFile claims the data file f once it is open, if register could
// not (it did not exist yet, or another file took its name since), and
// returns the region its NoLock handles share (nil for an exclusive
// claim).
func registerFile(id *envID, f *os.File) (*MemLock, error) {
	file, err := sys.FileIDOf(f)
	if err != nil {
		return nil, err
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if !id.hasFile || id.file != file {
		if _, err := claim(regFiles, file, id.shared); err != nil {
			return nil, err
		}
		if id.hasFile {
			release(regFiles, id.file)
		}
		id.file, id.hasFile = file, true
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
	if id.hasFile {
		release(regFiles, id.file)
	}
	registryMu.Unlock()
}
