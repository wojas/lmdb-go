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
| `internal/lockfile` | `format/lockfile.rs`, `os/shared_lock.rs`, `core/reader.rs`, the lock-file parts of `os/unix.rs` (`LockMap`, `fcntl` record locks) | lock-file layouts (both versions, per platform), the lock-file map, open / first-opener protocol, record locks, reader table, writer and reader mutexes shared with C (Linux: the cgo pair `shared_cgo.go` / `shared_nocgo.go`; macOS: `sem_darwin.go`, through cgo or, without cgo, libSystem stubs) |
| `internal/sys` | `os/unix.rs`, `os/mod.rs` | mmap, file I/O, fsync, the data map (`DataMap`), page-size and platform queries (the record locks are the lock file's, in `internal/lockfile`) |
| `internal/engine` | `rlmdb/src/core/*` minus `reader.rs` | env, read and write txns, cursors, put/del, split, rebalance, spill, free list, commit, nested txns, copy, audit |
| `lmdb` | lmdb-go's `lmdb` (API), rlmdb-capi's behaviour | the public API on top of `engine` |
| `lmdbscan`, `exp/lmdbsync`, `cmd/*` | lmdb-go | kept, ported to the new `lmdb` |
| `tests/` (nested module, cgo allowed) | rlmdb tests, interop, `bench` | everything that compares with C: `tests/cgolmdb` (pinned lmdb-go v1.9.4 copy, cgo, C 0.9.35) side by side with the port in one binary; the C binaries from rlmdb's interop build; cross-process tests; benchmarks (`tests/bench`); the Go interop peer (`tests/gopeer`) |
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
   against lmdb-go v1 on cgo. Target: close to lmdb-go+C including its cgo
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
  `INTEROP_GOPEER_IMPL=port` (rlmdb branch `t3/interop-gopeer`:
  `go_pairs`, `go_pairs10`, `go_peer`) passes in full on macOS and on
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
liveness through `OpenProcess`, with rlmdb's fix #42), files opened with
C's sharing. A write transaction keeps its OS thread (the mutexes belong
to it, as on Linux). lmdb-go v1's `lmdb/error_windows.go` is kept.

Status: windows/amd64 and windows/arm64 build and vet (the root module;
the `tests` module on amd64 with cgo through MinGW-w64, and its packages
that do not need cgo on both without), and so do their test binaries
(`go test -c`); nothing has run on Windows yet. `scripts/windows-test.sh` runs commands
on a Windows machine (a git-archive copy, its own work directory and
toolchain), and `tests/cmd/build-c-oracles` builds the C oracles there
with MinGW-w64.

## Deferred decisions (owner, revisit before a release)

* **Linux builds without cgo.** Go disables cgo by default when
  cross-compiling or when no C compiler is found, so a plain
  `GOOS=linux go build` from a Mac silently gets the `NoLock`-only build,
  which fails only at `Env.Open`. Option: make Linux `!cgo` builds fail to
  compile unless they opt in with a tag (e.g. `lmdb_nocgo`). Today: runtime
  error at `Open` without `NoLock`. (macOS builds without cgo have shared
  locks, see below.)
* **macOS locks without cgo: resolved (owner).** cgo stays the default on
  macOS (`darwin && cgo`: `sem_cgo_darwin.go`); a build without cgo
  (`darwin && !cgo`) calls the semaphores in libSystem directly
  (`sem_libsystem_darwin.go`, arm64 and amd64 trampolines, the mechanism
  of `x/sys/unix`, no third-party package), so `CGO_ENABLED=0` macOS
  builds and cross-compiles to macOS share locks with C fully. Both sit
  behind one set of calls (`sem_darwin.go`) and pass the same unit tests
  and the `tests/lockfile` suite against C 0.9 and 1.0, on arm64 and on
  amd64 under Rosetta. `sem_open` is variadic: Apple's arm64 ABI passes
  its mode and value on the stack, which its trampoline handles (and
  tests prove). The cgo path is the fallback if a Go release ever breaks
  the `syscall.syscall` linkname.
* **64-bit big-endian (s390x, ppc64, mips64).** Supported in principle
  (native byte order throughout, as C LMDB) but untested. Test later in
  an emulated Docker container (`LINUX_TEST_PLATFORM=linux/s390x`) with C
  peers built there. rlmdb is adding support for more architectures first
  (owner, 2026-10-31); when its thread reports, port that work here (the
  same way as any rlmdb change, see "Following rlmdb").
* **BSDs.** rlmdb supports FreeBSD, OpenBSD, NetBSD and DragonFly as an
  untested tier (e4a5ad9, ledger #39; C 1.0 uses SysV semaphores on
  OpenBSD/NetBSD/DragonFly, FreeBSD's robust mutex is 8 bytes). Port in
  the next rlmdb sync round, together with rlmdb's versions of #38 and the
  meta-sync fix, then big-endian when it lands there (Windows: see
  above).
* Cross-compiling cgo builds: `zig cc` as `CC` (bundled glibc, musl and
  macOS headers) is the suggested toolchain; document and test it.
