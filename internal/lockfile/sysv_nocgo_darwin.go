//go:build !cgo

// macOS builds without cgo have no SysV semaphore calls: no macOS lock file
// uses the set (sysv.go is built on macOS for its tests only), so these
// fail with ENOTSUP and the tests that need them skip.

package lockfile

import "syscall"

func sysvGet(int64, uint32) (int32, syscall.Errno) { return 0, syscall.ENOTSUP }
func sysvSetAll(int32) syscall.Errno               { return syscall.ENOTSUP }
func sysvCheck(int32) syscall.Errno                { return syscall.ENOTSUP }
func sysvHasThird(int32) (bool, syscall.Errno)     { return false, syscall.ENOTSUP }
func sysvOp(int32, uint16, int16) syscall.Errno    { return syscall.ENOTSUP }
func sysvRemove(int32) syscall.Errno               { return syscall.ENOTSUP }
