# Fuzzing

Go's native fuzzing (`go test -fuzz`) checks rule 4 of `AGENTS.md`
(malformed input returns an error, never a panic, hang or out-of-range
access) and the write path against a model and against C. A normal
`go test` runs every target on its seeds and on the inputs committed under
`testdata/fuzz/<Target>/` only, in well under a second each; fuzzing
itself is started by hand.

## Targets

| Package | Target | Input | Checks |
|---|---|---|---|
| `internal/format` | `FuzzPage` | one page (or any buffer), either format | every `Page`/`Node` accessor on branch, leaf, `P_LEAF2`, overflow and meta pages, `P_SUBP` sub-pages and `MDB_db` records inside nodes: no panic, results consistent with the fields they decode |
| `internal/format` | `FuzzMeta` | the head of a data file | `MDB_meta` and `MDB_db` decoding and round trips, `ReadMetaPage`, `SniffFormat`, `PickNewest` |
| `internal/format` | `FuzzIDL` | free-list records | decode/encode, merges (also of unsorted records), search, run search and removal |
| `internal/format` | `FuzzCompare` | two keys | every built-in comparator: antisymmetry, memn/memnr by definition, `mdb_cmp_cint` against C's loop |
| `internal/engine` | `FuzzPageOps` | one page and an edit | the engine's node search and in-place page edits (`pageops.go`, the DUPSORT sub-page builder) on damaged pages, called as their callers call them |
| `internal/engine` | `FuzzWriteModel` | an environment shape and a script of operations | the write path against an in-memory model: every put flag, cursor writes, `MDB_CURRENT`, `MDB_MULTIPLE`, deletes, range deletes, bulk loads, cursor moves, nested transactions, commits, aborts, drops, reopens; return codes, values, cursor positions and counts after every step, contents and `Env.Audit` after every commit, plain and compacting copies at the end |
| `internal/lockfile` | `FuzzLockFile` | the content of a lock file | header and reader-slot decoding of both lock-file versions for every platform's layout, and the reader-table walks of a `Region` over that table (in memory: no file, locks or semaphores) |
| `lmdb` | `FuzzEnvFile` | a whole data file | the public API end to end with `NoLock`: open, bounded reads of every named database (walks both ways, gets, duplicate and multiple-value operations), plain and compacting copy, one write transaction with a nested one, reading again; panics, hangs (an input running for 10 s while fuzzing) and heap growth past 256 MiB fail |
| `lmdb` | `FuzzAPI` | a script of API calls with arbitrary arguments | every write flag combination, cursor operation codes 0-255, nil, empty and oversized keys and values, `PutMulti` strides, unpositioned cursors, nested transactions, databases opened with any flags: no panic, and the committed file passes `Env.Audit` and reads back |
| `tests/engine` (nested module) | `FuzzWriteDifferential` | an oracle, a profile and a script | the lock-step write differential against C 0.9.36 (`writeops`) and C 1.0 (`writeops10`, 1 and 4 KiB pages): cursors after every step, then `mdb_dump`, C's page audit and the pages themselves; skips without the C oracles |

`FuzzEnvFile` starts from small environments of both formats that the
test builds (LMDB 0.9 with 512-byte and 1 KiB pages, LMDB 1.0 with 256-byte
to 1 KiB pages: deep trees, sub-pages, sub-DBs and overflow runs in a few
KiB) and two written by C (`testdata/fuzz/FuzzEnvFile/c09-*`, `c10-*`:
rlmdb's `writeops` and `writeops10` with 512-byte pages). `FuzzWriteModel` decodes its input as an environment (format, page
size from 256 bytes up, dirty-page limit for spilling, `MDB_DUPFIXED` item
size) followed by operations, one byte choosing each operation and the
next ones its database, key, value and flags; the failure message lists
the last 80 operations.

## Running

One target per package at a time (a Go restriction); `-parallel` sets the
number of worker processes (default: all CPUs; the commands use four):

```sh
go test -run='^$' -fuzz='^FuzzPage$'       -fuzztime=10m -parallel 4 ./internal/format/
go test -run='^$' -fuzz='^FuzzPageOps$'    -fuzztime=10m -parallel 4 ./internal/engine/
go test -run='^$' -fuzz='^FuzzWriteModel$' -fuzztime=1h -fuzzminimizetime=300x -parallel 4 ./internal/engine/
go test -run='^$' -fuzz='^FuzzLockFile$'   -fuzztime=5m  -parallel 4 ./internal/lockfile/
go test -run='^$' -fuzz='^FuzzEnvFile$'    -fuzztime=1h -fuzzminimizetime=50x -parallel 4 ./lmdb/
go test -run='^$' -fuzz='^FuzzAPI$'        -fuzztime=30m -parallel 4 ./lmdb/
(cd tests && RLMDB_DIR=<rlmdb checkout> \
  go test -run='^$' -fuzz='^FuzzWriteDifferential$' -fuzztime=30m -parallel 4 ./engine/)
```

Four workers fit a machine with 2 GB of RAM. Every worker costs memory
whatever the target: Go's fuzzer gives each worker process a buffer of
100 MB for mutating inputs, and the coordinating process one more per
worker once that worker has found something (`internal/fuzz`), so a
worker takes about 200 MB before the target allocates anything. With
eight workers, `FuzzEnvFile` ran out of 2 GB within a minute; with four,
every target stayed below 1 GB in eight minutes (measured in the 2 GB
container of `scripts/linux-test.sh`; `FuzzAPI` 0.95 GB, the C
differential 0.8 GB). `FuzzEnvFile`, `FuzzAPI`, `FuzzWriteModel` and
`FuzzWriteDifferential` lower the collector's goal in the fuzzing
processes (GOGC 25 unless `GOGC` is set; `internal/fuzzwatch`), which
keeps the garbage next to those buffers small: with Go's default, the C
differential took 1.2 GB. Raise `-parallel` only where memory allows,
about 250 MB per worker.

