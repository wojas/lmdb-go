//go:build (darwin || openbsd || netbsd || dragonfly) && cgo

// The SysV semaphore calls of sysv.go through cgo: semget, semctl and semop
// as mdb.c 1.0 makes them with MDB_USE_SYSV_SEM. They are in the C library
// of every BSD; NetBSD's <sys/sem.h> renames semctl (__semctl50), which
// compiling the calls in C takes care of. Built on macOS too, for the
// tests (sysv.go).

package lockfile

/*
#include <errno.h>
#include <sys/types.h>
#include <sys/ipc.h>
#include <sys/sem.h>

// semctl's fourth argument, union semun, which not every system declares
// (NetBSD does not; mdb.c 1.0 declares it there itself). semctl is
// variadic and takes the union by value; one with the same members is
// passed alike.
union lmdbgo_semun {
	int val;
	struct semid_ds *buf;
	unsigned short *array;
};

static int lmdbgo_semget(long long key, unsigned int mode, int *semid) {
	int id = semget((key_t)key, 2, (int)(mode & 0777) | IPC_CREAT);
	if (id < 0) return errno;
	*semid = id;
	return 0;
}
static int lmdbgo_semsetall(int semid) {
	unsigned short vals[2] = {1, 1};
	union lmdbgo_semun arg;
	arg.array = vals;
	return semctl(semid, 0, SETALL, arg) < 0 ? errno : 0;
}
static int lmdbgo_semcheck(int semid) {
	struct semid_ds buf;
	union lmdbgo_semun arg;
	arg.buf = &buf;
	// check for read access, then for write access, as mdb.c does
	if (semctl(semid, 0, IPC_STAT, arg) < 0) return errno;
	if (semctl(semid, 0, IPC_SET, arg) < 0) return errno;
	return 0;
}
// Whether semaphore 2 of the set exists: GETVAL takes no further argument
// and fails with EINVAL for a number the set does not have.
static int lmdbgo_semhasthird(int semid, int *third) {
	if (semctl(semid, 2, GETVAL) >= 0) {
		*third = 1;
		return 0;
	}
	*third = 0;
	return errno == EINVAL ? 0 : errno;
}
static int lmdbgo_semop(int semid, unsigned short num, short op) {
	struct sembuf sb;
	sb.sem_num = num;
	sb.sem_op = op;
	sb.sem_flg = SEM_UNDO;
	return semop(semid, &sb, 1) < 0 ? errno : 0;
}
static int lmdbgo_semrmid(int semid) {
	return semctl(semid, 0, IPC_RMID) < 0 ? errno : 0;
}
// For the tests: a new set of nsems semaphores at key, its values.
static int lmdbgo_semnew(long long key, int nsems, int *semid) {
	int id = semget((key_t)key, nsems, IPC_CREAT | IPC_EXCL | 0600);
	if (id < 0) return errno;
	*semid = id;
	return 0;
}
static int lmdbgo_semsetvals(int semid, unsigned short *vals) {
	union lmdbgo_semun arg;
	arg.array = vals;
	return semctl(semid, 0, SETALL, arg) < 0 ? errno : 0;
}
static int lmdbgo_semgetval(int semid, int num, int *val) {
	int v = semctl(semid, num, GETVAL);
	if (v < 0) return errno;
	*val = v;
	return 0;
}
*/
import "C"

import "syscall"

func sysvGet(key int64, mode uint32) (int32, syscall.Errno) {
	var id C.int
	if rc := C.lmdbgo_semget(C.longlong(key), C.uint(mode), &id); rc != 0 {
		return 0, syscall.Errno(rc)
	}
	return int32(id), 0
}

func sysvSetAll(semid int32) syscall.Errno {
	return syscall.Errno(C.lmdbgo_semsetall(C.int(semid)))
}

func sysvCheck(semid int32) syscall.Errno { return syscall.Errno(C.lmdbgo_semcheck(C.int(semid))) }

func sysvHasThird(semid int32) (bool, syscall.Errno) {
	var third C.int
	if rc := C.lmdbgo_semhasthird(C.int(semid), &third); rc != 0 {
		return false, syscall.Errno(rc)
	}
	return third != 0, 0
}

func sysvOp(semid int32, num uint16, op int16) syscall.Errno {
	return syscall.Errno(C.lmdbgo_semop(C.int(semid), C.ushort(num), C.short(op)))
}

func sysvRemove(semid int32) syscall.Errno { return syscall.Errno(C.lmdbgo_semrmid(C.int(semid))) }

// sysvNewSet, sysvSetValues and sysvValue are for the tests (cgo cannot be
// used in test files): another program's set at a key, and what it holds.
func sysvNewSet(key int64, nsems int) (int32, syscall.Errno) {
	var id C.int
	if rc := C.lmdbgo_semnew(C.longlong(key), C.int(nsems), &id); rc != 0 {
		return 0, syscall.Errno(rc)
	}
	return int32(id), 0
}

func sysvSetValues(semid int32, vals []uint16) syscall.Errno {
	cv := make([]C.ushort, len(vals))
	for i, v := range vals {
		cv[i] = C.ushort(v)
	}
	return syscall.Errno(C.lmdbgo_semsetvals(C.int(semid), &cv[0]))
}

func sysvValue(semid int32, num int) (int, syscall.Errno) {
	var v C.int
	if rc := C.lmdbgo_semgetval(C.int(semid), C.int(num), &v); rc != 0 {
		return 0, syscall.Errno(rc)
	}
	return int(v), 0
}
