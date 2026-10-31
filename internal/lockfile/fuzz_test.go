package lockfile

// FuzzLockFile: the lock-file header and reader table as another process
// (or damage) may have left them (go test -fuzz, see FUZZING.md at the
// repository root). The input is the content of an existing lock file.
// For every platform's layout of both lock-file versions it is decoded
// (ReadHeader, ReadReader) and then used as the reader table of a Region,
// as setup accepts an existing file: the slot count from the file size,
// mti_numreaders as stored. The reader-table walks (FindOldest,
// ReaderList, the slot accessors) must stay inside the mapped table
// whatever mti_numreaders says. The table lives in ordinary memory here:
// no file, no record locks, no shared mutexes or semaphores are touched.

import (
	"slices"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func FuzzLockFile(f *testing.F) {
	for _, p := range []platform{macOS, glibcAarch64, mutex40} {
		v09, v10 := p.layouts()
		for _, l := range []*Layout{v09, v10} {
			buf := make([]byte, l.FileSize(4))
			ne.PutUint32(buf[MagicOffset:], Magic)
			ne.PutUint32(buf[FormatOffset:], l.LockFormat)
			ne.PutUint64(buf[l.TxnID:], 42)
			ne.PutUint32(buf[l.NumReaders:], 3)
			for i, pid := range []int32{100, 0, 102} {
				s := l.ReaderOffset(i)
				ne.PutUint64(buf[s+ReaderTxnID:], uint64(40+i))
				ne.PutUint32(buf[s+ReaderPID:], uint32(pid))
				ne.PutUint64(buf[s+ReaderTID:], 7)
			}
			f.Add(buf, uint64(43))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, txnid uint64) {
		if len(data) > 1<<20 {
			return
		}
		for _, p := range []platform{macOS, glibcAarch64, mutex40} {
			v09, v10 := p.layouts()
			for _, l := range []*Layout{v09, v10} {
				checkLockFile(t, l, data, txnid)
			}
		}
	})
}

func checkLockFile(t *testing.T, l *Layout, data []byte, txnid uint64) {
	h, ok := ReadHeader(data, l)
	if ok != (len(data) >= l.Readers) {
		t.Fatalf("ReadHeader of %d bytes (readers at %d): %v", len(data), l.Readers, ok)
	}
	if ok && (h.Magic != ne.Uint32(data[MagicOffset:]) || h.TxnID != ne.Uint64(data[l.TxnID:])) {
		t.Fatal("ReadHeader decodes the wrong fields")
	}
	if len(h.RName) > l.RLockSize || len(h.WName) > l.RLockSize {
		t.Fatal("semaphore name longer than its field")
	}
	n := l.ReadersInFile(int64(len(data)))
	if n < 0 || (n > 0 && l.FileSize(uint32(n)) > int64(len(data))) {
		t.Fatalf("ReadersInFile(%d) = %d", len(data), n)
	}
	for i := -1; i <= int(min(n, 64)); i++ {
		if _, ok := ReadReader(data, l, i); ok != (i >= 0 && int64(i) < n) {
			t.Fatalf("ReadReader(%d) of %d slots: %v", i, n, ok)
		}
	}
	// setup's checks of an existing file: room for the header and a slot.
	if n <= 0 {
		return
	}
	r := &Region{m: &lockMap{mem: slices.Clone(data)}, layout: l, maxReaders: uint32(n), pid: 4242}
	if r.NumReaders() != ne.Uint32(data[l.NumReaders:]) || r.TxnID() != ne.Uint64(data[l.TxnID:]) {
		t.Fatal("header fields through the map")
	}
	if nr := r.numReaders(); nr < 0 || int64(nr) > n {
		t.Fatalf("numReaders %d of %d slots", nr, n)
	}
	if oldest := r.FindOldest(txnid); oldest > txnid-1 {
		t.Fatalf("FindOldest(%d) = %d", txnid, oldest)
	}
	list := r.ReaderList()
	if int64(len(list)) > n {
		t.Fatalf("%d readers listed of %d slots", len(list), n)
	}
	_ = format.FormatReaderList(list)
	_ = r.Header()
	last := int(n - 1)
	for _, i := range []int{0, last} {
		_ = r.SlotIsIdleAndOurs(i)
		_ = r.SlotTxnID(i)
	}
	r.SetTxnID(txnid)
	if got := r.PublishCurrentTxnID(last); got != txnid || r.SlotTxnID(last) != txnid {
		t.Fatalf("PublishCurrentTxnID: %d", got)
	}
	r.ResetSlot(last)
	r.ReleaseSlot(0)
	if r.SlotIsIdleAndOurs(0) {
		t.Fatal("a released slot is not ours")
	}
}
