package engine

// A meta page write that fails after its bytes reached the file, and
// plain copies of a file holding such a meta page (rlmdb bf3a0c0,
// KNOWN_C_BUGS.md #41).

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// putBig commits a value of n bytes under k.
func putBig(t *testing.T, env *Env, k string, n int) error {
	t.Helper()
	w := beginW(t, env)
	if err := w.Put(MainDBI, []byte(k), bytes.Repeat([]byte("B"), n), 0); err != nil {
		w.Abort()
		t.Fatal(err)
	}
	return w.Commit()
}

// copyAndRead copies env (plain copy) into a new directory and returns
// the value of k in the copy, and the copy's audit.
func copyAndRead(t *testing.T, env *Env, k string) ([]byte, error, error) {
	t.Helper()
	dir := t.TempDir()
	if err := env.Copy(dir, 0); err != nil {
		return nil, err, nil
	}
	cp, err := Open(dir, Options{Flags: format.NoLock | format.ReadOnly})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer cp.Close()
	r := beginR(t, cp)
	defer r.Abort()
	v, err := r.Get(MainDBI, []byte(k))
	return bytes.Clone(v), err, cp.Audit()
}

// fillKeys commits keys from..from+n-1 (key%06d), each with 300 bytes of
// its number.
func fillKeys(t *testing.T, env *Env, from, n int) {
	t.Helper()
	w := beginW(t, env)
	for i := from; i < from+n; i++ {
		if err := w.Put(MainDBI, []byte(fmt.Sprintf("key%06d", i)), bytes.Repeat([]byte{byte(i)}, 300), 0); err != nil {
			w.Abort()
			t.Fatal(err)
		}
	}
	must(t, w.Commit())
}

// assertKeys checks that the newest snapshot of env holds the keys keys
// of fillKeys, with their values, and no others.
func assertKeys(t *testing.T, env *Env, keys []int) {
	t.Helper()
	got := scan(t, env, MainDBI)
	if len(got) != len(keys) {
		t.Fatalf("%d keys, want %d", len(got), len(keys))
	}
	for j, i := range keys {
		if got[j][0] != fmt.Sprintf("key%06d", i) || got[j][1] != string(bytes.Repeat([]byte{byte(i)}, 300)) {
			t.Fatalf("item %d: key %q, want key%06d", j, got[j][0], i)
		}
	}
}

// snapshotID is the txnid a new read transaction of env reads.
func snapshotID(t *testing.T, env *Env) uint64 {
	t.Helper()
	r := beginR(t, env)
	defer r.Abort()
	return r.ID()
}

// fileTxnIDs is mm_txnid of meta pages 0 and 1 as the data file of env
// holds them (not as the map shows them).
func fileTxnIDs(t *testing.T, env *Env, dir string) [2]uint64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "data.mdb"))
	must(t, err)
	var ids [2]uint64
	for i := range ids {
		off := i*env.PageSize() + env.layout.MetaOffset() + format.MetaTxnID
		ids[i], _ = format.Get64(data, off)
	}
	return ids
}

// A sync that fails after the meta page reached the file (on macOS the
// F_FULLFSYNC after the O_DSYNC write; injected here, after the write, on
// every platform) is a failed meta page write: the environment is unusable
// (MDB_FATAL_ERROR: transactions and copies fail with MDB_PANIC), and the
// old txnid goes back into the page, so the file holds no commit newer
// than the published one (review finding, round 2, P2-4: the file kept
// meta N+1 while N was published, and a plain copy pinned N but copied
// meta N+1, naming pages it left out). Without a lock file, the txnid the
// file's handles share stays where it was, and another handle goes on
// reading, copying and writing. (rlmdb robust_failed_meta_sync_is_fatal;
// with a lock file a copy by another process is in tests/engine.)
func TestMetaWriteFailureIsFatal(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, flags := range []uint32{0, format.NoLock} {
			t.Run(fmt.Sprintf("%v/flags=%#x", f, flags), func(t *testing.T) {
				failedMetaSync(t, f, flags)
			})
		}
	}
}

