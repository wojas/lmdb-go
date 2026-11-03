package engine

// The ordering of extendUnder (rlmdb core/env.rs tests
// extending_reads_the_size_again_under_the_lock and
// extending_under_the_lock_never_cuts_a_commit): the environment extends
// an LMDB 0.9 data file on Windows only, but the ordering is the same
// everywhere, so it is tested on every system, with a sync.Mutex as the
// writer mutex and a second handle of the file as another process.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/sys"
)

func openRW(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func fileSize(t *testing.T, f *os.File) int64 {
	t.Helper()
	n, err := sys.FileSize(f)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestExtendingReadsTheSizeAgainUnderTheLock: the race of an opener that
// extends the data file from a stale size. While it waits for the writer
// mutex, another process with a larger map grows the file and commits a
// page at its end; under the mutex the size is read again, and the file is
// left as it is.
func TestExtendingReadsTheSizeAgainUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.mdb")
	f := openRW(t, path)
	must(t, f.Truncate(4096))
	other := openRW(t, path)
	noUnlock := func() error { return nil }
	err := extendUnder(f, 1<<20, func() (func() error, error) {
		must(t, other.Truncate(3<<20))
		must(t, sys.WriteAt(other, make9(4096), 3<<20-4096))
		return noUnlock, nil
	})
	must(t, err)
	if n := fileSize(t, f); n != 3<<20 {
		t.Fatalf("size %d, want %d", n, 3<<20)
	}
	last := make([]byte, 1)
	if _, err := sys.ReadAt(f, last, 3<<20-1); err != nil || last[0] != 9 {
		t.Fatalf("last byte %d, %v", last[0], err)
	}
	// A long file: not even the lock is taken.
	err = extendUnder(f, 1<<20, func() (func() error, error) {
		t.Fatal("locked")
		return noUnlock, nil
	})
	must(t, err)
	// A short one is extended.
	short := openRW(t, filepath.Join(dir, "short.mdb"))
	must(t, extendUnder(short, 1<<20, func() (func() error, error) { return noUnlock, nil }))
	if n := fileSize(t, short); n != 1<<20 {
		t.Fatalf("short: size %d", n)
	}
}

func make9(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 9
	}
	return b
}

// TestExtendingUnderTheLockNeverCutsACommit: two goroutines for two
// processes, many times. One grows the file past the other's map size
// without the lock (an opener with a larger map) and commits a page at its
// new end under the lock; the other extends the file to its smaller map
// size. No commit may be lost. Without the lock one is lost only now and
// then (the window is a few instructions wide); the test above catches
// that every time.
func TestExtendingUnderTheLockNeverCutsACommit(t *testing.T) {
	const (
		mapSize = 1 << 20
		rounds  = 1000
	)
	path := filepath.Join(t.TempDir(), "data.mdb")
	f := openRW(t, path)
	must(t, f.Truncate(4096))
	g := openRW(t, path)
	var writer sync.Mutex
	var done atomic.Bool
	lost := make(chan int, 1)
	go func() {
		defer done.Store(true)
		n, last := 0, int64(0)
		var got [8]byte
		for i := uint64(1); i <= rounds; i++ {
			writer.Lock()
			if last != 0 {
				r, _ := sys.ReadAt(g, got[:], last-8)
				if r < 8 || binary.NativeEndian.Uint64(got[:]) != i-1 {
					n++
				}
			}
			// Short again, so that the extension has work to do.
			if err := g.Truncate(4096); err != nil {
				panic(err)
			}
			writer.Unlock()
			end := int64(mapSize) + int64(i%64+1)*4096
			if err := g.Truncate(end); err != nil {
				panic(err)
			}
			writer.Lock()
			var b [8]byte
			binary.NativeEndian.PutUint64(b[:], i)
			if err := sys.WriteAt(g, b[:], end-8); err != nil {
				panic(err)
			}
			writer.Unlock()
			last = end
		}
		lost <- n
	}()
	lock := func() (func() error, error) {
		writer.Lock()
		return func() error { writer.Unlock(); return nil }, nil
	}
	for !done.Load() {
		must(t, extendUnder(f, mapSize, lock))
	}
	if n := <-lost; n != 0 {
		t.Fatalf("%d of %d commits cut", n, rounds)
	}
}
