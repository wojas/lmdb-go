package engine

// MDB_CURRENT on a MDB_DUPSORT key's only item with an item that does not
// fit a node with the key (rlmdb KNOWN_C_BUGS.md #38; rlmdb
// tests/common/differential.rs current_single_item_script and
// single_item_on_overflow_pages, write_differential.rs
// diff_current_single_item and c_single_item_on_overflow_pages,
// interop/tests/write_differential10.rs diff10_current_single_item and
// c10_single_item_on_overflow_pages). C stores the item on overflow pages,
// an F_BIGDATA node in a DUPSORT database, which the key's next duplicate
// put reads as inline data; the Go writer, as rlmdb, deletes the item and
// inserts the new one, which leaves the file C's mdb_cursor_del +
// mdb_cursor_put leave (the script's "cr" op), and repairs a file C left in
// that state.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// pageSizes10 are the 1.0 page sizes of the differential.
var pageSizes10 = []int{1024, 4096, 16384, 65536}

// nodemax09 is 0.9's me_nodemax for pages of ps bytes.
func nodemax09(ps int) int { return (((ps - 16) / 2) &^ 1) - 2 }

// maxKey09 is MDB_MAXKEYSIZE of LMDB 0.9.
const maxKey09 = 511

// emptyDataFile09 is a new 0.9 environment's data file with ps-byte
// pages: the two meta pages mdb_env_init_meta writes. C 0.9 creates
// environments with the OS page size only, but opens a file of any page
// size (mdb_env_open2 takes mm_psize), and so does the Go engine.
func emptyDataFile09(ps int) []byte {
	l := format.LayoutV09
	file := make([]byte, 2*ps)
	m := format.NewEnvMeta(l, uint32(ps), 64<<20, 0)
	m.WriteInitialPages(file, ps, l)
	return file
}

// oracle09File is C 0.9 on environments that start from a 0.9 data file
// of ps-byte pages (oracle.dataFile).
func oracle09File(t testing.TB, ps int) *oracle {
	o := oracle09(t)
	o.dataFile = emptyDataFile09(ps)
	return o
}

