// Package engine is the storage engine of the Go implementation of LMDB:
// a port of rlmdb's core (rlmdb/src/core, minus the lock file), binary
// compatible with C LMDB 0.9 and 1.0 (both data formats, all 1.0 page
// sizes from 256 B to 64 KiB). The public lmdb package (lmdb-go's API) is
// a thin layer on top of it; the lock file (reader table, writer and
// reader mutexes shared with C) is internal/lockfile, which the engine
// reaches only through the LockRegion interface.
//
// The API follows the C API closely:
//
//   - Open (mdb_env_create + mdb_env_set_* + mdb_env_open, with Options)
//     returns an Env: Stat, Info, Sync, SetFlags, SetMapSize, Copy/CopyFD/
//     CopyTo (plain and compacting), ReaderList, ReaderCheck, CloseDBI,
//     Close.
//   - Env.BeginTxn(parent, flags) begins a read transaction (MDB_RDONLY),
//     a write transaction, or a nested one (parent); Env.BeginRead reuses
//     a Txn for a read transaction without allocating. Txn: OpenDBI (with
//     MDB_CREATE in write transactions), DBIFlags, Stat, Drop, Get, Put,
//     PutReserve, Del, OpenCursor/OpenCursorInto, Reset, Renew, Commit,
//     Abort, ID.
//   - Cursor: Get (every MDB_cursor_op), Put (MDB_CURRENT & co.),
//     PutReserve, PutMulti (MDB_MULTIPLE), Del, Count, Renew (read
//     transactions), Close.
//
// Errors are format.Errno values for LMDB's codes and syscall.Errno for
// errno values, both exactly as C returns them; MDB_NOTFOUND is
// format.NotFound.
//
// Values returned from a read transaction are slices of the memory map,
// valid until the transaction ends (pages of a pinned snapshot never
// change). Values returned from a write transaction may live in dirty
// pages and are valid until the next change. Read transactions are not
// bound to a thread (MDB_NOTLS semantics, always). A write transaction
// must begin and end on the same OS thread when the lock file's writer
// mutex is thread-owned (Linux): the caller locks the goroutine to its
// thread.
//
// Without a lock file (MDB_NOLOCK, or a read-only file system) the
// environment keeps an in-memory reader table (MemLock), so a writer of
// this process never reuses pages a live reader can see (AGENTS.md rule
// 2a); C leaves that to the caller.
//
// The engine keeps rlmdb's structure (see PORTING.md): write cursors hold
// page numbers with dirty-list slot hints and are kept valid by C's cursor
// fixups; read operations on them rebuild read cursors. Every decision
// that shows in the data file (page allocation, split points, free-list
// handling, spilling, compacting copy) is C's, which tests/engine checks
// page by page against C 0.9.37 and C 1.0.
package engine
