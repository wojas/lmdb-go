//go:build !cgo

// Trampolines for sem_libsystem_darwin.go on darwin/amd64: each is entered
// from runtime·syscall or runtime·syscall6 (runtime/sys_darwin_amd64.s)
// with the C (System V AMD64) calling convention, the call's arguments in
// DI, SI, DX, CX, R8, R9 as the runtime loaded them, and the return address
// on the stack. Each trampoline's address is stored in the matching
// ·libc_*_trampoline_addr variable for the Go code. The layout follows
// golang.org/x/sys/unix's zsyscall_darwin_amd64.s.

#include "textflag.h"

// libc_sem_open_trampoline<>: sem_open(name, oflag, mode, value).
//
// In: DI = name, SI = oflag, DX = mode, CX = value. Variadic integer
// arguments use the same registers as named ones on x86-64, so all four
// are in place. A call to a variadic function must also set AL to an upper
// bound of the number of vector registers holding arguments (System V
// AMD64 psABI, "Variable Argument Lists"); there are none. runtime·syscall6
// clears AX before the call, but this does not rely on it: clear AX, then
// tail-jump, so sem_open returns (RAX = the sem_t * or SEM_FAILED) straight
// to runtime·syscall6.
TEXT libc_sem_open_trampoline<>(SB),NOSPLIT,$0-0
	XORL	AX, AX	// AL = 0: no vector registers used
	JMP	libc_sem_open(SB)
GLOBL	·libc_sem_open_trampoline_addr(SB), RODATA, $8
DATA	·libc_sem_open_trampoline_addr(SB)/8, $libc_sem_open_trampoline<>(SB)

// The other functions take named arguments only, already in place
// (DI = the handle or the name): a tail jump; the result is left in AX.

// libc_sem_wait_trampoline<>: sem_wait(sem). Blocks.
TEXT libc_sem_wait_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sem_wait(SB)
GLOBL	·libc_sem_wait_trampoline_addr(SB), RODATA, $8
DATA	·libc_sem_wait_trampoline_addr(SB)/8, $libc_sem_wait_trampoline<>(SB)

// libc_sem_trywait_trampoline<>: sem_trywait(sem).
TEXT libc_sem_trywait_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sem_trywait(SB)
GLOBL	·libc_sem_trywait_trampoline_addr(SB), RODATA, $8
DATA	·libc_sem_trywait_trampoline_addr(SB)/8, $libc_sem_trywait_trampoline<>(SB)

// libc_sem_post_trampoline<>: sem_post(sem).
TEXT libc_sem_post_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sem_post(SB)
GLOBL	·libc_sem_post_trampoline_addr(SB), RODATA, $8
DATA	·libc_sem_post_trampoline_addr(SB)/8, $libc_sem_post_trampoline<>(SB)

// libc_sem_close_trampoline<>: sem_close(sem).
TEXT libc_sem_close_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sem_close(SB)
GLOBL	·libc_sem_close_trampoline_addr(SB), RODATA, $8
DATA	·libc_sem_close_trampoline_addr(SB)/8, $libc_sem_close_trampoline<>(SB)

// libc_sem_unlink_trampoline<>: sem_unlink(name).
TEXT libc_sem_unlink_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sem_unlink(SB)
GLOBL	·libc_sem_unlink_trampoline_addr(SB), RODATA, $8
DATA	·libc_sem_unlink_trampoline_addr(SB)/8, $libc_sem_unlink_trampoline<>(SB)

// libc_pthread_self_trampoline<>: pthread_self(), no arguments.
TEXT libc_pthread_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_pthread_self(SB)
GLOBL	·libc_pthread_self_trampoline_addr(SB), RODATA, $8
DATA	·libc_pthread_self_trampoline_addr(SB)/8, $libc_pthread_self_trampoline<>(SB)
