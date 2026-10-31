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
| `internal/lockfile` | `format/lockfile.rs`, `os/shared_lock.rs`, `core/reader.rs` | lock-file layouts (both versions, per platform), open / first-opener protocol, reader table, writer and reader mutexes shared with C |
| `internal/sys` | `os/unix.rs`, `os/mod.rs` | mmap, file I/O, fsync, `fcntl` record locks, the data map (`DataMap`), page-size and platform queries |
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
	layout *Layout) (*Region, error) // nil, nil: read-only env on a read-only FS
func LayoutFor(f format.Format) *Layout // this platform's lock layout for a data format

// The engine's view (package engine), implemented by *lockfile.Region:
type LockRegion interface {
	IsExclusive() bool
	Downgrade(txnid uint64) error   // mdb_env_share_locks
	MaxReaders() uint32
	// LockWriter blocks until this process owns the writer mutex. On Linux
	// the calling goroutine must be locked to its OS thread
	// (runtime.LockOSThread) until UnlockWriter. newestTxnid repairs the
	// state after a dead owner (mdb_mutex_failed).
	LockWriter(newestTxnid func() (uint64, error)) error
	UnlockWriter()
	TxnID() uint64                  // mti_txnid
	SetTxnID(uint64)
	AcquireSlot() (int, error)      // reader slot, under the reader mutex
	SlotIsIdleAndOurs(i int) bool
	PublishCurrentTxnID(i int) uint64
	SlotTxnID(i int) uint64
	SetSlotTxnID(i int, txnid uint64)
	ResetSlot(i int)
	ReleaseSlot(i int)
	FindOldest(txnid uint64) uint64 // mdb_find_oldest
	ReaderList() []ReaderInfo
	ReaderCheck() (int, error)
	Close() error
}
```

If the Rust code shows that a method is missing or needs a different
shape, the lockfile agent owns the change and documents it here; the
engine agent adapts. Until `internal/lockfile` lands on `main`, the engine
uses a test fake of the interface (in-process only).

## Stages

1. **Parallel foundations.**
   * L (lockfile): `internal/lockfile` and the OS mutex code it needs
     (Linux: glibc robust mutex protocol over futexes, no cgo; macOS:
     POSIX semaphores through libSystem trampolines, no cgo), both lock
     file versions, with cross-process tests against C (the `cpeer`
     binaries, or a cgo helper in `tests/`): C and Go writers excluding
     each other, readers registering and being seen by C's `mdb_stat -r`
     / reader check, a writer dying while holding the mutex on either
     side and the other side recovering (Linux `EOWNERDEAD`), Linux arm64
     and x86-64 (emulated) layouts.
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
