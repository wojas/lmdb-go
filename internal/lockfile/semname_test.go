// Tests of the semaphore and Windows mutex names, ported from rlmdb
// os/shared_lock.rs and os/windows.rs (fnv1a_and_names); they run on every
// platform.

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

// TestMutexNamesV09 is the 0.9 half of rlmdb's Windows fnv1a_and_names:
// mdb_pack85 takes a 32-bit unsigned long on Windows, so each half of the
// hash gives five digits of its own 32 bits.
func TestMutexNamesV09(t *testing.T) {
	if got := string(pack85Win32(85*2+1, nil)); got != "12000" {
		t.Fatalf("pack85 = %q", got)
	}
	id := lockFileIDWin32(0x1234abcd, 7, 0x89abcdef)
	h := hashVal(id, hashInit)
	want := string(pack85Win32(uint32(h>>32), pack85Win32(uint32(h), nil)))
	if got := hashEncWin32(id); got != want || len(got) != 10 {
		t.Fatalf("encoding %q, want %q", got, want)
	}
	if hashEncWin32(id) == hashEnc(id) {
		t.Fatal("the 32-bit encoding equals the 64-bit one")
	}
	r, w := mutexNamesV09(id)
	if r != `Global\MDBr`+want || w != `Global\MDBw`+want {
		t.Fatalf("names %q %q", r, w)
	}
	// The name and its NUL fill 22 of the 32 bytes of mti_rmname.
	if len(r)+1 != 22 {
		t.Fatalf("name length %d", len(r))
	}
}

// TestMutexNamesV10 is the 1.0 half: the prefix, the letter, then the whole
// hash.
func TestMutexNamesV10(t *testing.T) {
	for _, c := range []struct {
		id   uint64
		ch   byte
		want string
	}{
		{85*2 + 1, 'r', `Global\MDBr12`},
		{0, 'w', `Global\MDBw`},
	} {
		if got := mutexNameV10(c.id, c.ch); got != c.want {
			t.Errorf("mutexNameV10(%d, %c) = %q, want %q", c.id, c.ch, got, c.want)
		}
	}
	if max := mutexNameV10(^uint64(0), 'w'); len(max) != 11+10 || max[11:] != semNameV10(^uint64(0), 'w')[5:] {
		t.Fatalf("%q", max)
	}
}

// TestLockFileIDWin32: struct { DWORD volume; DWORD nhigh; DWORD nlow; },
// no padding.
func TestLockFileIDWin32(t *testing.T) {
	b := lockFileIDWin32(0x01020304, 0x05060708, 0x090a0b0c)
	if len(b) != 12 || ne.Uint32(b) != 0x01020304 || ne.Uint32(b[4:]) != 0x05060708 || ne.Uint32(b[8:]) != 0x090a0b0c {
		t.Fatalf("%x", b)
	}
}
