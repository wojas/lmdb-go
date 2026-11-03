package engine

// FuzzWriteDifferential: the lock-step write differential against C
// (differential_test.go) with the script chosen by the fuzzer (go test
// -fuzz, see FUZZING.md at the repository root). The input picks the C
// oracle (writeops for C 0.9.37; writeops10 for C 1.0 with 1 and 4 KiB
// pages; the Go side optionally spilling), one of the profiles of
// TestDifferential09 and the number of cursors; the rest feeds the
// script generator in place of its random numbers, so every operation,
// key and value comes from the input while the generator keeps rlmdb's
// repairs of the C behaviours KNOWN_C_BUGS.md lists. After every step the
// cursors of C and Go must agree; the committed files must have the same
// contents (mdb_dump), pass C's page audit and hold the same pages.
// Slow (a C process per input); skipped without the C oracles (RLMDB_DIR
// or RLMDB_BIN).

import (
	"fmt"
	"slices"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/fuzzwatch"
)

func FuzzWriteDifferential(f *testing.F) {
	fuzzwatch.TuneGC()
	names := fuzzProfileNames()
	x := uint64(0x9e3779b97f4a7c15)
	for i, name := range []string{"plain", "dupsort", "dupfixed", "intdup", "nested_plain", "nested_dupsort"} {
		in := []byte{byte(i % 4), byte(slices.Index(names, name)), byte(1 + i%3), byte(i)}
		for len(in) < 900 {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			in = append(in, byte(x))
		}
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) < 4 {
			return
		}
		var o *oracle
		switch in[0] % 4 {
		case 0:
			o = oracle09(t)
		case 1:
			o = oracle09(t)
			o.dirtyLimit = 64
		case 2:
			o = oracle10(t, 1024)
		case 3:
			o = oracle10(t, 4096)
		}
		p := profiles(false)[names[int(in[1])%len(names)]].p
		if o.pageSize != 0 {
			p.appenddupNodemax = nodemax10(o.pageSize)
		}
		ncur := 1 + int(in[2])%4
		src := in[4:min(len(in), 4+1600)]
		nops := len(src) / 4
		r := &rng{s: uint64(in[3])<<1 | 1, src: src}
		g := &gen{r: r, p: &p, ncur: ncur, prefill: nops / 3, heal: -1}
		runRound(t, o, &p, ncur, g, nops, nil, fmt.Sprintf("fuzz-%x", in[:4]), 64<<20)
	})
}

// fuzzProfileNames are the names of the short profiles, sorted.
func fuzzProfileNames() []string {
	var names []string
	for name := range profiles(false) {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
