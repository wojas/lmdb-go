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

func sysvOp(semid int32, num uint16, op int16) syscall.Errno {
	return syscall.Errno(C.lmdbgo_semop(C.int(semid), C.ushort(num), C.short(op)))
}

func sysvRemove(semid int32) syscall.Errno { return syscall.Errno(C.lmdbgo_semrmid(C.int(semid))) }
