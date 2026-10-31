// Key and data comparators: exact ports of mdb_cmp_memn, mdb_cmp_memnr,
// mdb_cmp_long, mdb_cmp_int and mdb_cmp_cint (mdb.c lines 5321-5405) and the
// selection of mdb_default_cmp (rlmdb cmp.rs).
//
// The sort order of every B-tree is defined by these functions, so Go and C
// processes must agree bit for bit, including how keys of different lengths
// compare. Where C would read past a too short operand (integer comparators
// handed odd or mismatched sizes), the missing bytes count as zero; for
// well-formed integer keys the result is identical to C.

package format

import (
	"bytes"
	"runtime"
)

// CmpKind names one of LMDB's built-in comparators.
type CmpKind uint8

// The comparators.
const (
	// CmpMemn is mdb_cmp_memn: lexicographic, shorter first on a common
	// prefix.
	CmpMemn CmpKind = iota
	// CmpMemnr is mdb_cmp_memnr: lexicographic from the last byte.
	CmpMemnr
	// CmpLong is mdb_cmp_long: native u64. The free DB's order.
	CmpLong
	// CmpInt is mdb_cmp_int: native u32 of the first 4 bytes.
	CmpInt
	// CmpClong is mdb_cmp_clong: mdb_cmp_long on x86, mdb_cmp_cint
	// elsewhere (they agree on 8-byte operands).
	CmpClong
	// CmpCint is mdb_cmp_cint: native unsigned integer of the first
	// operand's size, compared 16 bits at a time from the most
	// significant end.
	CmpCint
	// CmpCustom is a user function (mdb_set_compare, mdb_set_dupsort).
	CmpCustom
)

// CmpFunc is a user comparison function: negative, zero or positive as a
// sorts before, with or after b.
type CmpFunc func(a, b []byte) int

// Comparator is one of the built-in comparators or a user function.
type Comparator struct {
	Kind CmpKind
	Fn   CmpFunc // for CmpCustom
}

// Built-in comparators as values.
var (
	Memn  = Comparator{Kind: CmpMemn}
	Memnr = Comparator{Kind: CmpMemnr}
	Long  = Comparator{Kind: CmpLong}
	Int   = Comparator{Kind: CmpInt}
	Clong = Comparator{Kind: CmpClong}
	Cint  = Comparator{Kind: CmpCint}
)

// Custom wraps a user function.
func Custom(f CmpFunc) Comparator { return Comparator{Kind: CmpCustom, Fn: f} }

// Compare compares a (the search key) with b (a stored key).
func (c Comparator) Compare(a, b []byte) int {
	switch c.Kind {
	case CmpMemn:
		return bytes.Compare(a, b)
	case CmpMemnr:
		return CmpMemnrBytes(a, b)
	case CmpLong:
		return cmpU64(nativeU64(a), nativeU64(b))
	case CmpInt:
		return cmpU32(nativeU32(a), nativeU32(b))
	case CmpClong:
		return CmpClongBytes(a, b)
	case CmpCint:
		return CmpCintBytes(a, b)
	}
	return c.Fn(a, b)
}

// CompareInPage compares a with the stored key b, where tail is b followed
// by the bytes after it in its page. The integer comparators read as many
// bytes of b as they need (len(a) for cint, 4 or 8 for int/long), so when
// a is longer than the stored key C reads what follows the key in the page
// (usually the node's data); using the tail reproduces that (rlmdb
// KNOWN_C_BUGS.md #7).
func (c Comparator) CompareInPage(a, b, tail []byte) int {
	switch c.Kind {
	case CmpLong, CmpInt, CmpClong, CmpCint:
		if len(tail) > len(b) {
			return c.Compare(a, tail)
		}
	}
	return c.Compare(a, b)
}

// IsInt reports mdb_cmp_int.
func (c Comparator) IsInt() bool { return c.Kind == CmpInt }

