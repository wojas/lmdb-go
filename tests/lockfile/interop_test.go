// Cross-process tests of the Go lock file against C LMDB 0.9 and 1.0
// processes, each scenario for both data formats (the C peer of that
// format on that format's lock file).

package lockfiletest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/lockfile"
)

// noRepair is a LockWriter callback for scenarios in which no writer dies.
func noRepair() (uint64, error) { return 0, errors.New("unexpected dead owner") }

// TestLayoutMatchesC compares the Go layouts with layout_dump's and
// layout_dump10's (the C builds' sizeof/offsetof).
func TestLayoutMatchesC(t *testing.T) {
	for _, c := range sides(t) {
		vals := map[string]string{}
		for _, line := range strings.Split(mustRun(t, c.ldump), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				vals[k] = v
			}
		}
		num := func(keys ...string) int {
			for _, k := range keys {
				if v, ok := vals[k]; ok {
					n, err := strconv.ParseInt(v, 0, 64)
					if err != nil {
						t.Fatal(err)
					}
					return int(n)
				}
			}
			return -1
		}
		l := c.layout
		rlock := []string{"mtb_rmutex", "mtb_rmname", "mtb_mutexid"}
		for i, f := range rlock {
			rlock[i] = "offsetof(MDB_txninfo.mt1.mtb." + f + ")"
		}
		rsize := make([]string, len(rlock))
		for i, k := range rlock {
			rsize[i] = "sizeof" + strings.TrimPrefix(k, "offsetof")
		}
		for _, chk := range []struct {
			name      string
			got, want int
		}{
			{"MDB_LOCK_FORMAT", int(l.LockFormat), num("MDB_LOCK_FORMAT")},
			{"mti_txnid", l.TxnID, num("offsetof(MDB_txninfo.mt1.mtb.mtb_txnid)")},
			{"mti_numreaders", l.NumReaders, num("offsetof(MDB_txninfo.mt1.mtb.mtb_numreaders)")},
			{"reader lock", l.RLock, num(rlock...)},
			{"reader lock size", l.RLockSize, num(rsize...)},
			{"writer lock", l.WLock, num("offsetof(MDB_txninfo.mt2.mt2_wmutex)", "offsetof(MDB_txninfo.mt2.mt2_wmname)")},
			{"sizeof(MDB_txbody)", l.TxbodySize, num("sizeof(MDB_txbody)")},
			{"mt1", l.HeaderSize, num("sizeof(MDB_txninfo.mt1)")},
			{"mti_readers", l.Readers, num("offsetof(MDB_txninfo.mti_readers)")},
			{"sizeof(MDB_reader)", lockfile.ReaderSize, num("sizeof(MDB_reader)")},
			{"mr_pid", lockfile.ReaderPID, num("offsetof(MDB_reader.mru.mrx.mrb_pid)")},
			{"mr_tid", lockfile.ReaderTID, num("offsetof(MDB_reader.mru.mrx.mrb_tid)")},
		} {
			if chk.got != chk.want {
				t.Errorf("%s %s: Go %d (%#x), C %d (%#x)", c.format, chk.name, chk.got, chk.got, chk.want, chk.want)
			}
		}
		t.Logf("%s: %s, mti_format %#x, readers at %d", c.format, vals["lock_mode"], l.LockFormat, l.Readers)
	}
}

