package engine

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// snapshotPages is the page numbers reachable from txn's snapshot (all
// trees, overflow runs, sub-DBs).
func snapshotPages(t *testing.T, txn *Txn) []uint64 {
	t.Helper()
	a := &auditor{t: txn, used: map[uint64]string{}}
	free, main := txn.dbe(format.FreeDBI).db, txn.dbe(format.MainDBI).db
	if err := a.tree("free", &free); err != nil {
		t.Fatal(err)
	}
	if err := a.tree("main", &main); err != nil {
		t.Fatal(err)
	}
	out := make([]uint64, 0, len(a.used))
	for p := range a.used {
		out = append(out, p)
	}
	return out
}

// longReader runs the workload of TestLongReader on env: a reader begins
// after the first fill and, if keep, stays across 40 commits that rewrite,
// delete and re-add most keys. It checks that the reader's zero-copy
// values and the bytes of every page of its snapshot never change. Returns
// the last page number in use at the end.
func longReader(t *testing.T, env *Env, keep bool) uint64 {
	put := func(round int) {
		txn, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 1500; i++ {
			k := []byte(fmt.Sprintf("k%05d", i))
			switch {
			case round > 0 && (i+round)%3 == 0:
				if err := txn.Del(MainDBI, k, nil); err != nil && err != format.NotFound {
					t.Fatal(err)
				}
			default:
				v := bytes.Repeat([]byte{byte(round), byte(i)}, 20+(i*round)%200)
				if i%50 == 0 {
					v = bytes.Repeat([]byte{byte(round)}, 5000)
				}
				if err := txn.Put(MainDBI, k, v, 0); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := txn.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	put(0)
	r, err := env.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Abort()
	// Zero-copy values and their expected contents.
	type item struct{ raw, want []byte }
	var items []item
	c, _ := r.OpenCursor(MainDBI)
	for {
		_, v, err := c.Get(nil, nil, format.Next)
		if err == format.NotFound {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item{v, append([]byte(nil), v...)})
	}
	pages := snapshotPages(t, r)
	saved := map[uint64][]byte{}
	ps := env.PageSize()
	for _, p := range pages {
		b := make([]byte, ps)
		if _, err := sys.ReadAt(env.DataFile(), b, int64(p)*int64(ps)); err != nil {
			t.Fatal(err)
		}
		saved[p] = b
	}
	if !keep {
		r.Abort()
	}
	for round := 1; round <= 40; round++ {
		put(round)
		if !keep {
			continue
		}
		for i, it := range items {
			if !bytes.Equal(it.raw, it.want) {
				t.Fatalf("round %d: the reader's value %d changed", round, i)
			}
		}
		b := make([]byte, ps)
		for _, p := range pages {
			if _, err := sys.ReadAt(env.DataFile(), b, int64(p)*int64(ps)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b, saved[p]) {
				t.Fatalf("round %d: page %d of the reader's snapshot was rewritten", round, p)
			}
		}
	}
	if keep {
		// A fresh scan through the old snapshot.
		c, _ := r.OpenCursor(MainDBI)
		for i := 0; ; i++ {
			_, v, err := c.Get(nil, nil, format.Next)
			if err == format.NotFound {
				if i != len(items) {
					t.Fatalf("rescan: %d items, want %d", i, len(items))
				}
				break
			}
			if err != nil || !bytes.Equal(v, items[i].want) {
				t.Fatalf("rescan item %d: %v", i, err)
			}
		}
	}
	if err := env.Audit(); err != nil {
		t.Fatal(err)
	}
	in, _ := env.Info()
	return in.LastPgno
}

// TestLongReader: a read transaction that stays open across many commits
// keeps its snapshot intact, under NoLock (the in-memory reader table,
// AGENTS.md rule 2a) exactly as with a lock region; and the workload does
// reuse pages when no reader holds them.
func TestLongReader(t *testing.T) {
	forFormats(t, func(t *testing.T, fm format.Format) {
		last := map[string]uint64{}
		for _, mode := range []string{"nolock", "lock"} {
			for _, keep := range []bool{true, false} {
				opts := Options{}
				if mode == "nolock" {
					opts.Flags = format.NoLock
					opts.OpenLock = func(string, os.FileMode, uint32, bool, format.Format) (LockRegion, error) {
						t.Fatal("OpenLock called under NoLock")
						return nil, nil
					}
				}
				env, _ := openTest(t, fm, opts)
				if mode == "nolock" && env.HasLockFile() {
					t.Fatal("NoLock env has a lock file")
				}
				name := fmt.Sprintf("%s-%v", mode, keep)
				last[name] = longReader(t, env, keep)
				env.Close()
			}
		}
		t.Logf("last pages: %v", last)
		if last["nolock-true"] != last["lock-true"] || last["nolock-false"] != last["lock-false"] {
			t.Fatalf("NoLock and the lock region differ: %v", last)
		}
		if last["nolock-false"] >= last["nolock-true"] {
			t.Fatalf("no page reuse without the reader: %v", last)
		}
	})
}

// TestNoLockObservables: what C reports without a lock file.
func TestNoLockObservables(t *testing.T) {
	env, dir := openTest(t, format.V09, Options{Flags: format.NoLock})
	r, err := env.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Abort()
	in, err := env.Info()
	if err != nil || in.NumReaders != 0 || in.MaxReaders != format.DefaultReaders {
		t.Fatalf("info %+v %v", in, err)
	}
	if env.ReaderList() != nil {
		t.Fatal("reader list without a lock file")
	}
	if n, err := env.ReaderCheck(); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := sys.FileSize(env.DataFile()); err != nil {
		t.Fatal(err)
	}
	if fileExists(dir + "/lock.mdb") {
		t.Fatal("NoLock created a lock file")
	}
	// A second open of the same path in this process is refused.
	if _, err := Open(dir, Options{Flags: format.NoLock}); err == nil {
		t.Fatal("second open succeeded")
	} else if err != syscall.EBUSY {
		t.Fatalf("second open: %v", err)
	}
	// Many readers: no limit without a lock file, as in C.
	var txns []*Txn
	for i := 0; i < 300; i++ {
		tx, err := env.BeginTxn(nil, format.ReadOnly)
		if err != nil {
			t.Fatal(i, err)
		}
		txns = append(txns, tx)
	}
	for _, tx := range txns {
		tx.Abort()
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
