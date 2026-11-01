// Running the C peers (rlmdb interop's peer protocol, interop/README.md)
// and Go helper processes (this test binary re-executed).

package lockfiletest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/lockfile"
	"github.com/PowerDNS/lmdb-go/tests/internal/coracle"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("LOCKFILETEST_HELPER"); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

// oracleDir returns the directory of the C binaries, or skips the test.
func oracleDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("LMDB_ORACLE_BIN"); d != "" {
		return d
	}
	if d := coracle.InteropBin(os.Getenv("RLMDB_DIR"), "cpeer10"); d != "" {
		return d
	}
	t.Skip("C oracles not found: set LMDB_ORACLE_BIN (scripts/build-c-oracles.sh, cmd/build-c-oracles) or RLMDB_DIR")
	return ""
}

// cside is the C side of one data format.
type cside struct {
	format            format.Format
	layout            *lockfile.Layout
	peer, stat, ldump string // cpeer, mdb_stat, layout_dump (or their 1.0 builds)
	otherLayout       *lockfile.Layout
}

func sides(t *testing.T) []cside {
	t.Helper()
	d := oracleDir(t)
	probe, err := lockfile.Open(filepath.Join(t.TempDir(), "lock.mdb"), 0o644, 1, false, lockfile.LockV09)
	if errors.Is(err, lockfile.ErrNoSharedLocks) {
		t.Fatal("tests/lockfile needs shared locking: on Linux a cgo build (CGO_ENABLED=1; cross-GOARCH builds default to 0)")
	}
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()
	bin := func(name string) string { return filepath.Join(d, coracle.Exe(name)) }
	return []cside{
		{format.V09, lockfile.LockV09, bin("cpeer"), bin("mdb_stat"), bin("layout_dump"), lockfile.LockV10},
		{format.V10, lockfile.LockV10, bin("cpeer10"), bin("mdb_stat10"), bin("layout_dump10"), lockfile.LockV09},
	}
}

// run runs a C program to completion and returns its stdout and exit code.
func run(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s %v: %v", filepath.Base(bin), args, err)
	}
	if code != 0 {
		t.Logf("%s %v: exit %d: %s", filepath.Base(bin), args, code, strings.TrimSpace(errb.String()))
	}
	return out.String() + errb.String(), code
}

// mustRun runs a C program that must succeed.
func mustRun(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, code := run(t, bin, args...)
	if code != 0 {
		t.Fatalf("%s %v failed (exit %d): %s", filepath.Base(bin), args, code, out)
	}
	return out
}

// field returns the value of key=value in a peer's output line.
func field(t *testing.T, out, key string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		for _, tok := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(tok, key+"="); ok {
				return v
			}
		}
	}
	t.Fatalf("no %s= in %q", key, out)
	return ""
}

func atoi(t *testing.T, s string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// newEnv creates an environment with some data with C and returns its
// path and last txnid.
func (c cside) newEnv(t *testing.T) (string, uint64) {
	t.Helper()
	env := filepath.Join(t.TempDir(), "env")
	if err := os.Mkdir(env, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, c.peer, "create", env, "67108864", "4")
	mustRun(t, c.peer, "fill", env, "-", "20", "1")
	return env, c.lastTxnID(t, env)
}

// lastTxnID is the environment's last committed txnid, as C sees it.
func (c cside) lastTxnID(t *testing.T, env string) uint64 {
	t.Helper()
	return atoi(t, field(t, mustRun(t, c.peer, "stat", env), "last_txnid"))
}

// holdSecs is the deadline of the C peers' hold-* commands that the tests
// start with --until-stdin (rlmdb interop hold_secs, 3df6995). The test
// releases them; the deadline only ends the hold of a test that is gone.
// It must outlast the slowest test (emulated runs take minutes): a
// hold-env keeper that left early lets the next opener of the
// environment reset the reader table.
const holdSecs = "600"

// proc is a running process with a READY line protocol: C peers'
// hold-reader, hold-writer, hold-env (interop/README.md), or a Go helper.
type proc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	ready string // the READY line
	done  chan struct{}
	err   error
}

func spawn(t *testing.T, cmd *exec.Cmd) *proc {
	t.Helper()
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
	p := &proc{cmd: cmd, stdin: stdin, lines: make(chan string, 16), done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
		p.err = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(p.kill)
	return p
}

// waitReady waits up to d for the READY line; false on timeout.
func (p *proc) waitReady(t *testing.T, d time.Duration) bool {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				<-p.done
				t.Fatalf("%s exited before READY: %v", p.cmd.Path, p.err)
			}
			if strings.HasPrefix(line, "READY") {
				p.ready = line
				return true
			}
		case <-timeout:
			return false
		}
	}
}

