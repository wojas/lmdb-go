# Stage 3 results: the Go implementation against lmdb-go v1

What stage 3 (benchmarks and performance, `PLAN.md`) measured and changed.
The baseline is lmdb-go v1.9.4 on cgo (C LMDB 0.9.35, the pinned copy in
`../cgolmdb`), including its cgo call overhead. **Ratios are port/cgo of
the medians: below 1 means the port is faster.** Times are ns per
operation (median of all samples; `lmdb-bench` prints the minimum in
parentheses), allocations are Go heap allocations per operation.

## How it was measured

* `cmd/lmdb-bench` (README.md), both implementations in one process,
  interleaved per group and round, the side that goes first alternating:
  `-n 1000000 -rounds 3 -reps 5` (15 timed repetitions per workload and
  implementation after a warm-up each), `-sync-commits 1000`.
* "Before" is `main` at the start of stage 3 (4b0232f), "after" is the
  end of this branch; each table comes from one run of both
  implementations, so its ratios are interleaved measurements. The
  per-change A/B measurements (`lmdb-bench -ab`, two builds of the port)
  are in the commit messages.
* **macOS**: Apple M1 Pro (8 performance and 2 efficiency cores), 32 GiB
  RAM, macOS 15, Go 1.25.4, 16 KiB pages. Environments in `$TMPDIR`, APFS
  on the internal SSD.
* **Linux arm64**: a Lima VM on the same Mac (8 vCPUs, 12 GiB), container
  image `golang:1.25-bookworm` (glibc, Go 1.25.14), 4 KiB pages.
  Environments in the container's `/tmp`, a tmpfs, so `txn_sync` (fsync)
  timings mean nothing there.
* The machine was shared with other work during every run (load average
  20 to 40 on 10 cores; the VM ran other containers), so single medians
  are noisy (up to about 15% between two runs of the same build, more for
  the multi-threaded and fsync workloads); the interleaving keeps the
  ratios fair, and the allocation counts are exact.
* Allocation counts of the public API's hot paths are pinned by
  `lmdb/alloc_test.go` (TestAllocs, `testing.AllocsPerRun`, failing on
  any change; BenchmarkAllocs reports allocs/op and the exact mean as
  mallocs/op).

## Summary: the priority workloads

Ratios port/cgo of the medians (**below 1: the port is faster**), from
the four runs below (n = 1 000 000), and the port's Go heap allocations
per operation (lmdb-go v1's are 0, and 1 for the transaction-per-operation
workloads).

| workload | macOS before | macOS after | Linux before | Linux after | port allocs/op before (macOS / Linux) | port allocs/op after (macOS / Linux) |
|---|---:|---:|---:|---:|---:|---:|
| ls_scan | 0.43 | **0.39** | 0.44 | **0.60** | 0.00 / 0.00 | 0.00 / 0.00 |
| ls_update | 1.12 | **1.02** | 0.80 | **0.89** | 0.96 / 1.36 | 0.00 / 0.00 |
| rand_get | 0.73 | **0.74** | 1.00 | **0.98** | 0.00 / 0.00 | 0.00 / 0.00 |
| readers8_get | 0.75 | **0.62** | 1.01 | **0.83** | 0.00 / 0.00 | 0.00 / 0.00 |
| scan_fwd | 0.43 | **0.43** | 0.57 | **0.57** | 0.00 / 0.00 | 0.00 / 0.00 |
| scan_bwd | 0.50 | **0.49** | 0.61 | **0.57** | 0.00 / 0.00 | 0.00 / 0.00 |
| rand_put | 1.16 | **0.96** | 1.10 | **0.94** | 0.47 / 0.92 | 0.01 / 0.01 |
| rand_overwrite | 1.10 | **1.02** | 1.06 | **0.96** | 0.67 / 0.96 | 0.00 / 0.00 |
| del_rand | 1.42 | **1.05** | 0.91 | **0.79** | 0.85 / 1.52 | 0.00 / 0.00 |
| txn_nosync | 2.81 | **1.13** | 2.66 | **1.10** | 30 / 32 | 1.01 / 1.04 |
| txn_sync | 1.99 | **1.92** | 2.67 | **1.20** | 30 / 30 | 1.04 / 1.08 |
| update_put | 1.84 | **1.17** | 1.52 | **1.11** | 50 / 52 | 1.04 / 1.07 |
| put_64k | 0.73 | **0.65** | 0.98 | **0.68** | 1.11 / 1.16 | 0.22 / 0.23 |
| get_64k | 0.90 | **0.87** | 0.97 | **0.99** | 0.00 / 0.00 | 0.00 / 0.00 |
| put_4k | 1.75 | **1.08** | 1.11 | **0.78** | 2.52 / 1.41 | 0.04 / 0.03 |
| mixed_writer | 1.63 | **0.70** | 1.07 | **0.90** | 2.09 / 2.74 | 0.02 / 0.03 |
| mixed_readers | 0.40 | **0.33** | 0.46 | **0.47** | 0.00 / 0.01 | 0.00 / 0.00 |
| seq_put_append | 1.82 | **1.64** | 1.47 | **1.39** | 0.02 / 0.04 | 0.00 / 0.00 |
| dup_put | 1.71 | **1.38** | 1.61 | **1.29** | 1.93 / 2.08 | 0.00 / 0.00 |

