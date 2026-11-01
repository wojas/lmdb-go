// Tests of the Windows data map, ported from rlmdb os/windows.rs
// (page_size_is_power_of_two, data_map_bounds,
// reserved_map_shows_the_file_growing,
// reserved_map_follows_the_file_past_the_first_mib,
// committed_pages_show_in_other_views,
// data_map_read_only_handle_maps_what_exists).

package sys

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestOSPageSize(t *testing.T) {
	p := OSPageSize()
	if p < 4096 || p&(p-1) != 0 {
		t.Fatalf("page size %d", p)
	}
}

// rwFile opens (creating) path read/write as the environment does.
func rwFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := OpenEnvFile(path, false, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func fill(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func size(t *testing.T, f *os.File) int64 {
	t.Helper()
	n, err := FileSize(f)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustMap(t *testing.T, f *os.File, n int, metaEnd int, reserve bool) *DataMap {
	t.Helper()
	m, err := MapData(f, n, false, metaEnd, reserve)
	if err != nil {
		t.Fatalf("map %d bytes (reserve %v): %v", n, reserve, err)
	}
	t.Cleanup(m.Release)
	return m
}

func slice(t *testing.T, m *DataMap, off, n int) []byte {
	t.Helper()
	b, ok := m.Slice(off, n)
	if !ok {
		t.Fatalf("slice %d+%d refused", off, n)
	}
	return b
}

// TestDataMapBounds is rlmdb's data_map_bounds: LMDB 0.9's map through a
// writable handle extends the file to the map size.
func TestDataMapBounds(t *testing.T) {
	f := rwFile(t, filepath.Join(t.TempDir(), "data.mdb"))
	if err := WriteAt(f, fill(8192, 7), 0); err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, f, 1<<20, 2048, false)
	if size(t, f) != 1<<20 {
		t.Fatalf("file size %d, want the map size", size(t, f))
	}
	if slice(t, m, 4096, 4096)[0] != 7 {
		t.Fatal("content")
	}
	for _, r := range [][2]int{{0, 16}, {2047, 1}, {1 << 20, 1}, {math.MaxInt, 2}, {4096, -1}} {
		if _, ok := m.Slice(r[0], r[1]); ok {
			t.Errorf("slice %d+%d allowed", r[0], r[1])
		}
	}
	if _, ok := m.Slice(2048, 1); !ok {
		t.Error("first byte past the meta pages refused")
	}
	if slice(t, m, 8192, 4096)[0] != 0 {
		t.Fatal("the extension is not zero")
	}
	buf := make([]byte, 4)
	if !m.ReadMeta(8190, buf) || !bytes.Equal(buf, []byte{7, 7, 0, 0}) {
		t.Fatalf("ReadMeta %v", buf)
	}
	// Written pages show through the map.
	if err := WriteAt(f, fill(4096, 9), 8192); err != nil {
		t.Fatal(err)
	}
	if slice(t, m, 8192, 4096)[4095] != 9 {
		t.Fatal("write not visible")
	}
	if err := m.CommitPages(8192, 4096); err != nil {
		t.Fatalf("CommitPages of a 0.9 map: %v", err)
	}
}

// TestReservedMapShowsTheFileGrowing is rlmdb's
// reserved_map_shows_the_file_growing: LMDB 1.0's map; the file keeps its
// size, and pages written past its end show through the reserved view
// (through this handle or another one, as another process's writes
// would).
func TestReservedMapShowsTheFileGrowing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.mdb")
	f := rwFile(t, path)
	if err := WriteAt(f, fill(8192, 7), 0); err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, f, 1<<20, 2048, true)
	if size(t, f) != 8192 {
		t.Fatalf("the file was extended to %d", size(t, f))
	}
	if m.Len() != 1<<20 || slice(t, m, 4096, 4096)[0] != 7 {
		t.Fatalf("len %d", m.Len())
	}
	if _, ok := m.Slice(8192, 4096); ok {
		t.Fatal("a page past the end of the file")
	}
	WriteAt(f, fill(4096, 9), 8192)
	if slice(t, m, 8192, 4096)[4095] != 9 {
		t.Fatal("own write not visible")
	}
	other := rwFile(t, path)
	WriteAt(other, fill(4096, 5), 64<<10)
	if slice(t, m, 64<<10, 4096)[0] != 5 {
		t.Fatal("other handle's write not visible")
	}
	if slice(t, m, 12288, 4096)[0] != 0 {
		t.Fatal("the hole is not zero")
	}
	buf := make([]byte, 4)
	if !m.ReadMeta(8190, buf) || !bytes.Equal(buf, []byte{7, 7, 9, 9}) {
		t.Fatalf("ReadMeta %v", buf)
	}
	// A read-only handle reserves the map size too where Windows allows
	// it, and then sees pages another handle adds.
	ro, err := OpenEnvFile(path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	r := mustMap(t, ro, 1<<20, 0, true)
	t.Logf("read-only reserved view: %d bytes", r.Len())
	if slice(t, r, 64<<10, 1)[0] != 5 {
		t.Fatal("read-only view")
	}
	WriteAt(other, fill(4096, 3), 256<<10)
	if r.Len() == 1<<20 {
		if slice(t, r, 256<<10, 4096)[4095] != 3 {
			t.Fatal("read-only view does not show the new page")
		}
	} else if _, ok := r.Slice(256<<10, 1); ok {
		t.Fatal("a page past a short read-only view")
	}
}

// TestReservedMapFollowsTheFilePastTheFirstMiB is rlmdb's
// reserved_map_follows_the_file_past_the_first_mib: a file grown 64 KiB at
// a time under a reserved view: every page shows, also past the first
// MiB, where the view faults without the explicit extension of the
// section.
func TestReservedMapFollowsTheFilePastTheFirstMiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.mdb")
	f := rwFile(t, path)
	WriteAt(f, fill(128<<10, 1), 0)
	m := mustMap(t, f, 256<<20, 0, true)
	for i := 2; i < 40; i++ {
		WriteAt(f, fill(64<<10, byte(i)), int64(i)<<16)
		got := slice(t, m, i<<16, 64<<10)
		if got[0] != byte(i) || got[len(got)-1] != byte(i) {
			t.Fatalf("page %d: %d %d", i, got[0], got[len(got)-1])
		}
	}
	if size(t, f) != 40<<16 {
		t.Fatalf("the file was extended to %d", size(t, f))
	}
	other := rwFile(t, path)
	WriteAt(other, fill(4096, 7), 8<<20)
	if slice(t, m, 8<<20, 4096)[0] != 7 || slice(t, m, 5<<20, 1)[0] != 0 {
		t.Fatal("far page or hole")
	}
}