// TestCTableGoAttaches: C initialises the table, Go attaches; readers of
// each side are visible to the other (Region.ReaderList, cpeer readers,
// mdb_stat -r).
func TestCTableGoAttaches(t *testing.T) {
	for _, c := range sides(t) {
		env, txnid := c.newEnv(t)
		cr := startC(t, c.peer, "hold-reader", env, holdSecs, "--until-stdin").mustBeReady(t)
		r := openGo(t, env, c.layout)
		if r.IsExclusive() {
			t.Fatalf("%s: Go is the first opener while C holds the environment", c.format)
		}
		if r.TxnID() != txnid {
			t.Fatalf("%s: mti_txnid %d, C says %d", c.format, r.TxnID(), txnid)
		}
		found := false
		for _, ri := range r.ReaderList() {
			if int(ri.PID) == cr.pid() && ri.InTxn && ri.TxnID == txnid {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: C's reader %d not in %+v", c.format, cr.pid(), r.ReaderList())
		}
		slot, err := r.AcquireSlot()
		if err != nil {
			t.Fatal(err)
		}
		if got := r.PublishCurrentTxnID(slot); got != txnid {
			t.Fatalf("%s: published %d", c.format, got)
		}
		me := os.Getpid()
		if got := readersInC(t, c.peer, env); got[me] != strconv.FormatUint(txnid, 10) || got[cr.pid()] == "" {
			t.Fatalf("%s: cpeer readers %v, want Go %d at %d and C %d", c.format, got, me, txnid, cr.pid())
		}
		if got := readersInMdbStat(t, c.stat, env); !got[me] || !got[cr.pid()] {
			t.Fatalf("%s: mdb_stat -r %v", c.format, got)
		}
		r.ResetSlot(slot)
		if got := readersInC(t, c.peer, env); got[me] != "-" {
			t.Fatalf("%s: idle Go slot in C: %v", c.format, got)
		}
		cr.release(t)
		r.Close()
		if got := readersInC(t, c.peer, env); len(got) != 0 {
			t.Fatalf("%s: readers left: %v", c.format, got)
		}
	}
}

// TestGoTableCAttaches: Go initialises the table (the lock file is new),
// C writes and reads through it while Go holds the environment.
func TestGoTableCAttaches(t *testing.T) {
	for _, c := range sides(t) {
		env, txnid := c.newEnv(t)
		if err := os.Remove(filepath.Join(env, "lock.mdb")); err != nil {
			t.Fatal(err)
		}
		r := openGo(t, env, c.layout)
		if !r.IsExclusive() {
			t.Fatalf("%s: not the first opener", c.format)
		}
		if err := r.Downgrade(txnid); err != nil {
			t.Fatal(err)
		}
		slot, _ := r.AcquireSlot()
		r.PublishCurrentTxnID(slot)
		// C uses the writer lock and the reader table Go set up.
		mustRun(t, c.peer, "fill", env, "db2", "40", "2", "--per-txn", "10")
		mustRun(t, c.peer, "verify", env, "db2", "40", "2")
		if got, want := r.TxnID(), txnid+4; got != want {
			t.Fatalf("%s: mti_txnid after 4 C commits: %d, want %d", c.format, got, want)
		}
		if got := readersInMdbStat(t, c.stat, env); !got[os.Getpid()] {
			t.Fatalf("%s: Go reader not in mdb_stat -r: %v", c.format, got)
		}
		// The Go reader still pins its snapshot.
		if r.FindOldest(r.TxnID()+1) != txnid {
			t.Fatalf("%s: oldest %d", c.format, r.FindOldest(r.TxnID()+1))
		}
		r.Close()
	}
}

// TestInitBytesMatchC compares a lock file Go initialised with the one C
// initialises in its place (same file, so the same inode for the macOS
// names): equal bytes, except for LMDB 0.9's macOS semaphore names, which
// C derives from uninitialised memory.
func TestInitBytesMatchC(t *testing.T) {
	for _, c := range sides(t) {
		env := filepath.Join(t.TempDir(), "env")
		os.Mkdir(env, 0o755)
		mustRun(t, c.peer, "create", env, "67108864", "4")
		lock := filepath.Join(env, "lock.mdb")
		os.Remove(lock)
		r := openGo(t, env, c.layout) // 126 slots, C's default
		if err := r.Downgrade(0); err != nil {
			t.Fatal(err)
		}
		r.Close()
		goBytes, err := os.ReadFile(lock)
		if err != nil {
			t.Fatal(err)
		}
		// cpeer readers opens read-only, alone: it re-initialises the
		// table, publishes the meta's txnid (0) and lists the readers.
		mustRun(t, c.peer, "readers", env)
		cBytes, err := os.ReadFile(lock)
		if err != nil {
			t.Fatal(err)
		}
		if len(goBytes) != len(cBytes) {
			t.Fatalf("%s: sizes %d, %d", c.format, len(goBytes), len(cBytes))
		}
		l := c.layout
		if l.Objects == lockfile.SemaphoreNames {
			gh, _ := lockfile.ReadHeader(goBytes, l)
			ch, _ := lockfile.ReadHeader(cBytes, l)
			t.Logf("%s: semaphore names Go %q %q, C %q %q", c.format, gh.RName, gh.WName, ch.RName, ch.WName)
			for _, n := range [][]byte{ch.RName, ch.WName} {
				if len(n) != 15 || !bytes.HasPrefix(n, []byte("/MDB")) {
					t.Fatalf("%s: C name %q", c.format, n)
				}
			}
			for _, off := range []int{l.RLock, l.WLock} {
				for i := off; i < off+l.RLockSize; i++ {
					goBytes[i], cBytes[i] = 0, 0
				}
			}
		}
		if !bytes.Equal(goBytes, cBytes) {
			for i := range goBytes {
				if goBytes[i] != cBytes[i] {
					t.Fatalf("%s: lock files differ at %d: Go % x, C % x", c.format, i,
						goBytes[i:min(i+16, len(goBytes))], cBytes[i:min(i+16, len(cBytes))])
				}
			}
		}
	}
}

// TestWritersExcludeEachOther: a C writer blocks a Go writer and the
// reverse.
func TestWritersExcludeEachOther(t *testing.T) {
	for _, c := range sides(t) {
		env, _ := c.newEnv(t)
		holder := startC(t, c.peer, "hold-env", env, holdSecs, "--until-stdin").mustBeReady(t)
		r := openGo(t, env, c.layout)

		// C holds the writer lock, Go waits.
		cw := startC(t, c.peer, "hold-writer", env, holdSecs, "--until-stdin").mustBeReady(t)
		got := make(chan error, 1)
		go func() {
			err := r.LockWriter(noRepair)
			if err == nil {
				err = r.UnlockWriter()
			}
			got <- err
		}()
		select {
		case err := <-got:
			t.Fatalf("%s: Go got the writer lock while C holds it (%v)", c.format, err)
		case <-time.After(500 * time.Millisecond):
		}
		cw.release(t)
		select {
		case err := <-got:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: Go did not get the writer lock after C released it", c.format)
		}

		// Go holds the writer lock, a C writer waits.
		if err := r.LockWriter(noRepair); err != nil {
			t.Fatal(err)
		}
		fill := startC(t, c.peer, "fill", env, "db3", "10", "3")
		if fill.exited(500 * time.Millisecond) {
			t.Fatalf("%s: C committed while Go holds the writer lock", c.format)
		}
		if err := r.UnlockWriter(); err != nil {
			t.Fatal(err)
		}
		if !fill.exited(30 * time.Second) {
			t.Fatalf("%s: C writer still blocked", c.format)
		}
		if fill.err != nil {
			t.Fatalf("%s: C fill: %v", c.format, fill.err)
		}
		mustRun(t, c.peer, "verify", env, "db3", "10", "3")
		r.Close()
		holder.release(t)
	}
}

// TestReaderCheckAcrossImplementations: C's reader check clears a dead Go
// process's slot, and Go's a dead C process's.
func TestReaderCheckAcrossImplementations(t *testing.T) {
	for _, c := range sides(t) {
		env, txnid := c.newEnv(t)
		// The test keeps the environment open, so that no opener is
		// exclusive (which would reset the table, dead slots and all).
		r := openGo(t, env, c.layout)
		if r.IsExclusive() {
			r.Downgrade(txnid)
		}

		g := startGo(t, "hold-reader", env, c.layout).mustBeReady(t)
		if got := readersInC(t, c.peer, env); got[g.pid()] != strconv.FormatUint(txnid, 10) {
			t.Fatalf("%s: Go helper's reader not seen by C: %v", c.format, got)
		}
		if got := field(t, mustRun(t, c.peer, "reader-check", env), "cleared"); got != "0" {
			t.Fatalf("%s: C cleared a live Go reader: %s", c.format, got)
		}
		g.kill()
		if got := field(t, mustRun(t, c.peer, "reader-check", env), "cleared"); got != "1" {
			t.Fatalf("%s: C reader check cleared %s, want 1", c.format, got)
		}
		if got := readersInC(t, c.peer, env); len(got) != 0 {
			t.Fatalf("%s: readers left: %v", c.format, got)
		}

		cr := startC(t, c.peer, "hold-reader", env, holdSecs, "--until-stdin").mustBeReady(t)
		if n, err := r.ReaderCheck(); n != 0 || err != nil {
			t.Fatalf("%s: Go cleared a live C reader: %d %v", c.format, n, err)
		}
		cr.kill()
		if n, err := r.ReaderCheck(); n != 1 || err != nil {
			t.Fatalf("%s: Go reader check: %d %v, want 1", c.format, n, err)
		}
		if len(r.ReaderList()) != 0 {
			t.Fatalf("%s: readers left: %+v", c.format, r.ReaderList())
		}
		r.Close()
	}
}

// TestWriterKilledHoldingLock: a C writer killed while it holds the writer
// mutex is recovered from by Go, and a Go writer killed while it holds it
// by C (robust mutexes, EOWNERDEAD and mdb_mutex_failed; Linux).
func TestWriterKilledHoldingLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("robust mutexes are Linux's; semaphores stay taken when the holder dies, in C too")
	}
	if os.Getenv("LMDB_TEST_NO_ROBUST_MUTEX") != "" {
		// QEMU user emulation has no robust futexes (in C too): the dead
		// owner is never reported (scripts/linux-test.sh).
		t.Skip("no robust mutexes here (LMDB_TEST_NO_ROBUST_MUTEX)")
	}
	for _, c := range sides(t) {
		env, txnid := c.newEnv(t)
		r := openGo(t, env, c.layout)
		if r.IsExclusive() {
			r.Downgrade(txnid)
		}

		// C dies holding the writer mutex; Go repairs.
		startC(t, c.peer, "hold-writer", env, holdSecs, "--until-stdin").mustBeReady(t).kill()
		called := false
		err := r.LockWriter(func() (uint64, error) { called = true; return txnid, nil })
		if err != nil || !called {
			t.Fatalf("%s: Go after dead C writer: %v, repair called %v", c.format, err, called)
		}
		if err := r.UnlockWriter(); err != nil {
			t.Fatal(err)
		}
		mustRun(t, c.peer, "fill", env, "db4", "10", "4")
		mustRun(t, c.peer, "verify", env, "db4", "10", "4")

		// Go dies holding the writer mutex; C repairs.
		startGo(t, "hold-writer", env, c.layout).mustBeReady(t).kill()
		mustRun(t, c.peer, "fill", env, "db5", "10", "5")
		mustRun(t, c.peer, "verify", env, "db5", "10", "5")
		// C marked it consistent: Go takes it normally.
		if err := r.LockWriter(noRepair); err != nil {
			t.Fatalf("%s: Go after C's repair: %v", c.format, err)
		}
		r.UnlockWriter()
		r.Close()
	}
}

