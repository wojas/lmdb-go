# Porting notes: rlmdb core -> internal/engine

Status of the engine port (PLAN.md). rlmdb sources: `rlmdb/src/core/*.rs`
and `format/*`.

**Sync point: rlmdb `e48ebe0`** (its main after the restructuring that
made the repository root the Cargo workspace and vendored the C sources in
`tests/lmdb-c`; the next commit there, `9c854c2`, changes only rlmdb's
scripts). The stages before were ported from `9ea2373`, whose `rust/`
tree is `5a2ebd2` of the restructured history. "Sync with rlmdb" below
lists what happened to each commit since.

## File map

| Go | rlmdb | status |
|---|---|---|
| `internal/format/*` | `format/*` (minus lockfile), consts, flags, error, cmp, idl | done, unit tests ported |
| `internal/sys/*` | `os/unix.rs`, `os/mod.rs` minus shared locks | done (data map, pread/pwrite, fdatasync) |
| `lockregion.go` | `core/reader.rs` (interface only) | done |
| `memlock.go` | `core/reader.rs` `LocalReaders`, `core/registry.rs` `LocalShared` | in-memory region: NoLock reader table, committed txnid and writer lock; test stand-in for the lock file |
| `registry.go` | `core/registry.rs` | done (claims keyed by the data file, see the ledger) |
| `slotpool.go` | (none) | reader slots kept for reuse (not in rlmdb) |
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
  `RLMDB_BIN`; the 0.9 tests skip when the oracles' page size is not this
  process's, e.g. arm64 oracles under `GOARCH=amd64`): the lock-step write
  differential against `writeops` (C 0.9.36) and `writeops10` (C 1.0 at 1,
  4, 16, 64 KiB pages; 256 B to 64 KiB for small `MDB_DUPFIXED` items, and
  `writeops10_ndebug` for the one script on which C 1.0's assertions
  abort, rlmdb `KNOWN_C_BUGS.md` #37) with every
  cursor compared after every step, then `mdb_dump`, `cpeer check-freelist`
  and a page-by-page structural comparison (port of interop `pagecmp`), and
  the Go engine reading C's result; Go-side spilling; plain and compacting
  copies byte-identical to `mdb_copy`/`mdb_copy -c`; Go reading `cpeer
  fill` data sets.
* Linux: `scripts/build-c-writeops.sh <rlmdb root> <out dir>` builds the
  oracles from rlmdb's vendored C sources in the container (glibc and
  musl/Alpine), see its header for the command.

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
  around their operations; reopening the path meanwhile is `EBUSY` with a
  lock file, and a `NoLock` handle opened meanwhile shares the region
  (stage 4, review findings P1-4 and P1-5: `Close` cleared their slots and
  closed the lock file under them).
* `LockRegion.UnlockWriter` returns an error (the Linux mutex refuses an
  unlock from another thread, EPERM): `Commit` reports it if the commit
  itself succeeded, `Abort` ignores it as C does; the in-process writer
  lock is released anyway, so the next writer recovers the mutex once the
  owning thread ends.

### Lock order

Locks are taken in this order, outermost first; a goroutine waits for a
lock only while it holds none that comes after it:

1. `lmdb.Env.closeLock` (package lmdb, one per handle): the handle's own
   state (`Open`, the settings made before it, `Close`) and the short
   `Env` methods that use the open environment. It is never held while
   waiting for a writer lock: transactions (begin, commit, abort,
   finalizers) and copies do not take it, `SetMapSize` only tries
   `writerMu`. The one wait under it is a `NoLock` `Open` that finds a new
   file being set up by another handle and waits for the file's shared
   writer lock (3), whose holder is that other `Open`.
2. `Env.writerMu` (one per handle): its write transaction, or a plain copy.
3. The region's writer lock: the lock file's writer mutex, or under
   `NoLock` the in-memory region's, which every handle of the file in the
   process shares. Its owner-death repair (Linux) takes the reader mutex.
   A goroutine holding one handle's writer locks never begins a write
   transaction or copy in another handle of the file.
4. Leaves, held briefly and never while waiting for anything else: the
   reader mutex (slot claims, releases, reader checks), `Env.mapMu`, the
   DBI registry's and the environment registry's mutexes. Readers never
   wait for 2 or 3; they at most try 3 (`TryLockWriter`).

