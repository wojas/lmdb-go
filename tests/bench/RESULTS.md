# Benchmark results: the Go implementation against lmdb-go v1

The baseline is the pinned cgo reference in `../cgolmdb`: lmdb-go
v1.9.4's Go code on C LMDB 0.9.36 plus ITS#10615, ITS#10642 and ITS#10643
(the same C code as LMDB 0.9.37), including its cgo call overhead. Until
2026-11-02 it was the released v1.9.4, on C LMDB 0.9.35; the sections that
say so measured that. **Ratios are port/cgo of
the medians: below 1 means the port is faster.** Times are ns per
operation (median of all samples; `lmdb-bench` prints the minimum in
parentheses), allocations are Go heap allocations per operation.

The current numbers come first: an experiment that was not kept (page
references in the write cursors, 2026-11-03), the first measurements on
Linux x86-64 hardware and what changed for them (2026-11-03), then the new reference
on macOS, Linux arm64 and OpenBSD (2026-11-02, preceded by why read
transactions on Linux got slower with Go 1.27), then the final validation
(2026-11-01) and what stage 3 (benchmarks and performance, `PLAN.md`) and
stage 4 measured and changed, all against C 0.9.35.

## Not kept: page references in the write cursors (2026-11-03)

The idea: write cursors hold page numbers and look them up again, where
C's hold page pointers, so let each level of the stack also hold the
page's bytes, valid while a generation shared by the transaction's
cursors is unchanged, and dropped when the transaction ends. It was built
(+333 / -94 lines of engine code) and passed the differential tests
against C, the interop matrix and `-race`. On Linux x86-64 (Intel Xeon
W-2150B, interleaved A/B against the same build without it, port only)
it gained 1-2% on random puts, overwrites and deletes, nothing on small
transactions, and cost 2% on sequential appends; garbage collection was
unchanged. The cursors' lookups were already cheap: the slot hints reach
a dirty page about as fast as a generation check, every put starts from
a fresh cursor (as in C), and all page lookups together are about 11% of
a random put's CPU and 5% of a small transaction's. What remains of the
gap to C on x86-64 is spread over every step (no function outside the
system calls takes more than 2%), not in one structure. The stress test
written for it (`TestCursorStress`, run with `LMDB_TEST_STRESS=1`) was
kept.

## Linux x86-64 on real hardware (2026-11-03)

Until now the port was measured on macOS (Apple M1 Pro) and in a Linux
arm64 VM on the same Mac; x86-64 ran only under emulation, for
correctness. The first run on x86-64 hardware found reads at parity or
faster, but writes and small transactions further behind than on arm64
(random puts 1.20, deletes 1.29, one-put commits 1.39; arm64 Linux 0.93,
0.72, 1.11). This section explains why and what this stage changed.

### How it was measured

* AMD EPYC Milan, 4 vCPUs (2 cores with 2 threads each) of a Hyper-V
  guest, 31 GiB RAM, Linux, 4 KiB pages; environments on
  ext4 (so `txn_sync` measures real flushes). The machine was shared with
  other work: load average 2 to 4 during the runs.
* Go 1.26.10 (a distribution's build, whose default `GOAMD64` is `v2`).
* The tables are `lmdb-bench -n 1000000 -rounds 3 -reps 5 -sync-commits
  1000` runs of both implementations, "before" on `main` (`2ea69b1`),
  "after" on this branch. What the changes did to the port is measured
  with `lmdb-bench -ab` (`-rounds 3 -reps 3`, n = 1 000 000), interleaved
  in one session; two builds of the same code differ by up to about 8%
  per workload here.
* For the cause: CPU profiles (pprof; C functions resolved with a cgo
  traceback that records the interrupted program counter, a tool of the
  investigation that is not committed), user and system time per worker
  process, system call counts (strace), and on macOS the instructions
  retired and cycles that `/usr/bin/time -l` reports.

### Why x86-64 differs from arm64

Not the cgo calls: an empty cgo call costs about 27 ns on this machine
(Go 1.26) and 33 to 39 ns on the M1 (Go 1.27), so the reference's calls
are about 10 ns cheaper here, 0.3% of a random put. Not the system calls
either: both implementations write the same pages with as many calls
(strace of 20 020 one-put commits: 91 054 `pwrite` for cgo, which adds
1 374 `lseek` + `writev` pairs for runs of pages, 92 428 `pwrite` for the
port) and spend the same system time (`small_txn`: 1.50 to 1.56 s each;
`rand_put`, 4 M puts: 6.3 to 6.5 s cgo, 5.8 to 5.9 s port). The
difference is user time: 7.3 to 7.7 s against 10.2 to 10.4 s for the puts,
0.51 to 0.54 s against 1.11 to 1.13 s for the commits.

The port executes more instructions than C for the same work, and the M1
hides them where this machine does not. macOS reports instructions and
cycles per process: for 3 M random puts into a 1 M-key database (one
warm-up and two timed repetitions), cgo retires 24.8 to 25.3 G
instructions in 29.9 to 31.2 G cycles, the port 40.1 to 40.3 G
instructions in 30.1 to 30.8 G cycles: 60% more instructions in the same
cycles (1.31 instructions per cycle against 0.82). At that size a put
waits on cache misses, the same in both (they touch the same pages), and
the M1's wide out-of-order core runs the port's extra instructions in
cycles C spends waiting. The EPYC, each core shared by two vCPUs,
overlaps much less: there they show as the extra user time above. With
everything in cache (20 000 keys, every page dirty; instructions per
operation on macOS, after this stage) a lookup takes 2.2 K instructions
against C's 1.9 K, an overwrite 3.7 K against 2.4 K, a delete and
re-insert 13.0 K against 8.0 K. A one-put commit on a 1 M-key database
costs the port 10.3 us of CPU outside the system calls on this machine,
C 4.7 us (both spend about 17 us in `pwrite` on ext4).

Where the time goes (CPU profiles of 6 M random puts, before this stage,
port against C): the descent and binary searches 9.8 s against 8.3 s,
touching pages (copy, allocation, dirty-list bookkeeping) 5.3 s against
about 2.9 s, inserting and splitting 2.1 s against about 0.9 s, and a copy
of every page a commit writes, 1.2 s, where C hands the pages to `writev`.
It is not the code generation: building with `GOAMD64=v3`, or without
bounds checks in the engine (`-gcflags=-B`, an experiment), moves the
in-cache micro benchmarks by less than their noise. It is the structure
stage 3 named for `del_rand`: write cursors hold page numbers and
dirty-list slot hints instead of C's page pointers, so every access looks
the page up again, positioning copies cursor state between write and read
cursors, and each step keeps its own checks and bookkeeping. Each costs
little; together they come to 1.2 (a lookup) to 1.6 (a delete and
re-insert) times C's instructions in cache, and about twice C's over
whole write workloads (n = 100 000 on macOS, per operation including the
commits: 7.5 K against 3.9 K instructions per random put, 17.8 K against
8.7 K per key deleted and rebuilt).

### Summary

Medians in ns/op and the ratio port/cgo (**below 1: the port is
faster**), "before" from the run of `main`, "after" from the run of this
branch (full tables below). Each ratio comes from one run of both
implementations, interleaved; between the two runs the reference's own
times for single-threaded writes moved by 5 to 7% (the machine was busier
during the second), so compare the ratios, and see the A/B below for what
the port itself gained.

| workload | before cgo | before port | before | after cgo | after port | after |
|---|---:|---:|---:|---:|---:|---:|
| rand_put | 3039 | 3640 | 1.20 | 3221 | 3717 | **1.15** |
| rand_overwrite | 3766 | 4586 | 1.22 | 3966 | 4521 | **1.14** |
| del_rand | 3690 | 4773 | 1.29 | 3939 | 4727 | **1.20** |
| txn_nosync | 7790 | 10795 | 1.39 | 7977 | 10774 | **1.35** |
| txn_sync | 316860 | 330786 | 1.04 | 328011 | 333167 | 1.02 |
| update_put | 20050 | 25908 | 1.29 | 19785 | 25684 | **1.30** |
| txn_put | 19935 | 26452 | 1.33 | 20100 | 26017 | **1.29** |
| ls_update | 5865 | 6348 | 1.08 | 6225 | 6718 | **1.08** |
| put_4k | 6947 | 7508 | 1.08 | 7703 | 8469 | 1.10 |
| renew_get | 1153 | 1271 | 1.10 | 1208 | 1222 | 1.01 |
| cursor_renew_set | 1212 | 1289 | 1.06 | 1183 | 1278 | 1.08 |
| rand_get | 1099 | 1040 | 0.95 | 1106 | 1063 | 0.96 |
| scan_fwd | 106 | 74.1 | 0.70 | 104 | 76.3 | 0.74 |
| readers8_get | 288 | 273 | 0.95 | 448 | 397 | 0.89 |
| ls_scan | 123 | 91.7 | 0.75 | 131 | 96.6 | 0.74 |
| view_empty | 224 | 227 | 1.01 | 226 | 231 | 1.02 |
| view8_get | 872 | 558 | 0.64 | 1059 | 848 | 0.80 |
| seq_put_append | 206 | 325 | 1.58 | 212 | 325 | 1.53 |
| dup_put | 877 | 1903 | 2.17 | 921 | 1929 | 2.10 |
| dup_get_both | 1089 | 1361 | 1.25 | 1124 | 1358 | 1.21 |

`txn_sync` here is a real `fdatasync` on ext4 and the `O_DSYNC` meta-page
write (about 0.33 ms per commit): at parity. The multi-threaded
workloads (`readers8_get`, `view8_get`, `mixed`) depend on what else the
4 vCPUs run; their ratios moved with the reference's times.

### What changed

| commit | change | A/B on x86-64, new/base |
|---|---|---|
| `engine: compare 8-byte key prefixes inline ...` | the binary search of the default (memcmp) order compares the first 8 bytes of both keys as big-endian integers and calls `bytes.Compare` only when they are equal (or a key is shorter): same results, no call and no register spills on most probes | rand_put 0.95, rand_overwrite 0.96, del_rand 0.99; in cache: overwrite 0.88, lookups 0.93 |
| `sys, engine: write commit batches with pwritev on Linux` | runs of contiguous pages go to `pwritev(2)` from an iovec array kept in the write transaction, as C with `MDB_USE_PWRITEV`, instead of being gathered into one buffer for `pwrite`; single pages and the meta page go to `pwrite(2)` on the descriptor. Other systems keep the gather. Same bytes, same order | rand_put 0.97, rand_overwrite 0.98, del_rand 1.01, txn_nosync 1.00 |

Both builds of each A/B are the port (`-impl port`), `-rounds 3 -reps 3`,
n = 1 000 000. Allocations are unchanged (`lmdb/alloc_test.go`,
`internal/sys/pagewrite_test.go` pins the new writer at 0).

Both together, as an A/B of `main` against this branch on x86-64 (the
port in both, `-rounds 3 -reps 3`, n = 1 000 000; median (minimum)
ns/op, below 1: this branch is faster):

| workload | main | this branch | new/base |
|---|---:|---:|---:|
| rand_put | 3845 (3741) | 3691 (3548) | 0.96 |
| rand_overwrite | 4835 (4572) | 4611 (4489) | 0.95 |
| del_rand | 5059 (4988) | 4880 (4779) | 0.96 |
| txn_nosync | 10909 (10449) | 11041 (10138) | 1.01 |
| txn_sync | 328605 (322828) | 335760 (319404) | 1.02 |
| update_put | 26308 (25729) | 25917 (25326) | 0.99 |
| txn_put | 26179 (26084) | 25630 (24971) | 0.98 |
| ls_update | 7006 (6397) | 6802 (6325) | 0.97 |
| rand_get | 1119 (1081) | 1061 (983) | 0.95 |
| get_raw | 1138 (1098) | 1079 (1060) | 0.95 |
| cursor_set | 1188 (1095) | 1138 (1038) | 0.96 |
| scan_fwd | 73.9 (70.2) | 75.9 (71.3) | 1.03 |
| ls_scan | 89.6 (87.8) | 90.6 (89.9) | 1.01 |

The other lookups and read transactions of `goapi` are 0.90 to 0.98,
`view_empty` 1.00 and `view8_get` 1.04 (`view_empty`, a transaction
without a lookup, and the scans run unchanged code).

On macOS the same A/B (`-only rand_put,overwrite,delete,small_txn,read`)
ran on a machine at load 19 to 33 and resolves nothing either way:
medians 0.85 to 0.93 for the writes (minima 0.98 to 1.14), 0.98 to 1.02
for lookups and scans, 1.15 for `readers8_get` (8 goroutines; minimum 127
against 178 ns). Instruction counts, which the load does not change, fall
there: the `read` group retires 13% fewer instructions per repetition
(n = 100 000, 3.10 G against 2.70 G), `rand_overwrite` 7% and `rand_put`
4% fewer. The pwritev change is Linux only.

### Tried and not kept

Measured and reverted, or not shippable:

* A direct-mapped cache of 4096 entries in front of the dirty list's
  binary search, instead of its 16: 3 to 4% fewer instructions on macOS,
  0.99 on x86-64 at n = 1 000 000 (10 000 dirty pages per commit miss
  it).
* Positioning the fresh cursors of the free-list save and of a named DB's
  record touch on their own stack, as `mdb_put`'s cursor is (instead of
  through a read cursor): 650 fewer instructions per one-put commit, 1.00
  and 0.99 on x86-64.
