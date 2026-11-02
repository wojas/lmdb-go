# lmdb-go with a Go implementation of LMDB

This is a fork of [PowerDNS/lmdb-go](https://github.com/PowerDNS/lmdb-go) v1
in which the bundled C library is replaced by an implementation of LMDB in
Go, ported from [rlmdb](https://github.com/wojas/rlmdb), a Rust rewrite of
LMDB. It keeps lmdb-go v1's API and
module path, so existing programs can switch with a `replace` directive, and
adds lmdb-go v2's API for the LMDB 1.0 file format. It uses C LMDB's data and
lock files, so C LMDB 0.9 and 1.0, rlmdb and Go processes can use one
environment at the same time.

## Status

Unreleased and under development. The final module and package name is not
decided yet; until then the module path stays `github.com/PowerDNS/lmdb-go`.

## Trying it

Point an existing lmdb-go v1 program at a checkout of this repository:

```sh
go mod edit -replace github.com/PowerDNS/lmdb-go=../lmdb-go
```

The module declares Go 1.22; development and testing use Go 1.25 to 1.27.

cgo is used only for the two locks LMDB shares between processes, the writer
lock and the reader-table lock: libc pthread mutexes on Linux and FreeBSD,
POSIX named semaphores on macOS, POSIX named or SysV semaphores on OpenBSD,
NetBSD and DragonFly. Everything else is Go. On Linux and the BSDs, shared
locking needs cgo: a build without cgo (`CGO_ENABLED=0`) can only open
environments with the `NoLock` flag; `Env.Open` without it returns
`ErrNoSharedLocking`, and `SharedLockingAvailable` reports which kind of
build is running. Go turns cgo off by default when cross-compiling or when
no C compiler is found: such a Linux or BSD build compiles, but fails at
`Env.Open`. On macOS, a cgo build (the
default) takes the semaphores through cgo, and a build without cgo calls them
in libSystem directly, as `golang.org/x/sys/unix` does for system calls; both
work fully, so cross-compiling to macOS needs no C toolchain. On Windows the
locks are named kernel mutexes, as in C LMDB there, reached through
`golang.org/x/sys/windows`: no cgo at all.

The packages are those of lmdb-go: `lmdb`, `lmdbscan`, `exp/lmdbsync`, and the
commands in `cmd/` and `exp/cmd/`. Read the API documentation with
`go doc ./lmdb`; it is lmdb-go v1's, with notes where behaviour changed.

## Compatibility

* **File formats.** Both LMDB 0.9 and LMDB 1.0 data files are read and
  written, with lmdb-go v2's API (`Env.SetFormat`, `Env.Format`,
  `SniffFormat`, ...). An existing environment opens in its own format; new
  ones are 0.9 unless `Env.SetFormat(lmdb.V10)` or `SetDefaultFormat` says
  otherwise. For the same writes, the data file is byte-identical to C's.
* **Sharing.** Go processes share environments with C LMDB 0.9 and 1.0 and
  with rlmdb: writers exclude each other, readers see each other in the
  reader table, and on Linux, FreeBSD and Windows, and on OpenBSD with
  LMDB 1.0 lock files, a writer that dies holding the lock is recovered
  from, as between C processes (by the same design with LMDB 1.0 lock
  files on NetBSD and DragonFly).
* **Platforms.** Linux (glibc and musl; arm64 and x86-64) and macOS.
  Windows x86-64 (after rlmdb's Windows port; tested on Windows 10,
  against C LMDB built with MinGW-w64; arm64 builds by the same rules,
  untested). FreeBSD,
  OpenBSD, NetBSD and DragonFly (64-bit) are implemented after rlmdb's BSD
  support, with C's lock primitives and lock-file layouts there: they
  build, also with cgo against each BSD's headers
  (`scripts/bsd-cc-check.sh`). FreeBSD and OpenBSD are tested on arm64
  (FreeBSD 15.1 and OpenBSD 7.9, against C LMDB 0.9 and 1.0 built
  there); NetBSD, DragonFly, and FreeBSD and OpenBSD on amd64 are
  untested. On OpenBSD, C LMDB itself needs
  upstream's fix ITS#10615, which LMDB 0.9.37 and 1.0.3 (2026-10-21) have
  and earlier releases do not: without it, C reads stale pages after its
  own writes there (lmdb-go v1.9.4, with C 0.9.35, fails on OpenBSD for
  that reason). (The parts macOS's kernel can run, the SysV
  semaphore set of LMDB 1.0 lock files and NetBSD's semaphore names, run
  in the lock-file tests on macOS.) 64-bit only: 32-bit builds
  stop with a compile error; 64-bit big-endian targets build, and s390x
  is tested under emulation (QEMU user mode), against C peers built for
  it.
* **`NoLock`** only removes the locking between processes. Within the
  process, write transactions are still serialised and pages that a live
  reader can see are never reused, also across several `NoLock` handles
  of one environment, which share their reader table; with a lock file,
  opening one environment twice in a process fails with `EBUSY`.
* **Version.** `lmdb.Version()` reports 0.9.90, above every C 0.9 release, so
  programs can tell this implementation from C.

Differences from lmdb-go v1 on C (a few v1 bugs not kept, ignored flags such
as `WriteMap`, stricter checks on ended handles) are listed in
[lmdb/DEVIATIONS.md](lmdb/DEVIATIONS.md).

## Performance

Compared with lmdb-go v1.9.4's Go code on C LMDB 0.9.36 plus ITS#10615,
ITS#10642 and ITS#10643 (the same C code as LMDB 0.9.37; the released
v1.9.4 bundles 0.9.35), including its cgo call overhead, both in one
process with interleaved runs, 1,000,000 keys. Times are medians in ns
per operation on Linux x86-64: an AMD EPYC Milan server (4 vCPUs of a
virtual machine), ext4, 4 KiB pages, Go 1.26. The ratio is this
implementation's time divided by lmdb-go's: below 1 means this one is
faster. The last two columns are the ratios on macOS (Apple M1 Pro, 16 KiB
pages, Go 1.27) and on Linux arm64 (a VM on that Mac, 4 KiB pages),
measured on 2026-11-02.

