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
| `lmdb` | `FuzzEnvFile` | a whole data file | the public API end to end with `NoLock`: open, bounded reads of every named database (walks both ways, gets, duplicate and multiple-value operations), plain and compacting copy, one write transaction with a nested one, reading again; panics, hangs (an input running for 10 s) and heap growth past 2 GiB fail |
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
number of worker processes (default: all CPUs):

```sh
go test -run='^$' -fuzz='^FuzzPage$'       -fuzztime=10m ./internal/format/
go test -run='^$' -fuzz='^FuzzPageOps$'    -fuzztime=10m ./internal/engine/
go test -run='^$' -fuzz='^FuzzWriteModel$' -fuzztime=1h -fuzzminimizetime=300x ./internal/engine/
go test -run='^$' -fuzz='^FuzzLockFile$'   -fuzztime=5m  ./internal/lockfile/
go test -run='^$' -fuzz='^FuzzEnvFile$'    -fuzztime=1h -fuzzminimizetime=50x ./lmdb/
go test -run='^$' -fuzz='^FuzzAPI$'        -fuzztime=30m ./lmdb/
(cd tests && RLMDB_DIR=<rlmdb checkout> \
  go test -run='^$' -fuzz='^FuzzWriteDifferential$' -fuzztime=30m -parallel 4 ./engine/)
```

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
| `FuzzAPI` | 40 min | only the 0.9 main-DB drop of rlmdb `KNOWN_C_BUGS.md` #21 (ported; no longer generated) |
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
  files with 4 KiB or larger pages. Not fixed: what to write instead is a
  decision for rlmdb (`KNOWN_C_BUGS.md`), and the fix belongs in the
  DUPSORT put path. `TestDupsortCurrentTooBigForNode` (skipped) is the
  reproducer; `FuzzWriteModel` does not generate the case until then.
