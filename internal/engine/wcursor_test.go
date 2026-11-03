package engine

// Positioning on a write cursor's own stack (port of the tests of rlmdb
// core/wcursor.rs): seekKey and seekLast, as mdb_put and mdb_del use them,
// give the results and errors of the read path (cursorSet and
// cursorLastForAppend through readOp) and leave the cursor and its
// sub-cursor in the same state, in trees with and without duplicates, on
// dirty, clean, nested and spilled pages, in both formats.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// posKey is key i of the positioning tests: 400 bytes, so that a few
// thousand make a three-level tree on any page size; ordered by i.
func posKey(i uint32) []byte {
	k := bytes.Repeat([]byte{'k'}, 400)
	binary.BigEndian.PutUint32(k, i)
	return k
}

// sameWCursor fails unless a and b are in the same state: stack, indices,
// flags, sub-page and walk.
func sameWCursor(t *testing.T, what string, a, b *wcursor) {
	t.Helper()
	sa := [5]any{a.snum, a.top, a.flags, a.subp, a.walk}
	sb := [5]any{b.snum, b.top, b.flags, b.subp, b.walk}
	if sa != sb {
		t.Fatalf("%s: (snum, top, flags, subp, walk) %v in place, %v through the read path", what, sa, sb)
	}
	if a.ki != b.ki {
		t.Fatalf("%s: ki %v in place, %v through the read path", what, a.ki, b.ki)
	}
	if !slices.Equal(a.pg[:a.snum], b.pg[:b.snum]) {
		t.Fatalf("%s: pg %v in place, %v through the read path", what, a.pg[:a.snum], b.pg[:b.snum])
	}
}

// sameSlots fails unless the main cursors of slots a and b and their
// sub-cursors (with the duplicates' records and order) are in the same
// state.
func sameSlots(t *testing.T, txn *Txn, what string, a, b int) {
	t.Helper()
	sa, sb := &txn.w.cursors.slots[a], &txn.w.cursors.slots[b]
	sameWCursor(t, what+", main cursor", &sa.main, &sb.main)
	if sa.hasX != sb.hasX {
		t.Fatalf("%s: one slot has a sub-cursor, the other not", what)
	}
	if !sa.hasX {
		return
	}
	sameWCursor(t, what+", sub-cursor", &sa.x.c, &sb.x.c)
	if sa.x.db != sb.x.db {
		t.Fatalf("%s: sub-cursor record %+v in place, %+v through the read path", what, sa.x.db, sb.x.db)
	}
	if sa.x.cmp.Kind != sb.x.cmp.Kind || (sa.x.cmp.Fn == nil) != (sb.x.cmp.Fn == nil) {
		t.Fatalf("%s: duplicate order %v in place, %v through the read path", what, sa.x.cmp.Kind, sb.x.cmp.Kind)
	}
}

// checkPositioning positions two fresh cursors of dbi, one on its own stack
// and one through the read path, for MDB_SET on probe and for
// mdb_cursor_last, and checks that results and cursor states agree.
func checkPositioning(t *testing.T, txn *Txn, dbi DBI, probe []byte) {
	t.Helper()
	// Resolve the DBI's record as mdb_put does on first use in a
	// transaction (openSlot reads the entry as it is).
	if _, err := txn.freshDBI(dbi, dbUsrValid); err != nil {
		t.Fatal(err)
	}
	what := fmt.Sprintf("MDB_SET %x", probe[:min(len(probe), 4)])
	a, b := txn.openSlot(int(dbi), false), txn.openSlot(int(dbi), false)
	ca, cb := mainRef(a), mainRef(b)
	if !txn.seeksInPlace(ca) {
		t.Fatalf("%s: a fresh cursor is not positioned in place", what)
	}
	db, cmp := *txn.dbOf(ca), txn.cmpOf(ca)
	got, gotErr := txn.seekKey(ca, &db, cmp, probe)
	wantErr := txn.readOp(cb, func(c, x *rcursor) error {
		_, _, _, err := cursorSet(txn, c, x, format.Comparator{}, false, probe, nil, false, format.Set, true)
		return err
	})
	want := wantErr == nil
	if wantErr == format.NotFound {
		wantErr = nil
	}
	if got != want || gotErr != wantErr {
		t.Fatalf("%s: in place (%v, %v), through the read path (%v, %v)", what, got, gotErr, want, wantErr)
	}
	sameSlots(t, txn, what, a, b)
	txn.closeSlot(a)
	txn.closeSlot(b)

	what = "last"
	a, b = txn.openSlot(int(dbi), false), txn.openSlot(int(dbi), false)
	ca, cb = mainRef(a), mainRef(b)
	if !txn.seeksInPlace(ca) {
		t.Fatalf("%s: a fresh cursor is not positioned in place", what)
	}
	gotKey, gotOK, gotErr := txn.seekLast(ca, &db, cmp)
	gotKey = bytes.Clone(gotKey)
	var wantKey []byte
	var wantOK bool
	wantErr = txn.readOp(cb, func(c, x *rcursor) error {
		k, _, ok, err := cursorLastForAppend(txn, c, x)
		wantKey, wantOK = bytes.Clone(k), ok
		return err
	})
	if gotErr != wantErr || gotErr == nil && (gotOK != wantOK || !bytes.Equal(gotKey, wantKey)) {
		t.Fatalf("%s: in place (%.4x, %v, %v), through the read path (%.4x, %v, %v)",
			what, gotKey, gotOK, gotErr, wantKey, wantOK, wantErr)
	}
	sameSlots(t, txn, what, a, b)
	txn.closeSlot(a)
	txn.closeSlot(b)
}