// TestOtherVersionHeld: a lock file of the other version, held open by
// someone, is MDB_VERSION_MISMATCH for Go and for C; a closed one is
// re-initialised by the first opener.
func TestOtherVersionHeld(t *testing.T) {
	for _, c := range sides(t) {
		env, txnid := c.newEnv(t)
		holder := startC(t, c.peer, "hold-env", env, holdSecs, "--until-stdin").mustBeReady(t)
		if _, err := lockfile.Open(filepath.Join(env, "lock.mdb"), 0o644, 126, false, c.otherLayout); err != format.VersionMismatch {
			t.Fatalf("%s held by C, Go opens v%d: %v", c.format, c.otherLayout.Version, err)
		}
		holder.release(t)

		// Go holds a table of the other version; C refuses it.
		r := openGo(t, env, c.otherLayout)
		r.Downgrade(0)
		out, code := run(t, c.peer, "readers", env)
		if code == 0 || !strings.Contains(out, "MDB_VERSION_MISMATCH") {
			t.Fatalf("%s: C on a held v%d table: exit %d: %s", c.format, c.otherLayout.Version, code, out)
		}
		r.Close()

		// Closed: the first opener re-initialises in its own version.
		r = openGo(t, env, c.layout)
		if !r.IsExclusive() || r.Header().Format != c.layout.LockFormat {
			t.Fatalf("%s: not re-initialised: %+v", c.format, r.Header())
		}
		r.Downgrade(txnid)
		mustRun(t, c.peer, "verify", env, "-", "20", "1")
		r.Close()
	}
}
