package engine

// searchPage's loop for the default order compares the first 8 bytes of
// both keys as integers before it calls bytes.Compare. Its results must be
// those of the generic loop with bytes.Compare (the same probes, the same
// signs), on sorted and unsorted pages (the probe sequence decides what an
// unsorted page yields), branch and leaf pages, and keys shorter than,
// equal to and sharing prefixes of 8 bytes.

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func TestSearchPageMemnPrefix(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []byte{0, 1, 'a', 'b', 0x7f, 0x80, 0xff}
	key := func() []byte {
		k := make([]byte, 1+rng.Intn(20))
		// Few symbols: many keys share their first 8 bytes.
		for i := range k {
			k[i] = alphabet[rng.Intn(len(alphabet))]
		}
		if rng.Intn(4) == 0 {
			copy(k, "prefix--")
		}
		return k
	}
	generic := format.Custom(bytes.Compare)
	for _, l := range []*format.Layout{format.LayoutV09, format.LayoutV10} {
		for iter := 0; iter < 2000; iter++ {
			flags := uint16(format.PLeaf)
			if iter%3 == 0 {
				flags = format.PBranch
			}
			p := make([]byte, 4096)
			format.PageBuilder{B: p, L: l}.Init(3, flags)
			keys := make([][]byte, rng.Intn(60))
			for i := range keys {
				keys[i] = key()
			}
			if iter%2 == 0 {
				// Sorted, as a valid page is (a branch page's first key is
				// not compared).
				for i := 1; i < len(keys); i++ {
					for j := i; j > 0 && bytes.Compare(keys[j-1], keys[j]) > 0; j-- {
						keys[j-1], keys[j] = keys[j], keys[j-1]
					}
				}
			}
			for i, k := range keys {
				body := nodeBody{kind: bodyInline, inline: []byte("v")}
				size := format.Even(format.NodeSize + l.NodeKeyLen(len(k)) + 1)
				if flags == format.PBranch {
					body = nodeBody{kind: bodyBranch, pgno: uint64(10 + i)}
					size = format.Even(format.NodeSize + l.NodeKeyLen(len(k)))
				}
				if _, ok := insertNode(l, p, i, size, k, 0, body); !ok {
					t.Fatalf("insertNode %d", i)
				}
			}
			mp := format.MakePage(p, l)
			for q := 0; q < 20; q++ {
				k := key()
				if q%4 == 0 && len(keys) > 0 {
					k = keys[rng.Intn(len(keys))]
				}
				i1, f1, e1, err1 := searchPage(mp, format.Memn, 0, k)
				i2, f2, e2, err2 := searchPage(mp, generic, 0, k)
				if i1 != i2 || f1 != f2 || e1 != e2 || err1 != err2 {
					t.Fatalf("%v page %d, key %q: Memn (%d %v %v %v), bytes.Compare (%d %v %v %v)",
						l.Format, iter, k, i1, f1, e1, err1, i2, f2, e2, err2)
				}
			}
		}
	}
}