func positioningProbes() [][]byte {
	var p [][]byte
	for _, i := range []uint32{0, 1, 2, 777, 1500, 3001, 5998, 5999, 6000, 9999} {
		p = append(p, posKey(i))
	}
	// Keys with a sub-page and with a sub-DB in the MDB_DUPSORT tree.
	p = append(p, posKey(2), posKey(1204))
	return append(p, []byte{}, []byte("a"), []byte{0})
}

func checkPositioningAll(t *testing.T, txn *Txn, dbi DBI) {
	t.Helper()
	for _, probe := range positioningProbes() {
		checkPositioning(t, txn, dbi, probe)
	}
}

// positioningOptions are the options of the positioning tests: LMDB 1.0
// (its layout and page header) on 4 KiB pages.
func positioningOptions(fm format.Format, mapSize int64) Options {
	opts := Options{MaxDBs: 2, MapSize: mapSize}
	if fm == format.V10 {
		opts.PageSize = 4096
	}
	return opts
}

func TestPositioningInPlaceMatchesTheReadPath(t *testing.T) {
	forFormats(t, func(t *testing.T, fm format.Format) {
		env, _ := openTest(t, fm, positioningOptions(fm, 256<<20))
		if env.Format() != fm {
			t.Fatalf("format %v", env.Format())
		}
		txn := beginW(t, env)
		db, err := txn.OpenDBI(nil, 0)
		must(t, err)
		// An empty tree.
		checkPositioningAll(t, txn, db)
		for i := uint32(0); i < 3000; i++ {
			must(t, txn.Put(db, posKey(2*i), []byte("v"), 0))
		}
		st, err := txn.Stat(db)
		must(t, err)
		if st.Depth < 3 {
			t.Fatalf("depth %d", st.Depth)
		}
		// Dirty pages.
		checkPositioningAll(t, txn, db)
		must(t, txn.Commit())

		// Clean pages, then some dirty again.
		txn = beginW(t, env)
		checkPositioningAll(t, txn, db)
		for i := uint32(0); i < 300; i++ {
			must(t, txn.Put(db, posKey(20*i+1), []byte("w"), 0))
		}
		checkPositioningAll(t, txn, db)
		// Pages dirty in the parent, then in the child too.
		child, err := env.BeginTxn(txn, 0)
		must(t, err)
		checkPositioningAll(t, child, db)
		must(t, child.Put(db, posKey(3), []byte("c"), 0))
		checkPositioningAll(t, child, db)
		child.Abort()
		txn.Abort()

		// Spilled pages.
		env.SetDirtyLimit(64)
		txn = beginW(t, env)
		for i := uint32(0); i < 3000; i++ {
			must(t, txn.Put(db, posKey(2*i), []byte("spill"), 0))
		}
		if len(txn.w.spilled) == 0 {
			t.Fatal("nothing spilled")
		}
		checkPositioningAll(t, txn, db)
		txn.Abort()
	})
}

