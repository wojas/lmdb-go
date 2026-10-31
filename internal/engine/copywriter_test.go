package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// TestCopyExcludesWriters: a plain copy keeps this process's writers out
// from taking its snapshot until it has copied the meta pages, with a lock
// file (mdb_env_copyfd0 holds the writer mutex) and without one (C takes
// no lock under MDB_NOLOCK; AGENTS.md rule 2a). A commit in between could
// use pages past the snapshot's end, which the copy leaves out, and write
// a meta page that names them.
func TestCopyExcludesWriters(t *testing.T) {
	forFormats(t, func(t *testing.T, fm format.Format) {
		for _, opts := range []Options{{Flags: format.NoLock}, {OpenLock: OpenMemLock}} {
			env, _ := openTest(t, fm, opts)
			putOne(t, env, "a", "x")
			ps := env.PageSize()
			wrote := false
			copySnapshotHook = func() {
				// A writer of this process, if one can begin now: it
				// commits a value of 20 pages.
				if !env.writerMu.TryLock() {
					return
				}
				env.writerMu.Unlock()
				putOne(t, env, "big", string(bytes.Repeat([]byte{'b'}, 20*ps)))
				wrote = true
			}
			var buf bytes.Buffer
			err := env.CopyTo(&buf, 0)
			copySnapshotHook = nil
			if err != nil {
				t.Fatal(err)
			}
			if wrote {
				t.Errorf("NoLock %v: a writer committed during the copy", opts.Flags&format.NoLock != 0)
			}
			checkCopy(t, buf.Bytes(), ps, env.layout)
		}
	})
}

// checkCopy checks that the newest meta page of the copy b names no page
// past its end, then that the copy opens, passes the audit and holds the
// key "a" (and not "big").
func checkCopy(t *testing.T, b []byte, ps int, l *format.Layout) {
	t.Helper()
	var metas [2]format.Meta
	for i := range metas {
		m, err := format.ReadMetaPage(b[i*ps:(i+1)*ps], l)
		if err != nil {
			t.Fatalf("meta %d: %v", i, err)
		}
		metas[i] = m
	}
	m := metas[format.PickNewest(&metas)]
	if end := (m.LastPgno + 1) * uint64(ps); end > uint64(len(b)) {
		t.Fatalf("newest meta (txn %d) ends at %d bytes, the copy at %d", m.TxnID, end, len(b))
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.mdb"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := Open(dir, Options{Flags: format.NoLock | format.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	mustAudit(t, env)
	got := scan(t, env, format.MainDBI)
	if len(got) != 1 || got[0] != [2]string{"a", "x"} {
		t.Fatalf("copy holds %d items: %.40q", len(got), got)
	}
}
