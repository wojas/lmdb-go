package engine

// Plain copies of a data file with a meta page newer than the snapshot,
// and of one after a failed meta page sync (rlmdb bf3a0c0,
// KNOWN_C_BUGS.md row 41; env_copy.rs
// plain_copy_never_emits_a_meta_newer_than_its_snapshot and the lock-file
// half of robust_io_errors.rs robust_failed_meta_sync_is_fatal), with a
// real lock file, so that C processes see the txnid the Go process
// published.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/lockfile"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// openLockFile opens the lock file shared with C (lmdb's openLock).
func openLockFile(path string, mode os.FileMode, maxReaders uint32, readOnly bool, f format.Format, claim eng.LockClaim) (eng.LockRegion, error) {
	if !lockfile.SharedLocksAvailable() {
		return nil, lockfile.ErrNoSharedLocks
	}
	r, err := lockfile.OpenClaimed(path, mode, maxReaders, readOnly, lockfile.LayoutFor(f), claim)
	if err != nil || r == nil {
		return nil, err
	}
	return r, nil
}

// The environment variables that make this test binary a copy process
// (copyHelper): the environment, its format and the directory to copy
// it to.
const (
	copyHelperSrc    = "ENGINE_TEST_COPY_SRC"
	copyHelperFormat = "ENGINE_TEST_COPY_FORMAT"
	copyHelperDst    = "ENGINE_TEST_COPY_DST"
)

// copyHelper is the second process of copyInAnotherProcess: a plain copy
// of src, which the test keeps open (with a lock file, a process opens an
// environment once).
func copyHelper(src string) int {
	f := format.V09
	if os.Getenv(copyHelperFormat) == "1.0" {
		f = format.V10
	}
	env, err := eng.Open(src, eng.Options{Format: f, MaxDBs: 2, OpenLock: openLockFile})
	if err == nil {
		err = env.Copy(os.Getenv(copyHelperDst), 0)
		env.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "copy helper:", err)
		return 1
	}
	return 0
}

// copyInAnotherProcess makes a plain copy of src into dst with the Go
// engine in another process.
func copyInAnotherProcess(t *testing.T, src string, f format.Format, dst string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), copyHelperSrc+"="+src, copyHelperFormat+"="+f.String(), copyHelperDst+"="+dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copy process: %v\n%s", err, out)
	}
}

// copyValue is the value of key i (rlmdb env_copy.rs value).
func copyValue(i, n int) []byte {
	v := make([]byte, n)
	for j := range v {
		v[j] = byte(i*31 + j*7)
	}
	return v
}

func copyKey(i int) []byte { return binary.BigEndian.AppendUint64(nil, uint64(i)) }

func firstDifference(a, b []byte, ps int) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("first difference at byte %d (page %d, offset %d)", i, i/ps, i%ps)
		}
	}
	return fmt.Sprintf("lengths %d and %d", len(a), len(b))
}

// layoutOf is the data-file layout of format f.
func layoutOf(f format.Format) *format.Layout {
	if f == format.V10 {
		return format.LayoutV10
	}
	return format.LayoutV09
}

