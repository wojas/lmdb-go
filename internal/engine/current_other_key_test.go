package engine

// MDB_CURRENT with a key other than the cursor's (rlmdb KNOWN_C_BUGS.md
// row 58; port of rlmdb's tests/write_api.rs current_with_another_key).
// tests/engine compares the files with C's (TestDifferential*CurrentOtherKey).

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// C writes the caller's key over the node where it rewrites it (data of
// another size, or too large for the overflow pages in place), leaving the
// leaf unsorted, and keeps the old key where it overwrites the data in
// place; the port keeps the cursor's key for every size. Wrong keys that
// sort before and after every key, next to the item, equal to a
// neighbour's and longer than any; data of the same size, larger, smaller,
// on overflow pages (in place and not) and reserved; then puts of keys
// between the items, which C would place by the unsorted leaf. Both
// formats, small 1.0 pages too.
func TestCurrentWithAnotherKey(t *testing.T) {
	shapes := []struct {
		f  format.Format
		ps int
	}{{format.V09, 0}, {format.V10, 0}, {format.V10, 256}, {format.V10, 1024}}
	for _, s := range shapes {
		for _, flags := range []uint32{0, format.DupSort} {
			t.Run(fmt.Sprintf("%v/ps=%d/flags=%#x", s.f, s.ps, flags), func(t *testing.T) {
				currentOtherKey(t, s.f, s.ps, flags)
			})
		}
	}
}

// otherKeyModel is a database's contents: key -> its items in order.
type otherKeyModel map[string][][]byte

func (m otherKeyModel) pairs() [][2]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out [][2]string
	for _, k := range keys {
		for _, v := range m[k] {
			out = append(out, [2]string{k, string(v)})
		}
	}
	return out
}

// verifyPairs checks that a read transaction walks dbi as want, forwards
// and backwards, and finds every pair with MDB_GET_BOTH (MDB_SET without
// duplicates).
func verifyPairs(t *testing.T, env *Env, dbi DBI, dupsort bool, want [][2]string, what string) {
	t.Helper()
	got := scan(t, env, dbi)
	if !slices.Equal(got, want) {
		t.Fatalf("%s: the database holds %d pairs, want %d (first difference at %d)", what, len(got), len(want), firstDiff(got, want))
	}
	r := beginR(t, env)
	defer r.Abort()
	c, err := r.OpenCursor(dbi)
	must(t, err)
	defer c.Close()
	for i := len(want) - 1; i >= 0; i-- {
		k, v, err := c.Get(nil, nil, format.Prev)
		if err != nil || string(k) != want[i][0] || string(v) != want[i][1] {
			t.Fatalf("%s: backwards at %d: %q %q %v", what, i, k, v, err)
		}
	}
	for _, p := range want {
		op, data := format.Set, []byte(nil)
		if dupsort {
			op, data = format.GetBoth, []byte(p[1])
		}
		if _, v, err := c.Get([]byte(p[0]), data, op); err != nil || string(v) != p[1] {
			t.Fatalf("%s: search for %q: %q %v", what, p[0], v, err)
		}
	}
}

