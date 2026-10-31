package engine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// fillEnv writes named databases of several kinds (plain with overflow
// values, DUPSORT with sub-pages and sub-DBs, DUPFIXED, DUPFIXED with
// 1-byte items) and deletes some items so the free list is not empty.
func fillEnv(t *testing.T, dir string, f format.Format, ps int) {
	env, err := eng.Open(dir, eng.Options{Format: f, PageSize: ps, MapSize: 256 << 20, MaxDBs: 8, Flags: format.NoLock | format.NoSync})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	for round := 0; round < 3; round++ {
		txn, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		plain, _ := txn.OpenDBI([]byte("plain"), format.Create)
		dups, _ := txn.OpenDBI([]byte("dups"), format.Create|format.DupSort)
		fixed, _ := txn.OpenDBI([]byte("fixed"), format.Create|format.DupSort|format.DupFixed)
		fixed1, _ := txn.OpenDBI([]byte("fixed1"), format.Create|format.DupSort|format.DupFixed)
		for i := 0; i < 3000; i++ {
			k := []byte(fmt.Sprintf("k%06d-%d", i*7919%3000, round))
			v := bytes.Repeat([]byte{byte(i)}, 10+i%300)
			if i%97 == 0 {
				v = bytes.Repeat([]byte{byte(i)}, 9000+i)
			}
			must(t, txn.Put(plain, k, v, 0))
			must(t, txn.Put(MainDBIOf(), k, v[:5], 0))
			for j := 0; j < 1+i%40; j++ {
				must(t, txn.Put(dups, k[:6], []byte(fmt.Sprintf("dup%05d", j*13+i%7)), 0))
				must(t, txn.Put(fixed, k[:5], []byte(fmt.Sprintf("%08d", j*31+i)), 0))
			}
		}
		for i := 0; i < 3000; i += 3 {
			_ = txn.Del(plain, []byte(fmt.Sprintf("k%06d-%d", i, round)), nil)
		}
		// 1-byte items: mp_upper of their P_LEAF2 sub-pages and pages lies
		// past the end, and the keys' size takes them to sub-DBs on 1 KiB
		// pages (mdb_page_copy of such a page in the compacting copy).
		for k := 0; k < 8; k++ {
			key := append(bytes.Repeat([]byte("f"), 300), byte('a'+k))
			for v := round; v < 256; v += 1 + k%3 {
				must(t, txn.Put(fixed1, key, []byte{byte(v)}, 0))
			}
			for v := round; v < 256 && k%2 == 1; v += 5 {
				_ = txn.Del(fixed1, key, []byte{byte(v)})
			}
		}
		must(t, txn.Commit())
	}
	if err := env.Audit(); err != nil {
		t.Fatal(err)
	}
}

// MainDBIOf is the main DB handle (a helper so fillEnv reads naturally).
func MainDBIOf() eng.DBI { return eng.MainDBI }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestCopyMatchesC: the plain and the compacting copy of a Go-written
// environment are byte for byte what C's mdb_copy and mdb_copy -c write
// (0.9 and 1.0, several page sizes).
func TestCopyMatchesC(t *testing.T) {
	type cs struct {
		f    format.Format
		ps   int
		copy string
	}
	cases := []cs{{format.V09, 0, "mdb_copy"}}
	for _, ps := range []int{1024, 4096, 65536} {
		cases = append(cases, cs{format.V10, ps, "mdb_copy10"})
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%v-%d", c.f, c.ps), func(t *testing.T) {
			tool(t, c.copy)
			src := t.TempDir()
			fillEnv(t, src, c.f, c.ps)
			env, err := eng.Open(src, eng.Options{Flags: format.NoLock | format.ReadOnly, MaxDBs: 8})
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			for _, compact := range []bool{false, true} {
				cdir, gdir := t.TempDir(), t.TempDir()
				args := []string{src, cdir}
				var flags uint32
				if compact {
					args = []string{"-c", src, cdir}
					flags = format.CopyCompact
				}
				if rc, _, errs := run(t, c.copy, args...); rc != 0 {
					t.Fatal(errs)
				}
				if err := env.Copy(gdir, flags); err != nil {
					t.Fatal(err)
				}
				a, _ := os.ReadFile(filepath.Join(cdir, "data.mdb"))
				b, _ := os.ReadFile(filepath.Join(gdir, "data.mdb"))
				if !bytes.Equal(a, b) {
					n := 0
					for n < len(a) && n < len(b) && a[n] == b[n] {
						n++
					}
					t.Fatalf("compact=%v: copies differ at byte %d (len %d vs %d)", compact, n, len(a), len(b))
				}
				peer := "cpeer"
				if c.f == format.V10 {
					peer = "cpeer10"
				}
				if rc, out, errs := run(t, peer, "check-freelist", gdir); rc != 0 {
					t.Fatalf("check-freelist of the copy:\n%s\n%s", out, errs)
				}
			}
		})
	}
}
