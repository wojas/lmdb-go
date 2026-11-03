package engine

// Hand-built files for rlmdb's review-round fixes (rlmdb tests/handbuilt.rs):
// 0.9 pages below 2 KiB (KNOWN_C_BUGS.md row 48), a lying md_depth, snapshots
// without their meta pages (row 49), and walks and drops over trees whose
// branch pages share their children (row 50).

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

var bothLayouts = []*format.Layout{format.LayoutV09, format.LayoutV10}

// handbuiltEnv writes file as the data file of a new directory.
func handbuiltEnv(t *testing.T, file []byte) (dir, data string) {
	t.Helper()
	dir = t.TempDir()
	data = filepath.Join(dir, "data.mdb")
	must(t, os.WriteFile(data, file, 0o644))
	return dir, data
}

// emptyFile is a data file of ps-byte pages holding only its meta pages.
func emptyFile(l *format.Layout, ps int) []byte {
	return handbuiltFile(l, ps, nil, format.DB{Root: format.PInvalid})
}

// leafOf is a leaf of l holding keys, each with the value "v".
func leafOf(t *testing.T, l *format.Layout, ps int, pgno uint64, keys ...string) []byte {
	t.Helper()
	p := make([]byte, ps)
	b := format.PageBuilder{B: p, L: l}
	b.Init(pgno, format.PLeaf)
	for i, k := range keys {
		if !b.AddNode(i, []byte(k), format.NodeData{Inline: []byte("v")}, 0) {
			t.Fatalf("leaf %d: node %d", pgno, i)
		}
	}
	return p
}

// branchNode is a separator and a child of branchOf.
type branchNode struct {
	sep   []byte
	child uint64
}

// branchOf is a branch page of l with nodes, the first separator being the
// implicit empty key.
func branchOf(t *testing.T, l *format.Layout, ps int, pgno uint64, nodes ...branchNode) []byte {
	t.Helper()
	p := make([]byte, ps)
	b := format.PageBuilder{B: p, L: l}
	b.Init(pgno, format.PBranch)
	for i, n := range nodes {
		if !b.AddNode(i, n.sep, format.NodeData{IsChild: true, Child: n.child}, 0) {
			t.Fatalf("branch %d: node %d", pgno, i)
		}
	}
	return p
}

// sameHead checks that the data file still starts with file. (Windows
// extends a 0.9 file opened for writing to the map size, with zeros.)
func sameHead(t *testing.T, what, data string, file []byte) {
	t.Helper()
	got, err := os.ReadFile(data)
	must(t, err)
	if len(got) < len(file) || !bytes.Equal(got[:len(file)], file) ||
		!bytes.Equal(got[len(file):], make([]byte, len(got)-len(file))) {
		t.Fatalf("%s: the file changed", what)
	}
}

// bounded runs f on a goroutine of its own and fails if it has not finished
// within a minute: the walks below take hours where they are not bounded
// (the goroutine is then left running until the test process ends).
func bounded(t *testing.T, what string, f func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(time.Minute):
		t.Fatalf("%s: still running after a minute", what)
	}
}

// wantCorrupted is nil for MDB_CORRUPTED, and the error saying what came
// instead otherwise.
func wantCorrupted(what string, err error) error {
	if errors.Is(err, format.Corrupted) {
		return nil
	}
	return fmt.Errorf("%s: %v, want MDB_CORRUPTED", what, err)
}