`-fuzzminimizetime` keeps the fuzzer from spending up to a minute
minimizing each new interesting input, which with the larger inputs of
`FuzzEnvFile` and `FuzzWriteModel` otherwise stalls progress for minutes
at a time. Interesting inputs accumulate in the Go build cache
(`$(go env GOCACHE)/fuzz/...`), so a later run continues where the last
one stopped.

On Linux, run the same commands through `scripts/linux-test.sh` (the
repository is mounted read-only there: copy a failing input from the
output rather than relying on the fuzzer to write it).

## When a target fails

The fuzzer writes the minimized input to `testdata/fuzz/<Target>/<hash>`
next to the target and prints how to rerun it
(`go test -run=<Target>/<hash>`). Commit that file with the fix: it is
then a regression test of every `go test`. A hang or runaway memory
crashes the worker with every goroutine's stack (a stuck goroutine cannot
be stopped), which the fuzzer records like a panic.

The watchdog (`internal/fuzzwatch`, used by `FuzzEnvFile`, `FuzzAPI` and
`FuzzWriteModel`) measures the input, not the process: the heap limit
applies to the heap's growth since the input began, and the hang limit
only in a fuzzing worker, which runs nothing but inputs. In a normal
`go test` the seeds run after the package's other tests in the same
process, whose heap may still hold gigabytes of their garbage, on a
machine that may be busy with other packages' tests; a hang there is
caught by `go test`'s `-timeout`, also with every goroutine's stack.

## Findings

Stage 4 (fuzzing time per target on Apple M1 Pro, 2-3 workers each, the
machine shared with other work):

| Target | Time | Result |
|---|---|---|
| `FuzzPage` | 6 min | no failure |
| `FuzzMeta` | 4 min | no failure |
| `FuzzIDL` | 5 min | no failure |
| `FuzzCompare` | 4 min | no failure |
| `FuzzPageOps` | 8 min | no failure |
| `FuzzLockFile` | 5 min | no failure |
| `FuzzEnvFile` | 40 min | no failure |
| `FuzzAPI` | 40 min | only the 0.9 main-DB drop of rlmdb `KNOWN_C_BUGS.md` row 21 (ported; no longer generated) |
| `FuzzWriteModel` | 61 min | the `MDB_CURRENT` bug below (in its first seconds), then none in 60 min |
| `FuzzWriteDifferential` | 30 min | no failure (67 thousand rounds against C 0.9 and 1.0) |

* **`MDB_CURRENT` with an item too big for a node on a DUPSORT key with a
  single item** (found by `FuzzWriteModel`; C LMDB 1.0, rlmdb and this
  port alike). The item replaces the old one through `mdb_node_add`,
  which puts it on overflow pages: an `F_BIGDATA` node in a DUPSORT
  database (a new key's first item of that size goes to a sub-DB
  instead). The next put of a duplicate of the key reads the node's data
  as if the item were inline: the first item becomes the overflow
  reference followed by whatever follows it in the page (C, and this port
  unless the node ends the page, where it returns `MDB_CORRUPTED`), the
  overflow run leaks and the DB record still counts it (C's page audit
  and `Env.Audit` report both). Reachable in LMDB 1.0 files with any page
  size (a 103-byte key and a 1978-byte item with 4 KiB pages); not in 0.9
  files with 4 KiB or larger pages. Fixed as rlmdb decided
  (`KNOWN_C_BUGS.md` row 38): the writer never creates an `F_BIGDATA` node in
  a DUPSORT database (such a replacement deletes the item and puts the new
  one as a fresh put would, into a sub-DB), and a lone `F_BIGDATA` item in
  a file C 1.0 wrote is read through its overflow pages, which are freed,
  when the key gets a duplicate. `TestDupsortCurrentTooBigForNode` and
  `tests/engine` (`TestDifferential*CurrentSingleItem`,
  `TestSingleItemOnOverflowPages*`, ported from rlmdb's fix) cover it;
  `FuzzWriteModel` generates the case again (no failure in 4 minutes
  after the fix).
* **`MDB_CURRENT` on a duplicate in a sub-DB that leaves the new item in a
  leaf searches do not look in** (found by `FuzzWriteModel` with 1.0,
  256-byte pages; C LMDB 0.9 and 1.0, rlmdb and this port alike). C keeps
  the new item in the old one's leaf at its index and does not look at the
  branch keys around the leaf, which need not be its items (deleting a
  leaf's first item leaves the key above it). Two cases: the first item of
  a leaf other than the leftmost, replaced by one of another size that
  sorts before the branch key above the leaf (C 0.9's ITS#8447 path, which
  lacks the `fix_parent` it does for an item of the same size); and the
  last item of a leaf other than the rightmost, replaced by one of any
  size at or after the next leaf's branch key (C 0.9 and 1.0, also
  `MDB_DUPFIXED`). `MDB_GET_BOTH` then cannot find the new item, and a
  later put of an item between it and that key lands in the other leaf,
  out of order in the committed file. (C 1.0, which lacks ITS#8447, writes
  an item of another size over the old one's key and keeps that key's
  size.) Fixed: in exactly these cases the replacement deletes the item
  and puts the new one, as `lmdb.h` describes `MDB_CURRENT` with another
  size (as for rows 15 and 38); every other replacement stays C's.
  `TestDupsortCurrentOutsideLeaf` and `tests/engine`
  (`TestDifferential*CurrentDupOutsideLeaf`: the files C's
  `mdb_cursor_del` + `mdb_cursor_put` leave, page for page, also with
  `MDB_DUPFIXED` and a dirty limit of 16 pages) cover it.
