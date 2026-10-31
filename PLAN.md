# Plan: an LMDB implementation in Go behind the lmdb-go API

Read `AGENTS.md` first. rlmdb (Rust) is the starting point: port its
structure, its decisions and its tests, not `mdb.c` directly. Where the
two differ, rlmdb wins unless it documents a reason specific to Rust.

## Package layout

Go packages mirror rlmdb's modules, so a reader can go from one to the
other:

| Go package | rlmdb source | contents |
|---|---|---|
| `internal/format` | `rlmdb/src/format/*` (minus lock file), `consts.rs`, `flags.rs`, `cmp.rs`, `idl.rs` | page, node, meta, DB record layouts for 0.9 and 1.0 (`Layout`), constants, comparators, ID lists |
| `internal/lockfile` | `format/lockfile.rs`, `os/shared_lock.rs`, `core/reader.rs`, the lock-file parts of `os/unix.rs` (`LockMap`, `fcntl` record locks) | lock-file layouts (both versions, per platform), the lock-file map, open / first-opener protocol, record locks, reader table, writer and reader mutexes shared with C (the cgo pair `shared_cgo.go` / `shared_nocgo.go`) |
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

## Deferred decisions (owner, revisit before a release)

* **Builds without cgo.** Go disables cgo by default when
  cross-compiling or when no C compiler is found, so a plain
  `GOOS=linux go build` from a Mac silently gets the `NoLock`-only build,
  which fails only at `Env.Open`. Option: make `!cgo` builds fail to
  compile unless they opt in with a tag (e.g. `lmdb_nocgo`). Today: runtime
  error at `Open` without `NoLock`.
* **macOS locks without cgo.** Go binaries on macOS always link
  libSystem, even with `CGO_ENABLED=0`, so the semaphore calls could use
  libSystem trampolines (as `x/sys/unix` does) instead of cgo, making
  macOS builds without cgo fully functional. Today: cgo, as on Linux.
* Cross-compiling cgo builds: `zig cc` as `CC` (bundled glibc, musl and
  macOS headers) is the suggested toolchain; document and test it.