// checkCopyKeys opens the copy in dir and checks its newest txnid and its
// keys 0..n-1 (copyKey, copyValue).
func checkCopyKeys(t *testing.T, dir string, txnid uint64, n int) {
	t.Helper()
	env, err := eng.Open(dir, eng.Options{MaxDBs: 2, Flags: format.NoLock | format.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	if info, err := env.Info(); err != nil || info.LastTxnID != txnid {
		t.Fatalf("the copy's newest meta page: txnid %d, %v; want %d", info.LastTxnID, err, txnid)
	}
	r, err := env.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Abort()
	c, err := r.OpenCursor(eng.MainDBI)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < n; i++ {
		k, v, err := c.Get(nil, nil, format.Next)
		if err != nil || !bytes.Equal(k, copyKey(i)) || !bytes.Equal(v, copyValue(i, 100)) {
			t.Fatalf("the copy's item %d: %x, %v", i, k, err)
		}
	}
	if _, _, err := c.Get(nil, nil, format.Next); err != format.NotFound {
		t.Fatalf("the copy has more than %d items: %v", n, err)
	}
}

// TestPlainCopyHidesANewerMeta: a meta page newer than the snapshot a
// plain copy pins, which a commit whose meta page reached the file while
// its write or sync failed leaves behind (C on macOS after a failed
// F_FULLFSYNC; anyone when writing the old txnid back fails too), forged
// here while the environment is open, so that its lock file still names
// the older snapshot. The copy holds the snapshot's pages, so it gives
// that page the snapshot's meta; C's copy keeps the page, and reading it
// fails (MDB_PAGE_NOTFOUND). Once a commit has overwritten the page, the
// copy is C's again.
func TestPlainCopyHidesANewerMeta(t *testing.T) {
	for _, c := range []struct {
		f    format.Format
		ps   int
		tool string
	}{{format.V09, 0, "mdb_copy"}, {format.V10, 1024, "mdb_copy10"}} {
		t.Run(c.f.String(), func(t *testing.T) {
			tool(t, c.tool)
			dir := t.TempDir()
			env, err := eng.Open(dir, eng.Options{Format: c.f, PageSize: c.ps, MapSize: 64 << 20, MaxDBs: 2,
				Flags: format.NoSync, OpenLock: openLockFile})
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			ps, l := env.PageSize(), layoutOf(c.f)
			commit := func(from, to int) {
				txn, err := env.BeginTxn(nil, 0)
				must(t, err)
				for i := from; i < to; i++ {
					must(t, txn.Put(eng.MainDBI, copyKey(i), copyValue(i, 100), 0))
				}
				must(t, txn.Commit())
			}
			for round := 0; round < 3; round++ {
				commit(round*200, (round+1)*200)
			}
			info, err := env.Info()
			must(t, err)
			txnid := info.LastTxnID
			data := filepath.Join(dir, "data.mdb")
			source, err := os.ReadFile(data)
			must(t, err)
			own, other := int(txnid&1), int(^txnid&1)
			metaAt := func(i int) int { return i*ps + l.MetaOffset() }
			meta, ok := format.ReadMeta(source[metaAt(own):])
			if !ok || meta.TxnID != txnid {
				t.Fatalf("meta page %d: %+v", own, meta)
			}
			// The next commit's meta page, naming pages past the file's end.
			newer := meta
			newer.TxnID = txnid + 1
			newer.LastPgno = meta.LastPgno + 40
			newer.DBs[1].Root = meta.LastPgno + 20
			var forged [format.MetaSize]byte
			newer.Write(forged[:])
			writeAsCommit(t, data, forged[:], metaAt(other))
			if info, _ := env.Info(); info.LastTxnID != txnid+1 {
				t.Fatalf("Info: txnid %d, want %d", info.LastTxnID, txnid+1)
			}
			r, err := env.BeginTxn(nil, format.ReadOnly)
			must(t, err)
			if r.ID() != txnid {
				t.Fatalf("a reader reads txnid %d, want %d", r.ID(), txnid)
			}
			r.Abort()

			target := t.TempDir()
			must(t, env.Copy(target, 0))
			copied, err := os.ReadFile(filepath.Join(target, "data.mdb"))
			must(t, err)
			// The source's bytes up to the snapshot's last page, but for the
			// part of the newer meta page that a commit writes, which is the
			// snapshot's.
			commitPart := func(b []byte, i int) []byte {
				return b[metaAt(i)+format.MetaMapSize : metaAt(i)+format.MetaSize]
			}
			expected, err := os.ReadFile(data)
			must(t, err)
			expected = expected[:min(len(source), int(meta.LastPgno+1)*ps)]
			copy(commitPart(expected, other), commitPart(expected, own))
			if !bytes.Equal(copied, expected) {
				t.Fatalf("the copy: %s", firstDifference(copied, expected, ps))
			}
			checkCopyKeys(t, target, txnid, 600)
			if c.f == format.V10 {
				// C 1.0 does not open such a file: mdb_env_open2 checks
				// that the newest meta page's roots are inside the file.
				if rc, _, errs := run(t, c.tool, dir); rc == 0 || !strings.Contains(errs, "MDB_PAGE_NOTFOUND") {
					t.Fatalf("mdb_copy10 of the file: exit %d, stderr %s", rc, errs)
				}
			} else {
				cCopyKeepsNewerMeta(t, c.tool, dir, ps, txnid, forged[format.MetaMapSize:], copied, commitPart, own, other)
			}

			// The next commit overwrites the newer page; copies are C's
			// again.
			txn, err := env.BeginTxn(nil, 0)
			must(t, err)
			must(t, txn.Put(eng.MainDBI, []byte("next"), []byte("value"), 0))
			must(t, txn.Commit())
			if info, _ := env.Info(); info.LastTxnID != txnid+1 {
				t.Fatalf("after the next commit: txnid %d, want %d", info.LastTxnID, txnid+1)
			}
			target = t.TempDir()
			must(t, env.Copy(target, 0))
			copied, err = os.ReadFile(filepath.Join(target, "data.mdb"))
			must(t, err)
			if _, theirs, _ := run(t, c.tool, dir); theirs != string(copied) {
				t.Fatalf("after the next commit: %s", firstDifference([]byte(theirs), copied, ps))
			}
			func() {
				cenv, err := eng.Open(target, eng.Options{MaxDBs: 2, Flags: format.NoLock | format.ReadOnly})
				must(t, err)
				defer cenv.Close()
				r, err := cenv.BeginTxn(nil, format.ReadOnly)
				must(t, err)
				defer r.Abort()
				if st, err := r.Stat(eng.MainDBI); err != nil || r.ID() != txnid+1 || st.Entries != 601 {
					t.Fatalf("the copy after the next commit: txnid %d, %+v, %v", r.ID(), st, err)
				}
			}()
		})
	}
}

// writeAsCommit writes b at off of the data file through a descriptor, as
// a commit writes its pages, and, where maps do not show such writes
// (OpenBSD, which has no unified buffer cache), drops the mapped copy of
// them as the writer does (MDB_Invalidate, sys.DataMap.Invalidate), so
// that the environment's map shows them too.
func writeAsCommit(t *testing.T, data string, b []byte, off int) {
	t.Helper()
	f, err := os.OpenFile(data, os.O_RDWR, 0)
	must(t, err)
	defer f.Close()
	_, err = f.WriteAt(b, int64(off))
	must(t, err)
	if sys.MapNeedsInvalidate {
		m, err := sys.MapData(f, off+len(b), false, 0, false)
		must(t, err)
		m.Invalidate(uint64(off), len(b))
		m.Release()
	}
}

// cCopyKeepsNewerMeta: C 0.9's plain copy of dir (whose meta page other
// holds forged, the commit part of a meta page newer than the snapshot
// txnid) keeps the newer page as it is, so it names pages it lacks
// (MDB_PAGE_NOTFOUND); with the snapshot's meta in that page it is the
// Go engine's copy, copied.
func cCopyKeepsNewerMeta(t *testing.T, tool, dir string, ps int, txnid uint64, forged, copied []byte,
	commitPart func([]byte, int) []byte, own, other int) {
	t.Helper()
	_, theirsOut, errs := run(t, tool, dir)
	theirs := []byte(theirsOut)
	if len(theirs) < 2*ps || !bytes.Equal(commitPart(theirs, other), forged) {
		t.Fatalf("C's copy (%d bytes) does not keep the newer meta page; stderr %s", len(theirs), errs)
	}
	tdir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(tdir, "data.mdb"), theirs, 0o644))
	cenv, err := eng.Open(tdir, eng.Options{MaxDBs: 2, Flags: format.NoLock | format.ReadOnly})
	must(t, err)
	defer cenv.Close()
	if info, err := cenv.Info(); err != nil || info.LastTxnID != txnid+1 {
		t.Fatalf("C's copy: txnid %d, %v; want %d", info.LastTxnID, err, txnid+1)
	}
	r, err := cenv.BeginTxn(nil, format.ReadOnly)
	must(t, err)
	defer r.Abort()
	c, err := r.OpenCursor(eng.MainDBI)
	must(t, err)
	defer c.Close()
	if _, _, err := c.Get(nil, nil, format.Next); err != format.PageNotFound {
		t.Fatalf("reading C's copy: %v, want MDB_PAGE_NOTFOUND", err)
	}
	copy(commitPart(theirs, other), commitPart(theirs, own))
	if !bytes.Equal(theirs, copied) {
		t.Fatalf("C's copy with the snapshot's meta: %s", firstDifference(theirs, copied, ps))
	}
}

