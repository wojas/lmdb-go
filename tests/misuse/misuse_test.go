package misuse

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "github.com/PowerDNS/lmdb-go/tests/misuse/cgo"
	port "github.com/PowerDNS/lmdb-go/tests/misuse/port"
)

// How a case is compared with lmdb-go v1 (the cgo reference).
type kind int

const (
	// agree: v1 has a defined result there, and the port returns the
	// same. Both run in this process.
	agree kind = iota
	// v1Fails: v1 crashes (C dereferences a NULL or freed handle), panics
	// (Go dereferences the freed transaction's buffers) or returns an
	// error, depending on what the freed memory holds; the port returns
	// EINVAL. v1 runs in a process of its own with an environment of its
	// own (a process that dies in a write transaction would leave the
	// writer lock taken: a named semaphore on macOS stays taken).
	v1Fails
	// v1Undefined: v1 uses freed memory and may even succeed (macOS: a
	// cursor of a committed write transaction writes into the next one, a
	// cursor of an aborted read-only one counts duplicates); it is not
	// run. The port returns EINVAL.
	v1Undefined
	// differs: a difference lmdb/DEVIATIONS.md lists, where v1's result
	// is defined in this test (no writes between the cursor's snapshot and
	// its use): both run in this process, each with its own result.
	differs
)

// expectation is what a case returns: want on the port, and on v1 want
// too (agree) or wantV1 (differs).
type expectation struct {
	kind   kind
	want   string
	wantV1 string
}

// Shorthands for the expectations.
func same(want string) expectation         { return expectation{kind: agree, want: want} }
func v1fails(want string) expectation      { return expectation{kind: v1Fails, want: want} }
func undefined(want string) expectation    { return expectation{kind: v1Undefined, want: want} }
func deviates(v1, port string) expectation { return expectation{kind: differs, want: port, wantV1: v1} }

