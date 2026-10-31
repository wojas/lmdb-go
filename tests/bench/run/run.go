// Package run holds what the workloads of both implementations (the twin
// packages ../cgo and ../port) and the programs that run them share: the
// parameters of a run, the timed samples and results, their statistics
// and the result table. It is the Go form of the driver half of rlmdb's
// rust/bench/src/main.rs (its RESULT lines, stats and table); it imports
// no LMDB.
package run

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// Groups are the workload groups, in the order they run: rlmdb's, then
// goapi (lmdb-go's API costs, Go only).
var Groups = []string{
	"seq_put", "rand_put", "read", "overwrite", "dupsort", "overflow",
	"delete", "small_txn", "mixed", "ls", "goapi",
}

// Params are the parameters of one group run (cbench's command line).
type Params struct {
	Dir         string // the environment directory, created and removed
	N           int    // keys; every other size scales with it
	Reps        int    // timed repetitions per workload, after one warm-up
	SyncCommits int    // commits of txn_sync
}

// Sample is one timed repetition: elapsed nanoseconds, operations, and Go
// heap allocations during it.
type Sample struct {
	NS, Ops, Allocs uint64
}

// Result is the timed repetitions of one workload.
type Result struct {
	Name    string
	Samples []Sample
}

// Line is the result as a RESULT line of cbench and rlmdb-bench's worker
// mode, with the allocations as a third field: "RESULT <name>
// <ns>/<ops>/<allocs> ...".
func (r Result) Line() string {
	var b strings.Builder
	b.WriteString("RESULT " + r.Name)
	for _, s := range r.Samples {
		fmt.Fprintf(&b, " %d/%d/%d", s.NS, s.Ops, s.Allocs)
	}
	return b.String()
}

// ParseLine parses a RESULT line (also cbench's and rlmdb-bench's, which
// have no allocations: Allocs is then math.MaxUint64).
func ParseLine(line string) (Result, bool) {
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "RESULT" {
		return Result{}, false
	}
	r := Result{Name: f[1]}
	for _, p := range f[2:] {
		parts := strings.Split(p, "/")
		if len(parts) < 2 || len(parts) > 3 {
			return Result{}, false
		}
		v := [3]uint64{2: math.MaxUint64}
		for k, s := range parts {
			x, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return Result{}, false
			}
			v[k] = x
		}
		r.Samples = append(r.Samples, Sample{v[0], v[1], v[2]})
	}
	return r, true
}

// Stats summarise the samples of one workload on one side.
type Stats struct {
	Median, Min float64 // ns per operation
	Ops         float64 // operations per repetition (median)
	Allocs      float64 // Go heap allocations per operation (median), -1 if unknown
}

func median(v []float64) float64 {
	slices.Sort(v)
	m := len(v) / 2
	if len(v)%2 == 0 {
		return (v[m-1] + v[m]) / 2
	}
	return v[m]
}

// Summarise computes the Stats of samples; nil for none.
func Summarise(samples []Sample) *Stats {
	if len(samples) == 0 {
		return nil
	}
	var perOp, ops, allocs []float64
	for _, s := range samples {
		n := math.Max(float64(s.Ops), 1)
		perOp = append(perOp, float64(s.NS)/n)
		ops = append(ops, float64(s.Ops))
		if s.Allocs != math.MaxUint64 {
			allocs = append(allocs, float64(s.Allocs)/n)
		}
	}
	st := &Stats{Median: median(perOp), Min: slices.Min(perOp), Ops: median(ops), Allocs: -1}
	if len(allocs) == len(samples) {
		st.Allocs = median(allocs)
	}
	return st
}

// Table collects the samples of every workload per side, in the order the
// workloads first appear, and prints them.
type Table struct {
	Sides []string              // column names, e.g. cgo and port
	order []string              // workload names
	rows  map[string][][]Sample // workload -> side -> samples
}

// NewTable is a table with these sides.
func NewTable(sides ...string) *Table {
	return &Table{Sides: sides, rows: map[string][][]Sample{}}
}

// Add pools the results of side k.
func (t *Table) Add(k int, results []Result) {
	for _, r := range results {
		row := t.rows[r.Name]
		if row == nil {
			row = make([][]Sample, len(t.Sides))
			t.rows[r.Name] = row
			t.order = append(t.order, r.Name)
		}
		row[k] = append(row[k], r.Samples...)
	}
}

func fmtNs(ns float64) string {
	if ns >= 100 {
		return fmt.Sprintf("%.0f", ns)
	}
	return fmt.Sprintf("%.1f", ns)
}

