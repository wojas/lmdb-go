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
