//go:build !cgo

// macOS's named semaphores without cgo: the semaphore calls listed in
// sem.go (sem_open, sem_wait, sem_trywait, sem_post, sem_close,
// sem_unlink, and pthread_self for mr_tid), made in libSystem directly.
// Builds with cgo use sem_cgo.go instead.
//
// # Why
//
// The shared locks must be the same objects C LMDB uses, so they must be
// taken through the C library. On Linux that needs cgo (the pthread
// mutexes' size and protocol are glibc's or musl's). On macOS it does not:
// every Go binary there is dynamically linked against libSystem, with or
// without cgo, because Go makes all its system calls through libSystem
// (Apple does not keep the system call interface stable). Package syscall
// and golang.org/x/sys/unix call libSystem functions with a small,
// documented mechanism; this file uses the same one for the semaphores.
// Builds without cgo (CGO_ENABLED=0, which is also Go's default when
// cross-compiling to macOS) therefore share environments with C fully.
//
// Not github.com/ebitengine/purego, which makes such calls in general: it
// would be this module's first dependency, for seven fixed functions, and
// it too depends on a runtime linkname (runtime.cgocall), so it would not
// remove the risk described under "Linkname" below.
//
// # How a call works
//
//  1. //go:cgo_import_dynamic libc_sem_wait sem_wait "/usr/lib/libSystem.B.dylib"
//     (below) makes the linker import sem_wait from libSystem and lets
//     assembly refer to it as libc_sem_wait. The directive is allowed
//     outside cgo-generated files for exactly this use (x/sys relies on it).
//  2. sem_libsystem_darwin_{arm64,amd64}.s defines a trampoline per
//     function, libc_sem_wait_trampoline<>, which jumps to libc_sem_wait,
//     and stores its address in libc_sem_wait_trampoline_addr (a DATA
//     directive, since Go code outside the runtime cannot take an assembly
//     function's address). The trampolines are ABI0 assembly entered with
//     the C calling convention; Go code never calls them directly.
//  3. Go code passes that address and the arguments to syscall_syscall
//     (syscall_syscall6 for four or more arguments): package syscall's
//     syscall and syscall6, reached with //go:linkname. They are
//     implemented by the runtime (runtime/sys_darwin.go): entersyscall (so
//     a sem_wait that blocks does not hold up other goroutines; the P is
//     handed off as for any blocking system call), a switch to the system
//     stack (libcCall), the arguments loaded into the C argument registers
//     (runtime·syscall6 in runtime/sys_darwin_{arm64,amd64}.s), a call to
//     the trampoline, then exitsyscall.
//  4. Errors: if the low 32 bits of the result are -1, runtime·syscall
//     (and syscall6) reads errno (__error()) on the same thread right after the call and
//     returns it as the third result; otherwise that result is 0. The
//     functions here returning int give -1 exactly on failure. sem_open
//     returns a pointer, failing with SEM_FAILED ((sem_t *)-1, all 64 bits
//     set), so its failure is decided on the full result, and errno is
//     taken only then (a successful handle whose low half happened to be
//     -1 would come with a stale errno, which is then ignored).
//
// A name is passed as a pointer to a NUL-terminated copy on the heap
// (syscall.BytePtrFromString). Converting it to uintptr inside the call's
// argument list keeps it alive for the call: the compiler treats uintptr
// arguments of functions without a Go body as possibly pointers that must
// stay live (cmd/compile, escape analysis; golang.org/issue/13372). The
// heap does not move, and the runtime does not move a goroutine's stack
// while it is in a system call.
//
// # sem_open is variadic
//
// sem_t *sem_open(const char *name, int oflag, ...) takes mode (mode_t,
// promoted to int) and value (unsigned int) as variadic arguments when
// oflag has O_CREAT. Variadic arguments are passed differently from named
// ones on both architectures, and a trampoline that ignores this creates a
// semaphore with a garbage value or mode:
//
//   - darwin/arm64: Apple's ABI ("Writing ARM64 code for Apple platforms",
//     https://developer.apple.com/documentation/xcode/writing-arm64-code-for-apple-platforms)
//     departs from the standard AArch64 procedure call standard here: all
//     variadic arguments are passed on the stack, each in its own 8-byte
//     slot, starting at the stack pointer at the call; none in registers.
//     runtime·syscall6 copies only its fourth argument to 0(RSP) (for
//     openat's single variadic mode) and keeps its argument-block pointer
//     at 8(RSP). A plain jump, as for the other functions, makes sem_open
//     read mode = value and value = the low half of that pointer; it then
//     fails with EINVAL (value above SEM_VALUE_MAX), which
//     TestSemCreateValue catches. libc_sem_open_trampoline<> therefore
//     builds the two slots itself (mode at 0(RSP), value at 8(RSP)) and
//     calls sem_open, as the runtime's own open_trampoline does for open's
//     mode; the .s file says how it keeps the saved frame pointer intact.
//   - darwin/amd64: the System V ABI passes variadic integer arguments in
//     the same registers as named ones (mode in DX, value in CX, already
//     loaded by runtime·syscall6), but the caller of a variadic function
//     sets AL to an upper bound of the number of vector registers used for
//     arguments (System V AMD64 psABI, "Variable Argument Lists"): 0 here.
//     runtime·syscall6 clears AX today; libc_sem_open_trampoline<> clears
//     it itself so as not to depend on that.
//
// sem_open(name, 0), without O_CREAT, reads no variadic argument, so
// semOpen uses the same trampoline with mode and value 0.
//
// # EINTR
//
// semWait makes one sem_wait call and returns EINTR as it comes;
// sharedMutex.lock retries, as mdb_sem_wait does. macOS's sem_wait fails
// with EINTR whenever a signal handler runs on the waiting thread, even if
// the handler was installed with SA_RESTART (as all of Go's are).
//
// # Handles
//
// Handles are uintptr throughout and never become a Go pointer: macOS's
// sem_t * values are descriptor numbers, which the garbage collector would
// crash on as invalid pointers (sem.go).
//
// # Linkname
//
// Since Go 1.23 the linker rejects a //go:linkname reference to a symbol
// of the standard library unless the symbol's definition is marked with a
// //go:linkname of its own ("push"), independently of the go directive in
// go.mod. syscall.syscall and syscall.syscall6 are so marked in
// runtime/sys_darwin.go, with "golang.org/x/sys linknames syscall.syscall
// ... Do not remove or change the type signature" (go.dev/issue/67401):
// they exist for golang.org/x/sys/unix, so removing them would break x/sys
// on macOS. Only those two are used here. Should a Go release restrict
// them anyway, CGO_ENABLED=0 macOS builds fail to link ("invalid
// reference to syscall.syscall"), and cgo builds (sem_cgo.go), the
// default, are unaffected.
//
// # Tests
//
// sem_darwin_test.go, run in both builds (CGO_ENABLED=0 tests this file):
// TestSemCreateValue (the value reaches sem_open: exactly value
// sem_trywaits succeed, the next fails with EAGAIN; SEM_VALUE_MAX+1 is
// EINVAL), TestCreateSemaphoreValueOne (LMDB's semaphores start at 1),
// TestSemCreateMode (the mode reaches sem_open: opening again fails with
// EACCES exactly for modes without read permission), TestSemErrors
// (EEXIST, ENOENT, ENAMETOOLONG, EBADF and EINVAL through errno),
// TestSemSharedByName, TestSemLockBlocks, TestSemWaitEINTR (a signal makes
// one sem_wait fail with EINTR; lock retries until the post),
// TestSemHandleZero (a handle of 0 works), TestThreadID. region_test.go
// (Go processes) and tests/lockfile (Go and C processes, both lock-file
// versions) test the locks across processes.

