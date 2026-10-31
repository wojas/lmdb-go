package engine

// MDB_DUPFIXED items of 1 to 3 bytes in the write differential (rlmdb
// tests/write_differential.rs diff_dupfixed_small_items,
// diff_dupfixed_1byte_script, diff_nested_dupfixed_1byte,
// diff_dupfixed_1byte_max_key; interop/tests/write_differential10.rs
// diff10_dupfixed_small_items, diff10_dupfixed_1byte_script,
// diff10_dupfixed_1byte_upper_wraps). A P_LEAF2 page counts its items in
// mp_lower/mp_upper, mp_upper dropping by ksize - sizeof(indx_t) per item:
// unsigned in C, so for 1-byte items it grows past the page end, and on
// 1.0's 64 KiB pages it wraps past 2^16, while SIZELEFT (16-bit) stays the
// exact free space. With pages of 4 KiB and more the 256 possible 1-byte
// values of a key stay on a sub-page; 1.0's small pages take them to
// sub-DBs, whose P_LEAF2 pages split, merge and move.

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// pageSizes10Small are the 1.0 page sizes of the small-item tests: the
// differential's, and 256 and 512 bytes.
var pageSizes10Small = []int{256, 512, 1024, 4096, 16384, 65536}

// dupfixed1ByteScript is a script (two cursors) on an MDB_DUPSORT |
// MDB_DUPFIXED DB with 1-byte items: all 256 values of key in a scrambled
// order, read back with MDB_GET_MULTIPLE/MDB_NEXT_MULTIPLE, some deleted
// in an aborted nested transaction, then all but 16 deleted one by one and
// a few with the cursor (rlmdb differential::dupfixed_1byte_script_for).
func dupfixed1ByteScript(key []byte) []string {
	k := hexs(key)
	v := func(i int) string { return hexs([]byte{byte(i * 167 % 256)}) }
	var ops []string
	for i := 0; i < 256; i++ {
		ops = append(ops, fmt.Sprintf("p:%s:%s:0", k, v(i)))
	}
	ops = append(ops, fmt.Sprintf("g:0:%d", format.First), fmt.Sprintf("g:0:%d", format.GetMultiple))
	for i := 0; i < 4; i++ {
		ops = append(ops, fmt.Sprintf("g:0:%d", format.NextMultiple))
	}
	ops = append(ops, fmt.Sprintf("g:1:%d", format.Last), "nb")
	for i := 0; i < 40; i++ {
		ops = append(ops, fmt.Sprintf("d:%s:%s", k, v(i*3)))
	}
	ops = append(ops, "na")
	for i := 0; i < 240; i++ {
		ops = append(ops, fmt.Sprintf("d:%s:%s", k, v(i*91)))
		if i%16 == 0 {
			ops = append(ops, fmt.Sprintf("g:1:%d", format.PrevDup))
		}
	}
	ops = append(ops, fmt.Sprintf("g:0:%d", format.First), "cd:0:0", "cd:0:0", "cd:0:0",
		fmt.Sprintf("g:0:%d", format.GetMultiple))
	return ops
}

// dupfixed1BytePuts are puts of the 1-byte items 0..n-1, in that order,
// as duplicates of key (rlmdb differential::dupfixed_1byte_puts).
func dupfixed1BytePuts(key []byte, n int) []string {
	k := hexs(key)
	ops := make([]string, n)
	for v := range ops {
		ops[v] = fmt.Sprintf("p:%s:%s:0", k, hexs([]byte{byte(v)}))
	}
	return ops
}

// runScript runs one round of ops on two cursors; it must agree in full.
func runScript(t *testing.T, o *oracle, ops []string, tag string, mapSize int) {
	t.Helper()
	p := profile{dbflags: format.DupSort | format.DupFixed}
	if !runRound(t, o, &p, 2, nil, 0, ops, tag, mapSize) {
		t.Fatalf("%s: C stopped", tag)
	}
}

func TestDifferential09DupfixedSmallItems(t *testing.T) {
	o := oracle09(t)
	for size := 1; size <= 3; size++ {
		t.Run(fmt.Sprintf("%dbyte", size), func(t *testing.T) {
			p := profile{dbflags: format.DupSort | format.DupFixed, keys: 4, dupfixedSize: size}
			runProfile(t, o, p, 0xf0+uint64(size), 6, 1200)
		})
	}
	t.Run("nested1byte", func(t *testing.T) {
		p := profile{dbflags: format.DupSort | format.DupFixed, keys: 4, dupfixedSize: 1, nested: true, drops: true}
		runProfile(t, o, p, 0xf4, 6, 1200)
	})
}

