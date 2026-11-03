package engine

import (
	"fmt"
	"strings"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// goDump lists the items of database db (nil: the main DB) of the
// environment at dir as mdb_dump's data lines (" <hex key>", " <hex
// value>"), read with the Go engine, forwards; it also checks that a
// backward scan agrees, and with lookups that point lookups do (the
// differential scripts' databases too: their MDB_CURRENT puts with another
// key give C the cursor's key, KNOWN_C_BUGS.md row 58, so neither side
// leaves a key out of order).
func goDump(t testing.TB, dir string, db []byte, lookups bool) []string {
	t.Helper()
	env, err := eng.Open(dir, eng.Options{Flags: format.NoLock | format.ReadOnly, MaxDBs: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	if err := env.Audit(); err != nil {
		t.Fatalf("Go audit of %s: %v", dir, err)
	}
	txn, err := env.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Abort()
	dbi, err := txn.OpenDBI(db, 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := txn.OpenCursor(dbi)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var items [][2]string
	for {
		k, v, err := c.Get(nil, nil, format.Next)
		if err == format.NotFound {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, " "+hexs(k), " "+hexs(v))
		items = append(items, [2]string{string(k), string(v)})
	}
	// Backwards.
	c2, _ := txn.OpenCursor(dbi)
	for i := len(items) - 1; i >= 0; i-- {
		k, v, err := c2.Get(nil, nil, format.Prev)
		if err != nil || string(k) != items[i][0] || string(v) != items[i][1] {
			t.Fatalf("backward scan item %d: %v", i, err)
		}
	}
	if _, _, err := c2.Get(nil, nil, format.Prev); err != format.NotFound {
		t.Fatalf("backward scan past the start: %v", err)
	}
	// Point lookups (the first value of each key).
	flags, _ := txn.DBIFlags(dbi)
	for i, it := range items {
		if !lookups {
			break
		}
		if i > 0 && items[i-1][0] == it[0] {
			continue
		}
		v, err := txn.Get(dbi, []byte(it[0]))
		if err != nil || string(v) != it[1] {
			t.Fatalf("get %x (item %d of %d): %v len %d want %d", it[0], i, len(items), err, len(v), len(it[1]))
		}
		if flags&format.DupSort != 0 {
			if _, v, err := c.Get([]byte(it[0]), []byte(it[1]), format.GetBoth); err != nil || string(v) != it[1] {
				t.Fatalf("get both %x: %v", it[0], err)
			}
		}
	}
	return out
}

// cDump is the data lines of mdb_dump -s db (or of the main DB).
func cDump(t testing.TB, dumpTool, dir string, db string) []string {
	args := []string{dir}
	if db != "" {
		args = []string{"-s", db, dir}
	}
	rc, out, errs := run(t, dumpTool, args...)
	if rc != 0 {
		t.Fatalf("%s: %s", dumpTool, errs)
	}
	_, data, _ := strings.Cut(out, "HEADER=END\n")
	data, _, _ = strings.Cut(data, "DATA=END")
	return strings.Split(strings.TrimRight(data, "\n"), "\n")
}

// TestGoReadsC: data sets written by C (cpeer fill, 0.9 and 1.0 at several
// page sizes) read with the Go engine give mdb_dump's items, in both
// directions and by lookup, and pass the Go page audit.
func TestGoReadsC(t *testing.T) {
	sets := []struct {
		name string
		opts []string
	}{
		{"plain", nil},
		{"big", []string{"--big", "20000", "--valsize", "100"}},
		{"intkey", []string{"--intkey"}},
		{"revkey", []string{"--revkey"}},
		{"dupsort", []string{"--dupsort", "--dups", "40"}},
		{"revdup", []string{"--revdup", "--dups", "40"}},
		{"dupfixed", []string{"--dupfixed", "--dups", "300"}},
		{"intdup", []string{"--intdup", "--dups", "300"}},
		{"deleted", []string{"--dupsort", "--dups", "20", "--deleted-every", "3"}},
	}
	type peer struct {
		peer, dump string
		pagesize   int
	}
	peers := []peer{{"cpeer", "mdb_dump", 0}, {"cpeer10", "mdb_dump10", 1024}, {"cpeer10", "mdb_dump10", 4096}, {"cpeer10", "mdb_dump10", 65536}}
	for _, p := range peers {
		for _, s := range sets {
			t.Run(fmt.Sprintf("%s-%d/%s", p.peer, p.pagesize, s.name), func(t *testing.T) {
				dir := t.TempDir()
				args := []string{"fill", dir, "data", "3000", "7", "--mapsize", "268435456"}
				if p.pagesize != 0 {
					args = append(args, "--pagesize", fmt.Sprint(p.pagesize))
				}
				args = append(args, "--per-txn", "700")
				args = append(args, s.opts...)
				if s.name == "deleted" {
					// The deleted set: fill, then delete.
					fill := append([]string(nil), args...)
					fill = removeOpt(fill, "--deleted-every", 1)
					if rc, _, errs := run(t, p.peer, fill...); rc != 0 {
						t.Fatal(errs)
					}
					del := []string{"delete", dir, "data", "3000", "7", "--every", "3", "--dupsort", "--dups", "20", "--mapsize", "268435456"}
					if rc, _, errs := run(t, p.peer, del...); rc != 0 {
						t.Fatal(errs)
					}
				} else if rc, _, errs := run(t, p.peer, args...); rc != 0 {
					t.Fatal(errs)
				}
				want := cDump(t, p.dump, dir, "data")
				got := goDump(t, dir, []byte("data"), true)
				if strings.Join(want, "\n") != strings.Join(got, "\n") {
					t.Fatalf("Go read %d lines, mdb_dump %d", len(got), len(want))
				}
			})
		}
	}
}

func removeOpt(args []string, opt string, n int) []string {
	for i, a := range args {
		if a == opt {
			return append(args[:i:i], args[i+1+n:]...)
		}
	}
	return args
}
