package engine

// FuzzWriteModel: a model-based fuzz test of the write path (go test
// -fuzz, see FUZZING.md at the repository root), after rlmdb's
// tests/model.rs. The fuzz input is decoded into an environment shape
// (format, page size from 256 bytes up, dirty-page limit, DUPFIXED item
// size) and a sequence of operations on seven databases (the main DB,
// plain, DUPSORT, DUPFIXED, INTEGERDUP, INTEGERKEY, REVERSEKEY|REVERSEDUP):
// puts with every flag (MDB_NOOVERWRITE, MDB_NODUPDATA, MDB_APPEND,
// MDB_APPENDDUP, MDB_RESERVE), through the transaction and through
// cursors, MDB_CURRENT, MDB_MULTIPLE, deletes of keys and single
// duplicates, range deletes through a cursor, bulk loads, cursor moves of
// every kind, nested transactions up to three deep (committed or aborted),
// top-level commits and aborts, empty- and delete-drops, reopens. The same
// operations run on an in-memory model ordered by the comparators C uses.
// Every result is checked against the model: return codes (success,
// MDB_KEYEXIST, MDB_NOTFOUND), the value of each touched key, where a
// cursor is after a move or a write (a cursor stays on its item while
// others change the database), the duplicate counts; after every nested
// transaction the parent's view, after every commit the committed
// contents (walks in both directions, entry counts) and the page audit
// (Env.Audit: every page used once, free list included, DB records match
// their trees); at the end, plain and compacting copies. MDB_TXN_FULL and
// MDB_MAP_FULL (small dirty limits leave nested transactions nothing to
// spill) must leave a transaction that refuses to commit; everything is
// then aborted and the model goes back to the last commit.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/fuzzwatch"
)

// Kinds of keys and values of a model database.
const (
	fzText = iota
	fzU64
)

const (
	fzAny    = iota // empty to several overflow pages
	fzDups          // short duplicates of varying length
	fzFixed         // DUPFIXED items of the environment's item size
	fzIntDup        // native integers of 4 or 8 bytes
)

type fzCfg struct {
	name  string // "" for the main DB
	flags uint32
	keys  int
	vals  int
}

// The databases. Keys put in the main DB start with 'k' or 'b', so they
// sort before the named databases' records, except APPEND keys made to
// sort after everything.
var fzCfgs = []fzCfg{
	{"", 0, fzText, fzAny},
	{"nplain", 0, fzText, fzAny},
	{"ndups", format.DupSort, fzText, fzDups},
	{"nfixed", format.DupSort | format.DupFixed, fzText, fzFixed},
	{"nintdup", format.DupSort | format.DupFixed | format.IntegerDup, fzText, fzIntDup},
	{"nintkey", format.IntegerKey, fzU64, fzAny},
	{"nrev", format.ReverseKey | format.DupSort | format.ReverseDup, fzText, fzDups},
}

func (c *fzCfg) dupsort() bool { return c.flags&format.DupSort != 0 }

// fzDB is the model of one database: key -> set of data items (one item
// without DUPSORT).
type fzDB struct {
	exists bool
	kv     map[string]map[string]struct{}
}

type fzModel []fzDB

func (m fzModel) clone() fzModel {
	out := make(fzModel, len(m))
	for i, db := range m {
		out[i].exists = db.exists
		out[i].kv = make(map[string]map[string]struct{}, len(db.kv))
		for k, s := range db.kv {
			out[i].kv[k] = cloneSet(s)
		}
	}
	return out
}

func cloneSet(s map[string]struct{}) map[string]struct{} {
	c := make(map[string]struct{}, len(s))
	for k := range s {
		c[k] = struct{}{}
	}
	return c
}

// fzPos is where the model has a cursor: on item (k, d), or unknown.
type fzPos struct {
	k, d []byte
	ok   bool
}

// fzLevel is a transaction of the nesting stack: its model at begin (for
// an abort) and one tracked cursor per database.
type fzLevel struct {
	txn   *Txn
	saved fzModel
	curs  []*Cursor
	pos   []fzPos
}

type fzRun struct {
	t      *testing.T
	in     []byte
	at     int
	done   bool
	dir    string
	env    *Env
	f      format.Format
	ps     int
	dirty  int
	maxKey int
	fixed  int // DUPFIXED item size
	intSz  int // INTEGERDUP item size
	model  fzModel
	commit fzModel
	stack  []*fzLevel
	log    []string
	nops   int
	// hook, if set, gets failures instead of t (minimizing).
	hook func(msg string)
}

// fzStop unwinds a run whose failure went to the hook.
type fzStop struct{}

// fzFull unwinds an operation that hit MDB_TXN_FULL or MDB_MAP_FULL
// (expected with small dirty limits: the dirty pages of a nested
// transaction's ancestors cannot be spilled).
type fzFull struct{ err error }

// fullCheck unwinds the operation if err is MDB_TXN_FULL or MDB_MAP_FULL.
func (r *fzRun) fullCheck(err error) {
	if isCode(err, format.TxnFull) || isCode(err, format.MapFull) {
		panic(fzFull{err})
	}
}

// afterFull: the transaction that hit the limit must refuse to commit
// (rlmdb tests/model.rs); then every open transaction is aborted.
func (r *fzRun) afterFull(f fzFull) {
	r.note("%v: abort everything", f.err)
	lv := r.top()
	r.closeCursors(lv)
	if err := lv.txn.Commit(); err == nil {
		r.fail("commit after %v succeeded", f.err)
	}
	r.stack = r.stack[:len(r.stack)-1]
	for len(r.stack) > 0 {
		lv := r.top()
		r.closeCursors(lv)
		lv.txn.Abort()
		r.stack = r.stack[:len(r.stack)-1]
	}
	r.model = r.commit.clone()
	r.verifyEnv(r.env, "after a full transaction")
	r.begin()
}

// guarded runs op, handling MDB_TXN_FULL and MDB_MAP_FULL.
func (r *fzRun) guarded(op func()) {
	full := func() (f *fzFull) {
		defer func() {
			if p := recover(); p != nil {
				ff, ok := p.(fzFull)
				if !ok {
					panic(p)
				}
				f = &ff
			}
		}()
		op()
		return nil
	}()
	if full != nil {
		r.afterFull(*full)
	}
}

func (r *fzRun) b() byte {
	if r.at >= len(r.in) {
		r.done = true
		return 0
	}
	v := r.in[r.at]
	r.at++
	return v
}

