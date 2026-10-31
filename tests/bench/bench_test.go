package bench

import (
	"flag"
	"path/filepath"
	"testing"

	cgowl "github.com/PowerDNS/lmdb-go/tests/bench/cgo"
	"github.com/PowerDNS/lmdb-go/tests/bench/run"
)

var (
	benchN      = flag.Int("lmdb.n", 100_000, "keys per workload group (lmdb-bench -n); every other size scales with it")
	syncCommits = flag.Int("lmdb.sync-commits", 100, "commits of txn_sync (lmdb-bench -sync-commits)")
)

// side is the workloads of one implementation.
type side struct {
	name string
	run  func(group string, p run.Params) ([]run.Result, error)
}

// sides are the implementations benchmarked: cgo, and the port
// (port_test.go).
var sides = []side{{cgowl.Impl, cgowl.Run}}

// benchGroup runs workload group as sub-benchmarks impl=<side>. A b.N
// iteration is one timed repetition of every workload of the group (after
// a warm-up and the group's setup, which the built-in ns/op includes); the
// numbers to compare are the per-workload metrics <workload>-ns/op (the
// median over the b.N repetitions of ns per operation) and
// <workload>-allocs/op (Go heap allocations per operation):
//
//	go test -run '^$' -bench . -benchtime 5x -count 6 ./bench | tee new.txt
//	benchstat -col /impl new.txt
func benchGroup(b *testing.B, group string) {
	for _, s := range sides {
		b.Run("impl="+s.name, func(b *testing.B) {
			p := run.Params{
				Dir:         filepath.Join(b.TempDir(), "env"),
				N:           *benchN,
				Reps:        b.N,
				SyncCommits: *syncCommits,
			}
			results, err := s.run(group, p)
			if err != nil {
				b.Fatal(err)
			}
			for _, r := range results {
				st := run.Summarise(r.Samples)
				b.ReportMetric(st.Median, r.Name+"-ns/op")
				b.ReportMetric(st.Allocs, r.Name+"-allocs/op")
			}
		})
	}
}

func BenchmarkSeqPut(b *testing.B)    { benchGroup(b, "seq_put") }
func BenchmarkRandPut(b *testing.B)   { benchGroup(b, "rand_put") }
func BenchmarkRead(b *testing.B)      { benchGroup(b, "read") }
func BenchmarkOverwrite(b *testing.B) { benchGroup(b, "overwrite") }
func BenchmarkDupsort(b *testing.B)   { benchGroup(b, "dupsort") }
func BenchmarkOverflow(b *testing.B)  { benchGroup(b, "overflow") }
func BenchmarkDelete(b *testing.B)    { benchGroup(b, "delete") }
func BenchmarkSmallTxn(b *testing.B)  { benchGroup(b, "small_txn") }
func BenchmarkMixed(b *testing.B)     { benchGroup(b, "mixed") }
func BenchmarkLS(b *testing.B)        { benchGroup(b, "ls") }
func BenchmarkGoAPI(b *testing.B)     { benchGroup(b, "goapi") }
