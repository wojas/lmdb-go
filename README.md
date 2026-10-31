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
lock and the reader-table lock: libc pthread mutexes on Linux, POSIX named
semaphores on macOS. Everything else is Go. A build without cgo
(`CGO_ENABLED=0`) can only open environments with the `NoLock` flag;
`Env.Open` without it returns `ErrNoSharedLocking`, and
`SharedLockingAvailable` reports which kind of build is running. Go turns cgo
off by default when cross-compiling or when no C compiler is found: such a
build compiles, but fails at `Env.Open`.

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
  reader table, and on Linux a writer that dies holding the lock is
  recovered from, as between C processes.
* **Platforms.** Linux (glibc and musl; arm64 and x86-64) and macOS. Windows
  is planned.
* **`NoLock`** only removes the locking between processes. Within the
  process, write transactions are still serialised and pages that a live
  reader can see are never reused; opening one environment twice in a
  process fails with `EBUSY`.
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
| random get | 1384 | 1018 | 0.74 | 0.98 |
| forward cursor scan | 71.8 | 31.2 | 0.43 | 0.57 |
| random get, 8 concurrent readers | 227 | 141 | 0.62 | 0.83 |
| random put | 2996 | 2871 | 0.96 | 0.94 |
| random delete | 3632 | 3813 | 1.05 | 0.79 |
| commit of one put, no sync | 7858 | 8888 | 1.13 | 1.10 |
| sequential append | 118 | 194 | 1.64 | 1.39 |

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
