package lmdb

// FuzzEnvFile: damaged environments opened and used end to end through
// the public API (go test -fuzz, see FUZZING.md at the repository root).
//
// The fuzz input is a whole data file. The seeds are small valid
// environments of both formats written by this package (LMDB 0.9 with
// 2 KiB pages, LMDB 1.0 with 256-byte to 1 KiB pages; small
// pages give deep trees, overflow runs and sub-DBs in a few KiB) holding
// named databases, DupSort and DupFixed duplicates in sub-pages and
// sub-DBs, integer and reverse keys, overflow values and a free list;
// testdata/fuzz/FuzzEnvFile adds files written by C. Each input is opened
// read-only with NoLock and read with a bounded number of cursor
// operations (walks both ways, gets, duplicate and multiple-value
// operations on every named database), copied plainly and compacted, then
// opened for writing, changed in one transaction with a nested one and
// committed, and read again. Errors of any kind are fine; a panic, a
// hang (an input running longer than fuzzHangLimit while fuzzing) or
// runaway memory is a failure (AGENTS.md rule 4). The watchdog
// (internal/fuzzwatch) crashes the process with every goroutine's stack,
// so that the fuzzer records the input.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/fuzzwatch"
)

const (
	// fuzzHangLimit is how long one input may run while fuzzing; inputs
	// take milliseconds.
	fuzzHangLimit = 10 * time.Second
	// fuzzHeapLimit is how much one input may grow the heap.
	fuzzHeapLimit = 2 << 30
	// fuzzMaxFile bounds the inputs (the seeds are a few KiB).
	fuzzMaxFile = 1 << 20
	// fuzzReadBudget bounds the cursor operations of one read pass.
	fuzzReadBudget = 3000
)

// fuzzSeedShapes are the formats and page sizes of the seeds.
var fuzzSeedShapes = []struct {
	f  Format
	ps int
}{{V09, 2048}, {V10, 256}, {V10, 512}, {V10, 1024}}

// buildFuzzSeed writes a seed environment in dir and returns its data
// file. For LMDB 0.9, whose page size is the OS's for a new environment,
// the two meta pages of an empty environment with page size ps are
// written first: an existing environment keeps its page size.
func buildFuzzSeed(tb testing.TB, dir string, f Format, ps int) []byte {
	tb.Helper()
	path := filepath.Join(dir, "data.mdb")
	env, err := NewEnv()
	if err != nil {
		tb.Fatal(err)
	}
	defer env.Close()
	if f == V09 {
		l := format.LayoutV09
		m := format.NewEnvMeta(l, uint32(ps), 1<<20, 0)
		buf := make([]byte, 2*ps)
		m.WriteInitialPages(buf, ps, l)
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			tb.Fatal(err)
		}
	} else {
		must(tb, env.SetFormat(V10))
		must(tb, env.SetPageSize(ps))
	}
	must(tb, env.SetMaxDBs(16))
	must(tb, env.SetMapSize(16<<20))
	must(tb, env.Open(dir, NoLock|NoSync, 0o644))
	maxKey := min(env.MaxKeySize(), f.layout().NodeMax(ps)-(format.NodeSize+format.DBSize))
	key := func(prefix string, i, n int) []byte {
		k := []byte(fmt.Sprintf("%s%04d", prefix, i))
		for len(k) < n && len(k) < maxKey {
			k = append(k, 'k')
		}
		return k
	}
	u32 := func(v uint32) []byte { return binary.NativeEndian.AppendUint32(nil, v) }
	u64 := func(v uint64) []byte { return binary.NativeEndian.AppendUint64(nil, v) }
	for round := 0; round < 2; round++ {
		err := env.Update(func(txn *Txn) error {
			plain, err := txn.OpenDBI("plain", Create)
			if err != nil {
				return err
			}
			dups, err := txn.OpenDBI("dups", Create|DupSort)
			if err != nil {
				return err
			}
			fixed, err := txn.OpenDBI("fixed", Create|DupSort|DupFixed|IntegerDup)
			if err != nil {
				return err
			}
			rev, err := txn.OpenDBI("rev", Create|ReverseKey)
			if err != nil {
				return err
			}
			intkey, err := txn.OpenDBI("intkey", Create|IntegerKey)
			if err != nil {
				return err
			}
			root, err := txn.OpenRoot(0)
			if err != nil {
				return err
			}
			if round == 1 {
				// Deletes, so that the free list has records.
				for i := 0; i < 40; i += 3 {
					_ = txn.Del(root, key("m", i, 12), nil)
				}
				_ = txn.Del(plain, key("p", 1, 6), nil)
				_ = txn.Del(dups, []byte("b"), u32(7)) // not a match: NotFound
				return txn.Put(rev, []byte("zz"), []byte("last"), 0)
			}
			for i := 0; i < 40; i++ {
				if err := txn.Put(root, key("m", i, 12+i%20), bytes.Repeat([]byte{byte(i)}, 1+i%9), 0); err != nil {
					return err
				}
			}
			big := bytes.Repeat([]byte("O"), 2*ps+ps/2)
			for i := 0; i < 6; i++ {
				v := []byte(fmt.Sprintf("v%d", i))
				if i == 2 {
					v = big
				}
				if err := txn.Put(plain, key("p", i, 6), v, 0); err != nil {
					return err
				}
			}
			// "a": a sub-page; "b": enough duplicates for a sub-DB.
			for i := 0; i < 3; i++ {
				if err := txn.Put(dups, []byte("a"), []byte(fmt.Sprintf("dup%02d", i)), 0); err != nil {
					return err
				}
				if err := txn.Put(fixed, []byte("a"), u32(uint32(100+i)), 0); err != nil {
					return err
				}
			}
			for i := 0; i < 10+ps/12; i++ {
				if err := txn.Put(dups, []byte("b"), []byte(fmt.Sprintf("duplicate%04d", i)), 0); err != nil {
					return err
				}
			}
			for i := 0; i < ps/3; i++ {
				if err := txn.Put(fixed, []byte("b"), u32(uint32(i*7)), 0); err != nil {
					return err
				}
			}
			for i := 0; i < 8; i++ {
				if err := txn.Put(rev, key("r", i, 3), []byte("r"), 0); err != nil {
					return err
				}
				if err := txn.Put(intkey, u64(uint64(i)<<33|uint64(i)), []byte("i"), 0); err != nil {
					return err
				}
			}
			return txn.Put(root, []byte("big"), big, 0)
		})
		if err != nil {
			tb.Fatal(err)
		}
	}
	info, err := env.Info()
	if err != nil {
		tb.Fatal(err)
	}
	env.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	// The pages in use: on Windows a 0.9 file opened for writing is as long
	// as its map, the rest zeros.
	if used := (info.LastPNO + 1) * int64(ps); int64(len(data)) > used {
		data = data[:used]
	}
	return data
}

