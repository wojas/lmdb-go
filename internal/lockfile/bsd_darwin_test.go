// The lock files of OpenBSD, NetBSD and DragonFly on macOS, whose kernel
// has the same named and SysV semaphores: the Region tests also run
// NetBSD's version 1 layout (named semaphores with 14-character names) and
// the version 2 layout of all three (a SysV semaphore set), so their lock
// code runs on a real kernel, as rlmdb's sysv tests do on macOS. SysV
// needs a cgo build (sysv_nocgo_darwin.go).

package lockfile

import "syscall"

func init() {
	v1, v2 := netBSD.layouts()
	testLayouts = append(testLayouts, v1)
	// semctl of an id that does not exist: EINVAL with SysV calls, ENOTSUP
	// without (no cgo).
	if sysvRemove(-1) != syscall.ENOTSUP {
		testLayouts = append(testLayouts, v2)
	}
}
