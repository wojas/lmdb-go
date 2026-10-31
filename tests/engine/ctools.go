// Package engine holds the tests of internal/engine that need C: the
// write-differential test against rlmdb's writeops/writeops10 programs,
// and checks of files with the C tools (mdb_dump, mdb_stat, cpeer). The
// binaries come from rlmdb's interop build; set RLMDB_BIN to their
// directory (default: rlmdb's release build next to this repository, see
// cBin). Tests skip with a message when they are absent.
package engine

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// cBin is the directory of the C oracle binaries: $RLMDB_BIN, or the
// first match of $RLMDB_DIR/target/release/build/rlmdb-interop-*/out/bin.
func cBin() string {
	if d := os.Getenv("RLMDB_BIN"); d != "" {
		return d
	}
	if d := os.Getenv("RLMDB_DIR"); d != "" {
		m, _ := filepath.Glob(filepath.Join(d, "rust", "target", "release", "build", "rlmdb-interop-*", "out", "bin"))
		if len(m) == 0 {
			m, _ = filepath.Glob(filepath.Join(d, "target", "release", "build", "rlmdb-interop-*", "out", "bin"))
		}
		if len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// tool returns the path of C binary name, or skips the test.
func tool(t testing.TB, name string) string {
	t.Helper()
	d := cBin()
	if d == "" {
		t.Skip("C oracle binaries not found: set RLMDB_BIN (rlmdb interop build's out/bin) or RLMDB_DIR")
	}
	p := filepath.Join(d, name)
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
	return code, out.String(), errb.String()
}
