# Plan: an LMDB implementation in Go behind the lmdb-go API

Read `AGENTS.md` first. rlmdb (Rust) is the starting point: port its
behaviour, its decisions and its tests, not `mdb.c` directly. Where the
two differ in behaviour, rlmdb wins unless it documents a reason specific
to Rust. Its structure is a starting point only: write idiomatic Go and
diverge where Go is better served (see "Following rlmdb").

## Package layout

The initial port maps rlmdb's modules onto Go packages roughly like this
(a starting point, not a constraint):

| Go package | rlmdb source | contents |
|---|---|---|
| `internal/format` | `rlmdb/src/format/*` (minus lock file), `consts.rs`, `flags.rs`, `cmp.rs`, `idl.rs` | page, node, meta, DB record layouts for 0.9 and 1.0 (`Layout`), constants, comparators, ID lists |
| `internal/lockfile` | `format/lockfile.rs`, `os/shared_lock.rs`, `core/reader.rs`, the lock-file parts of `os/unix.rs` (`LockMap`, `fcntl` record locks) | lock-file layouts (both versions, per platform), the lock-file map, open / first-opener protocol, record locks, reader table, writer and reader mutexes shared with C (Linux and FreeBSD: the cgo pair `shared_cgo.go` / `shared_nocgo.go`; macOS: `sem.go`, through cgo or, without cgo, libSystem stubs; OpenBSD, NetBSD, DragonFly: `sem.go` and `sysv.go` through cgo) |
| `internal/sys` | `os/unix.rs`, `os/mod.rs` | mmap, file I/O, fsync, the data map (`DataMap`), page-size and platform queries (the record locks are the lock file's, in `internal/lockfile`) |
| `internal/engine` | `rlmdb/src/core/*` minus `reader.rs` | env, read and write txns, cursors, put/del, split, rebalance, spill, free list, commit, nested txns, copy, audit |
| `lmdb` | lmdb-go's `lmdb` (API), rlmdb-capi's behaviour | the public API on top of `engine` |
| `lmdbscan`, `exp/lmdbsync`, `cmd/*` | lmdb-go | kept, ported to the new `lmdb` |
| `tests/` (nested module, cgo allowed) | rlmdb tests, interop, `bench` | everything that compares with C: `tests/cgolmdb` (pinned copy of lmdb-go at `e5af975`: v1.9.4's Go code, cgo, C 0.9.36 plus ITS#10615, ITS#10642 and ITS#10643) side by side with the port in one binary; the C binaries from rlmdb's interop build; cross-process tests; benchmarks (`tests/bench`); the Go interop peer (`tests/gopeer`) |
| `tests/gopeer` | `interop/src/bin/rpeer` | the interop peer protocol (`interop/README.md`), so rlmdb's harness can run Go as a fourth implementation |

Names inside packages follow rlmdb's concepts in Go style (`numKeys`,
`childPgno`, `Lower()`), with the C name in the doc comment.

## The `engine` / `lockfile` boundary

Two agents work in parallel at first: one on `internal/lockfile` (with the
OS-specific shared mutexes), one on everything else. The engine depends on
the lock file only through this interface, which the engine package
declares (`engine.LockRegion`) and `*lockfile.Region` implements. It is the
Go form of rlmdb's `LockRegion` (`core/reader.rs`); keep the semantics of
each Rust method exactly.

```go
// In package lockfile:
func Open(path string, mode os.FileMode, maxReaders uint32, readOnly bool,
	layout *Layout) (*Region, error) // nil, nil: read-only env on a read-only FS;
	                                 // ErrNoSharedLocks in a build without cgo
func LayoutFor(f format.Format) *Layout // this platform's lock layout for a data
	                                     // format; nil where unknown (Linux without cgo)
var ErrNoSharedLocks error

// The engine's view (package engine), implemented by *lockfile.Region:
type LockRegion interface {
	IsExclusive() bool
	Downgrade(txnid uint64) error   // mdb_env_share_locks
	MaxReaders() uint32
	NumReaders() uint32             // mti_numreaders as stored (mdb_env_info)
	// LockWriter blocks until this process owns the writer mutex. It pins
	// the calling goroutine to its OS thread (runtime.LockOSThread, nests)
	// until UnlockWriter, which must be called by the same goroutine (a
	// robust pthread mutex belongs to its thread; another thread gets
	// EPERM). newestTxnid repairs the state after a dead owner
	// (mdb_mutex_failed); if it or the repair fails, the mutex is left not
	// recoverable.
	LockWriter(newestTxnid func() (uint64, error)) error
	UnlockWriter() error
	TxnID() uint64                  // mti_txnid
	SetTxnID(uint64)
	AcquireSlot() (int, error)      // new reader slot, under the reader mutex
	SlotIsIdleAndOurs(i int) bool   // owned slots are reused without the mutex
	PublishCurrentTxnID(i int) uint64
	SlotTxnID(i int) uint64
	SetSlotTxnID(i int, txnid uint64)
	ResetSlot(i int)
	ReleaseSlot(i int)
	FindOldest(txnid uint64) uint64 // mdb_find_oldest
	ReaderList() []format.ReaderInfo
	ReaderCheck() (int, error)
	Close() error
}
```

Errors are `format.Errno` (LMDB's codes: `Invalid`, `VersionMismatch`,
`ReadersFull`), `syscall.Errno` (possibly wrapped in `*os.PathError`;
`ENOTRECOVERABLE`, `EPERM`, ...) or `lockfile.ErrNoSharedLocks`.
`*lockfile.Region` also has `Layout()` and `Header()` (a decoded copy of
the header) for diagnostics and tests. Changes from the first draft
(stage 1 L): `UnlockWriter` returns an error (misuse from another thread
is reported instead of leaving a silent deadlock); `NumReaders` added (rlmdb's
engine uses `raw_num_readers` for env info); `ReaderInfo` and the error
codes are `internal/format`'s, which the engine defines; `LockWriter` pins
the thread itself rather than requiring the caller to.

If the Rust code shows that a method is missing or needs a different
shape, the lockfile agent owns the change and documents it here; the
engine agent adapts. Until `internal/lockfile` lands on `main`, the engine
uses a test fake of the interface (in-process only).

## Stages

1. **Parallel foundations.**
   * L (lockfile): `internal/lockfile` and the libc mutex/semaphore calls
     it needs (cgo, one small file pair; `!cgo` builds support `NoLock`
     only), both lock file versions, with cross-process tests against C (the `cpeer`
     binaries, or a cgo helper in `tests/`): C and Go writers excluding
     each other, readers registering and being seen by C's `mdb_stat -r`
     / reader check, a writer dying while holding the mutex on either
     side and the other side recovering (Linux `EOWNERDEAD`), Linux glibc
     arm64 and x86-64 (emulated) and musl (Alpine) layouts.
   * E (engine): `internal/format`, `internal/sys`, `internal/engine`
     against the interface above: the read path, then the write path,
     nested transactions, copy (plain and compacting), both data formats,
     with rlmdb's unit tests ported and a write-differential test against
     C (`writeops` / `writeops10` from rlmdb's interop build, byte-equal
     files).
2. **Integration and API.** Wire `lockfile` into `engine`; the public
   `lmdb` package with lmdb-go's API; lmdb-go's own test suite passing
   unmodified where it does not depend on C internals; `lmdbscan`,
   `exp/lmdbsync`, `cmd/*`; the `gopeer` and a branch of rlmdb's interop
   harness that runs the scenario matrix with Go as a peer (C 0.9, C 1.0,
   Rust, Go, concurrently); Linux and macOS.
3. **Benchmarks and performance.** `tests/bench` with the workloads of rlmdb's
   `bench` (reads, cursor scans, the Lightning Stream scan, random puts,
   overwrites, deletes, one-put commits, big values), interleaved A/B
   against lmdb-go v1's Go code on cgo (`tests/cgolmdb`). Target: close to lmdb-go+C including its cgo
   overhead, better where cheap. Append and DUPSORT are low priority.
4. **Review and hardening.** Independent review, fuzzing of page parsing
   (`go test -fuzz`), robustness tests ported from rlmdb, docs.

## Status

* Stage 0 (coordinator): fork created from lmdb-go `master` (v1.9.4), C
  sources removed; this plan; `scripts/linux-test.sh`. The module path
  stays `github.com/PowerDNS/lmdb-go` (owner: drop-in `replace`). The nested `tests/` module holds the
  pinned cgo reference `tests/cgolmdb` (`scripts/update-cgolmdb.sh`).
* Stage 1 L: `internal/lockfile` with the interface above; Linux C
  oracles for any libc with `scripts/build-c-oracles.sh` (in the
  container; `RLMDB_DIR` set); cross-process tests against C 0.9 and 1.0
  in `tests/lockfile` (macOS, glibc arm64 and x86-64, musl).
* Stage 1 E: `internal/format`, `internal/sys`, `internal/engine`
  (see `internal/engine/PORTING.md`), byte-identical to C writers.
* Stage 2: package `lmdb` with lmdb-go v1's API (declarations kept in
  place, bodies on the engine; glue in `lmdb/engine.go`, the lock file
  wired in `lmdb/lock.go` only) and lmdb-go v2's format API;
  `lmdb/DEVIATIONS.md` is the ledger (three v1 bugs not kept, every
  changed test). lmdb-go v1's and v2's test suites, `lmdbscan`,
  `exp/lmdbsync`, the commands: pass with cgo and with `CGO_ENABLED=0`
  (`NoLock`; lock-file tests skip). The port twins of `tests/gopeer` and
  `tests/bench` build without a tag. rlmdb's interop matrix with
  `INTEROP_PEER_ARGS='--impl port'` (`external_pairs`,
  `external_pairs10`, `external_peer`) passes in full on macOS and on
  Linux arm64 with glibc and with musl (`tests/gopeer/README.md`).