// IsCint reports mdb_cmp_cint.
func (c Comparator) IsCint() bool { return c.Kind == CmpCint }

// String names the comparator.
func (c Comparator) String() string {
	return [...]string{"Memn", "Memnr", "Long", "Int", "Clong", "Cint", "Custom"}[c.Kind]
}

// DefaultKeyCmp is the key comparator mdb_default_cmp picks for DB flags.
func DefaultKeyCmp(flags uint32) Comparator {
	switch {
	case flags&ReverseKey != 0:
		return Memnr
	case flags&IntegerKey != 0:
		return Cint
	}
	return Memn
}

// DefaultDupCmp is the duplicate comparator mdb_default_cmp picks for DB
// flags; ok is false if the DB is not MDB_DUPSORT.
func DefaultDupCmp(flags uint32) (Comparator, bool) {
	if flags&DupSort == 0 {
		return Comparator{}, false
	}
	switch {
	case flags&IntegerDup != 0:
		if flags&DupFixed != 0 {
			return Int, true
		}
		return Cint, true
	case flags&ReverseDup != 0:
		return Memnr, true
	}
	return Memn, true
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpU16(a, b uint16) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CmpMemnrBytes is mdb_cmp_memnr: compare from the end backwards over the
// common length, then the shorter operand sorts first.
func CmpMemnrBytes(a, b []byte) int {
	i, j := len(a)-1, len(b)-1
	for i >= 0 && j >= 0 {
		if a[i] != b[j] {
			if a[i] < b[j] {
				return -1
			}
			return 1
		}
		i--
		j--
	}
	return cmpInt(len(a), len(b))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CmpClongBytes is mdb_cmp_clong.
func CmpClongBytes(a, b []byte) int {
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "386" {
		return cmpU64(nativeU64(a), nativeU64(b))
	}
	return CmpCintBytes(a, b)
}

func nativeU64(b []byte) uint64 {
	if len(b) >= 8 {
		return ne.Uint64(b)
	}
	var t [8]byte
	copy(t[:], b)
	return ne.Uint64(t[:])
}

func nativeU32(b []byte) uint32 {
	if len(b) >= 4 {
		return ne.Uint32(b)
	}
	var t [4]byte
	copy(t[:], b)
	return ne.Uint32(t[:])
}

// byteOrZero is byte i of b, or 0 past either end.
func byteOrZero(b []byte, i int) byte {
	if i < 0 || i >= len(b) {
		return 0
	}
	return b[i]
}

// wordAt is the 16-bit word at byte offset i in host order, out-of-range
// bytes as zero.
func wordAt(b []byte, i int) uint16 {
	var t [2]byte
	t[0], t[1] = byteOrZero(b, i), byteOrZero(b, i+1)
	return ne.Uint16(t[:])
}

var littleEndian = func() bool {
	var b [2]byte
	ne.PutUint16(b[:], 1)
	return b[0] == 1
}()

// CmpCintBytes is mdb_cmp_cint: both operands are taken to be len(a) bytes
// long and compared as unsigned 16-bit words from the most significant
// word. For 4- and 8-byte operands this is a native integer comparison.
func CmpCintBytes(a, b []byte) int {
	if len(a) == len(b) {
		if len(a) == 8 {
			return cmpU64(ne.Uint64(a), ne.Uint64(b))
		}
		if len(a) == 4 {
			return cmpU32(ne.Uint32(a), ne.Uint32(b))
		}
	}
	n := len(a)
	if littleEndian {
		// u = a + size; do { x = *--u - *--c; } while (!x && u > a);
		u := n
		for {
			u -= 2
			r := cmpU16(wordAt(a, u), wordAt(b, u))
			if r != 0 || u <= 0 {
				return r
			}
		}
	}
	// u = a; do { x = *u++ - *c++; } while (!x && u < end);
	u := 0
	for {
		r := cmpU16(wordAt(a, u), wordAt(b, u))
		u += 2
		if r != 0 || u >= n {
			return r
		}
	}
}
