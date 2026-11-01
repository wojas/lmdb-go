/* layout_probe.c - the lock-file layout of a C LMDB build, readable without
 * running it.
 *
 * It includes C LMDB's mdb.c (0.9 or 1.0: the include path picks it), as
 * rlmdb's interop/c/layout_dump.c does, and stores the lock file's offsets,
 * sizes and MDB_LOCK_FORMAT in the array lmdbgo_layout. Compiled to an
 * object file for another system (scripts/bsd-cc-check.sh: zig cc -target
 * ..., or clang with a sysroot), the values are those of the C build there,
 * with that system's headers; TestLayoutsMatchCompiledC reads them from the
 * object file and compares them with the layouts internal/lockfile computes
 * for that platform. Nothing needs to run on the target. Entry i is
 * described by probeFields[i] in layout_probe_test.go; keep the two in
 * step.
 */
#include "mdb.c"

#include <stddef.h>
#include <sys/types.h>

#define LMDB10	(MDB_VERSION_MAJOR >= 1)

#if defined(MDB_USE_POSIX_SEM)
#define LOCK_MODE	1
#elif defined(MDB_USE_POSIX_MUTEX)
#define LOCK_MODE	2
#elif defined(MDB_USE_SYSV_SEM)
#define LOCK_MODE	3
#elif defined(_WIN32)
#define LOCK_MODE	4
#endif

#define TXN(f)		offsetof(MDB_txninfo, f)
#define TXNSZ(f)	sizeof(((MDB_txninfo *)0)->f)

/* The reader lock field and the writer lock field (none: all ones). */
#if LMDB10 && (defined(_WIN32) || defined(MDB_USE_POSIX_SEM))
#define RLOCK		TXN(mt1.mtb.mtb_mutexid)
#define RLOCK_SIZE	TXNSZ(mt1.mtb.mtb_mutexid)
#define WLOCK		(~0ULL)
#elif LMDB10 && defined(MDB_USE_SYSV_SEM)
#define RLOCK		TXN(mt1.mtb.mtb_semid)
#define RLOCK_SIZE	(TXN(mt1.mtb.mtb_rlocked) + TXNSZ(mt1.mtb.mtb_rlocked) - TXN(mt1.mtb.mtb_semid))
#define WLOCK		TXN(mt2.mt2_wlocked)
#elif defined(_WIN32) || defined(MDB_USE_POSIX_SEM)
#define RLOCK		TXN(mt1.mtb.mtb_rmname)
#define RLOCK_SIZE	TXNSZ(mt1.mtb.mtb_rmname)
#define WLOCK		TXN(mt2.mt2_wmname)
#else
#define RLOCK		TXN(mt1.mtb.mtb_rmutex)
#define RLOCK_SIZE	TXNSZ(mt1.mtb.mtb_rmutex)
#define WLOCK		TXN(mt2.mt2_wmutex)
#endif

#ifdef MDB_SHORT_SEMNAMES
#define SHORT_SEMNAMES	1
#else
#define SHORT_SEMNAMES	0
#endif

const unsigned long long lmdbgo_layout[] = {
	1,	/* version of this table */
	LOCK_MODE,
	MDB_LOCK_VERSION,
	MDB_LOCK_FORMAT,
	TXN(mt1.mtb.mtb_txnid),
	TXN(mt1.mtb.mtb_numreaders),
	RLOCK,
	RLOCK_SIZE,
	WLOCK,
	sizeof(MDB_txbody),
	TXNSZ(mt1),
	TXN(mti_readers),
	sizeof(MDB_reader),
	offsetof(MDB_reader, mru.mrx.mrb_pid),
	offsetof(MDB_reader, mru.mrx.mrb_tid),
	sizeof(((MDB_reader *)0)->mru.mrx.mrb_tid),
	sizeof(dev_t),
	SHORT_SEMNAMES,
	MDB_PIDLOCK,
};
