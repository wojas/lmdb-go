// Package lockfile implements LMDB's lock file (lock.mdb, or <path>-lock
// with MDB_NOSUBDIR): the shared reader table, the last committed txnid,
// and the writer and reader-table locks that C, Rust (rlmdb) and Go
// processes share. It is the Go form of rlmdb's format/lockfile.rs,
// os/shared_lock.rs and core/reader.rs, and of the lock-file code of mdb.c
// (mdb_env_setup_locks, mdb_env_excl_lock, mdb_env_share_locks,
// mdb_reader_pid, the reader-slot part of mdb_txn_renew0, mdb_find_oldest,
// mdb_reader_list, mdb_reader_check0, mdb_mutex_failed, mdb_env_close0).
//
// Readers lock nothing while they read. They advertise the snapshot they
// use in a slot of the shared reader table, so that writers know which
// freed pages must not be reused yet (FindOldest). Everything follows C's
// protocol exactly:
//
//   - First opener: an exclusive fcntl lock on byte 0 means nobody else has
//     the environment open, so the opener (re)initialises the table and the
//     locks, then downgrades to a shared lock once the environment is open
//     (Region.Downgrade). Later openers wait for the shared lock and
//     validate the table.
//   - Versions: the lock file has the version of the environment's data
//     format, 1 for LMDB 0.9 and 2 for LMDB 1.0 (LayoutFor), each with a
//     per-platform layout. A table of the other version is
//     MDB_VERSION_MISMATCH while someone else holds the file, and is
//     re-initialised by an exclusive opener, as C does; the data format
//     must therefore be decided before the lock file is opened.
//   - Reader liveness: a process that has taken a reader slot holds an
//     exclusive fcntl lock on byte pid of the lock file; the reader check
//     probes those locks to clear slots of dead processes.
//   - Slots are claimed under the reader-table lock in mdb_txn_renew0's
//     publication order (reset, publish in mti_numreaders, claim with the
//     pid); a process reuses its slots without the lock.
//   - The writer lock is held for a whole write transaction. On Linux both
//     locks are process-shared robust pthread mutexes inside the lock file,
//     and the owner dying while holding one is repaired as mdb_mutex_failed
//     does; on macOS they are POSIX named semaphores (not robust, as in C),
//     whose names are stored in the file (version 1) or derived from a hash
//     stored there (version 2), and read, never recomputed.
//
// The two locks are libc objects and are taken through cgo, in one file
// pair (shared_cgo.go, shared_nocgo.go). Built without cgo, the package
// compiles but Open returns ErrNoSharedLocks; everything else here (the
// mapping, the record locks, the table) is Go. The size of pthread_mutex_t,
// which decides the Linux layouts, also comes from cgo, so glibc and musl
// builds each get their libc's layout: interoperation is with C processes
// of the same libc (on aarch64, glibc and musl lock files differ).
//
// A process must not open one environment twice, in C or here: closing any
// descriptor of the lock file drops all of the process's record locks on
// it. This package never opens the lock file a second time.
package lockfile
