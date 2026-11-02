package engine

// Write cursors under stress: many cursors stand on one tree while puts,
// deletes and cursor writes split, merge, touch and free the pages under
// them, nested transactions copy their parents' pages and are committed
// or aborted, and a small dirty-page limit makes puts spill pages and
// touches bring them back. After every step each cursor must still be on
// the item it was on (or, if that item went, on one the model has), and
// every cursor's neighbours must be the model's. This is the fixup work
// C does on mt_cursors[] (wcursor.go), and everything a cursor keeps
// about the pages it stands on must survive it.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// csItem is a cursor's expected item: ok false if unknown (moved by a
// delete, or never positioned).
type csItem struct {
	k, v []byte
	ok   bool
}

type csLevel struct {
	txn   *Txn
	model map[string][]byte
	curs  []*Cursor
	exp   []csItem
}

type csRun struct {
	t     *testing.T
	rng   *rand.Rand
	env   *Env
	dbi   DBI
	stack []*csLevel
	// saved are, per open nested transaction, the cursor expectations of
	// the levels below it when it began (an abort restores them).
	saved [][][]csItem
	log   []string
	// fulls counts the MDB_TXN_FULL aborts, spills the top-level
	// transactions that spilled.
	fulls, spills int
}

func (r *csRun) top() *csLevel { return r.stack[len(r.stack)-1] }

func (r *csRun) note(f string, a ...any) {
	r.log = append(r.log, fmt.Sprintf(f, a...))
	if len(r.log) > 60 {
		r.log = r.log[1:]
	}
}

func (r *csRun) fail(f string, a ...any) {
	r.t.Helper()
	for _, l := range r.log {
		r.t.Log(l)
	}
	r.t.Fatalf(f, a...)
}

func (r *csRun) key(i int) []byte { return binary.BigEndian.AppendUint32([]byte("k"), uint32(i)) }

// val is a value of a size that varies from a few bytes to an overflow
// run, so that puts split pages, replace nodes and free overflow pages.
func (r *csRun) val(tag int) []byte {
	n := 4 + r.rng.Intn(40)
	switch r.rng.Intn(10) {
	case 0:
		n = r.env.PageSize() + r.rng.Intn(2*r.env.PageSize())
	case 1, 2:
		n = r.env.PageSize()/4 + r.rng.Intn(r.env.PageSize()/4)
	}
	v := bytes.Repeat([]byte{byte(tag)}, n)
	binary.LittleEndian.PutUint32(v, uint32(tag))
	return v
}

// cursors are every cursor usable in the active transaction: its own and
// its ancestors' (C shadows them into the child).
func (r *csRun) cursors() (cs []*Cursor, exp []*csItem) {
	for _, lv := range r.stack {
		for i := range lv.curs {
			cs = append(cs, lv.curs[i])
			exp = append(exp, &lv.exp[i])
		}
	}
	return cs, exp
}

