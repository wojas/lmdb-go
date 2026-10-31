# Porting notes: rlmdb core -> internal/engine

Status of the stage-1 engine port (PLAN.md, agent E). rlmdb sources:
`rlmdb/src/core/*.rs` and `format/*` at the pinned commit `9ea2373`.

## File map

| Go | rlmdb | status |
|---|---|---|
| `internal/format/*` | `format/*` (minus lockfile), consts, flags, error, cmp, idl | done, unit tests ported |
| `internal/sys/*` | `os/unix.rs`, `os/mod.rs` minus shared locks | done (data map, pread/pwrite, fdatasync) |
| `lockregion.go` | `core/reader.rs` (interface only) | done |
| `memlock.go` | (none) | in-memory region: NoLock reader table, test stand-in |
| `env.go` | `core/env.rs` | done |
| `dbi.go` | `core/dbi.rs` | done |
| `txn.go` | `core/txn.rs` | done |
| `cursor.go` | `core/cursor.rs` | done |
| `rocursor.go` | `core/ro_cursor.rs` + handle cursors of `txn_rw.rs` | done |
| `dirty.go` | `core/dirty.rs` | done (single-page pool only) |
| `wcursor.go` | `core/wcursor.rs` | done |
| `txnrw.go` | `core/txn_rw.rs` | done |
| `pageops.go` | `core/page_ops.rs` | done |
| `freelist.go` | `core/freelist.rs` | done (find_oldest: see the ledger) |
| `cursorput.go` | `core/cursor_put.rs` | done |
| `split.go` | `core/split.rs` | done |
| `cursordel.go` | `core/cursor_del.rs` | done |
| `commit.go` | `core/commit.rs` | done |
| `nested.go` | `core/nested.rs` | done |
| `spill.go` | `core/spill.rs` | done |
| `copy.go` | `core/copy.rs` | done, byte-identical to mdb_copy (-c) |
| `audit.go` | `core/audit.rs` | done (exported for tests: `Env.Audit`) |

## Tests

* `internal/format`: layout numbers of both formats, pages, nodes, meta,
  sniffing, comparators (against the literal C loop), ID lists (search,
  runs and removal against the literal C code), error texts.
* `internal/engine` (pure Go, also `CGO_ENABLED=0`): model test, write API
  (rlmdb `write_api.rs`), nested edge cases (`nested_txn.rs`), spilling,
  format selection and refused 1.0 features, NoLock (rule 2a: a long-lived
  reader across 40 commits keeps its values and every page of its
  snapshot), corrupt-input mutations (`robust_mutation.rs` style; 6000
  cases checked, `ROBUST_CASES` for more), zero allocations on the hot read
  path, benchmarks.
* `tests/engine` (C oracles from rlmdb's interop build; `RLMDB_DIR` or
  `RLMDB_BIN`): the lock-step write differential against `writeops`
  (C 0.9.36) and `writeops10` (C 1.0 at 1, 4, 16, 64 KiB pages) with every
  cursor compared after every step, then `mdb_dump`, `cpeer check-freelist`
  and a page-by-page structural comparison (port of interop `pagecmp`), and
  the Go engine reading C's result; Go-side spilling; plain and compacting
  copies byte-identical to `mdb_copy`/`mdb_copy -c`; Go reading `cpeer
  fill` data sets.
* Linux: `scripts/build-c-writeops.sh` builds the oracles in the container
  (glibc and musl/Alpine), see its header for the command.

## Design decisions (Go vs Rust)

* Structure follows rlmdb, including write cursors that hold page numbers
  plus dirty-list slot hints (`wcursor.rs`). That structure exists because
  of Rust's borrow rules; C's pointer model would be possible in Go, but
  rlmdb's code is the tested artifact whose decisions (KNOWN_C_BUGS rows)
  the port must keep. Stage 3 may revisit it for speed.
* One `Txn` type for read and write transactions (`w == nil` for read), one
  `Cursor` type (read cursor state, or an arena slot of a write txn), so
  the stage-2 `lmdb` layer stays thin.
* Read transactions: the per-txn DBI view is reset lazily with a
  generation counter (`Txn.gen`), so begin/renew is O(1) in `MaxDBs`;
  `Env.BeginRead(t)` reuses a `Txn` without allocating.
* The data map is reference counted (`sys.DataMap`): the env holds one
  reference while it is current, every transaction one (rlmdb's
  `Arc<DataMap>`); `SetMapSize` keeps live readers' slices valid.
* rlmdb's buffer pools are allocation optimisations; only the single-page
  pool is ported. Scratch buffers of puts and splits are plain
  allocations. Stage 3 measures.
* Commit gathers each contiguous run into one reused buffer and writes it
  with one `pwrite` (the `syscall` package has no `pwritev`): same bytes,
  one more copy.
* The copy target is not opened with `O_DIRECT`/`F_NOCACHE` (rlmdb does,
  best effort): no effect on the bytes written.
* `mdb_txn_commit`/`mdb_txn_abort` of a parent with an active child commit
  or abort the child first, as C does (rlmdb cannot: the borrow checker
  forbids it).
* `LockRegion` gained `NumReaders()` (rlmdb `raw_num_readers`, used by
  `Env.Info`). `ReaderInfo` lives in `internal/format` so that lockfile can
  implement the interface without importing engine. `OpenLockFunc` must
  return a nil interface (not a typed nil pointer) for "no lock file".
* The environment variable for the default format is rlmdb's
  `RLMDB_DEFAULT_FORMAT` (`engine.DefaultFormatEnv`); stage 2 may rename.

## Ledger: deviations from C and rlmdb

* NoLock (AGENTS.md rule 2a): C's MDB_NOLOCK leaves writer serialisation
  and reader safety to the caller, and rlmdb's find_oldest returns
  txnid-1 without a lock region (a soundness bug being fixed in rlmdb,
  not ported). Here the environment always has a region: without a lock
  file (NoLock, or OpenLock returning nil on a read-only file system) an
  unbounded in-memory reader table (MemLock) that FindOldest consults, and
  write transactions stay serialised by Env.writerMu. Snapshots under
  NoLock still come from the newest meta page (C's mdb_env_pick_meta), so
  commits by another process (coordinated by the caller) are seen; the
  reader's slot is published before the meta is checked again. C's
  observable outputs are kept: no lock file is created, Info.NumReaders is
  0, ReaderList is nil (C prints "(no reader locks)"), ReaderCheck is 0,
  and there is no reader limit. A second open of the same path in one
  process fails with EBUSY (rlmdb's registry).
* Every KNOWN_C_BUGS.md decision of rlmdb is kept as rlmdb implements it
  (the differential test exercises the cursor-state ones with rlmdb's
  repairs of C's side).

## Open items

* `MemLock` doubles as the test stand-in for the lock file; the real
  `internal/lockfile` is wired in at stage 2. Cross-process tests (C and
  Go on one environment at once) need it.
* rlmdb's run-buffer pool, scratch-buffer reuse and `SpareTxn` are not
  ported (allocation only).
* x86-64 Linux is cross-compiled and vetted, not run (no emulated run
  yet).
* The read-only-file-system path (OpenLock returning nil) is exercised
  only through NoLock.
* Performance: measured only roughly (`go test -bench .`); stage 3.
