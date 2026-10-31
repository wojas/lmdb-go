// Package bench holds the benchmarks of the Go implementation against the
// cgo lmdb-go v1.9.4 (C LMDB 0.9.35): rlmdb's benchmark workloads
// (rust/bench) and lmdb-go's API costs, as standard Go benchmarks
// (bench_test.go: BenchmarkX/impl=cgo and BenchmarkX/impl=port, for
// benchstat) and as rlmdb-bench's interleaved table (cmd/lmdb-bench). See
// README.md.
//
// The workloads are written once, in ./cgo against the cgo reference;
// ./port is generated from it for the port by tests/gen-port.sh.
package bench

//go:generate sh ../gen-port.sh .
