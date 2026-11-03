package engine

// MDB_CURRENT with a key that is not the cursor's (rlmdb KNOWN_C_BUGS.md
// row 58; port of rlmdb's tests/common/differential.rs
// current_other_key_script, write_differential.rs diff_current_other_key
// and interop/tests/write_differential10.rs diff10_current_other_key).
// Where C keeps the old key (data of the same size, overflow pages
// overwritten in place, duplicates of the same size) both sides put with
// the other key; where C would write it over the node, C puts with the
// cursor's key and Go with the other one (the script's "cw" op): the Go
// writer keeps the cursor's key and must leave the file C leaves for the
// right key, page for page.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// currentOtherKeyScript is a script (two cursors, a DB with dbflags) of
// MDB_CURRENT puts with a key that is not the cursor's, for an oracle of
// ps-byte pages with me_nodemax nodemax and maximum key (and duplicate)
// size maxkey, keys padded to an even length in nodes if evenKeys (1.0).
//
// Where C keeps the old key, both sides put with the other key ("cp"):
// data of the same size in the node; data on overflow pages that fits the
// pages it replaces (overwritten in place, as they are this
// transaction's); a duplicate of the same size on a sub-page or in a
// sub-DB of one leaf (C does not touch the key there). Where C would write
// the other key over the node (data of another size, to or from overflow
// pages, onto more overflow pages; another size for a key's only
// MDB_DUPSORT item), C puts with the cursor's key and Go with the other
// one ("cw"). The other keys sort before and after every key, right after
// the cursor's, are a neighbour's, or as long as keys may be; the other
// cursor is on the same item or the next key, and looks the other key up
// afterwards. Two keys are replaced in nested transactions, one committed,
// one aborted; then keys between the old ones are put and the database is
// walked.
func currentOtherKeyScript(dbflags uint32, ps, nodemax, maxkey int, evenKeys bool) []string {
	const n = 120
	const klen = 12
	dupsort := dbflags&format.DupSort != 0
	fixed := dbflags&format.DupFixed != 0
	knode := klen
	if evenKeys {
		knode = (klen + 1) &^ 1
	}
	key := func(i int) []byte {
		k := fmt.Appendf(nil, "k%04d", i)
		for len(k) < klen {
			k = append(k, 'q')
		}
		return k
	}
	val := func(size, seed int) []byte {
		b := make([]byte, size)
		for i := range b {
			b[i] = byte(i*7 + seed*13 + 1)
		}
		return b
	}
	// Items of a 40th of the page: a few dozen in a leaf.
	vs := max(4, min(ps/40, maxkey-2))
	// A larger item for a key's only MDB_DUPSORT item: within the maximum
	// item size and a node with the key (KNOWN_C_BUGS.md row 38 otherwise).
	largerDup := min(2*vs, maxkey, nodemax-8-knode)
	// Duplicates: tag and j (2 bytes) first, so that other filler bytes
	// keep their place.
	dup := func(tag byte, j, seed int) []byte {
		return append([]byte{tag, byte(j >> 8), byte(j)}, val(vs-3, seed)...)
	}
	subpageKey := func(i int) bool { return dupsort && i%5 == 2 }
	// A sub-DB of one leaf: past the sub-page's limit (about half a page),
	// three quarters of a leaf.
	hdr := 16
	if evenKeys {
		hdr = 24
	}
	itemSpace := ((8 + vs + 1) &^ 1) + 2
	if fixed {
		itemSpace = vs
	}
	nSub := 3 * (ps - hdr) / 4 / itemSpace
	subKey := func(i int) bool { return dupsort && i == n/2+1 }
	k := func(i int) string { return hexs(key(i)) }
	var ops []string
	add := func(f string, a ...any) { ops = append(ops, fmt.Sprintf(f, a...)) }
	for i := range n {
		switch {
		case subKey(i):
			for j := range nSub {
				add("p:%s:%s:0", k(i), hexs(dup('e', j, j)))
			}
		case subpageKey(i):
			for _, j := range []int{10, 20, 30} {
				add("p:%s:%s:0", k(i), hexs(dup('d', j, i)))
			}
		default:
			add("p:%s:%s:0", k(i), hexs(val(vs, i)))
		}
	}
	wrongN := 0
	wrong := func(i int) string {
		wrongN++
		neighbour := key(i - 1)
		if i+1 < n {
			neighbour = key(i + 1)
		}
		switch wrongN % 5 {
		case 0:
			return hexs([]byte("a"))
		case 1:
			return hexs([]byte("zz"))
		case 2:
			return hexs(append(key(i), 'x'))
		case 3:
			return hexs(neighbour)
		}
		return hexs([]byte(strings.Repeat("w", maxkey)))
	}
	// Replaces the item of key i the cursors are positioned on (pos: the
	// "g" op's tail) by d, with the other key on both sides ("cp") or on
	// the Go side only ("cw"); the other cursor on the same item or the
	// next key.
	nrep := 0
	replace := func(i int, pos string, d []byte, inPlace bool) {
		nrep++
		add("g:0:%s", pos)
		if nrep%2 == 0 && i+1 < n {
			add("g:1:%d:%s", format.Set, k(i+1))
		} else {
			add("g:1:%s", pos)
		}
		w := wrong(i)
		if inPlace {
			add("cp:0:%s:%s:%d", w, hexs(d), format.Current)
		} else {
			add("cw:0:%s:%s:%s", k(i), w, hexs(d))
		}
		add("g:0:%d", format.GetCurrent)
		add("g:1:%d:%s", format.Set, w)
	}
	// Keys across the DB (in a DUPSORT DB a third of them with a sub-page),
	// the last one, the sub-DB's.
	var picks []int
	for c := range 12 {
		picks = append(picks, c*n/12+c%3)
	}
	picks = append(picks, n-1, n/2+1)
	for c, i := range picks {
		end := ""
		switch len(picks) - c {
		case 3:
			end = "nc"
		case 4:
			end = "na"
		}
		if end != "" {
			add("nb")
		}
		set := fmt.Sprintf("%d:%s", format.Set, k(i))
		switch {
		case subKey(i):
			for _, j := range []int{0, nSub / 2, nSub - 1} {
				pos := fmt.Sprintf("%d:%s:%s", format.GetBoth, k(i), hexs(dup('e', j, j)))
				replace(i, pos, dup('e', j, 900+j), true)
			}
		case subpageKey(i):
			for _, j := range []int{20, 10, 30} {
				pos := fmt.Sprintf("%d:%s:%s", format.GetBoth, k(i), hexs(dup('d', j, i)))
				replace(i, pos, dup('d', j, 500+i), true)
			}
		case dupsort:
			replace(i, set, val(vs, i+1), true)
			if !fixed {
				if largerDup > vs {
					replace(i, set, val(largerDup, i+2), false)
					replace(i, set, val(largerDup, i+3), true)
				}
				replace(i, set, val(vs/2, i+4), false)
			}
		default:
			steps := []struct {
				size    int
				inPlace bool
			}{
				{vs, true},
				{2 * vs, false},
				{2 * vs, true},
				{vs / 2, false},
				// To overflow pages, three of them.
				{2*ps + 50, false},
				// In place on them: as many pages, fewer.
				{2*ps + 10, true},
				{ps + 100, true},
				// More pages: new ones.
				{4 * ps, false},
				// Back into the node.
				{vs, false},
			}
			for s, st := range steps {
				replace(i, set, val(st.size, i+s), st.inPlace)
			}
		}
		if end != "" {
			add("%s", end)
			add("g:0:%d", format.GetCurrent)
		}
	}
	more := 0
	for i := 0; i < n; i += 3 {
		add("p:%s:%s:0", hexs(append(key(i), 'm')), hexs(val(vs, i)))
		more++
	}
	add("g:0:%d", format.First)
	items := n + more
	if dupsort {
		items += nSub + 2*(n/5)
	}
	for range items {
		add("g:0:%d", format.Next)
	}
	return ops
}

