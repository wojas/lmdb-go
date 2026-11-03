//go:build cgo

package engine

// Moves after a failed exact search (rlmdb KNOWN_C_BUGS.md row 59; port of
// rlmdb's rlmdb-capi/tests/c/capi_write.c test_moves_after_failed_search,
// c6c4fdf). Cursor A's MDB_SET of "b" leaves it on the node of "k"
// (between "a" and "z") with its sub-cursor reset; MDB_NEXT or MDB_PREV
// then enters the duplicates of "k". C enters them with what the
// sub-cursor last held: a record from before another cursor added items to
// "k" (the count is short; after the sub-DB's root split the walk sees
// only the old root; where the put moved a sub-page C reads the main leaf
// as its root and crashes), another key's record ("m", whose items C walks
// under the key "k"), none (a new cursor, row 6: C asserts), or a C_DEL
// mark left by a delete ("b" deleted under the cursor, row 29: C's
// MDB_NEXT returns the first item twice). The Go engine sets the
// sub-cursor up from the node. The cases C survives run against C 0.9
// (tests/cgolmdb, in this process) and the Go engine side by side, C's
// results written in the checks; the others against the Go engine only.
// The traces (count and MDB_GET_CURRENT after the search, the move's
// code, where the walk ends) agree.

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
	cgolmdb "github.com/PowerDNS/lmdb-go/tests/cgolmdb"
)

// The cursor before the failed search.
const (
	missOnK = iota
	missOnM
	missFresh
	missDeleted
)

// missResult is what missCase saw.
type missResult struct {
	total, min, max uint32 // items of "k" in the end, the smallest, the largest
	rcCount, rcCur  int    // count and MDB_GET_CURRENT after the search (not for missDeleted: 0)
	rc              int    // the move
	count           uint64 // count after it
	first           uint32 // the item it returned
	seen, sorted    uint32 // items of "k" the walk saw, how many in order
	fourbyte        uint32 // how many had 4 bytes
	end             byte   // the key the walk ended on, '-' for none
}

// missTxn and missCursor are the operations of missCase, on C 0.9 or the
// Go engine; results are C return codes.
type missTxn interface {
	open(name string, flags uint32) (int, int)
	put(dbi int, k, v []byte) int
	cursor(dbi int) (missCursor, int)
}

type missCursor interface {
	get(key []byte, op format.CursorOp) ([]byte, []byte, int)
	getBoth(key, data []byte) int
	count() (uint64, int)
	put(k, v []byte) int
	putMulti(k, page []byte, stride int) int
	del(flags uint32) int
	close()
}

type cMissTxn struct{ txn *cgolmdb.Txn }
type cMissCursor struct{ c *cgolmdb.Cursor }

func (x cMissTxn) open(name string, flags uint32) (int, int) {
	d, err := x.txn.OpenDBI(name, uint(flags))
	return int(d), cgoCode(err)
}
func (x cMissTxn) put(dbi int, k, v []byte) int {
	return cgoCode(x.txn.Put(cgolmdb.DBI(dbi), k, v, 0))
}
func (x cMissTxn) cursor(dbi int) (missCursor, int) {
	c, err := x.txn.OpenCursor(cgolmdb.DBI(dbi))
	return cMissCursor{c}, cgoCode(err)
}
func (x cMissCursor) get(key []byte, op format.CursorOp) ([]byte, []byte, int) {
	k, v, err := x.c.Get(key, nil, uint(op))
	return k, v, cgoCode(err)
}
func (x cMissCursor) getBoth(key, data []byte) int {
	_, _, err := x.c.Get(key, data, uint(format.GetBoth))
	return cgoCode(err)
}
func (x cMissCursor) count() (uint64, int) {
	n, err := x.c.Count()
	return n, cgoCode(err)
}
func (x cMissCursor) put(k, v []byte) int { return cgoCode(x.c.Put(k, v, 0)) }
func (x cMissCursor) putMulti(k, page []byte, stride int) int {
	return cgoCode(x.c.PutMulti(k, page, stride, 0))
}
func (x cMissCursor) del(flags uint32) int { return cgoCode(x.c.Del(uint(flags))) }
func (x cMissCursor) close()               { x.c.Close() }

