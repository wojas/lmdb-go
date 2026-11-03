# Package lmdb: where it differs from lmdb-go v1

Package `lmdb` has the API of lmdb-go v1 (`github.com/bmatsuo/lmdb-go`,
continued as `github.com/PowerDNS/lmdb-go` v1.9.4) and, for the LMDB 1.0
data format, that of lmdb-go v2. Underneath is not the C library but
`internal/engine`, a port to Go of rlmdb (the Rust rewrite of LMDB),
which follows LMDB (Howard Chu, Symas Corporation) function by function
and writes the same bytes as C LMDB 0.9 and 1.0. Behaviour is that of
lmdb-go v1 on C LMDB except as listed here. What the engine does
differently from C is in `internal/engine/PORTING.md` and in rlmdb's
`KNOWN_C_BUGS.md` and `rlmdb-capi/README.md` ("Differences from C LMDB
0.9.37"), whose decisions the port keeps.

## lmdb-go v1 bugs not kept

| Call | lmdb-go v1 on C | This package |
|---|---|---|
| `Txn.Del(dbi, key, nil)` on a `DupSort` database | Passed an empty value instead of `NULL`, so C looked for that value: `MDB_NOTFOUND`, or `MDB_BAD_VALSIZE` for a key whose values are in a sub-page or sub-DB. Nothing was deleted. | `nil` is C's `NULL`: all values of the key are deleted (`mdb_del` with `NULL` data). A non-nil empty slice is still an empty value. |
| `Cursor.Get(nil, nil, GetMultiple)` on a key with a single value | C succeeds without touching `data`; v1 cleared its value buffer after every call and returned an empty slice. | The value itself, a page of one item, so that `WrapMulti(page, stride)` works for every key. (C's caller still holds the value from the preceding `MDB_SET`; that is what this returns.) |
| `Cursor.Get` with `Txn.RawRead` after `FirstDup` or `LastDup` | C leaves the key unset; v1 made a slice of its NULL pointer and panicked. | Returns the current key and the value. |
| `Cursor.PutMulti` with an empty page, or a stride below 1 | Stored one zero byte (stride 1), panicked otherwise (division by zero for stride 0). | `EINVAL`. A page that is not a multiple of the stride still panics, as documented. |

## API additions

* LMDB 1.0's data format, with lmdb-go v2's API: `Format`, `FormatUnknown`,
  `V09`, `V10`, `SetDefaultFormat`, `Env.SetFormat`, `Env.Format`,
  `Env.EngineVersion`, `EnvVersion`, `SniffFormat`, `ErrFormatConflict`,
  `ErrFormatUnsupported`, `ErrInvalidFormat`, `ErrVersionUndetermined`,
  `DefaultFormatEnvVar` (`LMDBGO_DEFAULT_FORMAT`, experimental as in v2:
  a stray value creates 1.0 files that 0.9 programs cannot open),
  `PrevSnapshot`, and LMDB 1.0's error codes (`Problem` ... `AddrBusy`).
* `Env.SetPageSize(int) error`: the page size of a new LMDB 1.0
  environment (C 1.0's `mdb_env_set_pagesize`, rlmdb's
  `rlmdb_env_set_pagesize`); `EINVAL` at `Open` for an LMDB 0.9 one.
* `SharedLockingAvailable() bool` and `ErrNoSharedLocking`: see "Builds
  without cgo".
* `ErrTxnWrongThread`: see "Write transactions and threads".
* `OpError.Unwrap`: `errors.Is` and `errors.As` see the errno of an
  `*OpError`, so `errors.Is(err, NotFound)` holds. In v1 it was false;
  `IsNotFound`, `IsErrno` and the other helpers work as before.

## Behaviour that differs

* **Version.** `Version()` reports 0, 9, 90 and `VersionString()`
  `LMDB 0.9.90: (lmdb-go, Go port of rlmdb)`: the 0.9 series whose API
  and default format this is, with a patch level above every C release so
  that programs can tell the two apart (rlmdb's C library reports 0.9.85
  for the same reason; 90 reads as "go"). v1 reported its bundled C
  library (0.9.35). `Env.EngineVersion` reports the C release whose format
  an environment matches: 0.9.37 or 1.0.3 (lmdb-go v2: the bundled
  engine's `mdb_version`).
* **Formats.** Both formats are read and written by one engine; v2 has two
  C engines. As in v2, an existing file is opened in its format,
  `SetFormat` asking for the other one is `ErrFormatConflict`, and a file
  of lmdb-js's prerelease format (data version 2) is `ErrFormatUnsupported`.
  A valid magic with another unknown data version is `MDB_VERSION_MISMATCH`
  from `SniffFormat` and `Open` (as C's `mdb_env_read_header` and v2;
  rlmdb's engine says `MDB_INVALID`). `PrevSnapshot` is not implemented
  (rlmdb has no such feature): `Open` fails with `EINVAL` for either
  format, where v2's 1.0 engine opens the previous snapshot.
* **Builds without cgo.** lmdb-go v1 did not build without cgo. Here only
  the writer and reader-table locks shared with other processes are libc
  objects (`internal/lockfile`). On Linux and the BSDs they are reached
  through cgo: built with `CGO_ENABLED=0`, `Env.Open` without `NoLock`
  returns `ErrNoSharedLocking` before touching the file system; `NoLock`
  environments work. On macOS a build without cgo calls the semaphores in
  libSystem directly and works fully. Windows builds need no cgo at all
  (see Windows). `SharedLockingAvailable` says which kind of build is
  running.
* **`NoLock`.** Only the cross-process locking is removed: within the
  process, write transactions are serialised and readers are tracked in an
  in-memory table, so pages a live reader can see are never reused. C
  leaves both to the caller. Any number of `NoLock` handles (`Env`s) may
  open one data file in a process, as in C; they share that table and the
  writer lock (rlmdb's rule), so a writer of any handle keeps the pages of
  readers of every handle, and the writers of all handles exclude each
  other. The shared state lives while one of the handles is open. So a
  goroutine with a write transaction of one handle must not begin one in
  another handle of the file, nor start a plain `Copy` of it: it would
  wait for itself, as with two write transactions of one handle (C does
  not serialise the handles' writers at all).
* **One open per data file with a lock file.** A second `Open` of a data
  file that a handle with a lock file has open fails with `EBUSY` (rlmdb;
  C allows it, unsafely: closing one handle drops the other's record
  locks), with or without `NoLock`, and so does an `Open` with a lock file
  next to `NoLock` handles of the file (their readers would be invisible
  to each other's writers), before it touches the lock file. This holds
  for every name of the data file: the directory, the file itself with
  `NoSubdir`, symlinks and hard links (the data file's device and inode;
  rlmdb claims the path given and then the device and inode, which comes
  to the same). The lock file is claimed the same way before it is locked,
  so an `Open` that reaches the lock file of a handle of this process
  under another name, or a data file that is such a lock file, fails with
  `EBUSY` as well (C sets the table up again under the open handle's
  readers); a descriptor of such a file that the failed `Open` or
  `SniffFormat` opened stays open until that handle is gone, since closing
  it would drop the handle's record locks. Across processes C's file names
  apply: the lock file of a directory is `lock.mdb` in it, that of a
  `NoSubdir` file its name plus `-lock`, so processes that open one data
  file in different modes use two lock files and neither exclude each
  other's writers nor see each other's readers, in C as here. Open an
  environment in the same mode everywhere.
* **Read-only handles before the first commit.** A read transaction of a
  `Readonly` handle reads the snapshot `mti_txnid` names, also while it is
  0, as read transactions of writable handles do. C's and v1's read-only
  handles read the newest meta page while it is 0, which can be a first
  commit in flight, one that failed, or one whose writer died before it
  was published (rlmdb `KNOWN_C_BUGS.md` row 45). Such a reader sees an empty
  environment there, txnid 0.
* **0.9 pages below 2 KiB.** A 0.9 data file of pages below 2 KiB is
  refused with `MDB_INVALID`, read-write and read-only (rlmdb row 48). C and
  v1 open it (no 0.9 writer makes one; 511-byte keys and `DupFixed` items
  do not fit its pages, and C writes past them). LMDB 1.0 files keep their
  range from 256 bytes.
* **Damaged files, more of them refused.** A transaction on a snapshot
  whose last page is below the meta pages, or the largest page number,
  fails to begin with `MDB_CORRUPTED` (rlmdb row 49; a C writer overwrites a
  meta page there). Over branch pages that share children (rlmdb row 50): a
  cursor walk fails with `MDB_CORRUPTED` once it has stepped onto more
  leaves in one direction than the snapshot has pages plus the pages its
  write transaction allocated; walks of the free DB (page allocation,
  `FreePages`, the compacting copy's count) fail at a key not larger than
  the one before; `Txn.Drop` fails once its walk has visited, or its free
  list holds, more pages than the snapshot has; and a commit whose free
  list names a page twice fails. C and v1 walk such a tree for hours and
  commit such a free list. Sound files see no difference.
* **`Open` while the last user closes.** Where the last user of an
  environment removes its semaphores (macOS; OpenBSD, NetBSD and DragonFly),
  an `Open` that finds them gone starts again with the exclusive probe, up
  to 100 times, then fails with `ENOENT`. C and v1 fail with `ENOENT` at
  once (rlmdb row 47). On Windows the kernel destroys the named mutexes
  with the last user's handles, which it closes before it releases the
  lock file: an `Open` that finds them gone starts again the same way,
  where C fails with `ERROR_FILE_NOT_FOUND`.
* **At most 1,048,576 named databases and reader slots.**
  `SetMaxDBs` and `SetMaxReaders` beyond 1,048,576 (`1 << 20`) are
  `EINVAL`. C and v1 take any count (v1 one that fits C's `unsigned
  int`): C breaks above about 35 million databases, and every transaction
  has a slot per database (rlmdb row 54); a lock file holds 64 bytes per
  reader slot, 64 MiB at the limit and 256 GiB at four billion (rlmdb
  row 60). An existing lock file with a larger table is still attached to
  with the count its size gives, as in C.
* **Ignored flags.** `FixedMap`, `WriteMap`, `MapAsync` and `NoMemInit` are
  accepted and ignored (`Flags` reports them), as in rlmdb: the map is
  read-only and never at a fixed address.
* **Settings before `Open`** (as lmdb-go v2): `NewEnv` does not create the
  environment; `SetMapSize`, `SetMaxReaders`, `SetMaxDBs`, `SetFlags`,
  `SetFormat` and `SetPageSize` are kept for `Open`, and `Flags` before
  `Open` reports the flags given to `SetFlags`. `SetMaxReaders(0)` is
  `EINVAL` (C's check); `SetMaxReaders` and `SetMaxDBs` after `Open` are
  `EINVAL`, as in C. A failed `Open` may be retried. Methods that need an
  open environment return an error (not open, or already closed) where v1
  passed a NULL handle to C and crashed or returned C's error.
* **`Open` mode 0.** New files get mode 0644 (C would create them with
  mode 0).
* **Handles used after they ended.** A `Txn` used after `Commit` or
  `Abort`, and a `Cursor` used after `Close` or after its transaction
  ended, return `EINVAL` (as rlmdb's C API; v1 passed freed or NULL
  pointers to C: `Cursor.Get` after `Close` panicked, most cursor
  operations after the end of the transaction crashed, and a write cursor
  of a committed transaction could write into the next one). `Commit` and
  `Sub` of an ended `Txn` are `EINVAL` (v1's `Sub` began a new top-level
  write transaction), `Abort` and `Close` are no-ops. A copy of a `Cursor`
  value made before `Close` fails the same way and never touches the
  cursor that reuses its memory. A nested `Txn` that is still active when
  its parent commits or aborts ends with it (C commits or aborts it first
  and frees it; v1's handle then pointed to freed memory, and its
  finalizer aborted whatever transaction reused it): it fails the same
  way, and its finalizer does nothing.
* **Cursors that are not closed.** A cursor that is not closed keeps its
  memory (about 2 KiB) until its transaction ends or is reset, also when
  it is no longer reachable; then it drops its references to the map
  (so that a cursor kept for `Cursor.Renew` holds no pointers into a map
  that may be unmapped) and is garbage once unreachable. v1 freed a read
  cursor that was not closed in a finalizer. Close the cursors that a
  long-running read transaction opens.
* **Cursors of a renewed transaction.** After `Txn.Reset` and `Txn.Renew`,
  a cursor that was not renewed with `Cursor.Renew` starts over
  unpositioned in the new snapshot (rlmdb's C API). In C it keeps pointers
  into the old snapshot's pages.
* **`Txn.ID`** is 0 while a read transaction is reset and after it ended
  (the engine's `mdb_txn_id`, as rlmdb). C keeps the old snapshot's id in
  a reset transaction, and v1 returned that (or its cached copy).
* **`Cursor.Renew`** is `EINVAL` for a cursor of a write transaction, onto
  a write transaction, and onto a transaction of another environment (C
  does not check the last one and writes past its cursor).
* **Transactions that outlive `Env.Close`.** C requires every transaction
  to end before `mdb_env_close` (v1 then used freed C memory). Here `Close`
  stops new transactions and `Renew` (`EINVAL`), and a transaction that
  outlives it can only end: every other method of the `Txn` and of its
  cursors returns `EINVAL`, as for an ended transaction (`Reset` does
  nothing, `ID` keeps its value), while `Abort`, `Commit` and the
  finalizer end it. A read transaction keeps its snapshot protected until
  it ends, so `RawRead` slices returned before `Close` keep their bytes
  until then: the environment stays open underneath, with the
  transaction's reader slot, the map, the process's liveness lock in the
  lock file (so other processes' reader checks leave the slot alone), the
  files and the registry entry, until the last such transaction (active
  or reset) has ended, and is torn down then. Until that moment, opening
  the same path again in this process fails with `EBUSY` with a lock
  file; a `NoLock` handle opened meanwhile shares the closed one's reader
  table. On Windows, where open files cannot be removed, the
  environment's files can be removed only then. `Commit` of a write
  transaction after `Close` aborts it and returns `EINVAL`. An operation
  that began before `Close` finishes on the snapshot. `Close` waits for no
  commit or copy: a commit in progress, and a copy that has taken its
  snapshot, finish; a copy that has not fails with `EINVAL`.
* **`SetMapSize` on an open environment** may run while read transactions
  are active (they keep the map they began with); `EINVAL` while this
  process has a write transaction, as in C.
* **`MaxKeySize`** is 511 for LMDB 0.9 environments and follows from the
  page size for LMDB 1.0 ones (1978 for 4 KiB pages); before `Open` 511,
  or the 1.0 limit if 1.0 was requested with `SetFormat`.
* **Write transactions and threads.** As in v1, a write transaction must
  begin and end on one goroutine locked to its OS thread (`Update` does
  that). The lock file's writer mutex pins the goroutine itself while it
  holds the lock. A write transaction committed on another thread (a
  program error) is committed, but `Commit` returns an error wrapping
  `ErrTxnWrongThread` and the mutex's error (Linux and FreeBSD: `EPERM`;
  Windows: `ERROR_NOT_OWNER`), so `errors.Is` finds either and
  `IsErrnoSys(err, syscall.EPERM)` holds; it is not an `*OpError`. The
  mutex stays with the thread that took it, until that thread exits, and
  the next writer recovers it as from a dead writer. C ignores the error
  (v1's `Commit` returned nil); `Abort` reports nothing, as C. Semaphores
  (macOS, the other BSDs) belong to no thread: no error there.
* **`ReaderList`.** `fn` gets one line per call, as C's message function.
  `fn == nil` is `EINVAL` (v1: an `OpError` holding errno -1). Before
  `Open` and with `NoLock` the text is `(no reader locks)` (C's output
  without a lock file).
* **Reader slots outlive read transactions.** lmdb-go opens every
  environment with `MDB_NOTLS`, under which C claims a reader slot (under
  the shared reader mutex) at each read transaction's begin and frees it
  at the end. Here a read transaction that ends (`View`, `Abort`,
  `Commit`) leaves its slot owned by the process and idle (`mr_pid` the
  process, `mr_txnid` `(txnid_t)-1`), exactly as C's default TLS mode
  leaves a thread's slot, and the next read transaction of the
  environment reuses it without the mutex and without a cgo call. Idle
  slots pin no snapshot (C's `mdb_find_oldest` ignores them), and other
  processes' reader checks leave them alone while this process lives.
  Other processes see them: `mdb_stat -r`, `mdb_reader_list` and
  `ReaderList` print them with txnid `-`, and `Info().NumReaders` (the
  table's high-water mark) counts them. At most `MaxReaders/4` slots (and
  at most 64) are kept; when the table gets nearly full (a new slot in
  its top quarter, or `MDB_READERS_FULL`) ending transactions release
  their slots instead; closing the environment releases them all. A reused slot keeps
  the thread id of the thread that claimed it (diagnostic only). See
  `internal/engine/slotpool.go`.
* **Finalizers.** `Env` and unmanaged `Txn`s have v1's finalizers. Cursors
  have none: v1's freed the C cursor of a read transaction, and here
  there is nothing to free. The finalizer of an unreachable nested `Txn`
  does not run while its parent lives (v1's aborted the child on the
  finalizer's goroutine, under the parent's user); the child ends with
  the parent.
* **Errors.** `Errno.Error` has LMDB 1.0's messages for 1.0's codes (C
  0.9: "Invalid error code"). The `*OpError` values returned for
  `MDB_NOTFOUND` by `Txn.Get` and `Cursor.Get` are shared; do not modify
  them.
* **Allocations.** `Txn.Get` and `Cursor.Get` with `RawRead` false copy
  the value (and key) out of the map as v1 does, but `Cursor.Get` puts key
  and value in one allocation (v1: two); the key's capacity ends at its
  length, so appending to it never overwrites the value, but holding the
  key keeps the value's memory alive. Read transactions and cursors reuse
  the memory of ended ones (one small allocation each, for the handle).
* **`Current` on a duplicate at the edge of a page** (a C bug, not
  ported). Replacing a duplicate of a `DupSort` key with `Cursor.Put(...,
  Current)`, C keeps the new item in the old one's place without checking
  the keys of the tree above it. When the old item is the first one of a
  page (and the new one has another size) or the last one of a page, the
  new item can land where searches do not look: `GetBoth` no longer finds
  it, and a later put can store duplicates out of order. C 0.9 and 1.0
  have this; rlmdb fixes it the same way. In exactly these cases the item
  is deleted and the new one put, as `Cursor.Del` followed by `Cursor.Put`
  would; the file is then what C writes for those two calls. The same
  holds for `Cursor.PutMulti(..., Current)` with a single value (C has the
  same bug there); with two or more values it replaces the item in place
  with each in turn, as C does. Every other replacement is C's, byte for
  byte.
* **Windows** (64-bit: x86-64, and arm64 by the same rules) follows
  rlmdb's Windows port and C LMDB's `_WIN32` paths, without cgo
  (`golang.org/x/sys/windows`): `SharedLockingAvailable` is true with and
  without cgo, and the lock file is C's (named mutexes `Global\MDBr…`,
  `Global\MDBw…`, `LockFileEx` on byte 0). As with v1 on C there, `FD`
  returns and `CopyFD` takes a `HANDLE`, and a write transaction holds a
  mutex that belongs to its thread (another thread's `Commit` gets
  `ErrTxnWrongThread`, wrapping `ERROR_NOT_OWNER`). An LMDB 0.9 data
  file opened for writing is as long as its map size (C 0.9 extends it
  in order to map it), and is never shrunk: C 0.9 sets the length
  outright, from a meta page it read before, which also shrinks a longer
  file and can cut pages another process committed meanwhile (rlmdb
  `KNOWN_C_BUGS.md` row 57); here the file is only extended, and, next to
  other processes, holding the writer mutex, under which every commit is
  made. An LMDB 1.0 file grows with its writes. Errors: LMDB's own errno values are Go's
  `syscall` constants (`syscall.EINVAL`, ...), as v1's `error_windows.go`
  made them from the C runtime's errno; system errors keep their Windows
  error code (`syscall.Errno`, e.g. `ERROR_ACCESS_DENIED`, 5), where v1's
  table read the codes 2, 5, 12, 13, 16, 22 and 28 as the errno of that
  number (5 became `EIO`). `error_windows.go` is kept. `ReaderCheck`
  clears the slots of a process that has exited even while some program
  still holds a handle to it (C keeps them until the last handle closes:
  rlmdb `KNOWN_C_BUGS.md` row 42). Tested on Windows x86-64, also with
  `-race`, with C LMDB built with MinGW-w64 and rlmdb as the other side;
  windows/arm64 builds and vets and has not run.
* **FreeBSD, OpenBSD, NetBSD, DragonFly** (64-bit) follow rlmdb's BSD
  support and C LMDB's selection there, with cgo for the locks as on
  Linux. FreeBSD: Linux's robust process-shared mutexes in the lock file
  (8 bytes there), `fdatasync`. OpenBSD, NetBSD, DragonFly: POSIX named
  semaphores for LMDB 0.9 lock files, as on macOS (NetBSD's names are 14
  characters, as C's), and for LMDB 1.0 lock files the SysV semaphore set
  of C 1.0 (`semget(ftok(lockfile, 'M'))`), which tells the next writer
  about one that died. The first opener of such a lock file refuses a set
  at its key that is not a set of two it may change (another program's
  set whose `ftok` key collides: `EEXIST` for a larger one, the
  `semctl` error for another user's), where C 1.0 and rlmdb before
  `2fb635e` reset it (rlmdb `KNOWN_C_BUGS.md` row 46). `MDB_FDATASYNC` is
  `fsync`. On OpenBSD, which has
  no unified buffer cache, every write to the data file is followed by
  `msync(MS_INVALIDATE)` of the map, widened to whole OS pages where C's
  call fails unnoticed (rlmdb `KNOWN_C_BUGS.md` row 39). The port builds and
  vets for amd64 and arm64 (DragonFly: amd64), with and without cgo (the
  cgo builds against each BSD's headers and C library:
  `scripts/bsd-cc-check.sh`). FreeBSD 15.1 and OpenBSD 7.9 on arm64 are
  tested, against C LMDB 0.9 and 1.0 built there; NetBSD, DragonFly and
  amd64 have not run.
* **64-bit only.** lmdb-go v1 built and ran on 32-bit platforms with C
  LMDB, whose 32-bit data files are a format of their own (the file
  depends on `sizeof(size_t)`), not shared with 64-bit processes. This
  implementation has only the 64-bit format, as rlmdb: a build for a
  32-bit `GOARCH` (386, arm, mips, mipsle) fails with `undefined:
  lmdb_requires_a_64_bit_platform` (`internal/format/arch_unsupported.go`).
  64-bit big-endian targets (s390x, ppc64, mips64) build; s390x is tested
  under emulation (QEMU user mode, with C peers built for it), ppc64 and
  mips64 are not. Like C, files are in the host's byte order.

## Tests of lmdb-go v1 that were changed

lmdb-go v1's tests (`lmdb/*_test.go`, `lmdbscan`, `exp/lmdbsync`) run
unchanged except:

* `lmdb/msgfunc_test.go` is gone with `msgfunc.go`: it tested the registry
  that passed Go callbacks through C to `mdb_reader_list`.
* `lmdb/val_test.go`, `TestVal`: the half that checked `getBytes`, a
  zero-copy view of a C `MDB_val`, is gone; `getBytesCopy` is tested on a
  Go slice.
* `lmdb/txn_test.go`, `TestTxn_Reset_readonly_C_free`: checks that a read
  transaction keeps its (pooled) state across `Reset` and `Renew` and
  releases it in `Abort`, instead of the C memory of v1's value buffers.
* `lmdb/env_test.go`, `lmdb_test.go`, `txn_test.go`: `Env.Open` gets
  `testFlags(flags)`, which adds `NoLock` in a build without shared
  locking (Linux without cgo), or with `LMDB_TEST_NOLOCK=1`, to run the
  suite on `NoLock` in any build;
  `TestEnv_ReaderList` needs a lock file and skips then.
  (`testflags_test.go` is new.)
* `internal/lmdbtest.NewEnv` adds `NoLock` in a build without shared
  locking (`lmdbscan`'s tests use it).
* `lmdb/env_test.go`, `clean`: on Windows, removing the environment's
  directory is retried for up to five seconds (with a garbage
  collection), since a transaction that outlives `Close` keeps the files
  open until a finalizer ends it (`TestTxn_finalizer`).
* `exp/lmdbsync` tests: `newEnv` takes the `*testing.T` and skips without
  shared locking unless the test asks for `NoLock` (with `NoLock`, `lmdbsync.Env`
  serialises its own transactions, which changes what those tests check;
  its handler runner deadlocks there by design); `TestNewEnv`,
  `TestEnv_Open` and `TestResize` skip without shared locking.
  `TestResize` names its helper program with `.exe` on Windows (`exec`
  refuses a file without an executable extension there).

lmdb-go v2's tests for the format API (`sniff_test.go`, `version_test.go`,
`preopen_test.go`) run with `Open` through `testFlags`, and
`TestEnv_PrevSnapshot` expects `EINVAL` for LMDB 1.0 as well. Tests of
what this package does differently are in `port_test.go`.
