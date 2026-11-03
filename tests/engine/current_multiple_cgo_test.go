//go:build cgo

package engine

// MDB_MULTIPLE with MDB_CURRENT on a duplicate of a MDB_DUPFIXED sub-DB
// near the boundary between two leaves (rlmdb KNOWN_C_BUGS.md row 56; rlmdb
// tests/write_api.rs current_dup_outside_its_leaf covers MDB_MULTIPLE
// with one item, without C). C's sub-cursor put replaces the item under
// the cursor with each item in turn, in place. With one item that is the
// MDB_CURRENT put of row 56: where the new item ends up in a leaf searches do
// not look in, the Go writer, as rlmdb, deletes the item and puts the new
// one, and reports one item stored. With two or more items it is C's in
// place replacement, also where that leaves the last item out of reach
// (as rlmdb). rlmdb's writeops has no MDB_MULTIPLE op, so the oracle here
// is C 0.9 in this process: tests/cgolmdb (lmdb-go v1 on C 0.9.36 plus
// ITS#10615, ITS#10642 and ITS#10643, the C of the oracles), on its own
// environment next to the Go engine's, compared step by step and then
// page for page.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
	cgolmdb "github.com/PowerDNS/lmdb-go/tests/cgolmdb"
	"github.com/PowerDNS/lmdb-go/tests/internal/coracle"
)

// multiOp is one step of the script of TestCgoCurrentMultipleOutsideLeaf.
type multiOp struct {
	kind  string // "put", "getboth", "del", "multi", "first", "next"
	cur   int
	key   []byte
	items [][]byte // "multi": the items, all of one size
	// routed ("multi" of one item): C deletes and puts (mdb_cursor_del,
	// mdb_cursor_put with flags 0), which is what the Go writer's
	// MDB_MULTIPLE | MDB_CURRENT must write here.
	routed bool
	// slack ("multi"): the Go side's buffer has half an item more, which
	// is not an item (the C side cannot pass that through lmdb-go v1).
	slack bool
}

// cgoCode is the C error code of an error of tests/cgolmdb. On Windows
// lmdb-go v1 turns the C runtime's errno values into Go's own numbers of
// the syscall constants (cgolmdb/error_windows.go); coracle.CErrno turns
// them back, as code does for the Go engine's.
func cgoCode(err error) int {
	if err == nil {
		return 0
	}
	var oe *cgolmdb.OpError
	if errors.As(err, &oe) {
		switch e := oe.Errno.(type) {
		case cgolmdb.Errno:
			return int(e)
		case syscall.Errno:
			return coracle.CErrno(int(e))
		}
	}
	return -1
}

// cgoSide runs the script on C 0.9 through tests/cgolmdb.
type cgoSide struct {
	env *cgolmdb.Env
	txn *cgolmdb.Txn
	cur []*cgolmdb.Cursor
}

func (s *cgoSide) step(op multiOp) int {
	c := s.cur[op.cur]
	var err error
	switch op.kind {
	case "put":
		err = s.txn.Put(s.cur[0].DBI(), op.key, op.items[0], 0)
	case "getboth":
		_, _, err = c.Get(op.key, op.items[0], cgolmdb.GetBoth)
	case "del":
		err = c.Del(0)
	case "first":
		_, _, err = c.Get(nil, nil, cgolmdb.First)
	case "next":
		_, _, err = c.Get(nil, nil, cgolmdb.Next)
	case "multi":
		if op.routed {
			if err = c.Del(0); err == nil {
				err = c.Put(op.key, op.items[0], 0)
			}
		} else {
			err = c.PutMulti(op.key, bytes.Join(op.items, nil), len(op.items[0]), cgolmdb.Current)
		}
	}
	return cgoCode(err)
}

func (s *cgoSide) position(i int) string {
	k, v, err := s.cur[i].Get(nil, nil, cgolmdb.GetCurrent)
	return fmt.Sprintf("%d:%x:%x", cgoCode(err), k, v)
}

// goSide runs the script on the Go engine.
type goSide struct {
	t   *testing.T
	txn *eng.Txn
	dbi eng.DBI
	cur []*eng.Cursor
}

