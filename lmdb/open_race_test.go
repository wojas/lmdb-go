//go:build darwin || openbsd || netbsd || dragonfly

package lmdb

// Opening an environment while its last user closes it, on the platforms
// whose lock objects the last user removes: POSIX named semaphores (macOS;
// OpenBSD, NetBSD and DragonFly with 0.9 lock files) and SysV semaphore sets
// (those three BSDs with 1.0 lock files). rlmdb tests/lockfile_open_race.rs.
//
// The last user takes byte 0 of the lock file exclusively and removes the
// objects (mdb_env_close0). An opener whose exclusive probe failed because
// that process still held its lock waits for a shared lock, gets it once the
// closer has gone, and finds the lock file naming objects that no longer
// exist. C fails the open there (rlmdb KNOWN_C_BUGS.md #47); here the open
// goes back to the exclusive probe (lockfile.OpenClaimed).
//
// Several processes (this test binary run again) open the environment, begin
// a read transaction (which takes the reader lock) and close it, over and
// over; every openRaceWriteEvery-th time they increment a counter in a write
// transaction instead. Every open must succeed, and the counter must end at
// the number of increments: if two processes ever used two different sets of
// semaphores, their write transactions could overlap and lose increments.
//
// Knob: OPEN_RACE_ITERATIONS=N opens per worker.

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// openRaceWorkerVar is set (to <format>|<iterations>|<dir>) in the
	// environment of a worker.
	openRaceWorkerVar = "LMDB_TEST_OPEN_RACE_WORKER"
	// openRaceWorkers is the number of worker processes per format.
	openRaceWorkers = 4
	// openRaceWriteEvery: every this many opens, starting with the first, a
	// worker writes.
	openRaceWriteEvery = 8
)

var openRaceCounter = []byte("counter")

func init() {
	if spec := os.Getenv(openRaceWorkerVar); spec != "" {
		os.Exit(openRaceWorker(spec))
	}
}

// openRaceOpen opens the environment at dir in format f.
func openRaceOpen(dir string, f Format) (*Env, error) {
	env, err := NewEnv()
	if err != nil {
		return nil, err
	}
	if err = env.SetFormat(f); err == nil {
		err = env.SetMapSize(16 << 20)
	}
	if err == nil {
		err = env.Open(dir, NoSync, 0o644)
	}
	if err != nil {
		env.Close()
		return nil, err
	}
	return env, nil
}

// openRaceIteration is one iteration of a worker: open, read or increment
// the counter, close.
func openRaceIteration(dir string, f Format, write bool) error {
	env, err := openRaceOpen(dir, f)
	if err != nil {
		return err
	}
	defer env.Close()
	if !write {
		return env.View(func(*Txn) error { return nil })
	}
	return env.Update(func(txn *Txn) error {
		var n uint64
		v, err := txn.Get(1, openRaceCounter)
		switch {
		case err == nil:
			n = binary.NativeEndian.Uint64(v)
		case !IsNotFound(err):
			return err
		}
		return txn.Put(1, openRaceCounter, binary.NativeEndian.AppendUint64(nil, n+1), 0)
	})
}

// openRaceWorker is the body of a worker process.
func openRaceWorker(spec string) int {
	parts := strings.SplitN(spec, "|", 3)
	f := V09
	if parts[0] == "1.0" {
		f = V10
	}
	iterations, _ := strconv.Atoi(parts[1])
	for i := 0; i < iterations; i++ {
		if err := openRaceIteration(parts[2], f, i%openRaceWriteEvery == 0); err != nil {
			fmt.Fprintf(os.Stderr, "pid %d iteration %d: %v\n", os.Getpid(), i, err)
			return 2
		}
	}
	return 0
}

func TestOpenCloseRace(t *testing.T) {
	iterations := 1000
	if n, err := strconv.Atoi(os.Getenv("OPEN_RACE_ITERATIONS")); err == nil {
		iterations = n
	}
	if testing.Short() {
		iterations = 200
	}
	for _, f := range []Format{V09, V10} {
		t.Run(f.String(), func(t *testing.T) {
			dir := t.TempDir()
			env, err := openRaceOpen(dir, f)
			if err != nil {
				t.Fatal(err)
			}
			env.Close()

			var wg sync.WaitGroup
			errs := make([]string, openRaceWorkers)
			for w := range errs {
				cmd := exec.Command(os.Args[0], "-test.run=^$")
				cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s|%d|%s", openRaceWorkerVar, f, iterations, dir))
				wg.Add(1)
				go func() {
					defer wg.Done()
					out, err := cmd.CombinedOutput()
					if err != nil {
						errs[w] = fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out)))
					}
				}()
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Minute):
				t.Fatal("workers still running after five minutes")
			}
			for _, e := range errs {
				if e != "" {
					t.Error(e)
				}
			}
			if t.Failed() {
				return
			}
			env, err = openRaceOpen(dir, f)
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			writes := uint64(openRaceWorkers * ((iterations + openRaceWriteEvery - 1) / openRaceWriteEvery))
			err = env.View(func(txn *Txn) error {
				v, err := txn.Get(1, openRaceCounter)
				if err != nil {
					return err
				}
				if n := binary.NativeEndian.Uint64(v); n != writes {
					return fmt.Errorf("counter %d, want %d", n, writes)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