func (f Format) layout() *format.Layout { return format.Format(f).Layout() }

func must(tb testing.TB, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatal(err)
	}
}

// fuzzWatchdog runs fn, one fuzz input, under the watchdog: a crash with
// every goroutine's stack if it runs longer than fuzzHangLimit (while
// fuzzing) or grows the heap by more than fuzzHeapLimit.
func fuzzWatchdog(fn func()) { fuzzwatch.Run(fuzzHangLimit, fuzzHeapLimit, fn) }

func FuzzEnvFile(f *testing.F) {
	for _, s := range fuzzSeedShapes {
		f.Add(buildFuzzSeed(f, f.TempDir(), s.f, s.ps))
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { devnull.Close() })
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxFile {
			return
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "data.mdb"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		fuzzWatchdog(func() {
			names := fuzzReadEnv(dir, devnull, true)
			fuzzWriteEnv(dir, names, int64(crc32.ChecksumIEEE(data)))
			fuzzReadEnv(dir, nil, false)
		})
	})
}

// fuzzOpen opens dir with NoLock; nil if that fails.
func fuzzOpen(dir string, flags uint) *Env {
	env, err := NewEnv()
	if err != nil {
		return nil
	}
	if env.SetMaxDBs(16) != nil || env.SetMapSize(1<<20) != nil || env.Open(dir, NoLock|NoSync|flags, 0o644) != nil {
		env.Close()
		return nil
	}
	return env
}

// fuzzReadEnv reads the environment at dir with a bounded number of
// operations and copies it to out (if not nil); it returns the names of
// the databases it could open.
func fuzzReadEnv(dir string, out *os.File, copies bool) []string {
	env := fuzzOpen(dir, Readonly)
	if env == nil {
		return nil
	}
	defer env.Close()
	_, _ = env.Stat()
	_, _ = env.Info()
	_, _ = env.Flags()
	budget := fuzzReadBudget
	var names []string
	_ = env.View(func(txn *Txn) error {
		txn.RawRead = true
		root, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		keys := fuzzWalk(txn, root, &budget)
		for _, k := range keys {
			name := strings.TrimSuffix(string(k), "\x00")
			if name == "" || strings.IndexByte(name, 0) >= 0 || len(names) >= 12 {
				continue
			}
			dbi, err := txn.OpenDBI(name, 0)
			if err != nil {
				continue
			}
			names = append(names, name)
			_, _ = txn.Stat(dbi)
			_, _ = txn.Flags(dbi)
			fuzzWalk(txn, dbi, &budget)
		}
		return nil
	})
	if copies && out != nil {
		_ = env.CopyFDFlag(out.Fd(), 0)
		_ = env.CopyFDFlag(out.Fd(), CopyCompact)
	}
	return names
}

