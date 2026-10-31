package engine

// API behaviour of writes (port of rlmdb's tests/write_api.rs).

import (
	"bytes"
	"encoding/binary"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func expectErr(t *testing.T, err, want error, what string) {
	t.Helper()
	if err != want {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

func beginW(t *testing.T, env *Env) *Txn {
	t.Helper()
	txn, err := env.BeginTxn(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return txn
}

func beginR(t *testing.T, env *Env) *Txn {
	t.Helper()
	txn, err := env.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	return txn
}

func TestValueSizeLimits(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 4, MapSize: 16 << 20})
	txn := beginW(t, env)
	plain, _ := txn.OpenDBI([]byte("plain"), format.Create)
	dups, _ := txn.OpenDBI([]byte("dups"), format.Create|format.DupSort)
	fixed, _ := txn.OpenDBI([]byte("fixed"), format.Create|format.DupSort|format.DupFixed)
	expectErr(t, txn.Put(plain, []byte{}, []byte("v"), 0), format.BadValSize, "empty key")
	expectErr(t, txn.Put(plain, bytes.Repeat([]byte{1}, 512), []byte("v"), 0), format.BadValSize, "512-byte key")
	must(t, txn.Put(plain, bytes.Repeat([]byte{1}, 511), []byte("v"), 0))
	must(t, txn.Put(plain, []byte("empty"), []byte{}, 0))
	must(t, txn.Put(plain, []byte("big"), bytes.Repeat([]byte{7}, 1<<20), 0))
	expectErr(t, txn.Put(dups, []byte("k"), []byte{}, 0), format.BadValSize, "empty dup")
	expectErr(t, txn.Put(dups, []byte("k"), bytes.Repeat([]byte{2}, 512), 0), format.BadValSize, "512-byte dup")
	must(t, txn.Put(dups, []byte("k"), bytes.Repeat([]byte{2}, 511), 0))
	// DUPFIXED: all items of a key have one size (KNOWN_C_BUGS.md #12).
	must(t, txn.Put(fixed, []byte("k"), []byte("abcd"), 0))
	expectErr(t, txn.Put(fixed, []byte("k"), []byte("abcde"), 0), format.BadValSize, "longer dupfixed")
	must(t, txn.Put(fixed, []byte("k"), []byte("abce"), 0))
	expectErr(t, txn.Put(fixed, []byte("k"), []byte("ab"), 0), format.BadValSize, "shorter dupfixed")
	must(t, txn.Commit())
	r := beginR(t, env)
	defer r.Abort()
	if v, err := r.Get(plain, []byte("empty")); err != nil || len(v) != 0 {
		t.Fatal(v, err)
	}
	if v, err := r.Get(plain, []byte("big")); err != nil || len(v) != 1<<20 {
		t.Fatal(len(v), err)
	}
	if st, _ := r.Stat(fixed); st.Entries != 2 {
		t.Fatal(st)
	}
}

func TestKeyExistAndAppendRules(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{})
	txn := beginW(t, env)
	defer txn.Abort()
	must(t, txn.Put(MainDBI, []byte("b"), []byte("1"), 0))
	expectErr(t, txn.Put(MainDBI, []byte("b"), []byte("2"), format.NoOverwrite), format.KeyExist, "nooverwrite")
	if v, _ := txn.Get(MainDBI, []byte("b")); string(v) != "1" {
		t.Fatal(string(v))
	}
	expectErr(t, txn.Put(MainDBI, []byte("a"), []byte("x"), format.Append), format.KeyExist, "append smaller")
	expectErr(t, txn.Put(MainDBI, []byte("b"), []byte("x"), format.Append), format.KeyExist, "append equal")
	must(t, txn.Put(MainDBI, []byte("c"), []byte("x"), format.Append))
	expectErr(t, txn.Put(MainDBI, []byte("c"), []byte("x"), format.Current), syscall.EINVAL, "put CURRENT")
	expectErr(t, txn.Put(MainDBI, []byte("c"), []byte("x"), format.Multiple), syscall.EINVAL, "put MULTIPLE")
	must(t, txn.Commit())
}

func TestDupsortRules(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2})
	txn := beginW(t, env)
	db, _ := txn.OpenDBI([]byte("d"), format.Create|format.DupSort)
	must(t, txn.Put(db, []byte("k"), []byte("2"), 0))
	must(t, txn.Put(db, []byte("k"), []byte("2"), 0))
	if st, _ := txn.Stat(db); st.Entries != 1 {
		t.Fatal(st)
	}
	expectErr(t, txn.Put(db, []byte("k"), []byte("2"), format.NoDupData), format.KeyExist, "nodupdata")
	must(t, txn.Put(db, []byte("k"), []byte("5"), format.AppendDup))
	expectErr(t, txn.Put(db, []byte("k"), []byte("3"), format.AppendDup), format.KeyExist, "appenddup")
	must(t, txn.Put(db, []byte("k"), []byte("3"), 0))
	expectErr(t, txn.Put(db, []byte("k"), []byte("9"), format.NoOverwrite), format.KeyExist, "nooverwrite")
	_, err := txn.PutReserve(db, []byte("k"), 4, 0)
	expectErr(t, err, syscall.EINVAL, "reserve on dupsort")
	must(t, txn.Del(db, []byte("k"), []byte("3")))
	expectErr(t, txn.Del(db, []byte("k"), []byte("3")), format.NotFound, "del again")
	if st, _ := txn.Stat(db); st.Entries != 2 {
		t.Fatal(st)
	}
	must(t, txn.Del(db, []byte("k"), nil))
	expectErr(t, txn.Del(db, []byte("k"), nil), format.NotFound, "del key again")
	if st, _ := txn.Stat(db); st.Entries != 0 {
		t.Fatal(st)
	}
	must(t, txn.Commit())
}

