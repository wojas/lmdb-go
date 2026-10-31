// ID lists: sorted page-number lists (MDB_IDL), ports of midl.c (rlmdb
// idl.rs).
//
// LMDB keeps free page numbers in descending order (MDB_IDL), in memory
// (me_pghead, mt_free_pgs) and on disk as the data of free-DB records:
// [count u64, pgno_1, ..., pgno_count], native endian. C stores the count
// in element 0 and uses 1-based indices; an IDL here is a slice of the IDs
// only, and Search returns a 0-based index (C's result minus one). What
// affects files is kept exactly: descending order, AppendRange element
// order, XMerge and the encoding. (The dirty list, MDB_ID2L, is the
// engine's dirty-page index.)

package format

import (
	"slices"
	"sort"
)

// IDL is a list of page numbers sorted in descending order (MDB_IDL).
type IDL []uint64

// Search is mdb_midl_search: the 0-based position of the first ID <= id
// (where id is or would be inserted); len if every ID is greater.
func (l IDL) Search(id uint64) int {
	return sort.Search(len(l), func(i int) bool { return l[i] <= id })
}

// Contains reports whether id is in the list.
func (l IDL) Contains(id uint64) bool {
	i := l.Search(id)
	return i < len(l) && l[i] == id
}

// AppendRange is mdb_midl_append_range: append the n IDs id .. id+n-1 in
// descending order (id+n-1 first), as C does.
func (l *IDL) AppendRange(id uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		*l = append(*l, id+uint64(i))
	}
}

// XMerge is mdb_midl_xmerge: merge the descending list merge into this
// descending list, keeping it sorted; duplicates are kept.
func (l *IDL) XMerge(merge IDL) {
	n := len(merge)
	j := len(*l)
	*l = slices.Grow(*l, n)[:j+n]
	ids := *l
	k := len(ids)
	for m := n - 1; m >= 0; m-- {
		mid := merge[m]
		for j > 0 && ids[j-1] < mid {
			ids[k-1] = ids[j-1]
			k--
			j--
		}
		ids[k-1] = mid
		k--
		if k == 0 {
			break
		}
	}
}

// XMergeEncoded is XMerge of the IDs of an encoded free-DB record. ok is
// false, merging nothing, if b is shorter than its count claims.
func (l *IDL) XMergeEncoded(b []byte) bool {
	ids, ok := EncodedIDs(b)
	if !ok {
		return false
	}
	n := len(ids) / 8
	j := len(*l)
	*l = slices.Grow(*l, n)[:j+n]
	s := *l
	k := len(s)
	for m := n - 1; m >= 0; m-- {
		mid := ne.Uint64(ids[8*m:])
		for j > 0 && s[j-1] < mid {
			s[k-1] = s[j-1]
			k--
			j--
		}
		s[k-1] = mid
		k--
		if k == 0 {
			break
		}
	}
	return true
}

// EncodedIDs is the ID bytes of an encoded free-DB record (8 bytes each);
// ok is false if b is shorter than its count claims.
func EncodedIDs(b []byte) ([]byte, bool) {
	if len(b) < 8 {
		return nil, false
	}
	n := ne.Uint64(b)
	if n > uint64(len(b)-8)/8 {
		return nil, false
	}
	return b[8 : 8+8*n], true
}

// EncodedLen is the count at the start of an encoded free-DB record (what
// mdb_stat -f sums up).
func EncodedLen(b []byte) (uint64, bool) { return Get64(b, 0) }

// FindRun is the run search of mdb_page_alloc: the 1-based C index i of
// the lowest page of num contiguous page numbers, preferring the tail
// (lowest numbers); ok is false if there is no such run.
func (l IDL) FindRun(num int) (int, bool) {
	n2 := num - 1
	if n2 < 0 {
		return 0, false
	}
	i := len(l)
	if i <= n2 {
		return 0, false
	}
	for {
		pgno := l[i-1]
		if l[i-1-n2] == pgno+uint64(n2) {
			return i, true
		}
		i--
		if i <= n2 {
			return 0, false
		}
	}
}

// RemoveRun removes the run FindRun found (C index i, num IDs) and returns
// its lowest page number.
func (l *IDL) RemoveRun(i, num int) uint64 {
	pgno := (*l)[i-1]
	*l = slices.Delete(*l, i-num, i)
	return pgno
}

// InsertRun inserts pgno..pgno+n-1 keeping the descending order, as
// mdb_ovpage_free does for me_pghead.
func (l *IDL) InsertRun(pgno uint64, n int) {
	at := l.Search(pgno)
	run := make([]uint64, n)
	for i := 0; i < n; i++ {
		run[i] = pgno + uint64(n-1-i)
	}
	*l = slices.Insert(*l, at, run...)
}

// Sort is mdb_midl_sort: sort descending.
func (l IDL) Sort() {
	slices.SortFunc(l, func(a, b uint64) int {
		switch {
		case a > b:
			return -1
		case a < b:
			return 1
		}
		return 0
	})
}

// IsSortedDesc reports strictly descending order.
func (l IDL) IsSortedDesc() bool {
	for i := 1; i < len(l); i++ {
		if l[i-1] <= l[i] {
			return false
		}
	}
	return true
}

// Bytes encodes the list as stored in the free DB: [count, ids...].
func (l IDL) Bytes() []byte {
	out := make([]byte, 8*(len(l)+1))
	l.EncodeInto(out)
	return out
}

// EncodeInto writes the encoding into out, which must be exactly that
// long; false, writing nothing, if it is not.
func (l IDL) EncodeInto(out []byte) bool {
	if len(out) != 8*(len(l)+1) {
		return false
	}
	ne.PutUint64(out, uint64(len(l)))
	for i, id := range l {
		ne.PutUint64(out[8+8*i:], id)
	}
	return true
}

// DecodeIDL decodes a free-DB record; ok is false if b is shorter than its
// count claims.
func DecodeIDL(b []byte) (IDL, bool) {
	ids, ok := EncodedIDs(b)
	if !ok {
		return nil, false
	}
	l := make(IDL, len(ids)/8)
	for i := range l {
		l[i] = ne.Uint64(ids[8*i:])
	}
	return l, true
}