// TestSmall09PagesAreRefused: 0.9 files of pages below 2 KiB
// (Layout.MinPageSize): C 0.9 creates none (it uses the OS page size) but
// opens them, and its largest key and MDB_DUPSORT item (511 bytes) no longer
// fits their nodes, a P_LEAF2 page holding a single such item: C writes past
// its pages (KNOWN_C_BUGS.md row 48). They are refused with MDB_INVALID,
// read-write and read-only; at 2 KiB pages of items of the largest size
// split as at any other size.
func TestSmall09PagesAreRefused(t *testing.T) {
	l := format.LayoutV09
	for _, ps := range []int{256, 512, 1024, 2048} {
		dir, _ := handbuiltEnv(t, emptyFile(l, ps))
		open := func(flags uint32) (*Env, error) {
			return Open(dir, Options{MaxDBs: 2, Flags: flags, OpenLock: OpenMemLock})
		}
		if ps < l.MinPageSize {
			for _, flags := range []uint32{0, format.ReadOnly} {
				if _, err := open(flags); !errors.Is(err, format.Invalid) {
					t.Fatalf("%d: flags %#x: %v, want MDB_INVALID", ps, flags, err)
				}
			}
			continue
		}
		env, err := open(0)
		must(t, err)
		defer env.Close()
		if env.PageSize() != ps || env.MaxKeySize() != 511 {
			t.Fatalf("%d: page size %d, largest key %d", ps, env.PageSize(), env.MaxKeySize())
		}
		// Three to a page: the sub-DB of "k" splits three times.
		items := make([][]byte, 12)
		for i := range items {
			items[i] = bytes.Repeat([]byte{byte(i * 5 % 12)}, 511)
		}
		txn := beginW(t, env)
		db, err := txn.OpenDBI([]byte("d"), format.DupSort|format.DupFixed|format.Create)
		must(t, err)
		for _, item := range items {
			must(t, txn.Put(db, []byte("k"), item, 0))
		}
		must(t, txn.Commit())
		r := beginR(t, env)
		defer r.Abort()
		db, err = r.OpenDBI([]byte("d"), 0)
		must(t, err)
		c, err := r.OpenCursor(db)
		must(t, err)
		for i := range items {
			_, v, err := c.Get(nil, nil, format.Next)
			must(t, err)
			if len(v) != 511 || v[0] != byte(i) {
				t.Fatalf("%d: item %d: %d bytes of %d", ps, i, len(v), v[0])
			}
		}
		if _, _, err := c.Get(nil, nil, format.Next); err != format.NotFound {
			t.Fatalf("%d: after the last item: %v", ps, err)
		}
	}
}

// TestLeafSizeOfAnOverflowNode: a key that alone fills the node, with data
// shorter than the overflow reference. C's size_t subtraction wraps to the
// right value (rlmdb cafb356); it is that value here.
func TestLeafSizeOfAnOverflowNode(t *testing.T) {
	l := format.LayoutV09
	nodemax := l.NodeMax(4096)
	if got, want := leafSizeOf(l, nodemax, 3, 5000), format.Even(8+3+8+2); got != want {
		t.Fatalf("data on overflow pages: %d, want %d", got, want)
	}
	nodemax = l.NodeMax(256)
	if got, want := leafSizeOf(l, nodemax, 200, 5), format.Even(8+200+8+2); got != want {
		t.Fatalf("short data, long key: %d, want %d", got, want)
	}
}

// TestMergeWithALyingDepthCommits: a main DB record whose md_depth lies,
// 0x8000 over a sound tree of a branch root and two leaves. Nothing checks
// md_depth (C does not); a delete that merges the leaves collapses the root
// and makes it 0x7fff, and mdb_page_merge then adds the change to the
// cursor's height in 16 bits (rlmdb b405e6a). The delete commits, leaving the
// depth C leaves.
func TestMergeWithALyingDepthCommits(t *testing.T) {
	const ps = 4096
	for _, l := range bothLayouts {
		var a, b []string
		for i := 0; i < 5; i++ {
			a, b = append(a, fmt.Sprintf("a%d", i)), append(b, fmt.Sprintf("b%d", i))
		}
		pages := [][]byte{
			branchOf(t, l, ps, 2, branchNode{nil, 3}, branchNode{[]byte("b0"), 4}),
			leafOf(t, l, ps, 3, a...),
			leafOf(t, l, ps, 4, b...),
		}
		main := format.DB{Depth: 0x8000, BranchPages: 1, LeafPages: 2, Entries: 10, Root: 2}
		dir, _ := handbuiltEnv(t, handbuiltFile(l, ps, pages, main))
		env, err := Open(dir, Options{MaxDBs: 2, OpenLock: OpenMemLock})
		must(t, err)
		defer env.Close()
		txn := beginW(t, env)
		must(t, txn.Del(MainDBI, []byte("b0"), nil))
		must(t, txn.Commit())
		r := beginR(t, env)
		defer r.Abort()
		st, err := r.Stat(MainDBI)
		must(t, err)
		if st.Depth != 0x7fff || st.BranchPages != 0 || st.LeafPages != 1 {
			t.Fatalf("%v: %+v", l.Format, st)
		}
		c, err := r.OpenCursor(MainDBI)
		must(t, err)
		for _, want := range append(a, b[1:]...) {
			k, _, err := c.Get(nil, nil, format.Next)
			must(t, err)
			if string(k) != want {
				t.Fatalf("%v: key %q, want %q", l.Format, k, want)
			}
		}
	}
}