// fuzzWalk walks dbi with cursor operations of every kind until the
// budget is spent and returns the first keys it saw.
func fuzzWalk(txn *Txn, dbi DBI, budget *int) [][]byte {
	c, err := txn.OpenCursor(dbi)
	if err != nil {
		return nil
	}
	defer c.Close()
	var keys [][]byte
	for _, op := range []uint{Next, Prev, NextNoDup, PrevNoDup, NextDup, NextMultiple} {
		_, _, _ = c.Get(nil, nil, First)
		for i := 0; *budget > 0; i++ {
			*budget--
			k, v, err := c.Get(nil, nil, op)
			if err != nil {
				break
			}
			if op == Next && len(keys) < 64 {
				keys = append(keys, bytes.Clone(k))
			}
			if i%8 == 0 && len(k) > 0 {
				*budget -= 12
				_, _ = txn.Get(dbi, k)
				_, _ = c.Count()
				_, _, _ = c.Get(nil, nil, GetMultiple)
				_, _, _ = c.Get(nil, nil, LastDup)
				_, _, _ = c.Get(nil, nil, PrevDup)
				_, _, _ = c.Get(nil, nil, FirstDup)
				_, _, _ = c.Get(nil, nil, 18) // MDB_PREV_MULTIPLE
				_, _, _ = c.Get(k, v, GetBoth)
				_, _, _ = c.Get(k, v, GetBothRange)
				_, _, _ = c.Get(k, nil, SetKey)
				_, _, _ = c.Get(k, nil, SetRange)
				_, _, _ = c.Get(nil, nil, GetCurrent)
			}
		}
	}
	for op := uint(0); op <= 18 && *budget > 0; op++ {
		*budget--
		_, _, _ = c.Get([]byte("m0007"), []byte("dup01"), op)
	}
	return keys
}

// fuzzWriteEnv opens dir for writing and runs one write transaction of
// puts, deletes, cursor writes, a nested transaction and an empty-drop,
// chosen by seed, and commits it.
func fuzzWriteEnv(dir string, names []string, seed int64) {
	env := fuzzOpen(dir, 0)
	if env == nil {
		return
	}
	defer env.Close()
	r := rand.New(rand.NewSource(seed))
	_ = env.Update(func(txn *Txn) error {
		root, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		dbis := []DBI{root}
		for _, n := range names {
			if dbi, err := txn.OpenDBI(n, 0); err == nil {
				dbis = append(dbis, dbi)
			}
		}
		u32 := func(v uint32) []byte { return binary.NativeEndian.AppendUint32(nil, v) }
		for i := 0; i < 60; i++ {
			dbi := dbis[r.Intn(len(dbis))]
			k := []byte(fmt.Sprintf("m%04d", r.Intn(50)))
			switch r.Intn(8) {
			case 0, 1:
				_ = txn.Put(dbi, k, bytes.Repeat([]byte{'w'}, 1+r.Intn(1500)), 0)
			case 2:
				_ = txn.Put(dbi, k[:2], u32(r.Uint32()), NoDupData)
			case 3:
				_ = txn.Del(dbi, k, nil)
			case 4:
				if c, err := txn.OpenCursor(dbi); err == nil {
					if _, _, err := c.Get(k, nil, SetRange); err == nil {
						_ = c.Del(0)
						_ = c.Put(k, []byte("c"), Current)
					}
					c.Close()
				}
			case 5:
				_ = txn.Sub(func(child *Txn) error {
					_ = child.Put(dbi, k, []byte("child"), 0)
					_ = child.Del(dbi, []byte("m0001"), nil)
					if r.Intn(2) == 0 {
						return fmt.Errorf("abort")
					}
					return nil
				})
			case 6:
				if c, err := txn.OpenCursor(dbi); err == nil {
					_, _, _ = c.Get(nil, nil, Last)
					_ = c.Put([]byte("zzzz"), []byte("append"), Append)
					_ = c.Put(k, u32(0xffffffff), AppendDup)
					c.Close()
				}
			case 7:
				if r.Intn(8) == 0 && len(dbis) > 1 {
					_ = txn.Drop(dbis[1+r.Intn(len(dbis)-1)], false)
				} else {
					_, _ = txn.Get(dbi, k)
				}
			}
		}
		return nil
	})
}

// TestFuzzSeedsAreValid checks that the seed environments are what the
// fuzzer is meant to start from: small, readable, every database present.
func TestFuzzSeedsAreValid(t *testing.T) {
	for _, s := range fuzzSeedShapes {
		dir := t.TempDir()
		data := buildFuzzSeed(t, dir, s.f, s.ps)
		if len(data) > 64<<10 {
			t.Errorf("%v/%d: seed of %d bytes", s.f, s.ps, len(data))
		}
		// The main DB lists them in key order.
		if got := fuzzReadEnv(dir, nil, false); strings.Join(got, ",") != "dups,fixed,intkey,plain,rev" {
			t.Errorf("%v/%d: databases %v", s.f, s.ps, got)
		}
	}
}