// hx shows a key or value, shortened.
func hx(b []byte) string {
	if len(b) <= 24 {
		return fmt.Sprintf("%x", b)
	}
	return fmt.Sprintf("%x..(%d bytes)", b[:20], len(b))
}

func (r *fzRun) note(format string, args ...any) {
	for j, a := range args {
		if b, ok := a.([]byte); ok {
			args[j] = hx(b)
		}
	}
	r.log = append(r.log, fmt.Sprintf(format, args...))
	if len(r.log) > 80 {
		r.log = r.log[1:]
	}
}

func (r *fzRun) fail(format string, args ...any) {
	r.t.Helper()
	for j, a := range args {
		if b, ok := a.([]byte); ok {
			args[j] = hx(b)
		}
	}
	msg := fmt.Sprintf(format, args...)
	if r.hook != nil {
		r.hook(msg)
		panic(fzStop{})
	}
	r.t.Fatalf("%s/%d (dirty %d, fixed %d, int %d), op %d: %s\nlast operations:\n  %s",
		r.f, r.ps, r.dirty, r.fixed, r.intSz, r.nops, msg, strings.Join(r.log, "\n  "))
}

func (r *fzRun) top() *fzLevel { return r.stack[len(r.stack)-1] }

func (r *fzRun) txn() *Txn { return r.top().txn }

// --- comparators and model queries ----------------------------------------

func (r *fzRun) kcmp(i int) format.Comparator { return format.DefaultKeyCmp(fzCfgs[i].flags) }

func (r *fzRun) dcmp(i int) format.Comparator {
	c, ok := format.DefaultDupCmp(fzCfgs[i].flags)
	if !ok {
		return format.Memn
	}
	// mdb_xcursor_init1: 8-byte integer duplicates are size_t's.
	if c.IsInt() && r.intSz == 8 {
		c = format.Clong
	}
	return c
}

func (r *fzRun) keys(i int) [][]byte {
	db := r.model[i]
	out := make([][]byte, 0, len(db.kv))
	for k := range db.kv {
		out = append(out, []byte(k))
	}
	c := r.kcmp(i)
	slices.SortFunc(out, c.Compare)
	return out
}

// lastKey is the last key of database i, nil if it is empty. The main
// DB's keys include the records of the named databases.
func (r *fzRun) lastKey(i int) []byte {
	var last []byte
	if ks := r.keys(i); len(ks) > 0 {
		last = ks[len(ks)-1]
	}
	if i == 0 {
		for j := 1; j < len(fzCfgs); j++ {
			if !r.model[j].exists {
				continue
			}
			name := []byte(fzCfgs[j].name)
			if r.f.Layout().DBNameNUL {
				name = append(name, 0)
			}
			if last == nil || bytes.Compare(name, last) > 0 {
				last = name
			}
		}
	}
	return last
}

func (r *fzRun) dups(i int, k []byte) [][]byte {
	s := r.model[i].kv[string(k)]
	out := make([][]byte, 0, len(s))
	for d := range s {
		out = append(out, []byte(d))
	}
	c := r.dcmp(i)
	slices.SortFunc(out, c.Compare)
	return out
}

func (r *fzRun) has(i int, k []byte) bool { _, ok := r.model[i].kv[string(k)]; return ok }

func (r *fzRun) contains(i int, k, d []byte) bool {
	s, ok := r.model[i].kv[string(k)]
	if !ok {
		return false
	}
	_, ok = s[string(d)]
	return ok
}

func (r *fzRun) put(i int, k, d []byte) {
	db := &r.model[i]
	if !fzCfgs[i].dupsort() || db.kv[string(k)] == nil {
		db.kv[string(k)] = map[string]struct{}{}
	}
	db.kv[string(k)][string(d)] = struct{}{}
}

func (r *fzRun) del(i int, k, d []byte) {
	db := &r.model[i]
	if d == nil {
		delete(db.kv, string(k))
		return
	}
	s := db.kv[string(k)]
	delete(s, string(d))
	if len(s) == 0 {
		delete(db.kv, string(k))
	}
}

// pair is an item, or nil for none.
type pair struct{ k, d []byte }

func (p *pair) String() string {
	if p == nil {
		return "none"
	}
	return hx(p.k) + ":" + hx(p.d)
}

// flat is every item of database i in order.
func (r *fzRun) flat(i int) []pair {
	var out []pair
	for _, k := range r.keys(i) {
		for _, d := range r.dups(i, k) {
			out = append(out, pair{k, d})
		}
	}
	return out
}

// find is the index in flat of the first item >= (k, d).
func (r *fzRun) find(items []pair, i int, k, d []byte) int {
	kc, dc := r.kcmp(i), r.dcmp(i)
	return sortSearch(len(items), func(j int) bool {
		c := kc.Compare(items[j].k, k)
		if c == 0 && d != nil {
			c = dc.Compare(items[j].d, d)
		}
		return c >= 0
	})
}

