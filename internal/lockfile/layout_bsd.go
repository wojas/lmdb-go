//go:build openbsd || netbsd || dragonfly

// The lock-file platform of OpenBSD, NetBSD and DragonFly C builds, which
// C LMDB selects by defined(BSD) (mdb.c 0.9.37 lines 143-149, 1.0 lines
// 192-203): POSIX named semaphores for LMDB 0.9 (MDB_USE_POSIX_SEM, laid
// out as on macOS), a SysV semaphore set for LMDB 1.0 (MDB_USE_SYSV_SEM);
// rlmdb format/lockfile.rs (the platform of these three BSDs).

package lockfile

import "runtime"

// hostPlatform: version 1 semaphore names are 14 characters on NetBSD
// (MDB_SHORT_SEMNAMES, mdb.c 0.9.37 lines 4999-5011) and 15 elsewhere.
// pid_t is 4 bytes and pthread_t (a pointer) 8; mdb_hash_t, which only
// macOS's version 2 stores, would be 8-byte aligned.
var hostPlatform = platform{
	namedSemaphores: true,
	sysvV2:          true,
	shortSemNames:   runtime.GOOS == "netbsd",
	hashAlign:       8,
	pidSize:         4,
	pthreadSize:     8,
}