// putDups puts the items of key i of the MDB_DUPSORT test tree: one item
// for every third key, a sub-DB for one key in 300 (and so for the first
// key with duplicates, posKey(4)), else a sub-page of two or three items.
func putDups(t *testing.T, txn *Txn, db DBI, i uint32, tag byte) {
	t.Helper()
	var count uint32
	switch {
	case i%3 == 0:
		count = 1
	case i%300 == 2:
		count = 200
	default:
		count = i%3 + 1
	}
	for d := uint32(0); d < count; d++ {
		v := bytes.Repeat([]byte{tag}, 100)
		binary.BigEndian.PutUint32(v, d)
		must(t, txn.Put(db, posKey(2*i), v, 0))
	}
}

func openDupsDB(t *testing.T, txn *Txn, flags uint32) DBI {
	t.Helper()
	db, err := txn.OpenDBI([]byte("dups"), format.DupSort|format.Create|flags)
	must(t, err)
	return db
}

// The MDB_DUPFIXED run (not in rlmdb's test) also covers sub-pages and
// sub-DBs of P_LEAF2 pages.
func TestPositioningInPlaceMatchesTheReadPathWithDuplicates(t *testing.T) {
	for _, flags := range []uint32{0, format.DupFixed} {
		t.Run(fmt.Sprintf("flags=%#x", flags), func(t *testing.T) {
			forFormats(t, func(t *testing.T, fm format.Format) { dupPositioningMatches(t, fm, flags) })
		})
	}
}

func dupPositioningMatches(t *testing.T, fm format.Format, flags uint32) {
	env, _ := openTest(t, fm, positioningOptions(fm, 256<<20))
	txn := beginW(t, env)
	db := openDupsDB(t, txn, flags)
	// An empty tree.
	checkPositioningAll(t, txn, db)
	for i := uint32(0); i < 3000; i++ {
		putDups(t, txn, db, i, 'v')
		if i == 2 {
			// The last key holds a sub-DB.
			checkPositioningAll(t, txn, db)
		}
	}
	st, err := txn.Stat(db)
	must(t, err)
	if st.Depth < 3 {
		t.Fatalf("depth %d", st.Depth)
	}
	// Dirty pages; the last key holds a sub-page.
	checkPositioningAll(t, txn, db)
	must(t, txn.Commit())

	// Clean pages, then some dirty again.
	txn = beginW(t, env)
	checkPositioningAll(t, txn, db)
	for i := uint32(0); i < 3000; i += 7 {
		putDups(t, txn, db, i, 'w')
	}
	checkPositioningAll(t, txn, db)
	// Pages dirty in the parent, then in the child too.
	child, err := env.BeginTxn(txn, 0)
	must(t, err)
	checkPositioningAll(t, child, db)
	putDups(t, child, db, 1, 'c')
	checkPositioningAll(t, child, db)
	child.Abort()
	txn.Abort()

	// Spilled pages.
	env.SetDirtyLimit(64)
	txn = beginW(t, env)
	for i := uint32(0); i < 3000; i++ {
		putDups(t, txn, db, i, 's')
	}
	if len(txn.w.spilled) == 0 {
		t.Fatal("nothing spilled")
	}
	checkPositioningAll(t, txn, db)
	txn.Abort()
}

// seekInPlace positions a fresh cursor of dbi on key in place and returns
// its slot (the caller closes it).
func seekInPlace(t *testing.T, txn *Txn, dbi DBI, key []byte) (int, error) {
	t.Helper()
	if _, err := txn.freshDBI(dbi, dbUsrValid); err != nil {
		t.Fatal(err)
	}
	a := txn.openSlot(int(dbi), false)
	ca := mainRef(a)
	rec, cmp := *txn.dbOf(ca), txn.cmpOf(ca)
	found, err := txn.seekKey(ca, &rec, cmp, key)
	if err == nil && !found {
		t.Fatalf("key %.4x not found", key)
	}
	return a, err
}

