// Package coracle finds the C oracles of the tests: the C LMDB 0.9 and 1.0
// programs (writeops, cpeer, the C tools, layout_dump and their *10
// versions) that rlmdb's interop crate builds from its vendored C sources
// (tests/lmdb-c/0.9 and tests/lmdb-c/1.0 of the rlmdb checkout), or that
// scripts/build-c-oracles.sh and scripts/build-c-writeops.sh build for the
// libc of a Linux container, or cmd/build-c-oracles on any OS (Windows
// included).
package coracle

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// Exe is the file name of the program name on this OS: with .exe on
// Windows.
func Exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// InteropBin returns the directory of rlmdb's interop build under the rlmdb
// checkout rlmdbDir (its repository root, the Cargo workspace):
// target/release/build/rlmdb-interop-*/out/bin. Cargo keeps one such
// directory per build configuration; of those that hold the program want,
// the one where it is newest wins. "" if there is none.
func InteropBin(rlmdbDir, want string) string {
	if rlmdbDir == "" {
		return ""
	}
	dirs, _ := filepath.Glob(filepath.Join(rlmdbDir, "target", "release", "build", "rlmdb-interop-*", "out", "bin"))
	best, bestTime := "", int64(0)
	for _, d := range dirs {
		fi, err := os.Stat(filepath.Join(d, Exe(want)))
		if err != nil {
			continue
		}
		if t := fi.ModTime().UnixNano(); best == "" || t > bestTime {
			best, bestTime = d, t
		}
	}
	return best
}

// Text is the text output of a C program as on Unix: the Windows C runtime
// writes stdout and stderr in text mode, "\r\n" for every "\n", unless the
// program switched them to binary (rlmdb's tests/compat does for its own
// programs, not for the upstream tools such as mdb_dump and layout_dump).
func Text(s string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(s, "\r\n", "\n")
	}
	return s
}

// crtErrno are the C runtime's numbers (errno.h of MSVCRT and UCRT, which
// MinGW-w64 uses too) for the errno values LMDB returns, by the syscall
// constant that stands for them in Go. On Windows Go numbers these
// constants itself (syscall.EINVAL is 0x20000000 plus its index in Go's
// table); the engine returns them, as lmdb-go v1 did (lmdb/error_windows.go).
var crtErrno = map[syscall.Errno]int{
	syscall.EPERM:  1,
	syscall.EIO:    5,
	syscall.EBADF:  9,
	syscall.EAGAIN: 11,
	syscall.ENOMEM: 12,
	syscall.EACCES: 13,
	syscall.EBUSY:  16,
	syscall.EEXIST: 17,
	syscall.EINVAL: 22,
	syscall.ENOSPC: 28,
}

// CErrno is the number a C LMDB process returns for the error code Go's
// engine returned (format.Code), for comparing the two: on Windows Go's own
// numbers of the errno constants become the C runtime's; elsewhere, and
// for LMDB's codes and system error codes, the code is the same.
func CErrno(code int) int {
	if runtime.GOOS == "windows" {
		if c, ok := crtErrno[syscall.Errno(code)]; ok {
			return c
		}
	}
	return code
}
