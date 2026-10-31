package data

import (
	"encoding/binary"
	"slices"
	"testing"
)

// TestGeneratorIsStable pins the generator (data.rs's generator_is_stable)
// so that a change here, which would also need one in rlmdb's data.rs and
// cbench.c, does not go unnoticed.
func TestGeneratorIsStable(t *testing.T) {
	if got := Splitmix64(0); got != 0xe220_a839_7b1d_cdaf {
		t.Fatalf("splitmix64(0) = %#x", got)
	}
	if got := binary.BigEndian.Uint64(SeqKeys(3).Key(2)[:8]); got != 2 {
		t.Fatalf("seq key 2 starts with %d", got)
	}
	p := Perm(SDelete, 100)
	sorted := slices.Clone(p)
	slices.Sort(sorted)
	for k, v := range sorted {
		if v != uint32(k) {
			t.Fatalf("Perm is not a permutation: %v", p)
		}
	}
	for _, i := range RandIdx(SGet, 1000, 7) {
		if i >= 7 {
			t.Fatalf("RandIdx out of range: %d", i)
		}
	}
}

// TestLSRecords is data.rs's ls_records: keys of 20-40 bytes of letters,
// digits and dots, unique; values with a header whose transaction id is at
// or below the last sync; update values of the same length.
func TestLSRecords(t *testing.T) {
	pool := Pool()
	var k [LSKeyMax]byte
	var v [LSValMax]byte
	keys := make(map[string]bool)
	for i := uint64(0); i < 100_000; i++ {
		kl := LSKey(i, &k)
		if kl < 20 || kl > LSKeyMax {
			t.Fatalf("key length %d", kl)
		}
		for _, b := range k[:kl] {
			if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '.') {
				t.Fatalf("key %q", k[:kl])
			}
		}
		if keys[string(k[:kl])] {
			t.Fatalf("duplicate key %d", i)
		}
		keys[string(k[:kl])] = true
		vl := LSValue(pool, i, &v)
		if vl < LSHdrLen+40 || vl > LSValMax {
			t.Fatalf("value length %d", vl)
		}
		txnid, ok := LSTxnIDOf(v[:vl])
		if !ok || txnid < 1 || txnid > LSSyncTxnID {
			t.Fatalf("header txnid %d", txnid)
		}
		if got := LSUpdateValue(pool, i, 3, 7, &v); got != vl {
			t.Fatalf("update value length %d, want %d", got, vl)
		}
		if txnid, _ := LSTxnIDOf(v[:vl]); txnid != LSSyncTxnID+4 {
			t.Fatalf("update txnid %d", txnid)
		}
	}
	if _, ok := LSTxnIDOf(make([]byte, LSHdrLen-1)); ok {
		t.Fatal("short value has a header")
	}
}

// TestDatacheckDeterministic: the hash depends on n only.
func TestDatacheckDeterministic(t *testing.T) {
	a, b, c := Datacheck(2000), Datacheck(2000), Datacheck(2001)
	if a != b || a == c {
		t.Fatal("Datacheck is not a function of n")
	}
}