// expected are the cases of ../misuse/cgo (Cases) and what they return.
var expected = map[string]expectation{
	"closed-ro/Get-First":     v1fails("err mdb_cursor_get: EINVAL"),
	"closed-ro/Get-Set":       v1fails("err mdb_cursor_get: EINVAL"),
	"closed-ro/Count":         same("err mdb_cursor_count: EINVAL"),
	"closed-ro/Renew":         same("err mdb_cursor_renew: EINVAL"),
	"closed-ro/DBI":           same("4294967295"),
	"closed-ro/Close":         same("ok"),
	"closed-rw/Get-First":     v1fails("err mdb_cursor_get: EINVAL"),
	"closed-rw/Put":           same("err mdb_cursor_put: EINVAL"),
	"closed-rw/Del":           v1fails("err mdb_cursor_del: EINVAL"),
	"rw-commit/Get-First":     v1fails("err mdb_cursor_get: EINVAL"),
	"rw-commit/Put":           v1fails("err mdb_cursor_put: EINVAL"),
	"rw-commit/Del":           v1fails("err mdb_cursor_del: EINVAL"),
	"rw-commit/Count":         v1fails("err mdb_cursor_count: EINVAL"),
	"rw-commit/Close":         same("ok"),
	"rw-commit/Put-into-next": undefined("err mdb_cursor_put: EINVAL"),
	"rw-abort/Get-First":      v1fails("err mdb_cursor_get: EINVAL"),
	"rw-abort/Put":            v1fails("err mdb_cursor_put: EINVAL"),

	"ro-abort/Get-First":         v1fails("err mdb_cursor_get: EINVAL"),
	"ro-commit/Get-Next":         v1fails("err mdb_cursor_get: EINVAL"),
	"ro-abort/Count":             undefined("err mdb_cursor_count: EINVAL"),
	"ro-abort/Get-after-new-txn": v1fails("err mdb_cursor_get: EINVAL"),
	"ro-abort/Renew-then-Get":    same("ok a=va"),
	"ro-abort/Close":             same("ok"),
	"ro-copy-closed/Get":         undefined("err mdb_cursor_get: EINVAL; other cursor: ok c=vc"),

	"ro-reset/Get-First":    same("err mdb_cursor_get: MDB_BAD_TXN"),
	"ro-reset/Count":        same("err mdb_cursor_count: MDB_BAD_TXN"),
	"ro-reset/Count-nodup":  same("err mdb_cursor_count: MDB_INCOMPATIBLE"),
	"ro-reset/Put":          same("err mdb_cursor_put: EACCES"),
	"ro-reset/Del":          same("err mdb_cursor_del: EACCES"),
	"ro-reset/Cursor-Renew": same("err mdb_cursor_renew: EINVAL"),
	"ro-reset/Renew-other":  same("ok a=va"),
	"ro-reset/Close":        same("ok"),
	// After Reset and Renew a cursor that was not renewed starts over
	// unpositioned; v1 kept its pages of the old snapshot.
	"ro-reset-renew/Get-Next":              deviates("ok b=vb", "ok a=va"),
	"ro-reset-renew/Get-Current":           deviates("ok a=va", "err mdb_cursor_get: EINVAL"),
	"ro-reset-renew/Cursor-Renew-Get-Next": same("ok a=va"),
	"ro-renew-active/Get-Next":             same("err mdb_txn_renew: EINVAL; ok b=vb"),
	"ro-active/Put":                        same("err mdb_cursor_put: EACCES"),
	"rw-reset/Get-Next":                    same("ok b=vb"),
	"rw-reset/Put":                         same("ok"),

	"txn-ro-abort/Get":        v1fails("err mdb_get: EINVAL"),
	"txn-ro-abort/OpenCursor": same("err mdb_cursor_open: EINVAL"),
	"txn-ro-abort/Renew":      same("err mdb_txn_renew: EINVAL"),
	"txn-ro-abort/Reset":      same("ok"),
	"txn-ro-abort/ID":         same("0"),

	"txn-ro-reset/Get":         same("err mdb_get: EINVAL"),
	"txn-ro-reset/Put":         same("err mdb_put: EINVAL"),
	"txn-ro-reset/Del":         same("err mdb_del: EINVAL"),
	"txn-ro-reset/Drop":        same("err mdb_drop: EINVAL"),
	"txn-ro-reset/Stat":        same("err mdb_stat: EINVAL"),
	"txn-ro-reset/Flags":       same("err mdb_dbi_flags: EINVAL"),
	"txn-ro-reset/OpenCursor":  same("err mdb_cursor_open: EINVAL"),
	"txn-ro-reset/OpenDBI":     same("err mdb_dbi_open: MDB_BAD_TXN"),
	"txn-ro-reset/OpenRoot":    same("err mdb_dbi_open: MDB_BAD_TXN"),
	"txn-ro-reset/Commit":      same("ok"),
	"txn-ro-reset/Reset-again": same("ok"),
	// Txn.ID is 0 while reset; v1 kept the snapshot's id.
	"txn-ro-reset/ID":     deviates("the snapshot's", "0"),
	"txn-ro/Renew-active": same("err mdb_txn_renew: EINVAL"),

	"txn-rw-commit/Get":        same("err mdb_get: EINVAL"),
	"txn-rw-commit/Put":        same("err mdb_put: EINVAL"),
	"txn-rw-commit/PutReserve": same("err mdb_put: EINVAL"),
	"txn-rw-commit/Del":        same("err mdb_del: EINVAL"),
	"txn-rw-commit/Drop":       same("err mdb_drop: EINVAL"),
	"txn-rw-commit/Stat":       same("err mdb_stat: EINVAL"),
	"txn-rw-commit/Flags":      same("err mdb_dbi_flags: EINVAL"),
	"txn-rw-commit/OpenCursor": same("err mdb_cursor_open: EINVAL"),
	"txn-rw-commit/OpenDBI":    v1fails("err mdb_dbi_open: EINVAL"),
	// v1 began a new top-level write transaction (mdb_txn_begin with a
	// NULL parent) and committed it.
	"txn-rw-commit/Sub":    deviates("ok", "err mdb_txn_begin: EINVAL"),
	"txn-rw-commit/Commit": same("err mdb_txn_commit: EINVAL"),
	"txn-rw-commit/Abort":  same("ok"),
	"txn-rw-abort/Put":     same("err mdb_put: EINVAL"),
}