type gMissTxn struct{ txn *eng.Txn }
type gMissCursor struct{ c *eng.Cursor }

func (x gMissTxn) open(name string, flags uint32) (int, int) {
	d, err := x.txn.OpenDBI([]byte(name), flags)
	return int(d), code(err)
}
func (x gMissTxn) put(dbi int, k, v []byte) int { return code(x.txn.Put(eng.DBI(dbi), k, v, 0)) }
func (x gMissTxn) cursor(dbi int) (missCursor, int) {
	c, err := x.txn.OpenCursor(eng.DBI(dbi))
	return gMissCursor{c}, code(err)
}
func (x gMissCursor) get(key []byte, op format.CursorOp) ([]byte, []byte, int) {
	k, v, err := x.c.Get(key, nil, op)
	return k, v, code(err)
}
func (x gMissCursor) getBoth(key, data []byte) int {
	_, _, err := x.c.Get(key, data, format.GetBoth)
	return code(err)
}
func (x gMissCursor) count() (uint64, int) {
	n, err := x.c.Count()
	return n, code(err)
}
func (x gMissCursor) put(k, v []byte) int { return code(x.c.Put(k, v, 0)) }
func (x gMissCursor) putMulti(k, page []byte, stride int) int {
	_, err := x.c.PutMulti(k, page, stride, 0)
	return code(err)
}
func (x gMissCursor) del(flags uint32) int { return code(x.c.Del(flags)) }
func (x gMissCursor) close()               { x.c.Close() }

