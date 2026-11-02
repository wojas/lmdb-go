package lmdb

// FuzzAPI: the public API called with whatever arguments the fuzzer
// chooses (go test -fuzz, see FUZZING.md at the repository root): write
// flags with any combination of bits (MDB_NOOVERWRITE, MDB_NODUPDATA,
// MDB_CURRENT, MDB_RESERVE, MDB_APPEND, MDB_APPENDDUP, MDB_MULTIPLE),
// cursor operation codes from 0 to 255, empty, nil and oversized keys and
// values, MDB_MULTIPLE strides, cursors used before they are positioned
// and after deletes, nested transactions, drops, databases opened with
// any flags. Calls may fail with any error, but none may panic, and no
// sequence of calls may damage the environment: after the transaction
// (committed when it can be) the file must pass the engine's page audit
// and every database must read back without error. The documented panic of PutMulti (a page that is
// not a multiple of the stride) is not provoked.

import (
	"bytes"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/fuzzwatch"
)

// fzFlagBits are the write flags a flag byte combines.
var fzFlagBits = []uint{NoOverwrite, NoDupData, Current, 0x10000 /* MDB_RESERVE */, Append, AppendDup,
	0x80000 /* MDB_MULTIPLE */, 0x1}

type apiRun struct {
	in   []byte
	at   int
	txns []*Txn
	dbis []DBI
	curs []*Cursor
}

func (r *apiRun) b() byte {
	if r.at >= len(r.in) {
		return 0
	}
	v := r.in[r.at]
	r.at++
	return v
}

func (r *apiRun) flags() uint {
	m := r.b()
	var f uint
	for i, bit := range fzFlagBits {
		if m&(1<<i) != 0 {
			f |= bit
		}
	}
	return f
}

// bytes is nil, empty, short, at the key size limit or past it, or (for
// values) up to three 4 KiB pages.
func (r *apiRun) bytes(value bool) []byte {
	sel, fill := r.b(), r.b()
	var n int
	switch sel % 10 {
	case 0:
		return nil
	case 1:
		return []byte{}
	case 2, 3, 4:
		n = 1 + int(sel>>4)
	case 5:
		n = 4
	case 6:
		n = 8
	case 7:
		n = 511 + int(sel>>7)
	case 8:
		n = 20 + int(fill)
	default:
		n = 100 + int(sel>>4)*37
		if value {
			n = int(sel>>4) * 800
		}
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = fill + byte(i&3)
	}
	return b
}

func (r *apiRun) txn() *Txn { return r.txns[len(r.txns)-1] }

// cursor is one of a few cursors of the current transaction.
func (r *apiRun) cursor() *Cursor {
	i := int(r.b()) % len(r.dbis)
	for len(r.curs) <= i {
		r.curs = append(r.curs, nil)
	}
	if r.curs[i] == nil {
		c, err := r.txn().OpenCursor(r.dbis[i])
		if err != nil {
			return nil
		}
		r.curs[i] = c
	}
	return r.curs[i]
}

func (r *apiRun) closeCursors() {
	for i, c := range r.curs {
		if c != nil {
			c.Close()
			r.curs[i] = nil
		}
	}
}

