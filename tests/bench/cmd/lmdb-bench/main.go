// Command lmdb-bench runs rlmdb's benchmark workload groups on the cgo
// lmdb-go v1.9.4 (C LMDB 0.9.35) and on the Go implementation, in one
// process, interleaved: per round and group, each implementation runs the
// group (one warm-up and -reps timed repetitions of each workload, in a
// fresh environment), the one that goes first alternating; the samples of
// all rounds are pooled and printed as a markdown table with the ratio
// port/cgo. It is the Go form of rlmdb-bench's driver (rust/bench/src/
// main.rs). See ../../README.md.
//
//	lmdb-bench [-n N] [-reps R] [-rounds K] [-only g1,g2] [-impl both|cgo|port] [-dir D]
//	lmdb-bench -check-data [-n N] [-rlmdb-bench PATH]
//	lmdb-bench -ab OLD [-impl port] ...   (A/B of two builds of this program)
//	lmdb-bench -worker <impl> <group> <dir> <n> <reps> <sync_commits>
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	cgowl "github.com/PowerDNS/lmdb-go/tests/bench/cgo"
	"github.com/PowerDNS/lmdb-go/tests/bench/data"
	"github.com/PowerDNS/lmdb-go/tests/bench/run"
)

// impl is the workloads of one implementation in this binary.
type impl struct {
	name, desc string
	run        func(group string, p run.Params) ([]run.Result, error)
}

// impls are the implementations in this binary: cgo, and the port
// (port.go).
var impls = []impl{{cgowl.Impl, cgowl.Describe(), cgowl.Run}}

func findImpl(name string) (impl, bool) {
	for _, im := range impls {
		if im.name == name {
			return im, true
		}
	}
	return impl{}, false
}

type options struct {
	n, reps, syncCommits, rounds int
	dir                          string
	groups                       []string
	raw                          bool
	impl                         string
	checkData                    bool
	rlmdbBench, cbench           string
	ab                           string
}

func main() {
	// Write transactions run on the main goroutine (the workloads lock the
	// calling goroutine to its thread, as lmdb-go requires).
	if len(os.Args) > 1 && (os.Args[1] == "-worker" || os.Args[1] == "--worker") {
		os.Exit(worker(os.Args[2:]))
	}
	o := parseFlags()
	switch {
	case o.checkData:
		os.Exit(checkData(o))
	case o.ab != "":
		os.Exit(runAB(o))
	default:
		os.Exit(runInProcess(o))
	}
}

func usageExit(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "lmdb-bench: "+format+"\n", args...)
	flag.Usage()
	os.Exit(2)
}

func parseFlags() *options {
	o := &options{}
	flag.IntVar(&o.n, "n", 1_000_000, "number of keys (every other size scales with it)")
	flag.IntVar(&o.reps, "reps", 5, "timed repetitions per workload, after one warm-up")
	flag.IntVar(&o.syncCommits, "sync-commits", 1000, "commits of txn_sync")
	flag.IntVar(&o.rounds, "rounds", 1, "run every group this many times per implementation, alternating which goes first")
	flag.StringVar(&o.dir, "dir", filepath.Join(os.TempDir(), fmt.Sprintf("lmdb-bench-%d", os.Getpid())),
		"where to create the environments (a local file system; deleted afterwards)")
	only := flag.String("only", strings.Join(run.Groups, ","), "groups to run, comma-separated")
	flag.BoolVar(&o.raw, "raw", false, "also print the RESULT lines of every run")
	flag.StringVar(&o.impl, "impl", "both", "implementations: both, cgo or port (with -ab: the one both builds run, default port)")
	flag.BoolVar(&o.checkData, "check-data", false, "print the hash of the generated data (and compare with -rlmdb-bench, -cbench), and exit")
	flag.StringVar(&o.rlmdbBench, "rlmdb-bench", "", "with -check-data: rlmdb's rlmdb-bench to compare with")
	flag.StringVar(&o.cbench, "cbench", "", "with -check-data: rlmdb's cbench to compare with")
	flag.StringVar(&o.ab, "ab", "", "A/B: compare this build (new) with another build of lmdb-bench (base), both as subprocesses")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: lmdb-bench [options]\ngroups: %s\nimplementations in this binary:",
			strings.Join(run.Groups, " "))
		for _, im := range impls {
			fmt.Fprintf(flag.CommandLine.Output(), " %s", im.name)
		}
		fmt.Fprintln(flag.CommandLine.Output())
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		usageExit("unexpected argument %q", flag.Arg(0))
	}
	if o.n < 2000 || o.reps < 1 || o.reps > 64 {
		usageExit("need -n >= 2000 and 1 <= -reps <= 64")
	}
	o.rounds = max(o.rounds, 1)
	o.groups = strings.Split(*only, ",")
	for _, g := range o.groups {
		if !slices.Contains(run.Groups, g) {
			usageExit("unknown group %s", g)
		}
	}
	return o
}