// currentSingleItemScript is a script (two cursors, a MDB_DUPSORT DB) of
// MDB_CURRENT puts that replace a key's only item with items around the
// largest that fits a node with the key, for an oracle with me_nodemax
// nodemax and maximum key (and item) size maxkey, keys of klen bytes,
// padded to an even length in nodes if evenKeys (1.0).
//
// Where the new item does not fit a node, the op is "cr": C deletes the
// item and puts the new one, which is what the Go writer's MDB_CURRENT
// must write there (C's would store the item on overflow pages, #38).
// Elsewhere both sides put with MDB_CURRENT: items that fit, and one byte
// over maxkey (MDB_BAD_VALSIZE, nothing changed). The other cursor sits on
// the same item (where the next key has a single item too) or on the key
// before it. The replaced keys then get a second duplicate, a refused
// MDB_NODUPDATA put of the item, a lookup and some deletes; two
// replacements run in nested transactions, one committed, one aborted.
func currentSingleItemScript(nodemax, maxkey int, evenKeys bool, klen int) []string {
	const n = 40
	kpad := klen
	if evenKeys {
		kpad = (klen + 1) &^ 1
	}
	key := func(i int) string {
		k := []byte(fmt.Sprintf("k%03d", i))
		for len(k) < klen {
			k = append(k, 'q')
		}
		return hexs(k[:klen])
	}
	item := func(length, seed int) string {
		b := make([]byte, length)
		for i := range b {
			b[i] = byte(i*7 + seed*13 + 1)
		}
		return hexs(b)
	}
	var ops []string
	add := func(format string, args ...any) { ops = append(ops, fmt.Sprintf(format, args...)) }
	// Every fifth key has duplicates on a sub-page, the others one item.
	dups := map[int]bool{}
	for i := 0; i < n; i += 5 {
		dups[i] = true
	}
	for i := 0; i < n; i++ {
		add("p:%s:%s:0", key(i), item(3+i%7, i))
		if dups[i] {
			for j := 1; j < 4; j++ {
				add("p:%s:%s:0", key(i), item(3+j, 100+i+j))
			}
		}
	}
	// Largest item that fits a node with the key, and around it.
	fit := max(nodemax-8-kpad, 0)
	var lens []int
	for _, l := range []int{1, fit, fit + 1, fit + 2, maxkey - 1, maxkey, maxkey + 1} {
		if l >= 1 && l <= maxkey+1 {
			lens = append(lens, l)
		}
	}
	lens = sortedUnique(lens)
	var single []int
	for i := 0; i < n; i++ {
		if i%5 != 0 {
			single = append(single, i)
		}
	}
	nextSingle := func() int {
		i := single[0]
		single = single[1:]
		return i
	}
	// replace replaces key i's item by one of length bytes, the other
	// cursor on the same item or the key before; true if C must delete and
	// put.
	replace := func(i, length int, same bool) bool {
		d := item(length, i)
		add("g:0:%d:%s", format.Set, key(i))
		other := i - 1
		if same {
			other = i
		}
		add("g:1:%d:%s", format.Set, key(other))
		routed := length <= maxkey && 8+kpad+length > nodemax
		if routed {
			add("cr:0:%s:%s", key(i), d)
		} else {
			add("cp:0:%s:%s:%d", key(i), d, format.Current)
		}
		add("g:0:%d", format.GetCurrent)
		return routed
	}
	var routed [][2]int
	for k, length := range lens {
		for _, same := range []bool{false, true} {
			i := nextSingle()
			// After the delete, the other cursor is on the next key: keep
			// it off duplicates there (C's sub-cursor fixups after a
			// delete are another matter, KNOWN_C_BUGS.md #18).
			same = same && i+1 < n && !dups[i+1]
			if replace(i, length, same) {
				routed = append(routed, [2]int{i, length})
				dups[i] = true
			}
			if k%2 == 1 {
				add("g:1:%d", format.Next)
			}
		}
	}
	for _, r := range routed {
		i, d := r[0], item(r[1], r[0])
		add("p:%s:%s:0", key(i), item(5, 900+i))
		add("p:%s:%s:%d", key(i), d, format.NoDupData)
		add("g:1:%d:%s:%s", format.GetBoth, key(i), d)
		add("g:1:%d", format.NextDup)
	}
	if len(routed) > 0 {
		add("d:%s:%s", key(routed[0][0]), item(routed[0][1], routed[0][0]))
	}
	if len(routed) > 1 {
		add("d:%s", key(routed[1][0]))
	}
	big := 0
	for _, l := range lens {
		if l <= maxkey {
			big = l
		}
	}
	for _, end := range []string{"nc", "na"} {
		i := nextSingle()
		add("nb")
		replace(i, big, false)
		if end == "nc" {
			add("p:%s:%s:0", key(i), item(4, 700+i))
		}
		add("%s", end)
		add("g:0:%d", format.GetCurrent)
	}
	add("g:0:%d", format.First)
	for i := 0; i < 2*n; i++ {
		add("g:0:%d", format.Next)
	}
	return ops
}

// sortedUnique sorts v and drops repeated values.
func sortedUnique(v []int) []int {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	out := v[:0]
	for i, x := range v {
		if i == 0 || x != v[i-1] {
			out = append(out, x)
		}
	}
	return out
}

func hasCR(ops []string) bool {
	for _, op := range ops {
		if strings.HasPrefix(op, "cr:") {
			return true
		}
	}
	return false
}

// TestDifferential09CurrentSingleItem: the script with keys of 500 bytes.
// At the OS page size every 0.9 item fits a node with any key (both are at
// most 511 bytes, me_nodemax is at least 2038): both sides replace in
// place. With 2 KiB pages, a file C 0.9 does not create but opens, the
// larger items do not fit: C's MDB_CURRENT would store them on overflow
// pages, the Go writer's deletes and inserts, and must leave the file C's
// mdb_cursor_del + mdb_cursor_put leave, page for page (compareDataFiles,
// which skips the bytes C 0.9 leaves uninitialised).
func TestDifferential09CurrentSingleItem(t *testing.T) {
	o := oracle09(t)
	p := &profile{dbflags: format.DupSort}
	ops := currentSingleItemScript(nodemax09(os.Getpagesize()), maxKey09, false, 500)
	if hasCR(ops) {
		t.Fatal("a cr op at the OS page size")
	}
	if !runRound(t, o, p, 2, nil, 0, ops, "current", 64<<20) {
		t.Fatal("C stopped")
	}
	ops = currentSingleItemScript(nodemax09(2048), maxKey09, false, 500)
	if !hasCR(ops) {
		t.Fatal("no cr op with 2 KiB pages")
	}
	if !runRound(t, oracle09File(t, 2048), p, 2, nil, 0, ops, "current-2k", 64<<20) {
		t.Fatal("C stopped")
	}
}