func (s *goSide) step(op multiOp) int {
	c := s.cur[op.cur]
	var err error
	switch op.kind {
	case "put":
		err = s.txn.Put(s.dbi, op.key, op.items[0], 0)
	case "getboth":
		_, _, err = c.Get(op.key, op.items[0], format.GetBoth)
	case "del":
		err = c.Del(0)
	case "first":
		_, _, err = c.Get(nil, nil, format.First)
	case "next":
		_, _, err = c.Get(nil, nil, format.Next)
	case "multi":
		page := bytes.Join(op.items, nil)
		if op.slack {
			page = append(page, op.items[0][:len(op.items[0])/2]...)
		}
		var n int
		n, err = c.PutMulti(op.key, page, len(op.items[0]), format.Current)
		if err == nil && n != len(op.items) {
			// C's data[1].mv_size: one per item replaced.
			s.t.Fatalf("MDB_MULTIPLE of %d items stored %d", len(op.items), n)
		}
	}
	return code(err)
}

func (s *goSide) position(i int) string {
	k, v, err := s.cur[i].Get(nil, nil, format.GetCurrent)
	return fmt.Sprintf("%d:%x:%x", code(err), k, v)
}

// currentMultipleScript puts n items of itemLen bytes under one key of a
// MDB_DUPSORT | MDB_DUPFIXED DB, then, from the right, at one boundary
// between two leaves of the sub-DB at a time, replaces with MDB_MULTIPLE |
// MDB_CURRENT, cursor 0 on the item, cursor 1 on the item after the next
// one where the Go writer deletes and inserts and on the item itself
// elsewhere (where a delete would move it off): one item, the leaf's first
// by one before it (in place); after deleting the leaf's first item, the
// previous leaf's last by the deleted item or one after it (routed, also
// with half an item of slack in the Go side's buffer) or by one before it
// (in place); two items, the previous leaf's last after the delete (C in
// place: the last item stays where searches do not look), an item inside
// a leaf, and the leaf's first. Then every new item is looked up, items
// between new ones and the old ones are put, and the database is walked.
func currentMultipleScript(t *testing.T, key []byte, itemLen, n int) []multiOp {
	dbflags := uint32(format.DupSort | format.DupFixed)
	item := func(j int) []byte {
		d := []byte(fmt.Sprintf("b%04d", j))
		for len(d) < itemLen {
			d = append(d, 'p')
		}
		return d
	}
	items := make([][]byte, n)
	for j := range items {
		items[j] = item(j)
	}
	last := func(b []byte, c byte) []byte {
		d := bytes.Clone(b)
		d[len(d)-1] = c
		return d
	}
	firsts := subDBLeafFirsts(t, &oracle{f: format.V09}, dbflags, key, items)
	const ncases = 8
	if len(firsts) < ncases {
		t.Fatalf("%d leaf boundaries, want %d", len(firsts), ncases)
	}
	var ops []multiOp
	for _, d := range items {
		ops = append(ops, multiOp{kind: "put", key: key, items: [][]byte{d}})
	}
	var added, between [][]byte
	getBoth := func(cur int, d []byte) {
		ops = append(ops, multiOp{kind: "getboth", cur: cur, key: key, items: [][]byte{d}})
	}
	del := func(j int) {
		getBoth(0, items[j])
		ops = append(ops, multiOp{kind: "del", key: key})
	}
	replace := func(j int, ds [][]byte, routed, slack bool) {
		getBoth(0, items[j])
		if other := j + 2; routed && other < n {
			getBoth(1, items[other])
		} else if !routed {
			getBoth(1, items[j])
		}
		ops = append(ops, multiOp{kind: "multi", key: key, items: ds, routed: routed, slack: slack})
		added = append(added, ds[len(ds)-1])
	}
	for c := len(firsts) - 1; c >= 0; c-- {
		f := firsts[c]
		prev := items[f-1]
		switch c % ncases {
		case 0: // the first item, by one of the same size before it
			replace(f, [][]byte{last(items[f], 'o')}, false, false)
		case 1: // the previous leaf's last item, by the deleted first item
			del(f)
			replace(f-1, [][]byte{items[f]}, true, false)
		case 2: // ... by one after it
			del(f)
			replace(f-1, [][]byte{last(items[f], 'q')}, true, false)
			between = append(between, items[f])
		case 3: // ... the same, half an item of slack on the Go side
			del(f)
			replace(f-1, [][]byte{last(items[f], 'q')}, true, true)
		case 4: // ... by one before it
			del(f)
			replace(f-1, [][]byte{last(prev, 'q')}, false, false)
		case 5: // two items, the previous leaf's last: C's in place
			del(f)
			replace(f-1, [][]byte{items[f], last(items[f], 'q')}, false, false)
		case 6: // two items, inside a leaf
			replace(f+1, [][]byte{last(items[f+1], 'o'), last(items[f+1], 'q')}, false, false)
		case 7: // two items, the leaf's first
			replace(f, [][]byte{last(items[f], 'n'), last(items[f], 'o')}, false, false)
		}
	}
	for _, d := range added {
		getBoth(1, d)
	}
	for _, d := range between {
		ops = append(ops, multiOp{kind: "put", key: key, items: [][]byte{d}})
	}
	ops = append(ops, multiOp{kind: "first"})
	for i := 0; i < n+len(between); i++ {
		ops = append(ops, multiOp{kind: "next"})
	}
	return ops
}