// TestCommittedPagesShowInOtherViews is rlmdb's
// committed_pages_show_in_other_views: pages committed in one reserved
// view (as a writer allocates them) and then written show in another view
// of the file, with a section of its own, as in another process; read
// directly, without the extension of that view's section, which would
// hide a missing commit (a fault fails the test).
func TestCommittedPagesShowInOtherViews(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.mdb")
	f := rwFile(t, path)
	WriteAt(f, fill(8192, 1), 0)
	writer := mustMap(t, f, 64<<20, 0, true)
	other := mustMap(t, f, 64<<20, 0, true)
	for _, c := range []struct {
		b   byte
		off int
	}{{2, 3 << 20}, {3, 5 << 20}, {4, 5<<20 + 4096}} {
		if err := writer.CommitPages(c.off, 4096); err != nil {
			t.Fatal(err)
		}
		if size(t, f) < int64(c.off+4096) {
			t.Fatalf("commit did not extend the file: %d", size(t, f))
		}
		WriteAt(f, fill(4096, c.b), int64(c.off))
		if got := other.data[c.off+4095]; got != c.b {
			t.Fatalf("page at %d: %d", c.off, got)
		}
	}
	if err := writer.CommitPages(64<<20, 4096); err == nil {
		t.Fatal("commit past the view")
	}
	// Not a reserved view: nothing to commit.
	v09 := mustMap(t, f, 8<<20, 0, false)
	if err := v09.CommitPages(4096, 4096); err != nil {
		t.Fatal(err)
	}
}

// TestReadOnlyHandleMapsWhatExists is rlmdb's
// data_map_read_only_handle_maps_what_exists (LMDB 0.9's map).
func TestReadOnlyHandleMapsWhatExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.mdb")
	if err := os.WriteFile(path, fill(8192, 5), 0o644); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenEnvFile(path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := MapData(ro, 1<<20, false, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.Len() != 8192 || size(t, ro) != 8192 {
		t.Fatalf("len %d, file %d", m.Len(), size(t, ro))
	}
	if slice(t, m, 4096, 4096)[0] != 5 {
		t.Fatal("content")
	}
	if _, ok := m.Slice(8192, 1); ok {
		t.Fatal("past the file")
	}
	m.Release()
	ro.Close()
	// An empty file cannot be mapped.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ro, err = OpenEnvFile(path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if m, err := MapData(ro, 4096, false, 0, false); err == nil {
		m.Release()
		t.Fatal("mapped an empty file")
	}
}

// TestOpenEnvFileSharing: the environment's files are shared for reading
// and writing, not for deletion (mdb_fopen).
func TestOpenEnvFileSharing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.mdb")
	f := rwFile(t, path)
	g := rwFile(t, path)
	if err := WriteAt(g, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	meta, err := OpenDSync(path)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	if err := WriteAt(meta, []byte("y"), 1); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := ReadAt(f, buf, 0); err != nil || string(buf) != "xy" {
		t.Fatalf("read %q %v", buf, err)
	}
	if err := os.Remove(path); err == nil {
		t.Fatal("removed an open data file")
	}
}