* Copying a touched page whole instead of zeroing its free gap (which
  would also have changed the gap bytes the port writes): slower, 1.02 to
  1.05, as reading the source's cold gap costs more than zeroing it.
* `slices.Sort` and `slices.Reverse` instead of a comparison function for
  the free list's descending sort: 30% faster for 10 000 page numbers,
  below the noise of a whole commit.
* `GOAMD64=v3` builds, and the engine without bounds checks
  (`-gcflags=-B`), as experiments: no change beyond noise in the in-cache
  micro benchmarks.

### What remains, and why

* **Random puts, overwrites and deletes** (1.14 to 1.20 on x86-64): the
  instruction count above. The extra work is spread over the descent
  (pages looked up by number at every level), page touches (allocation
  and dirty-list bookkeeping), node inserts and splits, and the cursor
  bookkeeping around them; no single step is more than a few percent, as
  the attempts above show. Closing the gap takes the restructuring stages
  3 and 4 named: write cursors that hold page references (C's pointers,
  with a generation check against reuse) instead of page numbers and slot
  hints, so that positioning does not copy state into read cursors and a
  page is not looked up again at every step. That rewrites `wcursor.go`,
  `cursorput.go`, `cursordel.go` and `split.go`, the structure the port
  keeps from rlmdb so that C's decisions stay the ones rlmdb tested
  (`internal/engine/PORTING.md`), and is beyond this stage.
* **Small transactions** (`txn_nosync` 1.35, `update_put` and `txn_put`
  1.29 to 1.30): each commit writes the same pages with as many system
  calls as C (on the 1 M-key database about 17 us of `pwrite` per commit
  for both), and the port's CPU time outside the system calls is about
  twice C's (10.3 us against 4.7 us there). The free-list save alone
  (deleting the reclaimed record, touching the free DB, reserving and
  filling the new records) takes 3.7 us of it, nearly as much as all of
  C's work per commit. The same restructuring would address it.
* `ls_update` (1.08) and `put_4k` (1.10) are close to parity, reads at
  parity or faster, as before.
* Sequential appends and DUPSORT puts (1.53 and 2.10, low priority) did
  not change; see stage 4.

## Read transactions in the Linux VM with Go 1.26 and later (2026-11-02)

The new reference's run below found the port's read transactions slower
on Linux with Go 1.27 than with Go 1.25 (`view_empty` 2.26 times as long)
and not on macOS. The cause is how Go 1.26 and later copy structs on
arm64, combined with the VM the Linux numbers come from; the port did not
change and was not changed.

**Reproduced.** Go 1.25.12 against Go 1.27.3 builds of the same port code
in one Linux arm64 container (`lmdb-bench -ab`, `goapi`, `-n 100000
-rounds 3 -reps 3`; Go 1.27 / Go 1.25): `view_empty` 2.27 (177 to 401
ns), `view_get` 1.36, `txn_get` 1.71, `renew_get` 1.83,
`cursor_renew_set` 1.50; lookups, cursor operations and commits 1.03 to
1.12. Go 1.26.2 is as slow as Go 1.27.3: in a Go benchmark of the
`lmdb` package, interleaved, an empty `View` takes 214 to 233 ns with Go
1.25, 422 to 501 with Go 1.26 and 421 to 452 with Go 1.27; a `Reset` plus
`Renew` 136 to 140, 346 to 359 and 353 to 370 ns. No allocation changed.

**Cause: LDP Q.** Go 1.26 compiles copies of more than 64 bytes on arm64
to pairs of 32-byte vector loads and stores (`LDP Q` / `STP Q`, Go's
`FLDPQ` / `FSTPQ`) instead of `duffcopy`; Go 1.27 does the same for copies
of 32 to 64 bytes, which were pairs of 8-byte register loads (`LDP X`).
Beginning or renewing a read transaction copies such structs several
times: the decoded meta page (`format.Meta`, 136 bytes, returned by value
from `ReadMeta`, `readMetaIn` and `snapshotMeta` to `takeSnapshot`) and
the reset of the main and free DB's entries (`txnDB`, 104 bytes, in
`Txn.dbe`); a CPU profile of `Reset` plus `Renew` with Go 1.27 puts a
third of the time on the line of that reset. In the Linux VM one `LDP Q`
costs about 4.7 ns more than on macOS on the same CPU, and successive ones
do not overlap; `LDR Q` and `LDP X` cost the same in both:

| ns per call of a function that does | macOS | Linux VM |
|---|---:|---:|
| one `LDP Q` (32 bytes; assembly) | 2.1 | 6.9 |
| two `LDR Q` (32 bytes; assembly) | 2.1 | 2.2 |
| two `LDP X` (32 bytes; assembly) | 2.1 | 2.1 |
| four `LDP Q` (assembly) | 1.6 to 2.2 | 20 to 25 |
| a copy of a 104-byte struct, Go 1.25 / 1.26 / 1.27 | 3.1 / 2.8 / 3.0 | 3.1 / 21 to 25 / 21 to 22 |
| a copy of a 48-byte struct, Go 1.25 / 1.27 | 1.0 / 1.0 to 1.5 | 1.0 / 7.2 |

A Go 1.27.3 compiler changed to emit two `LDR Q` where it emits one `LDP
Q` for a copy (an experiment only) removes the slowdown: in the same
container, interleaved, an empty `View` takes 185 to 214 ns (stock Go
1.27: 416 to 452, Go 1.25: 202 to 218) and a `Reset` plus `Renew` 125 to
134 ns (352 to 357; 134 to 137).

**Why only Linux: the VM.** The Linux numbers come from a Lima VM on
Apple Virtualization with Rosetta enabled. Its CPUs order memory as x86
does (total store order): a message-passing litmus test (plain stores of
data then flag, plain loads of flag then data) sees flag without data
128389 times in 111189 trials on macOS and never in 282996 trials (three
runs) in the VM. That `LDP Q` is slow in that mode is the likely
explanation; it could not be checked here (the VM's mode was not changed),
nor was native arm64 Linux hardware measured. macOS
runs natively, where Go 1.27's copies are as fast as before (the port's
`view_empty` took 0.83 of its Go 1.25 time).

