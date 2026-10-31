package engine

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// openTest opens a fresh environment in a temporary directory.
func openTest(t testing.TB, f format.Format, opts Options) (*Env, string) {
	t.Helper()
	dir := t.TempDir()
	opts.Format = f
	if opts.OpenLock == nil {
		opts.OpenLock = OpenMemLock
	}
	if opts.MapSize == 0 {
		opts.MapSize = 64 << 20
	}
	opts.Flags |= format.NoSync
	env, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Close() })
	return env, dir
}

func mustAudit(t testing.TB, env *Env) {
	t.Helper()
	if err := env.Audit(); err != nil {
		t.Fatal("audit: ", err)
	}
}

func scan(t testing.TB, env *Env, dbi DBI) [][2]string {
	t.Helper()
	txn, err := env.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Abort()
	c, err := txn.OpenCursor(dbi)
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for {
		k, v, err := c.Get(nil, nil, format.Next)
		if err == format.NotFound {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, [2]string{string(k), string(v)})
	}
	return out
}

func forFormats(t *testing.T, f func(t *testing.T, fm format.Format)) {
	for _, fm := range []format.Format{format.V09, format.V10} {
		t.Run(fm.String(), func(t *testing.T) { f(t, fm) })
	}
}

func TestEmptyEnv(t *testing.T) {
	forFormats(t, func(t *testing.T, fm format.Format) {
		env, _ := openTest(t, fm, Options{})
		st, err := env.Stat()
		if err != nil || st.Entries != 0 || st.Depth != 0 {
			t.Fatal(st, err)
		}
		txn, err := env.BeginTxn(nil, format.ReadOnly)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Get(MainDBI, []byte("x")); err != format.NotFound {
			t.Fatal(err)
		}
		txn.Abort()
		mustAudit(t, env)
	})
}

func TestModelPutDel(t *testing.T) {
	forFormats(t, func(t *testing.T, fm format.Format) {
		env, _ := openTest(t, fm, Options{})
		rng := rand.New(rand.NewSource(1))
		model := map[string]string{}
		for round := 0; round < 20; round++ {
			txn, err := env.BeginTxn(nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 300; i++ {
				k := fmt.Sprintf("key%05d", rng.Intn(3000))
				if rng.Intn(4) == 0 {
					err := txn.Del(MainDBI, []byte(k), nil)
					if _, ok := model[k]; ok != (err == nil) {
						t.Fatalf("del %s: %v (in model %v)", k, err, ok)
					}
					delete(model, k)
					continue
				}
				vlen := rng.Intn(200)
				if rng.Intn(20) == 0 {
					vlen = 5000 + rng.Intn(20000)
				}
				v := bytes.Repeat([]byte{byte('a' + rng.Intn(26))}, vlen)
				if err := txn.Put(MainDBI, []byte(k), v, 0); err != nil {
					t.Fatal(err)
				}
				model[k] = string(v)
			}
			// Read back inside the write transaction.
			for k, v := range model {
				got, err := txn.Get(MainDBI, []byte(k))
				if err != nil || string(got) != v {
					t.Fatalf("get %s in txn: %v", k, err)
				}
			}
			if err := txn.Commit(); err != nil {
				t.Fatal(err)
			}
			mustAudit(t, env)
		}
		got := scan(t, env, MainDBI)
		keys := make([]string, 0, len(model))
		for k := range model {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(got) != len(keys) {
			t.Fatalf("scan: %d items, model %d", len(got), len(keys))
		}
		for i, k := range keys {
			if got[i][0] != k || got[i][1] != model[k] {
				t.Fatalf("item %d: %q", i, got[i][0])
			}
		}
	})
}