func sortSearch(n int, f func(int) bool) int {
	lo, hi := 0, n
	for lo < hi {
		m := (lo + hi) / 2
		if !f(m) {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

func at(items []pair, j int) *pair {
	if j < 0 || j >= len(items) {
		return nil
	}
	return &items[j]
}

// --- generators -------------------------------------------------------------

var fzKeyPad = []int{0, 0, 2, 7, 15, 40, 100, 400}

// key is generated key b of database i: 64 different keys per database,
// each of its own length.
func (r *fzRun) key(i int, b byte) []byte {
	if fzCfgs[i].keys == fzU64 {
		return binary.NativeEndian.AppendUint64(nil, uint64(b)*0x0001000100010001)
	}
	k := []byte(fmt.Sprintf("k%02d", b%64))
	k = append(k, bytes.Repeat([]byte{'x'}, fzKeyPad[(b>>3)&7])...)
	return k[:min(len(k), r.maxKey)]
}

// keyFor is an existing key now and then, else a generated one.
func (r *fzRun) keyFor(i int) []byte {
	b := r.b()
	if b >= 176 {
		if ks := r.keys(i); len(ks) > 0 {
			return ks[int(b)%len(ks)]
		}
	}
	return r.key(i, b)
}

// dupFor is an existing item of k now and then, else a generated one.
func (r *fzRun) dupFor(i int, k []byte) []byte {
	b1, b2 := r.b(), r.b()
	if b1 >= 64 {
		if ds := r.dups(i, k); len(ds) > 0 {
			return ds[int(b2)%len(ds)]
		}
	}
	return r.val(i, b1, b2)
}

func (r *fzRun) val(i int, b1, b2 byte) []byte {
	switch fzCfgs[i].vals {
	case fzDups:
		// Not empty: DUPSORT items of 0 bytes are MDB_BAD_VALSIZE.
		n := []int{1, 2, 3, 6, 12, 30, 60, r.maxKey}[b1&7]
		d := []byte(fmt.Sprintf("%03d", b2))
		if n < 3 {
			return d[3-n:]
		}
		for len(d) < min(n, r.maxKey) {
			d = append(d, 'd')
		}
		return d
	case fzFixed:
		d := make([]byte, r.fixed)
		d[0], d[1] = b1>>3, b2
		return d
	case fzIntDup:
		v := uint64(b1>>3)<<8 | uint64(b2)
		if r.intSz == 4 {
			return binary.NativeEndian.AppendUint32(nil, uint32(v))
		}
		return binary.NativeEndian.AppendUint64(nil, v<<29|v)
	}
	n := []int{0, 1, 5, 20, 100, r.ps / 3, r.ps + 10, 2*r.ps + r.ps/2}[b1&7]
	d := make([]byte, n)
	for j := range d {
		d[j] = b2 + byte(j)
	}
	return d
}

// greater makes a key or item that sorts after x under c, if one fits in
// limit bytes.
func greater(c format.Comparator, x []byte, limit int) ([]byte, bool) {
	switch c.Kind {
	case format.CmpMemn:
		if len(x) < limit {
			return append(slices.Clone(x), 'z'), true
		}
		// Increment the last byte that is not 0xff.
		y := slices.Clone(x)
		for j := len(y) - 1; j >= 0; j-- {
			if y[j] != 0xff {
				y[j]++
				return y[:j+1], true
			}
		}
	case format.CmpMemnr:
		if len(x) < limit {
			return append([]byte{'a'}, x...), true
		}
	default: // native integers
		switch len(x) {
		case 8:
			if v := binary.NativeEndian.Uint64(x); v != ^uint64(0) {
				return binary.NativeEndian.AppendUint64(nil, v+1), true
			}
		case 4:
			if v := binary.NativeEndian.Uint32(x); v != ^uint32(0) {
				return binary.NativeEndian.AppendUint32(nil, v+1), true
			}
		}
	}
	return nil, false
}

// --- engine access ------------------------------------------------------------

func (r *fzRun) dbi(i int) DBI {
	if i == 0 {
		return MainDBI
	}
	c := &fzCfgs[i]
	dbi, err := r.txn().OpenDBI([]byte(c.name), c.flags|format.Create)
	if err != nil {
		r.fullCheck(err)
		r.fail("open %s: %v", c.name, err)
	}
	r.model[i].exists = true
	return dbi
}

// cursor is the tracked cursor of database i in the current transaction.
func (r *fzRun) cursor(i int) *Cursor {
	lv := r.top()
	if lv.curs[i] == nil {
		c, err := lv.txn.OpenCursor(r.dbi(i))
		if err != nil {
			r.fail("open cursor %d: %v", i, err)
		}
		lv.curs[i] = c
		lv.pos[i] = fzPos{}
	}
	return lv.curs[i]
}

func (r *fzRun) closeCursors(lv *fzLevel) {
	for i, c := range lv.curs {
		if c != nil {
			c.Close()
			lv.curs[i] = nil
		}
		lv.pos[i] = fzPos{}
	}
}

func newLevel(txn *Txn, saved fzModel) *fzLevel {
	return &fzLevel{txn: txn, saved: saved, curs: make([]*Cursor, len(fzCfgs)), pos: make([]fzPos, len(fzCfgs))}
}

func isCode(err error, e format.Errno) bool { return format.Code(err) == int(e) }

// expect checks a cursor or get result against the model's item.
func (r *fzRun) expect(what string, k, d []byte, err error, want *pair) {
	r.t.Helper()
	switch {
	case want == nil && isCode(err, format.NotFound):
	case want != nil && err == nil && bytes.Equal(k, want.k) && bytes.Equal(d, want.d):
	default:
		r.fail("%s: got %s:%s (%v), model has %v", what, k, d, err, want)
	}
}

// checkGet checks txn.Get of k: the key's first item, or MDB_NOTFOUND.
func (r *fzRun) checkGet(i int, k []byte) {
	d, err := r.txn().Get(r.dbi(i), k)
	var want *pair
	if ds := r.dups(i, k); len(ds) > 0 {
		want = &pair{k, ds[0]}
	}
	r.expect(fmt.Sprintf("get %d %x", i, k), k, d, err, want)
}

// afterWrite checks that the tracked cursor of database i (other than
// the one that wrote) is still on its item.
func (r *fzRun) afterWrite(i int, writer *Cursor) {
	lv := r.top()
	c, p := lv.curs[i], &lv.pos[i]
	if c == nil || c == writer || !p.ok {
		return
	}
	want := pair{k: p.k, d: p.d}
	if fzCfgs[i].dupsort() {
		if !r.contains(i, p.k, p.d) {
			*p = fzPos{}
			return
		}
	} else {
		ds := r.dups(i, p.k)
		if len(ds) == 0 {
			*p = fzPos{}
			return
		}
		want.d = ds[0]
		p.d = ds[0]
	}
	k, d, err := c.Get(nil, nil, format.GetCurrent)
	r.expect(fmt.Sprintf("cursor %d stays on its item", i), k, d, err, &want)
}

// --- operations -----------------------------------------------------------------

// putFlags picks the flags of a put and whether the model expects success
// (else MDB_KEYEXIST), maybe replacing k or d by one that suits APPEND or
// APPENDDUP (rlmdb tests/model.rs put_flags).
func (r *fzRun) putFlags(i int, k, d []byte, sel byte) (uint32, []byte, []byte, bool) {
	c := &fzCfgs[i]
	switch sel & 15 {
	case 8, 9:
		return format.NoOverwrite, k, d, !r.has(i, k)
	case 10, 11:
		if c.dupsort() {
			return format.NoDupData, k, d, !r.contains(i, k, d)
		}
	case 12, 13:
		last := r.lastKey(i)
		if last == nil {
			return format.Append, k, d, true
		}
		if sel&16 != 0 {
			if g, ok := greater(r.kcmp(i), last, r.maxKey); ok {
				k = g
			}
		}
		return format.Append, k, d, r.kcmp(i).Compare(k, last) > 0
	case 14, 15:
		if c.dupsort() {
			ds := r.dups(i, k)
			if len(ds) == 0 {
				return format.AppendDup, k, d, true
			}
			last := ds[len(ds)-1]
			if sel&16 != 0 && c.vals == fzDups {
				if g, ok := greater(r.dcmp(i), last, r.maxKey); ok {
					d = g
				}
			} else if sel&16 != 0 {
				if g, ok := greater(r.dcmp(i), last, len(last)); ok && len(g) == len(last) {
					d = g
				}
			}
			return format.AppendDup, k, d, r.dcmp(i).Compare(d, last) > 0
		}
	}
	return 0, k, d, true
}

func (r *fzRun) db() int { return int(r.b()) % len(fzCfgs) }

func (r *fzRun) opPut(viaCursor bool) {
	i := r.db()
	k := r.keyFor(i)
	d := r.val(i, r.b(), r.b())
	sel := r.b()
	flags, k, d, ok := r.putFlags(i, k, d, sel)
	reserve := !fzCfgs[i].dupsort() && sel&32 != 0
	r.note("put%s db %d %s %s flags %#x reserve %v (model ok %v)", map[bool]string{true: " (cursor)"}[viaCursor], i, k, d, flags, reserve, ok)
	var c *Cursor
	var err error
	var buf []byte
	switch {
	case viaCursor && reserve:
		c = r.cursor(i)
		buf, err = c.PutReserve(k, len(d), flags)
	case viaCursor:
		c = r.cursor(i)
		err = c.Put(k, d, flags)
	case reserve:
		buf, err = r.txn().PutReserve(r.dbi(i), k, len(d), flags)
	default:
		err = r.txn().Put(r.dbi(i), k, d, flags)
	}
	r.fullCheck(err)
	switch {
	case err == nil && ok:
		if reserve {
			if len(buf) != len(d) {
				r.fail("reserve gave %d bytes for %d", len(buf), len(d))
			}
			copy(buf, d)
		}
		r.put(i, k, d)
		if c != nil {
			r.top().pos[i] = fzPos{k, d, true}
			kk, dd, err := c.Get(nil, nil, format.GetCurrent)
			r.expect("cursor after its put", kk, dd, err, &pair{k, d})
		}
	case !ok && isCode(err, format.KeyExist):
		if c != nil {
			r.top().pos[i] = fzPos{}
		}
	default:
		r.fail("put: %v, model expects ok=%v", err, ok)
	}
	r.afterWrite(i, c)
	r.checkGet(i, k)
}

func (r *fzRun) opDel() {
	i := r.db()
	k := r.keyFor(i)
	var d []byte
	withData := fzCfgs[i].dupsort() && r.b()&1 == 0
	if withData {
		d = r.dupFor(i, k)
	}
	found := r.has(i, k)
	if withData {
		found = r.contains(i, k, d)
	}
	r.note("del db %d %s %s (model found %v)", i, k, d, found)
	err := r.txn().Del(r.dbi(i), k, d)
	r.fullCheck(err)
	switch {
	case found && err == nil:
		r.del(i, k, d)
	case !found && isCode(err, format.NotFound):
	default:
		r.fail("del: %v, model found %v", err, found)
	}
	r.afterWrite(i, nil)
	r.checkGet(i, k)
}

// position puts the tracked cursor of i on (k, d) (d: any item of k
// without DUPSORT) and checks the result; false if the item is not there.
func (r *fzRun) position(i int, k, d []byte) bool {
	c := r.cursor(i)
	lv := r.top()
	var gk, gd []byte
	var err error
	var want *pair
	if fzCfgs[i].dupsort() {
		gk, gd, err = c.Get(k, d, format.GetBoth)
		if r.contains(i, k, d) {
			want = &pair{k, d}
		}
	} else {
		gk, gd, err = c.Get(k, nil, format.Set)
		if ds := r.dups(i, k); len(ds) > 0 {
			want = &pair{k, ds[0]}
		}
	}
	r.expect(fmt.Sprintf("position db %d on %x:%x", i, k, d), gk, gd, err, want)
	if want == nil {
		lv.pos[i] = fzPos{}
		return false
	}
	lv.pos[i] = fzPos{want.k, want.d, true}
	return true
}

func (r *fzRun) opCursorDel() {
	i := r.db()
	k := r.keyFor(i)
	var d []byte
	if fzCfgs[i].dupsort() {
		d = r.dupFor(i, k)
	}
	sel := r.b()
	if !r.position(i, k, d) {
		return
	}
	d = r.top().pos[i].d
	var flags uint32
	if fzCfgs[i].dupsort() && sel&1 != 0 {
		flags = format.NoDupData
	}
	r.note("cursor del db %d %s %s flags %#x", i, k, d, flags)
	c := r.cursor(i)
	if err := c.Del(flags); err != nil {
		r.fullCheck(err)
		r.fail("cursor del: %v", err)
	}
	if flags != 0 || !fzCfgs[i].dupsort() {
		r.del(i, k, nil)
	} else {
		r.del(i, k, d)
	}
	r.top().pos[i] = fzPos{}
	r.checkGet(i, k)
}

func (r *fzRun) opCurrent() {
	i := r.db()
	ks := r.keys(i)
	b1, b2, b3, b4, sel := r.b(), r.b(), r.b(), r.b(), r.b()
	if len(ks) == 0 {
		return
	}
	k := ks[int(b1)%len(ks)]
	ds := r.dups(i, k)
	j := int(b2) % len(ds)
	d := ds[j]
	if !r.position(i, k, d) {
		r.fail("current: cannot position on %s:%s", k, d)
	}
	nd := r.val(i, b3, b4)
	dupsort := fzCfgs[i].dupsort()
	if dupsort {
		// The new item must sort where the old one is.
		dc := r.dcmp(i)
		fits := !r.contains(i, k, nd) && (j == 0 || dc.Compare(ds[j-1], nd) < 0) &&
			(j == len(ds)-1 || dc.Compare(nd, ds[j+1]) < 0)
		if !fits {
			nd = d
		}
	}
	reserve := !dupsort && sel&1 != 0
	r.note("cursor put CURRENT db %d %s %s -> %s reserve %v", i, k, d, nd, reserve)
	c := r.cursor(i)
	var err error
	if reserve {
		var buf []byte
		if buf, err = c.PutReserve(k, len(nd), format.Current); err == nil {
			if len(buf) != len(nd) {
				r.fail("reserve CURRENT gave %d bytes for %d", len(buf), len(nd))
			}
			copy(buf, nd)
		}
	} else {
		err = c.Put(k, nd, format.Current)
	}
	if err != nil {
		r.fullCheck(err)
		r.fail("cursor put CURRENT: %v", err)
	}
	if dupsort && !bytes.Equal(nd, d) {
		r.del(i, k, d)
	}
	r.put(i, k, nd)
	r.top().pos[i] = fzPos{k, nd, true}
	gk, gd, err := c.Get(nil, nil, format.GetCurrent)
	r.expect("cursor after CURRENT", gk, gd, err, &pair{k, nd})
	r.checkGet(i, k)
}

func (r *fzRun) opMultiple() {
	i := 3 + int(r.b())%2 // the DUPFIXED databases
	k := r.keyFor(i)
	n := 1 + int(r.b())%40
	sel := r.b()
	items := make([][]byte, n)
	for j := range items {
		items[j] = r.dupFor(i, k)
	}
	var flags uint32
	if sel&1 != 0 {
		flags = format.NoDupData
	}
	// With MDB_NODUPDATA the first existing item stops it with KEYEXIST.
	sim := r.model.clone()
	saved := r.model
	r.model = sim
	stop := -1
	for x, it := range items {
		if flags != 0 && r.contains(i, k, it) {
			stop = x
			break
		}
		r.put(i, k, it)
	}
	r.model = saved
	r.note("cursor put MULTIPLE db %d %s %d items flags %#x (model stops at %d)", i, k, n, flags, stop)
	c := r.cursor(i)
	got, err := c.PutMulti(k, bytes.Join(items, nil), len(items[0]), flags)
	r.fullCheck(err)
	stored := n
	switch {
	case stop < 0 && err == nil && got == n:
	case stop >= 0 && isCode(err, format.KeyExist):
		stored = stop
	default:
		r.fail("put MULTIPLE: %d stored, %v; model stops at %d", got, err, stop)
	}
	for _, it := range items[:stored] {
		r.put(i, k, it)
	}
	if err == nil {
		r.top().pos[i] = fzPos{k, items[n-1], true}
		gk, gd, err := c.Get(nil, nil, format.GetCurrent)
		r.expect("cursor after MULTIPLE", gk, gd, err, &pair{k, items[n-1]})
	} else {
		r.top().pos[i] = fzPos{}
	}
	r.checkGet(i, k)
}

// opBulk loads up to 128 items, in order and maybe with MDB_APPEND or
// MDB_APPENDDUP, so that trees grow several levels deep.
func (r *fzRun) opBulk() {
	i := r.db()
	n := 1 + int(r.b())%128
	base := r.b()
	sel := r.b()
	c := &fzCfgs[i]
	dbi := r.dbi(i)
	r.note("bulk db %d n %d base %d sel %#x", i, n, base, sel)
	var dupKey []byte
	if c.dupsort() {
		dupKey = r.key(i, base)
	}
	for j := 0; j < n; j++ {
		var k, d []byte
		var flags uint32
		ok := true
		switch {
		case c.dupsort():
			k = dupKey
			switch c.vals {
			case fzFixed:
				d = make([]byte, r.fixed)
				binary.BigEndian.PutUint16(d, uint16(base)<<8|uint16(j))
			case fzIntDup:
				v := uint64(base)<<16 | uint64(j)
				if r.intSz == 4 {
					d = binary.NativeEndian.AppendUint32(nil, uint32(v))
				} else {
					d = binary.NativeEndian.AppendUint64(nil, v)
				}
			default:
				d = []byte(fmt.Sprintf("%03d%04d", base, j))
				if c.flags&format.ReverseDup != 0 {
					slices.Reverse(d)
				}
			}
			if sel&1 != 0 {
				flags = format.AppendDup
				if ds := r.dups(i, k); len(ds) > 0 {
					ok = r.dcmp(i).Compare(d, ds[len(ds)-1]) > 0
				}
			}
		default:
			if c.keys == fzU64 {
				k = binary.NativeEndian.AppendUint64(nil, uint64(base)<<32|uint64(j))
			} else {
				k = []byte(fmt.Sprintf("b%03d%04d", base, j))
				if c.flags&format.ReverseKey != 0 {
					slices.Reverse(k)
				}
			}
			d = bytes.Repeat([]byte{byte(j)}, j%17)
			if sel&1 != 0 {
				flags = format.Append
				if last := r.lastKey(i); last != nil {
					ok = r.kcmp(i).Compare(k, last) > 0
				}
			}
		}
		err := r.txn().Put(dbi, k, d, flags)
		r.fullCheck(err)
		switch {
		case err == nil && ok:
			r.put(i, k, d)
		case !ok && isCode(err, format.KeyExist):
		default:
			r.fail("bulk put %d %s %s flags %#x: %v (model ok %v)", j, k, d, flags, err, ok)
		}
	}
	r.afterWrite(i, nil)
}

// opRangeDel deletes up to 64 consecutive keys through the tracked
// cursor: after each delete MDB_NEXT must give the deleted key's
// successor.
func (r *fzRun) opRangeDel() {
	// Not the main DB: its last key is followed by the named databases.
	i := 1 + int(r.b())%(len(fzCfgs)-1)
	b1, b2 := r.b(), r.b()
	ks := r.keys(i)
	if len(ks) == 0 {
		return
	}
	start := int(b1) % len(ks)
	n := 1 + int(b2)%64
	dupsort := fzCfgs[i].dupsort()
	r.note("range del db %d from %s, %d keys", i, ks[start], n)
	ds := r.dups(i, ks[start])
	if !r.position(i, ks[start], ds[0]) {
		r.fail("range del: cannot position")
	}
	c := r.cursor(i)
	var flags uint32
	if dupsort {
		flags = format.NoDupData
	}
	for j := start; j < start+n && j < len(ks); j++ {
		if err := c.Del(flags); err != nil {
			r.fullCheck(err)
			r.fail("range del of %s: %v", ks[j], err)
		}
		r.del(i, ks[j], nil)
		k, d, err := c.Get(nil, nil, format.Next)
		var want *pair
		if j+1 < len(ks) {
			want = &pair{ks[j+1], r.dups(i, ks[j+1])[0]}
		}
		r.expect("MDB_NEXT after a cursor delete", k, d, err, want)
		if want == nil {
			break
		}
	}
	r.top().pos[i] = fzPos{}
	r.afterWrite(i, c)
}

// opMove moves the tracked cursor of a database (not the main DB, whose
// named-database records the model does not know) and checks where it
// lands.
func (r *fzRun) opMove() {
	i := 1 + int(r.b())%(len(fzCfgs)-1)
	sel := r.b()
	c := r.cursor(i)
	lv := r.top()
	p := lv.pos[i]
	items := r.flat(i)
	dupsort := fzCfgs[i].dupsort()
	var op format.CursorOp
	var k, d []byte
	var want *pair
	check := true
	cur := -1
	if p.ok {
		cur = r.find(items, i, p.k, p.d)
	}
	switch sel % 16 {
	case 0:
		op, want = format.First, at(items, 0)
	case 1:
		op, want = format.Last, at(items, len(items)-1)
	case 2, 3:
		op = format.Next
		check, want = p.ok, at(items, cur+1)
	case 4, 5:
		op = format.Prev
		check, want = p.ok, at(items, cur-1)
	case 6:
		op, k = format.Set, r.keyFor(i)
		j := r.find(items, i, k, nil)
		if w := at(items, j); w != nil && bytes.Equal(w.k, k) {
			want = w
		}
	case 7:
		op, k = format.SetRange, r.keyFor(i)
		want = at(items, r.find(items, i, k, nil))
	case 8:
		op = format.NextNoDup
		if p.ok {
			j := cur
			for j < len(items) && bytes.Equal(items[j].k, p.k) {
				j++
			}
			want = at(items, j)
		}
		check = p.ok
	case 9:
		op = format.PrevNoDup
		if p.ok {
			j := r.find(items, i, p.k, nil) - 1
			want = at(items, j)
		}
		check = p.ok
	case 10, 11:
		if !dupsort || !p.ok {
			return
		}
		op = format.NextDup
		if w := at(items, cur+1); w != nil && bytes.Equal(w.k, p.k) {
			want = w
		}
		if sel%16 == 11 {
			op = format.PrevDup
			want = nil
			if w := at(items, cur-1); w != nil && bytes.Equal(w.k, p.k) {
				want = w
			}
		}
	case 12:
		if !dupsort || !p.ok {
			return
		}
		ds := r.dups(i, p.k)
		op, want = format.FirstDup, &pair{p.k, ds[0]}
		if sel&16 != 0 {
			op, want = format.LastDup, &pair{p.k, ds[len(ds)-1]}
		}
	case 13:
		if !dupsort {
			return
		}
		k = r.keyFor(i)
		d = r.dupFor(i, k)
		op = format.GetBoth
		if r.contains(i, k, d) {
			want = &pair{k, d}
		}
		if sel&16 != 0 {
			op, want = format.GetBothRange, nil
			if w := at(items, r.find(items, i, k, d)); w != nil && bytes.Equal(w.k, k) {
				want = w
			}
		}
	case 14:
		if !p.ok {
			return
		}
		n, err := c.Count()
		if want := uint64(len(r.dups(i, p.k))); !dupsort {
			if err == nil && n != 1 {
				r.fail("count without DUPSORT: %d, %v", n, err)
			}
		} else if err != nil || n != want {
			r.fail("count of %s: %d (%v), model %d", p.k, n, err, want)
		}
		return
	case 15:
		r.opMultipleGet(i, c, p)
		return
	}
	gk, gd, err := c.Get(k, d, op)
	r.note("cursor %d op %d %s %s -> %s %s %v", i, op, k, d, gk, gd, err)
	if check {
		r.expect(fmt.Sprintf("cursor db %d op %d from %v", i, op, p), gk, gd, err, want)
	}
	if err == nil && (!check || want != nil) && r.contains(i, gk, gd) {
		lv.pos[i] = fzPos{bytes.Clone(gk), bytes.Clone(gd), true}
	} else if err == nil && !dupsort && r.has(i, gk) {
		lv.pos[i] = fzPos{bytes.Clone(gk), bytes.Clone(gd), true}
	} else {
		lv.pos[i] = fzPos{}
	}
}

// opMultipleGet is MDB_GET_MULTIPLE or MDB_NEXT_MULTIPLE on a DUPFIXED
// database: a run of consecutive items of the key (rlmdb model.rs
// multiple()).
func (r *fzRun) opMultipleGet(i int, c *Cursor, p fzPos) {
	if fzCfgs[i].vals != fzFixed && fzCfgs[i].vals != fzIntDup || !p.ok {
		return
	}
	ds := r.dups(i, p.k)
	size := len(ds[0])
	op := format.GetMultiple
	var target []byte
	if r.b()&1 == 0 {
		target = p.d
	} else {
		op = format.NextMultiple
		for j, x := range ds {
			if bytes.Equal(x, p.d) && j+1 < len(ds) {
				target = ds[j+1]
			}
		}
	}
	gk, gd, err := c.Get(nil, nil, op)
	r.note("cursor %d op %d -> %s %d bytes %v", i, op, gk, len(gd), err)
	lv := r.top()
	switch {
	case target == nil && op == format.NextMultiple:
		// Past the key's last item: NOTFOUND, or the next key's items.
		lv.pos[i] = fzPos{}
		return
	case err == nil && len(gd) == 0 && op == format.GetMultiple && len(ds) == 1:
		// A key whose only item is not in a sub-page has nothing to fetch.
		if !bytes.Equal(gk, p.k) {
			r.fail("GET_MULTIPLE: key %s, cursor on %s", gk, p.k)
		}
		return
	case err != nil || !bytes.Equal(gk, p.k) || len(gd) == 0 || len(gd)%size != 0:
		r.fail("op %d on %s:%s: %s, %d bytes, %v", op, p.k, p.d, gk, len(gd), err)
	}
	var run [][]byte
	for o := 0; o < len(gd); o += size {
		run = append(run, gd[o:o+size])
	}
	start := slices.IndexFunc(ds, func(x []byte) bool { return bytes.Equal(x, run[0]) })
	if start < 0 || start+len(run) > len(ds) {
		r.fail("op %d: run not in the model", op)
	}
	for j, x := range run {
		if !bytes.Equal(ds[start+j], x) {
			r.fail("op %d: items are not consecutive duplicates", op)
		}
	}
	if !slices.ContainsFunc(run, func(x []byte) bool { return bytes.Equal(x, target) }) {
		r.fail("op %d: run does not contain %s", op, target)
	}
	lv.pos[i] = fzPos{p.k, bytes.Clone(run[len(run)-1]), true}
}

func (r *fzRun) opDrop() {
	i := 1 + int(r.b())%(len(fzCfgs)-1)
	del := r.b()&1 != 0 && len(r.stack) == 1
	r.note("drop db %d del %v", i, del)
	lv := r.top()
	dbi := r.dbi(i)
	if lv.curs[i] != nil {
		lv.curs[i].Close()
		lv.curs[i] = nil
	}
	lv.pos[i] = fzPos{}
	if err := r.txn().Drop(dbi, del); err != nil {
		r.fullCheck(err)
		r.fail("drop: %v", err)
	}
	r.model[i].kv = map[string]map[string]struct{}{}
	if del {
		r.model[i].exists = false
	}
}

func (r *fzRun) opNested() {
	sel := r.b()
	if sel&1 == 0 {
		if len(r.stack) >= 4 {
			return
		}
		child, err := r.env.BeginTxn(r.txn(), 0)
		if err != nil {
			r.fail("begin nested: %v", err)
		}
		r.note("nested begin (depth %d)", len(r.stack))
		r.stack = append(r.stack, newLevel(child, r.model.clone()))
		return
	}
	if len(r.stack) < 2 {
		return
	}
	lv := r.top()
	r.closeCursors(lv)
	if sel&2 != 0 {
		r.note("nested commit")
		if err := lv.txn.Commit(); err != nil {
			r.fullCheck(err)
			r.fail("nested commit: %v", err)
		}
	} else {
		r.note("nested abort")
		lv.txn.Abort()
		r.model = lv.saved
	}
	r.stack = r.stack[:len(r.stack)-1]
	parent := r.top()
	for j := range parent.pos {
		parent.pos[j] = fzPos{}
	}
	r.verify(r.txn(), "after nested end")
}

// endTop commits (or aborts) every open transaction, checks the
// committed state and begins a new top-level transaction (after a reopen
// if asked).
func (r *fzRun) endTop(commit, reopen bool) {
	for len(r.stack) > 0 {
		lv := r.top()
		r.closeCursors(lv)
		if commit {
			if err := lv.txn.Commit(); err != nil {
				if isCode(err, format.TxnFull) || isCode(err, format.MapFull) {
					// The failed commit ended it: abort the rest.
					r.stack = r.stack[:len(r.stack)-1]
					r.note("commit: %v", err)
					commit = false
					continue
				}
				r.fail("commit (depth %d): %v", len(r.stack)-1, err)
			}
		} else {
			lv.txn.Abort()
		}
		r.stack = r.stack[:len(r.stack)-1]
	}
	if commit {
		r.note("commit")
		r.commit = r.model.clone()
	} else {
		r.note("abort")
		r.model = r.commit.clone()
	}
	r.verifyEnv(r.env, "committed")
	if reopen {
		r.note("reopen")
		r.env.Close()
		r.env = r.open(r.dir)
		r.verifyEnv(r.env, "reopened")
	}
}

func (r *fzRun) begin() {
	txn, err := r.env.BeginTxn(nil, 0)
	if err != nil {
		r.fail("begin: %v", err)
	}
	r.stack = append(r.stack, newLevel(txn, nil))
}

// --- verification -----------------------------------------------------------

// verify compares every database in txn with the model: walks both ways,
// duplicate counts, entry counts.
func (r *fzRun) verify(txn *Txn, what string) {
	named := 0
	for i := 1; i < len(fzCfgs); i++ {
		if r.model[i].exists {
			named++
		}
	}
	for i := range fzCfgs {
		c := &fzCfgs[i]
		dbi := MainDBI
		if i > 0 {
			var err error
			dbi, err = txn.OpenDBI([]byte(c.name), 0)
			if !r.model[i].exists {
				if !isCode(err, format.NotFound) {
					r.fail("%s: %s should not exist: %v", what, c.name, err)
				}
				continue
			}
			if err != nil {
				r.fail("%s: open %s: %v", what, c.name, err)
			}
		}
		want := r.flat(i)
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			r.fail("%s: cursor on %d: %v", what, i, err)
		}
		var got []pair
		for {
			k, d, err := cur.Get(nil, nil, format.Next)
			if isCode(err, format.NotFound) {
				break
			}
			if err != nil {
				r.fail("%s: walk %d: %v", what, i, err)
			}
			if i == 0 && r.isDBRecord(k) {
				continue
			}
			got = append(got, pair{bytes.Clone(k), bytes.Clone(d)})
		}
		r.sameItems(what+" forward", i, got, want)
		got = got[:0]
		// From MDB_LAST: after MDB_NEXT ran off the end the cursor is still
		// on the last item, and MDB_PREV would skip it (as in C).
		for op := format.Last; ; op = format.Prev {
			k, d, err := cur.Get(nil, nil, op)
			if isCode(err, format.NotFound) {
				break
			}
			if err != nil {
				r.fail("%s: walk back %d: %v", what, i, err)
			}
			if i == 0 && r.isDBRecord(k) {
				continue
			}
			got = append(got, pair{bytes.Clone(k), bytes.Clone(d)})
		}
		slices.Reverse(got)
		r.sameItems(what+" backward", i, got, want)
		if c.dupsort() {
			for _, k := range r.keys(i) {
				if _, _, err := cur.Get(k, nil, format.Set); err != nil {
					r.fail("%s: set %s: %v", what, k, err)
				}
				if n, err := cur.Count(); err != nil || n != uint64(len(r.model[i].kv[string(k)])) {
					r.fail("%s: count of %s in %d: %d (%v), model %d", what, k, i, n, err, len(r.model[i].kv[string(k)]))
				}
			}
		}
		cur.Close()
		st, err := txn.Stat(dbi)
		wantN := uint64(len(want))
		if i == 0 {
			wantN += uint64(named)
		}
		if err != nil || st.Entries != wantN {
			r.fail("%s: db %d has %d entries (%v), model %d", what, i, st.Entries, err, wantN)
		}
	}
}

// isDBRecord reports whether k, a key of the main DB, is the record of a
// named database.
func (r *fzRun) isDBRecord(k []byte) bool {
	for j := 1; j < len(fzCfgs); j++ {
		name := []byte(fzCfgs[j].name)
		if r.f.Layout().DBNameNUL {
			name = append(name, 0)
		}
		if bytes.Equal(k, name) {
			return true
		}
	}
	return false
}

func (r *fzRun) sameItems(what string, i int, got, want []pair) {
	if len(got) != len(want) {
		r.fail("%s: db %d has %d items, model %d", what, i, len(got), len(want))
	}
	for j := range got {
		if !bytes.Equal(got[j].k, want[j].k) || !bytes.Equal(got[j].d, want[j].d) {
			r.fail("%s: db %d item %d is %v, model %v", what, i, j, &got[j], &want[j])
		}
	}
}

// verifyEnv checks the committed state of env: contents and page audit.
func (r *fzRun) verifyEnv(env *Env, what string) {
	if err := env.Audit(); err != nil {
		r.fail("%s: audit: %v", what, err)
	}
	txn, err := env.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		r.fail("%s: begin read: %v", what, err)
	}
	defer txn.Abort()
	saved := r.model
	r.model = r.commit
	r.verify(txn, what)
	r.model = saved
}

// verifyCopies makes a plain and a compacting copy of the committed state
// and checks them.
func (r *fzRun) verifyCopies() {
	for _, flags := range []uint32{0, format.CopyCompact} {
		dir := r.t.TempDir()
		out, err := os.Create(filepath.Join(dir, "data.mdb"))
		if err != nil {
			r.t.Fatal(err)
		}
		if err := r.env.CopyTo(out, flags); err != nil {
			r.fail("copy (flags %d): %v", flags, err)
		}
		out.Close()
		env := r.open(dir)
		r.verifyEnv(env, fmt.Sprintf("copy (flags %d)", flags))
		env.Close()
	}
}

func (r *fzRun) open(dir string) *Env {
	env, err := Open(dir, Options{Format: r.f, MapSize: 256 << 20, MaxDBs: 8, Flags: format.NoLock | format.NoSync})
	if err != nil {
		r.fail("open %s: %v", dir, err)
	}
	if r.dirty > 0 {
		env.SetDirtyLimit(r.dirty)
	}
	return env
}

// --- the target -------------------------------------------------------------

// fzShapes are the formats and page sizes (0: the OS's).
var fzShapes = []struct {
	f  format.Format
	ps int
}{{format.V09, 0}, {format.V09, 512}, {format.V10, 256}, {format.V10, 512}, {format.V10, 1024}, {format.V10, 4096}}

func runFuzzModel(t *testing.T, in []byte) { runFuzzModelHook(t, in, nil) }

// runFuzzModelHook runs in; with hook set, a failure goes to hook and the
// run stops quietly (a panic of the engine still fails t).
func runFuzzModelHook(t *testing.T, in []byte, hook func(msg string)) {
	r := &fzRun{t: t, in: in, dir: t.TempDir(), hook: hook}
	if hook != nil {
		defer func() {
			if p := recover(); p != nil {
				if _, ok := p.(fzStop); !ok {
					hook(fmt.Sprintf("panic: %v", p))
				}
			}
		}()
	}
	shape := fzShapes[int(r.b())%len(fzShapes)]
	r.f = shape.f
	r.dirty = []int{0, 16, 24, 64}[r.b()%4]
	r.fixed = []int{2, 4, 8, 12}[r.b()%4]
	r.intSz = []int{4, 8}[r.b()%2]
	if r.done {
		return
	}
	opts := Options{Format: r.f, PageSize: shape.ps, MapSize: 256 << 20, MaxDBs: 8, Flags: format.NoLock | format.NoSync}
	if r.f == format.V09 && shape.ps != 0 {
		// A 0.9 environment of another page size than the OS's: write the
		// meta pages of an empty one first.
		l := format.LayoutV09
		m := format.NewEnvMeta(l, uint32(shape.ps), 1<<20, 0)
		buf := make([]byte, 2*shape.ps)
		m.WriteInitialPages(buf, shape.ps, l)
		if err := os.WriteFile(filepath.Join(r.dir, "data.mdb"), buf, 0o644); err != nil {
			t.Fatal(err)
		}
		opts.PageSize = 0
	}
	env, err := Open(r.dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	r.env = env
	defer func() { r.env.Close() }()
	if r.dirty > 0 {
		env.SetDirtyLimit(r.dirty)
	}
	r.ps = env.PageSize()
	l := r.f.Layout()
	r.maxKey = min(env.MaxKeySize(), l.NodeMax(r.ps)-(format.NodeSize+format.DBSize))
	r.model = make(fzModel, len(fzCfgs))
	for i := range r.model {
		r.model[i].kv = map[string]map[string]struct{}{}
	}
	r.model[0].exists = true
	r.commit = r.model.clone()
	r.begin()
	for r.nops = 0; r.nops < 400 && !r.done; r.nops++ {
		r.guarded(r.step)
		if r.t.Failed() {
			return
		}
	}
	r.endTop(true, false)
	r.verifyCopies()
}

// step decodes and runs one operation.
func (r *fzRun) step() {
	{
		switch op := r.b() % 24; {
		case op < 6:
			r.opPut(false)
		case op < 8:
			r.opPut(true)
		case op < 10:
			r.opDel()
		case op < 11:
			r.opCursorDel()
		case op < 12:
			r.opCurrent()
		case op < 13:
			r.opMultiple()
		case op < 14:
			r.opBulk()
		case op < 15:
			r.opRangeDel()
		case op < 19:
			r.opMove()
		case op < 20:
			r.opNested()
		case op < 21:
			sel := r.b()
			r.endTop(sel&1 == 0, sel&6 == 6)
			r.begin()
		case op < 22:
			r.opDrop()
		default:
			k := r.keyFor(0)
			r.checkGet(0, k)
		}
	}
}

func FuzzWriteModel(f *testing.F) {
	// Seeds: each shape with a mix of operations, from a fixed generator.
	for s := range fzShapes {
		for _, n := range []int{200, 1200} {
			in := []byte{byte(s), byte(n / 200), byte(s), byte(n)}
			x := uint64(s*7919 + n)
			for len(in) < n {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				in = append(in, byte(x))
			}
			f.Add(in)
		}
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) > 8192 {
			in = in[:8192]
		}
		fuzzWatchdog(func() { runFuzzModel(t, in) })
	})
}

// fuzzWatchdog crashes the process if fn runs longer than a minute while
// fuzzing (with every goroutine's stack), so that the fuzzer keeps a
// hanging input (internal/fuzzwatch).
func fuzzWatchdog(fn func()) { fuzzwatch.Run(time.Minute, 0, fn) }
