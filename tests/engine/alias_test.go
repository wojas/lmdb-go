package engine

// An environment under other names of its files, as a C process sees it
// (rlmdb 4762f54, rust_env.rs
// second_open_under_another_name_leaves_the_first_alone).

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// TestSecondOpenUnderAnotherNameLeavesTheFirstAlone: a process has an
// environment open with a live read transaction; its files are reachable
// under other names (hard links here; a bind mount looks the same). Opening
// those fails with EBUSY before the lock file is set up again, and without
// closing a descriptor of the lock file (which would drop the process's record
// locks on it). The first handle keeps its reader slot and its record locks:
// a C process attaches to the table instead of re-initialising it, and lists
// the reader. Its reader's value and its next writer's txnid stay as they
// were. Both files under another name, then the lock file alone.
func TestSecondOpenUnderAnotherNameLeavesTheFirstAlone(t *testing.T) {
	for _, c := range []struct {
		f    format.Format
		tool string
	}{{format.V09, "mdb_stat"}, {format.V10, "mdb_stat10"}} {
		t.Run(c.f.String(), func(t *testing.T) {
			tool(t, c.tool)
			dir := t.TempDir()
			var names [3]string
			for i, n := range []string{"a", "b", "c"} {
				names[i] = filepath.Join(dir, n)
				must(t, os.Mkdir(names[i], 0o755))
			}
			a, b, cc := names[0], names[1], names[2]
			open := func(path string) (*eng.Env, error) {
				return eng.Open(path, eng.Options{Format: c.f, MapSize: 1 << 20, Flags: format.NoSync, OpenLock: openLockFile})
			}
			env, err := open(a)
			must(t, err)
			defer env.Close()
			before := bytes.Repeat([]byte{11}, 32000)
			txn, err := env.BeginTxn(nil, 0)
			must(t, err)
			must(t, txn.Put(eng.MainDBI, []byte("key"), before, 0))
			must(t, txn.Commit())
			reader, err := env.BeginTxn(nil, format.ReadOnly)
			must(t, err)
			defer reader.Abort()
			held, err := reader.Get(eng.MainDBI, []byte("key"))
			must(t, err)
			cSeesTheReader := func(what string) {
				t.Helper()
				// mdb_stat -r exits 1 on success (rlmdb KNOWN_C_BUGS.md row 2).
				_, out, errs := run(t, c.tool, "-r", a)
				for _, l := range strings.Split(out, "\n") {
					f := strings.Fields(l)
					if len(f) == 3 && f[0] == fmt.Sprint(os.Getpid()) && f[2] == "1" {
						return
					}
				}
				t.Fatalf("%s: C's reader table:\n%s\n%s", what, out, errs)
			}
			cSeesTheReader("first")

			if err := os.Link(filepath.Join(a, "data.mdb"), filepath.Join(b, "data.mdb")); err != nil {
				t.Skipf("the file system refuses hard links: %v", err)
			}
			must(t, os.Link(filepath.Join(a, "lock.mdb"), filepath.Join(b, "lock.mdb")))
			must(t, os.Link(filepath.Join(a, "lock.mdb"), filepath.Join(cc, "lock.mdb")))
			for _, alias := range []string{b, cc} {
				if e, err := open(alias); err != syscall.EBUSY {
					if err == nil {
						e.Close()
					}
					t.Fatalf("%s: %v, want EBUSY", alias, err)
				}
				list := env.ReaderList()
				if len(list) != 1 || int(list[0].PID) != os.Getpid() || !list[0].InTxn || list[0].TxnID != 1 {
					t.Fatalf("%s: the reader table: %+v", alias, list)
				}
				cSeesTheReader(alias)
				if !bytes.Equal(held, before) {
					t.Fatalf("%s: the held value changed", alias)
				}
			}
			if _, err := os.Stat(filepath.Join(cc, "data.mdb")); !os.IsNotExist(err) {
				t.Fatalf("the refused open created the data file: %v", err)
			}

			txn, err = env.BeginTxn(nil, 0)
			must(t, err)
			if txn.ID() != 2 {
				t.Fatalf("next txnid %d", txn.ID())
			}
			must(t, txn.Put(eng.MainDBI, []byte("key"), bytes.Repeat([]byte{22}, 32000), 0))
			must(t, txn.Commit())
			if !bytes.Equal(held, before) {
				t.Fatal("the held value changed")
			}
		})
	}
}
