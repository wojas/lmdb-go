// Package lockfiletest runs the lock file of the Go implementation
// (internal/lockfile) against C LMDB processes: the C 0.9 and C 1.0 peers
// and tools of rlmdb's interop build (cpeer, cpeer10, mdb_stat,
// mdb_stat10, layout_dump, layout_dump10). It checks that C and Go
// processes share one lock file as C processes do among themselves: the
// table one side initialises is accepted by the other, writers exclude
// each other, readers see each other, the reader check clears dead
// processes of the other side, and (Linux) a writer killed while holding
// the writer mutex is recovered from by the other side (EOWNERDEAD).
//
// The C binaries are found in $LMDB_ORACLE_BIN, or in rlmdb's interop
// build under $RLMDB_DIR (macOS); on Linux, scripts/build-c-oracles.sh
// builds them for the container's libc. Without them the tests skip. From
// the repository root:
//
//	cd tests && RLMDB_DIR=<rlmdb checkout> go test ./lockfile   # macOS
//	RLMDB_DIR=<rlmdb checkout> scripts/linux-test.sh sh -c \
//	  'scripts/build-c-oracles.sh && cd tests &&
//	   LMDB_ORACLE_BIN=/tmp/c-oracles/bin go test ./lockfile'
//
// For musl, add LINUX_TEST_IMAGE=golang:1.25-alpine and start the command
// with "apk add -q build-base &&"; for x86-64 under emulation,
// LINUX_TEST_PLATFORM=linux/amd64. C and Go must use the same libc: on
// aarch64, glibc's and musl's lock files differ.
//
// The Go processes here are cgo binaries (this test binary, re-executed as
// a helper), like every program that opens an environment with a lock
// file.
package lockfiletest
