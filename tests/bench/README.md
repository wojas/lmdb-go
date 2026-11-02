# bench: the Go implementation against lmdb-go v1

Benchmarks of the Go implementation of LMDB (the port) against its
baseline, the pinned reference in `../cgolmdb` (lmdb-go v1.9.4's Go code on
C LMDB 0.9.36 plus ITS#10615, ITS#10642 and ITS#10643, the same code as
LMDB 0.9.37; including the cost of the cgo calls), with the workloads and the
data of rlmdb's benchmark (rlmdb's `bench`: `src/workloads.rs`,
`c/cbench.c`, `src/data.rs`), plus a group of lmdb-go-specific costs
(`goapi`). Both implementations run in one process.

Measured results, and what the port changed for them: `RESULTS.md`.

## Layout

| path | what | rlmdb counterpart |
|---|---|---|
| `data/` | the data generator, line by line, with its pinning tests | `src/data.rs`, top of `c/cbench.c` |
| `cgo/` | every workload group on the cgo lmdb-go (`package workloads`, `Run(group, params)`); `goapi.go` is the `goapi` group | `c/cbench.c`, `src/workloads.rs` |
| `port/` | the same workloads on the port: generated from `cgo/` by `../gen-port.sh` (`go generate ./bench`), except the hand-written `impl.go` | |
| `run/` | what both share: parameters, samples, results, statistics, the table | `src/main.rs` |
| `bench_test.go` | standard Go benchmarks, `BenchmarkX/impl=cgo` and `/impl=port` | |
| `cmd/lmdb-bench/` | rlmdb-bench's interleaved table, check-data, A/B of two builds | `src/main.rs` |

**One source, two implementations.** The port keeps lmdb-go's API, but
the cgo reference and the port are two packages with distinct types. The
workloads are written once against the cgo package (imported as `lmdb`, in
an import group of its own) and `../gen-port.sh` writes the copy for the
port, which only rewrites that import; `../twins_test.go` fails when the
copy is out of date. Each implementation is compiled against its own
package directly, without interfaces in the measured paths.

## Running

Environments must be on a local file system: the default directory is the
temporary directory (`$TMPDIR` on macOS, on the internal disk); in the
Linux container (`scripts/linux-test.sh`) it is `/tmp`, a tmpfs, never the
bind-mounted checkout. Run from `tests/`:

```sh
go run ./bench/cmd/lmdb-bench                        # every group, both implementations (n = 1 000 000)
go run ./bench/cmd/lmdb-bench -only read,ls -reps 9 -rounds 3
go run ./bench/cmd/lmdb-bench -impl cgo -n 100000    # one implementation
go run ./bench/cmd/lmdb-bench -h
# Linux (4 KiB pages) in the Debian container, environments in its /tmp:
scripts/linux-test.sh sh -c 'cd tests && go run ./bench/cmd/lmdb-bench -dir /tmp/lmdb-bench -n 100000'
# the same on musl (Alpine):
LINUX_TEST_IMAGE=golang:1.25-alpine scripts/linux-test.sh sh -c 'apk add -q build-base &&
    cd tests && go run ./bench/cmd/lmdb-bench -dir /tmp/lmdb-bench -n 100000'
```

Options: `-n N` (keys, default 1 000 000; every other size scales with
it), `-reps R` (default 5), `-sync-commits K` (commits of `txn_sync`,
default 1000), `-dir D`, `-only G1,G2`, `-rounds K` (run every group K
times per implementation, alternating which goes first; the samples are
pooled), `-raw` (also print every sample), `-impl both|cgo|port`.

Every workload runs one warm-up iteration and `-reps` timed ones, in a
fresh environment per group and implementation; the implementation that
goes first alternates between groups and rounds. The table has, per
implementation, the median and minimum time per operation (ns/op),
operations per second (from the median), the ratio of the medians
port/cgo (**below 1 means the port is faster**) and the Go heap
allocations per operation (counted with `runtime.ReadMemStats` outside the
timed part of each repetition). The head names the OS (major version),
CPU, page size and Go version, nothing that identifies a host; the
environment directory goes to stderr.

### Memory

