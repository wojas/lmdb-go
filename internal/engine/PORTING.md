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
| `slotpool.go` | (none; C's TLS-mode slot reuse) | idle reader slots kept for the next read transaction |
| `env.go` | `core/env.rs` | done |
| `dbi.go` | `core/dbi.rs` | done |
| `txn.go` | `core/txn.rs` | done |
| `cursor.go` | `core/cursor.rs` | done |
| `rocursor.go` | `core/ro_cursor.rs` + handle cursors of `txn_rw.rs` | done |
| `dirty.go` | `core/dirty.rs` | done (page pool for single pages and overflow runs) |
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
* rlmdb's buffer pools and spare containers are allocation
  optimisations; stage 3 ported them in Go form (see "Stage 3" below).
* Commit gathers each contiguous run into one reused buffer and writes it
  with one `pwrite` (the `syscall` package has no `pwritev`): same bytes,
  one more copy. Stage 3 tried a raw `pwritev` on Linux: no measurable
  gain (tmpfs), not kept.
* The copy target is not opened with `O_DIRECT`/`F_NOCACHE` (rlmdb does,
  best effort): no effect on the bytes written.
* `mdb_txn_commit`/`mdb_txn_abort` of a parent with an active child commit
  or abort the child first, as C does (rlmdb cannot: the borrow checker
  forbids it).
* `LockRegion` gained `NumReaders()` (rlmdb `raw_num_readers`, used by
  `Env.Info`). `ReaderInfo` lives in `internal/format` so that lockfile can
  implement the interface without importing engine. `OpenLockFunc` must
  return a nil interface (not a typed nil pointer) for "no lock file".
* The environment variable for the default format is lmdb-go v2's
  `LMDBGO_DEFAULT_FORMAT` (`engine.DefaultFormatEnv`, v2's values `0.9`,
  `09`, `9`, `1.0`, `10`) with the semantics of rlmdb's
  `RLMDB_DEFAULT_FORMAT` (experimental; read at every open of a new
  environment).

* The transaction's DB table is `me_maxdbs` long; `Txn.ndbs` (C's
  `mt_numdbs`) bounds every loop over it, and the top-level write
  transaction reuses its predecessor's table (C reuses `me_txn0`), so a
  write transaction costs O(DBs in use), not O(`MaxDBs`) (stage 2:
  lmdb-go's tests set `MaxDBs` to 65536).
* `Env.Close` with transactions still alive (stage 2, for lmdb-go's
  finalizers; C requires them all to end first): `BeginRead`, `Renew` and
  write `BeginTxn` fail with `EINVAL`, and the live ones keep working
  until they end. The teardown (pooled slots, lock region, files,
  registry entry) runs when the last data map is unmapped
  (`Env.mapFreed`): every transaction holds a map reference from its begin
  to its end, Reset included, and takes it before it touches the lock
  region; the environment holds one on its current map until `Close`. So
  surviving transactions keep their reader slots and the process's
  liveness lock (their snapshots stay protected against writers here and
  in other processes) and never use a torn-down region, without a lock
  around their operations; reopening the path meanwhile is `EBUSY`
  (stage 4, review findings P1-4 and P1-5: `Close` cleared their slots and
  closed the lock file under them).
* `LockRegion.UnlockWriter` returns an error (the Linux mutex refuses an
  unlock from another thread, EPERM): `Commit` reports it if the commit
  itself succeeded, `Abort` ignores it as C does; the in-process writer
  lock is released anyway, so the next writer recovers the mutex once the
  owning thread ends.

### Stage 3: performance (divergences from rlmdb's structure)

Measurements and what is left: `tests/bench/RESULTS.md`. Allocation
counts of the API's hot paths are pinned by `lmdb/alloc_test.go`.

* One `Txn` per environment for top-level write transactions
  (`writerState.txn`), reused by each as C reuses `me_txn0`: its DB
  table, dirty list and page pool (C's `me_dpages`; it keeps as many spare
  pages as the last transaction used, at most 256 MiB), cursor arena,
  spill set, page lists and every scratch buffer below. rlmdb keeps the
  containers in a `SpareTxn` (bounded to 4096 entries and 16 cursors);
  here the whole `Txn` stays, unbounded except for the pool, as the
  owner's allocation rules ask. A `Txn` must not be used after its
  `Commit` or `Abort` once another write transaction began (package
  `lmdb` drops its reference at the end, as lmdb-go did with C's);
  cursors of an ended transaction still fail cleanly (arena generations).
  The arena hands slots out in the order a new one would, so cursor
  fixups run in the same order as before.
* A parent keeps its last nested transaction's memory (`spareChild`) for
  the next child; a child borrows its parent's spare-page lists by taking
  them over (and gives them back), instead of copying them buffer by
  buffer; the merge of a child's dirty list builds into the index it
  replaced last time.
* Scratch space of the write path lives in the write transaction
  (`wstate`): a LIFO stack of page buffers for the temporary copies of
  splits and merges (they nest as the operations recurse up the tree),
  the node offsets of a split per recursion depth, the keys and value of
  `nodeMove`/separator updates, the DUPSORT put's old data, new sub-page
  and sub-DB record (C's `me_pbuf`), the free DB keys of `reclaimRun` and
  `freelistSave` (distinct: the latter's puts can call the former; a key
  passed to a comparator through a function value escapes, so a local
  array would be heap-allocated), the flush list, `me_pghead`'s memory
  and `freelistSave`'s copies. rlmdb allocates most of these per call.
* Overflow runs are pooled by length like single pages (C mallocs and
  frees them).
* `mdb_page_touch` copies a page as C's `mdb_page_copy` does, skipping
  the gap between `mp_lower` and `mp_upper` (zeroed instead of left as
  the buffer was; rlmdb copies the whole page).
* Tried and not kept: a branch-free binary search in `searchPage`. A
  lower-bound search is wrong where C's probe sequence matters (pages
  need not be sorted after the C behaviours the port keeps, e.g. keys
  replaced in place; the write differential caught it), and masking C's
  own step made lookups 15-40% slower on Apple M1: with the branch the
  CPU speculates into the next probe's load.

## Ledger: deviations from C and rlmdb

* NoLock (AGENTS.md rule 2a): C's MDB_NOLOCK leaves writer serialisation
  and reader safety to the caller, and rlmdb's find_oldest returns
  txnid-1 without a lock region (a soundness bug being fixed in rlmdb,
  not ported). Here the environment always has a region: without a lock
  file (NoLock, or OpenLock returning nil on a read-only file system) an
  unbounded in-memory reader table (MemLock) that FindOldest consults, and
  write transactions stay serialised by Env.writerMu. Snapshots under
  NoLock still come from the newest meta page (C's mdb_env_pick_meta), so
  commits by another process (coordinated by the caller) are seen. With
  no mti_txnid to order against, this process's meta writes are bracketed
  by a sequence word (Env.metaSeq, a seqlock): a reader accepts a meta copy
  only if no meta write of this process began or ended around it, reads
  the other page while one is in progress, and publishes its txnid in the
  slot before the final check (stage 4, review finding P1-1: comparing the
  txnids of two copies accepted a torn copy, N's roots with N+2's txnid).
  C's observable outputs are kept: no lock file is created, Info.NumReaders is
  0, ReaderList is nil (C prints "(no reader locks)"), ReaderCheck is 0,
  and there is no reader limit. A second open of the same path in one
  process fails with EBUSY (rlmdb's registry).
* Reader slots (stage 4, `slotpool.go`): C with `MDB_NOTLS` (lmdb-go's
  mode) and rlmdb claim a slot under the reader mutex at every read
  transaction's begin and free it (`mr_pid = 0`) at the end. Here the end
  leaves the slot idle and owned (`mr_txnid = (txnid_t)-1`, as C's TLS
  mode leaves a thread's slot) in a bounded per-`Env` pool, and the next
  read transaction takes it without the mutex (no cgo call on the read
  path, AGENTS.md rule 1); C's `MDB_BAD_RSLOT` check (`mr_pid` ours and
  idle) runs before reuse, and a slot that fails it is dropped. Bound:
  `min(MaxReaders/4, 64)`; a claim at index `MaxReaders*3/4` or above, or
  `MDB_READERS_FULL`, turns pooling off until a claim lands lower. Other
  processes see the idle slots (`mdb_stat -r`: txnid `-`) and count them
  in `mti_numreaders`, as for a C process in TLS mode; `Env.Close`
  releases them. Not in rlmdb (to be proposed there).
* `MDB_DUPFIXED` items of one byte (stage 2): rlmdb refuses `P_LEAF2`
  keys shorter than 2 bytes (`ksize.checked_sub(2)` in `insert_leaf2`
  and the `P_LEAF2` delete) and fails with `MDB_PAGE_FULL` on the second
  duplicate; C adds 1 to `mp_upper` per item there (indx_t arithmetic).
  The port does what C does, byte for byte (`tests/engine` profile
  `dupfixed1`). To be fixed in rlmdb.
* Every KNOWN_C_BUGS.md decision of rlmdb is kept as rlmdb implements it
  (the differential test exercises the cursor-state ones with rlmdb's
  repairs of C's side).

## Open items

* `MemLock` doubles as the test stand-in for the lock file; the real
  `internal/lockfile` is wired in at stage 2. Cross-process tests (C and
  Go on one environment at once) need it.
* x86-64 Linux is cross-compiled and vetted, not run (no emulated run
  yet).
* The read-only-file-system path (OpenLock returning nil) is exercised
  only through NoLock.
* Performance: see `tests/bench/RESULTS.md` (stage 3).