// TestCgoCurrentMultipleOutsideLeaf runs currentMultipleScript on C 0.9
// (tests/cgolmdb) and on the Go engine, at the OS page size, also with a
// dirty limit of 16 pages on the Go side: the same results and cursor
// positions after every step, then the same pages.
func TestCgoCurrentMultipleOutsideLeaf(t *testing.T) {
	const mapSize = 64 << 20
	key := []byte("k")
	itemLen := min(os.Getpagesize()/24, maxKey09-1)
	ops := currentMultipleScript(t, key, itemLen, 480)
	for _, limit := range []int{0, 16} {
		t.Run(fmt.Sprintf("dirty%d", limit), func(t *testing.T) {
			cdir, gdir := t.TempDir(), t.TempDir()
			cs := openCgoSide(t, cdir, mapSize)
			defer cs.env.Close()
			defer cs.txn.Abort()
			env, err := eng.Open(gdir, eng.Options{
				Format: format.V09, MapSize: mapSize, MaxDBs: 8,
				Flags: format.NoSync, OpenLock: eng.OpenMemLock,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			if limit > 0 {
				env.SetDirtyLimit(limit)
			}
			gs := &goSide{t: t}
			if gs.txn, err = env.BeginTxn(nil, 0); err != nil {
				t.Fatal(err)
			}
			defer gs.txn.Abort()
			if gs.dbi, err = gs.txn.OpenDBI([]byte("t"), format.DupSort|format.DupFixed|format.Create); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				c, err := gs.txn.OpenCursor(gs.dbi)
				if err != nil {
					t.Fatal(err)
				}
				gs.cur = append(gs.cur, c)
			}
			routed := 0
			for i, op := range ops {
				crc, grc := cs.step(op), gs.step(op)
				if crc != grc {
					t.Fatalf("step %d %s: C %d, Go %d", i, op.kind, crc, grc)
				}
				for c := range 2 {
					if cp, gp := cs.position(c), gs.position(c); cp != gp {
						t.Fatalf("step %d %s: cursor %d: C %.80s, Go %.80s", i, op.kind, c, cp, gp)
					}
				}
				if op.routed {
					routed++
				}
			}
			if routed < 3 {
				t.Fatalf("%d routed replacements", routed)
			}
			for _, c := range cs.cur {
				c.Close()
			}
			if err := cs.txn.Commit(); err != nil {
				t.Fatalf("C commit: %v", err)
			}
			for _, c := range gs.cur {
				c.Close()
			}
			if err := gs.txn.Commit(); err != nil {
				t.Fatalf("Go commit: %v", err)
			}
			cs.env.Close()
			env.Close()
			if err := compareDataFiles(filepath.Join(cdir, "data.mdb"), filepath.Join(gdir, "data.mdb")); err != nil {
				t.Fatal(err)
			}
			if strings.Join(goDump(t, cdir, []byte("t"), false), "\n") != strings.Join(goDump(t, gdir, []byte("t"), false), "\n") {
				t.Fatal("Go reads C's environment differently")
			}
		})
	}
}

// openCgoSide opens a C 0.9 environment in dir with a write transaction,
// the DB "t" (MDB_DUPSORT | MDB_DUPFIXED) and two cursors.
func openCgoSide(t *testing.T, dir string, mapSize int64) *cgoSide {
	env, err := cgolmdb.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	s := &cgoSide{env: env}
	if err := env.SetMapSize(mapSize); err != nil {
		t.Fatal(err)
	}
	if err := env.SetMaxDBs(8); err != nil {
		t.Fatal(err)
	}
	if err := env.Open(dir, cgolmdb.NoSync, 0o644); err != nil {
		t.Fatal(err)
	}
	// A write transaction holds C's writer lock on its OS thread
	// (lmdb-go v1 requires the caller to lock it, as Update does).
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	if s.txn, err = env.BeginTxn(nil, 0); err != nil {
		t.Fatal(err)
	}
	dbi, err := s.txn.OpenDBI("t", cgolmdb.DupSort|cgolmdb.DupFixed|cgolmdb.Create)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		c, err := s.txn.OpenCursor(dbi)
		if err != nil {
			t.Fatal(err)
		}
		s.cur = append(s.cur, c)
	}
	return s
}
