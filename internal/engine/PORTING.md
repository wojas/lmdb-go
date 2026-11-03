# Porting notes: rlmdb core -> internal/engine

Status of the engine port (PLAN.md). rlmdb sources: `rlmdb/src/core/*.rs`
and `format/*`.

**Sync point: rlmdb `7c60f17`** (its main with the limits on databases
and reader slots, ledger rows 53 to 60, and the Windows fixes of the file
length and of the open race). The previous sync points were `3a52745`
(its main with the review-round fixes and the C references at the 0.9.37
and 1.0.3 releases), `3df6995` (its main with the BSDs, Windows and
big-endian testing) and `e48ebe0` (its main after the restructuring that made the
repository root the Cargo workspace and vendored the C sources in
`tests/lmdb-c`); the stages before were ported from `9ea2373`, whose
`rust/` tree is `5a2ebd2` of the restructured history. The "Sync with
rlmdb" sections below list what happened to each commit since; rlmdb's
BSD commits up to `3df6995` are the BSD port's (branch `stage7-bsd`).

## File map

| Go | rlmdb | status |
|---|---|---|
| `internal/format/*` | `format/*` (minus lockfile), consts, flags, error, cmp, idl | done, unit tests ported |
| `internal/sys/*` | `os/unix.rs`, `os/windows.rs`, `os/mod.rs` minus shared locks and the lock file | done (data map, pread/pwrite, fdatasync, file opens; `*_unix.go`, `*_windows.go`) |
| `lockregion.go` | `core/reader.rs` (interface only) | done |
| `memlock.go` | `core/reader.rs` `LocalReaders`, `core/registry.rs` `LocalShared` | in-memory region: NoLock reader table, committed txnid and writer lock; test stand-in for the lock file |
| `registry.go` | `core/registry.rs` | done (claims keyed by the data file and the lock file, see the ledger) |
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
  sniffing, comparators (against the literal C loops of both byte
  orders), ID lists (search, runs and removal against the literal C code),
  error texts.
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
  abort, rlmdb `KNOWN_C_BUGS.md` row 37) with every
  cursor compared after every step, then `mdb_dump`, `cpeer check-freelist`
  and a page-by-page structural comparison (port of interop `pagecmp`), and
  the Go engine reading C's result; Go-side spilling; plain and compacting
  copies byte-identical to `mdb_copy`/`mdb_copy -c`; Go reading `cpeer
  fill` data sets; `MDB_CURRENT` replacements against C's
  `mdb_cursor_del` + `mdb_cursor_put` (the `cr` op, row 38; also on a 2 KiB
  0.9 file, which the harness can start both sides from) and C-made files
  with a DUPSORT item on overflow pages, changed by Go and audited by C;
  plain copies with a real lock file (after a failed meta sync, by another
  process, equal to `mdb_copy`; of a file with a forged newer meta page).
  It takes 2 to 4 minutes natively and 7 to 9 under
  emulation (`GOARCH=amd64` under Rosetta with x86-64 oracles, or the
  x86-64 Linux container), where `go test`'s default 10-minute timeout
  fails it on a loaded machine: run it there with `-timeout 30m`.
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

* The transaction's DB table is as long as the environment's DBI table
  when the transaction begins (`me_numdbs`, at least `minDBTable`, 16
  slots), not `me_maxdbs` as in C and rlmdb, and grows when the
  transaction touches a later slot (`Txn.reach`: a handle opened after it
  began, by it or by a child; doubling, up to `me_maxdbs`). A reused `Txn`
  keeps what its table grew to (`Env.BeginRead`, the top-level write
  transaction, a parent's spare child). `Txn.ndbs` (C's `mt_numdbs`)
  bounds every loop over it, and the top-level write transaction reuses
  its predecessor's table (C reuses `me_txn0`), so a transaction costs
  O(DBs in use), not O(`MaxDBs`) (stage 2: lmdb-go's tests set `MaxDBs`
  to 65536; stage 20: at that setting a table of `MaxDBs` slots was 6.8
  MiB, made anew for every read transaction a `Txn` pool did not supply).
  `Txn.dbe` does not grow the table, so that it stays inlinable: the slot
  indices that do not come from a checked handle (`dbiEntry`), a core DB
  or `ndbs` are reached first (`openDB`, `adoptRegistration`, `createDB`,
  `recreateDB`, and the parent at the end of a child). Handles behave as
  with tables of `me_maxdbs` slots: `MDB_DBS_FULL` at `MaxDBs`, `EINVAL`
  beyond it, a handle opened elsewhere usable at once. The environment's
  sequence numbers (`dbiRegistry.seqs`, 4 bytes a slot) stay `me_maxdbs`
  long: they are read without a lock on every use of a handle.
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
  and there is no reader limit. A meta copy that fails validation is
  retried if the published txnid moved meanwhile, at most 16 failed copies
  per begin, in both reader paths (rlmdb 3811bea; this port retried
  without bound under NoLock and not at all with a lock file before).
  Differences from rlmdb's code that remain (no observable difference
  found): a write transaction takes its handle's writerMu
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
  core/registry.rs since d3d9239, as of 4762f54): a data file is claimed
  exclusively with a lock file (a second open is EBUSY, before the lock
  file is touched) and shared under NoLock (any number of handles, one
  MemLock); the two kinds refuse each other, and a read-only handle on a
  read-only file system claims exclusively. Claims are by the data file's
  resolved path (before anything is opened) and by device and inode (once
  it exists, from `stat` before the lock file is touched, again from each
  opened descriptor, the meta descriptor too). The lock file is claimed by
  device and inode once it is open, before it is locked
  (`lockfile.OpenClaimed` with the `LockClaim` of `OpenLockFunc`). A
  descriptor a refused open cannot keep is closed, or parked in the registry
  until the handle holding that lock file is gone (`regParked`, `Dispose`,
  also for `SniffFormat`). rlmdb claims the path given (the directory, or
  the file with MDB_NOSUBDIR), then opens the data file read-only to claim
  it and sniffs its format from that descriptor; here `stat` gives the
  identity without a descriptor to park, and the sniff opens by name after
  it. Both refuse and share the same opens. Processes that open a data file
  in different modes use different lock files (`lock.mdb`, `<file>-lock`),
  in C as here: they do not exclude each other.