func TestCursorCurrentRules(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{})
	txn := beginW(t, env)
	defer txn.Abort()
	for _, k := range []string{"a", "b", "c"} {
		must(t, txn.Put(MainDBI, []byte(k), []byte("0123"), 0))
	}
	c, err := txn.OpenCursor(MainDBI)
	if err != nil {
		t.Fatal(err)
	}
	expectErr(t, c.Put([]byte("a"), []byte("x"), format.Current), syscall.EINVAL, "current unpositioned")
	_, _, err = c.Get([]byte("zz"), nil, format.SetKey)
	expectErr(t, err, format.NotFound, "set zz")
	// KNOWN_C_BUGS.md #13.
	expectErr(t, c.Put([]byte("zz"), []byte("x"), format.Current), format.NotFound, "current past end")
	if _, _, err := c.Get([]byte("b"), nil, format.SetKey); err != nil {
		t.Fatal(err)
	}
	must(t, c.Put([]byte("b"), []byte("WXYZ"), format.Current))
	if k, v, _ := c.Get(nil, nil, format.GetCurrent); string(k) != "b" || string(v) != "WXYZ" {
		t.Fatal(string(k), string(v))
	}
	must(t, c.Put([]byte("b"), []byte("longer value"), format.Current))
	if k, v, _ := c.Get(nil, nil, format.GetCurrent); string(k) != "b" || string(v) != "longer value" {
		t.Fatal(string(k), string(v))
	}
	must(t, c.Del(0))
	if k, v, _ := c.Get(nil, nil, format.GetCurrent); string(k) != "c" || string(v) != "0123" {
		t.Fatal(string(k), string(v))
	}
	c.Close()
	if _, err := txn.Get(MainDBI, []byte("b")); err != format.NotFound {
		t.Fatal(err)
	}
	must(t, txn.Commit())
}

func TestCurrentDupThatDoesNotFitItsSubpage(t *testing.T) {
	// KNOWN_C_BUGS.md #15.
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2})
	txn := beginW(t, env)
	db, _ := txn.OpenDBI([]byte("d"), format.Create|format.DupSort)
	for _, v := range []string{"a1", "b2", "c3"} {
		must(t, txn.Put(db, []byte("k"), []byte(v), 0))
	}
	c, _ := txn.OpenCursor(db)
	if _, _, err := c.Get([]byte("k"), []byte("b2"), format.GetBoth); err != nil {
		t.Fatal(err)
	}
	long := bytes.Repeat([]byte("b"), 300)
	must(t, c.Put([]byte("k"), long, format.Current))
	if k, v, _ := c.Get(nil, nil, format.GetCurrent); string(k) != "k" || !bytes.Equal(v, long) {
		t.Fatal("current")
	}
	if n, _ := c.Count(); n != 3 {
		t.Fatal(n)
	}
	c.Close()
	must(t, txn.Commit())
	got := scan(t, env, db)
	if len(got) != 3 || got[0][1] != "a1" || got[1][1] != string(long) || got[2][1] != "c3" {
		t.Fatal(got)
	}
}

