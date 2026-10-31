package engine

// Corrupt-input robustness (after rlmdb's tests/robust_mutation.rs): valid
// environments written by the Go writer (0.9, and 1.0 with 1 KiB and 4 KiB
// pages) are damaged with random byte-level and structure-aware mutations
// (bit flips, zeroed and random ranges, swapped and duplicated pages,
// truncation, bogus mp_lower/mp_upper/mp_ptrs, child and overflow page
// numbers that are huge, point at meta pages or back up the tree, node
// sizes beyond the page, LEAF2 key sizes of 0, lying DB records), then
// opened, read (every cursor operation, walks both ways), written (puts,
// deletes, cursor writes, nested transactions, drops, commit) and copied
// (plain and compacting). The only requirement is robustness: no panic and
// no endless loop. Errors of any kind are fine.
//
// ROBUST_CASES=N sets the cases per base (default 40, 10 with -short);
// ROBUST_SEED=<base>:<seed> runs one case.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

type robustBase struct {
	name  string
	f     format.Format
	ps    int
	data  []byte
	names []string
}

func buildRobustBase(t *testing.T, name string, f format.Format, ps int) robustBase {
	dir := t.TempDir()
	env, err := Open(dir, Options{Format: f, PageSize: ps, MapSize: 64 << 20, MaxDBs: 8, Flags: format.NoLock | format.NoSync})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"plain", "dups", "fixed"}
	for round := 0; round < 2; round++ {
		txn, _ := env.BeginTxn(nil, 0)
		plain, _ := txn.OpenDBI([]byte("plain"), format.Create)
		dups, _ := txn.OpenDBI([]byte("dups"), format.Create|format.DupSort)
		fixed, _ := txn.OpenDBI([]byte("fixed"), format.Create|format.DupSort|format.DupFixed|format.IntegerDup)
		for i := 0; i < 600; i++ {
			k := []byte(fmt.Sprintf("key%05d", i*37%600))
			v := bytes.Repeat([]byte{byte(i + round)}, 1+i%90)
			if i%41 == 0 {
				v = bytes.Repeat([]byte{byte(i)}, 3*env.PageSize()+i)
			}
			must(t, txn.Put(plain, k, v, 0))
			must(t, txn.Put(MainDBI, append([]byte("m"), k...), v[:1], 0))
			for j := 0; j < 1+i%25; j++ {
				must(t, txn.Put(dups, k[:6], []byte(fmt.Sprintf("d%04d", j*7+round)), 0))
				must(t, txn.Put(fixed, k[:5], binary.NativeEndian.AppendUint64(nil, uint64(j*1000+i)), 0))
			}
		}
		for i := 0; i < 600; i += 4 {
			_ = txn.Del(plain, []byte(fmt.Sprintf("key%05d", i)), nil)
		}
		must(t, txn.Commit())
	}
	env.Close()
	data, err := os.ReadFile(filepath.Join(dir, "data.mdb"))
	if err != nil {
		t.Fatal(err)
	}
	return robustBase{name: name, f: f, ps: env.PageSize(), data: data, names: names}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mutate damages data (a whole data file of page size ps).