// writeBehind writes b at off of the data file through a descriptor, as
// another process would; where maps do not show such writes (OpenBSD:
// sys.MapNeedsInvalidate), the mapped copy is dropped as the writer does.
func writeBehind(t *testing.T, env *Env, data string, b []byte, off int64) {
	t.Helper()
	f, err := os.OpenFile(data, os.O_RDWR, 0)
	must(t, err)
	defer f.Close()
	_, err = f.WriteAt(b, off)
	must(t, err)
	if sys.MapNeedsInvalidate {
		m, err := env.acquireMap()
		must(t, err)
		defer m.Release()
		m.Invalidate(uint64(off), len(b))
	}
}

// TestSnapshotsWithoutTheirMetaPagesAreRefused: meta pages whose last page
// is below the meta pages (or the largest value): a writer takes
// mm_last_pg + 1 as its next new page, so with 0 the first put of a
// transaction built its root leaf on page 1 and the commit wrote it over meta
// page 1, after which the environment no longer opened; C does the same
// (KNOWN_C_BUGS.md row 49). Every transaction on such a snapshot fails to begin
// with MDB_CORRUPTED and the file stays as it was: for 0 recorded when the
// environment is opened, and for the largest value (which the open refuses)
// written into the newest meta page after the open, as another process could.
func TestSnapshotsWithoutTheirMetaPagesAreRefused(t *testing.T) {
	const ps = 4096
	for _, l := range bothLayouts {
		setLast := func(file []byte, meta int, last uint64) {
			binary.NativeEndian.PutUint64(file[meta*ps+l.MetaOffset()+format.MetaLastPgno:], last)
		}
		refused := func(env *Env, how string) {
			t.Helper()
			if _, err := env.BeginTxn(nil, format.ReadOnly); !errors.Is(err, format.Corrupted) {
				t.Fatalf("%v: %s: read: %v, want MDB_CORRUPTED", l.Format, how, err)
			}
			if _, err := env.BeginTxn(nil, 0); !errors.Is(err, format.Corrupted) {
				t.Fatalf("%v: %s: write: %v, want MDB_CORRUPTED", l.Format, how, err)
			}
		}

		file := emptyFile(l, ps)
		setLast(file, 0, 0)
		setLast(file, 1, 0)
		dir, data := handbuiltEnv(t, file)
		env, err := Open(dir, Options{MaxDBs: 2, OpenLock: OpenMemLock})
		must(t, err)
		refused(env, "last page 0")
		env.Close()
		sameHead(t, l.Format.String(), data, file)

		file = emptyFile(l, ps)
		dir, data = handbuiltEnv(t, file)
		env, err = Open(dir, Options{MaxDBs: 2, OpenLock: OpenMemLock})
		must(t, err)
		beginW(t, env).Abort()
		// Meta page 1 holds the newest snapshot (txnid 1).
		setLast(file, 1, ^uint64(0))
		writeBehind(t, env, data, file[ps:2*ps], ps)
		refused(env, "the largest last page")
		env.Close()
		sameHead(t, l.Format.String(), data, file)
	}
}

// sharedLevels: 20 levels of 4 make a 24-page file that a walk crosses 4^20
// times.
const sharedLevels = 20