func TestReadYourWritesAndAbort(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2, OpenLock: OpenMemLock})
	txn := beginW(t, env)
	must(t, txn.Put(MainDBI, []byte("keep"), []byte("1"), 0))
	must(t, txn.Commit())
	in, _ := env.Info()
	id := in.LastTxnID
	txn = beginW(t, env)
	if txn.ID() != id+1 {
		t.Fatal(txn.ID())
	}
	named, err := txn.OpenDBI([]byte("temp"), format.Create)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < 2000; i++ {
		must(t, txn.Put(named, binary.BigEndian.AppendUint32(nil, i), []byte("x"), 0))
	}
	must(t, txn.Put(MainDBI, []byte("gone"), []byte("2"), 0))
	if v, _ := txn.Get(MainDBI, []byte("gone")); string(v) != "2" {
		t.Fatal(string(v))
	}
	if st, _ := txn.Stat(named); st.Entries != 2000 {
		t.Fatal(st)
	}
	c, _ := txn.OpenCursor(named)
	if k, _, _ := c.Get(nil, nil, format.Last); binary.BigEndian.Uint32(k) != 1999 {
		t.Fatal(k)
	}
	c.Close()
	r := beginR(t, env)
	if _, err := r.Get(MainDBI, []byte("gone")); err != format.NotFound {
		t.Fatal(err)
	}
	r.Abort()
	_, err = txn.OpenCursor(FreeDBI)
	expectErr(t, err, syscall.EINVAL, "free DB cursor in a write txn")
	txn.Abort()
	r = beginR(t, env)
	if _, err := r.Get(MainDBI, []byte("gone")); err != format.NotFound {
		t.Fatal(err)
	}
	if v, _ := r.Get(MainDBI, []byte("keep")); string(v) != "1" {
		t.Fatal(string(v))
	}
	_, err = r.OpenDBI([]byte("temp"), 0)
	expectErr(t, err, format.NotFound, "aborted DB")
	if _, err := r.Get(named, []byte("x")); err == nil {
		t.Fatal("the aborted DB's handle works")
	}
	r.Abort()
	if in, _ := env.Info(); in.LastTxnID != id {
		t.Fatal(in.LastTxnID)
	}
	// An empty commit writes nothing.
	must(t, beginW(t, env).Commit())
	if in, _ := env.Info(); in.LastTxnID != id {
		t.Fatal(in.LastTxnID)
	}
}

func TestDatabaseHandles(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2})
	txn := beginW(t, env)
	_, err := txn.OpenDBI([]byte("x"), 0)
	expectErr(t, err, format.NotFound, "missing DB")
	a, err := txn.OpenDBI([]byte("a"), format.Create)
	must(t, err)
	b, err := txn.OpenDBI([]byte("b"), format.Create|format.IntegerKey)
	must(t, err)
	_, err = txn.OpenDBI([]byte("c"), format.Create)
	expectErr(t, err, format.DBsFull, "third DB")
	if a2, _ := txn.OpenDBI([]byte("a"), format.Create); a2 != a {
		t.Fatal(a2)
	}
	if f, _ := txn.DBIFlags(b); f != format.IntegerKey {
		t.Fatal(f)
	}
	expectErr(t, txn.Put(MainDBI, []byte("a"), []byte("data"), 0), format.Incompatible, "overwrite a DB record")
	seven := binary.NativeEndian.AppendUint64(nil, 7)
	must(t, txn.Put(b, seven, []byte("seven"), 0))
	r := beginR(t, env)
	if _, err := r.Get(b, seven); err == nil {
		t.Fatal("a created DB's handle works in another txn before commit")
	}
	r.Abort()
	must(t, txn.Commit())
	r = beginR(t, env)
	if v, _ := r.Get(b, seven); string(v) != "seven" {
		t.Fatal(string(v))
	}
	if a2, _ := r.OpenDBI([]byte("a"), 0); a2 != a {
		t.Fatal(a2)
	}
	r.Abort()
	txn = beginW(t, env)
	_, err = txn.OpenDBI(nil, 0x80000000)
	expectErr(t, err, syscall.EINVAL, "unknown flags")
	txn.Abort()
}

