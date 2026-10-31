# Working on this repository

This repository is a fork of `PowerDNS/lmdb-go` (v1, `master`, LMDB 0.9)
with the bundled C library removed. The goal is a Go implementation of
LMDB behind lmdb-go's public API, ported from **rlmdb**, the Rust rewrite
of LMDB, and binary compatible with C LMDB: a C process, a Rust (rlmdb)
process and a Go process must be able to use one environment at the same
time. The package and module names are not decided. The module path
stays `github.com/PowerDNS/lmdb-go` (do not change it: the owner tests the
port as a drop-in `replace` of lmdb-go v1); the package stays
`lmdb`. Do not call the project "pure Go" anywhere (names, docs, commit
messages) until that is established.

`PLAN.md` holds the architecture and the stages. This file holds the rules.

## Sources of truth

* **rlmdb** at `$RLMDB_DIR`, a
  detached worktree pinned at commit `9ea2373`. Read-only for you: do not
  commit there, do not edit sources there. Building in it is fine (its
  `target/` is already built in release mode, see below). Its documents are
  the specification of behaviour:
  * `DESIGN.md`: on-disk and lock-file format, architecture.
  * `COMPATIBILITY.md`: the compatibility contract and which test checks it.
  * `KNOWN_C_BUGS.md`: C bugs rlmdb does not port (and the few it does).
    The Go port follows rlmdb's decisions, row by row.
  * `DUAL_FORMAT.md`: the LMDB 1.0 data file and lock file.
  * `rlmdb-capi/README.md`: C API argument checks, error codes, deviations.
    lmdb-go is a thin binding of the C API, so this is the closest model
    for how the Go API must behave.
  * `AGENTS.md`: rlmdb's rules. Its sections 1 (binary compatibility),
    2 (do not port C bugs, track them) and 8 (C is the oracle) apply here
    unchanged; translate "Rust" to "Go".
* **The feature set is rlmdb's**: both data formats (LMDB 0.9 and 1.0),
  everything rlmdb implements, nothing rlmdb lacks (no `MDB_WRITEMAP`
  unless rlmdb has it; accept and ignore the flags rlmdb ignores).
