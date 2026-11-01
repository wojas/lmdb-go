// The computed layouts of other platforms against C's, read from object
// files: cprobe/layout_probe.c includes C LMDB's mdb.c and stores the
// lock file's offsets, sizes and MDB_LOCK_FORMAT in an array, which a
// cross compiler fills in with the target's own headers
// (scripts/bsd-cc-check.sh). This measures a platform's layout as rlmdb's
// layout_dump does, without running anything there.

package lockfile

import (
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probeFields names the entries of lmdbgo_layout in cprobe/layout_probe.c,
// in order.
var probeFields = []string{
	"probe version", "lock mode", "MDB_LOCK_VERSION", "MDB_LOCK_FORMAT",
	"mtb_txnid", "mtb_numreaders", "reader lock", "reader lock size", "writer lock",
	"sizeof(MDB_txbody)", "sizeof(mt1)", "mti_readers",
	"sizeof(MDB_reader)", "mr_pid", "mr_tid", "sizeof(mr_tid)",
	"sizeof(dev_t)", "MDB_SHORT_SEMNAMES", "MDB_PIDLOCK",
}

// The lock modes of the probe (LOCK_MODE).
const (
	probePosixSem   = 1
	probePosixMutex = 2
	probeSysVSem    = 3
	probeWin32      = 4
)

// probeTarget is a platform the probe can be compiled for: the name of its
// object files (<name>-09.o and <name>-10.o), its layouts, and the size of
// its dev_t (what lockFileID hashes; Go's syscall.Stat_t.Dev there).
type probeTarget struct {
	name    string
	p       platform
	devSize int
}

var probeTargets = []probeTarget{
	{"darwin-arm64", macOS, 4},
	{"darwin-amd64", macOS, 4},
	{"linux-arm64-gnu", glibcAarch64, 8},
	{"linux-amd64-gnu", mutex40, 8},
	{"linux-arm64-musl", mutex40, 8},
	{"linux-amd64-musl", mutex40, 8},
	{"freebsd-amd64", freeBSD, 8},
	{"freebsd-arm64", freeBSD, 8},
	{"openbsd-amd64", semBSD, 4},
	{"openbsd-arm64", semBSD, 4},
	{"netbsd-amd64", netBSD, 8},
	{"netbsd-arm64", netBSD, 8},
	{"dragonfly-amd64", semBSD, 4},
}

// TestLayoutsMatchCompiledC checks the layouts of every platform whose
// probe objects are in $LMDB_TEST_LAYOUT_OBJS (skipped if unset).
func TestLayoutsMatchCompiledC(t *testing.T) {
	dir := os.Getenv("LMDB_TEST_LAYOUT_OBJS")
	if dir == "" {
		t.Skip("LMDB_TEST_LAYOUT_OBJS is not set (scripts/bsd-cc-check.sh sets it)")
	}
	n := 0
	for _, pt := range probeTargets {
		v09, v10 := pt.p.layouts()
		for _, c := range []struct {
			ver string
			l   *Layout
		}{{"09", v09}, {"10", v10}} {
			path := filepath.Join(dir, pt.name+"-"+c.ver+".o")
			if _, err := os.Stat(path); err != nil {
				continue
			}
			n++
			vals, err := readProbe(path)
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			for _, d := range compareProbe(vals, c.l, pt) {
				t.Errorf("%s-%s: %s", pt.name, c.ver, d)
			}
			t.Logf("%s-%s: mti_format %#x, readers at %d", pt.name, c.ver, vals[3], vals[11])
		}
	}
	if n == 0 {
		t.Fatalf("no probe objects in %s", dir)
	}
}

// compareProbe lists the differences between C's values and layout l.
func compareProbe(vals []uint64, l *Layout, pt probeTarget) []string {
	if len(vals) != len(probeFields) || vals[0] != 1 {
		return []string{fmt.Sprintf("unknown probe table %v", vals)}
	}
	wlock := uint64(l.WLock)
	if l.WLock < 0 {
		wlock = ^uint64(0)
	}
	mode := map[Objects]uint64{
		PthreadMutexes: probePosixMutex,
		SemaphoreNames: probePosixSem, SemaphoreHash: probePosixSem,
		SysVSemaphores: probeSysVSem,
		MutexNames:     probeWin32, MutexHash: probeWin32,
	}[l.Objects]
	tid, tidSize := uint64(16), uint64(8)
	if pt.p.windowsMutexes {
		tid, tidSize = 12, 4
	}
	want := []uint64{
		1, mode, uint64(l.Version), uint64(l.LockFormat),
		uint64(l.TxnID), uint64(l.NumReaders), uint64(l.RLock), uint64(l.RLockSize), wlock,
		uint64(l.TxbodySize), uint64(l.HeaderSize), uint64(l.Readers),
		ReaderSize, ReaderPID, tid, tidSize,
		uint64(pt.devSize), uint64(b2i(l.shortSemNames)), uint64(b2i(pt.p.pidLock())),
	}
	var diffs []string
	for i, w := range want {
		if vals[i] != w {
			diffs = append(diffs, fmt.Sprintf("%s: C %#x, Go %#x", probeFields[i], vals[i], w))
		}
	}
	return diffs
}

// readProbe reads lmdbgo_layout from an ELF or Mach-O object file.
func readProbe(path string) ([]uint64, error) {
	size := uint64(len(probeFields) * 8)
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		syms, err := f.Symbols()
		if err != nil {
			return nil, err
		}
		for _, s := range syms {
			if s.Name != "lmdbgo_layout" || int(s.Section) >= len(f.Sections) {
				continue
			}
			data, err := f.Sections[s.Section].Data()
			if err != nil {
				return nil, err
			}
			return decodeProbe(data, s.Value, size, f.ByteOrder)
		}
		return nil, fmt.Errorf("no lmdbgo_layout")
	}
	f, err := macho.Open(path)
	if err != nil {
		return nil, fmt.Errorf("neither ELF nor Mach-O: %v", err)
	}
	defer f.Close()
	if f.Symtab == nil {
		return nil, fmt.Errorf("no symbol table")
	}
	for _, s := range f.Symtab.Syms {
		if strings.TrimPrefix(s.Name, "_") != "lmdbgo_layout" || s.Sect == 0 || int(s.Sect) > len(f.Sections) {
			continue
		}
		sec := f.Sections[s.Sect-1]
		data, err := sec.Data()
		if err != nil {
			return nil, err
		}
		return decodeProbe(data, s.Value-sec.Addr, size, f.ByteOrder)
	}
	return nil, fmt.Errorf("no lmdbgo_layout")
}

func decodeProbe(data []byte, off, size uint64, bo binary.ByteOrder) ([]uint64, error) {
	if off > uint64(len(data)) || size > uint64(len(data))-off {
		return nil, fmt.Errorf("lmdbgo_layout outside its section")
	}
	b := data[off : off+size]
	vals := make([]uint64, len(b)/8)
	for i := range vals {
		vals[i] = bo.Uint64(b[8*i:])
	}
	return vals, nil
}
