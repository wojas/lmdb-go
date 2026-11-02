// The lock-file platform of FreeBSD C builds: FreeBSD 11 and later take
// MDB_USE_POSIX_MUTEX with MDB_USE_ROBUST (mdb.c 0.9.37 lines 135-142,
// 1.0 lines 184-191), Linux's robust process-shared mutexes in the lock
// file; rlmdb format/lockfile.rs (the FreeBSD platform).

package lockfile

// hostPlatform: the mutexes' size and alignment come from the C library
// through cgo (shared_cgo.go), as on Linux. FreeBSD's pthread_mutex_t is a
// pointer, 8 bytes: with PTHREAD_PROCESS_SHARED the mutex itself lives in a
// kernel object keyed by its address in the shared mapping, so both
// lock-file versions put the reader table at 128. pid_t is 4 bytes and
// pthread_t (a pointer) 8.
var hostPlatform = platform{
	mutexSize:   mutexSize,
	mutexAlign:  mutexAlign,
	pidSize:     4,
	pthreadSize: 8,
}
