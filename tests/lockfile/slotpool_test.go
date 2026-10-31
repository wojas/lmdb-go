// The reader slots of a Go environment against C LMDB processes: what C
// sees of the idle slots it keeps between its read transactions
// (internal/engine, slotpool.go), and of the slots of transactions that
// outlive Env.Close.

package lockfiletest

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/PowerDNS/lmdb-go/lmdb"
)

// slotsOf returns the txnids C sees in the slots of process pid ("cpeer
// readers"; "-" for an idle slot).
func slotsOf(t *testing.T, peer, env string, pid int) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(mustRun(t, peer, "readers", env), "\n") {
		if strings.HasPrefix(line, "reader ") && int(atoi(t, field(t, line, "pid"))) == pid {
			out = append(out, field(t, line, "txnid"))
		}
	}
	return out
}

// TestIdleGoSlotsInC: read transactions of a Go environment leave their
// slots idle and owned by the process when they end, as a C process in its
// default TLS mode leaves a thread's slot. C sees them as such ("-", which
// its mdb_find_oldest ignores), its reader check leaves them alone while
// the Go process lives, the next Go readers reuse them (no new slots) and
// see C's commits, and Env.Close releases them.
func TestIdleGoSlotsInC(t *testing.T) {
	for _, c := range sides(t) {
		env, _ := c.newEnv(t)
		// No opener after this one is exclusive (which would reset the
		// table and hide what Close left).
		holder := startC(t, c.peer, "hold-env", env, "60", "--until-stdin").mustBeReady(t)
		genv, err := lmdb.NewEnv()
		if err != nil {
			t.Fatal(err)
		}
		if err := genv.Open(env, 0, 0o644); err != nil {
			t.Fatalf("%s: Open: %v", c.format, err)
		}
		var txns []*lmdb.Txn
		for i := 0; i < 3; i++ {
			txn, err := genv.BeginTxn(nil, lmdb.Readonly)
			if err != nil {
				t.Fatal(err)
			}
			txns = append(txns, txn)
		}
		me := os.Getpid()
		if got := slotsOf(t, c.peer, env, me); len(got) != 3 || got[0] == "-" {
			t.Fatalf("%s: Go readers in C: %v", c.format, got)
		}
		for _, txn := range txns {
			txn.Abort()
		}
		idle := []string{"-", "-", "-"}
		if got := slotsOf(t, c.peer, env, me); strings.Join(got, " ") != strings.Join(idle, " ") {
			t.Fatalf("%s: after the transactions ended, C sees %v, want 3 idle slots", c.format, got)
		}
		if !readersInMdbStat(t, c.stat, env)[me] {
			t.Fatalf("%s: mdb_stat -r does not list the idle slots", c.format)
		}
		if got := field(t, mustRun(t, c.peer, "reader-check", env), "cleared"); got != "0" {
			t.Fatalf("%s: C's reader check cleared %s idle Go slots", c.format, got)
		}

		// A C commit, then Go readers reusing the idle slots.
		mustRun(t, c.peer, "fill", env, "-", "20", "2")
		want := c.lastTxnID(t, env)
		for i := 0; i < 5; i++ {
			err := genv.View(func(txn *lmdb.Txn) error {
				if id := uint64(txn.ID()); id != want {
					t.Errorf("%s: Go reader sees txnid %d, want %d", c.format, id, want)
				}
				if got := slotsOf(t, c.peer, env, me); len(got) != 3 ||
					strings.Count(strings.Join(got, " "), strconv.FormatUint(want, 10)) != 1 {
					t.Errorf("%s: during a View C sees %v", c.format, got)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if got := slotsOf(t, c.peer, env, me); len(got) != 3 {
			t.Fatalf("%s: after reuse C sees %v, want the same 3 slots", c.format, got)
		}

		genv.Close()
		if got := slotsOf(t, c.peer, env, me); len(got) != 0 {
			t.Fatalf("%s: slots left after Env.Close: %v", c.format, got)
		}
		holder.release(t)
	}
}

// TestGoSnapshotOutlivesClose: a Go read transaction that outlives
// Env.Close keeps its reader slot and the process's liveness lock until it
// ends (review finding P1-4: Close cleared the slot, and a C writer then
// reused the pages of the snapshot the transaction was still reading). C
// writers delete and rewrite the data set in many commits; the Go
// transaction's zero-copy values stay as they were. When it ends, its slot
// goes.
func TestGoSnapshotOutlivesClose(t *testing.T) {
	for _, c := range sides(t) {
		env, txnid := c.newEnv(t)
		holder := startC(t, c.peer, "hold-env", env, "60", "--until-stdin").mustBeReady(t)
		genv, err := lmdb.NewEnv()
		if err != nil {
			t.Fatal(err)
		}
		if err := genv.Open(env, 0, 0o644); err != nil {
			t.Fatalf("%s: Open: %v", c.format, err)
		}
		txn, err := genv.BeginTxn(nil, lmdb.Readonly)
		if err != nil {
			t.Fatal(err)
		}
		txn.RawRead = true
		cur, err := txn.OpenCursor(lmdb.DBI(1))
		if err != nil {
			t.Fatal(err)
		}
		type item struct{ raw, want []byte }
		var items []item
		for {
			_, v, err := cur.Get(nil, nil, lmdb.Next)
			if lmdb.IsNotFound(err) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			items = append(items, item{v, append([]byte(nil), v...)})
		}
		cur.Close()
		if len(items) != 20 {
			t.Fatalf("%s: %d items", c.format, len(items))
		}
		genv.Close()

		me := os.Getpid()
		want := strconv.FormatUint(txnid, 10)
		if got := slotsOf(t, c.peer, env, me); strings.Join(got, " ") != want {
			t.Fatalf("%s: after Close C sees the Go slots %v, want [%s]", c.format, got, want)
		}
		if got := field(t, mustRun(t, c.peer, "reader-check", env), "cleared"); got != "0" {
			t.Fatalf("%s: C's reader check cleared %s Go slots", c.format, got)
		}
		mustRun(t, c.peer, "delete", env, "-", "20", "1", "--per-txn", "1")
		for seed := 2; seed < 5; seed++ {
			mustRun(t, c.peer, "fill", env, "-", "20", strconv.Itoa(seed), "--per-txn", "1")
		}
		for i, it := range items {
			if !bytes.Equal(it.raw, it.want) {
				t.Fatalf("%s: value %d of the Go snapshot changed after Close: %q, was %q", c.format, i, it.raw, it.want)
			}
		}
		txn.Abort()
		if got := slotsOf(t, c.peer, env, me); len(got) != 0 {
			t.Fatalf("%s: Go slots left after the last transaction: %v", c.format, got)
		}
		holder.release(t)
	}
}
