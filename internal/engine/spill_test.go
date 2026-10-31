package engine

// Spilling (port of rlmdb core/spill.rs tests): the same writes with and
// without spilling give the same data and a consistent page audit.

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func spillRun(t *testing.T, fm format.Format, limit int, nested bool) ([][2]string, bool) {
	env, _ := openTest(t, fm, Options{MaxDBs: 4, MapSize: 256 << 20})
	env.SetDirtyLimit(limit)
	txn := beginW(t, env)
	db, _ := txn.OpenDBI([]byte("s"), format.Create)
	ps := env.PageSize()
	w := txn
	if nested {
		w, _ = env.BeginTxn(txn, 0)
	}
	for round := uint32(0); round < 3; round++ {
		for i := uint32(0); i < 600; i++ {
			n := 50 + int(i)%300
			switch i % 5 {
			case 0:
				n = 2*ps + 100 // overflow run
			case 1:
				n = ps/2 + 10
			}
			v := bytes.Repeat([]byte{byte(i ^ round)}, n)
			binary.LittleEndian.PutUint32(v, i)
			must(t, w.Put(db, binary.BigEndian.AppendUint32(nil, i), v, 0))
		}
		for i := round; i < 600; i += 7 {
			must(t, w.Del(db, binary.BigEndian.AppendUint32(nil, i), nil))
		}
	}
	spilled := w.w.flags&txnSpills != 0
	if nested {
		must(t, w.Commit())
	}
	must(t, txn.Commit())
	mustAudit(t, env)
	return scan(t, env, db), spilled
}

func TestSpillingMatchesUnspilled(t *testing.T) {
	forFormats(t, func(t *testing.T, fm format.Format) {
		for _, nested := range []bool{false, true} {
			big, spilledBig := spillRun(t, fm, format.IDLUMMax, nested)
			small, spilledSmall := spillRun(t, fm, 64, nested)
			if spilledBig || !spilledSmall {
				t.Fatalf("nested=%v: spilled %v (big limit), %v (small)", nested, spilledBig, spilledSmall)
			}
			if len(big) != len(small) {
				t.Fatal(len(big), len(small))
			}
			for i := range big {
				if big[i] != small[i] {
					t.Fatalf("nested=%v: item %d differs", nested, i)
				}
			}
		}
	})
}
