//go:build cgo

package engine

// A main DB out of order, then a nested transaction's abort and a new
// database: the Go writer must write what C writes. In such a main DB
// searches can miss keys that are there. Two places depended on such a
// search where C does not:
//
//   - The abort of a child closes the handles that name a database which
//     does not exist in the state it reverts to (rlmdb KNOWN_C_BUGS.md row
//     19; C closes only DB_NEW handles). Its lookup missed "dups", which
//     the parent had changed: the handle was closed, and the commit failed
//     with MDB_BAD_DBI, or, once a new database had taken the slot, lost
//     the put and leaked its leaf page (found by FuzzAPI). A database the
//     parent has a view of is now not looked up.
//   - mdb_dbi_open puts a new database's record with the cursor its lookup
//     left positioned, whose mdb_cursor_set then decides by the first and
//     last key of the page; the Go writer searched afresh, which put the
//     record elsewhere in such a main DB.
//
// The two sides break the order differently into the same pages: C with a
// cursor put with MDB_CURRENT and a key other than the item's, which C
// writes over the node (KNOWN_C_BUGS.md row 58; lmdb-go v1 has no
// comparator API), the Go engine with a comparator changed within the
// transaction for one put (as rlmdb's capi_unsorted_main.c does on both
// sides), since its MDB_CURRENT keeps the cursor's key. rlmdb's writeops
// works on one database and cannot express this, so the oracle is C 0.9
// in this process: tests/cgolmdb, on its own environment next to the Go
// engine's, compared step by step and then page for page. (rlmdb's
// capi_unsorted_main.c checks the same three runs against C 1.0, whose
// records internal/engine's unsorted_main_test.go expects.)

import (
	"bytes"
	"path/filepath"
	"runtime"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
	cgolmdb "github.com/PowerDNS/lmdb-go/tests/cgolmdb"
)

func TestCgoUnsortedMainChildAbortAndOpen(t *testing.T) {
	const mapSize = 64 << 20
	for _, tc := range []struct {
		name        string
		child, open bool
	}{
		{"abort", true, false},
		{"abort-open", true, true},
		{"open", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cdir, gdir := t.TempDir(), t.TempDir()
			var steps []string
			step := func(what string, c, g int) {
				steps = append(steps, what)
				if c != g {
					t.Fatalf("%s: C %d, Go %d", what, c, g)
				}
			}
			misuse := [][]byte{{0, 1, 2}, {0x89, 0x8a, 0x8b, 0x8c}}
			value := bytes.Repeat([]byte("v"), 10)

			// C 0.9.
			cenv, err := cgolmdb.NewEnv()
			must(t, err)
			defer cenv.Close()
			must(t, cenv.SetMapSize(mapSize))
			must(t, cenv.SetMaxDBs(8))
			must(t, cenv.Open(cdir, cgolmdb.NoSync, 0o644))
			// lmdb-go v1 requires the caller to lock the OS thread of a
			// write transaction, as Update does.
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			ctxn, err := cenv.BeginTxn(nil, 0)
			must(t, err)
			defer ctxn.Abort()

			// The Go engine.
			env, err := eng.Open(gdir, eng.Options{
				Format: format.V09, MapSize: mapSize, MaxDBs: 8,
				Flags: format.NoSync, OpenLock: eng.OpenMemLock,
			})
			must(t, err)
			defer env.Close()
			gtxn, err := env.BeginTxn(nil, 0)
			must(t, err)
			defer gtxn.Abort()

			cd, cerr := ctxn.OpenDBI("dups", cgolmdb.Create)
			gd, gerr := gtxn.OpenDBI([]byte("dups"), format.Create)
			step("open dups", cgoCode(cerr), code(gerr))
			step("dups handle", int(cd), int(gd))
			step("put dups", cgoCode(ctxn.Put(cd, []byte("k"), []byte("v"), 0)),
				code(gtxn.Put(gd, []byte("k"), []byte("v"), 0)))
			// More records: after the misuse the main DB holds 898a8b8c,
			// dups, fixed, ints, plain, where a search for "dups" turns
			// left at "fixed" and misses it, and a fresh search for "xa"
			// goes to the end (C's put of its record compares it with the
			// first key and goes to the front).
			for _, name := range []string{"fixed", "ints", "plain"} {
				ci, cerr := ctxn.OpenDBI(name, cgolmdb.Create)
				gi, gerr := gtxn.OpenDBI([]byte(name), format.Create)
				step("open "+name, cgoCode(cerr), code(gerr))
				step(name+" handle", int(ci), int(gi))
			}
			cmain, err := ctxn.OpenRoot(0)
			must(t, err)
			cc, err := ctxn.OpenCursor(cmain)
			must(t, err)
			// The key 89 8a 8b 8c at index 0: C writes it over the node of
			// 00 01 02 (MDB_CURRENT with data of another size), the Go
			// engine puts it in an order that sorts it first.
			step("put main", cgoCode(cc.Put(misuse[0], nil, 0)), 0)
			step("MDB_CURRENT, another key", cgoCode(cc.Put(misuse[1], value, cgolmdb.Current)), 0)
			must(t, gtxn.SetCompare(eng.MainDBI, hibitFirst))
			step("put in another order", 0, code(gtxn.Put(eng.MainDBI, misuse[1], value, format.NoOverwrite)))
			must(t, gtxn.SetCompare(eng.MainDBI, nil))
			cc.Close()
			if tc.child {
				cchild, err := cenv.BeginTxn(ctxn, 0)
				must(t, err)
				cchild.Abort()
				gchild, err := env.BeginTxn(gtxn, 0)
				must(t, err)
				gchild.Abort()
			}
			if tc.open {
				cx, cerr := ctxn.OpenDBI("xa", cgolmdb.Create)
				gx, gerr := gtxn.OpenDBI([]byte("xa"), format.Create)
				step("open xa", cgoCode(cerr), code(gerr))
				step("xa handle", int(cx), int(gx))
			}
			cv, cerr := ctxn.Get(cd, []byte("k"))
			gv, gerr := gtxn.Get(gd, []byte("k"))
			step("get dups", cgoCode(cerr), code(gerr))
			if !bytes.Equal(cv, gv) {
				t.Fatalf("get dups: C %q, Go %q", cv, gv)
			}
			step("commit", cgoCode(ctxn.Commit()), code(gtxn.Commit()))
			cenv.Close()
			env.Close()
			if err := compareDataFiles(filepath.Join(cdir, "data.mdb"), filepath.Join(gdir, "data.mdb")); err != nil {
				t.Fatalf("after %d steps: %v", len(steps), err)
			}
		})
	}
}

// hibitFirst is the default key order with the top bit of every byte
// flipped: bytes from 0x80 sort before the others.
func hibitFirst(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if d := int(a[i]^0x80) - int(b[i]^0x80); d != 0 {
			return d
		}
	}
	return len(a) - len(b)
}
