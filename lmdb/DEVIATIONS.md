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
0.9.36"), whose decisions the port keeps.

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

## Behaviour that differs

* **Version.** `Version()` reports 0, 9, 85 and `VersionString()`
  `LMDB 0.9.85: (lmdb-go, Go port of rlmdb)`, as rlmdb's C library does: the
  0.9 series whose API and default format this is, with a patch level
  above every C release so that programs can tell the two apart. v1
  reported its bundled C library (0.9.35). `Env.EngineVersion` reports the
  C release whose format an environment matches: 0.9.36 or 1.0.2 (lmdb-go
  v2: the bundled engine's `mdb_version`).
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
  objects (`internal/lockfile`). On Linux they are reached through cgo:
  built with `CGO_ENABLED=0`, `Env.Open` without `NoLock` returns
  `ErrNoSharedLocking` before touching the file system; `NoLock`
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
  to the same). Across processes C's file names
  apply: the lock file of a directory is `lock.mdb` in it, that of a
  `NoSubdir` file its name plus `-lock`, so processes that open one data
  file in different modes use two lock files and neither exclude each
  other's writers nor see each other's readers, in C as here. Open an
  environment in the same mode everywhere.
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
  pointers to C). `Commit` of an ended `Txn` is `EINVAL`, `Abort` and
  `Close` are no-ops. A nested `Txn` that is still active when its
  parent commits or aborts ends with it (C commits or aborts it first and
  frees it; v1's handle then pointed to freed memory, and its finalizer
  aborted whatever transaction reused it): it fails the same way, and
  its finalizer does nothing.
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
  stops new transactions and `Renew` (`EINVAL`), but a read transaction
  that outlives it keeps its snapshot readable and protected, and may
  still be read, reset and aborted (the finalizer does that): the
  environment stays open underneath, with the transaction's reader slot,
  the process's liveness lock in the lock file (so other processes' reader
  checks leave the slot alone), the files and the registry entry, until
  the last such transaction (active or reset) has ended, and is torn down
  then. Until that moment, opening the same path again in this process
  fails with `EBUSY` with a lock file; a `NoLock` handle opened meanwhile
  shares the closed one's reader table. `Commit` of a write transaction after `Close` aborts
  it and returns `EINVAL`. `Close` waits for no commit or copy: a commit
  in progress, and a copy that has taken its snapshot, finish; a copy that
  has not fails with `EINVAL`.
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
  program error) gets the mutex's error (Linux: `EPERM`) and the mutex
  stays with the thread that took it, until it exits; C ignores the
  error.
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
* **Windows** (64-bit: x86-64, and arm64 by the same rules) follows
  rlmdb's Windows port and C LMDB's `_WIN32` paths, without cgo
  (`golang.org/x/sys/windows`): `SharedLockingAvailable` is true with and
  without cgo, and the lock file is C's (named mutexes `Global\MDBr…`,
  `Global\MDBw…`, `LockFileEx` on byte 0). As with v1 on C there, `FD`
  returns and `CopyFD` takes a `HANDLE`, and a write transaction holds a
  mutex that belongs to its thread (another thread's `Commit` gets
  `ERROR_NOT_OWNER`). An LMDB 0.9 data file opened for writing is as long
  as its map size (C 0.9 extends it in order to map it), and is never
  shrunk (C 0.9 tries when the map size shrinks; rlmdb does not); an LMDB
  1.0 file grows with its writes. Errors: LMDB's own errno values are Go's
  `syscall` constants (`syscall.EINVAL`, ...), as v1's `error_windows.go`
  made them from the C runtime's errno; system errors keep their Windows
  error code (`syscall.Errno`, e.g. `ERROR_ACCESS_DENIED`, 5), where v1's
  table read the codes 2, 5, 12, 13, 16, 22 and 28 as the errno of that
  number (5 became `EIO`). `error_windows.go` is kept. `ReaderCheck`
  clears the slots of a process that has exited even while some program
  still holds a handle to it (C keeps them until the last handle closes:
  rlmdb `KNOWN_C_BUGS.md` #42). Status: builds and vets for windows/amd64
  and windows/arm64; the tests have not run on Windows yet.
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
* `exp/lmdbsync` tests: `newEnv` takes the `*testing.T` and skips without
  shared locking unless the test asks for `NoLock` (with `NoLock`, `lmdbsync.Env`
  serialises its own transactions, which changes what those tests check;
  its handler runner deadlocks there by design); `TestNewEnv`,
  `TestEnv_Open` and `TestResize` skip without shared locking.

lmdb-go v2's tests for the format API (`sniff_test.go`, `version_test.go`,
`preopen_test.go`) run with `Open` through `testFlags`, and
`TestEnv_PrevSnapshot` expects `EINVAL` for LMDB 1.0 as well. Tests of
what this package does differently are in `port_test.go`.
