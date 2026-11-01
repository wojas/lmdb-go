//go:build unix

// Making and removing the lock-file mapping on Unix: mmap(PROT_READ |
// PROT_WRITE, MAP_SHARED) in mdb_env_setup_locks, munmap in
// mdb_env_close0.

package lockfile

import "syscall"

// mapLockFile maps the first size bytes of the open file fd read/write and
// shared.
func mapLockFile(fd uintptr, size int) (*lockMap, error) {
	if size <= 0 {
		return nil, syscall.EINVAL
	}
	mem, err := syscall.Mmap(int(fd), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return &lockMap{mem: mem}, nil
}

// unmapMem removes a mapping made by mapLockFile.
func unmapMem(mem []byte) error { return syscall.Munmap(mem) }