func fmtRate(nsPerOp float64) string {
	perS := 1e9 / nsPerOp
	switch {
	case perS >= 1e6:
		return fmt.Sprintf("%.2fM", perS/1e6)
	case perS >= 1e3:
		return fmt.Sprintf("%.1fk", perS/1e3)
	default:
		return fmt.Sprintf("%.0f", perS)
	}
}

func fmtAllocs(a float64) string {
	switch {
	case a < 0:
		return "-"
	case a < 10:
		return fmt.Sprintf("%.2f", a)
	default:
		return fmt.Sprintf("%.0f", a)
	}
}

// Print writes the markdown table: per workload the operations per
// repetition, per side ns/op as median (min) and ops/s from the median,
// with two sides the ratio of the medians (second / first), and per side
// the Go heap allocations per operation.
func (t *Table) Print(header string) {
	s := t.Sides
	fmt.Println()
	fmt.Print(header + "; ns/op: median (min); allocs/op: Go heap allocations per operation (median)")
	if len(s) == 2 {
		fmt.Printf("; ratio = %s median / %s median (below 1 means %s is faster)", s[1], s[0], s[1])
	}
	fmt.Print("\n\n")
	head, sep := "| workload | ops/rep |", "|---|---:|"
	for _, col := range []string{"ns/op", "ops/s"} {
		for _, n := range s {
			head += fmt.Sprintf(" %s %s |", n, col)
			sep += "---:|"
		}
	}
	if len(s) == 2 {
		head += fmt.Sprintf(" %s/%s |", s[1], s[0])
		sep += "---:|"
	}
	for _, n := range s {
		head += fmt.Sprintf(" %s allocs/op |", n)
		sep += "---:|"
	}
	fmt.Println(head)
	fmt.Println(sep)
	for _, name := range t.order {
		st := make([]*Stats, len(s))
		ops := 0.0
		for k := range s {
			if st[k] = Summarise(t.rows[name][k]); st[k] != nil && ops == 0 {
				ops = st[k].Ops
			}
		}
		line := fmt.Sprintf("| %s | %.0f |", name, ops)
		cell := func(f func(*Stats) string) {
			for _, x := range st {
				if x == nil {
					line += " - |"
				} else {
					line += " " + f(x) + " |"
				}
			}
		}
		cell(func(x *Stats) string { return fmtNs(x.Median) + " (" + fmtNs(x.Min) + ")" })
		cell(func(x *Stats) string { return fmtRate(x.Median) })
		if len(st) == 2 {
			if st[0] != nil && st[1] != nil {
				line += fmt.Sprintf(" %.2f |", st[1].Median/st[0].Median)
			} else {
				line += " - |"
			}
		}
		cell(func(x *Stats) string { return fmtAllocs(x.Allocs) })
		fmt.Println(line)
	}
}

// sh runs a shell command for the machine description; "" on failure.
func sh(cmd string) string {
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gib(bytes string) string {
	b, err := strconv.ParseFloat(bytes, 64)
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%.0f", b/(1<<30))
}

// MachineInfo describes what the numbers depend on: OS (macOS major
// version only), architecture, CPU, memory, page size, Go version. No
// host names or kernel build strings, so that a report can be published.
func MachineInfo() string {
	var b strings.Builder
	switch runtime.GOOS {
	case "darwin":
		major, _, _ := strings.Cut(sh("sw_vers -productVersion"), ".")
		fmt.Fprintf(&b, "* macOS %s, %s, CPU: %s, %d cores, %s GiB RAM\n", major, runtime.GOARCH,
			sh("sysctl -n machdep.cpu.brand_string"), runtime.NumCPU(), gib(sh("sysctl -n hw.memsize")))
	default:
		cpu := sh("sed -n 's/^model name[[:space:]]*: //p' /proc/cpuinfo | head -1")
		if cpu == "" {
			cpu = sh("lscpu 2>/dev/null | sed -n 's/^Model name: *//p' | head -1")
		}
		if cpu == "" || cpu == "-" {
			// Virtual machines on Apple Silicon report no model name
			// (and Alpine has no lscpu).
			cpu = strings.TrimSpace(sh("lscpu 2>/dev/null | sed -n 's/^Vendor ID: *//p' | head -1") + " (model not reported)")
		}
		mem := sh("awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo")
		fmt.Fprintf(&b, "* %s %s, CPU: %s, %d cores, %s GiB RAM\n", runtime.GOOS, runtime.GOARCH,
			cpu, runtime.NumCPU(), gib(mem))
	}
	fmt.Fprintf(&b, "* OS page size (= LMDB page size): %d\n", os.Getpagesize())
	fmt.Fprintf(&b, "* Go: %s\n", runtime.Version())
	return b.String()
}
