package fuzzwatch

import (
	"flag"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// The testing package still has the flag that marks a fuzzing worker (a
// Go release that renames it would silently turn the hang limit off).
func TestFuzzingFlag(t *testing.T) {
	if flag.Lookup("test.fuzzworker") == nil {
		t.Fatal("no -test.fuzzworker flag: Fuzzing cannot tell a fuzzing worker")
	}
	if Fuzzing() {
		t.Fatal("Fuzzing in a normal test")
	}
}

// The testing package still has -test.fuzz (TuneGC's sign of go test
// -fuzz), and TuneGC leaves the collector alone in a normal test.
func TestTuneGCOnlyWhenFuzzing(t *testing.T) {
	if flag.Lookup("test.fuzz") == nil {
		t.Fatal("no -test.fuzz flag: FuzzingProcess cannot tell go test -fuzz")
	}
	if FuzzingProcess() {
		t.Fatal("FuzzingProcess in a normal test")
	}
	before := debug.SetGCPercent(-1)
	debug.SetGCPercent(before)
	TuneGC()
	if after := debug.SetGCPercent(before); after != before {
		t.Fatalf("TuneGC changed GOGC from %d to %d in a normal test", before, after)
	}
}

// The testing package still has -test.fuzzminimizetime, and LimitMinimize
// leaves it alone in a normal test.
func TestLimitMinimizeOnlyWhenFuzzing(t *testing.T) {
	f := flag.Lookup(minimizeFlag)
	if f == nil {
		t.Fatal("no -test.fuzzminimizetime flag: LimitMinimize cannot set it")
	}
	before := f.Value.String()
	if err := LimitMinimize(30); err != nil {
		t.Fatal(err)
	}
	if after := f.Value.String(); after != before {
		t.Fatalf("LimitMinimize changed -test.fuzzminimizetime from %s to %s in a normal test", before, after)
	}
}

// keep holds memory live across a watched input.
var keep [][]byte

// crashCase runs as a subprocess of TestCrashes: one watched input that
// must crash the process.
func crashCase(name string) {
	switch name {
	case "hang":
		run(50*time.Millisecond, 0, func() { time.Sleep(time.Minute) })
	case "heap":
		run(0, 16<<20, func() {
			keep = append(keep, make([]byte, 64<<20))
			time.Sleep(time.Minute)
		})
	}
}

func TestMain(m *testing.M) {
	if name := os.Getenv("FUZZWATCH_CRASH"); name != "" {
		crashCase(name)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// An input that hangs (in a fuzzing worker) or grows the heap past the
// limit crashes the process with every goroutine's stack.
func TestCrashes(t *testing.T) {
	for name, want := range map[string]string{
		"hang": "fuzz input still running after 50ms (hang)",
		"heap": "fuzz input grew the heap by",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "FUZZWATCH_CRASH="+name)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("no crash:\n%s", out)
			}
			if !strings.Contains(string(out), want) || !strings.Contains(string(out), "goroutine ") {
				t.Fatalf("crash without %q and the goroutines' stacks:\n%s", want, out)
			}
		})
	}
}

// The limits measure the input: a heap that is large when the input
// begins does not count, and outside a fuzzing worker an input may run
// longer than the hang limit.
func TestMeasuresTheInput(t *testing.T) {
	keep = append(keep, make([]byte, 64<<20))
	defer func() { keep = nil }()
	Run(10*time.Millisecond, 16<<20, func() {
		b := make([]byte, 1<<20)
		time.Sleep(100 * time.Millisecond)
		runtime.KeepAlive(b)
	})
}