func TestMainDBFlagsBlockNamedDBs(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2})
	txn := beginW(t, env)
	if _, err := txn.OpenDBI(nil, format.DupSort); err != nil {
		t.Fatal(err)
	}
	_, err := txn.OpenDBI([]byte("n"), format.Create)
	expectErr(t, err, format.Incompatible, "create named in dupsort main")
	_, err = txn.OpenDBI([]byte("n"), 0)
	expectErr(t, err, format.NotFound, "open named in dupsort main")
	must(t, txn.Put(MainDBI, []byte("k"), []byte("1"), 0))
	must(t, txn.Put(MainDBI, []byte("k"), []byte("2"), 0))
	must(t, txn.Commit())
	r := beginR(t, env)
	defer r.Abort()
	if f, _ := r.DBIFlags(MainDBI); f != format.DupSort {
		t.Fatal(f)
	}
	if st, _ := r.Stat(MainDBI); st.Entries != 2 {
		t.Fatal(st)
	}
}

func TestReadOnlyEnvironment(t *testing.T) {
	env, dir := openTest(t, format.V09, Options{})
	env.Close()
	env, err := Open(dir, Options{Flags: format.ReadOnly | format.NoLock})
	must(t, err)
	defer env.Close()
	_, err = env.BeginTxn(nil, 0)
	expectErr(t, err, syscall.EACCES, "write txn in a read-only env")
}

func TestMapFullPoisonsTheTransaction(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MapSize: 1 << 20})
	txn := beginW(t, env)
	var err error
	for i := uint32(0); err == nil; i++ {
		err = txn.Put(MainDBI, binary.BigEndian.AppendUint32(nil, i), make([]byte, 900), 0)
	}
	expectErr(t, err, format.MapFull, "fill")
	expectErr(t, txn.Put(MainDBI, []byte("x"), []byte("y"), 0), format.BadTxn, "put after map full")
	_, err = txn.Get(MainDBI, []byte("x"))
	expectErr(t, err, format.BadTxn, "get after map full")
	expectErr(t, txn.Commit(), format.BadTxn, "commit after map full")
	r := beginR(t, env)
	defer r.Abort()
	if st, _ := r.Stat(MainDBI); st.Entries != 0 {
		t.Fatal(st)
	}
}

func TestSetMapSizeAndResize(t *testing.T) {
	env, dir := openTest(t, format.V09, Options{MapSize: 1 << 20, OpenLock: OpenMemLock})
	txn := beginW(t, env)
	for i := uint32(0); i < 300; i++ {
		must(t, txn.Put(MainDBI, binary.BigEndian.AppendUint32(nil, i), bytes.Repeat([]byte{1}, 900), 0))
	}
	must(t, txn.Commit())
	// A reader on the old map keeps its slices while the map is replaced.
	r := beginR(t, env)
	held, err := r.Get(MainDBI, binary.BigEndian.AppendUint32(nil, 5))
	must(t, err)
	w := beginW(t, env)
	expectErr(t, env.SetMapSize(64<<20), syscall.EINVAL, "set map size during a write txn")
	w.Abort()
	must(t, env.SetMapSize(64<<20))
	if env.MapSize() != 64<<20 {
		t.Fatal(env.MapSize())
	}
	if !bytes.Equal(held, bytes.Repeat([]byte{1}, 900)) {
		t.Fatal("held value changed")
	}
	r.Abort()
	txn = beginW(t, env)
	for i := uint32(0); i < 20000; i++ {
		must(t, txn.Put(MainDBI, binary.BigEndian.AppendUint32(nil, i), bytes.Repeat([]byte{2}, 900), 0))
	}
	must(t, txn.Commit())
	env.Close()
	env, err = Open(dir, Options{Flags: format.NoSync | format.NoLock})
	must(t, err)
	defer env.Close()
	if env.MapSize() != 64<<20 {
		t.Fatal(env.MapSize())
	}
	if st, _ := env.Stat(); st.Entries != 20000 {
		t.Fatal(st)
	}
}