## Following rlmdb

New features and improvements, Windows support included, land in rlmdb
first and are then ported here. To keep that cheap:

* rlmdb is the reference for behaviour, not a template for structure.
  Write idiomatic Go: Go has different costs (GC, escape analysis,
  interfaces, bounds checks, no lifetimes) and abstractions that pay off in
  Rust may not in Go, and the reverse. Staying close to rlmdb's structure is
  a convenience for porting where it costs nothing; diverge whenever Go is
  better served, and note larger divergences in `PORTING.md` so later
  ports know where to look.
* `internal/engine/PORTING.md` records the rlmdb commit the port is synced
  to and, per sync, the rlmdb commits ported, skipped (with the reason) or
  replaced by a Go-specific solution.
* Go-side fixes that also apply to rlmdb (bugs found here, like the
  `NoLock` reader tracking) are reported to the rlmdb side, so the two
  stay in step.

## Windows

Ported from rlmdb (its Windows port, `0bf0553` to `3df6995`; the sync
rows are in `internal/engine/PORTING.md`), without cgo: the OS layer
calls the system through `golang.org/x/sys/windows` (v0.30.0, the newest
release whose `go` directive fits `go 1.22`) and, for the NT section
calls C 1.0 makes, ntdll. The decisions are listed in the file comments
of `internal/sys/datamap_windows.go`, `internal/sys/io_windows.go`,
`internal/lockfile/filelock_windows.go`, `mutex_windows.go` and
`locks_windows.go`: an LMDB 0.9 data file extended to the map size and
mapped whole, an LMDB 1.0 one mapped with a reserved section it grows
into (pages taken from the end of the map committed in the view, as C
1.0 does), read-only maps of the file as it is (mapped again when a
snapshot needs more), the lock file locked with `LockFileEx` on byte 0,
named mutexes `Global\MDBr…` / `Global\MDBw…` (version 1: names in the
file; version 2: from the hash stored there, read, never recomputed),
abandoned mutexes repaired as dead robust ones, no pid locks (reader
liveness through `OpenProcess`, with rlmdb's fix of KNOWN_C_BUGS.md row
42), files opened with C's sharing. A write transaction keeps its OS
thread (the mutexes belong to it, as on Linux). lmdb-go v1's
`lmdb/error_windows.go` is kept.

