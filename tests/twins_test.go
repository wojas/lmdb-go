package tests

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// twins are the packages written against the cgo reference (<dir>/cgo)
// whose port twin (<dir>/port) gen-port.sh writes.
var twins = []string{"bench", "gopeer"}

// TestPortTwinsUpToDate checks that every generated port twin is what
// gen-port.sh makes of its cgo source now (run go generate ./... after
// changing a cgo package).
func TestPortTwinsUpToDate(t *testing.T) {
	for _, dir := range twins {
		tmp := t.TempDir()
		out, err := exec.Command("sh", "gen-port.sh", dir, tmp).CombinedOutput()
		if err != nil {
			t.Fatalf("gen-port.sh %s: %v\n%s", dir, err, out)
		}
		want, _ := filepath.Glob(filepath.Join(tmp, "*.go"))
		for _, w := range want {
			name := filepath.Base(w)
			exp, _ := os.ReadFile(w)
			got, err := os.ReadFile(filepath.Join(dir, "port", name))
			if err != nil || !bytes.Equal(got, exp) {
				t.Errorf("%s/port/%s is out of date: run go generate ./... in tests", dir, name)
			}
		}
		have, _ := filepath.Glob(filepath.Join(dir, "port", "*.go"))
		for _, h := range have {
			name := filepath.Base(h)
			if name == "impl.go" {
				continue
			}
			if _, err := os.Stat(filepath.Join(tmp, name)); err != nil {
				t.Errorf("%s/port/%s has no source in %s/cgo: run go generate ./... in tests", dir, name, dir)
			}
		}
	}
}