// countOps is the number of ops with prefix and suffix.
func countOps(ops []string, prefix, suffix string) int {
	c := 0
	for _, op := range ops {
		if strings.HasPrefix(op, prefix) && strings.HasSuffix(op, suffix) {
			c++
		}
	}
	return c
}

// TestDifferential09CurrentOtherKey: 0.9 at the OS page size, in a plain,
// a MDB_DUPSORT and a MDB_DUPFIXED DB, also with a dirty limit of 16
// pages on the Go side.
func TestDifferential09CurrentOtherKey(t *testing.T) {
	o := oracle09(t)
	ps := os.Getpagesize()
	current := fmt.Sprintf(":%d", format.Current)
	for _, dbflags := range []uint32{0, format.DupSort, format.DupSort | format.DupFixed} {
		p := &profile{dbflags: dbflags}
		ops := currentOtherKeyScript(dbflags, ps, nodemax09(ps), maxKey09, false)
		if c := countOps(ops, "cp:", current); c < 10 {
			t.Fatalf("flags %#x: %d cp ops with MDB_CURRENT", dbflags, c)
		}
		if c := countOps(ops, "cw:", ""); dbflags&format.DupFixed == 0 && c < 10 {
			t.Fatalf("flags %#x: %d cw ops", dbflags, c)
		}
		for _, limit := range []int{0, 16} {
			o.dirtyLimit = limit
			tag := fmt.Sprintf("current-other-key-%#x-dirty%d", dbflags, limit)
			if !runRound(t, o, p, 2, nil, 0, ops, tag, 64<<20) {
				t.Fatalf("%s: C stopped", tag)
			}
		}
	}
}

// TestDifferential10CurrentOtherKey: 1.0 at 256 B, 1 KiB, 4 KiB and 64 KiB
// pages, in a plain, a MDB_DUPSORT and a MDB_DUPFIXED DB, also with a
// dirty limit of 16 pages on the Go side.
func TestDifferential10CurrentOtherKey(t *testing.T) {
	tool(t, "writeops10")
	current := fmt.Sprintf(":%d", format.Current)
	for _, ps := range []int{256, 1024, 4096, 65536} {
		maxkey := nodemax10(ps) - 56
		o := oracle10(t, ps)
		for _, dbflags := range []uint32{0, format.DupSort, format.DupSort | format.DupFixed} {
			p := &profile{dbflags: dbflags}
			ops := currentOtherKeyScript(dbflags, ps, nodemax10(ps), maxkey, true)
			if c := countOps(ops, "cp:", current); c < 10 {
				t.Fatalf("page size %d, flags %#x: %d cp ops with MDB_CURRENT", ps, dbflags, c)
			}
			for _, limit := range []int{0, 16} {
				o.dirtyLimit = limit
				tag := fmt.Sprintf("current-other-key-%d-%#x-dirty%d", ps, dbflags, limit)
				if !runRound(t, o, p, 2, nil, 0, ops, tag, 64<<20) {
					t.Fatalf("%s: C stopped", tag)
				}
			}
		}
	}
}
