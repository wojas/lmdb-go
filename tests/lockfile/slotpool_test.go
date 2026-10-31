// The reader-slot pool of the Go implementation (internal/engine,
// slotpool.go) against C LMDB processes: what C sees of the idle slots a
// Go environment keeps between its read transactions.

package lockfiletest

import (
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