The full tables, with times in ns/op, follow at the end.

## What changed

The write path allocated: every top-level write transaction made a new
`Txn` with its dirty list and an empty page pool, so every page it touched
was a fresh 16 KiB (4 KiB on Linux) allocation; with the small live heap
of a benchmark the collector then ran every few commits, which cost more
than the allocations themselves (a one-put commit spent ~14 us of user
CPU on macOS, C 2 us). In commit order:

| commit | change | effect |
|---|---|---|
| `lmdb: allocation counts ...` | TestAllocs/BenchmarkAllocs, with the counts at the start | the counts below |
| `engine: reuse the top-level write transaction per Env ...` | one write `Txn` per `Env` with its DB table, dirty list, page pool, cursor arena, lists (C's `me_txn0`, rlmdb's `SpareTxn`); nested transactions take the parent's spare pages over instead of copying the list | Update 30 -> 5 allocs; txn_nosync (macOS) 18.5 -> 8.4 us |
| `engine: no allocations in commit's flush list, free DB keys and pghead` | flush list, free DB keys, `me_pghead` memory kept in the write txn | Update 5 -> 1 alloc (the `*Txn`, as v1) |
| `engine: split and rebalance scratch ...` | page copies of splits/merges from a scratch stack, node offsets per split depth, rebalance key/value buffers | del_rand 0.30 -> 0.0009 allocs/op; put_4k -42%, rand_put -6%, del_rand -8% (A/B minima) |
| `engine: pool overflow runs too` | overflow runs pooled by length | 64 KiB put 2 -> 1 alloc |
| `engine: copy touched pages as mdb_page_copy does ...` | page touch skips the free gap, as C | txn_nosync -5%, rand_put -4% |
| `engine: DUPSORT put scratch ...` | sub-page building in write-txn scratch (C's `me_pbuf`) | DupSort put 2 -> 0 allocs; dup_put 1.93 -> 0.008 allocs/op |
| `engine: reuse a nested transaction's memory ...` | a parent keeps its last child's memory for the next | Sub 14 -> 1 alloc, 3.2-4.1 -> 1.3 us |

Allocation counts of the API (TestAllocs, per operation, whole
allocations; the same with and without cgo, on macOS and Linux):

| hot path | before | after | lmdb-go v1 (lmdb-bench) |
|---|---:|---:|---:|
| View with one Get | 1 | 1 | 1 |
| Get, RawRead / copying | 0 / 1 | 0 / 1 | 0 / 1 |
| cursor First, Next, Prev, Set, SetRange, GetBoth | 0 | 0 | 0 |
| Txn Reset+Renew, Cursor Renew | 0 | 0 | 0 |
| Update with one put | 30 | 1 | 1 |
| BeginTxn + Put + Commit | 30 | 1 | 1 |
| Put, overwrite, Del, PutReserve in a transaction | 0 | 0 | 0 |
| DupSort put in a transaction | 2 | 0 | 0 |
| Sub (nested transaction) with one put | 18 | 1 | |
| Update with one 64 KiB put | 31 | 1 | |

Tried and not kept (measured, reverted):

* `pwritev` for commit's batched writes on Linux (instead of gathering
  into one buffer): no difference on tmpfs.
