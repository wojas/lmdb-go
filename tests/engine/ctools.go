// Package engine holds the tests of internal/engine that need C: the
// write-differential test against rlmdb's writeops/writeops10 programs,
// and checks of files with the C tools (mdb_dump, mdb_stat, cpeer). The
// binaries come from rlmdb's interop build; set RLMDB_BIN to their
// directory, or RLMDB_DIR to the rlmdb checkout (see cBin). Tests skip
// with a message when they are absent.
package engine

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PowerDNS/lmdb-go/tests/internal/coracle"
)

// cBin is the directory of the C oracle binaries: $RLMDB_BIN (for example
// the output of scripts/build-c-writeops.sh or cmd/build-c-oracles), or
// rlmdb's interop build under $RLMDB_DIR
// (target/release/build/rlmdb-interop-*/out/bin).
func cBin() string {
	if d := os.Getenv("RLMDB_BIN"); d != "" {
		return d
	}
	return coracle.InteropBin(os.Getenv("RLMDB_DIR"), "writeops")
}

// tool returns the path of C binary name, or skips the test.
func tool(t testing.TB, name string) string {
	t.Helper()
	d := cBin()
	if d == "" {
		t.Skip("C oracle binaries not found: set RLMDB_BIN (rlmdb interop build's out/bin) or RLMDB_DIR")
	}
	p := filepath.Join(d, coracle.Exe(name))
	if _, err := os.Stat(p); err != nil {
		t.Skipf("C oracle %s not found in %s", name, d)
	}
	return p
}

// run runs a C tool and returns its exit code, stdout and stderr.
func run(t testing.TB, name string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(tool(t, name), args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, coracle.Text(out.String()), coracle.Text(errb.String())
}

// cPageSize is the page size of a new environment of the C 0.9 oracles,
// found once by creating one with cpeer.
var cPageSize struct {
	once sync.Once
	ps   int
	err  error
}

// requireSamePageSize skips t if the C 0.9 oracles create pages of another
// size than this process. LMDB 0.9 takes the OS page size (mdb_env_open;
// the Go engine does the same), so the files of the two cannot be the
// same. That is the case when the oracles and the test binary are built
// for different architectures, for example arm64 oracles (16 KiB pages on
// Apple silicon) and a GOARCH=amd64 test binary under Rosetta (4 KiB).
func requireSamePageSize(t testing.TB) {
	t.Helper()
	cpeer := tool(t, "cpeer")
	c := &cPageSize
	c.once.Do(func() {
		dir, err := os.MkdirTemp("", "coracle-pagesize-")
		if err != nil {
			c.err = err
			return
		}
		defer os.RemoveAll(dir)
		out, err := exec.Command(cpeer, "create", dir, "1048576", "1").Output()
		if err != nil {
			c.err = fmt.Errorf("cpeer create: %v", err)
			return
		}
		for _, f := range strings.Fields(string(out)) {
			if v, ok := strings.CutPrefix(f, "psize="); ok {
				_, c.err = fmt.Sscan(v, &c.ps)
				return
			}
		}
		c.err = fmt.Errorf("cpeer create printed no psize: %q", out)
	})
	if c.err != nil {
		t.Fatal(c.err)
	}
	if own := os.Getpagesize(); c.ps != own {
		t.Skipf("the C 0.9 oracles in %s create %d-byte pages, this process %d-byte pages "+
			"(LMDB 0.9 takes the OS page size, so the files cannot match; are the oracles "+
			"built for another architecture, such as arm64 oracles under GOARCH=amd64?): "+
			"run the test for the oracles' architecture, or build oracles for this one "+
			"(scripts/build-c-writeops.sh, with CC set for it) and set RLMDB_BIN", filepath.Dir(cpeer), c.ps, own)
	}
}