func failedMetaSync(t *testing.T, f format.Format, flags uint32) {
	env, dir := openTest(t, f, Options{Flags: flags})
	fillKeys(t, env, 0, 300)
	txnid := snapshotID(t, env)
	info, err := env.Info()
	must(t, err)
	// The end of the snapshot's pages. (Not the file's size: on Windows a
	// 0.9 file opened for writing is as long as its map.)
	end := int64(info.LastPgno+1) * int64(env.PageSize())
	// The page the commit writes, and the old txnid it holds.
	slot := int((txnid + 1) & 1)
	old := fileTxnIDs(t, env, dir)[slot]
	if old >= txnid {
		t.Fatalf("meta page %d holds txnid %d, newer than %d", slot, old, txnid)
	}
	var other *Env
	if flags&format.NoLock != 0 {
		if other, err = Open(dir, Options{Format: f, Flags: flags | format.NoSync, MapSize: 64 << 20}); err != nil {
			t.Fatal(err)
		}
		defer other.Close()
	}

	// A commit whose pages go past the snapshot's last page.
	w := beginW(t, env)
	for i := 300; i < 2300; i++ {
		must(t, w.Put(MainDBI, []byte(fmt.Sprintf("key%06d", i)), bytes.Repeat([]byte{byte(i)}, 300), 0))
	}
	env.FailNextMetaSync(syscall.EIO)
	if err := w.Commit(); err != syscall.EIO {
		t.Fatalf("commit: %v, want EIO", err)
	}
	if !pageWritten(t, filepath.Join(dir, "data.mdb"), end, env.PageSize()) {
		t.Fatal("the commit wrote no new pages")
	}
	// MDB_FATAL_ERROR: every transaction and copy fails with MDB_PANIC.
	if err := env.BeginRead(new(Txn)); err != format.Panic {
		t.Fatalf("read transaction: %v, want MDB_PANIC", err)
	}
	if _, err := env.BeginTxn(nil, 0); err != format.Panic {
		t.Fatalf("write transaction: %v, want MDB_PANIC", err)
	}
	if _, err, _ := copyAndRead(t, env, "key000000"); err != format.Panic {
		t.Fatalf("copy: %v, want MDB_PANIC", err)
	}
	// The page that was written has the old txnid again.
	ids := fileTxnIDs(t, env, dir)
	if ids[slot] != old || ids[slot^1] != txnid {
		t.Fatalf("meta pages hold txnids %v, want %d in page %d and %d in the other", ids, old, slot, txnid)
	}
	keys := make([]int, 0, 320)
	for i := 0; i < 300; i++ {
		keys = append(keys, i)
	}
	if other != nil {
		// A plain copy by another handle: its newest meta page is the
		// published snapshot, whose pages it holds.
		if id := snapshotID(t, other); id != txnid {
			t.Fatalf("the other handle reads txnid %d, want %d", id, txnid)
		}
		cdir := t.TempDir()
		must(t, other.Copy(cdir, 0))
		cp, err := Open(cdir, Options{Format: f, Flags: format.NoLock | format.ReadOnly})
		must(t, err)
		if info, err := cp.Info(); err != nil || info.LastTxnID != txnid {
			t.Fatalf("the copy's newest meta page: txnid %d, %v; want %d", info.LastTxnID, err, txnid)
		}
		assertKeys(t, cp, keys)
		mustAudit(t, cp)
		cp.Close()
		// The other handle writes on, from the published snapshot.
		fillKeys(t, other, 5000, 10)
		for i := 5000; i < 5010; i++ {
			keys = append(keys, i)
		}
		assertKeys(t, other, keys)
		if ids = fileTxnIDs(t, env, dir); ids[slot] != txnid+1 {
			t.Fatalf("meta page %d: txnid %d after the other handle's commit, want %d", slot, ids[slot], txnid+1)
		}
		other.Close()
	}
	last := max(ids[0], ids[1])
	env.Close()
	env2, err := Open(dir, Options{Format: f, Flags: flags | format.NoSync, OpenLock: OpenMemLock})
	must(t, err)
	defer env2.Close()
	if id := snapshotID(t, env2); id != last {
		t.Fatalf("after the re-open: txnid %d, want %d", id, last)
	}
	assertKeys(t, env2, keys)
	mustAudit(t, env2)
	if v, err, aerr := copyAndRead(t, env2, "key000001"); err != nil || len(v) != 300 || aerr != nil {
		t.Fatalf("copy after the re-open: %d bytes, %v, audit %v", len(v), err, aerr)
	}
	fillKeys(t, env2, 6000, 10)
	for i := 6000; i < 6010; i++ {
		keys = append(keys, i)
	}
	assertKeys(t, env2, keys)
}

// A plain copy writes no meta page newer than the snapshot it copies: one
// that names pages past the snapshot's end gets the snapshot's meta
// instead. Such a meta page is on the file after a commit whose meta page
// write or sync failed in another process (C and rlmdb leave it there,
// see TestMetaWriteFailureIsFatal; here it is forged by publishing the
// older txnid again). Before, the copy named pages it did not contain
// (MDB_PAGE_NOTFOUND reading it). C's mdb_copy copies both meta pages as
// they are.
func TestCopyMetasNotNewerThanSnapshot(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			env, _ := openTest(t, f, Options{})
			putKV(t, env, "k", "old")
			if err := putBig(t, env, "k", 100000); err != nil {
				t.Fatal(err)
			}
			env.lock.SetTxnID(1)
			env.writer.hasLastMeta = false
			v, err, aerr := copyAndRead(t, env, "k")
			if err != nil || string(v) != "old" || aerr != nil {
				t.Fatalf("copy: %q, %v, audit %v", v, err, aerr)
			}
		})
	}
}

// pageWritten reports whether the file at path holds a page at off that is
// not all zeros (one the engine wrote; the pages past the end of a file
// that was extended read as zeros).
func pageWritten(t *testing.T, path string, off int64, ps int) bool {
	t.Helper()
	f, err := os.Open(path)
	must(t, err)
	defer f.Close()
	buf := make([]byte, ps)
	n, _ := f.ReadAt(buf, off)
	return n == ps && !bytes.Equal(buf, make([]byte, ps))
}
