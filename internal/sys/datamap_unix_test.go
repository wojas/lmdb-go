//go:build unix

// Tests of the Unix data map: rlmdb os/unix.rs data_map_invalidate.

package sys

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestDataMapInvalidate: Invalidate (MDB_Invalidate, only called on
// OpenBSD) accepts any range: pages written through the descriptor read
// back through the map, and ranges that are unaligned, empty or reach past
// the map do no harm.
func TestDataMapInvalidate(t *testing.T) {
	ps := OSPageSize()
	f, err := os.Create(filepath.Join(t.TempDir(), "data.mdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(3 * ps)); err != nil {
		t.Fatal(err)
	}
	const size = 1 << 20
	m, err := MapData(f, size, false, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Release()
	if b, ok := m.Slice(ps, 1); !ok || b[0] != 0 {
		t.Fatalf("page 1: %v %v", b, ok)
	}
	page := bytes.Repeat([]byte{5}, ps)
	if err := WriteAt(f, page, int64(ps)); err != nil {
		t.Fatal(err)
	}
	m.Invalidate(uint64(ps), ps)
	m.Invalidate(uint64(ps)+4, 10)
	m.Invalidate(size-1, ps)
	m.Invalidate(size+uint64(ps), 1)
	m.Invalidate(^uint64(0), 1)
	m.Invalidate(0, 0)
	m.Invalidate(7, -1)
	if b, ok := m.Slice(ps, ps); !ok || !bytes.Equal(b, page) {
		t.Fatal("the written page does not read back through the map")
	}
}