func firstDiff(a, b [][2]string) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// currentOtherKey is one shape of TestCurrentWithAnotherKey.
func currentOtherKey(t *testing.T, f format.Format, ps int, flags uint32) {
	env, _ := openTest(t, f, Options{MaxDBs: 4, PageSize: ps})
	st, err := env.Stat()
	must(t, err)
	psize := int(st.PageSize)
	maxkey := env.MaxKeySize()
	dupsort := flags&format.DupSort != 0
	key := func(i int) []byte { return fmt.Appendf(nil, "k%04d", i) }
	val := func(n, seed int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*7 + seed*13 + 1)
		}
		return b
	}
	// Enough keys of 8-byte values for several full leaves.
	n := max(4*psize/24, 60)
	model := otherKeyModel{}
	txn := beginW(t, env)
	db, err := txn.OpenDBI([]byte("d"), flags|format.Create)
	must(t, err)
	for i := range n {
		must(t, txn.Put(db, key(i), val(8, i), 0))
		model[string(key(i))] = [][]byte{val(8, i)}
	}
	// Every seventh key gets duplicates (on a sub-page) in a DUPSORT DB.
	dupItems := func(i int) [][]byte {
		var out [][]byte
		for _, d := range []byte{10, 20, 30} {
			out = append(out, append([]byte{'d', d}, val(6, i)...))
		}
		return out
	}
	if dupsort {
		for i := 3; i < n; i += 7 {
			must(t, txn.Del(db, key(i), nil))
			for _, d := range dupItems(i) {
				must(t, txn.Put(db, key(i), d, 0))
			}
			model[string(key(i))] = dupItems(i)
		}
	}
	c, err := txn.OpenCursor(db)
	must(t, err)
	// An empty key fails as in C (the size check comes first).
	_, _, err = c.Get(key(1), nil, format.Set)
	must(t, err)
	expectErr(t, c.Put([]byte{}, val(8, 0), format.Current), format.BadValSize, "empty key")
	wrongKeys := func(i int) [][]byte {
		return [][]byte{
			[]byte("a"),
			[]byte("zz"),
			append(key(i), 'x'),
			key(i + 1),
			key(i - 1),
			bytes.Repeat([]byte{'w'}, maxkey),
		}
	}
	// Data sizes: the same, larger, smaller, then (plain DB) on overflow
	// pages, overwritten there in place, and back inline; for DUPSORT the
	// largest item (it does not fit a node with 256-byte pages,
	// KNOWN_C_BUGS.md row 38).
	sizes := []int{8, 60, 2, 3 * psize, 3*psize - 100, 8}
	if dupsort {
		sizes = []int{8, 40, 2, maxkey, 8}
	}
	step := 0
	replace := func(i int, wrong, data []byte, reserve bool) string {
		step++
		what := fmt.Sprintf("key %d by %q.. (%d bytes), %d bytes of data, step %d",
			i, wrong[:min(len(wrong), 6)], len(wrong), len(data), step)
		if reserve {
			buf, err := c.PutReserve(wrong, len(data), format.Current)
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			copy(buf, data)
		} else if err := c.Put(wrong, data, format.Current); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		k, v, err := c.Get(nil, nil, format.GetCurrent)
		if err != nil || !bytes.Equal(k, key(i)) || !bytes.Equal(v, data) {
			t.Fatalf("%s: the cursor is on %q (%d bytes), %v", what, k, len(v), err)
		}
		return what
	}
	for i := 1; i < n-1; i += n / 9 {
		if len(model[string(key(i))]) > 1 {
			continue // duplicates: below
		}
		for _, wrong := range wrongKeys(i) {
			for s, size := range sizes {
				data := val(size, i+s)
				_, _, err := c.Get(key(i), nil, format.Set)
				must(t, err)
				reserve := !dupsort && s == 0
				what := replace(i, wrong, data, reserve)
				model[string(key(i))] = [][]byte{data}
				if v, err := txn.Get(db, key(i)); err != nil || !bytes.Equal(v, data) {
					t.Fatalf("%s: get: %d bytes, %v", what, len(v), err)
				}
				v, err := txn.Get(db, wrong)
				if other, ok := model[string(wrong)]; ok {
					if err != nil || !bytes.Equal(v, other[0]) {
						t.Fatalf("%s: get of the other key: %d bytes, %v", what, len(v), err)
					}
				} else if err != format.NotFound {
					t.Fatalf("%s: get of the other key: %d bytes, %v", what, len(v), err)
				}
			}
		}
	}
	if dupsort {
		// A duplicate replaced through a wrong key: the key stays, also
		// where the sub-page is rebuilt (an item of another size). The
		// middle one of the three is replaced by items between the other
		// two: of the same size, larger and smaller.
		for j, i := 0, 3; j < 4 && i < n; j, i = j+1, i+7 {
			news := []struct{ wrong, item []byte }{
				{[]byte("a"), append([]byte{'d', 20}, val(6, i+50)...)},
				{[]byte("zz"), append([]byte{'d', 21}, val(12, i)...)},
				{[]byte("k"), []byte{'d', 22}},
			}
			for _, nw := range news {
				mid := model[string(key(i))][1]
				_, _, err := c.Get(key(i), mid, format.GetBoth)
				must(t, err)
				what := replace(i, nw.wrong, nw.item, false)
				dups := model[string(key(i))]
				dups[1] = nw.item
				if !slices.IsSortedFunc(dups, bytes.Compare) {
					t.Fatalf("%s: the test's items are out of order", what)
				}
				if _, err := txn.Get(db, nw.wrong); err != format.NotFound {
					t.Fatalf("%s: get of the other key: %v", what, err)
				}
			}
		}
	}
	c.Close()
	must(t, txn.Commit())
	verifyPairs(t, env, db, dupsort, model.pairs(), "committed")
	mustAudit(t, env)
	// Keys between the items land in order (C's unsorted leaf would send
	// them elsewhere).
	txn = beginW(t, env)
	for i := 0; i < n; i += 3 {
		k := append(key(i), 'm')
		must(t, txn.Put(db, k, val(8, i), 0))
		model[string(k)] = [][]byte{val(8, i)}
	}
	must(t, txn.Commit())
	verifyPairs(t, env, db, dupsort, model.pairs(), "after the puts between")
	mustAudit(t, env)
}