* A branch-free binary search in `searchPage`. A lower-bound search is
  not equivalent to C's: after the C behaviours the port keeps (keys
  replaced in place), pages need not be sorted, and there what C finds
  depends on its probe sequence (the write differential caught it).
  Masking C's own step instead was 15-40% slower for lookups on the M1:
  with the branch the CPU speculates into the next probe's memory load.

## What remains, and why

* **One-put commits** (`txn_nosync` 1.13 / 1.10, `update_put` 1.17 /
  1.11, `txn_put` 1.09 / 0.99; macOS / Linux): both implementations
  write the same pages with the same system calls (counted with
  instrumented builds: 3.92 writes and 49 KiB per commit), and the port
  now allocates once per commit, like v1. What is left is the Go side of
  those calls: every system call goes through the runtime (on macOS
  through the libSystem trampolines), and taking and releasing the writer
  lock are the port's two cgo calls per write transaction. On macOS a
  16 KiB `pwrite` costs a few microseconds, which dominates either way.
* **`txn_sync` on macOS** (1.92): the port, like C LMDB 0.9.36 and later
  and rlmdb (ITS#10454), follows the `O_DSYNC` meta-page write with
  `F_FULLFSYNC`, because `O_DSYNC` does not flush the drive's cache on
  macOS. lmdb-go v1 bundles 0.9.35, which does not: one full flush per
  commit there, two here. That is durability, not overhead. On Linux the
  number is meaningless (tmpfs).
* **`del_rand` on macOS** (1.05; Linux 0.79): about 10% more CPU than C
  per delete (user+system time over 200 000 deletes: 1.85-2.0 us against
  1.7-1.75 us), spread over the rebalancing code: write cursors that hold
  page numbers and copy their state to read cursors for searches
  (rlmdb's structure, see `internal/engine/PORTING.md`), where C follows
  pointers. Changing that structure is a large refactor that stage 3 did
  not take.
* **Reads on Linux** (`rand_get` 0.98, the `goapi` lookups 0.95-1.15; on
  macOS 0.67-0.77): lookups are bound by the binary search in each page
  (75% of the read profile: cache misses and mispredicted comparisons,
  which C pays as well), with one more tree level at 4 KiB pages, while
  v1's cgo call per lookup costs less on Linux than on macOS. The
  lookups whose ratio moved between the before and after runs (e.g.
  `get_raw` 1.07 -> 1.15, `cursor_open_set` 0.89 -> 1.15) run unchanged
  code: that is the noise of the shared VM. A branch-free search was
  tried and rejected (above). `ls_scan` on Linux (0.44 -> 0.60) changed
  because cgo's time did (145 -> 104 ns), the port's did not (64 -> 62
  ns).
* **Sequential append and DUPSORT puts** (`seq_put_append` 1.64 / 1.39,
  `dup_put` 1.38 / 1.29): low priority by the brief. Both allocate
  nothing now; `seq_put_append` spends about 60% of its time in commit
  writing the new pages (the same pages as C), not investigated further.
* **API overheads** (`goapi`): View, Update, BeginTxn+Commit and
  OpenCursor cost one allocation each (the `*Txn` or `*Cursor` the API
  returns, as in v1); Reset+Renew and Cursor.Renew allocate nothing and
  are faster than v1 on macOS (0.73-0.74), at parity on Linux. Beginning
  a read transaction takes the shared reader mutex, as C does with
  `MDB_NOTLS` (lmdb-go's mode), so `txn_get` is at parity on Linux (0.77
  on macOS, where v1's cgo calls cost more); keeping released reader
  slots owned by the process would avoid that
  (and beat C), but other processes would then see idle slots of this
  process in the reader table (`mdb_stat -r`), so that is left to the
  owner.
* **GC**: with no steady-state allocations on these paths the collector
  is no longer a factor (context switches per one-put commit on macOS:
  1.16 before, 0.05 after). The remaining allocations in lmdb-bench's
  write groups (`put_64k` 0.22, `put_4k` 0.04 allocs/op) are the first
  transaction of the fresh environment each repetition opens.

## Full tables

Each table is one `lmdb-bench -rounds 3 -reps 5` run (n = 1 000 000) of
both implementations, as printed: ns/op median (minimum), ops/s from the
median, ratio port/cgo of the medians (below 1: the port is faster), Go
heap allocations per operation (median over the repetitions). "Before" is
`main` at 4b0232f, "after" the end of this branch (the build measured
predates the fix folded into the nested-transaction commit, which only
changes nested transactions; no workload here uses them).

### macOS, before

* macOS 15, arm64, CPU: Apple M1 Pro, 10 cores, 32 GiB RAM
* OS page size (= LMDB page size): 16384
* Go: go1.25.4
* cgo: lmdb-go v1.9.4 (cgo, LMDB 0.9.35: (Jan 27, 2026))
* port: the Go implementation (LMDB 0.9.85: (lmdb-go, Go port of rlmdb))

n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 123 (116) | 224 (214) | 8.13M | 4.46M | 1.82 | 0.00 | 0.02 |
| rand_put | 1000000 | 3226 (2670) | 3728 (3195) | 310.0k | 268.2k | 1.16 | 0.00 | 0.47 |
| rand_get | 1000000 | 1358 (1238) | 996 (894) | 736.5k | 1.00M | 0.73 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 71.9 (70.8) | 31.1 (30.3) | 13.91M | 32.16M | 0.43 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 69.6 (68.7) | 34.5 (33.4) | 14.37M | 29.02M | 0.50 | 0.00 | 0.00 |
| readers8_get | 8000000 | 199 (174) | 150 (121) | 5.02M | 6.68M | 0.75 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 4775 (3949) | 5240 (4679) | 209.4k | 190.8k | 1.10 | 0.00 | 0.67 |
| dup_put | 1000000 | 951 (889) | 1630 (1575) | 1.05M | 613.7k | 1.71 | 0.00 | 1.93 |
| dup_get_both | 1000000 | 1269 (1126) | 1120 (897) | 788.0k | 893.3k | 0.88 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 189 (169) | 136 (129) | 5.30M | 7.36M | 0.72 | 0.20 | 0.00 |
| put_4k | 50000 | 7352 (6574) | 12859 (10899) | 136.0k | 77.8k | 1.75 | 0.00 | 2.52 |
| get_4k | 1000000 | 825 (714) | 688 (616) | 1.21M | 1.45M | 0.83 | 0.00 | 0.00 |
| put_64k | 5000 | 27412 (23301) | 20002 (16613) | 36.5k | 50.0k | 0.73 | 0.00 | 1.11 |
| get_64k | 1000000 | 374 (337) | 336 (296) | 2.67M | 2.97M | 0.90 | 0.00 | 0.00 |
| del_rand | 1000000 | 3680 (3409) | 5214 (4234) | 271.8k | 191.8k | 1.42 | 0.00 | 0.85 |
| txn_nosync | 50000 | 7336 (6928) | 20640 (19617) | 136.3k | 48.4k | 2.81 | 1.00 | 30 |
| txn_sync | 1000 | 5413194 (4910995) | 10786143 (10199746) | 185 | 93 | 1.99 | 1.00 | 30 |
| mixed_writer | 50000 | 7285 (5842) | 11862 (9375) | 137.3k | 84.3k | 1.63 | 0.01 | 2.09 |
| mixed_readers | 12896256 | 27.1 (20.7) | 10.7 (8.8) | 36.93M | 93.15M | 0.40 | 0.00 | 0.00 |
| ls_scan | 5000000 | 73.9 (71.9) | 31.6 (30.6) | 13.53M | 31.62M | 0.43 | 0.00 | 0.00 |
| ls_update | 50000 | 6612 (5359) | 7398 (6402) | 151.2k | 135.2k | 1.12 | 0.00 | 0.96 |
| get_raw | 1000000 | 1394 (1254) | 1007 (875) | 717.6k | 992.9k | 0.72 | 0.00 | 0.00 |
| get_copy | 1000000 | 1612 (1358) | 1110 (947) | 620.4k | 900.7k | 0.69 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1509 (1276) | 1050 (958) | 662.6k | 952.2k | 0.70 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1641 (1380) | 1183 (1029) | 609.5k | 845.3k | 0.72 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1478 (1303) | 1049 (959) | 676.8k | 953.3k | 0.71 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1654 (1532) | 1019 (963) | 604.4k | 981.6k | 0.62 | 1.00 | 1.00 |
| view_get | 100000 | 2270 (2197) | 1970 (1860) | 440.5k | 507.7k | 0.87 | 1.00 | 1.00 |
| txn_get | 100000 | 2584 (2316) | 2018 (1889) | 387.0k | 495.5k | 0.78 | 1.00 | 1.00 |
| renew_get | 100000 | 1449 (1242) | 1033 (936) | 690.4k | 968.0k | 0.71 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1442 (1412) | 1083 (1030) | 693.7k | 923.2k | 0.75 | 0.00 | 0.00 |
| update_put | 100000 | 31315 (28215) | 57573 (46983) | 31.9k | 17.4k | 1.84 | 1.00 | 50 |
| txn_put | 100000 | 32378 (28428) | 54861 (47112) | 30.9k | 18.2k | 1.69 | 1.00 | 50 |

### macOS, after

* macOS 15, arm64, CPU: Apple M1 Pro, 10 cores, 32 GiB RAM
* OS page size (= LMDB page size): 16384
* Go: go1.25.4
* cgo: lmdb-go v1.9.4 (cgo, LMDB 0.9.35: (Jan 27, 2026))
* port: the Go implementation (LMDB 0.9.85: (lmdb-go, Go port of rlmdb))

n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 118 (116) | 194 (186) | 8.44M | 5.15M | 1.64 | 0.00 | 0.00 |
| rand_put | 1000000 | 2996 (2747) | 2871 (2520) | 333.8k | 348.3k | 0.96 | 0.00 | 0.01 |
| rand_get | 1000000 | 1384 (1260) | 1018 (910) | 722.3k | 982.6k | 0.74 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 71.8 (71.0) | 31.2 (30.2) | 13.93M | 32.10M | 0.43 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 69.6 (67.9) | 33.8 (32.6) | 14.37M | 29.59M | 0.49 | 0.00 | 0.00 |
| readers8_get | 8000000 | 227 (147) | 141 (115) | 4.41M | 7.09M | 0.62 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 4485 (4094) | 4564 (3875) | 223.0k | 219.1k | 1.02 | 0.00 | 0.00 |
| dup_put | 1000000 | 931 (828) | 1285 (1229) | 1.07M | 778.5k | 1.38 | 0.00 | 0.00 |
| dup_get_both | 1000000 | 1220 (1134) | 948 (869) | 819.9k | 1.05M | 0.78 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 180 (172) | 125 (114) | 5.56M | 8.00M | 0.69 | 0.20 | 0.00 |
| put_4k | 50000 | 6425 (6128) | 6951 (6573) | 155.6k | 143.9k | 1.08 | 0.00 | 0.04 |
| get_4k | 1000000 | 773 (656) | 678 (521) | 1.29M | 1.47M | 0.88 | 0.00 | 0.00 |
| put_64k | 5000 | 25581 (23705) | 16570 (14697) | 39.1k | 60.3k | 0.65 | 0.00 | 0.22 |
| get_64k | 1000000 | 344 (318) | 301 (276) | 2.91M | 3.32M | 0.87 | 0.00 | 0.00 |
| del_rand | 1000000 | 3632 (3197) | 3813 (3439) | 275.4k | 262.3k | 1.05 | 0.00 | 0.00 |
| txn_nosync | 50000 | 7858 (6848) | 8888 (8508) | 127.3k | 112.5k | 1.13 | 1.00 | 1.01 |
| txn_sync | 1000 | 6362098 (5003901) | 12197470 (10044469) | 157 | 82 | 1.92 | 1.00 | 1.04 |
| mixed_writer | 50000 | 9009 (5572) | 6264 (5740) | 111.0k | 159.6k | 0.70 | 0.01 | 0.02 |
| mixed_readers | 16445440 | 27.4 (21.9) | 9.0 (8.2) | 36.53M | 111.17M | 0.33 | 0.00 | 0.00 |
| ls_scan | 5000000 | 79.3 (71.0) | 31.1 (30.3) | 12.61M | 32.17M | 0.39 | 0.00 | 0.00 |
| ls_update | 50000 | 5751 (5271) | 5860 (5143) | 173.9k | 170.7k | 1.02 | 0.00 | 0.00 |
| get_raw | 1000000 | 1390 (1222) | 998 (855) | 719.2k | 1.00M | 0.72 | 0.00 | 0.00 |
| get_copy | 1000000 | 1415 (1233) | 1021 (956) | 706.6k | 979.5k | 0.72 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1457 (1272) | 977 (860) | 686.5k | 1.02M | 0.67 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1545 (1311) | 1054 (957) | 647.3k | 948.7k | 0.68 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1461 (1334) | 997 (909) | 684.2k | 1.00M | 0.68 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1625 (1477) | 1065 (883) | 615.3k | 939.4k | 0.66 | 1.00 | 1.00 |
| view_get | 100000 | 2921 (2420) | 1989 (1726) | 342.4k | 502.8k | 0.68 | 1.00 | 1.00 |
| txn_get | 100000 | 2612 (2404) | 2010 (1898) | 382.9k | 497.5k | 0.77 | 1.00 | 1.00 |
| renew_get | 100000 | 1537 (1433) | 1136 (1057) | 650.5k | 879.9k | 0.74 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1691 (1467) | 1233 (1117) | 591.5k | 811.2k | 0.73 | 0.00 | 0.00 |
| update_put | 100000 | 31383 (27014) | 36747 (31900) | 31.9k | 27.2k | 1.17 | 1.00 | 1.04 |
| txn_put | 100000 | 31417 (27414) | 34231 (31420) | 31.8k | 29.2k | 1.09 | 1.00 | 1.04 |

### Linux arm64 (container, tmpfs), before

* linux arm64, CPU: Apple (model not reported), 8 cores, 12 GiB RAM
* OS page size (= LMDB page size): 4096
* Go: go1.25.14
* cgo: lmdb-go v1.9.4 (cgo, LMDB 0.9.35: (Jan 27, 2026))
* port: the Go implementation (LMDB 0.9.85: (lmdb-go, Go port of rlmdb))

n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 162 (155) | 239 (230) | 6.16M | 4.18M | 1.47 | 0.00 | 0.04 |
| rand_put | 1000000 | 3027 (2836) | 3323 (3147) | 330.4k | 300.9k | 1.10 | 0.00 | 0.92 |
| rand_get | 1000000 | 1219 (1108) | 1221 (1078) | 820.6k | 818.8k | 1.00 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 99.1 (90.3) | 56.1 (51.6) | 10.09M | 17.81M | 0.57 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 91.6 (87.6) | 55.6 (49.8) | 10.92M | 17.98M | 0.61 | 0.00 | 0.00 |
| readers8_get | 8000000 | 210 (167) | 213 (180) | 4.77M | 4.70M | 1.01 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 4041 (3805) | 4298 (3916) | 247.5k | 232.7k | 1.06 | 0.00 | 0.96 |
| dup_put | 1000000 | 1129 (1063) | 1822 (1673) | 886.1k | 549.0k | 1.61 | 0.00 | 2.08 |
| dup_get_both | 1000000 | 1219 (1042) | 1299 (990) | 820.5k | 769.8k | 1.07 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 207 (173) | 166 (142) | 4.83M | 6.04M | 0.80 | 0.20 | 0.00 |
| put_4k | 50000 | 5870 (5374) | 6515 (5465) | 170.4k | 153.5k | 1.11 | 0.00 | 1.41 |
| get_4k | 1000000 | 748 (582) | 776 (460) | 1.34M | 1.29M | 1.04 | 0.00 | 0.00 |
| put_64k | 5000 | 50167 (44895) | 49176 (29454) | 19.9k | 20.3k | 0.98 | 0.00 | 1.16 |
| get_64k | 1000000 | 377 (328) | 366 (288) | 2.65M | 2.73M | 0.97 | 0.00 | 0.00 |
| del_rand | 1000000 | 5727 (4966) | 5192 (4483) | 174.6k | 192.6k | 0.91 | 0.00 | 1.52 |
| txn_nosync | 50000 | 5288 (4980) | 14089 (12441) | 189.1k | 71.0k | 2.66 | 1.00 | 32 |
| txn_sync | 1000 | 3777 (3653) | 10095 (8916) | 264.7k | 99.1k | 2.67 | 1.00 | 30 |
| mixed_writer | 50000 | 6786 (5640) | 7263 (6212) | 147.4k | 137.7k | 1.07 | 0.01 | 2.74 |
| mixed_readers | 9892864 | 31.4 (27.9) | 14.4 (13.6) | 31.86M | 69.20M | 0.46 | 0.00 | 0.01 |
| ls_scan | 5000000 | 145 (98.0) | 64.3 (57.1) | 6.88M | 15.56M | 0.44 | 0.00 | 0.00 |
| ls_update | 50000 | 6767 (4940) | 5436 (4861) | 147.8k | 184.0k | 0.80 | 0.00 | 1.36 |
| get_raw | 1000000 | 1140 (1018) | 1218 (1033) | 877.0k | 821.2k | 1.07 | 0.00 | 0.00 |
| get_copy | 1000000 | 1217 (1098) | 1248 (1051) | 821.7k | 801.1k | 1.03 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1322 (1030) | 1456 (1083) | 756.6k | 686.8k | 1.10 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1550 (1346) | 1540 (1334) | 645.3k | 649.5k | 0.99 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1310 (1138) | 1242 (1094) | 763.6k | 805.2k | 0.95 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1629 (1306) | 1453 (1103) | 613.9k | 688.2k | 0.89 | 1.00 | 1.00 |
| view_get | 100000 | 1750 (1403) | 1592 (1436) | 571.6k | 628.2k | 0.91 | 1.00 | 1.00 |
| txn_get | 100000 | 2380 (1526) | 2184 (1520) | 420.1k | 457.9k | 0.92 | 1.00 | 1.00 |
| renew_get | 100000 | 1566 (1249) | 1697 (1295) | 638.7k | 589.2k | 1.08 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1506 (1193) | 1394 (1266) | 664.1k | 717.1k | 0.93 | 0.00 | 0.00 |
| update_put | 100000 | 20322 (17547) | 30865 (23663) | 49.2k | 32.4k | 1.52 | 1.00 | 52 |
| txn_put | 100000 | 18355 (16721) | 28037 (24213) | 54.5k | 35.7k | 1.53 | 1.00 | 52 |

### Linux arm64 (container, tmpfs), after

* linux arm64, CPU: Apple (model not reported), 8 cores, 12 GiB RAM
* OS page size (= LMDB page size): 4096
* Go: go1.25.14
* cgo: lmdb-go v1.9.4 (cgo, LMDB 0.9.35: (Jan 27, 2026))
* port: the Go implementation (LMDB 0.9.85: (lmdb-go, Go port of rlmdb))

n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 160 (154) | 223 (212) | 6.23M | 4.49M | 1.39 | 0.00 | 0.00 |
| rand_put | 1000000 | 3109 (2736) | 2923 (2723) | 321.6k | 342.1k | 0.94 | 0.00 | 0.01 |
| rand_get | 1000000 | 1300 (1062) | 1270 (1183) | 769.1k | 787.7k | 0.98 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 103 (97.6) | 59.2 (53.0) | 9.68M | 16.89M | 0.57 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 98.6 (88.3) | 56.1 (52.0) | 10.14M | 17.82M | 0.57 | 0.00 | 0.00 |
| readers8_get | 8000000 | 201 (157) | 167 (142) | 4.97M | 5.99M | 0.83 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 3932 (3716) | 3788 (3546) | 254.3k | 264.0k | 0.96 | 0.00 | 0.00 |
| dup_put | 1000000 | 1057 (1018) | 1361 (1312) | 946.4k | 734.5k | 1.29 | 0.00 | 0.00 |
| dup_get_both | 1000000 | 1312 (1123) | 1352 (1208) | 762.4k | 739.5k | 1.03 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 195 (172) | 153 (140) | 5.14M | 6.56M | 0.78 | 0.20 | 0.00 |
| put_4k | 50000 | 5793 (5080) | 4546 (3993) | 172.6k | 220.0k | 0.78 | 0.00 | 0.03 |
| get_4k | 1000000 | 674 (594) | 767 (678) | 1.48M | 1.30M | 1.14 | 0.00 | 0.00 |
| put_64k | 5000 | 46124 (45801) | 31324 (26303) | 21.7k | 31.9k | 0.68 | 0.00 | 0.23 |
| get_64k | 1000000 | 381 (323) | 379 (362) | 2.62M | 2.64M | 0.99 | 0.00 | 0.00 |
| del_rand | 1000000 | 5191 (4926) | 4115 (3880) | 192.6k | 243.0k | 0.79 | 0.00 | 0.00 |
| txn_nosync | 50000 | 5179 (5024) | 5682 (5506) | 193.1k | 176.0k | 1.10 | 1.00 | 1.04 |
| txn_sync | 1000 | 3806 (3667) | 4568 (4236) | 262.7k | 218.9k | 1.20 | 1.00 | 1.08 |
| mixed_writer | 50000 | 6320 (5612) | 5706 (4870) | 158.2k | 175.2k | 0.90 | 0.01 | 0.03 |
| mixed_readers | 10434560 | 30.5 (27.3) | 14.3 (12.8) | 32.76M | 70.03M | 0.47 | 0.00 | 0.00 |
| ls_scan | 5000000 | 104 (98.0) | 62.1 (56.9) | 9.65M | 16.09M | 0.60 | 0.00 | 0.00 |
| ls_update | 50000 | 5321 (5065) | 4758 (4308) | 187.9k | 210.2k | 0.89 | 0.00 | 0.00 |
| get_raw | 1000000 | 1096 (968) | 1261 (1146) | 912.1k | 792.8k | 1.15 | 0.00 | 0.00 |
| get_copy | 1000000 | 1286 (1070) | 1379 (1223) | 777.8k | 725.2k | 1.07 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1339 (1023) | 1344 (1210) | 746.8k | 743.8k | 1.00 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1508 (1205) | 1540 (1328) | 663.3k | 649.2k | 1.02 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1212 (1040) | 1222 (1107) | 825.2k | 818.1k | 1.01 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1462 (1290) | 1682 (1172) | 684.0k | 594.6k | 1.15 | 1.00 | 1.00 |
| view_get | 100000 | 1678 (1383) | 1591 (1375) | 595.9k | 628.7k | 0.95 | 1.00 | 1.00 |
| txn_get | 100000 | 1658 (1467) | 1661 (1526) | 603.2k | 602.2k | 1.00 | 1.00 | 1.00 |
| renew_get | 100000 | 1290 (1112) | 1343 (1240) | 775.2k | 744.5k | 1.04 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1500 (1169) | 1366 (1237) | 666.8k | 732.3k | 0.91 | 0.00 | 0.00 |
| update_put | 100000 | 18676 (16758) | 20708 (17085) | 53.5k | 48.3k | 1.11 | 1.00 | 1.07 |
| txn_put | 100000 | 18250 (16670) | 18149 (16702) | 54.8k | 55.1k | 0.99 | 1.00 | 1.07 |
