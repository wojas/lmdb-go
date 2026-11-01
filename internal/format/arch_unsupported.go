//go:build !(amd64 || arm64 || loong64 || mips64 || mips64le || ppc64 || ppc64le || riscv64 || s390x || wasm)

// This implementation is 64-bit only, and this file stops a build for any
// other GOARCH (386, arm, mips, mipsle) with "undefined:
// lmdb_requires_a_64_bit_platform" rather than an obscure error deeper in
// the engine.
//
// LMDB's data file depends on sizeof(size_t): page numbers, transaction
// IDs, and the sizes and counts in the meta page and the MDB_db records
// are size_t, so a 32-bit C build writes another format, which 64-bit
// processes cannot open or share, and the reverse. Only the 64-bit layout
// is implemented, as in rlmdb, which refuses 32-bit targets with a
// compile_error! (format/mod.rs). lmdb-go v1, on C LMDB, also ran on
// 32-bit platforms, with their 32-bit format.
//
// 64-bit big-endian targets (mips64, ppc64, s390x) build; s390x is tested
// under emulation (QEMU user mode, scripts/linux-test.sh), mips64 and
// ppc64 are not: LMDB stores integers in the host's byte order and so does
// this implementation, so files are shared with C processes on the same
// kind of machine, as between C processes.

package format

var _ = lmdb_requires_a_64_bit_platform