| workload | lmdb-go ns/op | this ns/op | ratio | ratio, macOS | ratio, Linux arm64 |
|---|---:|---:|---:|---:|---:|
| random get | 1106 | 1063 | 0.96 | 0.74 | 1.04 |
| forward cursor scan | 104 | 76.3 | 0.74 | 0.49 | 0.64 |
| random get, 8 concurrent readers | 448 | 397 | 0.89 | 0.70 | 0.99 |
| read transaction with one get (`View`) | 1415 | 1327 | 0.94 | 0.50 | 1.13 |
| random put | 3221 | 3717 | 1.15 | 1.01 | 0.93 |
| random overwrite | 3966 | 4521 | 1.14 | 0.95 | 0.92 |
| random delete | 3939 | 4727 | 1.20 | 1.05 | 0.72 |
| commit of one put, no sync | 7977 | 10774 | 1.35 | 1.14 | 1.11 |
| commit of one put, synced | 328011 | 333167 | 1.02 | 1.00 | (tmpfs) |
| sequential append | 212 | 325 | 1.53 | 1.72 | 1.65 |

Reads are as fast as lmdb-go's or faster. Writes cost relatively more on
x86-64 than on Apple silicon: this implementation executes up to about
twice as many instructions per write as C LMDB, which the M1 runs in the
cycles both spend waiting for memory, and the EPYC largely does not.
Transactions of a single put show it most, as each pays the fixed cost of
a commit.
Synced commits take as long as lmdb-go's. On macOS both flush the drive
cache twice per commit, as C LMDB does since 0.9.36; the released lmdb-go
v1.9.4, with C 0.9.35, flushes once, so against it synced commits on macOS
take about twice as long. The Linux arm64 column comes from a VM on the
same Mac, in which the 32-byte vector loads that Go 1.26 and later use to
copy structs on arm64 are slow: read transactions (`View`, `Renew`) cost
more there with those Go versions than with Go 1.25, where the `View` row
was 0.90.
On OpenBSD 7.9 arm64 (a 2-CPU VM, Go 1.26, 100,000 keys) the ratios are
1.01 for a random get, 0.43 for a forward scan, 0.36 for `View`, 0.97 for a
random put, 0.96 for a random delete, 1.03 for a commit without sync and
1.13 for a sequential append.
All workloads, allocation counts, the method and the remaining gaps:
[tests/bench/RESULTS.md](tests/bench/RESULTS.md).

## Testing

C LMDB is the oracle. lmdb-go v1's test suite and v2's tests of the format
API run against this implementation. A differential test applies the same
operations to this engine and to C LMDB 0.9 and 1.0 in lock step and requires
byte-identical data files. Cross-process tests run C, rlmdb and Go processes
on one environment through rlmdb's interop harness, with a Go peer
(`tests/gopeer`), on macOS and on Linux with glibc and musl. The C programs
come from rlmdb's interop build; the comparisons with lmdb-go v1 live in the
nested module `tests/`. See [tests/gopeer/README.md](tests/gopeer/README.md),
[internal/engine/PORTING.md](internal/engine/PORTING.md) and
[tests/bench/README.md](tests/bench/README.md).

## History and credits

LMDB was written by Howard Chu of Symas Corp. The engine is ported from
[rlmdb](https://github.com/wojas/rlmdb), a Rust rewrite that follows LMDB
function by function.

The Go API and this repository's history start with Ferenc Szalai's gomdb
(2013), which Bryan Matsuo developed into
[bmatsuo/lmdb-go](https://github.com/bmatsuo/lmdb-go). PowerDNS forked it
after that repository went without updates for four years, and thanks
*bmatsuo* for creating and maintaining it until 2017. The PowerDNS fork was
renamed so it could be used without `replace` directives; versions from
1.9.0 on use the import path `github.com/PowerDNS/lmdb-go`.

## License

Except where noted otherwise, files in this repository are licensed under
the BSD 3-clause license ([LICENSE.md](LICENSE.md)). The engine, a
derivative of LMDB through rlmdb, and the copy of LMDB's C sources in
`tests/cgolmdb` are covered by the OpenLDAP Public License
([LICENSE.mdb.md](LICENSE.mdb.md)). [COPYRIGHT](COPYRIGHT) lists which
parts fall under which license and the copyright notices they carry.