// A sub-page whose first item is damaged: positioning on its key in place
// fails as through the read path, and leaves the same state.
func TestPositioningOnADamagedSubPageMatchesTheReadPath(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2, MapSize: 64 << 20})
	txn := beginW(t, env)
	db := openDupsDB(t, txn, 0)
	must(t, txn.Commit())
	// Each damages the sub-page of posKey(2) (its key's two items).
	damages := []struct {
		what   string
		damage func(l *format.Layout, sp []byte)
	}{
		{"not a leaf", func(l *format.Layout, sp []byte) {
			setFlags(l, sp, pFlags(l, sp)&^format.PLeaf)
		}},
		{"the first item has duplicates of its own", func(l *format.Layout, sp []byte) {
			off, _ := ptrAt(l, sp, 0)
			format.Put16(sp, off+4, format.FDupData) // mn_flags
		}},
		{"the first item's key runs past the sub-page", func(l *format.Layout, sp []byte) {
			off, _ := ptrAt(l, sp, 0)
			format.Put16(sp, off+6, 0xffff) // mn_ksize
		}},
	}
	for _, d := range damages {
		t.Run(d.what, func(t *testing.T) {
			txn := beginW(t, env)
			defer txn.Abort()
			for i := uint32(0); i < 3; i++ {
				putDups(t, txn, db, i, 'v')
			}
			a, err := seekInPlace(t, txn, db, posKey(2))
			must(t, err)
			ca := mainRef(a)
			ki := txn.cur(ca).kiTop()
			leaf, err := txn.topPageMut(ca)
			must(t, err)
			start, end, ok := nodeDataRange(txn.l, leaf, ki)
			if !ok {
				t.Fatal("no node data")
			}
			d.damage(txn.l, leaf[start:end])
			txn.closeSlot(a)
			checkPositioning(t, txn, db, posKey(2))
		})
	}
}

// A sub-DB record without a root: positioning on its key in place fails
// with MDB_CORRUPTED, and mdb_put and mdb_del on the key change nothing
// (through the read path the put would insert the key a second time, as C
// does).
func TestASubDBRecordWithoutARootIsCorrupt(t *testing.T) {
	forFormats(t, func(t *testing.T, fm format.Format) {
		env, _ := openTest(t, fm, positioningOptions(fm, 64<<20))
		txn := beginW(t, env)
		db := openDupsDB(t, txn, 0)
		// Keys 0 (one item), 2 (a sub-page) and 4 (a sub-DB).
		for i := uint32(0); i < 3; i++ {
			putDups(t, txn, db, i, 'v')
		}
		a, err := seekInPlace(t, txn, db, posKey(4))
		must(t, err)
		ca := mainRef(a)
		ki := txn.cur(ca).kiTop()
		leaf, err := txn.topPageMut(ca)
		must(t, err)
		nkeys := numKeys(txn.l, leaf)
		start, end, ok := nodeDataRange(txn.l, leaf, ki)
		if !ok {
			t.Fatal("no node data")
		}
		rec, ok := format.ReadDB(leaf[start:end])
		if !ok {
			t.Fatal("no sub-DB record")
		}
		rec.Root = format.PInvalid
		rec.Write(leaf[start:end])
		txn.closeSlot(a)
		must(t, txn.Commit())

		// A failed put leaves the transaction unusable, as in C: each
		// operation gets its own.
		txn = beginW(t, env)
		a, err = seekInPlace(t, txn, db, posKey(4))
		if err != format.Corrupted {
			t.Fatalf("positioning: %v, want MDB_CORRUPTED", err)
		}
		txn.closeSlot(a)
		if err := txn.Put(db, posKey(4), bytes.Repeat([]byte{'x'}, 100), 0); err != format.Corrupted {
			t.Fatalf("put: %v, want MDB_CORRUPTED", err)
		}
		txn.Abort()
		txn = beginW(t, env)
		if err := txn.Del(db, posKey(4), nil); err != format.Corrupted {
			t.Fatalf("del: %v, want MDB_CORRUPTED", err)
		}
		txn.Abort()
		// Nothing changed: the leaf still holds the same keys, and the
		// other keys are still reachable.
		txn = beginW(t, env)
		defer txn.Abort()
		a, err = seekInPlace(t, txn, db, posKey(2))
		must(t, err)
		page, err := txn.topPage(mainRef(a))
		must(t, err)
		if page.NumKeys() != nkeys {
			t.Fatalf("%d keys in the leaf, want %d", page.NumKeys(), nkeys)
		}
		if n, ok := page.Node(ki); !ok || !n.IsSubData() {
			t.Fatal("the sub-DB node is gone")
		}
		txn.closeSlot(a)
	})
}
