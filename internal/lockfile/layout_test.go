// Tests of the lock-file layouts, ported from rlmdb format/lockfile.rs,
// extended to compute every platform's layout on every host. The C side
// (layout_dump, layout_dump10) is compared in tests/lockfile.

package lockfile

import (
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

var (
	macOS        = platform{namedSemaphores: true, hashAlign: 8, pidSize: 4, pthreadSize: 8}
	glibcAarch64 = platform{mutexSize: 48, mutexAlign: 8, pidSize: 4, pthreadSize: 8}
	// glibc x86-64 and musl (both architectures) have a 40-byte mutex.
	mutex40 = platform{mutexSize: 40, mutexAlign: 8, pidSize: 4, pthreadSize: 8}
)

// want is one column of the table in layout.go.
type want struct {
	lockFormat              uint32
	objects                 Objects
	txnid, numReaders       int
	rlock, rlockSize, wlock int
	txbody, header, readers int
}

func checkLayout(t *testing.T, name string, l *Layout, w want) {
	t.Helper()
	got := want{l.LockFormat, l.Objects, l.TxnID, l.NumReaders, l.RLock, l.RLockSize, l.WLock,
		l.TxbodySize, l.HeaderSize, l.Readers}
	if got != w {
		t.Errorf("%s: layout\n got %+v\nwant %+v", name, got, w)
	}
}

// TestLayoutsPerPlatform checks the computed layouts against the numbers
// C's layout_dump and layout_dump10 print on each platform (rlmdb's
// DESIGN.md table; musl's are glibc x86-64's, measured in Alpine).
func TestLayoutsPerPlatform(t *testing.T) {
	v1, v2 := macOS.layouts()
	checkLayout(t, "macOS v1", v1, want{0x10001, SemaphoreNames, 40, 48, 8, 32, 64, 56, 64, 128})
	checkLayout(t, "macOS v2", v2, want{0x28570002, SemaphoreHash, 8, 16, 24, 8, -1, 32, 64, 64})
	v1, v2 = glibcAarch64.layouts()
	checkLayout(t, "glibc aarch64 v1", v1, want{0x10001, PthreadMutexes, 56, 64, 8, 48, 128, 72, 128, 192})
	checkLayout(t, "glibc aarch64 v2", v2, want{0x2cd82002, PthreadMutexes, 8, 16, 24, 48, 128, 72, 128, 192})
	v1, v2 = mutex40.layouts()
	checkLayout(t, "40-byte mutex v1", v1, want{0x10001, PthreadMutexes, 48, 56, 8, 40, 64, 64, 64, 128})
	checkLayout(t, "40-byte mutex v2", v2, want{0x2c2f6002, PthreadMutexes, 8, 16, 24, 40, 64, 64, 64, 128})

	if v1, v2 := (platform{}).layouts(); v1 != nil || v2 != nil {
		t.Error("a platform with an unknown mutex size has layouts")
	}
}

// TestHostLayout checks that this build picked its platform's column.
func TestHostLayout(t *testing.T) {
	switch runtime.GOOS {
	case "darwin":
		checkLayout(t, "host v1", LockV09, want{0x10001, SemaphoreNames, 40, 48, 8, 32, 64, 56, 64, 128})
	case "linux":
		if !sharedLocksAvailable {
			if LockV09 != nil || LockV10 != nil || LayoutFor(format.V09) != nil {
				t.Fatal("layouts without cgo on Linux")
			}
			return
		}
		ref := mutex40
		if mutexSize == 48 {
			ref = glibcAarch64
		} else if mutexSize != 40 {
			t.Fatalf("sizeof(pthread_mutex_t) = %d, expected 40 or 48", mutexSize)
		}
		v1, v2 := ref.layouts()
		if *LockV09 != *v1 || *LockV10 != *v2 {
			t.Fatalf("host layouts %+v %+v, want %+v %+v", LockV09, LockV10, v1, v2)
		}
		t.Logf("sizeof(pthread_mutex_t) = %d: readers at %d (v1), %d (v2)", mutexSize, v1.Readers, v2.Readers)
	}
}

func TestLayoutFor(t *testing.T) {
	if LayoutFor(format.V09) != LockV09 || LayoutFor(format.V10) != LockV10 || LayoutFor(0) != nil {
		t.Fatal("LayoutFor picks the wrong layout")
	}
	for _, p := range []platform{macOS, glibcAarch64, mutex40} {
		v1, v2 := p.layouts()
		if v1.Format != format.V09 || v2.Format != format.V10 || v1.Version != 1 || v2.Version != 2 {
			t.Fatalf("format or version wrong: %+v %+v", v1, v2)
		}
		if v2.LockFormat&0xfff != 2 {
			t.Fatalf("v2 format word %#x", v2.LockFormat)
		}
	}
}

// TestReaderSlotLayout is rlmdb's reader_slot_layout, for every platform.
func TestReaderSlotLayout(t *testing.T) {
	for _, p := range []platform{macOS, glibcAarch64, mutex40} {
		v1, v2 := p.layouts()
		for _, l := range []*Layout{v1, v2} {
			if l.Readers%64 != 0 || l.HeaderSize%64 != 0 {
				t.Errorf("%+v: not cache-line aligned", l)
			}
			if l.FileSize(126) != int64(l.Readers+126*64) || l.ReadersInFile(l.FileSize(7)) != 7 ||
				l.ReadersInFile(10) != 0 || l.ReaderOffset(2) != l.Readers+128 {
				t.Errorf("%+v: slot arithmetic", l)
			}
			if l.TxbodySize > l.HeaderSize || l.RLock+l.RLockSize > l.TxbodySize || l.NumReaders+4 > l.TxbodySize {
				t.Errorf("%+v: fields outside MDB_txbody", l)
			}
		}
	}
}

func TestLog2Mod(t *testing.T) {
	// LOG2_MOD(p2, 5) is log2(p2) % 5 for powers of two.
	for _, c := range [][2]int{{1, 0}, {2, 1}, {4, 2}, {8, 3}, {16, 4}, {32, 0}, {64, 1}} {
		if got := log2Mod(c[0], 5); got != c[1] {
			t.Errorf("log2Mod(%d, 5) = %d, want %d", c[0], got, c[1])
		}
	}
}

// TestLockDesc checks MDB_LOCK_TYPE and MDB_lock_desc against the values
// layout_dump10 prints (rlmdb lock_format_v2_per_platform).
func TestLockDesc(t *testing.T) {
	in := func(txbody, lockType int) lockDescInputs {
		return lockDescInputs{cacheline: 64, pidSize: 4, pthreadSize: 8, txbodySize: txbody,
			lockType: lockType, txnidSize: 8, readerSize: 64, pidlock: true}
	}
	if lt := lockTypePosixSem(8); lt != 5 || lockDesc(in(32, lt)) != 165232 || lockFormatV2(in(32, lt)) != 0x28570002 {
		t.Error("macOS lock description")
	}
	if lt := lockTypePosixMutex(48, 8); lt != 73 || lockDesc(in(72, lt)) != 183682 || lockFormatV2(in(72, lt)) != 0x2cd82002 {
		t.Error("glibc aarch64 lock description")
	}
	if lt := lockTypePosixMutex(40, 8); lt != 63 || lockFormatV2(in(64, lt)) != 0x2c2f6002 {
		t.Error("40-byte mutex lock description")
	}
	// The CACHELINE term is 0 only for 64-byte cache lines.
	other := in(32, 5)
	other.cacheline = 128
	if lockDesc(other) == lockDesc(in(32, 5)) {
		t.Error("cache line size not encoded")
	}
}

// TestSnapshots is rlmdb's snapshots test.
func TestSnapshots(t *testing.T) {
	for _, p := range []platform{macOS, glibcAarch64, mutex40} {
		v1, v2 := p.layouts()
		for _, l := range []*Layout{v1, v2} {
			buf := make([]byte, l.FileSize(2))
			ne := binary.NativeEndian
			ne.PutUint32(buf[0:], Magic)
			ne.PutUint32(buf[4:], l.LockFormat)
			ne.PutUint64(buf[l.TxnID:], 42)
			ne.PutUint32(buf[l.NumReaders:], 2)
			if l.Objects == SemaphoreHash {
				ne.PutUint64(buf[l.RLock:], 0x1234)
			}
			if l.Objects == SemaphoreNames {
				copy(buf[l.RLock:], "/MDB")
			}
			off := l.ReaderOffset(1)
			ne.PutUint64(buf[off:], 7)
			ne.PutUint32(buf[off+8:], 99)
			ne.PutUint64(buf[off+16:], 5)

			h, ok := ReadHeader(buf, l)
			if !ok || h.Magic != Magic || h.Format != l.LockFormat || h.TxnID != 42 || h.NumReaders != 2 {
				t.Fatalf("header %+v", h)
			}
			if (l.Objects == SemaphoreHash) != (h.MutexID == 0x1234) {
				t.Errorf("mutex id %#x", h.MutexID)
			}
			if (l.Objects == SemaphoreNames) != (string(h.RName) == "/MDB") || len(h.WName) != 0 {
				t.Errorf("names %q %q", h.RName, h.WName)
			}
			r, ok := ReadReader(buf, l, 1)
			if !ok || r != (ReaderSnapshot{TxnID: 7, PID: 99, TID: 5}) {
				t.Errorf("reader %+v", r)
			}
			if _, ok := ReadReader(buf, l, 2); ok {
				t.Error("slot past the end")
			}
			if _, ok := ReadReader(buf, l, -1); ok {
				t.Error("negative slot")
			}
			if _, ok := ReadHeader(buf[:l.NumReaders], l); ok {
				t.Error("short header")
			}
		}
	}
}