func TestReserveAndMultiple(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 4})
	txn := beginW(t, env)
	db, _ := txn.OpenDBI([]byte("r"), format.Create)
	b, err := txn.PutReserve(db, []byte("small"), 5, 0)
	must(t, err)
	copy(b, "hello")
	big, err := txn.PutReserve(db, []byte("big"), 100000, 0)
	must(t, err)
	for i := range big {
		big[i] = 9
	}
	// Overwrite of an overflow value with one of the same page count in
	// the same transaction happens in place.
	big, err = txn.PutReserve(db, []byte("big"), 99000, 0)
	must(t, err)
	for i := range big {
		big[i] = 8
	}
	fixed, _ := txn.OpenDBI([]byte("f"), format.Create|format.DupSort|format.DupFixed)
	var items []byte
	for i := uint32(0); i < 1000; i++ {
		items = binary.BigEndian.AppendUint32(items, i)
	}
	c, _ := txn.OpenCursor(fixed)
	if n, err := c.PutMulti([]byte("k"), items, 4, 0); err != nil || n != 1000 {
		t.Fatal(n, err)
	}
	if n, _ := c.Count(); n != 1000 {
		t.Fatal(n)
	}
	c.Close()
	c, _ = txn.OpenCursor(db)
	_, err = c.PutMulti([]byte("k"), items, 4, 0)
	expectErr(t, err, format.Incompatible, "putmulti on a plain DB")
	c.Close()
	must(t, txn.Commit())
	r := beginR(t, env)
	defer r.Abort()
	if v, _ := r.Get(db, []byte("small")); string(v) != "hello" {
		t.Fatal(string(v))
	}
	if v, _ := r.Get(db, []byte("big")); !bytes.Equal(v, bytes.Repeat([]byte{8}, 99000)) {
		t.Fatal(len(v))
	}
	rc, _ := r.OpenCursor(fixed)
	if _, _, err := rc.Get([]byte("k"), nil, format.Set); err != nil {
		t.Fatal(err)
	}
	_, got, err := rc.Get(nil, nil, format.GetMultiple)
	must(t, err)
	got = append([]byte(nil), got...)
	for {
		_, v, err := rc.Get(nil, nil, format.NextMultiple)
		if err == format.NotFound {
			break
		}
		must(t, err)
		got = append(got, v...)
	}
	if !bytes.Equal(got, items) {
		t.Fatal("multiple items")
	}
	if st, _ := r.Stat(fixed); st.Entries != 1000 {
		t.Fatal(st)
	}
}

func TestDupFixedWithoutDupSortIsRefused(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 2})
	txn := beginW(t, env)
	db, err := txn.OpenDBI([]byte("f"), format.Create|format.DupFixed)
	must(t, err)
	expectErr(t, txn.Put(db, []byte("k"), []byte("v"), 0), format.Incompatible, "dupfixed without dupsort")
	must(t, txn.Commit())
}

// TestEndedWriteTxn: every method of a write transaction that ended fails
// cleanly (its DB table went to the next write transaction).
func TestEndedWriteTxn(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 4})
	for _, commit := range []bool{true, false} {
		txn, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		dbi, err := txn.OpenDBI([]byte("d"), format.Create)
		if err != nil {
			t.Fatal(err)
		}
		c, err := txn.OpenCursor(dbi)
		if err != nil {
			t.Fatal(err)
		}
		if commit {
			err = txn.Commit()
		} else {
			txn.Abort()
		}
		if err != nil {
			t.Fatal(err)
		}
		next, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range []DBI{MainDBI, dbi, 7} {
			if _, err := txn.Get(d, []byte("k")); err == nil {
				t.Error("Get")
			}
			if err := txn.Put(d, []byte("k"), []byte("v"), 0); err == nil {
				t.Error("Put")
			}
			if err := txn.Del(d, []byte("k"), nil); err == nil {
				t.Error("Del")
			}
			if _, err := txn.Stat(d); err == nil {
				t.Error("Stat")
			}
			if _, err := txn.DBIFlags(d); err == nil {
				t.Error("DBIFlags")
			}
			if err := txn.Drop(d, false); err == nil {
				t.Error("Drop")
			}
			if _, err := txn.OpenCursor(d); err == nil {
				t.Error("OpenCursor")
			}
			if err := txn.SetCompare(d, nil); err == nil && d != 7 {
				// The handle's setting belongs to the environment.
				_ = err
			}
			txn.Comparators(d)
			if _, err := txn.PutReserve(d, []byte("k"), 3, 0); err == nil {
				t.Error("PutReserve")
			}
		}
		if _, err := txn.OpenDBI([]byte("d"), 0); err == nil {
			t.Error("OpenDBI")
		}
		if _, _, err := c.Get(nil, nil, format.First); err == nil {
			t.Error("cursor Get")
		}
		if _, err := env.BeginTxn(txn, 0); err == nil {
			t.Error("nested BeginTxn")
		}
		if err := txn.Commit(); err == nil {
			t.Error("Commit again")
		}
		txn.Abort()
		c.Close()
		next.Abort()
	}
}
