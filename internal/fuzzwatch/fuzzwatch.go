// Package fuzzwatch is the watchdog of the fuzz targets (FUZZING.md at the
// repository root). It crashes the process, with the stacks of every
// goroutine, when one fuzz input hangs or builds up too much heap: a stuck
// goroutine cannot be stopped, and a crash makes the fuzzer keep the input.
//
// Both limits measure the input, not the process:
//
//   - The heap limit applies to the growth of the heap since the input
//     began. In a normal go test, the seeds and the committed inputs run in
//     the process that has just run the package's other tests, and the heap
//     still holds what they left for the collector (up to the collector's
//     goal, which a test with a large live heap raises to gigabytes); the
//     size of the whole heap would blame the input for it.
//   - The hang limit applies only in a fuzzing worker process (go test
//     -fuzz), which runs nothing but fuzz inputs, one at a time. In a normal
//     go test the machine may be busy with the tests of other packages (or
//     the race detector, or an emulated architecture), so wall time says
//     little about the input there; go test's -timeout catches a hang
//     instead, also with every goroutine's stack.
//
// The package is used by tests only.
package fuzzwatch

import (
	"flag"
	"fmt"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"time"
)

// heapMetric is the heap occupied by objects, live or not yet swept.
const heapMetric = "/memory/classes/heap/objects:bytes"

// Fuzzing reports whether this process is a fuzzing worker: the testing
// package starts the workers of go test -fuzz with -test.fuzzworker (and
// runs no tests in them).
func Fuzzing() bool {
	f := flag.Lookup("test.fuzzworker")
	return f != nil && f.Value.String() == "true"
}

// Run runs fn, the code of one fuzz input, and crashes the process if fn
// runs longer than hang (in a fuzzing worker only; 0: no limit) or the heap
// grows by more than heap bytes while it runs (0: no limit).
func Run(hang time.Duration, heap uint64, fn func()) {
	if !Fuzzing() {
		hang = 0
	}
	run(hang, heap, fn)
}

// run is Run with the hang limit applied as given.
func run(hang time.Duration, heap uint64, fn func()) {
	if hang == 0 && heap == 0 {
		fn()
		return
	}
	sample := []metrics.Sample{{Name: heapMetric}}
	metrics.Read(sample)
	base := sample[0].Value.Uint64()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var deadline <-chan time.Time
		if hang > 0 {
			t := time.NewTimer(hang)
			defer t.Stop()
			deadline = t.C
		}
		var tick <-chan time.Time
		if heap > 0 {
			t := time.NewTicker(20 * time.Millisecond)
			defer t.Stop()
			tick = t.C
		}
		for {
			select {
			case <-done:
				return
			case <-deadline:
				debug.SetTraceback("all")
				panic(fmt.Sprintf("fuzz input still running after %v (hang)", hang))
			case <-tick:
				metrics.Read(sample)
				if v := sample[0].Value.Uint64(); v > base && v-base > heap {
					debug.SetTraceback("all")
					panic(fmt.Sprintf("fuzz input grew the heap by %d MiB (to %d MiB)", (v-base)>>20, v>>20))
				}
			}
		}
	}()
	defer func() {
		close(done)
		wg.Wait()
	}()
	fn()
}