* `MDB_DUPFIXED` items of one byte: from stage 2 on the port inserted,
  deleted and split them as C does, but its SIZELEFT saturated where a
  64 KiB page's `mp_upper` wraps (every later insert split the page) and
  its compacting copy refused such pages as corrupt. rlmdb fixed the same
  in d039bf9 and 8dcdb22, and the port now follows its code (one
  `leaf2Count` with C's wrapping 16-bit arithmetic, SIZELEFT of a
  `P_LEAF2` page modulo 2^16, `copyUsed`; rlmdb `KNOWN_C_BUGS.md` row 37 for
  C 1.0's assertion).
* MDB_CURRENT with an item too big for a node on a DUPSORT key's only
  item (rlmdb `KNOWN_C_BUGS.md` row 38, found by this port's FuzzWriteModel):
  implemented from rlmdb's decision first, aligned with its fix (`dc90f21`)
  in the sync to `3df6995`. The writer never creates an F_BIGDATA node in
  a DUPSORT database: such an MDB_CURRENT is caught right after the
  positioning and runs as mdb_cursor_del + mdb_cursor_put
  (`replaceCurrent`, which the row 15 route shares; the transaction fails if
  the put after the delete fails), and so does a put of an item the
  duplicate order calls equal that does not fit (in an MDB_MULTIPLE batch
  the batch goes on); the test is mdb_node_add's (`overflowsNode`). A
  lone F_BIGDATA item (C writes one) is read through its overflow pages
  when it becomes a duplicate, and they are freed before the sub-page or
  sub-DB root is built (an item longer than the maximum key size is
  MDB_CORRUPTED).
* MDB_CURRENT on a duplicate in a sub-DB that C leaves in a leaf searches
  do not look in (rlmdb `KNOWN_C_BUGS.md` row 56, found by this port's
  FuzzWriteModel; C 0.9 and C 1.0 alike). Fixed in this port first
  (229ea68), then in rlmdb (`b778cca`, after the pinned `3df6995`), and
  aligned with rlmdb's fix. C keeps the new item in the old one's leaf at
  its index: the first item of a leaf other than the leftmost, replaced by
  one of another size that sorts before the branch key above the leaf
  (0.9's ITS#8447 path, without fix_parent), or the last item of a leaf
  other than the rightmost, replaced by one of any size at or after the
  next leaf's branch key, which a delete of that leaf's first item leaves
  behind (0.9 and 1.0, MDB_DUPFIXED too). MDB_GET_BOTH then does not find
  the item, and later puts land out of order. Exactly these replacements
  are caught right after the row 38 check, before the spill check and the
  page touch, and run as mdb_cursor_del + mdb_cursor_put
  (`currentDupOutsideLeaf`, then `replaceCurrent`); the branch keys are
  compared as searches compare them (`CompareInPage`). As in rlmdb this
  includes MDB_MULTIPLE with one item (C has the same bug there), which
  then reports one item stored; two or more items are C's in-place
  replacement. The files are C's for those two calls
  (`TestDifferential*CurrentDupOutsideLeaf`, the other cursor on the
  replaced item where it stays in place;
  `TestCgoCurrentMultipleOutsideLeaf` for MDB_MULTIPLE against C 0.9
  through `tests/cgolmdb`).
* A failed sync after the meta page write (macOS: the `F_FULLFSYNC` after
  the `O_DSYNC` write; stage 4 review, round 2, P2-4; rlmdb
  `KNOWN_C_BUGS.md` row 41) is handled as C handles a failed meta page write:
  the old txnid is written back and the environment is fatal (`MDB_PANIC`
  for later transactions and copies). C 0.9.36 and 1.0 return the sync's
  error and keep the page there; rlmdb did the same until `bf3a0c0`, whose
  version this is (the test hook is its `fail_next_meta_sync`:
  `Env.FailNextMetaSync`).
* A plain copy writes no meta page newer than its snapshot
  (`snapshotMetas`, rlmdb's `hide_newer_meta`, `bf3a0c0`): if the other
  meta page names a newer txnid and the snapshot's own page holds the
  snapshot, the other page gets the snapshot's meta (the part a commit
  writes); otherwise both are copied as they are (until the sync to
  `3df6995` this port failed with MDB_CORRUPTED when the own page did not
  hold the snapshot, which only a racing writer of another process causes
  without a lock file). Such a newer page exists only after a commit whose
  meta page reached the file while its write or sync failed (C on macOS
  after a failed `F_FULLFSYNC`; anyone when the write-back failed too), or
  without a lock file with a writer of another process racing the copy.
  C's `mdb_copy` copies both pages as they are, so its copy then names
  pages it does not contain (C 1.0 does not even open such a file). Copies
  of every other file stay byte-identical to `mdb_copy`'s.
* A compacting copy stops with MDB_CORRUPTED as soon as it would hold more
  pages than the source snapshot (`copyWriter.limit`, rlmdb `5d8bc38`,
  `KNOWN_C_BUGS.md` row 44): C's mdb_env_cwalk writes the shared or cyclic
  pages of a damaged tree once per path to them, without bound.
* rlmdb's ledger rows 39 to 44, here: row 39 (OpenBSD's `msync` of a range
  that does not start on an OS page) is the BSD port's; row 40 (the robust
  mutex repair validates the meta pages) holds here as in rlmdb
  (`pickMetaIn` validates both pages, and a failed repair leaves the
  mutex unrecoverable, `internal/lockfile` `TestWriterRepairFailure`); rows 41
  and 44 as above; row 42 (`OpenProcess` with `SYNCHRONIZE`) and row 43 (the
  Windows lock-file size in 64 bits) are the Windows port's, which follows
  both (`internal/lockfile/filelock_windows.go`).
* rlmdb's ledger rows 45 to 52, here: row 45 as in rlmdb (`takeSnapshot`
  pins what mti_txnid names, also on read-only handles while it is 0; test
  hook `Env.AfterNextMetaWrite`); row 46 (`checkReusable`); row 47
  (`lockfile.OpenClaimed`, `errLocksRemoved`; on Windows since the sync to
  `7c60f17`: a named mutex that does not exist); row 48 (`Layout.MinPageSize`);
  row 49 (`setSnapshot`); row 50 (`walk`, `rcursor.step`, `Txn.walkLimit`,
  `nextTxnidKey`, the limits of `drop0Walk`, the check in `freelistSave`);
  row 51 has nothing to port (no chunked map); row 52: as C and rlmdb, nothing
  invalidates the map after a failed meta write, so on OpenBSD a plain copy
  taken from a map differs from this port's in that page
  (`sameButFailedMetaPage` in `tests/engine`).