A map reference (`Env.pin`, a transaction's map) is not a lock: taking
one never waits, and it keeps alive what the holder uses (lock region,
files) until it is released. Rules that follow from this (stage 4 review,
round 2):

* The owner-death repair reads the meta pages through the map the waiting
  goroutine pinned before it began to wait (`Env.lockWriter`), never
  through `Env.dmap`, which `Close` clears (finding P1-3: after a `Close`
  the repair failed, and the mutex was left `ENOTRECOVERABLE` for every
  process).
* Once a write transaction has released the writer locks (2 and 3), its
  `Txn` is the next writer's (`writerState.txn`): `end` detaches the map
  before the unlock, releases it after, and touches nothing of the `Txn`
  after the unlock (finding P1-1).
* Copies and transaction ends do not take `closeLock` (finding P1-2: a
  copy waiting for `writerMu` under it, a `Close` queued for it, and the
  writer's commit waiting for it behind the `Close` deadlocked). After
  `Close`, the engine keeps what they use until their transaction ends.

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
  `lmdb` drops its reference at the end, as lmdb-go did with C's, also
  for nested transactions that end with their parent; `Txn.Owner`
  changes at every begin, so a handle can tell its transaction is
  over); cursors of an ended transaction still fail cleanly (arena
  generations).
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
  and reader safety to the caller. rlmdb tracks readers in memory too
  since 86a4d1b (the Go port did from the start). Here the environment
  always has a region: without a lock file (NoLock, or OpenLock returning
  nil on a read-only file system) an unbounded in-memory reader table
  (MemLock) that FindOldest consults, and write transactions stay
  serialised by its writer lock. The NoLock handles of one data file in
  the process share one MemLock (registry.go, rlmdb d3d9239), so this
  holds across handles. Snapshots under
  NoLock still come from the newest meta page (C's mdb_env_pick_meta), so
  commits by another process (coordinated by the caller) are seen. Readers
  use the lock-file protocol with an in-process stand-in for mti_txnid
  (the in-memory region's TxnID: set by this process's writers after the
  meta write, and to the snapshot they began from before it), plus a
  catch-up with other processes' commits while no writer of this process
  holds the shared writer lock (TryLockWriter); a plain copy adopts the newest txnid under the
  writer lock before its transaction renews (noLockSnapshot; rlmdb's
  design, commit ade31f0; stage 4, review finding P1-1: comparing the
  txnids of two copies accepted a torn copy, N's roots with N+2's txnid).
  C's observable outputs are kept: no lock file is created, Info.NumReaders is
  0, ReaderList is nil (C prints "(no reader locks)"), ReaderCheck is 0,
  and there is no reader limit. Differences from rlmdb's code that
  remain (no observable difference found): a meta copy that fails
  validation is retried if the committed txnid moved meanwhile (rlmdb
  returns the error); a write transaction takes its handle's writerMu
  before the shared writer lock (rlmdb the file's lock first), so
  SetMapSize of a handle whose writer waits for another handle's is
  EINVAL already; idle reader slots stay in the shared table per handle
  (slotpool.go), released at the handle's teardown.
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
  in `mti_numreaders`, as for a C process in TLS mode; the environment's
  teardown releases them. Not in rlmdb (to be proposed there).
* The registry of open environments (registry.go, rlmdb
  core/registry.rs since d3d9239): a data file is claimed exclusively
  with a lock file (a second open is EBUSY, before the lock file is
  touched) and shared under NoLock (any number of handles, one MemLock);
  the two kinds refuse each other, and a read-only handle on a read-only
  file system claims exclusively. Claims are by the data file's resolved
  path (before anything is opened) and by device and inode (once it
  exists, again from the opened descriptor). rlmdb claims the path given
  (the directory, or the file with MDB_NOSUBDIR) and then device and
  inode, which refuses and shares the same opens, only later in the open
  (after the lock file of the other mode was opened). Processes that open
  a data file in different modes use different lock files (`lock.mdb`,
  `<file>-lock`), in C as here: they do not exclude each other.
* `MDB_DUPFIXED` items of one byte: from stage 2 on the port inserted,
  deleted and split them as C does, but its SIZELEFT saturated where a
  64 KiB page's `mp_upper` wraps (every later insert split the page) and
  its compacting copy refused such pages as corrupt. rlmdb fixed the same
  in d039bf9 and 8dcdb22, and the port now follows its code (one
  `leaf2Count` with C's wrapping 16-bit arithmetic, SIZELEFT of a
  `P_LEAF2` page modulo 2^16, `copyUsed`; rlmdb `KNOWN_C_BUGS.md` #37 for
  C 1.0's assertion).
* MDB_CURRENT with an item too big for a node on a DUPSORT key's only
  item (rlmdb `KNOWN_C_BUGS.md` #38, found by this port's FuzzWriteModel):
  implemented from rlmdb's decision while its fix
  (`fix/current-bigdata-dupsort`) has not landed; compare with it when it
  does. The writer never creates an F_BIGDATA node in a DUPSORT database
  (dupPrepare sends such a replacement, also a put of an item the
  duplicate order calls equal, the delete-then-insert route of #15), and a
  lone F_BIGDATA item (C 1.0 writes one) is read through its overflow
  pages, which are freed, when it becomes a duplicate.
* A failed sync after the meta page write (macOS: the `F_FULLFSYNC` after
  the `O_DSYNC` write; stage 4 review, round 2, P2-4) is handled as C
  handles a failed meta page write: the old txnid is written back and the
  environment is fatal (`MDB_PANIC` for later transactions and copies).
  C 0.9.36 has no sync there (its `MDB_WRITEMAP` branch, which syncs the
  meta page on macOS, makes a failed sync fatal, without the write-back);
  rlmdb (also at e48ebe0) returns the error and does neither, which leaves
  meta N+1 on the file with N published: a later plain copy pinned N and
  copied meta N+1, naming pages it left out. To be fixed in rlmdb.
* A plain copy writes no meta page newer than its snapshot
  (`snapshotMetas`): such a page gets the snapshot's meta (the part a
  commit writes). It exists only after a commit whose meta page reached
  the file while its write or sync failed, in another process (C restores
  the old txnid after a failed write, but not when the restore fails too,
  nor with `MDB_WRITEMAP`; rlmdb not after a failed sync) or in this one
  before the entry above. C's `mdb_copy` and rlmdb copy both meta pages as
  they are, so their copy then names pages it does not contain. Copies of
  every other file stay byte-identical to `mdb_copy`'s.
* Every KNOWN_C_BUGS.md decision of rlmdb is kept as rlmdb implements it
  (the differential test exercises the cursor-state ones with rlmdb's
  repairs of C's side).

## Sync with rlmdb `5a2ebd2..e48ebe0`

The commits since the old pin (`9ea2373`, which is `5a2ebd2` in the
restructured history), oldest first:

| rlmdb | what | here |
|---|---|---|
| `760fae9` | vendor the C 0.9.36 and 1.0 sources in `tests/lmdb-c` | the oracle scripts build from them (byte-identical to the old oracle's sources) |
| `ab4c3d1` | workspace at the repository root, vendored C sources in the builds | `tests/internal/coracle`, `scripts/*`, docs: new paths |
| `d97d02a` | docs: paths and commit ids | not applicable |
| `c1caf42` | CI | not applicable |
| `86a4d1b` | NO_LOCK: track read transactions in memory | the port did from the start (rule 2a); compared, see the NoLock row |
| `d3d9239` | NO_LOCK handles of one file share the reader table and writer lock | ported (`registry.go`, `MemLock` shared, new-file set-up under the writer lock; tests) |
| `d039bf9` | 1-byte `MDB_DUPFIXED` items; the interop page audit accepts them | ported: `leaf2Count`, `copyUsed`; gopeer's audit and data set; tests |
| `5015941` | refuse a root split that would overflow the cursor stack (#36) | ported (`checkRootPush`, `splitPeer`; rlmdb's handbuilt test) |
| `8dcdb22` | `P_LEAF2` free space wraps like C's 16-bit counters (#37) | ported (`sizeLeft`, `Page.SizeLeft`; the wrap test against `writeops10_ndebug`) |
| `1ade420` | a CursorId names its arena | no change needed: a `Cursor` holds its `*Txn`, the arena is never replaced and keeps its generations (test ported) |
| `ade31f0` | NO_LOCK readers publish the committed txnid before copying the meta page | ported in stage 4 (`noLockSnapshot`); compared again, see the NoLock row |
| `774f8d0` | rlmdb-capi: clear C comparators when an abort closes a handle | no change needed: comparators live in the DBI slot that every close clears (test ported); `lmdb` has no comparator API |
| `e48ebe0` | `LockMap::read_bytes` dead code on Linux | not applicable |

Also from rlmdb's decisions, not yet a commit on its main: #38 (see the
ledger).

## Open items

* `MemLock` doubles as the test stand-in for the lock file; the real
  `internal/lockfile` is wired in at stage 2. Cross-process tests (C and
  Go on one environment at once) need it.
* x86-64 Linux is cross-compiled and vetted, not run (no emulated run
  yet).
* The read-only-file-system path (OpenLock returning nil) is exercised
  only through NoLock.
* Performance: see `tests/bench/RESULTS.md` (stage 3).