// sharedTree is a damaged tree whose branch pages share their children: page
// first is a leaf holding kvs, and each of the levels branch pages above it
// (the last is the root) names the page below fanout times, under the
// separators sep(1), sep(2), ... A walk reaches the leaf fanout^levels times.
// The pages, in page number order.
func sharedTree(t *testing.T, l *format.Layout, ps int, first uint64, levels, fanout int, kvs [][2][]byte, sep func(int) []byte) [][]byte {
	t.Helper()
	p := make([]byte, ps)
	b := format.PageBuilder{B: p, L: l}
	b.Init(first, format.PLeaf)
	for i, kv := range kvs {
		if !b.AddNode(i, kv[0], format.NodeData{Inline: kv[1]}, 0) {
			t.Fatalf("leaf: node %d", i)
		}
	}
	pages := [][]byte{p}
	for pgno := first + 1; pgno <= first+uint64(levels); pgno++ {
		nodes := make([]branchNode, fanout)
		for i := range nodes {
			nodes[i].child = pgno - 1
			if i > 0 {
				nodes[i].sep = sep(i)
			}
		}
		pages = append(pages, branchOf(t, l, ps, pgno, nodes...))
	}
	return pages
}

// sharedDB is the record of a sharedTree.
func sharedDB(first uint64, levels int, entries uint64) format.DB {
	return format.DB{Depth: uint16(levels + 1), BranchPages: uint64(levels), LeafPages: 1,
		Entries: entries, Root: first + uint64(levels)}
}

var sharedLeafKV = [][2][]byte{
	{[]byte("a"), []byte("1")}, {[]byte("m"), []byte("2")}, {[]byte("z"), []byte("3")},
}

func sharedSep(i int) []byte { return []byte{byte('a' + i)} }

// TestWalksOfAFreeDBWithSharedChildrenStop: a free DB whose branch pages
// share their children (20 levels of fan-out 4 over one record: txnid 1, no
// pages) under a main DB of one key. Walking it reads the record 4^20 times,
// which C does, without end in practice, in mdb_page_alloc holding the writer
// lock (every writer of the environment then waits), in the compacting copy's
// count of free pages and in mdb_stat -f. Free DB keys are unique and ascend,
// so each of these fails with MDB_CORRUPTED at the second sight of the record
// (KNOWN_C_BUGS.md row 50); a plain cursor walk is stopped by its step limit
// (Txn.walkLimit). The file is unchanged.
func TestWalksOfAFreeDBWithSharedChildrenStop(t *testing.T) {
	const ps = 4096
	for _, l := range bothLayouts {
		var key, none [8]byte
		binary.NativeEndian.PutUint64(key[:], 1)
		record := [][2][]byte{{key[:], none[:]}}
		mainPgno := uint64(3 + sharedLevels)
		pages := sharedTree(t, l, ps, 2, sharedLevels, 4, record, func(i int) []byte {
			return binary.NativeEndian.AppendUint64(nil, uint64(i))
		})
		pages = append(pages, leafOf(t, l, ps, mainPgno, "a"))
		main := format.DB{Depth: 1, LeafPages: 1, Entries: 1, Root: mainPgno}
		// Snapshot 9: the record is old enough to be reused.
		file := handbuiltFileDBs(l, ps, pages, sharedDB(2, sharedLevels, 1), main, 9)
		dir, data := handbuiltEnv(t, file)
		compact := t.TempDir()
		bounded(t, l.Format.String(), func() error {
			env, err := Open(dir, Options{MaxDBs: 2, OpenLock: OpenMemLock})
			if err != nil {
				return err
			}
			defer env.Close()
			txn, err := env.BeginTxn(nil, 0)
			if err != nil {
				return err
			}
			if err := wantCorrupted("put", txn.Put(MainDBI, []byte("b"), []byte("v"), 0)); err != nil {
				return err
			}
			if err := txn.Commit(); !errors.Is(err, format.BadTxn) {
				return fmt.Errorf("commit after the put: %v, want MDB_BAD_TXN", err)
			}
			_, err = env.FreePages()
			if err := wantCorrupted("FreePages", err); err != nil {
				return err
			}
			if err := wantCorrupted("compacting copy", env.Copy(compact, format.CopyCompact)); err != nil {
				return err
			}
			r, err := env.BeginTxn(nil, format.ReadOnly)
			if err != nil {
				return err
			}
			defer r.Abort()
			c, err := r.OpenCursor(FreeDBI)
			if err != nil {
				return err
			}
			for {
				if _, _, err = c.Get(nil, nil, format.Next); err != nil {
					return wantCorrupted("cursor walk", err)
				}
			}
		})
		sameHead(t, l.Format.String(), data, file)
	}
}

