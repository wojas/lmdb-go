// The lock-file platform of Linux C builds: MDB_USE_POSIX_MUTEX with
// robust mutexes (mdb.c lines 120-160 and 280-316, 0.9.37).

package lockfile

// hostPlatform: the mutexes' size and alignment come from the C library
// through cgo (shared_cgo.go): glibc's pthread_mutex_t is 40 bytes on
// x86-64 and 48 on aarch64, musl's is 40. pid_t is 4 bytes and pthread_t 8
// on every 64-bit Linux; 32-bit targets are not supported (the data format
// depends on sizeof(size_t), as in rlmdb).
var hostPlatform = platform{
	mutexSize:   mutexSize,
	mutexAlign:  mutexAlign,
	pidSize:     4,
	pthreadSize: 8,
}
