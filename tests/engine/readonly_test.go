package engine

// Read-only handles and the published snapshot (rlmdb a91d9f5,
// KNOWN_C_BUGS.md #45; robust_io_errors.rs
// robust_read_only_reader_skips_unpublished_first_commit).

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// The environment variables that make this test binary a read process
// (readHelper): the environment and its format.
const (
	readHelperSrc    = "ENGINE_TEST_READ_SRC"
	readHelperFormat = "ENGINE_TEST_READ_FORMAT"
)

// readHelper is the second process of readInAnotherProcess: a read
// transaction of a read-only handle; prints its txnid and the value of "key".
func readHelper(src string) int {
	f := format.V09
	if os.Getenv(readHelperFormat) == "1.0" {
		f = format.V10
	}
	env, err := eng.Open(src, eng.Options{Format: f, MaxDBs: 2, Flags: format.ReadOnly, OpenLock: openLockFile})
	if err != nil {
		fmt.Fprintln(os.Stderr, "read helper:", err)
		return 1
	}
	defer env.Close()
	r, err := env.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read helper:", err)
		return 1
	}
	defer r.Abort()
	v, err := r.Get(eng.MainDBI, []byte("key"))
	if err != nil && err != format.NotFound {
		fmt.Fprintln(os.Stderr, "read helper:", err)
		return 1
	}
	fmt.Printf("txnid=%d value=%q\n", r.ID(), v)
	return 0
}

// readInAnotherProcess is what a read transaction of a read-only handle in
// another process sees in src, as readHelper prints it.
func readInAnotherProcess(t *testing.T, src string, f format.Format) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), readHelperSrc+"="+src, readHelperFormat+"="+f.String())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("read process: %v (%s)", err, out)
	}
	return strings.TrimSpace(string(out))
}

// cDumpShows reports whether C's mdb_dump (a read transaction of a read-only
// handle) of src shows value.
func cDumpShows(t *testing.T, tool, src, value string) bool {
	t.Helper()
	_, out, _ := run(t, tool, src)
	want := hex.EncodeToString([]byte(value))
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

// TestReadOnlyReaderSkipsUnpublishedFirstCommit: the first commit of an
// environment, stopped after its meta page reached the file: mti_txnid is
// still 0. A read-only handle of another process reads the published snapshot
// (txnid 0, no data), not the newest meta page, which is the in-flight
// commit's; C's reads that one (rlmdb KNOWN_C_BUGS.md #45). The commit then
// fails (its sync), and the old txnid goes back into the page. With the
// commit's meta page put back, as C leaves it after a failed F_FULLFSYNC
// (#41), the writer lock is free and still no reader of ours takes the page.
func TestReadOnlyReaderSkipsUnpublishedFirstCommit(t *testing.T) {
	for _, c := range []struct {
		f    format.Format
		tool string
	}{{format.V09, "mdb_dump"}, {format.V10, "mdb_dump10"}} {
		t.Run(c.f.String(), func(t *testing.T) {
			tool(t, c.tool)
			src := t.TempDir()
			env, err := eng.Open(src, eng.Options{Format: c.f, MaxDBs: 2, OpenLock: openLockFile})
			must(t, err)
			defer env.Close()
			ps, l := env.PageSize(), layoutOf(c.f)
			data := filepath.Join(src, "data.mdb")
			published := `txnid=0 value=""`
			fileTxnids := func() [2]uint64 {
				t.Helper()
				b, err := os.ReadFile(data)
				must(t, err)
				var ids [2]uint64
				for i := range ids {
					m, ok := format.ReadMeta(b[i*ps+l.MetaOffset():])
					if !ok {
						t.Fatalf("meta page %d", i)
					}
					ids[i] = m.TxnID
				}
				return ids
			}

			txn, err := env.BeginTxn(nil, 0)
			must(t, err)
			must(t, txn.Put(eng.MainDBI, []byte("key"), []byte("uncommitted"), 0))
			var inFlight []byte
			var inFlightRead string
			var cSawIt bool
			env.AfterNextMetaWrite(func() {
				b, err := os.ReadFile(data)
				must(t, err)
				inFlight = b[ps : 2*ps]
				inFlightRead = readInAnotherProcess(t, src, c.f)
				cSawIt = cDumpShows(t, c.tool, src, "uncommitted")
			})
			env.FailNextMetaSync(syscall.EIO)
			if err := txn.Commit(); err == nil {
				t.Fatal("the commit succeeded")
			}
			m, ok := format.ReadMeta(inFlight[l.MetaOffset():])
			if !ok || m.TxnID != 1 {
				t.Fatalf("not in flight: %+v", m)
			}
			if inFlightRead != published {
				t.Fatalf("commit in flight: %s, want %s", inFlightRead, published)
			}
			// Not on OpenBSD: there C's map still holds the meta page as it
			// was before the write (the writer invalidates it only after the
			// sync, MDB_Invalidate), so C's fallback reads the published
			// snapshot too.
			if !cSawIt && runtime.GOOS != "openbsd" {
				t.Fatal("C's read-only fallback no longer reads the commit in flight")
			}
			if ids := fileTxnids(); ids != [2]uint64{0, 0} {
				t.Fatalf("after the failed commit: meta txnids %v", ids)
			}

			// C's leftover of such a failure: the commit's meta page.
			writeAsCommit(t, data, inFlight, ps)
			if ids := fileTxnids(); ids != [2]uint64{0, 1} {
				t.Fatalf("with the commit's meta page: meta txnids %v", ids)
			}
			if got := readInAnotherProcess(t, src, c.f); got != published {
				t.Fatalf("with the commit's meta page: %s, want %s", got, published)
			}
			if !cDumpShows(t, c.tool, src, "uncommitted") {
				t.Fatal("C's read-only fallback no longer reads the commit's meta page")
			}
		})
	}
}