* **The public API is lmdb-go's** (`git show upstream/master:lmdb/<file>`
  for the original cgo sources, and `upstream/master`'s tests). Same
  package names, types, method sets, signatures, error values
  (`*OpError`, `Errno`, `IsNotFound`, ...), flag constants, doc comments
  where they still hold. Existing lmdb-go users must be able to switch by
  changing an import path. For the 1.0 format, take the API of lmdb-go v2
  (`git show upstream/v2-dual-lmdb-versions:lmdb/env.go`, `sniff.go`,
  `version.go`): `Format`, `V09`, `V10`, `Env.SetFormat`, `Env.Format`,
  `SniffFormat`, `ErrFormatConflict`, the `LMDB_DEFAULT_FORMAT`-style
  default (use rlmdb's `RLMDB_DEFAULT_FORMAT` semantics, experimental).
* **C is the oracle.** C LMDB 0.9.36 and the pinned LMDB 1.0 are built by
  rlmdb's interop crate; the binaries are in
  `$RLMDB/target/release/build/rlmdb-interop-*/out/bin/` (`cpeer`, `cpeer10`,
  `writeops`, `writeops10`, `mdb_stat`, `mdb_dump`, `mdb_load`, `mdb_copy`,
  their `*10` versions, `layout_dump`, `mtest*`), plus `rpeer` and `rtools`
  in `$RLMDB/target/release/`. These are macOS binaries; for Linux build
  them in the container (see "Linux" below). lmdb-go v1 itself (cgo, C
  0.9.35, `github.com/PowerDNS/lmdb-go v1.9.4`) is the API oracle and the
  benchmark baseline.

## Hard rules

1. **No cgo in the library.** The `lmdb` package and everything it imports
   must build with `CGO_ENABLED=0` on Linux and macOS. Use `syscall`,
   `golang.org/x/sys/unix` and, where the OS offers no syscall (macOS POSIX
   semaphores), dynamic imports of libSystem with assembly trampolines in
   the style of `x/sys/unix`'s darwin files. Tests, oracles and benchmarks
   may use cgo, in separate packages or modules that the library does not
   import (for example a `tests/` or `bench/` module with its own `go.mod`).
   Because the port keeps lmdb-go's module path, lmdb-go v1 (cgo) and the
   port cannot be in one build. Code that runs against both imports
   `github.com/PowerDNS/lmdb-go/lmdb` and is built twice: with a `go.mod`
   that `replace`s the module with this repository (the port), and with a
   second mod file (`go build -modfile=go.cgo.mod`) that requires the real
   `v1.9.4` (cgo). Tests that need both in one process (in-process
   differential tests) must use the C binaries from rlmdb or a helper
   process instead.
2. **Interoperability with C locking is an absolute must.** On Linux the
   writer and reader-table mutexes are glibc's process-shared robust
   `pthread_mutex_t`s inside the lock file; the Go implementation must
   take and release them with the exact glibc protocol (futex word, owner
   TID, `FUTEX_WAITERS`, `FUTEX_OWNER_DIED`, the per-thread robust list
   via `set_robust_list`, `EOWNERDEAD` recovery, `NOTRECOVERABLE`) so that a
   C process waiting on a mutex a Go process holds behaves exactly as if
   the holder were C, including when the holder dies. On macOS they are
   POSIX named semaphores whose names are stored in the lock file (read
   them, never recompute them; rlmdb `AGENTS.md` §1). Both lock-file
   versions (0.9's and 1.0's) as rlmdb implements them.
3. Binary compatibility invariants of rlmdb `AGENTS.md` §1 hold: data
   files, lock files, free list, reader slots, meta protocol, sort order.
   Layout exactness against C is the default test oracle (a Go writer
   leaves the same file bytes as C and rlmdb); a deliberate divergence
   needs the same justification and ledger as in rlmdb.
4. Malformed input returns an error, never a panic, hang or out-of-range
   access. Crafted files must not crash the process (except the MVCC
   damage cases rlmdb documents).
5. `unsafe` only where needed: the OS layer (mmap, futexes, semaphores,
   shared memory atomics) and measured hot paths. Each use has a comment
   saying why it is sound. Values returned to users are zero-copy slices
   into the map, as in lmdb-go, with the same lifetime caveats.
6. Commit only your own paths, staged explicitly (`git add <paths>`).
   Never `git commit -a`, `git add -A` or `git add .`. Never push; there
   is deliberately no push URL. Do not rewrite history on `main`.
7. Committed text (docs, results, commit messages) must not contain local
   paths (`/Users/...`), usernames, hostnames, exact macOS versions or
   kernel build strings. Hardware specs, "macOS 15", toolchain versions
   and container image names are fine.

## Testing

* `go vet ./...`, `gofmt -l` clean, `staticcheck` if available.
* `CGO_ENABLED=0 go build ./...` must pass for the library packages.
* Run everything on macOS **and** Linux before declaring done:
  `scripts/linux-test.sh` runs a command in a Debian golang container
  (arm64 native; `LINUX_TEST_PLATFORM=linux/amd64` for x86-64 under
  emulation, slow but it checks the x86-64 `pthread_mutex_t` layout).
  `RLMDB_DIR=<rlmdb checkout>` mounts rlmdb read-only at `/rlmdb`; build
  the Linux C oracles there into a directory under `/tmp` (the container's
  tmpfs), never into the mounted checkout.
* Environments for tests and benchmarks live on local file systems only:
  `$TMPDIR` / `t.TempDir()` on macOS, `/tmp` (tmpfs) in the container.
  Never on bind mounts.
* Test scratch directories are per test process.
* Cross-process tests (C and Go processes on one environment at once) are
  required for anything that touches the lock file, reader table, writer
  lock or meta pages.

## Benchmarks

The baseline is lmdb-go v1 (cgo, C 0.9.35), including its cgo call
overhead. Interleave A/B runs in one session (machines are noisy); never
compare separate runs. Report units (ns/op) and say what a ratio is
(Go/C: below 1 means Go is faster). DUPSORT and sequential-append
performance matter less; do not spend much effort there.

## Style

Idiomatic, documented Go. Each file starts with a comment saying what it
models and which part of `mdb.c` and of rlmdb it corresponds to. Exported
identifiers have doc comments. Comments say why. Small functions. Where
behaviour must match C exactly, say so and cite the C function.
