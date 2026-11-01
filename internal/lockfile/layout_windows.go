// The lock-file platform of Windows C builds (_WIN32, mdb.c 0.9.36 lines
// 776-811 and 1.0 lines 923-955; rlmdb format/lockfile.rs, the Windows
// platform): two named kernel mutexes, kept as the macOS semaphores are
// (names in version 1, their hash in version 2, no mt2 in version 2), and
// no pid locks (MDB_PIDLOCK 0). MSVC and MinGW-w64 lay the structures out
// alike; x86-64 and arm64 have the same sizes.

package lockfile

// hostPlatform: mdb_hash_t (unsigned long long) is 8-byte aligned,
// MDB_PID_T is an int (4 bytes) and pthread_t is #defined to HANDLE, a
// pointer (8 bytes).
var hostPlatform = platform{
	windowsMutexes: true,
	hashAlign:      8,
	pidSize:        4,
	pthreadSize:    8,
}

const (
	// ReaderTID is the offset of mr_tid in a reader slot: MDB_THR_T is a
	// DWORD on Windows (GetCurrentThreadId()), right after the int pid.
	ReaderTID = 12
	// ReaderTIDSize is the size of mr_tid: 4 (DWORD).
	ReaderTIDSize = 4
)