The default run (`-n 1000000`, every group, both implementations) fits
on a machine with 2 GB of RAM. The process itself takes up to 360 MB (the
data sets, dirty pages and the heaps of both implementations); the rest is
the environment of the group that runs (each group's is removed before
the next starts), whose data file reaches 1.1 GB in the `ls` group. On a
disk file system that file is page cache, which the kernel reclaims under
pressure (the process's resident size, which counts the mapped file, then
reaches 1.4 GB); on a tmpfs it is memory: in the 2 GB container of
`scripts/linux-test.sh`, with the environments in its tmpfs `/tmp`, the
default run peaks at 1.4 GB. Where `/tmp` is a tmpfs of half the RAM (as
on many Linux distributions), a machine with 2 GB has 1 GB there, too
little for the `ls` group: give such a machine `-dir` on a disk file
system. The `go test` benchmarks below (`-lmdb.n 100000`) peak at about
0.5 GB in that container, compiling included.

### Go benchmarks and benchstat

`bench_test.go` has one benchmark per group, with a sub-benchmark per
implementation. A `b.N` iteration is one timed repetition of every
workload of the group; the numbers to compare are the per-workload
metrics `<workload>-ns/op` (median over the `b.N` repetitions) and
`<workload>-allocs/op`. The built-in `ns/op` is the group's wall time per
repetition including its setup and warm-up. The size comes from
`-lmdb.n` (default 100 000) and `-lmdb.sync-commits` (default 100):

```sh
go test ./bench -run '^$' -bench . -benchtime 5x -count 6 -args -lmdb.n 100000 | tee new.txt
benchstat -col /impl new.txt                         # cgo against port, per metric
go test ./bench -run '^$' -bench 'Read|LS' -benchtime 5x -count 6
```

### A/B of two builds of the port