func mutate(r *rand.Rand, data []byte, ps int, l *format.Layout) []byte {
	npages := len(data) / ps
	page := func() int { return r.Intn(npages) }
	n := 1 + r.Intn(4)
	for m := 0; m < n; m++ {
		p := page()
		base := p * ps
		switch r.Intn(14) {
		case 0: // bit flips
			for i := 0; i < 1+r.Intn(8); i++ {
				data[r.Intn(len(data))] ^= 1 << r.Intn(8)
			}
		case 1: // zeroed range
			s := r.Intn(len(data))
			clear(data[s:min(len(data), s+1+r.Intn(ps))])
		case 2: // random range
			s := r.Intn(len(data))
			r.Read(data[s:min(len(data), s+1+r.Intn(64))])
		case 3: // swap two pages (not the metas)
			if npages > 3 {
				a, b := 2+r.Intn(npages-2), 2+r.Intn(npages-2)
				tmp := append([]byte(nil), data[a*ps:(a+1)*ps]...)
				copy(data[a*ps:], data[b*ps:(b+1)*ps])
				copy(data[b*ps:], tmp)
			}
		case 4: // duplicate a page
			if npages > 3 {
				a, b := 2+r.Intn(npages-2), 2+r.Intn(npages-2)
				copy(data[a*ps:(a+1)*ps], data[b*ps:(b+1)*ps])
			}
		case 5: // truncate
			data = data[:r.Intn(len(data))]
			npages = max(len(data)/ps, 1)
			if len(data) < ps {
				return data
			}
		case 6: // mp_lower / mp_upper
			v := []uint16{0, 1, 2, 8, 16, 17, 24, uint16(ps - 1), uint16(ps), 0x7fff, 0xffff}[r.Intn(11)]
			off := l.LowerOffset()
			if r.Intn(2) == 0 {
				off = l.UpperOffset()
			}
			binary.NativeEndian.PutUint16(data[base+off:], v)
		case 7: // a pointer
			i := r.Intn(16)
			if l.PageHdr+2*i+2 <= ps {
				binary.NativeEndian.PutUint16(data[base+l.PageHdr+2*i:], uint16(r.Intn(ps+64)))
			}
		case 8: // node header fields (anywhere in the node area)
			o := base + l.PageHdr + r.Intn(ps-l.PageHdr-8)
			vals := []uint16{0, 1, 2, 3, 4, 7, 0xffff, uint16(ps), uint16(r.Intn(65536))}
			binary.NativeEndian.PutUint16(data[o:], vals[r.Intn(len(vals))])
		case 9: // child or overflow page number: huge, meta, this page
			o := base + l.PageHdr + r.Intn(ps-l.PageHdr-8)
			vals := []uint64{0, 1, uint64(p), uint64(npages), 1 << 40, ^uint64(0)}
			binary.NativeEndian.PutUint64(data[o:], vals[r.Intn(len(vals))])
		case 10: // page flags
			fl := []uint16{0, format.PBranch, format.PLeaf, format.PLeaf | format.PLeaf2, format.POverflow, format.PMeta, 0xffff}
			binary.NativeEndian.PutUint16(data[base+l.FlagsOffset():], fl[r.Intn(len(fl))])
		case 11: // mp_pad (LEAF2 key size)
			binary.NativeEndian.PutUint16(data[base+l.PadOffset():], []uint16{0, 1, 3, uint16(ps), 0xffff}[r.Intn(5)])
		case 12: // a DB record in a meta page: root, depth, entries
			mi := r.Intn(2)
			if (mi+1)*ps > len(data) {
				break
			}
			o := mi*ps + l.MetaOffset() + 24 + r.Intn(2)*format.DBSize
			switch r.Intn(4) {
			case 0:
				binary.NativeEndian.PutUint64(data[o+40:], []uint64{0, 1, uint64(npages + 5), 1 << 50}[r.Intn(4)])
			case 1:
				binary.NativeEndian.PutUint16(data[o+6:], uint16(r.Intn(70)))
			case 2:
				binary.NativeEndian.PutUint64(data[o+32:], r.Uint64())
			case 3:
				binary.NativeEndian.PutUint16(data[o+4:], uint16(r.Intn(0x80)))
			}
		case 13: // last_pgno / txnid of a meta page
			mi := r.Intn(2)
			if (mi+1)*ps > len(data) {
				break
			}
			o := mi*ps + l.MetaOffset() + format.MetaLastPgno
			if r.Intn(2) == 0 {
				binary.NativeEndian.PutUint64(data[o:], []uint64{0, 1, uint64(npages * 2), 1 << 45}[r.Intn(4)])
			} else {
				binary.NativeEndian.PutUint64(data[o+8:], r.Uint64()%1000)
			}
		}
	}
	return data
}

// exercise reads, writes and copies the environment at dir. Errors are
// fine; steps bounds each walk.
var robustStats struct{ opened, read, items, committed int }

