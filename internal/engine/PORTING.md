# Porting notes: rlmdb core -> internal/engine

Running status of the stage-1 engine port (PLAN.md, agent E), so work can
resume after an interruption. rlmdb sources: `rlmdb/src/core/*.rs` at the
pinned commit.

## File map

| Go | rlmdb | status |
|---|---|---|
| `internal/format/*` | `format/*` (minus lockfile), consts, flags, error, cmp, idl | done, tests ported |
| `internal/sys/*` | `os/unix.rs`, `os/mod.rs` minus shared locks | done |
| `lockregion.go` | `core/reader.rs` (interface only) | done |
| `env.go` | `core/env.rs` | written |
| `cursor.go` | `core/cursor.rs` | written |
| `dbi.go` | `core/dbi.rs` | in progress |
| `txn.go` | `core/txn.rs` | |
| `rocursor.go` | `core/ro_cursor.rs` | |
| `dirty.go` | `core/dirty.rs` | |
| `wcursor.go` | `core/wcursor.rs` | |
| `txnrw.go` | `core/txn_rw.rs` | |
| `pageops.go` | `core/page_ops.rs` | |
| `freelist.go` | `core/freelist.rs` | |
| `cursorput.go` | `core/cursor_put.rs` | |
| `split.go` | `core/split.rs` | |
| `cursordel.go` | `core/cursor_del.rs` | |
| `commit.go` | `core/commit.rs` | |
| `nested.go` | `core/nested.rs` | |
| `spill.go` | `core/spill.rs` | |
| `copy.go` | `core/copy.rs` | done, byte-identical to mdb_copy (-c) |
| `audit.go` | `core/audit.rs` | |

## Design decisions (Go vs Rust)

* Structure follows rlmdb, including write cursors that hold page numbers
  plus dirty-list slot hints (`wcursor.rs`). C's pointer model would be
  possible in Go, but rlmdb's structure is the tested artifact whose
  decisions (KNOWN_C_BUGS rows) the port must keep.
* One `Txn` type for read and write transactions (`w == nil` for read),
  one `Cursor` type (read cursor state, or an arena slot of a write txn),
  so the stage-2 `lmdb` layer stays thin.
* Read transactions: the per-txn DBI view is lazily reset with a
  generation counter (`Txn.gen`), so begin/renew is O(1) in `MaxDBs`.
* The data map is reference counted (`sys.DataMap`): the env holds one
  reference while it is current, every transaction one. Replaces rlmdb's
  `Arc<DataMap>`.
* rlmdb's buffer pools (`PagePool`, `SpareTxn`, scratch buffers) are
  allocation optimisations; ported only where cheap. Stage 3 measures.
* The copy target is not opened with `O_DIRECT`/`F_NOCACHE` (rlmdb does,
  best effort): no effect on the bytes written.
* `LockRegion` gained `NumReaders()` (rlmdb `raw_num_readers`, used by
  `Env.Info`). `ReaderInfo` lives in `internal/format` so that lockfile can
  implement the interface without importing engine.
* The environment variable for the default format is rlmdb's
  `RLMDB_DEFAULT_FORMAT` (`engine.DefaultFormatEnv`); stage 2 may rename.

## Open items

(see the end of the stage report)