To measure a change to the port, keep a build of `lmdb-bench` from before
it and compare, interleaved; both run as worker subprocesses
(`lmdb-bench -worker <impl> <group> <dir> <n> <reps> <sync_commits>`,
cbench's protocol), `base` the old build and `new` this one:

```sh
go build -o /tmp/lmdb-bench-old ./bench/cmd/lmdb-bench
# ... change the port ...
go build -o /tmp/lmdb-bench ./bench/cmd/lmdb-bench
/tmp/lmdb-bench -ab /tmp/lmdb-bench-old -rounds 3 -reps 3   # -impl port is the default with -ab
```

Machines are noisy: compare only runs that were interleaved in one
session, never two separate runs. (Two copies of the same cgo build
compared this way differ by up to about 10% per workload at `-n 20000`.)

### Same data as rlmdb

`data/` must produce exactly the data of rlmdb's generator, so that every
implementation builds identical databases and does the same operations.
`-check-data` hashes everything the generator produces for `n` (keys, the
value pool, index streams, permutations, duplicates, the `ls` data set and
its update stream), as cbench and rlmdb-bench do, and compares the hash
with theirs:

```sh
go run ./bench/cmd/lmdb-bench -check-data -n 100000 \
    -rlmdb-bench <rlmdb>/target/release/rlmdb-bench   # and/or -cbench <path to cbench>
```

It exits with status 1 if they differ. `go test ./bench/data` pins the
generator as `data.rs`'s tests do.

## Workloads

All environments use `MDB_NOSYNC` except `txn_sync`, a 4 GiB map, 4 DBs
and 126 readers; transactions are committed every 10 000 operations
unless stated otherwise. Read transactions set `RawRead`, so values are
slices of the map as in C and rlmdb (lmdb-go copies them by default; see
`get_copy`). Write transactions run on the goroutine that calls `Run`,
which locks it to its OS thread as lmdb-go requires. `n` is `-n`.

| group | workload | what is timed |
|---|---|---|
| `seq_put` | `seq_put_append` | `n` puts of ascending keys with `Append` into a new DB |
| `rand_put` | `rand_put` | `n` puts of random keys into a new DB |
| `read` | `rand_get` | `n` `Txn.Get` of random existing keys, one read txn |
| | `scan_fwd`, `scan_bwd` | a full `Next` / `Prev` cursor scan |
| | `readers8_get` | 8 goroutines, each its own read txn and `n` random gets (wall time, all gets) |
| `overwrite` | `rand_overwrite` | `n` puts overwriting every key once, random order |
| `dupsort` | `dup_put` | `n/10` keys x 10 duplicates (100 bytes) into a new `DupSort` DB |
| | `dup_get_both` | `n` `GetBoth` lookups of random pairs |
| | `dup_next_dup` | for every key in random order: `Set`, then `NextDup` to the end |
| `overflow` | `put_4k`, `put_64k` | `n/20` puts of 4 KiB values, `n/200` puts of 64 KiB values (txn per 1000) |
| | `get_4k`, `get_64k` | `n` random gets, touching the first and last byte |
| `delete` | `del_rand` | delete all `n` keys in random order (the DB is rebuilt untimed per rep) |
| `small_txn` | `txn_nosync` | `n/20` transactions of one put each, `MDB_NOSYNC` |
| | `txn_sync` | `-sync-commits` such transactions with the default (synchronous) commit (meaningless on the container's tmpfs) |
| `mixed` | `mixed_writer` | `n/2000` commits of 100 random overwrites each, while 4 goroutines scan |
| | `mixed_readers` | entries those 4 scanners read during the writer's run (ns per entry over the writer's wall time) |
| `ls` | `ls_scan` | a Lightning Stream sync scan of the `ls` data set (`5n` records): one read txn, a full `Next` cursor scan reading every value's header txnid and counting those above the last sync (none are); ns per record |
| | `ls_update` | `5n/100` puts of a new value (fresh header, same-size payload) for random existing `ls` keys (duplicates possible), committed every 10 000 puts; ns per put |
| `goapi` | see below | lmdb-go's own costs (Go only) |

The `ls` group loosely simulates [Lightning Stream](https://github.com/PowerDNS/lightningstream)
syncing a PowerDNS Authoritative database; rlmdb's `bench/README.md`
describes its data set. Its database is built once per group (untimed);
`ls_scan` runs first, then `ls_update`, and an untimed scan afterwards
checks that exactly the updated records are above the last sync.

On macOS (16 KiB pages) 4 KiB values fit in a leaf page; on Linux (4 KiB
pages) they go to overflow pages. 64 KiB values are overflow pages
everywhere. The largest data set is the `ls` group's, about 1.3 GB at the
default `-n`.

### `goapi`: what Go users pay for the API

One environment with the `rand_put` data set; random existing keys. Each
workload has a baseline in the group. Transaction-per-operation workloads
do `n/10` operations, the others `n`.

| workload | what is timed | compare with |
|---|---|---|
| `get_raw` | `Txn.Get` with `RawRead` (a slice into the map), the key a sub-slice of one long-lived buffer: `rand_get` | |
| `get_copy` | the same with `RawRead` false, lmdb-go's default: every value is copied into a new slice | `get_raw` |
| `get_key_local` | the key copied into a local `[16]byte` per lookup: with cgo the array escapes (a Go pointer passed to C), one heap allocation per lookup | `get_raw` |
| `get_key_string` | `txn.Get(db, []byte(s))` from a string key: a conversion, and with cgo an allocation, per lookup | `get_raw` |
| `cursor_set` | `Cursor.Get(key, nil, Set)` through one cursor | `get_raw` |
| `cursor_open_set` | `OpenCursor` + `Set` + `Close` per lookup | `cursor_set` |
| `view_get` | one lookup per `Env.View` (managed read txn, closure) | `txn_get` |
| `view_empty` | an `Env.View` that does nothing: the read transaction's own cost | `view_get` |
| `view8_get` | `view_get` on 8 goroutines at once, `n/10` lookups each (wall time per lookup over all of them): concurrent begins and ends of read transactions | `view_get` |
| `txn_get` | one lookup per `BeginTxn(nil, Readonly)` + `Abort` | `renew_get` |
| `renew_get` | one lookup per `Reset` + `Renew` of one read txn | `txn_get` |
| `cursor_renew_set` | `Reset` + `Renew` + `Cursor.Renew` + `Set` per lookup: a read txn and a cursor reused | `renew_get` |
| `update_put` | one overwrite per `Env.Update` (managed write txn, `MDB_NOSYNC`) | `txn_put` |
| `txn_put` | one overwrite per `BeginTxn` + `Put` + `Commit` | `txn_nosync` |

## Differences from rlmdb's benchmark

* lmdb-go opens every environment with `MDB_NOTLS`; cbench uses C's
  default TLS mode (rlmdb behaves like `MDB_NOTLS`).
* The reference has the same `mdb.c` as rlmdb's cbench links. The released
  lmdb-go v1.9.4 bundles C LMDB 0.9.35, from before the changes to DUPSORT
  page accounting (ITS#9388, ITS#10643) and the meta-page sync on macOS
  (ITS#10454, a second full flush per commit), so its `dup_put` and
  `txn_sync` differ from the reference's for those reasons (`RESULTS.md`).
* Both implementations run in one process, group after group; cbench and
  rlmdb-bench run every group in a fresh process.
* Readers are goroutines, not threads; while they wait for the start they
  yield (`runtime.Gosched`), so that they cannot starve the writer on a
  machine with fewer CPUs than goroutines.
* Results carry a third number per repetition, the Go heap allocations;
  `run.ParseLine` reads cbench's and rlmdb-bench's two-number results too.
* The report has no `uname` (kernel build strings) and no host names.
