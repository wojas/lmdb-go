package lmdb

// Helper processes: tests that need several processes on one environment
// run this test binary again (exec.Command(os.Args[0])) with
// LMDB_TEST_HELPER naming what to do. Not in lmdb-go v1's tests.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("LMDB_TEST_HELPER"); mode != "" {
		os.Exit(runHelper(mode, os.Getenv("LMDB_TEST_HELPER_PATH")))
	}
	// Keep the main thread to this goroutine, so no test goroutine runs on
	// it: Go never ends the main thread, so a writer mutex that a goroutine
	// holds there when it exits is never released
	// (TestTxn_Commit_wrongThread).
	runtime.LockOSThread()
	os.Exit(m.Run())
}

// runHelper opens the environment at path (with its lock file) and does
// what mode says:
//   - hold-env prints READY, waits for a line on stdin, then writes a key
//     and prints RESULT and the Update's error;
//   - hold-writer prints READY inside an Update, and waits for stdin to
//     close (the test kills it there, as a crash would).
func runHelper(mode, path string) int {
	env, err := NewEnv()
	if err == nil {
		err = env.Open(path, 0, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open:", err)
		return 3
	}
	defer env.Close()
	stdin := bufio.NewReader(os.Stdin)
	switch mode {
	case "hold-env":
		fmt.Println("READY")
		stdin.ReadString('\n')
		err := env.Update(func(txn *Txn) error { return txn.Put(1, []byte("helper"), []byte("yes"), 0) })
		fmt.Println("RESULT", err)
	case "hold-writer":
		err := env.Update(func(*Txn) error {
			fmt.Println("READY")
			_, err := io.Copy(io.Discard, stdin)
			return err
		})
		fmt.Println("RESULT", err)
	default:
		return 2
	}
	return 0
}

// helperProc is a running helper process.
type helperProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	done  chan struct{}
}

// startHelper starts a helper in mode on the environment at path and
// waits for its READY line.
func startHelper(t *testing.T, mode, path string) *helperProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LMDB_TEST_HELPER="+mode, "LMDB_TEST_HELPER_PATH="+path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helperProc{cmd: cmd, stdin: stdin, lines: make(chan string, 4), done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
		close(h.lines)
		cmd.Wait()
		close(h.done)
	}()
	t.Cleanup(h.kill)
	if line := h.line(t, 30*time.Second); line != "READY" {
		t.Fatalf("helper %s: %q", mode, line)
	}
	return h
}

// line is the helper's next output line.
func (h *helperProc) line(t *testing.T, d time.Duration) string {
	t.Helper()
	select {
	case line, ok := <-h.lines:
		if !ok {
			t.Fatal("helper exited")
		}
		return strings.TrimSpace(line)
	case <-time.After(d):
		t.Fatal("helper: no output")
	}
	return ""
}

// kill kills the helper with SIGKILL, as a crash would.
func (h *helperProc) kill() {
	select {
	case <-h.done:
	default:
		h.cmd.Process.Kill()
		<-h.done
	}
}

// waitGoroutine waits until a goroutine of this process runs fn (a part
// of a function name in its stack) in a wait whose state (the "[...]" of
// its stack header, such as "sync.Mutex.Lock") contains state; "" for any
// state.
func waitGoroutine(t *testing.T, fn, state string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if goroutineWaits(fn, state) {
			return
		}
	}
	t.Fatalf("no goroutine waits in %s (%s)", fn, state)
}

// goroutineWaits reports whether a goroutine runs fn in a wait whose state
// contains state (see waitGoroutine).
func goroutineWaits(fn, state string) bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if strings.Contains(g, fn) && strings.Contains(header, "["+state) {
			return true
		}
	}
	return false
}

// allStacks is the stack of every goroutine, for a test that hangs.
func allStacks() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}