func (r *csRun) sorted() []string {
	m := r.top().model
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// check verifies every cursor against the model: on its expected item if
// known, else on some item of the model (or unpositioned), and its
// neighbours are the model's.
func (r *csRun) check(what string) {
	r.t.Helper()
	m := r.top().model
	ks := r.sorted()
	cs, exp := r.cursors()
	for i, c := range cs {
		k, v, err := c.Get(nil, nil, format.GetCurrent)
		e := exp[i]
		if err != nil {
			if e.ok {
				r.fail("%s: cursor %d: current: %v, want %x", what, i, err, e.k)
			}
			continue
		}
		mv, ok := m[string(k)]
		if !ok || !bytes.Equal(mv, v) {
			r.fail("%s: cursor %d on %x (%d bytes), model has %v (%d bytes)", what, i, k, len(v), ok, len(mv))
		}
		if e.ok && !bytes.Equal(e.k, k) {
			r.fail("%s: cursor %d on %x, want %x", what, i, k, e.k)
		}
		*e = csItem{k: append([]byte(nil), k...), v: v, ok: true}
		// The model's neighbours, read through the cursor and back.
		j, _ := slices.BinarySearch(ks, string(k))
		nk, _, err := c.Get(nil, nil, format.Next)
		switch {
		case j+1 < len(ks) && (err != nil || string(nk) != ks[j+1]):
			r.fail("%s: cursor %d: next after %x is %x (%v), want %x", what, i, k, nk, err, ks[j+1])
		case j+1 == len(ks) && err != format.NotFound:
			r.fail("%s: cursor %d: next after the last key %x: %x (%v)", what, i, k, nk, err)
		}
		if err == nil {
			pk, _, err := c.Get(nil, nil, format.Prev)
			if err != nil || !bytes.Equal(pk, k) {
				r.fail("%s: cursor %d: back from %x to %x (%v), want %x", what, i, nk, pk, err, k)
			}
		} else if _, _, err := c.Get(k, nil, format.Set); err != nil {
			r.fail("%s: cursor %d: set %x: %v", what, i, k, err)
		}
	}
}

// moved forgets the expected item of every cursor on key k (a delete
// moves them to the next item).
func (r *csRun) moved(k []byte) {
	_, exp := r.cursors()
	for _, e := range exp {
		if e.ok && bytes.Equal(e.k, k) {
			e.ok = false
		}
	}
}

// setValue records a new value of k for the cursors on it.
func (r *csRun) setValue(k, v []byte) {
	_, exp := r.cursors()
	for _, e := range exp {
		if e.ok && bytes.Equal(e.k, k) {
			e.v = v
		}
	}
}

func (r *csRun) pickCursor() (*Cursor, *csItem) {
	cs, exp := r.cursors()
	i := r.rng.Intn(len(cs))
	return cs[i], exp[i]
}

func (r *csRun) existing() ([]byte, bool) {
	ks := r.sorted()
	if len(ks) == 0 {
		return nil, false
	}
	return []byte(ks[r.rng.Intn(len(ks))]), true
}

func (r *csRun) openCursors(lv *csLevel, n int) {
	for range n {
		c, err := lv.txn.OpenCursor(r.dbi)
		if err != nil {
			r.fail("open cursor: %v", err)
		}
		lv.curs = append(lv.curs, c)
		lv.exp = append(lv.exp, csItem{})
		if k, ok := r.existing(); ok {
			if _, _, err := c.Get(k, nil, format.Set); err != nil {
				r.fail("position cursor on %x: %v", k, err)
			}
			lv.exp[len(lv.exp)-1] = csItem{k: k, v: r.top().model[string(k)], ok: true}
		}
	}
}

// opFail fails the test unless err is MDB_TXN_FULL, which a nested
// transaction gets when the dirty limit leaves it nothing to spill (its
// parents' pages cannot be): then everything is aborted and the run goes
// on from the last commit.
func (r *csRun) opFail(err error, f string, a ...any) {
	r.t.Helper()
	if err != format.TxnFull {
		r.fail(f, a...)
	}
	r.note("MDB_TXN_FULL: abort to the last commit")
	r.fulls++
	r.stack[0].txn.Abort()
	r.begin()
}

// begin starts a top-level write transaction with fresh cursors, its
// model what is committed.
func (r *csRun) begin() {
	r.t.Helper()
	txn, err := r.env.BeginTxn(nil, 0)
	if err != nil {
		r.fail("begin: %v", err)
	}
	dbi, err := txn.OpenDBI([]byte("stress"), format.Create)
	if err != nil {
		r.fail("open: %v", err)
	}
	r.dbi = dbi
	model := map[string][]byte{}
	for _, kv := range scanTxn(r.t, txn, dbi) {
		model[kv[0]] = []byte(kv[1])
	}
	r.stack = []*csLevel{{txn: txn, model: model}}
	r.saved = nil
	r.openCursors(r.top(), 12)
}

func cloneModel(m map[string][]byte) map[string][]byte {
	c := make(map[string][]byte, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func cloneExp(e []csItem) []csItem { return append([]csItem(nil), e...) }

// step runs one random operation.
func (r *csRun) step(n int) {
	lv := r.top()
	m := lv.model
	switch op := r.rng.Intn(100); {
	case op < 30: // put through the transaction
		k := r.key(r.rng.Intn(2000))
		v := r.val(n)
		r.note("%d put %x (%d bytes)", n, k, len(v))
		if err := lv.txn.Put(r.dbi, k, v, 0); err != nil {
			r.opFail(err, "put %x: %v", k, err)
			return
		}
		m[string(k)] = v
		r.setValue(k, v)
	case op < 45: // delete through the transaction
		k, ok := r.existing()
		if !ok {
			return
		}
		r.note("%d del %x", n, k)
		if err := lv.txn.Del(r.dbi, k, nil); err != nil {
			r.opFail(err, "del %x: %v", k, err)
			return
		}
		delete(m, string(k))
		r.moved(k)
	case op < 55: // put through a cursor
		c, e := r.pickCursor()
		k := r.key(r.rng.Intn(2000))
		v := r.val(n)
		r.note("%d cursor put %x (%d bytes)", n, k, len(v))
		if err := c.Put(k, v, 0); err != nil {
			r.opFail(err, "cursor put %x: %v", k, err)
			return
		}
		m[string(k)] = v
		r.setValue(k, v)
		*e = csItem{k: k, v: v, ok: true}
	case op < 65: // delete through a cursor
		c, e := r.pickCursor()
		if !e.ok {
			return
		}
		k := e.k
		r.note("%d cursor del %x", n, k)
		if err := c.Del(0); err != nil {
			r.opFail(err, "cursor del %x: %v", k, err)
			return
		}
		delete(m, string(k))
		r.moved(k)
	case op < 70: // range delete through a cursor
		c, e := r.pickCursor()
		if !e.ok {
			return
		}
		r.note("%d range del from %x", n, e.k)
		for range 1 + r.rng.Intn(30) {
			k, _, err := c.Get(nil, nil, format.GetCurrent)
			if err != nil {
				break
			}
			k = append([]byte(nil), k...)
			if err := c.Del(0); err != nil {
				r.opFail(err, "range del %x: %v", k, err)
				return
			}
			delete(m, string(k))
			r.moved(k)
		}
		e.ok = false
	case op < 80: // move a cursor
		c, e := r.pickCursor()
		var k []byte
		var err error
		switch r.rng.Intn(4) {
		case 0:
			k, _, err = c.Get(nil, nil, format.First)
		case 1:
			k, _, err = c.Get(nil, nil, format.Last)
		default:
			k, _, err = c.Get(r.key(r.rng.Intn(2000)), nil, format.SetRange)
		}
		r.note("%d move -> %x (%v)", n, k, err)
		switch err {
		case nil:
			*e = csItem{k: append([]byte(nil), k...), v: m[string(k)], ok: true}
		case format.NotFound:
			*e = csItem{}
		default:
			r.fail("move: %v", err)
		}
	case op < 88: // begin a nested transaction
		if len(r.stack) >= 4 {
			return
		}
		child, err := r.env.BeginTxn(lv.txn, 0)
		if err != nil {
			r.opFail(err, "begin nested: %v", err)
			return
		}
		r.note("%d begin nested (depth %d)", n, len(r.stack))
		saved := make([][]csItem, len(r.stack))
		for i, l := range r.stack {
			saved[i] = cloneExp(l.exp)
		}
		nl := &csLevel{txn: child, model: cloneModel(m)}
		r.stack = append(r.stack, nl)
		r.openCursors(nl, 1+r.rng.Intn(3))
		r.saved = append(r.saved, saved)
	default: // end the innermost nested transaction
		if len(r.stack) < 2 {
			return
		}
		commit := r.rng.Intn(2) == 0
		r.note("%d end nested (commit %v)", n, commit)
		r.stack = r.stack[:len(r.stack)-1]
		saved := r.saved[len(r.saved)-1]
		r.saved = r.saved[:len(r.saved)-1]
		parent := r.top()
		if commit {
			if err := lv.txn.Commit(); err != nil {
				r.opFail(err, "commit nested: %v", err)
				return
			}
			parent.model = lv.model
		} else {
			lv.txn.Abort()
			// The parents' cursors are back where they were.
			for i, l := range r.stack {
				l.exp = saved[i]
			}
		}
	}
	r.check(fmt.Sprintf("step %d", n))
}

func TestCursorStress(t *testing.T) {
	type shape struct {
		f     format.Format
		ps    int
		dirty int
	}
	shapes := []shape{
		{format.V09, 0, 16},
		{format.V09, 0, 1 << 20},
		{format.V10, 256, 16},
		{format.V10, 512, 24},
		{format.V10, 1024, 1 << 20},
	}
	steps := 1500
	if testing.Short() {
		steps = 400
	}
	for _, sh := range shapes {
		for seed := int64(1); seed <= 3; seed++ {
			t.Run(fmt.Sprintf("%v-%d-dirty%d-seed%d", sh.f, sh.ps, sh.dirty, seed), func(t *testing.T) {
				env, _ := openTest(t, sh.f, Options{MaxDBs: 4, MapSize: 256 << 20, PageSize: sh.ps})
				env.SetDirtyLimit(sh.dirty)
				r := &csRun{t: t, rng: rand.New(rand.NewSource(seed)), env: env}
				for round := range 3 {
					r.begin()
					for n := range steps {
						r.step(round*steps + n)
					}
					for len(r.stack) > 1 {
						if err := r.top().txn.Commit(); err != nil {
							t.Fatal(err)
						}
						r.stack[len(r.stack)-2].model = r.top().model
						r.stack = r.stack[:len(r.stack)-1]
					}
					txn := r.top().txn
					if txn.w.flags&txnSpills != 0 {
						r.spills++
					}
					must(t, txn.Commit())
					mustAudit(t, env)
					got := scan(t, env, r.dbi)
					want := r.sorted()
					if len(got) != len(want) {
						t.Fatalf("round %d: %d items committed, model has %d", round, len(got), len(want))
					}
					for i, kv := range got {
						if kv[0] != want[i] || kv[1] != string(r.top().model[want[i]]) {
							t.Fatalf("round %d: item %d is %x, model %x", round, i, kv[0], want[i])
						}
					}
				}
				t.Logf("%d spilling commits, %d MDB_TXN_FULL aborts", r.spills, r.fulls)
				if sh.dirty < 100 && r.spills == 0 {
					t.Fatalf("dirty limit %d: nothing spilled", sh.dirty)
				}
			})
		}
	}
}

// scanTxn is every item of dbi as txn sees it.
func scanTxn(t *testing.T, txn *Txn, dbi DBI) [][2]string {
	t.Helper()
	c, err := txn.OpenCursor(dbi)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var out [][2]string
	for k, v, err := c.Get(nil, nil, format.First); err == nil; k, v, err = c.Get(nil, nil, format.Next) {
		out = append(out, [2]string{string(k), string(v)})
	}
	return out
}