func exercise(dir string, names []string, r *rand.Rand) {
	env, err := Open(dir, Options{MapSize: 64 << 20, MaxDBs: 8, Flags: format.NoLock | format.NoSync})
	if err != nil {
		return
	}
	robustStats.opened++
	defer env.Close()
	_, _ = env.Stat()
	_, _ = env.Info()
	_ = env.Audit()
	_, _ = env.FreePages()
	walk := func(txn *Txn, dbi DBI) {
		c, err := txn.OpenCursor(dbi)
		if err != nil {
			return
		}
		defer c.Close()
		for _, op := range []format.CursorOp{format.Next, format.Prev, format.NextNoDup, format.PrevNoDup} {
			for i := 0; i < 5000; i++ {
				k, _, err := c.Get(nil, nil, op)
				if err != nil {
					break
				}
				robustStats.items++
				if i%50 == 0 && len(k) > 0 {
					_, _ = txn.Get(dbi, k)
					_, _, _ = c.Get(k, nil, format.SetRange)
					_, _ = c.Count()
					_, _, _ = c.Get(nil, nil, format.GetMultiple)
					_, _, _ = c.Get(nil, nil, format.NextMultiple)
					_, _, _ = c.Get(nil, nil, format.LastDup)
					_, _, _ = c.Get(nil, nil, format.FirstDup)
					_, _, _ = c.Get(nil, nil, format.PrevMultiple)
					_, _, _ = c.Get(k, []byte("d0001"), format.GetBothRange)
				}
			}
		}
		for op := format.CursorOp(0); op < 19; op++ {
			_, _, _ = c.Get([]byte("key00100"), []byte("d0007"), op)
		}
	}
	if txn, err := env.BeginTxn(nil, format.ReadOnly); err == nil {
		robustStats.read++
		walk(txn, MainDBI)
		walk(txn, FreeDBI)
		for _, n := range names {
			if dbi, err := txn.OpenDBI([]byte(n), 0); err == nil {
				walk(txn, dbi)
				_, _ = txn.Stat(dbi)
			}
		}
		txn.Abort()
	}
	// Writes.
	if txn, err := env.BeginTxn(nil, 0); err == nil {
		dbis := []DBI{MainDBI}
		for _, n := range names {
			if dbi, err := txn.OpenDBI([]byte(n), 0); err == nil {
				dbis = append(dbis, dbi)
			}
		}
		for i := 0; i < 300; i++ {
			dbi := dbis[r.Intn(len(dbis))]
			k := []byte(fmt.Sprintf("key%05d", r.Intn(700)))
			switch r.Intn(6) {
			case 0, 1:
				_ = txn.Put(dbi, k, bytes.Repeat([]byte{1}, 1+r.Intn(3000)), 0)
			case 2:
				_ = txn.Put(dbi, k[:5], binary.NativeEndian.AppendUint64(nil, r.Uint64()), format.NoDupData)
			case 3:
				_ = txn.Del(dbi, k, nil)
			case 4:
				if c, err := txn.OpenCursor(dbi); err == nil {
					if _, _, err := c.Get(k, nil, format.SetRange); err == nil {
						_ = c.Del(0)
						_ = c.Put(k, []byte("v"), format.Current)
					}
					c.Close()
				}
			case 5:
				if child, err := env.BeginTxn(txn, 0); err == nil {
					_ = child.Put(dbi, k, []byte("child"), 0)
					_ = child.Del(dbi, []byte("key00001"), nil)
					if r.Intn(2) == 0 {
						_ = child.Commit()
					} else {
						child.Abort()
					}
				}
			}
		}
		if r.Intn(4) == 0 && len(dbis) > 1 {
			_ = txn.Drop(dbis[1], r.Intn(2) == 0)
		}
		if txn.Commit() == nil {
			robustStats.committed++
		}
	}
	_ = env.CopyTo(io.Discard, 0)
	_ = env.CopyTo(io.Discard, format.CopyCompact)
}

func TestRobustMutation(t *testing.T) {
	cases := 40
	if testing.Short() {
		cases = 10
	}
	if s := os.Getenv("ROBUST_CASES"); s != "" {
		cases, _ = strconv.Atoi(s)
	}
	only := os.Getenv("ROBUST_SEED")
	bases := []robustBase{
		buildRobustBase(t, "v09", format.V09, 0),
		buildRobustBase(t, "v10-1k", format.V10, 1024),
		buildRobustBase(t, "v10-4k", format.V10, 4096),
	}
	for _, b := range bases {
		for c := 0; c < cases; c++ {
			seed := int64(c*7919 + 1)
			if only != "" {
				name, s, _ := strings.Cut(only, ":")
				if name != b.name {
					continue
				}
				seed, _ = strconv.ParseInt(s, 10, 64)
				if c > 0 {
					break
				}
			}
			runRobustCase(t, b, seed)
		}
	}
	t.Logf("%d bases x %d cases: opened %d, read %d (%d items), committed %d",
		len(bases), cases, robustStats.opened, robustStats.read, robustStats.items, robustStats.committed)
}

func runRobustCase(t *testing.T, b robustBase, seed int64) {
	r := rand.New(rand.NewSource(seed))
	data := mutate(r, append([]byte(nil), b.data...), b.ps, b.f.Layout())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.mdb"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan any, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Sprintf("%v\n%s", p, debug.Stack())
				return
			}
			done <- nil
		}()
		exercise(dir, b.names, r)
	}()
	select {
	case p := <-done:
		if p != nil {
			t.Fatalf("case %s:%d panicked: %v", b.name, seed, p)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("case %s:%d did not finish in 60 s", b.name, seed)
	}
}