* rlmdb's ledger rows 53 to 60 (58 and 59 are not in the ledger at
  `7c60f17`), here: rows 53 and 55 are `mdb_load`'s (not here); row 54
  (`format.MaxDBsLimit`: `SetMaxDBs` and `Open` beyond 1,048,576 are
  EINVAL); row 56 above; row 57 (Windows: a 0.9 data file is only
  extended, holding the writer mutex next to other processes:
  `Env.extendForMap`, `extendUnder`, `sys.ExtendFile`,
  `sys.MapNeedsFileLength`; `sys.MapData` maps what the file has); row 60
  (`format.MaxReadersLimit`: `SetMaxReaders` and `Open` beyond 1,048,576
  are EINVAL, before any file is created; an existing lock file with a
  larger table is attached to, as before).
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
| `5015941` | refuse a root split that would overflow the cursor stack (row 36) | ported (`checkRootPush`, `splitPeer`; rlmdb's handbuilt test) |
| `8dcdb22` | `P_LEAF2` free space wraps like C's 16-bit counters (row 37) | ported (`sizeLeft`, `Page.SizeLeft`; the wrap test against `writeops10_ndebug`) |
| `1ade420` | a CursorId names its arena | no change needed: a `Cursor` holds its `*Txn`, the arena is never replaced and keeps its generations (test ported) |
| `ade31f0` | NO_LOCK readers publish the committed txnid before copying the meta page | ported in stage 4 (`noLockSnapshot`); compared again, see the NoLock row |
| `774f8d0` | rlmdb-capi: clear C comparators when an abort closes a handle | no change needed: comparators live in the DBI slot that every close clears (test ported); `lmdb` has no comparator API |
| `e48ebe0` | `LockMap::read_bytes` dead code on Linux | not applicable |

Also from rlmdb's decisions, then not yet a commit on its main: row 38, which
landed in `dc90f21` (next section).

## Sync with rlmdb `e48ebe0..3df6995`

The engine's sync (branch `stage7-sync2`), oldest first. The Windows
commits are the Windows port's (next section), the BSD ones the BSD
port's (`stage7-bsd`).

| rlmdb | what | here |
|---|---|---|
| `9c854c2` | scripts: clippy in the Linux container too | not applicable |
| `e4a5ad9` | FreeBSD, OpenBSD, NetBSD and DragonFly with C's lock primitives (row 39) | BSD port |
| `bfef7e6` | CI and docs for the BSDs | BSD port |
| `12a6731` | the OpenBSD `msync` row is number 39 | ledger numbering only (row 39: BSD port) |
| `ce88ec6` | the shared C-oracle build directory is locked and rebuilt when stale | not applicable: the tests here take built oracles (`RLMDB_DIR`, `RLMDB_BIN`, `LMDB_ORACLE_BIN`) and build none into a shared directory |
| `bf3a0c0` | a failed sync after the meta write is fatal and restores the old txnid; a plain copy never emits a meta newer than its snapshot (row 41) | the port had both since stage 4; compared: a copy whose own meta page does not hold the snapshot no longer fails (ledger); rlmdb's tests ported (`Env.FailNextMetaSync`, `TestMetaWriteFailureIsFatal` with another `NoLock` handle, `TestFailedFullFsyncIsFatal` on macOS, `tests/engine` `TestCopyAfterFailedMetaSync` and `TestPlainCopyHidesANewerMeta` with a real lock file and C's `mdb_copy`) |
| `ee87eb1` | ledger row 40: the robust-mutex repair validates the meta pages | the port does the same (ledger); no change |
| `3811bea` | retry a torn meta copy when the published txnid moved | ported: both reader paths, at most 16 failed copies (`maxTornCopies`; the NoLock path retried without bound, the lock-file path not at all); rlmdb's three tests (`torncopy_test.go`, through `testHookSnapshotMeta`) |
| `dc90f21` | MDB_CURRENT never stores a DUPSORT value on overflow pages; repair files where C did (row 38) | compared with the port's version and aligned (ledger: the check before the spill and touch, mdb_node_add's test, the MDB_MULTIPLE batch going on, the overflow pages freed before the sub-DB root is allocated, the maximum-size check, 16-bit `mp_upper` of a `P_LEAF2` sub-DB root); tests ported (`TestDifferential*CurrentSingleItem` with the `cr` op, `TestSingleItemOnOverflowPages*`, `TestDifferential10DupfixedItemsOverAQuarterPage` with 64 KiB pages, `TestDupsortCurrentReplacesThroughASubDB`); `pagecmp` leaves out 0.9's uninitialised `mp_pad` of ordinary pages (rlmdb's `canonical09`) |
| `9155a5d` | the NO_LOCK snapshot stress test reserves a 1 GiB map | ported together with the test (`TestNoLockReadersPinTheSnapshotTheyRead`), which came with `ade31f0` and was not ported then |
| `2e5101f` | tests and the C page audit no longer assume a little-endian host | mirrored: `pagecmp` reads `mn_lo`/`mn_hi` in host order, the literal `mdb_cmp_cint` loops of both byte orders, the meta tests' field pokes, a byte-swapped magic is MDB_INVALID; the C page audit is rlmdb's (`cpeer check-freelist`, built from its sources). The s390x run found one engine bug here: `Page.OverflowPages` combined `mp_lower`/`mp_upper` as on little-endian hosts |
| `8230c7c` | `scripts/linux-test.sh`: `LINUX_TEST_TARGET` runs the suites cross-built under QEMU | here `LINUX_TEST_PLATFORM=linux/s390x` (image `golang:1.25-trixie`) runs everything under QEMU; `LMDB_TEST_NO_ROBUST_MUTEX` for its `INTEROP_NO_ROBUST_MUTEX` |
| `5773e0a` | nightly big-endian job; byte order documented | the docs (`README.md`, `lmdb/DEVIATIONS.md`, `internal/format/arch_unsupported.go`): tested on s390x under emulation; no CI here |
| `56e5f45` | `diff_current_single_item` does not assume a little-endian host | covered by `pagecmp`'s host-order node fields |
| `0bf0553`, `24571fa`, `bf7d115`, `a933f11` | Windows | Windows port (next section) |
| `44ca757` | the cross route registers the VM's amd64 handler alongside QEMU | mirrored in `scripts/linux-test.sh`: missing handlers (QEMU's, Rosetta) are registered from a privileged holder container, never removed |
| `0092556` | the C cursor and write helpers read their scripts from stdin (FreeBSD `ARG_MAX`) | not applicable: `writeops` already reads stdin here, and the Go tests do not run `cursorops` |
| `1009bbd` | FreeBSD tested in CI | BSD port |
| `63b591c`, `b3ed51e`, `d27e68f`, `03fccb3`, `86563af`, `7dcf511`, `ea1bbdb` | MSVC builds of the C test programs, Windows CI, the mtest seed shim, the C library's DLL, rlmdb-tools' `atoi` and `maxreaders=-1` | Windows port / not applicable (no C API, no mtest, no C-tool clones here) |
| `5d8bc38` | a compacting copy stops when it outgrows its source (row 44) | ported (`copyWriter.limit`; rlmdb's hand-built test, `TestCompactingCopyOfSharedPagesStops`) |
| `3df6995` | interop holds outlast slow scenarios; stale reader and row 42 on Windows | the holds: `tests/lockfile` gives its C peers' `--until-stdin` holds 600 s (`holdSecs`); row 42: Windows port |

## Sync with rlmdb `3df6995..3a52745`

Branch `stage11-rlmdb-sync3`, oldest first. "Already there" means the
test named also passes on the code before this sync; the tests of
"ported" rows fail there.

| rlmdb | what | here |
|---|---|---|
| `bd5d644` | docs: Windows tested in CI with MSVC and MinGW | not applicable (rlmdb's CI) |
| `f679226` | a differential dump mismatch shows where, and if it was a misread | ported: `tests/engine` `dumpDiff`, the second dump, `keepEnv` |
| `9dc16eb` | the Windows toolchain file for `windows-test.sh` is in the repository | ported: `scripts/windows-env.cmd`, the script's header points at it |
| `3a97557` | a leaked nested transaction blocks every ancestor | not applicable as such: Go cannot leak a child that took its parent's state (`mem::forget`), and the end of an ancestor ends its descendants, as in C; already there, tested (`TestActiveDescendantsAtAnyDepth`, `TestFailedDescendantFailsTheCommit`, `lmdb` `TestTxn_Commit_descendants`, from `capi_write.c`) |
| `a91d9f5` | read-only readers pin mti_txnid's snapshot also while it is 0 (row 45) | ported (`takeSnapshot`, `Env.AfterNextMetaWrite`, `tests/engine` `TestReadOnlyReaderSkipsUnpublishedFirstCommit` with C's `mdb_dump` and `mdb_dump10`) |
| `4762f54` | refuse an environment under another name before its lock file | the data file under another name was refused before the lock file already (`register`); ported: the lock file's claim, parked descriptors, the meta descriptor's claim (ledger; `TestEnvUnderAnotherNameIsRefusedBeforeItsLockFile`, `tests/engine` `TestSecondOpenUnderAnotherNameLeavesTheFirstAlone` with C's `mdb_stat -r`) |
| `fe9fd67` | `mdb_del` does not read the data of a non-DUPSORT database | already there, tested (`lmdb` `TestTxn_Del_ignoresVal`) |
| `769f352` | a read cursor searches with the comparators set after it opened | ported (`Cursor.refreshOrders`; `TestComparatorsSetAfterAReadCursorOpened`); `lmdb` has no comparator API, so only the engine's callers see it |
| `54a8645` | `mdb_cmp` and `mdb_dcmp` take the C function from the core | no C API here; the comparators already live in the DBI slot that a reopen restores, tested (`TestComparatorsFollowAReopenedHandle`) |
| `0bfd914` | `mdb_load -N` does not count skipped records toward a batch | not applicable (no `mdb_load` here) |
| `16feee5` | `mdb_cursor_get` checks a failed transaction before its operation | already there, tested (`lmdb` `TestCursor_Get_failedTxn`) |
| `a59627f` | `mdb_env_copyfd` of descriptor -1 is `EBADF` | already there, tested (`lmdb` `TestEnv_CopyFD_badDescriptor`) |
| `2fb635e` | refuse a SysV set at the lock file's key that is not ours (row 46) | ported (`checkReusable`, `sysvHasThird`; `TestSysVCreateRefusesALargerSet`, `TestSysVCreateReusesASetOfTwo`) |
| `7d9054f` | an opener that finds the semaphores removed probes again (row 47) | ported (`lockfile.OpenClaimed`; `lmdb` `TestOpenCloseRace`, which fails within a few dozen opens without it; `TestOpenWithItsSemaphoresRemoved`) |
| `607fc5a` | take the data file's size before mapping it | ported (`sys.MapData` on Unix; on Windows the size was read first already) |
| `7b11099` | refuse 0.9 files of pages below 2 KiB (row 48) | ported (`Layout.MinPageSize`; `TestSmall09PagesAreRefused`; the root-split test and the fuzz seeds at 2 KiB) |
| `cafb356` | `leaf_size` computes the overflow node size without wrapping | already the right value (Go's signed `int`); the order follows rlmdb's; tested (`TestLeafSizeOfAnOverflowNode`) |
| `b405e6a` | `page_merge` adds a depth change of a lying `md_depth` as C does | already the right value (Go's `int16` wraps instead of panicking); tested (`TestMergeWithALyingDepthCommits`) |
| `f4003a3` | no transaction begins on a snapshot without its meta pages (row 49) | ported (`setSnapshot`; `TestSnapshotsWithoutTheirMetaPagesAreRefused`) |
| `0d455ff`, `0c9c238` | walks over branch pages that share children stop; the limit is the snapshot's pages plus the allocations (row 50) | ported (`walk`, `rcursor.step`, `Txn.walkLimit`, `Txn.allocations`, `nextTxnidKey`, `FreePages`; `TestWalksOfAFreeDBWithSharedChildrenStop`, `TestCursorWalksOverSharedChildrenStop`, `TestQueueWalksInAWriteTxnAreNotBoundedByTheFile`); the leaf-step fast path is untouched |
| `270fe7a` | dropping a tree of shared children fails instead of listing pages twice (row 50) | ported (`drop0Walk`, `freelistSave`; `TestDropsOfSharedChildrenStop`) |
| `bb97159` | link `-ldl` only where `dlopen` is a library of its own | already there (`tests/cmd/build-c-oracles` `hasLibdl`) |
| `5f8b5c8` | the shared-children dump test reads the tools' output as text | not applicable (no C-tool clones here) |
| `6329f7f` | the Windows env template says how to put build output and scratch on another drive | ported with `9dc16eb` |
| `c37a067` | the shared C builds are keyed by toolchain too | not applicable (the tests here take built oracles) |
| `c6a6760` | tests that touch files behind an open environment work on OpenBSD | already there in this port's own form (`writeAsCommit`, `sameButFailedMetaPage`); the new tests use it (`writeBehind`) |
| `26c2fc5` | OpenBSD's `getopt` messages and `mdb_stat -r` exit code | not applicable (no C-tool clones here; the tests here do not check `mdb_stat -r`'s exit code) |
| `fd6b00a`, `4e8d5c5` | the mtest programs' `rand()` shim | not applicable (no mtest here) |
| `27b108b` | the interop matrix runs on OpenBSD | rlmdb's harness; the Go peer runs in it as `Peer::External` |
| `fc1d99a` | docs: OpenBSD tested by hand in a VM | not applicable (rlmdb's status; this port's is `PLAN.md` "The BSDs") |
| `7de9dfb` | the Windows `maxreaders=-1` load case runs the C tool only | not applicable (no `mdb_load` here) |
| `81cc892`, `0f902b4` | `Peer::External`, the scenario matrix with an external peer | rlmdb's harness; `tests/gopeer` is that peer |
| `a1ec6cc` | ledger rows 51 and 52 | ledger above; row 52 as C (nothing to change) |
| `3a52745` | the C references are the 0.9.37 and 1.0.3 releases | `Env.EngineVersion` (0.9.37, 1.0.3; `Version` stays 0.9.90), the comments and docs that name the reference; `mdb.c` is unchanged, so the line numbers cited hold |

## Sync with rlmdb `3a52745..7c60f17`

Branch `stage19-rlmdb-sync4` (which merged `stage11-rlmdb-sync3`),
oldest first, with the same meaning of "already there" and "ported".

| rlmdb | what | here |
|---|---|---|
| `e584fe9` | `mdb_load` takes only the `maxreaders` value `mdb_dump` writes (row 53) | not applicable (no `mdb_load` here) |
| `d7f23e1` | AGENTS: changes to what is written are opt-in features, never the default | the rule holds here through `AGENTS.md` rule 3 (layout exactness is the default oracle, a divergence needs rlmdb's justification); no opt-in feature exists |
| `3b06072` | refuse `max_dbs` beyond 1,048,576 (row 54), junk `mdb_load` headers (rows 53, 55) | ported for `max_dbs` (`format.MaxDBsLimit`; `lmdb` `SetMaxDBs`, `engine.Open`; `TestSetMaxDBsLimit`, `TestOpenRefusesCountsBeyondTheLimits`); `mdb_load`: not applicable |
| `49bb8bd` | the capability API is gone; the version string reports versions only | not applicable (this port never had the capability API) |
| `d23941a` | rlmdb-capi README: the shipped header's patch level is 37 | not applicable |
| `340b318` | version 0.0.0 | not applicable (`Version` reports 0.9.90, `EngineVersion` the C releases) |
| `ca9598c` | AGENTS: performance is judged on x86-64 Linux first | the same rule holds here (benchmarks lead with real x86-64 Linux) |
| `8d494b6` | `linux-test.sh` runs a 2 GiB container; large interop environments one at a time | already there (`scripts/linux-test.sh`, `AGENTS.md` "2 GB of RAM"); the interop part is rlmdb's harness |
| `ed2377e` | FreeBSD: the harness and the tools know its libc | the harness is rlmdb's (its `killed_writer` scenario expects the recovery on FreeBSD, which the Go peer has had since `7bfc806`); no tools here |
| `2c62301` | interop: `concurrent_writers_readers` runs on a 1 GiB map | rlmdb's harness |
| `4e1856b` | AGENTS: benchmark numbers are relative, in commit messages | the same rule holds here |
| `96f57b1` | `MDB_CURRENT` keeps a sub-DB duplicate where searches find it (row 56) | already there: fixed here first and aligned with rlmdb's fix (`b778cca` before the restructuring of its history) on branch `stage18-current-56` |
| `a9bbd2c` | cite ledger rows as "row nn", never "#nn" | already there (`f4d60c1`), and the comments merged from `stage11-rlmdb-sync3` |
| `25240d2` | copy: the free-DB walk's double-count comment cites row 50 | the comment in `copy.go` cites it |
| `be4f7b0` | a 0.9 data file on Windows is extended under the writer mutex, never cut (row 57) | ported (`Env.extendForMap` in `Open` and `SetMapSize`, `extendUnder`, `sys.ExtendFile`, `sys.MapNeedsFileLength`; `sys.MapData` no longer extends the file; `TestExtendingReadsTheSizeAgainUnderTheLock`, `TestExtendingUnderTheLockNeverCutsACommit` on every system, `TestExtendFile`, Windows `TestExtendFileNeverShortensTheFile`) |
| `9e54d28` | on Windows too, an opener that finds the lock objects gone probes again (row 47) | ported (`attachLocks` on Windows: `ERROR_FILE_NOT_FOUND` is `errLocksRemoved`; `lmdb` `TestOpenCloseRace` builds on Windows) |
| `e1292d1` | the alias-path test skips where the scratch volume has no links | ported (`fileNamesOf` and the late-link case of `TestOpenTwiceWithLockFile` skip a link the volume refuses on every system, not only on Windows; the other link tests already skipped) |
| `f4c708e` | refuse `max_readers` beyond 1,048,576 (row 60) | ported (`format.MaxReadersLimit`; `lmdb` `SetMaxReaders`, `engine.Open`; `TestSetMaxReadersLimit`, `TestOpenRefusesCountsBeyondTheLimits`, `TestLockFileBeyondTheReaderLimit`) |
| `7c60f17` | ledger: row 54 cites `mdb_env_set_maxdbs` at 1.0's line 5231 | not applicable |

## Windows: rlmdb's Windows commits (up to `3df6995`)

The Windows port of rlmdb, ported on branch `stage7-windows`; the other
commits of `e48ebe0..3df6995` are the engine sync's. Every Windows
decision is in the file comments of `internal/sys/datamap_windows.go`,
`io_windows.go`, `internal/lockfile/filelock_windows.go`,
`mutex_windows.go` and `locks_windows.go` (rlmdb keeps them in one module
comment, `os/windows.rs`).

| rlmdb | what | here |
|---|---|---|
| `0bf0553` | the OS layer on 64-bit Windows | ported without cgo (`golang.org/x/sys/windows`, ntdll for `NtCreateSection`, `NtMapViewOfSection`, `NtExtendSection`): `sys.MapData` (0.9 extended to the map size and mapped whole, since
`be4f7b0` extended by the engine first, see the sync to `7c60f17`; 1.0 reserved section, `DataMap.CommitPages` where pages are taken from the end of the map, section extended to the file size), `Env.renewShortMap` for read-only maps of the file as it was, the files with C's sharing, `FILE_FLAG_WRITE_THROUGH` meta handle, copy target, file identity, `IsMissing`, registry key of an unresolvable directory; the lock file (`MutexNames`, `MutexHash`, `LockFileEx`, named mutexes, abandoned mutexes as dead owners, no pid locks, `OpenProcess` liveness with `SYNCHRONIZE`, `mr_tid` a `DWORD` at 12, `mti_format` `0x1` and `0x08138002`); rlmdb's Windows unit tests (`*_windows_test.go`) and the Windows layout, computed on every host |
| `24571fa` | rlmdb-capi, tools, tests and interop on Windows | the parts that apply: the C oracles built with MinGW-w64 as interop/build.rs builds them (`tests/cmd/build-c-oracles`), `.exe` names (`coracle.Exe`), `scripts/windows-test.sh`; test expectations that differ on Windows (lock byte not writable through another handle, owner-death tests); rlmdb-capi and rlmdb-tools: not applicable |
| `bf7d115` | CI workflow and docs | docs here (`PLAN.md`, `README.md`, `AGENTS.md`, `lmdb/DEVIATIONS.md`); no CI in this repository |
| `a933f11` | refer to `KNOWN_C_BUGS.md` rows 42 and 43 | the comments cite them |
| `b3ed51e` | interop: argv[0] copies; CI temp directory on the local disk | not applicable (the harness is rlmdb's); run the tests with `TMP` on a local disk, as there |
| `5d8bc38` | a compacting copy stops when it outgrows its source (row 44) | not Windows-specific: the engine sync's |
| `3df6995` | interop holds outlast slow scenarios; stale reader and row 42 on Windows | rlmdb's interop harness (the Go peer runs in it as `Peer::External`, in rlmdb's main since `0f902b4`) |
| `63b591c`, `d27e68f`, `03fccb3`, `86563af`, `7dcf511`, `ea1bbdb` | MSVC builds of the C test programs, the C library's DLL, rlmdb-tools' `atoi` per C runtime | not applicable (no MSVC build here: the oracles are built with MinGW-w64) |

Divergences from rlmdb on Windows (none changes a file):

* The copy target is created without `FILE_FLAG_NO_BUFFERING` (rlmdb sets
  it for pages of at least the OS page size, C always), as the Unix copy
  target is not opened with `O_DIRECT`: it changes no byte of the copy.
* Errors are Go's: the system's error codes as `syscall.Errno` (C:
  `GetLastError()`), and LMDB's own errno values as Go's `syscall`
  constants (`syscall.EINVAL` ...; rlmdb returns the C runtime's numbers),
  which is what lmdb-go v1 returned on Windows (`lmdb/DEVIATIONS.md`).
  `format.Strerror` keeps "Invalid error code" for unknown negative codes
  (rlmdb on Windows: the system's empty message).
* The OS page size is `GetSystemInfo`'s, cached once per process.

## The BSDs: rlmdb's BSD commits (up to `3df6995`)

rlmdb's BSD support, ported on branch `stage7-bsd`. The decisions are in
the file comments of `internal/lockfile/layout.go` (the per-BSD table),
`layout_freebsd.go`, `layout_bsd.go`, `sem.go`, `sysv.go`, `semname.go`
(`SysVKey`) and `internal/sys/sync_freebsd.go`, `sync_bsd.go`,
`sync_dragonfly.go`, `datamap_unix.go` (`Invalidate`).

| rlmdb | what | here |
|---|---|---|
| `e4a5ad9` | FreeBSD, OpenBSD, NetBSD and DragonFly with C's lock primitives (row 39) | ported: FreeBSD's robust mutexes (the cgo pair covers `linux \|\| freebsd`; `layout_freebsd.go`); named semaphores for 0.9 lock files on the other three (macOS's files shared: `sem.go`, `sem_cgo.go`, `-lrt` on NetBSD), NetBSD's 14-character names and 8-byte `dev_t` in the hashed struct (`semNamesV09`, `lockFileID`); the SysV set of 1.0 lock files (`SysVSemaphores`, `sysv.go`, `sysv_cgo.go`, `SysVKey` = rlmdb's `ftok_key`); the last user removes semaphores and the set (`removedByLastUser`); `MDB_FDATASYNC`, `MDB_DSYNC` and `madvise` per BSD; OpenBSD's `MDB_Invalidate` at rlmdb's three call sites (`Txn.invalidateMap`, `sys.DataMap.Invalidate`, widened to whole OS pages: row 39); rlmdb's tests (layouts and lock formats of every platform, names, the SysV protocol on macOS, `ftok` against libc in `tests/lockfile`, `data_map_invalidate`); the C API link lines and the interop lock-line helper: not applicable |
| `bfef7e6` | CI and docs for the BSDs | docs here (`PLAN.md` "The BSDs", `README.md`, `AGENTS.md`, `lmdb/DEVIATIONS.md`); no CI in this repository |
| `12a6731` | the OpenBSD `msync` row is number 39 | the comments cite row 39 |
| `1009bbd` | FreeBSD tested in rlmdb's CI | rlmdb's status; this port is tested on FreeBSD 15.1 and OpenBSD 7.9 arm64 (PLAN.md "The BSDs") |

Divergences from rlmdb on the BSDs (none changes a file):

* The locks are taken through cgo, as on Linux: a BSD build without cgo
  has `NoLock` only (`shared_nocgo.go`). rlmdb always has them (Rust
  links libc either way).
* The SysV code is built on macOS in cgo builds too (rlmdb: in its test
  builds only), so the Region tests run the BSDs' 1.0 layout, and
  NetBSD's 0.9 layout, against macOS's kernel (`bsd_darwin_test.go`); no
  macOS lock file uses it.
* The SysV flag is read and set with two atomic operations under the
  semaphore, as C does (rlmdb: one swap); only the holder touches it.
* FreeBSD's `fdatasync` is called by its system call number and its
  `O_DSYNC` (`0x01000000`) is a constant here: neither package `syscall`
  nor `golang.org/x/sys/unix` v0.30.0 has them for FreeBSD.
* OpenBSD has no `O_DIRECT` for copies: nothing to do, the copy target is
  never opened with it here ("Design decisions").

## Open items

* `MemLock` doubles as the test stand-in for the lock file; the real
  `internal/lockfile` is wired in at stage 2. Cross-process tests (C and
  Go on one environment at once) need it.
* x86-64 Linux is cross-compiled and vetted, not run (no emulated run
  yet).
* The read-only-file-system path (OpenLock returning nil) is exercised
  only through NoLock.
* Performance: see `tests/bench/RESULTS.md` (stage 3).
* Windows: tested on x86-64 (PLAN.md "Windows"); arm64 built and vetted,
  not run. Not run there: `-race` (needs cgo for every package), rlmdb's
  interop matrix with the Go peer.
* The BSDs: built, vetted and cross-compiled (`go test -c`) for amd64
  and arm64 (DragonFly: amd64), with cgo against each BSD's headers too,
  their lock-file layouts checked against C compiled for them
  (`scripts/bsd-cc-check.sh`); FreeBSD 15.1 and OpenBSD 7.9 arm64
  tested, the others not run (PLAN.md "The BSDs").
* Big-endian: the root module and the tests module (C oracles built in
  the container) pass on s390x under QEMU user emulation
  (`LINUX_TEST_IMAGE=golang:1.25-trixie LINUX_TEST_PLATFORM=linux/s390x
  scripts/linux-test.sh`), owner-death tests skipped (QEMU has no robust
  futexes).