**Stage 11** (the rlmdb fixes pending on `stage11-rlmdb-sync3`, including
read-only readers pinning `mti_txnid`'s snapshot while it is 0) does not
change it: against `main`, both built with Go 1.27.3 in the Linux VM
(`goapi`, `-n 100000`, 3 and 4 rounds of 3 reps), `view_empty` is 1.03
and 1.01.

## New reference: C 0.9.36 plus fixes (2026-11-02)

The cgo reference changed: `../cgolmdb` is now lmdb-go at `e5af975`, the
Go code of v1.9.4 on C LMDB 0.9.36 plus ITS#10615, ITS#10642 and
ITS#10643 (the same C code as LMDB 0.9.37, and as the C oracles' 0.9). The
port did not change. The sections after this one measured the released
v1.9.4's C code, 0.9.35, with Go 1.25; they stay as history.

### How it was measured

* `cmd/lmdb-bench -n 1000000 -rounds 3 -reps 5 -sync-commits 1000`, as
  in the final validation below, built with Go 1.27.3.
* **macOS**: Apple M1 Pro (8 performance and 2 efficiency cores), 32 GiB
  RAM, macOS 15, 16 KiB pages, environments in `$TMPDIR` (APFS on the
  internal SSD). The machine was shared with other work: one-minute load
  5 to 40 on 10 cores during the full run. `seq_put` and `mixed`, whose
  medians in the full run were far from their minima, and `read`, whose
  `readers8_get` median was far from its minimum too, were run again
  (same options, both implementations) at load 2.5 to 4 and 3 to 21; the
  summary takes those three groups from those runs (`goapi`, also rerun,
  agreed with the full run within 0.05 in every ratio but `txn_put`'s,
  1.10 and 1.34).
* **Linux arm64**: a Lima VM on the same Mac (8 vCPUs, 12 GiB), container
  image `golang:1.27-bookworm` (glibc), 4 KiB pages, environments in the
  container's `/tmp`, a tmpfs (`txn_sync` timings mean nothing there).
  Load average inside the VM 0 to 4.7 (mean 1.9).
* **OpenBSD 7.9 arm64**: a 2-vCPU, 2 GiB VM (Apple Virtualization on an
  Apple silicon Mac), Go 1.26.2, 4 KiB pages, environments in `/tmp` on
  the VM's disk; `-n 100000 -rounds 3 -reps 3 -sync-commits 100`
  (smaller, for the VM's memory). Run twice; the ratios below are the
  first run's, the second agrees within a few percent except where noted.
* What moved, and why, was checked with interleaved A/Bs (`lmdb-bench
  -ab`, `-rounds 2` or `3`, `-reps 3`): the old reference against the new
  one (both built with Go 1.27, `-impl cgo`), and Go 1.25.12 against Go
  1.27.3 builds of this branch (`-impl port` and `-impl cgo`).

### Summary

Medians in ns/op and the ratio port/cgo (**below 1: the port is
faster**); "old" is the ratio of the final validation below (C 0.9.35,
Go 1.25). Allocations per operation are unchanged (full tables).

| workload | macOS cgo | macOS port | macOS port/cgo | old | Linux cgo | Linux port | Linux port/cgo | old |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| ls_scan | 62.3 | 31.0 | 0.50 | 0.35 | 89.0 | 61.8 | 0.69 | 0.60 |
| ls_update | 6332 | 5993 | 0.95 | 1.01 | 5052 | 4508 | 0.89 | 0.92 |
| rand_get | 1202 | 889 | 0.74 | 0.70 | 965 | 1002 | 1.04 | 1.07 |
| readers8_get | 166 | 116 | 0.70 | 0.75 | 145 | 144 | 0.99 | 0.99 |
| scan_fwd | 58.5 | 28.8 | 0.49 | 0.42 | 76.7 | 48.9 | 0.64 | 0.58 |
| scan_bwd | 57.4 | 32.8 | 0.57 | 0.48 | 66.8 | 47.7 | 0.71 | 0.56 |
| rand_put | 2725 | 2763 | 1.01 | 1.01 | 2607 | 2415 | 0.93 | 0.97 |
| rand_overwrite | 4271 | 4072 | 0.95 | 1.00 | 3500 | 3207 | 0.92 | 0.89 |
| del_rand | 3326 | 3503 | 1.05 | 0.99 | 4888 | 3532 | 0.72 | 0.71 |
| txn_nosync | 6833 | 7801 | 1.14 | 1.19 | 5302 | 5907 | 1.11 | 1.09 |
| txn_sync | 10620272 | 10638233 | **1.00** | 1.95 | 3726 | 4765 | 1.28 | 1.17 |
| update_put | 31097 | 34584 | 1.11 | 1.22 | 17387 | 17727 | 1.02 | 1.04 |
| put_64k | 25821 | 16045 | 0.62 | 0.58 | 42146 | 27250 | 0.65 | 0.56 |
| get_64k | 327 | 273 | 0.83 | 0.70 | 311 | 341 | 1.09 | 1.09 |
| put_4k | 6299 | 6518 | 1.03 | 1.03 | 4431 | 3565 | 0.80 | 0.75 |
| mixed_writer | 5172 | 5209 | 1.01 | 1.08 | 5493 | 4909 | 0.89 | 0.93 |
| mixed_readers | 16.1 | 7.6 | 0.47 | 0.34 | 20.5 | 12.2 | 0.59 | 0.50 |
| seq_put_append | 106 | 181 | 1.72 | 1.60 | 135 | 223 | 1.65 | 1.39 |
| dup_put | 902 | 1346 | 1.49 | 1.40 | 986 | 1320 | 1.34 | 1.26 |
| view_get | 2083 | 1036 | 0.50 | 0.44 | 1373 | 1549 | **1.13** | 0.90 |
| view_empty | 807 | 139 | 0.17 | 0.15 | 249 | 399 | **1.60** | 0.63 |
| view8_get | 3288 | 362 | 0.11 | 0.04 | 629 | 457 | 0.73 | 0.66 |
| txn_get | 2152 | 1103 | 0.51 | 0.51 | 1416 | 1649 | **1.16** | 0.87 |
| renew_get | 1323 | 984 | 0.74 | 0.81 | 1113 | 1589 | **1.43** | 1.01 |

### What moved, and why

Most ratios are within the noise of such runs (about 10% per workload
on this machine). Four things moved, three of them not because of the C
version:

* **`txn_sync` on macOS: 1.95 to 1.00.** The new reference, like the
  port and C 0.9.36 and later (ITS#10454), follows the meta-page write
  with `F_FULLFSYNC`: two full flushes per commit, where C 0.9.35 makes
  one. This is the only workload the C version moves: in the A/B of the
  old reference against the new one on macOS (`-impl cgo`, every group
  but `mixed` and `goapi`), `txn_sync` takes 1.95 times as long with the
  new one, and every other workload is within 0.84 to 1.05 (DUPSORT
  puts 1.00, sequential appends 1.00). Against the released lmdb-go
  v1.9.4 the port's synced commits on macOS still take about twice as
  long, which is durability, not overhead.
* **Cursor scans on macOS: 0.42 to 0.49 (`scan_fwd`), `ls_scan` 0.35 to
  0.50, `mixed_readers` 0.34 to 0.47.** The cgo side got faster with Go
  1.27: in the A/B of Go 1.25.12 against Go 1.27.3 builds, the reference
  scans in 0.82 (`scan_fwd`), 0.83 (`scan_bwd`) and 0.85 (`ls_scan`) of
  its Go 1.25 time (cgo calls got cheaper), while the port's times stay
  within 0.92 to 1.04 (`view_empty` 0.83). On Linux the reference's scans
  gained less (0.81 to 0.89), and so did the ratios.
* **Read transactions on Linux: `view_empty` 0.63 to 1.60, `view_get`
  0.90 to 1.13, `txn_get` 0.87 to 1.16, `renew_get` 1.01 to 1.43.** The
  port got slower with Go 1.27 on Linux, and only there: in the A/B of Go
  1.25.12 against Go 1.27.3 builds of the same port code in the same
  container, `view_empty` takes 2.26 times as long with Go 1.27,
  `view_get` 1.38, `cursor_renew_set` 1.34, `txn_get` 1.21, `renew_get`
  1.11, while lookups, scans and commits are within 0.89 to 1.03, and the
  reference's read transactions got faster (0.82 to 0.97). On macOS the
  same A/B shows no such change. The cause was not investigated here.
* **Sequential appends and DUPSORT puts: 1.60 to 1.72 and 1.40 to 1.49
  on macOS, 1.39 to 1.65 and 1.26 to 1.34 on Linux** (low priority).
  The C version does not change them (above); the reference's appends
  got faster (macOS 117 to 106 ns, Linux 150 to 135), the port's stayed
  within their noise.

`txn_sync` on Linux (1.17 to 1.28) runs on a tmpfs and means nothing.
`view8_get` on macOS (0.04 to 0.11) moves with the machine's load: the
reference's median was 9700 ns in the final validation, 3288 here and
2816 to 2994 in the A/Bs, the port's 350 to 362 throughout.
The port itself did not change; its allocation counts are the same.

### OpenBSD

Before this change the reference could not run on OpenBSD: C 0.9.35 has
no ITS#10615, reads stale pages after its own writes there, and the
benchmark failed with `MDB_NOTFOUND` and crashed in `mdb_put`. With the
new reference every group runs (`-n 100000`; port/cgo, below 1: the port
is faster):

| workload | cgo ns/op | port ns/op | port/cgo | second run |
|---|---:|---:|---:|---:|
| ls_scan | 44.7 | 26.6 | 0.59 | 0.58 |
| ls_update | 8914 | 8453 | 0.95 | 0.95 |
| rand_get | 186 | 188 | 1.01 | 1.01 |
| readers8_get | 95.5 | 95.3 | 1.00 | 0.99 |
| scan_fwd | 31.1 | 13.5 | 0.43 | 0.44 |
| rand_put | 1595 | 1541 | 0.97 | 0.97 |
| rand_overwrite | 3371 | 3303 | 0.98 | 0.96 |
| del_rand | 2649 | 2551 | 0.96 | 0.97 |
| txn_nosync | 33981 | 34907 | 1.03 | 1.00 |
| txn_sync | 93168 | 103078 | 1.11 | 0.99 |
| update_put | 60208 | 57818 | 0.96 | 0.97 |
| put_4k | 10547 | 4141 | 0.39 | 0.39 |
| put_64k | 75037 | 50572 | 0.67 | 0.57 |
| get_64k | 93.8 | 94.6 | 1.01 | 1.00 |
| mixed_writer | 21777 | 27337 | 1.26 | 0.61 |
| mixed_readers | 56.5 | 12.8 | 0.23 | 0.67 |
| seq_put_append | 136 | 154 | 1.13 | 1.13 |
| dup_put | 957 | 969 | 1.01 | 0.98 |
| dup_next_dup | 53.7 | 36.8 | 0.68 | 0.69 |
| view_get | 755 | 271 | 0.36 | 0.35 |
| view_empty | 549 | 78.3 | 0.14 | 0.13 |
| txn_get | 785 | 297 | 0.38 | 0.36 |

Reads and lookups are at parity with C there (0.99 to 1.01), scans and
read transactions much faster (0.13 to 0.59), puts, deletes and commits
at parity or faster (0.96 to 1.03 with `txn_sync` 0.99 to 1.11, on the
VM's disk), and, unlike on macOS and Linux, sequential appends (1.13) and
DUPSORT puts (0.98 to 1.01) are close to C. Values of 4 KiB and more are
written much faster (`put_4k` 0.39, `put_64k` 0.43 to 0.67); why was not
measured (both invalidate the written range of the map after each write,
ITS#10615 in C, rlmdb `KNOWN_C_BUGS.md` #39 in the port). `mixed` (the
writer 1.26 and 0.61, the readers 0.23 and 0.67) has too few CPUs for its
4 readers to be meaningful. A first
attempt at this run stopped in its second round when the VM stopped
responding (no network, both vCPUs busy); two later runs, in fresh VMs,
did not repeat it, and its cause is unknown.

### FreeBSD (2026-11-03)

FreeBSD 15.1 arm64 in a 2-vCPU, 2 GiB VM (Apple Virtualization on an
Apple silicon Mac), Go 1.26.9, the base system's clang, 4 KiB pages,
environments in `/tmp` on the VM's disk (UFS); `-n 100000 -rounds 3
-reps 3 -sync-commits 100`, as on OpenBSD; one run. A VM on a shared
host, not a performance claim. The port is this branch's, with the fix
that pooled cursors and transactions drop their pages (`rcursor.forget`,
neutral in an A/B on macOS). Port/cgo, below 1: the port is faster (full
table below):

| workload | cgo ns/op | port ns/op | port/cgo |
|---|---:|---:|---:|
| rand_get | 193 | 186 | 0.96 |
| readers8_get | 102 | 100 | 0.98 |
| scan_fwd | 28.6 | 13.5 | 0.47 |
| ls_scan | 43.8 | 27.8 | 0.63 |
| dup_next_dup | 50.2 | 36.7 | 0.73 |
| view_get | 328 | 304 | 0.93 |
| view_empty | 119 | 116 | 0.97 |
| txn_get | 362 | 314 | 0.87 |
| rand_put | 438 | 559 | 1.28 |
| rand_overwrite | 654 | 696 | 1.06 |
| del_rand | 525 | 699 | 1.33 |
| txn_nosync | 2091 | 2852 | 1.36 |
| txn_sync | 65867 | 66339 | 1.01 |
| update_put | 5367 | 6997 | 1.30 |
| put_4k | 1346 | 1679 | 1.25 |
| put_64k | 16648 | 17064 | 1.02 |
| seq_put_append | 65.4 | 111 | 1.70 |
| dup_put | 304 | 554 | 1.82 |

Reads and lookups are at parity with C (0.90 to 1.04), scans and cursor
walks faster (0.47 to 0.73). Beginning read transactions (`view_*`,
`txn_get`) is close to C (0.78 to 0.97), unlike on OpenBSD (0.14 to
0.44); renewing them is at parity on both (0.99 to 1.08). The
reference's reader lock is a robust mutex in the lock file here, as on
Linux, and a POSIX semaphore on OpenBSD (not measured separately). The
write workloads (1.02 to 1.82, `txn_sync`
1.01 on the VM's disk) compare as on Linux and macOS; on OpenBSD, where
both implementations invalidate the map after each write, they were close
to C. `mixed` has too few CPUs for its 4 readers to be meaningful.

## Final validation (2026-11-01, C 0.9.35)

The code of `main` after stages 4 and 5 (reader slots kept for reuse,
the `NoLock` and lifecycle fixes of the reviews, the sync with rlmdb's
main), measured as in stage 3.

### How it was measured

* `cmd/lmdb-bench -n 1000000 -rounds 3 -reps 5 -sync-commits 1000`:
  every group, both implementations in one process, interleaved per group
  and round (15 timed repetitions per workload and implementation after a
  warm-up each).
* **macOS**: Apple M1 Pro (8 performance and 2 efficiency cores), 32 GiB
  RAM, macOS 15, Go 1.25.4, 16 KiB pages. Environments in `$TMPDIR`, APFS
  on the internal SSD. The machine was shared with other work: load
  average 5 to 76 (one-minute) on 10 cores during the full run, highest in
  its first round. `seq_put` and `mixed`, whose first-round samples were
  the noisiest, were run again (same options, both implementations) at
  load 4.5 to 5; the summary takes those two groups from that run.
* **Linux arm64**: a Lima VM on the same Mac (8 vCPUs, 12 GiB), container
  image `golang:1.25-bookworm` (glibc, Go 1.25.14), 4 KiB pages.
  Environments in the container's `/tmp`, a tmpfs, so `txn_sync` (fsync)
  timings mean nothing there. Load average inside the VM 1.1 to 7.5 (mean
  2.5), much quieter than the stage 3 runs (12 to 40); the macOS A/B
  below ran on the host at the same time.
* "Stage 3" is the end of stage 3 (`b9a806f`). Whether the port itself
  got slower since then is measured with `lmdb-bench -ab` (the stage-3
  build of the port against this one, interleaved), not by comparing
  separate runs.

### Summary

Medians in ns/op, ratio port/cgo of the medians (**below 1: the port is
faster**), and the port's Go heap allocations per operation (lmdb-go v1's
are 0, and 1 for the transaction-per-operation workloads `txn_nosync`,
`txn_sync`, `update_put` and the `View` ones, which allocate the `*Txn`
in both implementations).

| workload | macOS cgo | macOS port | macOS port/cgo | Linux cgo | Linux port | Linux port/cgo | port allocs/op (macOS / Linux) |
|---|---:|---:|---:|---:|---:|---:|---:|
| ls_scan | 89.6 | 31.0 | 0.35 | 94.3 | 57.0 | 0.60 | 0.00 / 0.00 |
| ls_update | 5469 | 5497 | 1.01 | 4755 | 4352 | 0.92 | 0.00 / 0.00 |
| rand_get | 1341 | 938 | 0.70 | 1046 | 1121 | 1.07 | 0.00 / 0.00 |
| readers8_get | 166 | 124 | 0.75 | 156 | 155 | 0.99 | 0.00 / 0.00 |
| scan_fwd | 72.3 | 30.2 | 0.42 | 86.1 | 49.6 | 0.58 | 0.00 / 0.00 |
| scan_bwd | 70.0 | 33.5 | 0.48 | 80.7 | 45.5 | 0.56 | 0.00 / 0.00 |
| rand_put | 2622 | 2656 | 1.01 | 2673 | 2585 | 0.97 | 0.01 / 0.01 |
| rand_overwrite | 4284 | 4271 | 1.00 | 3653 | 3244 | 0.89 | 0.00 / 0.00 |
| del_rand | 3593 | 3540 | 0.99 | 4899 | 3470 | 0.71 | 0.00 / 0.00 |
| txn_nosync | 6973 | 8275 | 1.19 | 4923 | 5350 | 1.09 | 1.01 / 1.04 |
| txn_sync | 5126988 | 9982706 | 1.95 | 3776 | 4434 | 1.17 | 1.04 / 1.08 |
| update_put | 30352 | 37019 | 1.22 | 16485 | 17109 | 1.04 | 1.04 / 1.07 |
| put_64k | 26912 | 15656 | 0.58 | 47517 | 26697 | 0.56 | 0.22 / 0.23 |
| get_64k | 393 | 274 | 0.70 | 297 | 324 | 1.09 | 0.00 / 0.00 |
| put_4k | 6451 | 6669 | 1.03 | 4893 | 3673 | 0.75 | 0.04 / 0.03 |
| mixed_writer | 5570 | 6010 | 1.08 | 5480 | 5078 | 0.93 | 0.02 / 0.03 |
| mixed_readers | 23.7 | 8.2 | 0.34 | 25.0 | 12.6 | 0.50 | 0.00 / 0.00 |
| seq_put_append | 117 | 187 | 1.60 | 150 | 209 | 1.39 | 0.00 / 0.00 |
| dup_put | 856 | 1200 | 1.40 | 1008 | 1266 | 1.26 | 0.00 / 0.00 |
| view_get | 2456 | 1076 | 0.44 | 1418 | 1281 | 0.90 | 1.00 / 1.00 |
| view_empty | 1009 | 149 | 0.15 | 291 | 184 | 0.63 | 1.00 / 1.00 |
| view8_get | 9700 | 356 | 0.04 | 579 | 384 | 0.66 | 1.00 / 1.00 |
| txn_get | 2319 | 1191 | 0.51 | 1454 | 1269 | 0.87 | 1.00 / 1.00 |

Every workload, with minima, is in the full tables below. What the
ratios say has not changed since stages 3 and 4: reads and scans are
faster than v1 (on Linux, where v1's cgo calls cost less, single lookups
are within 10% of v1 either way, 0.84 to 1.09), puts, overwrites and
deletes at parity or faster, one-put commits 4 to 22% slower (the system
calls through the Go runtime, and the writer lock's two cgo calls),
synced commits on macOS twice as slow by design (a second full flush, as
C 0.9.36 and later), sequential appends and DUPSORT puts slower (low
priority, see stage 4), read transactions much cheaper (the slot pool of
stage 4).

### Changes since stage 3: no regression

The port's own times are lower than in stage 3's "after" tables for
every workload on Linux (0.64 to 0.97 of stage 3), and on macOS lower or
within 1% (`update_put`, `ls_scan`) for every workload but two. Where a
ratio rose by more than about 10% (macOS
`readers8_get` 0.62 to 0.75, `dup_get_both` 0.78 to 0.88; Linux
`rand_get` 0.98 to 1.07, `readers8_get` 0.83 to 0.99, `get_64k` 0.99 to
1.09, `dup_next_dup` 0.78 to 0.89), the port's time went down and v1's
went down more: the stage 3 runs were made on a busier machine, which
costs v1's cgo calls more than the port's code.

The two exceptions were the first groups of the macOS full run, at its
highest load: `seq_put_append` (median 340 ns against stage 3's 194,
minimum 185 against 186) and `mixed_writer` (7616 against 6264). The
interleaved A/B of the stage-3 build of the port against this one (macOS,
`-only seq_put,mixed,small_txn -rounds 3 -reps 5`, load 5 to 15; below 1:
this build is faster) shows no change:

| workload | stage 3 ns/op | now ns/op | now/stage 3 |
|---|---:|---:|---:|
| seq_put_append | 194 (188) | 198 (188) | 1.02 |
| mixed_writer | 6272 (5336) | 5801 (5492) | 0.92 |
| mixed_readers | 8.8 (7.9) | 8.0 (7.8) | 0.91 |
| txn_nosync | 8346 (7883) | 8372 (8138) | 1.00 |
| txn_sync | 9967935 (8997361) | 9738150 (8945328) | 0.98 |

and the rerun of both groups at load 5 gives `seq_put_append` 1.60 and
`mixed_writer` 1.08 against v1 (stage 3: 1.64 and 0.70; stage 3's v1
median of `mixed_writer`, 9009 with a minimum of 5572, was the noisy
one). Nothing on the per-put path of a plain put changed since stage 3:
the rlmdb sync added checks per split (a root split that would overflow
the cursor stack, `MDB_CURSOR_FULL`) and in DUPSORT conversions only.

On Linux, the same A/B of the groups whose ratios rose (`-only
read,dupsort,overflow,goapi -rounds 3 -reps 5`, load in the VM 0.8 to
4.1; below 1: this build is faster) shows the port unchanged or faster
there too: `rand_get` 0.96, `readers8_get` 0.88, `get_64k` 0.98,
`dup_next_dup` 0.90, `get_4k` 0.90, `view_get` and `txn_get` 0.85 (the
slot pool). The lookups of `goapi` move both ways between two such runs
of the same two builds, which is their noise in the VM:

| workload | first A/B | second A/B (`-only goapi`, load 3 to 8) |
|---|---:|---:|
| get_raw | 1.00 | 0.94 |
| get_key_string | 1.08 | 0.97 |
| cursor_set | 1.10 | 1.04 |
| txn_get | 0.85 | 1.35 (minimum 1247 against 1304) |
| renew_get | 0.97 | 1.11 |
| cursor_renew_set | 0.99 | 1.06 |
| update_put | 1.03 | 0.99 |
| txn_put | 0.96 | 0.86 |

On the lookup paths (`Get`, `Cursor.Get`) the only change since stage 3
is one comparison per call (a handle checks that its transaction is
still the one it began, the stage 4 review fix for stale handles); a
read transaction now keeps its data map referenced across `Reset`, and
`txn_get` takes its slot from the stage 4 pool. On macOS these
workloads are as fast as in stage 3 or faster (0.83 to 1.01 of stage 3,
`txn_get` 0.59).

## Stage 3 (2026-10-31, C 0.9.35)

### How it was measured

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

### Summary: the priority workloads

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

### What changed

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

### What remains, and why

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
  owner. (Stage 4 did that, with the owner's approval: see below.)
* **GC**: with no steady-state allocations on these paths the collector
  is no longer a factor (context switches per one-put commit on macOS:
  1.16 before, 0.05 after). The remaining allocations in lmdb-bench's
  write groups (`put_64k` 0.22, `put_4k` 0.04 allocs/op) are the first
  transaction of the fresh environment each repetition opens.

## Stage 4: reader slots kept for reuse

Measured as above (`lmdb-bench`, interleaved, median (minimum) ns/op), at
`-n 500000 -rounds 3 -reps 5`. "Before" is the start of stage 4 (`main`
with the two new `goapi` workloads, below), "after" the end of branch
`stage4-slots`, review fixes included. Each column pair comes from one
run of both sides. The machine was loaded more heavily than in stage 3
(load average 22 to 92 on 10 cores during the macOS runs), so medians
are noisy; the effects below are many times the noise, and the minima
agree.

**What changed.** A read transaction that ends no longer gives its
reader slot back: the slot stays owned by the process and idle
(`mr_txnid = (txnid_t)-1`, what C leaves behind in its default TLS mode)
in a bounded pool of the `Env`, and the next read transaction takes it
without the shared reader mutex and without a cgo call
(`internal/engine/slotpool.go`, with the correctness argument; ledgers in
`internal/engine/PORTING.md` and `lmdb/DEVIATIONS.md`). Before, every
`Env.View` (lmdb-go begins a new transaction per `View`) locked and
unlocked the reader mutex through cgo (on macOS a semaphore system call
each way), and all readers of the process queued on it. Two `goapi`
workloads were added to see it: `view_empty` (a `View` that does
nothing) and `view8_get` (`view_get` on 8 goroutines at once, wall time
per `View`).

Port before and after (`lmdb-bench -ab`, macOS, interleaved; below 1:
after is faster):

| workload | before | after | after/before |
|---|---:|---:|---:|
| view_empty | 1513 (976) | 164 (150) | 0.11 |
| view_get | 3098 (2051) | 1132 (994) | 0.37 |
| txn_get | 3611 (2665) | 1377 (1070) | 0.38 |
| view8_get | 15259 (13042) | 445 (359) | 0.03 |
| renew_get, cursor_renew_set, gets, scans, readers8_get | | | 0.71-1.28, noise both ways (unchanged code) |
| update_put, txn_put | | | 0.95, 1.03 |

Ratios port/cgo (**below 1: the port is faster**), each from a run of
both implementations with the build named:

| workload | macOS before | macOS after | Linux before | Linux after |
|---|---:|---:|---:|---:|
| view_empty | 1.02 | **0.16** | 1.16 | **0.63** |
| view_get | 0.81 | **0.51** | 0.98 | **0.82** |
| txn_get | 0.87 | **0.54** | 1.26 | **0.89** |
| view8_get | 1.43 | **0.03** | 2.41 | **0.63** |
| renew_get | 0.82 | 0.80 | 1.34 | 0.97 |

On macOS after: `view_empty` 967 (931) ns with cgo, 158 (150) with the
port; `view8_get` 15475 (4718) against 406 (337). On Linux (container,
load average 12 to 18), where v1's cgo calls cost less, port before and
after (`-ab`): `view_empty` 334 (318) -> 179 (168), `view_get` 0.88,
`txn_get` 0.85, `view8_get` 1548 (1134) -> 625 (493); everything else
0.92-1.14 (unchanged code). `renew_get` on Linux moved between the two
cgo runs (1.34, 0.97) with unchanged code on both sides: noise of the
shared VM. Allocations are
unchanged (one per `View`, the `*Txn`, as in v1; `lmdb/alloc_test.go`).
The pool's cost: its cells are a compare-and-swap each way, and other
processes see up to `min(MaxReaders/4, 64)` idle slots of this process
in the reader table (`mdb_stat -r`, txnid `-`). Concurrent `View`s on 8
goroutines now reach 2.5 to 3 times one goroutine's rate (before: less
than one goroutine's, all queued on the reader mutex); the next shared
word they all write is the data map's reference count (a profile shows
it next to the pool's cells; padding the cells to a cache line each
moved the time to the count and was not kept).

The review fixes that came with this branch (meta pages read through a
referenced map, NoLock snapshots with rlmdb's in-process `mti_txnid`, the
environment kept alive until its last transaction ends) do the same
atomic operations on the read path as before, in another order; a write
transaction takes one more map reference. (The runs above predate the
NoLock change, which touches only environments without a lock file;
the benchmarks use a lock file.)

### Sequential append and DUPSORT puts (time-boxed, nothing kept)

Profiled on macOS (`go test ./bench -bench 'SeqPut|Dupsort'
-cpuprofile`, `-lmdb.n 1000000`) to look for cheap wins; none was found
that would measurably change these workloads without restructuring, so
nothing changed:

* **`seq_put_append`** (1.64 macOS / 1.39 Linux in stage 3): system time
  is the same as v1's (0.37 s for 10 runs of 1 M puts, both), the port
  spends about 70 ns more user time per put. pprof attributes half the
  samples to `pwrite`, but the system times say that is sampling bias:
  the difference is in the put path (about 68 ns per put in the
  profile), spread thinly: the descent to the last
  leaf (`seekLast`, 22 ns: C also searches from the root for every
  `mdb_put` with `MDB_APPEND`), the node insert (`insertLeafNode`,
  19 ns), and per-put bookkeeping of the write-cursor structure (opening
  and closing the put's cursor in the arena, dirty-list lookups behind
  page numbers and slot hints, the cursor fix-up loop). Commit's own user
  time is small (it writes the same pages as C, in the same batches).
* **`dup_put`** (1.38 / 1.29): the profile is dominated by the first
  read of a freshly mapped page on the way down the tree (one 16-bit
  load holding about a third of the samples), which `rand_put`, at
  parity with C, shows in the same way, and whose page faults C pays
  equally (minor faults counted for the whole group: 52 210 cgo,
  52 816 port). The rest is spread over the same write-cursor
  bookkeeping, plus the read-path copy of the cursor state that a
  DUPSORT put's positioning goes through.

What would close the gap is the refactor stage 3 already named for
`del_rand`: write cursors that hold page references (C's pointers, with
a generation check against reuse) instead of page numbers plus
dirty-list slot hints, so that a put neither looks pages up again nor
copies cursor state into a read cursor to position itself; and, on top
of it, a transaction-level append cursor kept across `mdb_put` calls
(invalidated by any other change to the tree), which C does not have.
Both are large changes to `wcursor.go`, `cursorput.go`, `split.go` and
`cursordel.go`, against the low priority of these workloads.

## Full tables

Each table is one `lmdb-bench -rounds 3 -reps 5` run (n = 1 000 000;
OpenBSD and FreeBSD: `-rounds 3 -reps 3`, n = 100 000) of both implementations, as printed: ns/op median (minimum), ops/s from the
median, ratio port/cgo of the medians (below 1: the port is faster), Go
heap allocations per operation (median over the repetitions).

### Linux x86-64 (EPYC Milan VM, ext4), before

* linux amd64, CPU: AMD EPYC-Milan Processor, 4 cores, 31 GiB RAM
* OS page size (= LMDB page size): 4096
* Go: go1.26.10 (a distribution's build)
* cgo: lmdb-go v1.9.4's Go code at e5af975 (cgo, LMDB 0.9.36: (Aug 6, 2026), plus ITS#10615, ITS#10642, ITS#10643)
* port: the Go implementation (LMDB 0.9.90: (lmdb-go, Go port of rlmdb))

n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 206 (199) | 325 (311) | 4.86M | 3.08M | 1.58 | 0.00 | 0.00 |
| rand_put | 1000000 | 3039 (2827) | 3640 (3391) | 329.1k | 274.8k | 1.20 | 0.00 | 0.01 |
| rand_get | 1000000 | 1099 (958) | 1040 (930) | 910.0k | 962.0k | 0.95 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 106 (98.9) | 74.1 (67.6) | 9.44M | 13.49M | 0.70 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 92.6 (85.6) | 64.0 (59.7) | 10.80M | 15.63M | 0.69 | 0.00 | 0.00 |
| readers8_get | 8000000 | 288 (256) | 273 (238) | 3.47M | 3.66M | 0.95 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 3766 (3480) | 4586 (4173) | 265.5k | 218.1k | 1.22 | 0.00 | 0.00 |
| dup_put | 1000000 | 877 (838) | 1903 (1867) | 1.14M | 525.6k | 2.17 | 0.00 | 0.00 |
| dup_get_both | 1000000 | 1089 (987) | 1361 (1264) | 918.4k | 734.7k | 1.25 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 212 (200) | 197 (188) | 4.72M | 5.08M | 0.93 | 0.20 | 0.00 |
| put_4k | 50000 | 6947 (6513) | 7508 (7146) | 143.9k | 133.2k | 1.08 | 0.00 | 0.03 |
| get_4k | 1000000 | 539 (500) | 535 (520) | 1.86M | 1.87M | 0.99 | 0.00 | 0.00 |
| put_64k | 5000 | 67307 (61366) | 56984 (53164) | 14.9k | 17.5k | 0.85 | 0.00 | 0.23 |
| get_64k | 1000000 | 279 (275) | 281 (272) | 3.58M | 3.56M | 1.01 | 0.00 | 0.00 |
| del_rand | 1000000 | 3690 (3578) | 4773 (4605) | 271.0k | 209.5k | 1.29 | 0.00 | 0.00 |
| txn_nosync | 50000 | 7790 (7536) | 10795 (10549) | 128.4k | 92.6k | 1.39 | 1.00 | 1.04 |
| txn_sync | 1000 | 316860 (282708) | 330786 (305004) | 3.2k | 3.0k | 1.04 | 1.00 | 1.08 |
| mixed_writer | 50000 | 14261 (11511) | 10294 (8736) | 70.1k | 97.1k | 0.72 | 0.01 | 0.03 |
| mixed_readers | 22112256 | 31.1 (29.6) | 22.0 (20.9) | 32.15M | 45.54M | 0.71 | 0.00 | 0.00 |
| ls_scan | 5000000 | 123 (118) | 91.7 (85.5) | 8.15M | 10.91M | 0.75 | 0.00 | 0.00 |
| ls_update | 50000 | 5865 (5481) | 6348 (6048) | 170.5k | 157.5k | 1.08 | 0.00 | 0.00 |
| get_raw | 1000000 | 1066 (1036) | 1129 (1064) | 938.2k | 885.7k | 1.06 | 0.00 | 0.00 |
| get_copy | 1000000 | 1189 (1156) | 1247 (1202) | 840.9k | 801.9k | 1.05 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1093 (1057) | 1095 (1025) | 915.3k | 912.8k | 1.00 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1272 (1199) | 1282 (1208) | 786.3k | 780.2k | 1.01 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1137 (1092) | 1152 (1119) | 879.6k | 868.0k | 1.01 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1284 (1245) | 1198 (1118) | 779.0k | 834.8k | 0.93 | 1.00 | 1.00 |
| view_get | 100000 | 1384 (1305) | 1452 (1301) | 722.7k | 688.5k | 1.05 | 1.00 | 1.00 |
| view_empty | 100000 | 224 (216) | 227 (219) | 4.46M | 4.41M | 1.01 | 1.00 | 1.00 |
| view8_get | 800000 | 872 (761) | 558 (525) | 1.15M | 1.79M | 0.64 | 1.00 | 1.00 |
| txn_get | 100000 | 1511 (1432) | 1482 (1375) | 661.9k | 674.6k | 0.98 | 1.00 | 1.00 |
| renew_get | 100000 | 1153 (1073) | 1271 (1217) | 867.3k | 787.0k | 1.10 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1212 (1150) | 1289 (1215) | 825.3k | 775.8k | 1.06 | 0.00 | 0.00 |
| update_put | 100000 | 20050 (19184) | 25908 (25407) | 49.9k | 38.6k | 1.29 | 1.00 | 1.07 |
| txn_put | 100000 | 19935 (19529) | 26452 (25665) | 50.2k | 37.8k | 1.33 | 1.00 | 1.07 |

### Linux x86-64 (EPYC Milan VM, ext4), after

* linux amd64, CPU: AMD EPYC-Milan Processor, 4 cores, 31 GiB RAM
* OS page size (= LMDB page size): 4096
* Go: go1.26.10 (a distribution's build)
* cgo: lmdb-go v1.9.4's Go code at e5af975 (cgo, LMDB 0.9.36: (Aug 6, 2026), plus ITS#10615, ITS#10642, ITS#10643)
* port: the Go implementation (LMDB 0.9.90: (lmdb-go, Go port of rlmdb))

n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 212 (203) | 325 (308) | 4.71M | 3.08M | 1.53 | 0.00 | 0.00 |
| rand_put | 1000000 | 3221 (3076) | 3717 (3470) | 310.5k | 269.0k | 1.15 | 0.00 | 0.01 |
| rand_get | 1000000 | 1106 (1046) | 1063 (1018) | 904.4k | 940.7k | 0.96 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 104 (99.2) | 76.3 (69.1) | 9.65M | 13.10M | 0.74 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 95.4 (86.9) | 67.7 (62.6) | 10.48M | 14.76M | 0.71 | 0.00 | 0.00 |
| readers8_get | 8000000 | 448 (433) | 397 (378) | 2.23M | 2.52M | 0.89 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 3966 (3836) | 4521 (4429) | 252.1k | 221.2k | 1.14 | 0.00 | 0.00 |
| dup_put | 1000000 | 921 (855) | 1929 (1870) | 1.09M | 518.3k | 2.10 | 0.00 | 0.00 |
| dup_get_both | 1000000 | 1124 (1044) | 1358 (1236) | 889.8k | 736.2k | 1.21 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 218 (205) | 195 (182) | 4.59M | 5.13M | 0.89 | 0.20 | 0.00 |
| put_4k | 50000 | 7703 (7458) | 8469 (7792) | 129.8k | 118.1k | 1.10 | 0.00 | 0.03 |
| get_4k | 1000000 | 578 (540) | 562 (516) | 1.73M | 1.78M | 0.97 | 0.00 | 0.00 |
| put_64k | 5000 | 73772 (69686) | 62013 (57491) | 13.6k | 16.1k | 0.84 | 0.00 | 0.23 |
| get_64k | 1000000 | 290 (282) | 260 (245) | 3.45M | 3.85M | 0.90 | 0.00 | 0.00 |
| del_rand | 1000000 | 3939 (3758) | 4727 (4632) | 253.9k | 211.5k | 1.20 | 0.00 | 0.00 |
| txn_nosync | 50000 | 7977 (7797) | 10774 (10314) | 125.4k | 92.8k | 1.35 | 1.00 | 1.04 |
| txn_sync | 1000 | 328011 (316041) | 333167 (304954) | 3.0k | 3.0k | 1.02 | 1.00 | 1.07 |
| mixed_writer | 50000 | 17761 (13714) | 14737 (11882) | 56.3k | 67.9k | 0.83 | 0.01 | 0.03 |
| mixed_readers | 17583104 | 49.5 (47.0) | 33.5 (31.7) | 20.18M | 29.90M | 0.68 | 0.00 | 0.00 |
| ls_scan | 5000000 | 131 (122) | 96.6 (92.5) | 7.64M | 10.36M | 0.74 | 0.00 | 0.00 |
| ls_update | 50000 | 6225 (5879) | 6718 (6279) | 160.7k | 148.8k | 1.08 | 0.00 | 0.00 |
| get_raw | 1000000 | 1087 (1069) | 1058 (985) | 919.8k | 945.3k | 0.97 | 0.00 | 0.00 |
| get_copy | 1000000 | 1225 (1170) | 1221 (1157) | 816.5k | 819.3k | 1.00 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1145 (1101) | 1086 (1017) | 873.7k | 920.9k | 0.95 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1287 (1223) | 1306 (1213) | 777.3k | 765.9k | 1.01 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1102 (1067) | 1136 (1020) | 907.7k | 880.5k | 1.03 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1295 (1216) | 1127 (1073) | 772.2k | 887.2k | 0.87 | 1.00 | 1.00 |
| view_get | 100000 | 1415 (1306) | 1327 (1262) | 706.8k | 753.3k | 0.94 | 1.00 | 1.00 |
| view_empty | 100000 | 226 (219) | 231 (223) | 4.42M | 4.32M | 1.02 | 1.00 | 1.00 |
| view8_get | 800000 | 1059 (987) | 848 (785) | 944.3k | 1.18M | 0.80 | 1.00 | 1.00 |
| txn_get | 100000 | 1514 (1428) | 1433 (1307) | 660.6k | 697.9k | 0.95 | 1.00 | 1.00 |
| renew_get | 100000 | 1208 (1108) | 1222 (1137) | 827.9k | 818.1k | 1.01 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1183 (1123) | 1278 (1146) | 845.3k | 782.5k | 1.08 | 0.00 | 0.00 |
| update_put | 100000 | 19785 (19477) | 25684 (24887) | 50.5k | 38.9k | 1.30 | 1.00 | 1.07 |
| txn_put | 100000 | 20100 (19629) | 26017 (25397) | 49.8k | 38.4k | 1.29 | 1.00 | 1.07 |

### New reference: macOS

* macOS 15, arm64, CPU: Apple M1 Pro, 10 cores, 32 GiB RAM
* OS page size (= LMDB page size): 16384
* Go: go1.27.3
* cgo: lmdb-go v1.9.4's Go code at e5af975 (cgo, LMDB 0.9.36: (Aug 6, 2026), plus ITS#10615, ITS#10642, ITS#10643)
* port: the Go implementation (LMDB 0.9.90: (lmdb-go, Go port of rlmdb))

n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 108 (105) | 201 (183) | 9.28M | 4.98M | 1.86 | 0.00 | 0.00 |
| rand_put | 1000000 | 2725 (2518) | 2763 (2621) | 367.0k | 361.9k | 1.01 | 0.00 | 0.01 |
| rand_get | 1000000 | 1292 (1138) | 947 (859) | 773.9k | 1.06M | 0.73 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 63.5 (58.6) | 30.4 (29.8) | 15.74M | 32.85M | 0.48 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 57.9 (56.3) | 34.6 (32.8) | 17.26M | 28.93M | 0.60 | 0.00 | 0.00 |
| readers8_get | 8000000 | 276 (151) | 145 (121) | 3.62M | 6.89M | 0.53 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 4271 (3703) | 4072 (3768) | 234.2k | 245.6k | 0.95 | 0.00 | 0.00 |
| dup_put | 1000000 | 902 (787) | 1346 (1183) | 1.11M | 743.2k | 1.49 | 0.00 | 0.00 |
| dup_get_both | 1000000 | 1145 (1004) | 978 (848) | 873.6k | 1.02M | 0.85 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 197 (171) | 118 (115) | 5.06M | 8.48M | 0.60 | 0.20 | 0.00 |
| put_4k | 50000 | 6299 (5930) | 6518 (6357) | 158.7k | 153.4k | 1.03 | 0.00 | 0.04 |
| get_4k | 1000000 | 644 (575) | 604 (516) | 1.55M | 1.66M | 0.94 | 0.00 | 0.00 |
| put_64k | 5000 | 25821 (24441) | 16045 (14460) | 38.7k | 62.3k | 0.62 | 0.00 | 0.22 |
| get_64k | 1000000 | 327 (304) | 273 (264) | 3.06M | 3.67M | 0.83 | 0.00 | 0.00 |
| del_rand | 1000000 | 3326 (3106) | 3503 (3249) | 300.6k | 285.4k | 1.05 | 0.00 | 0.00 |
| txn_nosync | 50000 | 6833 (6457) | 7801 (7412) | 146.3k | 128.2k | 1.14 | 1.00 | 1.01 |
| txn_sync | 1000 | 10620272 (9746987) | 10638233 (9799242) | 94 | 94 | 1.00 | 1.00 | 1.04 |
| mixed_writer | 50000 | 5892 (5302) | 9899 (5363) | 169.7k | 101.0k | 1.68 | 0.01 | 0.02 |
| mixed_readers | 17801216 | 17.7 (15.1) | 10.6 (7.5) | 56.59M | 94.74M | 0.60 | 0.00 | 0.00 |
| ls_scan | 5000000 | 62.3 (59.8) | 31.0 (30.1) | 16.06M | 32.29M | 0.50 | 0.00 | 0.00 |
| ls_update | 50000 | 6332 (5679) | 5993 (5248) | 157.9k | 166.9k | 0.95 | 0.00 | 0.00 |
| get_raw | 1000000 | 1311 (1040) | 917 (799) | 762.9k | 1.09M | 0.70 | 0.00 | 0.00 |
| get_copy | 1000000 | 1347 (1140) | 937 (836) | 742.6k | 1.07M | 0.70 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1239 (1137) | 873 (802) | 807.0k | 1.15M | 0.70 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1292 (1170) | 990 (907) | 773.9k | 1.01M | 0.77 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1252 (1135) | 952 (900) | 798.7k | 1.05M | 0.76 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1496 (1335) | 915 (829) | 668.5k | 1.09M | 0.61 | 1.00 | 1.00 |
| view_get | 100000 | 2083 (1928) | 1036 (974) | 480.1k | 965.3k | 0.50 | 1.00 | 1.00 |
| view_empty | 100000 | 807 (792) | 139 (136) | 1.24M | 7.18M | 0.17 | 1.00 | 1.00 |
| view8_get | 800000 | 3288 (2570) | 362 (307) | 304.2k | 2.76M | 0.11 | 1.00 | 1.00 |
| txn_get | 100000 | 2152 (1940) | 1103 (996) | 464.6k | 906.7k | 0.51 | 1.00 | 1.00 |
| renew_get | 100000 | 1323 (1201) | 984 (961) | 756.0k | 1.02M | 0.74 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1251 (1127) | 1058 (950) | 799.0k | 944.9k | 0.85 | 0.00 | 0.00 |
| update_put | 100000 | 31097 (26008) | 34584 (29739) | 32.2k | 28.9k | 1.11 | 1.00 | 1.04 |
| txn_put | 100000 | 32619 (26868) | 36009 (29455) | 30.7k | 27.8k | 1.10 | 1.00 | 1.04 |


### New reference: Linux arm64 (container, tmpfs)

* linux arm64, CPU: Apple (model not reported), 8 cores, 12 GiB RAM
* OS page size (= LMDB page size): 4096
* Go: go1.27.3
* cgo: lmdb-go v1.9.4's Go code at e5af975 (cgo, LMDB 0.9.36: (Aug 6, 2026), plus ITS#10615, ITS#10642, ITS#10643)
* port: the Go implementation (LMDB 0.9.90: (lmdb-go, Go port of rlmdb))

n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 135 (133) | 223 (213) | 7.42M | 4.48M | 1.65 | 0.00 | 0.00 |
| rand_put | 1000000 | 2607 (2375) | 2415 (2177) | 383.5k | 414.0k | 0.93 | 0.00 | 0.01 |
| rand_get | 1000000 | 965 (849) | 1002 (841) | 1.04M | 997.5k | 1.04 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 76.7 (70.9) | 48.9 (46.9) | 13.04M | 20.45M | 0.64 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 66.8 (64.9) | 47.7 (42.7) | 14.98M | 20.97M | 0.71 | 0.00 | 0.00 |
| readers8_get | 8000000 | 145 (126) | 144 (114) | 6.88M | 6.95M | 0.99 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 3500 (3334) | 3207 (2948) | 285.7k | 311.8k | 0.92 | 0.00 | 0.00 |
| dup_put | 1000000 | 986 (913) | 1320 (1220) | 1.01M | 757.7k | 1.34 | 0.00 | 0.00 |
| dup_get_both | 1000000 | 1050 (864) | 1036 (894) | 952.5k | 965.1k | 0.99 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 160 (148) | 135 (118) | 6.24M | 7.38M | 0.84 | 0.20 | 0.00 |
| put_4k | 50000 | 4431 (4225) | 3565 (2899) | 225.7k | 280.5k | 0.80 | 0.00 | 0.03 |
| get_4k | 1000000 | 557 (450) | 595 (431) | 1.80M | 1.68M | 1.07 | 0.00 | 0.00 |
| put_64k | 5000 | 42146 (36887) | 27250 (20532) | 23.7k | 36.7k | 0.65 | 0.00 | 0.23 |
| get_64k | 1000000 | 311 (279) | 341 (311) | 3.21M | 2.94M | 1.09 | 0.00 | 0.00 |
| del_rand | 1000000 | 4888 (4614) | 3532 (3240) | 204.6k | 283.1k | 0.72 | 0.00 | 0.00 |
| txn_nosync | 50000 | 5302 (4966) | 5907 (5410) | 188.6k | 169.3k | 1.11 | 1.00 | 1.04 |
| txn_sync | 1000 | 3726 (3571) | 4765 (4545) | 268.4k | 209.9k | 1.28 | 1.00 | 1.08 |
| mixed_writer | 50000 | 5493 (5003) | 4909 (4773) | 182.1k | 203.7k | 0.89 | 0.01 | 0.03 |
| mixed_readers | 13704192 | 20.5 (18.7) | 12.2 (11.8) | 48.77M | 82.20M | 0.59 | 0.00 | 0.00 |
| ls_scan | 5000000 | 89.0 (82.2) | 61.8 (58.9) | 11.23M | 16.19M | 0.69 | 0.00 | 0.00 |
| ls_update | 50000 | 5052 (4929) | 4508 (4306) | 197.9k | 221.8k | 0.89 | 0.00 | 0.00 |
| get_raw | 1000000 | 1066 (858) | 1111 (981) | 937.9k | 899.8k | 1.04 | 0.00 | 0.00 |
| get_copy | 1000000 | 1172 (936) | 1114 (936) | 853.3k | 897.4k | 0.95 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1062 (881) | 1063 (851) | 942.0k | 940.4k | 1.00 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1271 (1018) | 1295 (1001) | 786.8k | 772.3k | 1.02 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1088 (956) | 1019 (933) | 919.0k | 981.1k | 0.94 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1229 (1098) | 1177 (1030) | 814.0k | 849.4k | 0.96 | 1.00 | 1.00 |
| view_get | 100000 | 1373 (1277) | 1549 (1475) | 728.4k | 645.7k | 1.13 | 1.00 | 1.00 |
| view_empty | 100000 | 249 (238) | 399 (387) | 4.02M | 2.51M | 1.60 | 1.00 | 1.00 |
| view8_get | 800000 | 629 (527) | 457 (363) | 1.59M | 2.19M | 0.73 | 1.00 | 1.00 |
| txn_get | 100000 | 1416 (1198) | 1649 (1391) | 706.0k | 606.3k | 1.16 | 1.00 | 1.00 |
| renew_get | 100000 | 1113 (916) | 1589 (1241) | 898.4k | 629.3k | 1.43 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1115 (967) | 1588 (1255) | 896.7k | 629.7k | 1.42 | 0.00 | 0.00 |
| update_put | 100000 | 17387 (14576) | 17727 (16792) | 57.5k | 56.4k | 1.02 | 1.00 | 1.07 |
| txn_put | 100000 | 17147 (15728) | 17421 (15412) | 58.3k | 57.4k | 1.02 | 1.00 | 1.07 |


### New reference: OpenBSD 7.9 arm64 (VM, n = 100 000)

* openbsd arm64, CPU: (model not reported), 2 cores, ? GiB RAM
* OS page size (= LMDB page size): 4096
* Go: go1.26.2
* cgo: lmdb-go v1.9.4's Go code at e5af975 (cgo, LMDB 0.9.36: (Aug 6, 2026), plus ITS#10615, ITS#10642, ITS#10643)
* port: the Go implementation (LMDB 0.9.90: (lmdb-go, Go port of rlmdb))

n = 100000, 3 round(s) of 3 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 100000 | 136 (135) | 154 (147) | 7.36M | 6.51M | 1.13 | 0.00 | 0.00 |
| rand_put | 100000 | 1595 (1540) | 1541 (1501) | 626.8k | 648.8k | 0.97 | 0.00 | 0.04 |
| rand_get | 100000 | 186 (185) | 188 (185) | 5.38M | 5.32M | 1.01 | 0.00 | 0.00 |
| scan_fwd | 100000 | 31.1 (30.4) | 13.5 (13.1) | 32.18M | 74.07M | 0.43 | 0.00 | 0.00 |
| scan_bwd | 100000 | 30.3 (29.8) | 14.7 (14.5) | 32.99M | 68.06M | 0.48 | 0.00 | 0.00 |
| readers8_get | 800000 | 95.5 (95.2) | 95.3 (94.8) | 10.47M | 10.50M | 1.00 | 0.00 | 0.00 |
| rand_overwrite | 100000 | 3371 (3313) | 3303 (3178) | 296.7k | 302.7k | 0.98 | 0.00 | 0.00 |
| dup_put | 100000 | 957 (940) | 969 (942) | 1.05M | 1.03M | 1.01 | 0.00 | 0.02 |
| dup_get_both | 100000 | 242 (240) | 232 (232) | 4.13M | 4.31M | 0.96 | 0.00 | 0.00 |
| dup_next_dup | 100000 | 53.7 (52.5) | 36.8 (36.4) | 18.62M | 27.21M | 0.68 | 0.20 | 0.00 |
| put_4k | 5000 | 10547 (9873) | 4141 (3924) | 94.8k | 241.5k | 0.39 | 0.00 | 0.23 |
| get_4k | 100000 | 141 (139) | 143 (142) | 7.10M | 7.01M | 1.01 | 0.00 | 0.00 |
| put_64k | 500 | 75037 (69260) | 50572 (29527) | 13.3k | 19.8k | 0.67 | 0.00 | 1.18 |
| get_64k | 100000 | 93.8 (92.9) | 94.6 (94.4) | 10.66M | 10.57M | 1.01 | 0.00 | 0.00 |
| del_rand | 100000 | 2649 (2601) | 2551 (2442) | 377.5k | 392.0k | 0.96 | 0.00 | 0.00 |
| txn_nosync | 5000 | 33981 (32674) | 34907 (33291) | 29.4k | 28.6k | 1.03 | 1.00 | 1.05 |
| txn_sync | 100 | 93168 (90395) | 103078 (90371) | 10.7k | 9.7k | 1.11 | 1.00 | 1.36 |
| mixed_writer | 5000 | 21777 (9846) | 27337 (16914) | 45.9k | 36.6k | 1.26 | 0.03 | 0.06 |
| mixed_readers | 1926144 | 56.5 (19.0) | 12.8 (7.9) | 17.69M | 77.94M | 0.23 | 0.00 | 0.00 |
| ls_scan | 500000 | 44.7 (43.8) | 26.6 (25.5) | 22.35M | 37.60M | 0.59 | 0.00 | 0.00 |
| ls_update | 5000 | 8914 (8526) | 8453 (8218) | 112.2k | 118.3k | 0.95 | 0.00 | 0.00 |
| get_raw | 100000 | 187 (185) | 188 (185) | 5.34M | 5.33M | 1.00 | 0.00 | 0.00 |
| get_copy | 100000 | 216 (203) | 246 (200) | 4.62M | 4.06M | 1.14 | 1.00 | 1.00 |
| get_key_local | 100000 | 193 (189) | 195 (194) | 5.19M | 5.12M | 1.01 | 1.00 | 1.00 |
| get_key_string | 100000 | 206 (197) | 203 (202) | 4.86M | 4.92M | 0.99 | 1.00 | 1.00 |
| cursor_set | 100000 | 196 (194) | 203 (201) | 5.09M | 4.92M | 1.04 | 0.00 | 0.00 |
| cursor_open_set | 100000 | 338 (331) | 215 (214) | 2.96M | 4.65M | 0.63 | 1.00 | 1.00 |
| view_get | 10000 | 755 (736) | 271 (267) | 1.32M | 3.69M | 0.36 | 1.00 | 1.00 |
| view_empty | 10000 | 549 (538) | 78.3 (77.8) | 1.82M | 12.77M | 0.14 | 1.00 | 1.00 |
| view8_get | 80000 | 497 (489) | 219 (217) | 2.01M | 4.57M | 0.44 | 1.00 | 1.00 |
| txn_get | 10000 | 785 (774) | 297 (290) | 1.27M | 3.37M | 0.38 | 1.00 | 1.00 |
| renew_get | 10000 | 220 (215) | 238 (236) | 4.55M | 4.20M | 1.08 | 0.00 | 0.00 |
| cursor_renew_set | 10000 | 248 (243) | 246 (243) | 4.03M | 4.07M | 0.99 | 0.00 | 0.00 |
| update_put | 10000 | 60208 (59003) | 57818 (57279) | 16.6k | 17.3k | 0.96 | 1.00 | 1.06 |
| txn_put | 10000 | 60716 (59344) | 58694 (58606) | 16.5k | 17.0k | 0.97 | 1.00 | 1.06 |


### New reference: FreeBSD 15.1 arm64 (VM, n = 100 000)

* freebsd arm64, CPU: (model not reported), 2 cores, ? GiB RAM
* OS page size (= LMDB page size): 4096
* Go: go1.26.9
* cgo: lmdb-go v1.9.4's Go code at e5af975 (cgo, LMDB 0.9.36: (Aug 6, 2026), plus ITS#10615, ITS#10642, ITS#10643)
* port: the Go implementation (LMDB 0.9.90: (lmdb-go, Go port of rlmdb))

n = 100000, 3 round(s) of 3 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 100000 | 65.4 (63.0) | 111 (106) | 15.29M | 8.99M | 1.70 | 0.00 | 0.00 |
| rand_put | 100000 | 438 (431) | 559 (546) | 2.28M | 1.79M | 1.28 | 0.00 | 0.04 |
| rand_get | 100000 | 193 (191) | 186 (183) | 5.18M | 5.38M | 0.96 | 0.00 | 0.00 |
| scan_fwd | 100000 | 28.6 (28.1) | 13.5 (13.3) | 34.99M | 74.10M | 0.47 | 0.00 | 0.00 |
| scan_bwd | 100000 | 28.2 (27.6) | 14.9 (14.6) | 35.43M | 67.20M | 0.53 | 0.00 | 0.00 |
| readers8_get | 800000 | 102 (99.4) | 100 (94.9) | 9.78M | 9.97M | 0.98 | 0.00 | 0.00 |
| rand_overwrite | 100000 | 654 (629) | 696 (684) | 1.53M | 1.44M | 1.06 | 0.00 | 0.00 |
| dup_put | 100000 | 304 (299) | 554 (522) | 3.29M | 1.80M | 1.82 | 0.00 | 0.02 |
| dup_get_both | 100000 | 224 (221) | 232 (231) | 4.46M | 4.31M | 1.04 | 0.00 | 0.00 |
| dup_next_dup | 100000 | 50.2 (49.6) | 36.7 (36.5) | 19.92M | 27.25M | 0.73 | 0.20 | 0.00 |
| put_4k | 5000 | 1346 (1296) | 1679 (1596) | 742.9k | 595.6k | 1.25 | 0.00 | 0.23 |
| get_4k | 100000 | 147 (144) | 144 (143) | 6.82M | 6.95M | 0.98 | 0.00 | 0.00 |
| put_64k | 500 | 16648 (11853) | 17064 (12374) | 60.1k | 58.6k | 1.02 | 0.00 | 1.18 |
| get_64k | 100000 | 94.1 (92.6) | 95.2 (94.3) | 10.63M | 10.51M | 1.01 | 0.00 | 0.00 |
| del_rand | 100000 | 525 (517) | 699 (690) | 1.90M | 1.43M | 1.33 | 0.00 | 0.00 |
| txn_nosync | 5000 | 2091 (2025) | 2852 (2749) | 478.3k | 350.6k | 1.36 | 1.00 | 1.05 |
| txn_sync | 100 | 65867 (62367) | 66339 (65232) | 15.2k | 15.1k | 1.01 | 1.00 | 1.36 |
| mixed_writer | 5000 | 1366 (1320) | 1512 (1468) | 732.0k | 661.6k | 1.11 | 0.01 | 0.02 |
| mixed_readers | 200704 | 33.8 (15.2) | 17.9 (16.9) | 29.55M | 55.79M | 0.53 | 0.00 | 0.00 |
| ls_scan | 500000 | 43.8 (41.9) | 27.8 (27.7) | 22.82M | 36.02M | 0.63 | 0.00 | 0.00 |
| ls_update | 5000 | 2142 (1885) | 2353 (2060) | 466.9k | 425.0k | 1.10 | 0.00 | 0.00 |
| get_raw | 100000 | 192 (190) | 186 (185) | 5.20M | 5.37M | 0.97 | 0.00 | 0.00 |
| get_copy | 100000 | 226 (212) | 203 (199) | 4.42M | 4.92M | 0.90 | 1.00 | 1.00 |
| get_key_local | 100000 | 205 (200) | 195 (193) | 4.88M | 5.13M | 0.95 | 1.00 | 1.00 |
| get_key_string | 100000 | 206 (203) | 203 (201) | 4.84M | 4.91M | 0.99 | 1.00 | 1.00 |
| cursor_set | 100000 | 201 (198) | 202 (200) | 4.99M | 4.96M | 1.01 | 0.00 | 0.00 |
| cursor_open_set | 100000 | 285 (281) | 223 (221) | 3.51M | 4.48M | 0.78 | 1.00 | 1.00 |
| view_get | 10000 | 328 (311) | 304 (282) | 3.05M | 3.29M | 0.93 | 1.00 | 1.00 |
| view_empty | 10000 | 119 (115) | 116 (93.9) | 8.43M | 8.66M | 0.97 | 1.00 | 1.00 |
| view8_get | 80000 | 305 (298) | 236 (223) | 3.28M | 4.23M | 0.78 | 1.00 | 1.00 |
| txn_get | 10000 | 362 (350) | 314 (308) | 2.77M | 3.19M | 0.87 | 1.00 | 1.00 |
| renew_get | 10000 | 228 (224) | 246 (239) | 4.38M | 4.07M | 1.08 | 0.00 | 0.00 |
| cursor_renew_set | 10000 | 249 (246) | 255 (249) | 4.02M | 3.93M | 1.02 | 0.00 | 0.00 |
| update_put | 10000 | 5367 (5273) | 6997 (6913) | 186.3k | 142.9k | 1.30 | 1.00 | 1.06 |
| txn_put | 10000 | 5393 (5352) | 6952 (6906) | 185.4k | 143.9k | 1.29 | 1.00 | 1.06 |

### Final validation: macOS

* macOS 15, arm64, CPU: Apple M1 Pro, 10 cores, 32 GiB RAM
* OS page size (= LMDB page size): 16384
* Go: go1.25.4
* cgo: lmdb-go v1.9.4 (cgo, LMDB 0.9.35: (Jan 27, 2026))
* port: the Go implementation (LMDB 0.9.85: (lmdb-go, Go port of rlmdb))


n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 136 (116) | 340 (185) | 7.36M | 2.94M | 2.50 | 0.00 | 0.00 |
| rand_put | 1000000 | 2622 (2412) | 2656 (2552) | 381.4k | 376.5k | 1.01 | 0.00 | 0.01 |
| rand_get | 1000000 | 1341 (1066) | 938 (823) | 745.8k | 1.07M | 0.70 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 72.3 (69.5) | 30.2 (28.7) | 13.84M | 33.15M | 0.42 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 70.0 (67.7) | 33.5 (31.9) | 14.29M | 29.81M | 0.48 | 0.00 | 0.00 |
| readers8_get | 8000000 | 166 (143) | 124 (113) | 6.03M | 8.06M | 0.75 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 4284 (3931) | 4271 (3643) | 233.4k | 234.2k | 1.00 | 0.00 | 0.00 |
| dup_put | 1000000 | 856 (800) | 1200 (1189) | 1.17M | 833.5k | 1.40 | 0.00 | 0.00 |
| dup_get_both | 1000000 | 1045 (998) | 924 (844) | 957.2k | 1.08M | 0.88 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 178 (173) | 119 (110) | 5.63M | 8.38M | 0.67 | 0.20 | 0.00 |
| put_4k | 50000 | 6451 (5929) | 6669 (6512) | 155.0k | 149.9k | 1.03 | 0.00 | 0.04 |
| get_4k | 1000000 | 725 (603) | 572 (527) | 1.38M | 1.75M | 0.79 | 0.00 | 0.00 |
| put_64k | 5000 | 26912 (24186) | 15656 (14793) | 37.2k | 63.9k | 0.58 | 0.00 | 0.22 |
| get_64k | 1000000 | 393 (332) | 274 (269) | 2.55M | 3.65M | 0.70 | 0.00 | 0.00 |
| del_rand | 1000000 | 3593 (3037) | 3540 (3311) | 278.3k | 282.5k | 0.99 | 0.00 | 0.00 |
| txn_nosync | 50000 | 6973 (6325) | 8275 (7967) | 143.4k | 120.8k | 1.19 | 1.00 | 1.01 |
| txn_sync | 1000 | 5126988 (4945013) | 9982706 (9757339) | 195 | 100 | 1.95 | 1.00 | 1.04 |
| mixed_writer | 50000 | 5258 (5030) | 7616 (6205) | 190.2k | 131.3k | 1.45 | 0.01 | 0.02 |
| mixed_readers | 12091392 | 23.8 (18.7) | 8.9 (8.2) | 42.01M | 112.93M | 0.37 | 0.00 | 0.00 |
| ls_scan | 5000000 | 89.6 (70.3) | 31.0 (30.2) | 11.15M | 32.27M | 0.35 | 0.00 | 0.00 |
| ls_update | 50000 | 5469 (5062) | 5497 (4975) | 182.9k | 181.9k | 1.01 | 0.00 | 0.00 |
| get_raw | 1000000 | 1375 (1092) | 846 (819) | 727.3k | 1.18M | 0.62 | 0.00 | 0.00 |
| get_copy | 1000000 | 1353 (1124) | 969 (875) | 739.0k | 1.03M | 0.72 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1235 (1152) | 861 (830) | 809.8k | 1.16M | 0.70 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1388 (1236) | 1037 (949) | 720.7k | 964.3k | 0.75 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1236 (1099) | 955 (861) | 809.3k | 1.05M | 0.77 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1620 (1342) | 910 (859) | 617.4k | 1.10M | 0.56 | 1.00 | 1.00 |
| view_get | 100000 | 2456 (2134) | 1076 (969) | 407.1k | 929.6k | 0.44 | 1.00 | 1.00 |
| view_empty | 100000 | 1009 (861) | 149 (145) | 990.7k | 6.70M | 0.15 | 1.00 | 1.00 |
| view8_get | 800000 | 9700 (3031) | 356 (347) | 103.1k | 2.81M | 0.04 | 1.00 | 1.00 |
| txn_get | 100000 | 2319 (2091) | 1191 (1033) | 431.3k | 840.0k | 0.51 | 1.00 | 1.00 |
| renew_get | 100000 | 1313 (1176) | 1066 (926) | 761.4k | 938.1k | 0.81 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1345 (1205) | 1022 (941) | 743.5k | 978.7k | 0.76 | 0.00 | 0.00 |
| update_put | 100000 | 30352 (27208) | 37019 (31524) | 32.9k | 27.0k | 1.22 | 1.00 | 1.04 |
| txn_put | 100000 | 29312 (27707) | 34059 (31536) | 34.1k | 29.4k | 1.16 | 1.00 | 1.04 |

`seq_put` and `mixed` again, at load 4.5 to 5 (`-only seq_put,mixed`, same
options; the summary takes these):

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 117 (116) | 187 (185) | 8.56M | 5.35M | 1.60 | 0.00 | 0.00 |
| mixed_writer | 50000 | 5570 (5211) | 6010 (5565) | 179.5k | 166.4k | 1.08 | 0.01 | 0.02 |
| mixed_readers | 11837440 | 23.7 (21.6) | 8.2 (7.9) | 42.17M | 122.64M | 0.34 | 0.00 | 0.00 |

### Final validation: Linux arm64 (container, tmpfs)

* linux arm64, CPU: Apple (model not reported), 8 cores, 12 GiB RAM
* OS page size (= LMDB page size): 4096
* Go: go1.25.14
* cgo: lmdb-go v1.9.4 (cgo, LMDB 0.9.35: (Jan 27, 2026))
* port: the Go implementation (LMDB 0.9.85: (lmdb-go, Go port of rlmdb))


n = 1000000, 3 round(s) of 5 timed reps after 1 warm-up; ns/op: median (min); allocs/op: Go heap allocations per operation (median); ratio = port median / cgo median (below 1 means port is faster)

| workload | ops/rep | cgo ns/op | port ns/op | cgo ops/s | port ops/s | port/cgo | cgo allocs/op | port allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| seq_put_append | 1000000 | 150 (142) | 209 (198) | 6.67M | 4.79M | 1.39 | 0.00 | 0.00 |
| rand_put | 1000000 | 2673 (2468) | 2585 (2234) | 374.1k | 386.9k | 0.97 | 0.00 | 0.01 |
| rand_get | 1000000 | 1046 (941) | 1121 (902) | 956.2k | 891.9k | 1.07 | 0.00 | 0.00 |
| scan_fwd | 1000000 | 86.1 (81.9) | 49.6 (44.9) | 11.61M | 20.17M | 0.58 | 0.00 | 0.00 |
| scan_bwd | 1000000 | 80.7 (76.3) | 45.5 (44.0) | 12.39M | 22.00M | 0.56 | 0.00 | 0.00 |
| readers8_get | 8000000 | 156 (132) | 155 (126) | 6.42M | 6.46M | 0.99 | 0.00 | 0.00 |
| rand_overwrite | 1000000 | 3653 (3435) | 3244 (3095) | 273.8k | 308.2k | 0.89 | 0.00 | 0.00 |
| dup_put | 1000000 | 1008 (930) | 1266 (1165) | 992.4k | 789.9k | 1.26 | 0.00 | 0.00 |
| dup_get_both | 1000000 | 994 (909) | 1067 (929) | 1.01M | 937.0k | 1.07 | 0.00 | 0.00 |
| dup_next_dup | 1000000 | 160 (144) | 141 (122) | 6.27M | 7.08M | 0.89 | 0.20 | 0.00 |
| put_4k | 50000 | 4893 (4474) | 3673 (3432) | 204.4k | 272.2k | 0.75 | 0.00 | 0.03 |
| get_4k | 1000000 | 582 (466) | 496 (423) | 1.72M | 2.02M | 0.85 | 0.00 | 0.00 |
| put_64k | 5000 | 47517 (43418) | 26697 (23253) | 21.0k | 37.5k | 0.56 | 0.00 | 0.23 |
| get_64k | 1000000 | 297 (284) | 324 (295) | 3.37M | 3.08M | 1.09 | 0.00 | 0.00 |
| del_rand | 1000000 | 4899 (4785) | 3470 (3379) | 204.1k | 288.2k | 0.71 | 0.00 | 0.00 |
| txn_nosync | 50000 | 4923 (4567) | 5350 (4886) | 203.1k | 186.9k | 1.09 | 1.00 | 1.04 |
| txn_sync | 1000 | 3776 (3636) | 4434 (4084) | 264.8k | 225.5k | 1.17 | 1.00 | 1.08 |
| mixed_writer | 50000 | 5480 (5179) | 5078 (4743) | 182.5k | 196.9k | 0.93 | 0.01 | 0.03 |
| mixed_readers | 10608640 | 25.0 (24.4) | 12.6 (11.9) | 39.97M | 79.60M | 0.50 | 0.00 | 0.00 |
| ls_scan | 5000000 | 94.3 (90.7) | 57.0 (55.0) | 10.60M | 17.56M | 0.60 | 0.00 | 0.00 |
| ls_update | 50000 | 4755 (4549) | 4352 (4153) | 210.3k | 229.8k | 0.92 | 0.00 | 0.00 |
| get_raw | 1000000 | 1002 (895) | 1085 (900) | 997.9k | 921.7k | 1.08 | 0.00 | 0.00 |
| get_copy | 1000000 | 1111 (975) | 1043 (925) | 900.1k | 958.7k | 0.94 | 1.00 | 1.00 |
| get_key_local | 1000000 | 1003 (891) | 1073 (922) | 997.3k | 932.2k | 1.07 | 1.00 | 1.00 |
| get_key_string | 1000000 | 1288 (1122) | 1232 (1038) | 776.2k | 811.5k | 0.96 | 1.00 | 1.00 |
| cursor_set | 1000000 | 1038 (930) | 1072 (978) | 963.4k | 933.1k | 1.03 | 0.00 | 0.00 |
| cursor_open_set | 1000000 | 1278 (1182) | 1080 (1013) | 782.2k | 926.3k | 0.84 | 1.00 | 1.00 |
| view_get | 100000 | 1418 (1266) | 1281 (1095) | 705.3k | 780.8k | 0.90 | 1.00 | 1.00 |
| view_empty | 100000 | 291 (280) | 184 (175) | 3.44M | 5.43M | 0.63 | 1.00 | 1.00 |
| view8_get | 800000 | 579 (543) | 384 (333) | 1.73M | 2.60M | 0.66 | 1.00 | 1.00 |
| txn_get | 100000 | 1454 (1399) | 1269 (1181) | 687.6k | 787.9k | 0.87 | 1.00 | 1.00 |
| renew_get | 100000 | 1113 (992) | 1123 (1042) | 898.1k | 890.2k | 1.01 | 0.00 | 0.00 |
| cursor_renew_set | 100000 | 1158 (1102) | 1106 (1002) | 863.6k | 904.5k | 0.95 | 0.00 | 0.00 |
| update_put | 100000 | 16485 (16035) | 17109 (15550) | 60.7k | 58.4k | 1.04 | 1.00 | 1.07 |
| txn_put | 100000 | 16617 (16099) | 17012 (15616) | 60.2k | 58.8k | 1.02 | 1.00 | 1.07 |

The stage 3 tables: "before" is `main` at 4b0232f, "after" the end of
stage 3's branch (the build measured predates the fix folded into the
nested-transaction commit, which only changes nested transactions; no
workload here uses them).

### Stage 3: macOS, before

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

### Stage 3: macOS, after

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

### Stage 3: Linux arm64 (container, tmpfs), before

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

### Stage 3: Linux arm64 (container, tmpfs), after

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