// TestCursorWalksOverSharedChildrenStop: a main DB whose branch pages share
// their children (20 levels of fan-out 4 over a leaf of three keys): a cursor
// walk returns the three keys over and over, 4^20 times in C. A walk stops
// with MDB_CORRUPTED once it has stepped onto more leaves than the snapshot
// has pages (KNOWN_C_BUGS.md row 50), forward and backward, in read and in write
// transactions, having returned each key at most that many times.
func TestCursorWalksOverSharedChildrenStop(t *testing.T) {
	const ps = 4096
	for _, l := range bothLayouts {
		pages := sharedTree(t, l, ps, 2, sharedLevels, 4, sharedLeafKV, sharedSep)
		file := handbuiltFile(l, ps, pages, sharedDB(2, sharedLevels, 3))
		dir, _ := handbuiltEnv(t, file)
		most := uint64(3 * (len(file)/ps + 1))
		bounded(t, l.Format.String(), func() error {
			env, err := Open(dir, Options{MaxDBs: 2, OpenLock: OpenMemLock})
			if err != nil {
				return err
			}
			defer env.Close()
			for _, op := range []format.CursorOp{format.Next, format.Prev} {
				for _, flags := range []uint32{format.ReadOnly, 0} {
					txn, err := env.BeginTxn(nil, flags)
					if err != nil {
						return err
					}
					c, err := txn.OpenCursor(MainDBI)
					if err != nil {
						txn.Abort()
						return err
					}
					var items uint64
					for err == nil {
						if _, _, err = c.Get(nil, nil, op); err == nil {
							items++
						}
					}
					txn.Abort()
					if err := wantCorrupted(fmt.Sprintf("op %d flags %#x", op, flags), err); err != nil {
						return err
					}
					if items <= 3 || items > most {
						return fmt.Errorf("op %d flags %#x: %d items", op, flags, items)
					}
				}
			}
			return nil
		})
	}
}

// TestQueueWalksInAWriteTxnAreNotBoundedByTheFile: a sound tree that a write
// transaction keeps changing under a walking cursor, as a queue: at each step
// the cursor moves to the next item, one is appended at the end and the one
// 1000 items behind the cursor is deleted. Pages emptied behind the cursor
// are reused for the new leaves ahead of it, so the cursor steps onto many
// more leaves than the file has pages; each of those leaves was allocated in
// the transaction, which Txn.walkLimit counts, so the walk is not taken for
// one over shared children.
func TestQueueWalksInAWriteTxnAreNotBoundedByTheFile(t *testing.T) {
	const items, lag, steps = 2000, 1000, 40000
	env, _ := openTest(t, format.V09, Options{MapSize: 64 << 20})
	key := func(i int) []byte { return []byte(fmt.Sprintf("q%07d", i)) }
	value := bytes.Repeat([]byte("v"), 100)
	txn := beginW(t, env)
	for i := 0; i < items; i++ {
		must(t, txn.Put(MainDBI, key(i), value, 0))
	}
	must(t, txn.Commit())

	txn = beginW(t, env)
	c, err := txn.OpenCursor(MainDBI)
	must(t, err)
	k, _, err := c.Get(nil, nil, format.First)
	for i := 0; i < steps; i++ {
		must(t, err)
		if !bytes.Equal(k, key(i)) {
			t.Fatalf("step %d: key %q", i, k)
		}
		must(t, txn.Put(MainDBI, key(items+i), value, 0))
		if i >= lag {
			must(t, txn.Del(MainDBI, key(i-lag), nil))
		}
		k, _, err = c.Get(nil, nil, format.Next)
	}
	must(t, err)
	must(t, txn.Commit())
	// An item takes more than 110 bytes of a page: the walk stepped onto at
	// least this many leaves.
	leaves := uint64(steps/(env.PageSize()/110) - 1)
	info, err := env.Info()
	must(t, err)
	if pages := info.LastPgno + 1; leaves <= 2*pages {
		t.Fatalf("%d leaves, %d pages", leaves, pages)
	}
}

