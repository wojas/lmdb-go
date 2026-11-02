// Tests of PageWriter and Pwrite: what they write, also for batches longer
// than one pwritev call takes, and that they allocate nothing.

package sys

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPageWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.mdb")
	f, err := OpenEnvFile(path, false, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const ps = 512
	var want []byte
	put := func(off int, b []byte) {
		if end := off + len(b); end > len(want) {
			want = append(want, make([]byte, end-len(want))...)
		}
		copy(want[off:], b)
	}
	page := func(fill byte, n int) []byte { return bytes.Repeat([]byte{fill}, n*ps) }
	var w PageWriter
	fill := byte(1)
	for _, c := range []struct {
		off   int
		sizes []int // in pages
	}{
		{0, nil},
		{0, []int{1}},
		{ps, []int{1, 1, 1}},
		{8 * ps, []int{1, 3, 1, 2}},
		{20 * ps, make([]int, 150)}, // more than one pwritev takes
		{3 * ps, []int{2}},
	} {
		var bufs [][]byte
		off := c.off
		for _, n := range c.sizes {
			if n == 0 {
				n = 1
			}
			b := page(fill, n)
			fill++
			bufs = append(bufs, b)
			put(off, b)
			off += len(b)
		}
		if err := w.WriteAt(f, bufs, int64(c.off)); err != nil {
			t.Fatalf("WriteAt(%d buffers at %d): %v", len(bufs), c.off, err)
		}
	}
	if err := Pwrite(f, page(0xEE, 1)[:100], 5*ps+7); err != nil {
		t.Fatal(err)
	}
	put(5*ps+7, page(0xEE, 1)[:100])
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file content differs (%d bytes, want %d)", len(got), len(want))
	}
	for i, v := range w.iovRefs() {
		if v {
			t.Fatalf("PageWriter keeps a reference in slot %d", i)
		}
	}
}

func TestAllocsPageWriter(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	path := filepath.Join(t.TempDir(), "data.mdb")
	f, err := OpenEnvFile(path, false, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bufs := make([][]byte, 64)
	for i := range bufs {
		bufs[i] = make([]byte, 4096)
	}
	var w PageWriter
	_ = w.WriteAt(f, bufs, 0) // the gather buffer outside Linux
	for _, c := range []struct {
		name string
		fn   func()
	}{
		{"PageWriter one page", func() { _ = w.WriteAt(f, bufs[:1], 8192) }},
		{"PageWriter 64 pages", func() { _ = w.WriteAt(f, bufs, 8192) }},
		{"Pwrite", func() { _ = Pwrite(f, bufs[0][:128], 16) }},
	} {
		if n := testing.AllocsPerRun(20, c.fn); n != 0 {
			t.Errorf("%s: %v allocations per call, want 0", c.name, n)
		}
	}
}
