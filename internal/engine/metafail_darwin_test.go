package engine

// The real F_FULLFSYNC after the meta page write fails (macOS only: the
// other systems have no separate sync there). rlmdb
// robust_failed_full_fsync_is_fatal.

import (
	"os"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// TestFailedFullFsyncIsFatal: the meta descriptor (O_DSYNC) is replaced
// by a write-only /dev/null, which takes the write and refuses the
// F_FULLFSYNC after it (ENODEV). The commit fails, the environment is
// fatal, and a re-open sees the last good snapshot and writes on. (The new
// page goes nowhere here, so the file holds the old txnid either way;
// TestMetaWriteFailureIsFatal covers the page reaching the file.)
func TestFailedFullFsyncIsFatal(t *testing.T) {
	null, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	must(t, err)
	defer null.Close()
	if sys.Fdatasync(null) == nil {
		t.Skip("F_FULLFSYNC works on /dev/null here")
	}
	for _, f := range []format.Format{format.V09, format.V10} {
		dir := t.TempDir()
		open := func() *Env {
			env, err := Open(dir, Options{Format: f, MapSize: 64 << 20, OpenLock: OpenMemLock})
			must(t, err)
			return env
		}
		env := open()
		fillKeys(t, env, 0, 300)
		txnid := snapshotID(t, env)
		w := beginW(t, env)
		must(t, w.Put(MainDBI, []byte("lost"), []byte("value"), 0))
		fd := int(env.metaFile.Fd())
		saved, err := syscall.Dup(fd)
		must(t, err)
		must(t, syscall.Dup2(int(null.Fd()), fd))
		err = w.Commit()
		must(t, syscall.Dup2(saved, fd))
		syscall.Close(saved)
		if err == nil {
			t.Fatalf("%v: the commit succeeded", f)
		}
		if err := env.BeginRead(new(Txn)); err != format.Panic {
			t.Fatalf("%v: read transaction: %v, want MDB_PANIC", f, err)
		}
		if _, err := env.BeginTxn(nil, 0); err != format.Panic {
			t.Fatalf("%v: write transaction: %v, want MDB_PANIC", f, err)
		}
		env.Close()
		env = open()
		if id := snapshotID(t, env); id != txnid {
			t.Fatalf("%v: after the re-open: txnid %d, want %d", f, id, txnid)
		}
		keys := make([]int, 0, 310)
		for i := 0; i < 310; i++ {
			keys = append(keys, i)
		}
		assertKeys(t, env, keys[:300])
		fillKeys(t, env, 300, 10)
		assertKeys(t, env, keys)
		mustAudit(t, env)
		env.Close()
	}
}
