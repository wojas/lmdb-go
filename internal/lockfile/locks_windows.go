// createLocks and attachLocks on Windows: the two named kernel mutexes of
// mdb_env_setup_locks's _WIN32 branch (0.9.37 lines 4964-4994; 1.0 lines
// 6064-6110 with mdb_env_mname_init); rlmdb os/windows.rs
// (SharedLocks::create, SharedLocks::attach).
//
//   - The exclusive opener creates them with CreateMutex and a NULL DACL
//     (C's mdb_all_sa: any account may open them); everyone else opens
//     them with OpenMutex(SYNCHRONIZE), enough to wait on and release them.
//   - Their names come from the identity of the lock file, struct { DWORD
//     volume; DWORD nhigh; DWORD nlow; } from GetFileInformationByHandle.
//     Version 1: "Global\MDBr" and "Global\MDBw" followed by mdb_hash_enc
//     of it, whose mdb_pack85 takes a 32-bit unsigned long on Windows
//     (semname.go); the names are stored in the lock file (mti_rmname,
//     mti_wmname) and read by the other openers. Version 2: the 64-bit hash
//     is stored (mti_mutexid), and every opener names the mutexes
//     "Global\MDB", 'r' or 'w', then up to ten base-85 digits of the stored
//     hash (MUTEXNAME). What is in the file is read, never recomputed.
//   - The kernel destroys a named mutex with its last handle, so there is
//     nothing to remove when an environment closes.

package lockfile

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// createLocks creates both mutexes for a lock file of layout l that this
// process holds exclusively (fd is its handle), and records in it what the
// other openers need to find them: the names (version 1) or the hash they
// are derived from (version 2). mode is not used.
func createLocks(m *lockMap, fd uintptr, mode os.FileMode, l *Layout) (*sharedLocks, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(fd), &info); err != nil {
		return nil, err
	}
	id := lockFileIDWin32(info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
	var rname, wname string
	switch {
	case l.Objects == MutexNames && l.WLock >= 0:
		rname, wname = mutexNamesV09(id)
		if len(rname)+1 > l.RLockSize {
			return nil, syscall.EINVAL
		}
		// sprintf: the name and its NUL; the rest of the field keeps
		// whatever was there, as in C.
		m.writeBytes(l.RLock, append([]byte(rname), 0))
		m.writeBytes(l.WLock, append([]byte(wname), 0))
	case l.Objects == MutexHash && l.RLockSize == 8:
		mutexID := hashVal(id, hashInit)
		m.storeUint64(l.RLock, mutexID)
		rname, wname = mutexNameV10(mutexID, 'r'), mutexNameV10(mutexID, 'w')
	default:
		return nil, syscall.EINVAL
	}
	r, err := createMutex(rname)
	if err != nil {
		return nil, err
	}
	w, err := createMutex(wname)
	if err != nil {
		r.close(false)
		return nil, err
	}
	return &sharedLocks{r: r, w: w}, nil
}

// attachLocks opens the mutexes another process created: named in the
// lock file (version 1), or named after the hash stored in it (version 2).
func attachLocks(m *lockMap, l *Layout) (*sharedLocks, error) {
	var rname, wname string
	switch {
	case l.Objects == MutexNames && l.WLock >= 0:
		var err error
		if rname, err = readName(m, l.RLock, l.RLockSize); err != nil {
			return nil, err
		}
		if wname, err = readName(m, l.WLock, l.RLockSize); err != nil {
			return nil, err
		}
	case l.Objects == MutexHash && l.RLockSize == 8:
		mutexID := m.loadUint64(l.RLock)
		rname, wname = mutexNameV10(mutexID, 'r'), mutexNameV10(mutexID, 'w')
	default:
		return nil, syscall.EINVAL
	}
	r, err := openMutex(rname)
	if err != nil {
		return nil, err
	}
	w, err := openMutex(wname)
	if err != nil {
		r.close(false)
		return nil, err
	}
	return &sharedLocks{r: r, w: w}, nil
}