Status: tested on Windows 10 x86-64 with Go 1.27 and MinGW-w64 gcc
(MSYS2): the root module with and without cgo, and the `tests` module
(cgo) against C LMDB 0.9.36 and 1.0 built there with
`tests/cmd/build-c-oracles` (lock-file interop with C peers, the write
differential, copies, Go reading C's files) pass, on an exFAT drive and,
for the file-system-sensitive tests (`internal/sys`, `internal/lockfile`,
the registry's links, `lmdb`, `tests/lockfile`), on NTFS, also with
`-race` (no race reported). rlmdb's interop matrix with the Go peer
passes there too (165, 231 and 5 tests; `tests/gopeer/README.md`), also
with the peer built with `-race`. `go vet` is clean on the root module;
on the `tests` module it warns about `tests/cgolmdb`, the pinned v1 copy
that is never edited (`C.mdb_filehandle_t(fd)` turns a `uintptr` into a
`HANDLE` pointer, vet's unsafeptr rule), and about nothing else.
`scripts/windows-test.sh` runs commands on a Windows machine (a
git-archive copy, its own work directory and toolchain). windows/arm64
builds and vets (the `tests` module with zig as its C compiler, with the
same warning); it has not run.

## The BSDs

Ported from rlmdb's BSD support (`e4a5ad9`, `bfef7e6`, `12a6731`; the
rows are in `internal/engine/PORTING.md`), following C LMDB's selection
on each BSD: FreeBSD takes Linux's robust process-shared mutexes (8 bytes
there; `shared_cgo.go`, `layout_freebsd.go`); OpenBSD, NetBSD and
DragonFly take POSIX named semaphores for LMDB 0.9 lock files, as macOS
(`sem.go`, `sem_cgo.go`; NetBSD's names are 14 characters), and the SysV
semaphore set of C 1.0 for LMDB 1.0 lock files (`sysv.go`,
`sysv_cgo.go`: `semget(ftok(lockfile, 'M'))`, the id stored at offset 20,
`SEM_UNDO` and flags in the file for a dead holder). The locks go through
cgo, as on Linux; without cgo these builds have `NoLock` only. The rest
is Go: `MDB_FDATASYNC` (`fdatasync` on FreeBSD, `fsync` elsewhere),
`MDB_DSYNC` (`O_SYNC` on DragonFly), and on OpenBSD, which has no unified
buffer cache, `msync(MS_INVALIDATE)` of the map after each write to the
data file (`sys.DataMap.Invalidate`, widened to whole OS pages: rlmdb
ledger row 39). The layouts are computed with C's formulas (rlmdb's numbers)
and tested on every host.

Status: OpenBSD 7.9 arm64 is tested (Go 1.26, the base system's clang):
the root module with cgo and without it (`NoLock`; opening with a lock
file fails with the error that names cgo), and the `tests` module (cgo)
against C LMDB 0.9.36 and 1.0 built there with `tests/cmd/build-c-oracles`
pass. That covers the lock-file interop with C peers of both formats
(named semaphores for 0.9, the SysV set for 1.0, including a writer that
dies holding the 1.0 writer lock, in both directions), the layouts
against `layout_dump` and the probe compiled there, the write
differential, copies and Go reading C's files. `-race` is not available
on openbsd/arm64. On OpenBSD, the plain copy of a file whose last meta
page write failed differs from C's `mdb_copy` in that meta page, which no
reader of the copy uses: C copies its map, which still shows the page as
it was before the failed commit, and this port, as rlmdb, reads the file
(`tests/engine`, `TestCopyAfterFailedMetaSync`). C LMDB keeps its map
coherent with its writes on OpenBSD only since upstream's ITS#10615
(`MDB_Invalidate`, in the vendored sources and in the cgo reference),
released in LMDB 0.9.37 and 1.0.3 (2026-10-21; 0.9.36 and 1.0.2 do not
have it): the released lmdb-go v1.9.4 (C 0.9.35) reads stale pages there
after its own writes (on it the benchmark fails with `MDB_NOTFOUND` and
crashes in `mdb_put`), and such a C writer leaves the maps of every
process sharing the file stale. FreeBSD 15.1 arm64 is tested likewise
(Go 1.26, the base system's clang): the root module with cgo and without
it, the `tests` module against C LMDB 0.9.36 and 1.0 built there by
rlmdb's interop build (the robust mutexes of both lock-file versions, a
writer that dies holding the writer mutex recovered from in both
directions, the layouts against `layout_dump` and the probe compiled
there, the write differential, copies), and rlmdb's interop matrix with
the Go peer (`tests/gopeer`: `--impl port` on both formats, `--impl cgo`
on 0.9) next to C LMDB 0.9.37 and 1.0.3, rlmdb and C on rlmdb. Two
failures there are the harness's: its `killed_writer` scenario counts
only Linux's and Windows's writer locks as robust and expects the next
writer to wait on FreeBSD, which fails for C against C as well and
passes with FreeBSD counted; and on two virtual CPUs the `--nosync`
writer loop of `concurrent_writers_readers` now and then fills the
scenario's 512 MiB map (`MDB_MAP_FULL`) while a preempted reader pins an
old snapshot, readers of every implementation alike. `-race` is not
available on freebsd/arm64. Fuzzing there found that pooled read cursors
and transactions kept page slices of unmapped maps, which the collector
follows once FreeBSD places Go heap in the same addresses (fixed:
`rcursor.forget`). NetBSD, DragonFly, and FreeBSD and OpenBSD on amd64,
have not run. All four build,
vet and compile their test binaries (`go test -c`) for amd64 and arm64
(DragonFly: amd64 only, as Go), without cgo and with it:
`scripts/bsd-cc-check.sh` compiles the root module (the cgo lock files),
the `tests` module and the C oracles against each BSD's own headers and C
library (zig's for FreeBSD 14, NetBSD 10.1 and OpenBSD 7.8; a DragonFly
6.4 sysroot under `$HOME/sdk`), and checks the lock-file layouts against
C's `mdb.c` compiled there (`TestLayoutsMatchCompiledC`): they agree.
On macOS the lock-file tests also run the BSDs' LMDB 1.0 layout (the SysV
set, including a writer that dies) and NetBSD's LMDB 0.9 layout,
`tests/lockfile` compares the `ftok` formula with libc's, and
`internal/sys` tests the invalidation. To run the others: a VM with Go
and the base system's `cc`, running `go test ./...` and the `tests`
module against the C oracles built there (`tests/cmd/build-c-oracles`),
as on FreeBSD and OpenBSD, with `-lrt` on NetBSD coming from
`sem_cgo.go`.

## Deferred decisions (owner, revisit before a release)

* **Linux builds without cgo.** Go disables cgo by default when
  cross-compiling or when no C compiler is found, so a plain
  `GOOS=linux go build` from a Mac silently gets the `NoLock`-only build,
  which fails only at `Env.Open` (the same holds for the BSDs). Option:
  make Linux `!cgo` builds fail to compile unless they opt in with a tag
  (e.g. `lmdb_nocgo`). Today: runtime error at `Open` without `NoLock`.
  (macOS builds without cgo have shared locks, see below.)
* **macOS locks without cgo: resolved (owner).** cgo stays the default on
  macOS (`darwin && cgo`: `sem_cgo.go`); a build without cgo
  (`darwin && !cgo`) calls the semaphores in libSystem directly
  (`sem_libsystem_darwin.go`, arm64 and amd64 trampolines, the mechanism
  of `x/sys/unix`, no third-party package), so `CGO_ENABLED=0` macOS
  builds and cross-compiles to macOS share locks with C fully. Both sit
  behind one set of calls (`sem.go`) and pass the same unit tests
  and the `tests/lockfile` suite against C 0.9 and 1.0, on arm64 and on
  amd64 under Rosetta. `sem_open` is variadic: Apple's arm64 ABI passes
  its mode and value on the stack, which its trampoline handles (and
  tests prove). The cgo path is the fallback if a Go release ever breaks
  the `syscall.syscall` linkname.
* **64-bit big-endian (s390x, ppc64, mips64): tested on s390x under
  emulation.** Native byte order throughout, as C LMDB. The root module
  and the tests module, with C peers built in the container, pass on
  s390x under QEMU user emulation (`LINUX_TEST_IMAGE=golang:1.25-trixie
  LINUX_TEST_PLATFORM=linux/s390x scripts/linux-test.sh`; owner-death
  tests skip, QEMU has no robust futexes), as rlmdb's suites do since
  its `5773e0a`; ppc64 and mips64 are untested. Later big-endian work of
  rlmdb is ported here (the
  same way as any rlmdb change, see "Following rlmdb").
* **BSDs: ported** (see "The BSDs"); FreeBSD and OpenBSD arm64 are
  tested. Next: NetBSD and DragonFly, and amd64.
* Cross-compiling cgo builds: `zig cc` as `CC` (bundled glibc, musl and
  macOS headers) is the suggested toolchain; document and test it.