// TestDifferential10CurrentSingleItem: the script at every page size 1.0
// allows, with keys of 103 bytes (fewer where keys are limited to less).
// With 4 KiB pages it includes the Go fuzzer's reproducer, an item of 1978
// bytes (the maximum) replacing one under a 103-byte key, then a second
// duplicate.
func TestDifferential10CurrentSingleItem(t *testing.T) {
	tool(t, "writeops10")
	p := &profile{dbflags: format.DupSort}
	for _, ps := range append([]int{256, 512}, pageSizes10...) {
		maxkey := nodemax10(ps) - 56
		ops := currentSingleItemScript(nodemax10(ps), maxkey, true, min(maxkey, 103))
		if !hasCR(ops) {
			t.Fatalf("page size %d: no cr op", ps)
		}
		if !runRound(t, oracle10(t, ps), p, 2, nil, 0, ops, fmt.Sprintf("current-%d", ps), 64<<20) {
			t.Fatalf("page size %d: C stopped", ps)
		}
	}
}

// TestSingleItemOnOverflowPages09: with 2 KiB pages C 0.9's MDB_CURRENT
// leaves a key's only item (511 bytes, the key 500) on overflow pages; the
// Go writer converts, replaces and deletes it correctly, in a MDB_DUPSORT
// and a MDB_DUPFIXED DB.
func TestSingleItemOnOverflowPages09(t *testing.T) {
	o := oracle09File(t, 2048)
	for _, flags := range []uint32{format.DupSort, format.DupSort | format.DupFixed} {
		t.Run(fmt.Sprintf("flags=%#x", flags), func(t *testing.T) {
			singleItemOnOverflowPages(t, o, flags, 500, maxKey09)
		})
	}
}

// TestSingleItemOnOverflowPages10: the same with C 1.0, whose MDB_CURRENT
// leaves an item of the maximum key size under a key of 103 bytes (or the
// maximum) on overflow pages at every page size (4 KiB: the Go fuzzer's
// shape).
func TestSingleItemOnOverflowPages10(t *testing.T) {
	tool(t, "writeops10")
	for _, ps := range []int{256, 1024, 4096, 65536} {
		maxkey := nodemax10(ps) - 56
		for _, flags := range []uint32{format.DupSort, format.DupSort | format.DupFixed} {
			t.Run(fmt.Sprintf("%d/flags=%#x", ps, flags), func(t *testing.T) {
				singleItemOnOverflowPages(t, oracle10(t, ps), flags, min(maxkey, 103), maxkey)
			})
		}
	}
}

// overflowItems are the items of singleItemOnOverflowPages: big is the one
// C stored on overflow pages, the others are of its size.
type overflowItems struct {
	key, big []byte
	after    []byte // sorts after big
	before   []byte // sorts before big
}

// repairCase is a case of singleItemOnOverflowPages: what it does to the
// key in a write transaction (cursor c is on its item), the result, the
// key's items afterwards and the overflow pages left (-1: as many as C's
// file has).
type repairCase struct {
	name  string
	op    func(txn *eng.Txn, dbi eng.DBI, c *eng.Cursor, it *overflowItems) error
	rc    error
	items [][]byte
	left  int
}