// The environment variables that make the test binary run one case of v1
// (v1.Run): its name, and the directory of its environment.
const (
	helperCase = "MISUSE_V1_CASE"
	helperDir  = "MISUSE_V1_DIR"
)

func TestMain(m *testing.M) {
	if name := os.Getenv(helperCase); name != "" {
		runtime.LockOSThread()
		os.Exit(v1.Run(os.Getenv(helperDir), name))
	}
	os.Exit(m.Run())
}

// names are the cases of kind k (all for k < 0), sorted.
func names(k kind) []string {
	var out []string
	for name, e := range expected {
		if k < 0 || e.kind == k {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// TestMisuseCases checks that both implementations have exactly the cases
// of expected (the port's are generated from v1's).
func TestMisuseCases(t *testing.T) {
	for name := range expected {
		if v1.Cases[name] == nil || port.Cases[name] == nil {
			t.Errorf("case %s: missing", name)
		}
	}
	for name := range v1.Cases {
		if _, ok := expected[name]; !ok {
			t.Errorf("case %s: no expectation", name)
		}
	}
	if len(port.Cases) != len(v1.Cases) {
		t.Errorf("%d cases on the port, %d on v1", len(port.Cases), len(v1.Cases))
	}
}

// TestMisuse runs every case on the port, and on v1 those it agrees or
// differs on, in this process.
func TestMisuse(t *testing.T) {
	// Write transactions: one OS thread (not subtests, which run on
	// goroutines of their own).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pe, err := port.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pe.Close()
	ve, err := v1.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ve.Close()
	for _, name := range names(-1) {
		e := expected[name]
		if got := port.Cases[name](pe); got != e.want {
			t.Errorf("%s: port: %s, want %s", name, got, e.want)
		}
		switch e.kind {
		case agree:
			if got := v1.Cases[name](ve); got != e.want {
				t.Errorf("%s: v1: %s, want %s (as the port)", name, got, e.want)
			}
		case differs:
			if got := v1.Cases[name](ve); got != e.wantV1 {
				t.Errorf("%s: v1: %s, want %s", name, got, e.wantV1)
			}
		}
	}
}

// TestMisuseV1Fails runs the cases where v1 crashes on v1, each in a
// process of its own (the test binary, v1.Run), and checks that it does
// not succeed: it dies or returns an error. Which of them it does depends
// on C's allocator, so it is not checked.
func TestMisuseV1Fails(t *testing.T) {
	if runtime.GOOS == "windows" {
		// A crash in C code is a Windows exception there, which this
		// test does not cover.
		t.Skip("v1's crashes are observed on Unix only")
	}
	for _, name := range names(v1Fails) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, crash := runV1(t, name)
			if crash == "" && !strings.HasPrefix(out, "err ") {
				t.Errorf("v1 succeeded: %s", out)
			}
			if crash != "" {
				t.Logf("v1 crashed: %s", crash)
			} else {
				t.Logf("v1: %s", out)
			}
		})
	}
}

// runV1 runs the case name of v1 in a process of its own and returns
// what it printed, or a line saying how it crashed (a panic or a signal,
// as the Go runtime reports both).
func runV1(t *testing.T, name string) (out, crash string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperCase+"="+name, helperDir+"="+t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("v1 did not finish: %s", stderr.String())
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return strings.TrimSpace(stdout.String()), ""
	case errors.As(err, &exit) && exit.ExitCode() == v1.SetupFailed:
		t.Fatalf("v1 helper: %s", stderr.String())
	case !errors.As(err, &exit):
		t.Fatalf("v1 helper: %v", err)
	}
	return "", crashLine(stderr.String(), err)
}

// crashLine is the line of the Go runtime's report that says why the
// process died, or the exit status.
func crashLine(stderr string, err error) string {
	for _, l := range strings.Split(stderr, "\n") {
		for _, p := range []string{"panic:", "fatal error:", "SIGSEGV", "SIGBUS", "unexpected signal"} {
			if strings.HasPrefix(l, p) {
				return l
			}
		}
	}
	return err.Error()
}