// TestCopyAfterFailedMetaSync: after a sync that failed once the meta
// page was in the file (injected), the page holds the old txnid again, so
// a plain copy that another process takes before the environment is
// re-opened shows the published snapshot, whose pages it holds: one by
// the Go engine, and C's mdb_copy, byte for byte the same. (Before the
// fix the file kept the newer page, and such a copy named pages it
// lacked.)
func TestCopyAfterFailedMetaSync(t *testing.T) {
	for _, c := range []struct {
		f    format.Format
		tool string
	}{{format.V09, "mdb_copy"}, {format.V10, "mdb_copy10"}} {
		t.Run(c.f.String(), func(t *testing.T) {
			tool(t, c.tool)
			dir := t.TempDir()
			env, err := eng.Open(dir, eng.Options{Format: c.f, MapSize: 64 << 20, MaxDBs: 2,
				Flags: format.NoSync, OpenLock: openLockFile})
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			commit := func(from, to int) error {
				txn, err := env.BeginTxn(nil, 0)
				must(t, err)
				for i := from; i < to; i++ {
					must(t, txn.Put(eng.MainDBI, copyKey(i), copyValue(i, 100), 0))
				}
				return txn.Commit()
			}
			must(t, commit(0, 300))
			info, err := env.Info()
			must(t, err)
			txnid := info.LastTxnID
			env.FailNextMetaSync(syscall.EIO)
			if err := commit(300, 2300); err != syscall.EIO {
				t.Fatalf("commit: %v, want EIO", err)
			}
			gdir := t.TempDir()
			copyInAnotherProcess(t, dir, c.f, gdir)
			checkCopyKeys(t, gdir, txnid, 300)
			ours, err := os.ReadFile(filepath.Join(gdir, "data.mdb"))
			must(t, err)
			_, theirsOut, errs := run(t, c.tool, dir)
			theirs := []byte(theirsOut)
			if sys.MapNeedsInvalidate {
				sameButFailedMetaPage(t, theirs, ours, env.PageSize(), layoutOf(c.f), txnid)
			}
			if !bytes.Equal(theirs, ours) {
				t.Fatalf("C's copy: %s; stderr %s", firstDifference(theirs, ours, env.PageSize()), errs)
			}
			peer := "cpeer"
			if c.f == format.V10 {
				peer = "cpeer10"
			}
			if rc, out, errs := run(t, peer, "check-freelist", gdir); rc != 0 {
				t.Fatalf("check-freelist of the copy:\n%s\n%s", out, errs)
			}
		})
	}
}