// TestDifferential09Dupfixed1ByteScript: one key's sub-page grows to all
// 256 1-byte values and shrinks again; then the same with a key of 0.9's
// largest size (511 bytes), after puts of 0..99 in order. No 0.9 page can
// wrap mp_upper (pages of at most 32 KiB; see the 1.0 test).
func TestDifferential09Dupfixed1ByteScript(t *testing.T) {
	o := oracle09(t)
	runScript(t, o, dupfixed1ByteScript([]byte("k")), "1byte", 64<<20)
	key := bytes.Repeat([]byte("K"), 511)
	runScript(t, o, dupfixed1BytePuts(key, 100), "1byte-puts", 64<<20)
	runScript(t, o, dupfixed1ByteScript(key), "1byte-max-key", 64<<20)
}

func TestDifferential10DupfixedSmallItems(t *testing.T) {
	tool(t, "writeops10")
	for _, ps := range pageSizes10Small {
		o := oracle10(t, ps)
		for size := 1; size <= 3; size++ {
			t.Run(fmt.Sprintf("%d/%dbyte", ps, size), func(t *testing.T) {
				p := profile{dbflags: format.DupSort | format.DupFixed, keys: 4, dupfixedSize: size,
					appenddupNodemax: nodemax10(ps)}
				runProfile(t, o, p, 0x1f0+uint64(size+ps), 4, 1200)
			})
		}
	}
	// One key at 256-byte pages: its 1-byte items fill the 232 slots of a
	// P_LEAF2 page of its sub-DB, which splits (and merges again).
	t.Run("256/1byte-one-key", func(t *testing.T) {
		p := profile{dbflags: format.DupSort | format.DupFixed, keys: 1, dupfixedSize: 1,
			appenddupNodemax: nodemax10(256)}
		runProfile(t, oracle10(t, 256), p, 0x1fe, 4, 3000)
	})
}

func TestDifferential10Dupfixed1ByteScript(t *testing.T) {
	tool(t, "writeops10")
	for _, ps := range pageSizes10Small {
		t.Run(fmt.Sprint(ps), func(t *testing.T) {
			runScript(t, oracle10(t, ps), dupfixed1ByteScript([]byte("k")), "1byte", 64<<20)
		})
	}
}

// TestDifferential10Dupfixed1ByteUpperWraps: a P_LEAF2 page whose stored
// mp_upper wraps. With 64 KiB pages a key of 1.0's largest size (32,698
// bytes) leaves almost no room for a sub-page, so its 1-byte items soon go
// to a sub-DB, all of them on its leaf, which starts with mp_upper 65,512;
// each item raises it by one and the 24th wraps it to 0, mp_lower being
// 48. C's SIZELEFT is 16-bit, so its free space stays exact and C keeps
// the 100 items (1-byte values 0..99 in order) on that one page; a
// saturating SIZELEFT split on every later put and ran out of an 8-page
// map. The longer script with the same key then takes the page to all 256
// values and deletes them again (merges, PAGEFILL of the wrapped page).
//
// C with the upstream flags asserts MP_UPPER(mp) >= MP_LOWER(mp) at the
// top of mdb_node_add once mp_upper wrapped (rlmdb KNOWN_C_BUGS.md #37),
// so the C side is writeops10_ndebug.
func TestDifferential10Dupfixed1ByteUpperWraps(t *testing.T) {
	const ps = 65536
	o := oracle10NDebug(t, ps)
	key := bytes.Repeat([]byte("K"), nodemax10(ps)-8-48)
	if len(key) != 32698 {
		t.Fatalf("key of %d bytes, want mdb_env_get_maxkeysize 32698", len(key))
	}
	for _, pages := range []int{8, 1024} {
		runScript(t, o, dupfixed1BytePuts(key, 100), fmt.Sprintf("wrap-%dpages", pages), pages*ps)
	}
	runScript(t, o, dupfixed1ByteScript(key), "wrap-script", 64<<20)
}
