// The lock-file platform of macOS C builds: MDB_USE_POSIX_SEM (mdb.c lines
// 143-149, 0.9.36), named semaphores, so the layout does not depend on
// pthread_mutex_t.

package lockfile

// hostPlatform: mdb_hash_t (unsigned long long) is 8-byte aligned, pid_t
// is 4 bytes and pthread_t (a pointer) 8, on arm64 and x86-64.
var hostPlatform = platform{
	namedSemaphores: true,
	hashAlign:       8,
	pidSize:         4,
	pthreadSize:     8,
}
