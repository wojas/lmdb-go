// Package bench holds the benchmarks of the Go implementation against the
// pinned cgo reference (lmdb-go v1.9.4's Go code on C LMDB 0.9.36 plus
// ITS#10615, ITS#10642 and ITS#10643): rlmdb's benchmark workloads
// (bench) and lmdb-go's API costs, as standard Go benchmarks
// (bench_test.go: BenchmarkX/impl=cgo and BenchmarkX/impl=port, for
// benchstat) and as rlmdb-bench's interleaved table (cmd/lmdb-bench). See
// README.md.
//
// The workloads are written once, in ./cgo against the cgo reference;
// ./port is generated from it for the port by tests/gen-port.sh.
package bench

//go:generate sh ../gen-port.sh .