func (r *apiRun) step() {
	txn := r.txn()
	dbi := r.dbis[int(r.b())%len(r.dbis)]
	switch r.b() % 14 {
	case 0, 1:
		_ = txn.Put(dbi, r.bytes(false), r.bytes(true), r.flags())
	case 2:
		k, n, fl := r.bytes(false), int(r.b())*40, r.flags()
		if buf, err := txn.PutReserve(dbi, k, n, fl); err == nil {
			for i := range buf {
				buf[i] = byte(i)
			}
		}
	case 3:
		_ = txn.Del(dbi, r.bytes(false), r.bytes(true))
	case 4, 5:
		if c := r.cursor(); c != nil {
			_, _, _ = c.Get(r.bytes(false), r.bytes(true), uint(r.b()))
		}
	case 6:
		if c := r.cursor(); c != nil {
			_ = c.Put(r.bytes(false), r.bytes(true), r.flags())
		}
	case 7:
		if c := r.cursor(); c != nil {
			k, n, fl := r.bytes(false), int(r.b())*40, r.flags()
			if buf, err := c.PutReserve(k, n, fl); err == nil {
				for i := range buf {
					buf[i] = byte(i)
				}
			}
		}
	case 8:
		if c := r.cursor(); c != nil {
			k, stride, count := r.bytes(false), int(r.b()%20), int(r.b()%40)
			page := bytes.Repeat([]byte{r.b()}, stride*count)
			for i := range page {
				page[i] += byte(i / max(stride, 1))
			}
			_ = c.PutMulti(k, page, stride, r.flags())
		}
	case 9:
		if c := r.cursor(); c != nil {
			_ = c.Del(r.flags())
			_, _ = c.Count()
		}
	case 10:
		_, _ = txn.Get(dbi, r.bytes(false))
		_, _ = txn.Stat(dbi)
	case 11: // a nested transaction, committed or aborted
		if len(r.txns) < 4 {
			child, err := txn.env.BeginTxn(txn, 0)
			if err != nil {
				return
			}
			saved := r.curs
			r.curs = make([]*Cursor, len(r.dbis))
			r.txns = append(r.txns, child)
			for n := int(r.b() % 16); n > 0; n-- {
				r.step()
			}
			r.closeCursors()
			r.curs = saved
			r.txns = r.txns[:len(r.txns)-1]
			if r.b()&1 == 0 {
				_ = child.Commit()
			} else {
				child.Abort()
			}
		}
	case 12: // empty a database (deleting one would invalidate handles)
		// Not the main DB of a 0.9 environment: that leaks the named
		// databases' pages, as in C 0.9 (rlmdb KNOWN_C_BUGS.md #21, ported).
		if r.b()%4 == 0 && !(dbi == r.dbis[0] && txn.env.Format() == V09) {
			r.closeCursors()
			_ = txn.Drop(dbi, false)
		}
	case 13: // a database with any flags
		name := string([]byte{'x', 'a' + r.b()%4})
		if dbi, err := txn.OpenDBI(name, uint(r.b())|uint(r.b()&1)*Create); err == nil && len(r.dbis) < 12 {
			r.dbis = append(r.dbis, dbi)
		}
	}
}

func FuzzAPI(f *testing.F) {
	fuzzwatch.TuneGC()
	f.Add([]byte{0, 0, 2, 5, 3, 7, 0, 1, 1, 2, 0, 6, 2, 5, 9, 9, 1, 4, 2, 2, 15, 3, 4})
	f.Add(bytes.Repeat([]byte{1, 6, 3, 4, 0x24, 2, 8, 9, 7, 13, 1, 0x40, 3}, 30))
	f.Add(bytes.Repeat([]byte{3, 8, 2, 3, 4, 5, 9, 3, 1, 2, 11, 5, 4}, 30))
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) > 4096 {
			in = in[:4096]
		}
		dir := t.TempDir()
		fuzzWatchdog(func() { runFuzzAPI(t, dir, in) })
	})
}

func runFuzzAPI(t *testing.T, dir string, in []byte) {
	r := &apiRun{in: in}
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	f := V09
	if len(in) > 0 && in[0]&1 != 0 {
		f = V10
		must(t, env.SetPageSize(1024))
	}
	must(t, env.SetFormat(f))
	must(t, env.SetMaxDBs(16))
	must(t, env.SetMapSize(64<<20))
	must(t, env.Open(dir, NoLock|NoSync, 0o644))
	r.at = 1
	err = env.Update(func(txn *Txn) error {
		r.txns = []*Txn{txn}
		root, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		r.dbis = []DBI{root}
		for _, d := range []struct {
			name  string
			flags uint
		}{{"plain", 0}, {"dups", DupSort}, {"fixed", DupSort | DupFixed}, {"ints", IntegerKey | DupSort | IntegerDup}} {
			dbi, err := txn.OpenDBI(d.name, d.flags|Create)
			if err != nil {
				return err
			}
			r.dbis = append(r.dbis, dbi)
		}
		r.curs = make([]*Cursor, len(r.dbis))
		for r.at < len(r.in) {
			r.step()
		}
		r.closeCursors()
		return nil
	})
	_ = err // any error: the transaction did not commit
	env.Close()

	// No sequence of API calls may leave a damaged environment.
	e, err := engine.Open(dir, engine.Options{Flags: format.NoLock | format.ReadOnly, MaxDBs: 16})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e.Close()
	if err := e.Audit(); err != nil {
		t.Fatalf("audit: %v", err)
	}
	txn, err := e.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer txn.Abort()
	walk := func(dbi engine.DBI) {
		c, err := txn.OpenCursor(dbi)
		if err != nil {
			t.Fatalf("cursor: %v", err)
		}
		defer c.Close()
		for {
			_, _, err := c.Get(nil, nil, format.Next)
			if format.Code(err) == int(format.NotFound) {
				return
			}
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
		}
	}
	walk(engine.MainDBI)
	for _, name := range []string{"plain", "dups", "fixed", "ints", "xa", "xb", "xc", "xd"} {
		if dbi, err := txn.OpenDBI([]byte(name), 0); err == nil {
			walk(dbi)
		}
	}
}