// header is the first line of the table's caption.
func header(o *options) string {
	return fmt.Sprintf("n = %d, %d round(s) of %d timed reps after 1 warm-up", o.n, o.rounds, o.reps)
}

// printInfo prints the report's head: the machine and the sides. The
// environment directory goes to stderr (a local path, not for reports).
func printInfo(o *options, sides []string) {
	fmt.Fprintf(os.Stderr, "lmdb-bench: environments in %s\n", o.dir)
	fmt.Println("## lmdb-bench")
	fmt.Println()
	fmt.Print(run.MachineInfo())
	for _, s := range sides {
		fmt.Printf("* %s\n", s)
	}
	fmt.Println()
}

// runInProcess runs the groups on the implementations of this binary,
// interleaved, in this process.
func runInProcess(o *options) int {
	var sides []impl
	switch o.impl {
	case "both":
		sides = impls
		if len(impls) == 1 {
			fmt.Fprintln(os.Stderr, "lmdb-bench: this binary has no port: running cgo alone")
		}
	default:
		im, ok := findImpl(o.impl)
		if !ok {
			usageExit("-impl %s: not in this binary", o.impl)
		}
		sides = []impl{im}
	}
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "lmdb-bench: %v\n", err)
		return 1
	}
	var names, descs []string
	for _, s := range sides {
		names = append(names, s.name)
		descs = append(descs, s.name+": "+s.desc)
	}
	printInfo(o, descs)
	table := run.NewTable(names...)
	for round := 0; round < o.rounds; round++ {
		for gi, group := range o.groups {
			for _, k := range order(len(sides), gi+round) {
				s := sides[k]
				fmt.Fprintf(os.Stderr, "lmdb-bench: %s (%s) ...\n", group, s.name)
				p := run.Params{Dir: filepath.Join(o.dir, s.name+"-"+group), N: o.n, Reps: o.reps, SyncCommits: o.syncCommits}
				results, err := s.run(group, p)
				_ = os.RemoveAll(p.Dir)
				if err != nil {
					fmt.Fprintf(os.Stderr, "lmdb-bench: %v\n", err)
					return 1
				}
				if o.raw {
					for _, r := range results {
						fmt.Printf("%-5s %s\n", s.name, r.Line())
					}
				}
				table.Add(k, results)
			}
		}
	}
	_ = os.Remove(o.dir)
	table.Print(header(o))
	return 0
}

// order is the order in which n sides run a group: alternating which goes
// first (by group and round), so that slow drift (thermals, page cache,
// other load) does not always favour the same one.
func order(n, k int) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	if k%2 == 1 {
		slices.Reverse(idx)
	}
	return idx
}

// worker runs one group of one implementation and prints its RESULT lines
// (cbench's and rlmdb-bench's worker protocol, plus allocations): the
// subprocesses of -ab. The pseudo-groups info and datacheck print a
// description of the implementation and the DATA hash.
func worker(args []string) int {
	if len(args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: lmdb-bench -worker <impl> <group> <dir> <n> <reps> <sync_commits>")
		return 2
	}
	im, ok := findImpl(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "lmdb-bench: -worker %s: not in this binary\n", args[0])
		return 2
	}
	var num [3]int
	for k := range num {
		v, err := strconv.Atoi(args[3+k])
		if err != nil {
			fmt.Fprintf(os.Stderr, "lmdb-bench: %v\n", err)
			return 2
		}
		num[k] = v
	}
	switch args[1] {
	case "info":
		fmt.Printf("INFO %s, %s\n", im.desc, runtime.Version())
		return 0
	case "datacheck":
		fmt.Printf("DATA %016x\n", data.Datacheck(num[0]))
		return 0
	}
	results, err := im.run(args[1], run.Params{Dir: args[2], N: num[0], Reps: num[1], SyncCommits: num[2]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "lmdb-bench: %v\n", err)
		return 1
	}
	for _, r := range results {
		fmt.Println(r.Line())
	}
	return 0
}