func be32of(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

// missCase: a new database (name) with "a" and "z" (item 1) and "k" (items
// 10 ... 10n), "m" with nm items of 8 bytes (missOnM) or "b"
// (missDeleted); A positioned as before says, then the failed MDB_SET;
// another cursor adds the items 5, 15, ... (add of them, with
// MDB_MULTIPLE if multiple); then op and the same move while on "k". "b"
// is deleted through the other cursor (lmdb-go v1's Txn.Del passes an
// empty value for nil; mdb_cursor_del makes the same fix-ups as mdb_del's
// cursor). No checks of its own: 0, or the code of a step before the move.
func missCase(x missTxn, name string, flags uint32, n uint32, before int, nm, add uint32, multiple bool, op format.CursorOp, r *missResult) int {
	be := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	*r = missResult{total: n + add, min: 10, max: 10 * n}
	if add > 0 {
		r.min = 5
		r.max = max(10*(add-1)+5, 10*n)
	}
	db, rc := x.open(name, format.Create|flags)
	if rc != 0 {
		return rc
	}
	for _, k := range []string{"a", "z"} {
		if rc := x.put(db, []byte(k), be(1)); rc != 0 {
			return rc
		}
	}
	if before == missDeleted {
		if rc := x.put(db, []byte("b"), be(1)); rc != 0 {
			return rc
		}
	}
	for i := uint32(1); i <= n; i++ {
		if rc := x.put(db, []byte("k"), be(10*i)); rc != 0 {
			return rc
		}
	}
	for i := uint32(0); before == missOnM && i < nm; i++ {
		if rc := x.put(db, []byte("m"), binary.BigEndian.AppendUint64(nil, uint64(i))); rc != 0 {
			return rc
		}
	}
	a, rc := x.cursor(db)
	if rc != 0 {
		return rc
	}
	defer a.close()
	c, rc := x.cursor(db)
	if rc != 0 {
		return rc
	}
	defer c.close()
	if before != missFresh {
		key := map[int]string{missOnK: "k", missOnM: "m", missDeleted: "b"}[before]
		if _, _, rc := a.get([]byte(key), format.Set); rc != 0 {
			return rc
		}
	}
	if before == missDeleted {
		if _, _, rc := c.get([]byte("b"), format.Set); rc != 0 {
			return rc
		}
		if rc := c.del(format.NoDupData); rc != 0 {
			return rc
		}
	}
	if _, _, rc := a.get([]byte("b"), format.Set); rc != notFound {
		if rc == 0 {
			return -1
		}
		return rc
	}
	_, r.rcCount = a.count()
	// C's MDB_GET_CURRENT through the sub-cursor clears its C_DEL, also
	// when it fails.
	if before != missDeleted {
		_, _, r.rcCur = a.get(nil, format.GetCurrent)
	}
	if multiple {
		var items []byte
		for i := range add {
			items = append(items, be(10*i+5)...)
		}
		if rc := c.putMulti([]byte("k"), items, 4); rc != 0 {
			return rc
		}
	}
	for i := uint32(0); !multiple && i < add; i++ {
		if rc := c.put([]byte("k"), be(10*i+5)); rc != 0 {
			return rc
		}
	}
	k, d, rc := a.get(nil, op)
	r.rc = rc
	if rc == 0 {
		r.first = be32of(d)
		if n, rc := a.count(); rc == 0 {
			r.count = n
		}
	}
	var prev uint32
	for rc == 0 && string(k) == "k" {
		v := be32of(d)
		if len(d) == 4 {
			r.fourbyte++
		}
		if r.seen == 0 || (op == format.Next && v > prev) || (op != format.Next && v < prev) {
			r.sorted++
		}
		r.seen++
		prev = v
		k, d, rc = a.get(nil, op)
	}
	r.end = '-'
	if rc == 0 && len(k) > 0 {
		r.end = k[0]
	}
	return 0
}

// notFound is MDB_NOTFOUND's code.
var notFound = code(format.NotFound)

// missRight is the Go engine's result: every item of "k" once, in order,
// from the first (the last for MDB_PREV), and the right count.
func missRight(r *missResult, op format.CursorOp) bool {
	first := r.min
	if op != format.Next {
		first = r.max
	}
	return r.first == first && r.count == uint64(r.total) && r.seen == r.total &&
		r.sorted == r.total && r.fourbyte == r.total
}

// How C's walk differs from the Go engine's, in the cases it survives.
const (
	cRight    = iota
	cCount    // the count before the put, as row 14
	cOldRoot  // the old root's items only
	cOtherKey // the items of "m"
	cTwice    // the first item twice
)

func TestCgoMovesAfterAFailedSearch(t *testing.T) {
	const mapSize = 16 << 20
	cenv, err := cgolmdb.NewEnv()
	must(t, err)
	defer cenv.Close()
	must(t, cenv.SetMapSize(mapSize))
	must(t, cenv.SetMaxDBs(200))
	must(t, cenv.Open(t.TempDir(), cgolmdb.NoSync, 0o644))
	env, err := eng.Open(t.TempDir(), eng.Options{
		Format: format.V09, MapSize: mapSize, MaxDBs: 200,
		Flags: format.NoSync, OpenLock: eng.OpenMemLock,
	})
	must(t, err)
	defer env.Close()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctxn, err := cenv.BeginTxn(nil, 0)
	must(t, err)
	defer ctxn.Abort()
	gtxn, err := env.BeginTxn(nil, 0)
	must(t, err)
	defer gtxn.Abort()
	cx, gx := cMissTxn{ctxn}, gMissTxn{gtxn}
	einval := code(syscall.EINVAL)
	seq := 0
	name := func() string { seq++; return fmt.Sprintf("miss%d", seq) }

	both := func(what string, flags, n uint32, before int, nm, add uint32, multiple bool, op format.CursorOp, cResult int) {
		t.Helper()
		what = fmt.Sprintf("%s (flags %#x, op %d)", what, flags, op)
		var cr, gr missResult
		db := name()
		if rc := missCase(cx, db, flags, n, before, nm, add, multiple, op, &cr); rc != 0 {
			t.Fatalf("%s: C: %d", what, rc)
		}
		if rc := missCase(gx, db, flags, n, before, nm, add, multiple, op, &gr); rc != 0 {
			t.Fatalf("%s: Go: %d", what, rc)
		}
		wantCur := einval
		if before == missDeleted {
			wantCur = 0
		}
		wantEnd := byte('z')
		switch {
		case op == format.Prev:
			wantEnd = 'a'
		case before == missOnM:
			wantEnd = 'm'
		}
		for _, side := range []struct {
			name string
			r    *missResult
		}{{"C", &cr}, {"Go", &gr}} {
			r := side.r
			// The argument checks: the sub-cursor is not on an item.
			if r.rcCount != einval || r.rcCur != wantCur || r.rc != 0 || r.end != wantEnd {
				t.Fatalf("%s: %s: count %d, current %d, move %d, ends on %c", what, side.name, r.rcCount, r.rcCur, r.rc, r.end)
			}
		}
		if !missRight(&gr, op) {
			t.Errorf("%s: Go: %+v", what, gr)
		}
		var ok bool
		switch cResult {
		case cRight:
			ok = missRight(&cr, op)
		case cCount:
			ok = cr.count == uint64(n) && cr.seen == cr.total && cr.sorted == cr.total
		case cOldRoot:
			ok = cr.count == uint64(n) && cr.seen < cr.total && cr.sorted == cr.seen
		case cOtherKey:
			ok = cr.count == uint64(nm) && cr.seen == nm && cr.fourbyte == 0
		case cTwice:
			ok = cr.first == 10 && cr.count == uint64(n) && cr.seen == n+1 && cr.sorted == n
		}
		if !ok {
			t.Errorf("%s: C: %+v (expected result %d)", what, cr, cResult)
		}
	}
	// A case C crashes on, asserts in or answers with garbage: the Go
	// engine only.
	goOnly := func(what string, flags, n uint32, before int, add uint32, multiple bool, op format.CursorOp) {
		t.Helper()
		what = fmt.Sprintf("%s (flags %#x, op %d)", what, flags, op)
		var r missResult
		if rc := missCase(gx, name(), flags, n, before, 0, add, multiple, op, &r); rc != 0 {
			t.Fatalf("%s: %d", what, rc)
		}
		wantEnd := byte('z')
		if op == format.Prev {
			wantEnd = 'a'
		}
		if r.rcCount != einval || r.rcCur != einval || r.rc != 0 || r.end != wantEnd || !missRight(&r, op) {
			t.Errorf("%s: %+v", what, r)
		}
	}

	for _, flags := range []uint32{format.DupSort | format.DupFixed, format.DupSort} {
		s, split := subDBThresholdsAZK(t, flags)
		if s <= 4 || split <= s+1 {
			t.Fatalf("flags %#x: sub-DB at %d, split at %d", flags, s, split)
		}
		fixed := flags&format.DupFixed != 0
		for _, op := range []format.CursorOp{format.Next, format.Prev} {
			twice := cRight
			if op == format.Next {
				twice = cTwice
			}
			both("sub-DB, another cursor adds one", flags, s+1, missOnK, 0, 1, false, op, cCount)
			both("sub-DB's root splits", flags, s+1, missOnK, 0, split-s+1, false, op, cOldRoot)
			both("another key's sub-page", flags, 3, missOnM, 2, 0, false, op, cOtherKey)
			both("another key's sub-DB", flags, 3, missOnM, s+1, 0, false, op, cOtherKey)
			both("a sub-DB, another key's sub-page", flags, s+1, missOnM, 2, 0, false, op, cOtherKey)
			both("sub-page, a delete marked it", flags, 3, missDeleted, 0, 0, false, op, twice)
			both("sub-DB, a delete marked it", flags, s+1, missDeleted, 0, 0, false, op, twice)
			both("sub-page, the record is right", flags, 3, missOnK, 0, 0, false, op, cRight)
			both("sub-DB, the record is right", flags, s+1, missOnK, 0, 0, false, op, cRight)
			if fixed {
				// The sub-page has room for the item: it does not move.
				both("sub-page, another cursor adds one", flags, 3, missOnK, 0, 1, false, op, cCount)
				both("sub-page, MDB_MULTIPLE of one", flags, 3, missOnK, 0, 1, true, op, cCount)
				both("sub-DB's root splits, MDB_MULTIPLE", flags, s+1, missOnK, 0, split-s+1, true, op, cOldRoot)
				// The sub-page moves: C crashes.
				goOnly("sub-page moved by MDB_MULTIPLE", flags, 3, missOnK, 2, true, op)
			} else {
				goOnly("sub-page moved by a put", flags, 3, missOnK, 1, false, op)
			}
			// The sub-page becomes a sub-DB: C reads garbage.
			goOnly("sub-page becomes a sub-DB", flags, s-1, missOnK, 2, false, op)
			// Never on duplicates (row 6): C asserts.
			goOnly("a sub-page, never on duplicates", flags, 3, missFresh, 0, false, op)
			goOnly("a sub-DB, never on duplicates", flags, s+1, missFresh, 0, false, op)
		}
	}
}

// subDBThresholdsAZK is how many items "k" has (with "a" and "z" next to
// it, 4-byte items, in a 0.9 environment of the OS page size, as C 0.9's
// in this process) when it moves to a sub-DB (s) and when that sub-DB's
// leaf splits (split).
func subDBThresholdsAZK(t *testing.T, flags uint32) (s, split uint32) {
	env, err := eng.Open(t.TempDir(), eng.Options{
		Format: format.V09, MapSize: 16 << 20, MaxDBs: 4,
		Flags: format.NoSync, OpenLock: eng.OpenMemLock,
	})
	must(t, err)
	defer env.Close()
	txn, err := env.BeginTxn(nil, 0)
	must(t, err)
	defer txn.Abort()
	db, err := txn.OpenDBI([]byte("probe"), format.Create|flags)
	must(t, err)
	for _, k := range []string{"a", "z"} {
		must(t, txn.Put(db, []byte(k), binary.BigEndian.AppendUint32(nil, 1), 0))
	}
	for i := uint32(1); ; i++ {
		st, err := txn.Stat(db)
		must(t, err)
		must(t, txn.Put(db, []byte("k"), binary.BigEndian.AppendUint32(nil, 10*i), 0))
		st2, err := txn.Stat(db)
		must(t, err)
		if st2.LeafPages != st.LeafPages {
			if s > 0 {
				return s, i
			}
			s = i
		}
	}
}

// TestCgoCountAfterAnotherCursorsPut: mdb_cursor_count of a cursor on a
// key after another cursor (or mdb_put, or MDB_MULTIPLE) put duplicates of
// that key: converting a single item, into a sub-page, growing it,
// promoting it to a sub-DB, into a sub-DB, splitting its leaf; before and
// after the cursor's item (rlmdb's capi_write.c test_count_after_other_put,
// a508e1b). C 0.9's count is the one before the put (its cursor keeps a
// stale copy of the duplicates' record, KNOWN_C_BUGS.md row 14), the Go
// engine's is right; the positions after it (MDB_GET_CURRENT,
// MDB_NEXT_DUP, MDB_PREV_DUP) are C's.
func TestCgoCountAfterAnotherCursorsPut(t *testing.T) {
	const mapSize = 16 << 20
	cenv, err := cgolmdb.NewEnv()
	must(t, err)
	defer cenv.Close()
	must(t, cenv.SetMapSize(mapSize))
	must(t, cenv.SetMaxDBs(200))
	must(t, cenv.Open(t.TempDir(), cgolmdb.NoSync, 0o644))
	env, err := eng.Open(t.TempDir(), eng.Options{
		Format: format.V09, MapSize: mapSize, MaxDBs: 200,
		Flags: format.NoSync, OpenLock: eng.OpenMemLock,
	})
	must(t, err)
	defer env.Close()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctxn, err := cenv.BeginTxn(nil, 0)
	must(t, err)
	defer ctxn.Abort()
	gtxn, err := env.BeginTxn(nil, 0)
	must(t, err)
	defer gtxn.Abort()
	be := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	seq := 0

	// count runs one case on one side: cursor A on item on of "k" (items
	// 10 ... 10n), then how (0: another cursor puts v, 1: mdb_put puts it,
	// 2: MDB_MULTIPLE puts v, v+1, v+2). Returns the key's count, A's
	// count and the trace of A's moves.
	count := func(x missTxn, name string, flags uint32, how int, n, on, v uint32) (truth, cnt uint64, trace string) {
		db, rc := x.open(name, format.Create|flags)
		if rc != 0 {
			t.Fatalf("open %s: %d", name, rc)
		}
		for _, k := range []string{"a", "z"} {
			if rc := x.put(db, []byte(k), be(1)); rc != 0 {
				t.Fatalf("put %s: %d", k, rc)
			}
		}
		for i := uint32(1); i <= n; i++ {
			if rc := x.put(db, []byte("k"), be(10*i)); rc != 0 {
				t.Fatalf("put k: %d", rc)
			}
		}
		a, _ := x.cursor(db)
		defer a.close()
		c, _ := x.cursor(db)
		defer c.close()
		if rc := a.getBoth([]byte("k"), be(on)); rc != 0 {
			t.Fatalf("get both k %d: %d", on, rc)
		}
		switch how {
		case 0:
			rc = c.put([]byte("k"), be(v))
		case 1:
			rc = x.put(db, []byte("k"), be(v))
		default:
			rc = c.putMulti([]byte("k"), append(append(be(v), be(v+1)...), be(v+2)...), 4)
		}
		if rc != 0 {
			t.Fatalf("put %d: %d", how, rc)
		}
		if _, _, rc := c.get([]byte("k"), format.Set); rc != 0 {
			t.Fatalf("set k with the other cursor: %d", rc)
		}
		truth, _ = c.count()
		cnt, _ = a.count()
		_, d, rcCur := a.get(nil, format.GetCurrent)
		cur := be32of(d)
		_, d, rcNext := a.get(nil, format.NextDup)
		next := be32of(d)
		_, d, rcPrev := a.get(nil, format.PrevDup)
		prev := be32of(d)
		return truth, cnt, fmt.Sprintf("A on %d (%d), next dup %d (%d), then previous %d (%d)",
			cur, rcCur, next, rcNext, prev, rcPrev)
	}

	for f, flags := range []uint32{format.DupSort | format.DupFixed, format.DupSort} {
		s, split := subDBThresholdsAZK(t, flags)
		if s <= 4 || split <= s {
			t.Fatalf("flags %#x: sub-DB at %d, split at %d", flags, s, split)
		}
		cases := []struct {
			what     string
			n, on, v uint32
		}{
			{"single item, after", 1, 10, 15},
			{"single item, before", 1, 10, 5},
			{"sub-page, after", 3, 20, 25},
			{"sub-page, before", 3, 20, 15},
			{"sub-page grows", 4, 20, 45},
			{"to a sub-DB, after", s - 1, 20, 10*s + 5},
			{"to a sub-DB, before", s - 1, 20, 5},
			{"sub-DB, after", s + 5, 20, 25},
			{"sub-DB, before", s + 5, 20, 15},
			{"sub-DB leaf splits, after", split - 1, 10, 10*split + 5},
			{"sub-DB leaf splits, before", split - 1, 10 * (split - 1), 5},
		}
		hows := 2
		if f == 0 {
			hows = 3
		}
		for how := range hows {
			added := uint64(1)
			if how == 2 {
				added = 3
			}
			for _, tc := range cases {
				what := fmt.Sprintf("%s (flags %#x, put %d)", tc.what, flags, how)
				seq++
				name := fmt.Sprintf("count%d", seq)
				ctruth, ccnt, ctrace := count(cMissTxn{ctxn}, name, flags, how, tc.n, tc.on, tc.v)
				gtruth, gcnt, gtrace := count(gMissTxn{gtxn}, name, flags, how, tc.n, tc.on, tc.v)
				want := uint64(tc.n) + added
				if ctruth != want || gtruth != want {
					t.Fatalf("%s: the key's count: C %d, Go %d, want %d", what, ctruth, gtruth, want)
				}
				if ccnt >= want {
					t.Errorf("%s: C's count %d is not stale (row 14)", what, ccnt)
				}
				if gcnt != want {
					t.Errorf("%s: Go's count %d, want %d", what, gcnt, want)
				}
				if ctrace != gtrace {
					t.Errorf("%s:\n  C:  %s\n  Go: %s", what, ctrace, gtrace)
				}
			}
		}
	}
}
