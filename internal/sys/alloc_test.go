// The calls of a commit allocate nothing (AGENTS.md "Allocations"): the
// positional writes of the pages and the meta page, and the sync.

package sys

import (
	"path/filepath"
	"testing"
)

func TestAllocsFileIO(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	path := filepath.Join(t.TempDir(), "data.mdb")
	f, err := OpenEnvFile(path, false, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	meta, err := OpenDSync(path)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	buf := make([]byte, 4096)
	for _, c := range []struct {
		name string
		fn   func()
	}{
		{"WriteAt", func() { _ = WriteAt(f, buf, 8192) }},
		{"WriteAt meta", func() { _ = WriteAt(meta, buf[:128], 16) }},
		{"ReadAt", func() { _, _ = ReadAt(f, buf, 4096) }},
		{"Fdatasync", func() { _ = Fdatasync(f) }},
	} {
		if n := testing.AllocsPerRun(20, c.fn); n != 0 {
			t.Errorf("%s: %v allocations per call, want 0", c.name, n)
		}
	}
}
