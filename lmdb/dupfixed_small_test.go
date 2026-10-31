package lmdb

// DupFixed items of 1, 2 and 3 bytes through the API (rlmdb
// rlmdb-capi/tests/c/capi_write.c test_dupfixed_small). A P_LEAF2 page or
// sub-page counts its items in mp_lower/mp_upper, mp_upper dropping by
// ksize - sizeof(indx_t) per item: unsigned in C's mdb_node_add, so for
// 1-byte items mp_upper grows past the end of the page. The 2- and 3-byte
// databases guard the neighbours. That the files are C's, byte for byte,
// is the write differential's job (tests/engine).

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDupFixed_smallItems(t *testing.T) {
	for _, c := range []struct {
		name   string
		format Format
		psize  int
	}{
		{"0.9", V09, 0},
		// At 256-byte pages the 256 1-byte values of a key need a sub-DB,
		// whose P_LEAF2 pages split.
		{"1.0-256", V10, 256},
		{"1.0-4096", V10, 4096},
	} {
		t.Run(c.name, func(t *testing.T) {
			env, err := NewEnv()
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			if err := env.SetFormat(c.format); err != nil {
				t.Fatal(err)
			}
			if c.psize != 0 {
				if err := env.SetPageSize(c.psize); err != nil {
					t.Fatal(err)
				}
			}
			if err := env.SetMaxDBs(8); err != nil {
				t.Fatal(err)
			}
			if err := env.SetMapSize(4 << 20); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := env.Open(dir, testFlags(0), 0o644); err != nil {
				t.Fatal(err)
			}
			for size := 1; size <= 3; size++ {
				if err := env.Update(func(txn *Txn) error { return fillSmall(t, txn, size) }); err != nil {
					t.Fatalf("%d-byte items: %v", size, err)
				}
			}
			checkSmall(t, env)
			// The compacting copy writes the P_LEAF2 pages as C's
			// mdb_page_copy does; the copy holds the same items.
			cp := filepath.Join(t.TempDir(), "copy")
			if err := os.Mkdir(cp, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := env.CopyFlag(cp, CopyCompact); err != nil {
				t.Fatal(err)
			}
			cenv, err := NewEnv()
			if err != nil {
				t.Fatal(err)
			}
			defer cenv.Close()
			if err := cenv.SetMaxDBs(8); err != nil {
				t.Fatal(err)
			}
			if err := cenv.Open(cp, testFlags(Readonly), 0o644); err != nil {
				t.Fatal(err)
			}
			checkSmall(t, cenv)
		})
	}
}

// fillSmall writes database fixed<size>: key "a" gets 200 single puts in a
// scrambled order (167 is odd, so i*167 mod 256 visits every byte once),
// key "b" all 256 first bytes with one PutMulti, then every third value of
// "a" goes again.
func fillSmall(t *testing.T, txn *Txn, size int) error {
	db, err := txn.OpenDBI(fmt.Sprintf("fixed%d", size), Create|DupSort|DupFixed)
	if err != nil {
		return err
	}
	v := bytes.Repeat([]byte("x"), size)
	for i := 0; i < 200; i++ {
		v[0] = byte(i * 167)
		if err := txn.Put(db, []byte("a"), v, 0); err != nil {
			return err
		}
	}
	items := bytes.Repeat([]byte("y"), 256*size)
	for i := 0; i < 256; i++ {
		items[i*size] = byte(255 - i)
	}
	c, err := txn.OpenCursor(db)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.PutMulti([]byte("b"), items, size, 0); err != nil {
		return err
	}
	if n, err := c.Count(); err != nil || n != 256 {
		t.Errorf("fixed%d: b has %d values (%v), want 256", size, n, err)
	}
	for i := 0; i < 200; i += 3 {
		v[0] = byte(i * 167)
		if err := txn.Del(db, []byte("a"), v); err != nil {
			return err
		}
	}
	return nil
}

// checkSmall reads the databases fillSmall wrote: the values of "a" are
// sorted, one item each, and MDB_GET_MULTIPLE/MDB_NEXT_MULTIPLE return
// whole items of "b", 256 of them.
func checkSmall(t *testing.T, env *Env) {
	t.Helper()
	err := env.View(func(txn *Txn) error {
		for size := 1; size <= 3; size++ {
			db, err := txn.OpenDBI(fmt.Sprintf("fixed%d", size), 0)
			if err != nil {
				return err
			}
			c, err := txn.OpenCursor(db)
			if err != nil {
				return err
			}
			n, prev := 0, -1
			_, d, err := c.Get([]byte("a"), nil, Set)
			for ; err == nil; _, d, err = c.Get(nil, nil, NextDup) {
				if len(d) != size || int(d[0]) <= prev {
					t.Errorf("fixed%d: a: value %x after %d", size, d, prev)
				}
				prev = int(d[0])
				n++
			}
			if !IsNotFound(err) {
				return err
			}
			if want := 200 - 67; n != want {
				t.Errorf("fixed%d: a has %d values, want %d", size, n, want)
			}
			if _, _, err := c.Get([]byte("b"), nil, Set); err != nil {
				return err
			}
			total := 0
			_, d, err = c.Get(nil, nil, GetMultiple)
			for ; err == nil; _, d, err = c.Get(nil, nil, NextMultiple) {
				if len(d)%size != 0 {
					t.Errorf("fixed%d: multiple of %d bytes", size, len(d))
				}
				total += len(d)
			}
			if !IsNotFound(err) {
				return err
			}
			if total != 256*size {
				t.Errorf("fixed%d: b: %d bytes, want %d", size, total, 256*size)
			}
			c.Close()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