package lockfile

import (
	"syscall"
	"unsafe"
)

// syscall_syscall and syscall_syscall6 call the C function at fn with the
// C calling convention and up to three or six integer arguments, on the
// system stack, as a system call for the scheduler. err is errno if the low
// 32 bits of r1 are -1, else 0. Implemented in the runtime
// (runtime/sys_darwin.go) for package syscall and golang.org/x/sys/unix.
//
//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// The trampolines' addresses, set by the assembly files.
var (
	libc_sem_open_trampoline_addr     uintptr
	libc_sem_wait_trampoline_addr     uintptr
	libc_sem_trywait_trampoline_addr  uintptr
	libc_sem_post_trampoline_addr     uintptr
	libc_sem_close_trampoline_addr    uintptr
	libc_sem_unlink_trampoline_addr   uintptr
	libc_pthread_self_trampoline_addr uintptr
)

//go:cgo_import_dynamic libc_sem_open sem_open "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_sem_wait sem_wait "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_sem_trywait sem_trywait "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_sem_post sem_post "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_sem_close sem_close "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_sem_unlink sem_unlink "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_pthread_self pthread_self "/usr/lib/libSystem.B.dylib"

// semFailed is SEM_FAILED, (sem_t *)-1.
const semFailed = ^uintptr(0)

// semOpenCall is sem_open(name, oflag, mode, value). mode and value are
// read by sem_open only with O_CREAT; the trampoline passes them as
// variadic arguments (see the file comment).
func semOpenCall(name string, oflag int, mode, value uint32) (uintptr, syscall.Errno) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return 0, syscall.EINVAL
	}
	h, _, e := syscall_syscall6(libc_sem_open_trampoline_addr, uintptr(unsafe.Pointer(p)),
		uintptr(oflag), uintptr(mode), uintptr(value), 0, 0)
	if h == semFailed {
		if e == 0 {
			e = syscall.EINVAL // not expected: SEM_FAILED always sets errno
		}
		return 0, e
	}
	return h, 0
}

// semImpl names this implementation, for test logs.
const semImpl = "libSystem"

func semCreate(name string, mode, value uint32) (uintptr, syscall.Errno) {
	return semOpenCall(name, syscall.O_CREAT|syscall.O_EXCL, mode, value)
}

func semOpen(name string) (uintptr, syscall.Errno) { return semOpenCall(name, 0, 0, 0) }

// semCall calls a one-argument semaphore function returning 0 or -1.
func semCall(fn, a uintptr) syscall.Errno {
	_, _, e := syscall_syscall(fn, a, 0, 0)
	return e
}

func semWait(h uintptr) syscall.Errno    { return semCall(libc_sem_wait_trampoline_addr, h) }
func semTryWait(h uintptr) syscall.Errno { return semCall(libc_sem_trywait_trampoline_addr, h) }
func semPost(h uintptr) syscall.Errno    { return semCall(libc_sem_post_trampoline_addr, h) }
func semClose(h uintptr) syscall.Errno   { return semCall(libc_sem_close_trampoline_addr, h) }

func semUnlink(name string) syscall.Errno {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return syscall.EINVAL
	}
	_, _, e := syscall_syscall(libc_sem_unlink_trampoline_addr, uintptr(unsafe.Pointer(p)), 0, 0)
	return e
}

// pthreadSelf is pthread_self(). It cannot fail; the errno result, which
// the runtime fills in if the low half of the pointer happens to be -1, is
// meaningless and ignored.
func pthreadSelf() uint64 {
	r, _, _ := syscall_syscall(libc_pthread_self_trampoline_addr, 0, 0, 0)
	return uint64(r)
}
