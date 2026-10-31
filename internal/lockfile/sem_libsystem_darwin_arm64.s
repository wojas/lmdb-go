//go:build !cgo

// Trampolines for sem_libsystem_darwin.go on darwin/arm64: each is entered
// from runtime·syscall or runtime·syscall6 (runtime/sys_darwin_arm64.s)
// with the C calling convention, the call's arguments in R0..R5 as the
// runtime loaded them, and the return address in R30. Each trampoline's
// address is stored in the matching ·libc_*_trampoline_addr variable for
// the Go code. The layout follows golang.org/x/sys/unix's
// zsyscall_darwin_arm64.s.

#include "textflag.h"

// libc_sem_open_trampoline<>: sem_open(name, oflag, mode, value).
//
// In: R0 = name, R1 = oflag (named arguments, passed in registers),
// R2 = mode, R3 = value (variadic arguments). Apple's arm64 ABI passes
// variadic arguments on the stack only, each in an 8-byte slot from the
// stack pointer up, so at the call sem_open reads mode from 0(RSP) and
// value from 8(RSP). Those slots must be this trampoline's own: the
// caller's stack above the entry RSP (E) holds runtime·syscall6's data.
//
// The $0 frame is not a leaf (it calls), so the assembler adds a prologue
// that stores R30 (the return address) at E-16 and R29 (the caller's frame
// pointer) at E-24, sets R29 = E-24 and RSP = E-16, and an epilogue that
// reloads both before RET. SUB $32 then puts RSP at E-48 (16-byte
// aligned): slot 0 at E-48, slot 1 at E-40, and the saved R29 at E-24
// untouched. (SUB $16 would put slot 1 on the saved R29 and return with a
// corrupted frame pointer; runtime·open_trampoline can use SUB $16 because
// it passes one variadic argument only.)
//
// Out: R0 = the sem_t * or SEM_FAILED, for runtime·syscall6.
TEXT libc_sem_open_trampoline<>(SB),NOSPLIT,$0-0
	SUB	$32, RSP	// two variadic slots, clear of the saved R29
	MOVD	R2, 0(RSP)	// slot 0: mode (mode_t, promoted to int)
	MOVD	R3, 8(RSP)	// slot 1: value (unsigned int)
	CALL	libc_sem_open(SB)
	ADD	$32, RSP
	RET
GLOBL	·libc_sem_open_trampoline_addr(SB), RODATA, $8
DATA	·libc_sem_open_trampoline_addr(SB)/8, $libc_sem_open_trampoline<>(SB)

// The other functions take named arguments only, already in place
// (R0 = the handle or the name): a tail jump; the result is left in R0.

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