// TestDropsOfSharedChildrenStop: emptying a main DB whose branch pages share
// their children: C's mdb_drop0 puts every page on the free list once per
// path to it, so the commit stores a free list naming pages twice (a 7-page
// file of 3 levels of fan-out 3: 41 entries) and every later write
// transaction hands a page out twice; with 20 levels of 4 the list outgrows
// memory first. Drop fails with MDB_CORRUPTED once the walk has visited more
// pages, or listed more, than the file has (KNOWN_C_BUGS.md row 50), also where
// it scans the leaves (here because md_overflow_pages says there are overflow
// pages), and the file stays as it was. A root naming one leaf twice gets past
// that (three entries in a file of four pages); its commit fails, since the
// free list is not strictly descending, and the environment takes writes
// again.
func TestDropsOfSharedChildrenStop(t *testing.T) {
	const ps = 4096
	for _, l := range bothLayouts {
		for _, shape := range []struct {
			levels, fanout int
			overflow       uint64
			name           string
		}{
			{3, 3, 0, "3 levels of 3"},
			{sharedLevels, 4, 0, "20 levels of 4"},
			{sharedLevels, 4, 1, "20 levels of 4, leaves scanned"},
		} {
			what := l.Format.String() + ": " + shape.name
			pages := sharedTree(t, l, ps, 2, shape.levels, shape.fanout, sharedLeafKV, sharedSep)
			main := sharedDB(2, shape.levels, 3)
			main.OverflowPages = shape.overflow
			file := handbuiltFile(l, ps, pages, main)
			dir, data := handbuiltEnv(t, file)
			bounded(t, what, func() error {
				env, err := Open(dir, Options{MaxDBs: 2, OpenLock: OpenMemLock})
				if err != nil {
					return err
				}
				defer env.Close()
				txn, err := env.BeginTxn(nil, 0)
				if err != nil {
					return err
				}
				if err := wantCorrupted("drop", txn.Drop(MainDBI, false)); err != nil {
					txn.Abort()
					return err
				}
				if err := txn.Commit(); !errors.Is(err, format.BadTxn) {
					return fmt.Errorf("commit: %v, want MDB_BAD_TXN", err)
				}
				return nil
			})
			sameHead(t, what, data, file)
		}

		what := l.Format.String() + ": one leaf twice"
		pages := [][]byte{
			leafOf(t, l, ps, 2, "a", "m"),
			branchOf(t, l, ps, 3, branchNode{nil, 2}, branchNode{[]byte("m"), 2}),
		}
		main := format.DB{Depth: 2, BranchPages: 1, LeafPages: 1, Entries: 2, Root: 3}
		file := handbuiltFile(l, ps, pages, main)
		dir, data := handbuiltEnv(t, file)
		env, err := Open(dir, Options{MaxDBs: 2, OpenLock: OpenMemLock})
		must(t, err)
		txn := beginW(t, env)
		must(t, txn.Drop(MainDBI, false))
		if err := txn.Commit(); !errors.Is(err, format.Corrupted) {
			t.Fatalf("%s: commit: %v, want MDB_CORRUPTED", what, err)
		}
		txn = beginW(t, env)
		must(t, txn.Put(MainDBI, []byte("b"), []byte("2"), 0))
		must(t, txn.Commit())
		env.Close()
		got, err := os.ReadFile(data)
		must(t, err)
		if !bytes.Equal(got[2*ps:len(file)], file[2*ps:]) {
			t.Fatalf("%s: pages changed", what)
		}
	}
}