func (p *proc) mustBeReady(t *testing.T) *proc {
	t.Helper()
	if !p.waitReady(t, 30*time.Second) {
		t.Fatalf("%s not ready", p.cmd.Path)
	}
	return p
}

func (p *proc) pid() int { return p.cmd.Process.Pid }

// release ends a --until-stdin wait and waits for a clean exit.
func (p *proc) release(t *testing.T) {
	t.Helper()
	p.stdin.Close()
	<-p.done
	if p.err != nil {
		t.Fatalf("%s: %v", p.cmd.Path, p.err)
	}
}

// exited reports whether the process ended within d.
func (p *proc) exited(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}

// kill kills the process with SIGKILL, as a crash would.
func (p *proc) kill() {
	select {
	case <-p.done:
	default:
		p.cmd.Process.Kill()
		<-p.done
	}
}

// startC starts a C peer command that prints READY.
func startC(t *testing.T, bin string, args ...string) *proc {
	t.Helper()
	return spawn(t, exec.Command(bin, args...))
}

// startGo starts this test binary as a Go helper (runHelper) on the lock
// file of env, in the lock-file version of l.
func startGo(t *testing.T, mode, env string, l *lockfile.Layout) *proc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LOCKFILETEST_HELPER="+mode,
		"LOCKFILETEST_LOCK="+filepath.Join(env, "lock.mdb"),
		fmt.Sprintf("LOCKFILETEST_VERSION=%d", l.Version))
	return spawn(t, cmd)
}

// runHelper is a Go process using the lock file: it attaches to the lock
// file (which a C process or the test holds open, so it is never the
// first opener), does what mode says, prints READY and waits for stdin.
func runHelper(mode string) int {
	l := lockfile.LockV09
	if os.Getenv("LOCKFILETEST_VERSION") == "2" {
		l = lockfile.LockV10
	}
	r, err := lockfile.Open(os.Getenv("LOCKFILETEST_LOCK"), 0o644, 126, false, l)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open:", err)
		return 3
	}
	if r.IsExclusive() {
		fmt.Fprintln(os.Stderr, "helper: unexpectedly the first opener")
		return 3
	}
	switch mode {
	case "hold-reader":
		i, err := r.AcquireSlot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: slot:", err)
			return 3
		}
		fmt.Printf("READY txnid=%d pid=%d\n", r.PublishCurrentTxnID(i), os.Getpid())
	case "hold-writer":
		if err := r.LockWriter(func() (uint64, error) { return r.TxnID(), nil }); err != nil {
			fmt.Fprintln(os.Stderr, "helper: writer:", err)
			return 3
		}
		defer r.UnlockWriter()
		fmt.Printf("READY txnid=%d pid=%d\n", r.TxnID(), os.Getpid())
	default:
		return 2
	}
	bufio.NewReader(os.Stdin).ReadString('\n')
	return 0
}

// readersInC returns the reader slots C sees ("cpeer readers" lines:
// pid -> txnid, "-" for an idle slot).
func readersInC(t *testing.T, peer, env string) map[int]string {
	t.Helper()
	m := map[int]string{}
	for _, line := range strings.Split(mustRun(t, peer, "readers", env), "\n") {
		if !strings.HasPrefix(line, "reader ") {
			continue
		}
		pid := int(atoi(t, field(t, line, "pid")))
		m[pid] = field(t, line, "txnid")
	}
	return m
}

// readersInMdbStat returns the pids of mdb_stat -r's reader table.
func readersInMdbStat(t *testing.T, stat, env string) map[int]bool {
	t.Helper()
	out, _ := run(t, stat, "-r", env) // exits 1 on success (KNOWN_C_BUGS.md #2)
	m := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 {
			if pid, err := strconv.Atoi(f[0]); err == nil {
				m[pid] = true
			}
		}
	}
	return m
}

func openGo(t *testing.T, env string, l *lockfile.Layout) *lockfile.Region {
	t.Helper()
	r, err := lockfile.Open(filepath.Join(env, "lock.mdb"), 0o644, 126, false, l)
	if err != nil || r == nil {
		t.Fatalf("lockfile.Open: %v", err)
	}
	return r
}