// sameButFailedMetaPage accounts for OpenBSD, which has no unified buffer
// cache, in TestCopyAfterFailedMetaSync: neither the failed write of the
// meta page nor the write that puts the old txnid back is followed by
// MDB_Invalidate (in C as here), so the maps of the file still show that
// page as it was before the failed commit. C's mdb_copy copies meta pages
// from its map; the plain copy here reads the file (pread), which holds the
// failed commit's records under the old txnid. Both name the same txnid,
// which no reader of the copy takes (the snapshot's page is newer); theirs
// gets our commit part of that page, for the comparison of the rest.
func sameButFailedMetaPage(t *testing.T, theirs, ours []byte, ps int, l *format.Layout, txnid uint64) {
	t.Helper()
	at := int((txnid+1)&1)*ps + l.MetaOffset()
	if len(theirs) < at+format.MetaSize || len(ours) < at+format.MetaSize {
		return
	}
	cm, okc := format.ReadMeta(theirs[at:])
	gm, okg := format.ReadMeta(ours[at:])
	if !okc || !okg || cm.TxnID != gm.TxnID || cm.TxnID >= txnid {
		t.Fatalf("the failed commit's meta page: C's copy %+v, ours %+v", cm, gm)
	}
	copy(theirs[at+format.MetaMapSize:at+format.MetaSize], ours[at+format.MetaMapSize:at+format.MetaSize])
}