// sub runs a worker subprocess (argv, then args) and returns its stdout;
// its stderr goes to ours.
func sub(argv []string, args ...string) (string, error) {
	cmd := exec.Command(argv[0], append(slices.Clone(argv[1:]), args...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return string(out), err
}

// runAB compares two builds of lmdb-bench on one implementation (-impl,
// default port): base (-ab) and new (this executable), both as worker
// subprocesses, interleaved like runInProcess.
func runAB(o *options) int {
	imp := o.impl
	if imp == "both" {
		imp = "port"
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lmdb-bench: %v\n", err)
		return 1
	}
	sides := [][]string{{o.ab, "-worker", imp}, {self, "-worker", imp}}
	names := []string{"base", "new"}
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "lmdb-bench: %v\n", err)
		return 1
	}
	var descs []string
	for k, s := range sides {
		info, err := sub(s, "info", o.dir, "2000", "1", "1")
		if err != nil {
			fmt.Fprintf(os.Stderr, "lmdb-bench: %s -worker %s info: %v\n", s[0], imp, err)
			return 1
		}
		descs = append(descs, fmt.Sprintf("%s = %s: %s", names[k], s[0], strings.TrimPrefix(strings.TrimSpace(info), "INFO ")))
	}
	printInfo(o, descs)
	table := run.NewTable(names...)
	for round := 0; round < o.rounds; round++ {
		for gi, group := range o.groups {
			for _, k := range order(2, gi+round) {
				dir := filepath.Join(o.dir, names[k]+"-"+group)
				fmt.Fprintf(os.Stderr, "lmdb-bench: %s (%s) ...\n", group, names[k])
				out, err := sub(sides[k], group, dir, strconv.Itoa(o.n), strconv.Itoa(o.reps), strconv.Itoa(o.syncCommits))
				_ = os.RemoveAll(dir)
				if err != nil {
					fmt.Fprintf(os.Stderr, "lmdb-bench: the %s worker for %s failed: %v\n", names[k], group, err)
					return 1
				}
				var results []run.Result
				for _, line := range strings.Split(out, "\n") {
					if r, ok := run.ParseLine(line); ok {
						results = append(results, r)
						if o.raw {
							fmt.Printf("%-5s %s\n", names[k], line)
						}
					}
				}
				table.Add(k, results)
			}
		}
	}
	_ = os.Remove(o.dir)
	table.Print(header(o) + " (" + imp + ")")
	return 0
}

// checkData prints the hash of everything the data generator produces for
// -n (data.Datacheck) and compares it with the datacheck group of
// rlmdb-bench's worker and of rlmdb's cbench, if given: equal hashes mean
// the Go, Rust and C sides generate the same data.
func checkData(o *options) int {
	type check struct {
		name string
		argv []string
	}
	var checks []check
	if o.rlmdbBench != "" {
		checks = append(checks, check{"rlmdb-bench", []string{o.rlmdbBench, "--worker"}})
	}
	if o.cbench != "" {
		checks = append(checks, check{"cbench", []string{o.cbench}})
	}
	goLine := fmt.Sprintf("DATA %016x", data.Datacheck(o.n))
	fmt.Printf("%-12s %s\n", "Go:", goLine)
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "lmdb-bench: %v\n", err)
		return 1
	}
	defer os.RemoveAll(o.dir)
	same := true
	for _, c := range checks {
		dir := filepath.Join(o.dir, "check-"+c.name)
		out, err := sub(c.argv, "datacheck", dir, strconv.Itoa(o.n), "1", "1")
		line := ""
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "DATA ") {
				line = sc.Text()
			}
		}
		if err != nil || line == "" {
			fmt.Printf("%-12s failed: %v\n", c.name+":", err)
			same = false
			continue
		}
		fmt.Printf("%-12s %s\n", c.name+":", line)
		same = same && line == goLine
	}
	if !same {
		fmt.Println("lmdb-bench: the data generators differ")
		return 1
	}
	if len(checks) > 0 {
		fmt.Printf("same data (n = %d)\n", o.n)
	}
	return 0
}
