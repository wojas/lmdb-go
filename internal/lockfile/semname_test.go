// Tests of the semaphore names, ported from rlmdb os/shared_lock.rs.

package lockfile

import (
	"strings"
	"testing"
)

func TestFNV1aReferenceValues(t *testing.T) {
	// Standard FNV-1a 64 test vectors.
	for s, want := range map[string]uint64{
		"":       0xcbf29ce484222325,
		"a":      0xaf63dc4c8601ec8c,
		"foobar": 0x85944171f73967e8,
	} {
		if got := hashVal([]byte(s), hashInit); got != want {
			t.Errorf("hash(%q) = %#x, want %#x", s, got, want)
		}
	}
}

func TestNamesV09(t *testing.T) {
	e := hashEnc(lockFileID(0x01000004, 123456))
	if len(e) != 10 || strings.Trim(e, a85) != "" {
		t.Fatalf("encoding %q", e)
	}
	// Base 85, least significant digit first.
	if got := string(pack85V09(85*2+1, nil)); got != "12000" {
		t.Fatalf("pack85 = %q", got)
	}
	r, w := semNamesV09(lockFileID(1, 2))
	if !strings.HasPrefix(r, "/MDBr") || !strings.HasPrefix(w, "/MDBw") || r[5:] != w[5:] || len(r) != 15 {
		t.Fatalf("names %q %q", r, w)
	}
}

// TestNamesV10: "/MDB", the lock letter, then the hash in up to ten base-85
// digits, least significant first, without trailing zero digits.
func TestNamesV10(t *testing.T) {
	for _, c := range []struct {
		id   uint64
		ch   byte
		want string
	}{
		{85*2 + 1, 'r', "/MDBr12"},
		{85*2 + 1, 'w', "/MDBw12"},
		{0, 'r', "/MDBr"},
		{84, 'r', "/MDBr~"},
	} {
		if got := semNameV10(c.id, c.ch); got != c.want {
			t.Errorf("semNameV10(%d, %c) = %q, want %q", c.id, c.ch, got, c.want)
		}
	}
	// 85^10 > 2^64: every hash fits in ten digits, the largest needs all.
	max := semNameV10(^uint64(0), 'w')
	if len(max) != 15 {
		t.Fatalf("%q", max)
	}
	// The whole hash is encoded, not two 32-bit halves as in 0.9.
	var digits []byte
	for l := ^uint64(0); l != 0; l /= 85 {
		digits = append(digits, a85[l%85])
	}
	if max[5:] != string(digits) {
		t.Fatalf("%q != %q", max[5:], digits)
	}
}

func TestLockFileID(t *testing.T) {
	b := lockFileID(0xffffffff01020304, 0x1122334455667788)
	if len(b) != 16 || ne.Uint32(b) != 0x01020304 || ne.Uint32(b[4:]) != 0 || ne.Uint64(b[8:]) != 0x1122334455667788 {
		t.Fatalf("%x", b)
	}
}
