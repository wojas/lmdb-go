# lmdb-go with a Go implementation of LMDB

This is a fork of [PowerDNS/lmdb-go](https://github.com/PowerDNS/lmdb-go) v1
in which the bundled C library is replaced by an implementation of LMDB in
Go, ported from rlmdb, a Rust rewrite of LMDB. It keeps lmdb-go v1's API and
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

The module declares Go 1.22; development and testing use Go 1.25.

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
  reader table, and on Linux and Windows a writer that dies holding the
  lock is recovered from, as between C processes (by the same design on
  FreeBSD, and with LMDB 1.0 lock files on the other BSDs).
* **Platforms.** Linux (glibc and musl; arm64 and x86-64) and macOS.
  Windows (x86-64; arm64 by the same rules) is implemented after rlmdb's
  Windows port and builds, but has not run on Windows yet. FreeBSD,
  OpenBSD, NetBSD and DragonFly (64-bit) are implemented after rlmdb's BSD
  support, with C's lock primitives and lock-file layouts there: they
  build and are untested. (rlmdb's own suites pass on FreeBSD in its CI;
  this port has not run on any BSD yet. The parts macOS's kernel can run,
  the SysV semaphore set of LMDB 1.0 lock files and NetBSD's semaphore
  names, run in the lock-file tests on macOS.) 64-bit only: 32-bit builds
  stop with a compile error; 64-bit big-endian targets build, and s390x
  is tested under emulation (QEMU user mode), against C peers built for
  it.
* **`NoLock`** only removes the locking between processes. Within the
  process, write transactions are still serialised and pages that a live
  reader can see are never reused, also across several `NoLock` handles
  of one environment, which share their reader table; with a lock file,
  opening one environment twice in a process fails with `EBUSY`.
* **Version.** `lmdb.Version()` reports 0.9.85, above every C 0.9 release, so
  programs can tell this implementation from C.

Differences from lmdb-go v1 on C (a few v1 bugs not kept, ignored flags such
as `WriteMap`, stricter checks on ended handles) are listed in
[lmdb/DEVIATIONS.md](lmdb/DEVIATIONS.md).

## Performance

Compared with lmdb-go v1.9.4 (cgo, C LMDB 0.9.35), including its cgo call
overhead, both in one process with interleaved runs, 1,000,000 keys. Times
are medians in ns per operation on macOS (Apple M1 Pro, 16 KiB pages). The
ratio is this implementation's time divided by v1's: below 1 means this one
is faster. The last column is the ratio on Linux arm64 (4 KiB pages).

| workload | v1 ns/op | this ns/op | ratio | ratio, Linux |
|---|---:|---:|---:|---:|
| random get | 1341 | 938 | 0.70 | 1.07 |
| forward cursor scan | 72.3 | 30.2 | 0.42 | 0.58 |
| random get, 8 concurrent readers | 166 | 124 | 0.75 | 0.99 |
| read transaction with one get (`View`) | 2456 | 1076 | 0.44 | 0.90 |
| random put | 2622 | 2656 | 1.01 | 0.97 |
| random delete | 3593 | 3540 | 0.99 | 0.71 |
| commit of one put, no sync | 6973 | 8275 | 1.19 | 1.09 |
| sequential append | 117 | 187 | 1.60 | 1.39 |

Synced commits on macOS take about twice as long as v1's: like C LMDB 0.9.36
and later, a commit flushes the drive cache twice, where 0.9.35 flushes once.
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
rlmdb, a Rust rewrite that follows LMDB function by function.

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
([LICENSE.mdb.md](LICENSE.mdb.md)).