// singleItemOnOverflowPages (rlmdb KNOWN_C_BUGS.md #38): C's MDB_CURRENT
// replacing a key's only item in a MDB_DUPSORT DB (dbflags) by one of n
// bytes that does not fit a node with the key (of klen bytes) stores it on
// overflow pages. The oracle's writeops builds that state, between two
// other keys; the Go engine reads it, then changes it in each way that
// converts, replaces or deletes the item, starting from a copy of the
// C-built file each time: a duplicate put converts the item to duplicates
// (read through its overflow pages, which are freed), an equal one or
// MDB_CURRENT replaces it with a sub-DB (or an inline item if it fits),
// deletes and mdb_drop free the pages, and a MDB_NODUPDATA put of the
// item changes nothing. After each, md_overflow_pages is what the pages
// say, C's page audit passes, and C's dump is that of the expected
// contents written by the Go engine from scratch. A compacting copy of the
// C-built file passes the audit and dumps the same.
func singleItemOnOverflowPages(t *testing.T, o *oracle, dbflags uint32, klen, n int) {
	dupfixed := dbflags&format.DupFixed != 0
	fill := func(seed byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i)*7 + seed
		}
		return b
	}
	it := &overflowItems{key: bytes.Repeat([]byte("k"), klen), big: fill(0x41), after: fill(0x42), before: []byte{0}}
	if dupfixed {
		it.before = make([]byte, n)
	}
	ka, kz := bytes.Repeat([]byte("a"), klen), bytes.Repeat([]byte("z"), klen)
	newDir := func() string {
		dir := t.TempDir()
		if o.dataFile != nil {
			if err := os.WriteFile(filepath.Join(dir, "data.mdb"), o.dataFile, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	open := func(dir string) *eng.Env {
		t.Helper()
		opts := eng.Options{Format: o.f, MaxDBs: 8, MapSize: 64 << 20, Flags: format.NoLock | format.NoSync}
		if o.dataFile == nil {
			opts.PageSize = o.pageSize
		}
		env, err := eng.Open(dir, opts)
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
	overflowPages := func(dir string) int {
		t.Helper()
		env := open(dir)
		defer env.Close()
		txn, err := env.BeginTxn(nil, format.ReadOnly)
		if err != nil {
			t.Fatal(err)
		}
		defer txn.Abort()
		dbi, err := txn.OpenDBI([]byte("t"), 0)
		if err != nil {
			t.Fatal(err)
		}
		st, err := txn.Stat(dbi)
		if err != nil {
			t.Fatal(err)
		}
		return int(st.OverflowPages)
	}
	audit := func(dir, what string) {
		t.Helper()
		if rc, out, errs := run(t, o.peer, "check-freelist", dir); rc != 0 {
			t.Fatalf("%s: page audit:\n%s\n%s", what, out, errs)
		}
	}
	dump := func(dir string) string {
		_, d, _ := run(t, o.dump, "-s", "t", dir)
		return normDump(d)
	}
	// The expected contents, written by the Go engine from scratch.
	expectedDump := func(items [][]byte) string {
		t.Helper()
		dir := newDir()
		env := open(dir)
		txn, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		dbi, err := txn.OpenDBI([]byte("t"), dbflags|format.Create)
		if err != nil {
			t.Fatal(err)
		}
		puts := [][2][]byte{{ka, []byte("aa")}}
		for _, d := range items {
			puts = append(puts, [2][]byte{it.key, d})
		}
		puts = append(puts, [2][]byte{kz, []byte("zz")})
		for _, kv := range puts {
			if err := txn.Put(dbi, kv[0], kv[1], 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := txn.Commit(); err != nil {
			t.Fatal(err)
		}
		env.Close()
		return dump(dir)
	}

	// The state, as C leaves it.
	state := newDir()
	c := startC(t, o, state, dbflags, 1, 64<<20)
	for _, op := range []string{
		fmt.Sprintf("p:%s:%s:0", hexs(ka), hexs([]byte("aa"))),
		fmt.Sprintf("p:%s:%s:0", hexs(it.key), hexs([]byte("first"))),
		fmt.Sprintf("p:%s:%s:0", hexs(kz), hexs([]byte("zz"))),
		fmt.Sprintf("g:0:%d:%s", format.Set, hexs(it.key)),
		fmt.Sprintf("cp:0:%s:%s:%d", hexs(it.key), hexs(it.big), format.Current),
	} {
		line, ok := c.step(op)
		if f := strings.Fields(line); !ok || len(f) < 2 || f[1] != "rc=0" {
			_, errs := c.finish()
			t.Fatalf("C: %.40s: %q; stderr %s", op, line, errs)
		}
	}
	if commit, errs := c.finish(); commit != "commit rc=0" {
		t.Fatalf("C commit: %q; stderr %s", commit, errs)
	}
	audit(state, "C's state")
	ovpages := overflowPages(state)
	if ovpages == 0 {
		t.Fatal("C did not store the item on overflow pages")
	}
	func() {
		env := open(state)
		defer env.Close()
		txn, err := env.BeginTxn(nil, format.ReadOnly)
		if err != nil {
			t.Fatal(err)
		}
		defer txn.Abort()
		dbi, err := txn.OpenDBI([]byte("t"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if v, err := txn.Get(dbi, it.key); err != nil || !bytes.Equal(v, it.big) {
			t.Fatalf("C's item: %d bytes, %v", len(v), err)
		}
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			t.Fatal(err)
		}
		defer cur.Close()
		if _, _, err := cur.Get(it.key, it.big, format.GetBoth); err != nil {
			t.Fatalf("MDB_GET_BOTH of C's item: %v", err)
		}
		if cnt, err := cur.Count(); err != nil || cnt != 1 {
			t.Fatalf("count of C's item: %d, %v", cnt, err)
		}
	}()
	cDump := dump(state)
	if want := expectedDump([][]byte{it.big}); cDump != want {
		t.Fatalf("C's dump:\n%.600s\nwant:\n%.600s", cDump, want)
	}

	cases := []repairCase{
		{"a second duplicate", func(txn *eng.Txn, dbi eng.DBI, _ *eng.Cursor, it *overflowItems) error {
			return txn.Put(dbi, it.key, it.after, 0)
		}, nil, [][]byte{it.big, it.after}, 0},
		{"a duplicate that sorts first", func(txn *eng.Txn, dbi eng.DBI, _ *eng.Cursor, it *overflowItems) error {
			return txn.Put(dbi, it.key, it.before, 0)
		}, nil, [][]byte{it.before, it.big}, 0},
		{"MDB_NODUPDATA of the item", func(txn *eng.Txn, dbi eng.DBI, _ *eng.Cursor, it *overflowItems) error {
			return txn.Put(dbi, it.key, it.big, format.NoDupData)
		}, format.KeyExist, [][]byte{it.big}, -1},
		{"the item again", func(txn *eng.Txn, dbi eng.DBI, _ *eng.Cursor, it *overflowItems) error {
			return txn.Put(dbi, it.key, it.big, 0)
		}, nil, [][]byte{it.big}, 0},
		{"MDB_APPENDDUP of a smaller item", func(txn *eng.Txn, dbi eng.DBI, _ *eng.Cursor, it *overflowItems) error {
			return txn.Put(dbi, it.key, it.before, format.AppendDup)
		}, format.KeyExist, [][]byte{it.big}, 0},
		{"MDB_CURRENT of another item of the size", func(_ *eng.Txn, _ eng.DBI, c *eng.Cursor, it *overflowItems) error {
			return c.Put(it.key, it.after, format.Current)
		}, nil, [][]byte{it.after}, 0},
		{"mdb_del of the key", func(txn *eng.Txn, dbi eng.DBI, _ *eng.Cursor, it *overflowItems) error {
			return txn.Del(dbi, it.key, nil)
		}, nil, nil, 0},
		{"mdb_del of the item", func(txn *eng.Txn, dbi eng.DBI, _ *eng.Cursor, it *overflowItems) error {
			return txn.Del(dbi, it.key, it.big)
		}, nil, nil, 0},
		{"mdb_cursor_del", func(_ *eng.Txn, _ eng.DBI, c *eng.Cursor, _ *overflowItems) error {
			return c.Del(0)
		}, nil, nil, 0},
	}
	if dupfixed {
		cases = append(cases,
			repairCase{"MDB_MULTIPLE starting with the item", func(_ *eng.Txn, _ eng.DBI, c *eng.Cursor, it *overflowItems) error {
				both := append(bytes.Clone(it.big), it.after...)
				cnt, err := c.PutMulti(it.key, both, len(it.big), 0)
				if err == nil && cnt != 2 {
					return fmt.Errorf("MDB_MULTIPLE stored %d items, want 2", cnt)
				}
				return err
			}, nil, [][]byte{it.big, it.after}, 0},
			repairCase{"MDB_CURRENT of another size", func(_ *eng.Txn, _ eng.DBI, c *eng.Cursor, it *overflowItems) error {
				return c.Put(it.key, []byte("tiny"), format.Current)
			}, format.BadValSize, [][]byte{it.big}, -1})
	} else {
		cases = append(cases, repairCase{"MDB_CURRENT of a small item", func(_ *eng.Txn, _ eng.DBI, c *eng.Cursor, it *overflowItems) error {
			return c.Put(it.key, []byte("tiny"), format.Current)
		}, nil, [][]byte{[]byte("tiny")}, 0})
	}
	copyState := func() string {
		t.Helper()
		dir := t.TempDir()
		data, err := os.ReadFile(filepath.Join(state, "data.mdb"))
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, "data.mdb"), data, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, rc := range cases {
		dir := copyState()
		func() {
			env := open(dir)
			defer env.Close()
			txn, err := env.BeginTxn(nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer txn.Abort()
			dbi, err := txn.OpenDBI([]byte("t"), dbflags)
			if err != nil {
				t.Fatal(err)
			}
			cur, err := txn.OpenCursor(dbi)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := cur.Get(it.key, nil, format.Set); err != nil {
				t.Fatal(err)
			}
			if err := rc.op(txn, dbi, cur, it); err != rc.rc {
				t.Fatalf("%s: %v, want %v", rc.name, err, rc.rc)
			}
			cur.Close()
			if err := txn.Commit(); err != nil {
				t.Fatalf("%s: commit: %v", rc.name, err)
			}
		}()
		left := rc.left
		if left < 0 {
			left = ovpages
		}
		if got := overflowPages(dir); got != left {
			t.Fatalf("%s: md_overflow_pages %d, want %d", rc.name, got, left)
		}
		audit(dir, rc.name)
		if got, want := dump(dir), expectedDump(rc.items); got != want {
			t.Fatalf("%s: dump:\n%.600s\nwant:\n%.600s", rc.name, got, want)
		}
	}
	dir := copyState()
	func() {
		env := open(dir)
		defer env.Close()
		txn, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		dbi, err := txn.OpenDBI([]byte("t"), dbflags)
		if err == nil {
			err = txn.Drop(dbi, false)
		}
		if err == nil {
			err = txn.Commit()
		} else {
			txn.Abort()
		}
		if err != nil {
			t.Fatalf("mdb_drop: %v", err)
		}
	}()
	if got := overflowPages(dir); got != 0 {
		t.Fatalf("mdb_drop: md_overflow_pages %d", got)
	}
	audit(dir, "mdb_drop")
	compact := t.TempDir()
	func() {
		env := open(state)
		defer env.Close()
		if err := env.Copy(compact, format.CopyCompact); err != nil {
			t.Fatalf("compacting copy: %v", err)
		}
	}()
	audit(compact, "compacting copy")
	if got := dump(compact); got != cDump {
		t.Fatalf("compacting copy: dump:\n%.600s\nwant:\n%.600s", got, cDump)
	}
}

// TestDifferential10DupfixedItemsOverAQuarterPage: MDB_DUPFIXED items
// over a quarter page, which only 1.0 allows (in 0.9 items are at most 511
// bytes and pages at least 4 KiB): a key's second item turns its single
// item into a sub-DB at once, because the fake sub-page (two items and
// room for two more) is larger than a page. C's unsigned offset arithmetic
// wraps there and still gives the root its mp_upper. With 64 KiB pages and
// items over 16 KiB the fake sub-page is larger than 64 KiB, so its 16-bit
// mp_upper wraps too, and C's sum wraps back (rlmdb
// diff10_dupfixed_items_over_a_quarter_page).
func TestDifferential10DupfixedItemsOverAQuarterPage(t *testing.T) {
	tool(t, "writeops10")
	for _, c := range []struct{ ps, size int }{
		{512, 150}, {512, 186}, {1024, 300}, {1024, 442}, {65536, 20000}, {65536, 32698},
	} {
		t.Run(fmt.Sprintf("%d/%d", c.ps, c.size), func(t *testing.T) {
			p := profile{dbflags: format.DupSort | format.DupFixed, keys: 20, dupfixedSize: c.size,
				appenddupNodemax: nodemax10(c.ps)}
			// Items of 20 and 32 KiB: fewer and shorter rounds.
			rounds, n := 4, 300
			if c.ps == 65536 {
				rounds, n = 2, 60
			}
			runProfile(t, oracle10(t, c.ps), p, 0x1da+uint64(c.size), rounds, n)
		})
	}
}
