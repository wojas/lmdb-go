// Database handles: the environment's DBI table and each transaction's
// view of it (rlmdb core/dbi.rs).
//
// C keeps per-environment arrays indexed by MDB_dbi: me_dbxs (name and
// comparators), me_dbflags (persistent flags plus MDB_VALID) and
// me_dbiseqs (a sequence number bumped whenever a slot is closed or
// reused). Each transaction has mt_dbs (the MDB_db records of its
// snapshot) and mt_dbflags (DB_VALID, DB_STALE, ...). Slot 0 is the free
// DB, slot 1 the main DB; named databases live in the main DB as
// F_SUBDATA nodes.
//
//   - A handle's record is stale (DB_STALE) until the transaction first uses
//     it; it is then re-read from the transaction's own snapshot of the main
//     DB (the refresh in mdb_page_search). Changed flags (dropped and
//     recreated): MDB_INCOMPATIBLE; gone from the snapshot: MDB_BAD_DBI.
//   - A handle closed with Env.CloseDBI has its sequence number bumped;
//     transactions that cached it then get MDB_BAD_DBI.
//
// Deviation from C (rlmdb's): a handle opened in a read transaction is
// published to the environment at once and stays valid after the
// transaction ends, whether it commits or aborts.

package engine

import (
	"bytes"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// DBI is a database handle (MDB_dbi).
type DBI uint32

// The core databases.
const (
	FreeDBI DBI = format.FreeDBI // the free-page database
	MainDBI DBI = format.MainDBI // the main (unnamed) database
)

// mt_dbflags bits.
const (
	dbDirty    uint8 = 0x01 // DB_DIRTY: written in this txn; record saved at commit
	dbStale    uint8 = 0x02 // DB_STALE: record older than the txn
	dbNew      uint8 = 0x04 // DB_NEW: created by this write txn
	dbValid    uint8 = 0x08 // DB_VALID
	dbUsrValid uint8 = 0x10 // DB_USRVALID: valid for user ops (not FREE_DBI)
	dbDupData  uint8 = 0x20 // DB_DUPDATA: a DUPSORT sub-DB (mx_dbflag)
)

// dbiSlot is one slot of the environment's DBI table.
type dbiSlot struct {
	// name is md_name (with 1.0's NUL); nil for the core DBs and free
	// slots.
	name []byte
	// flags are the persistent flags.
	flags uint16
	// valid is MDB_VALID. A slot reserved by a write transaction for a
	// database it created stays invalid for others until the commit.
	valid      bool
	customCmp  format.CmpFunc
	customDcmp format.CmpFunc
}

// dbiRegistry is the environment's DBI table.
type dbiRegistry struct {
	mu    sync.RWMutex
	slots []dbiSlot // len is me_numdbs
	seqs  []atomic.Uint32
}

func (r *dbiRegistry) init(maxDBs uint32) {
	total := int(maxDBs) + format.CoreDBs
	r.slots = []dbiSlot{{valid: true}, {valid: true}}
	r.seqs = make([]atomic.Uint32, total)
}

// capacity is me_maxdbs.
func (r *dbiRegistry) capacity() int { return len(r.seqs) }

// seqOf is me_dbiseqs[i].
func (r *dbiRegistry) seqOf(i int) uint32 {
	if i < 0 || i >= len(r.seqs) {
		return ^uint32(0)
	}
	return r.seqs[i].Load()
}

// bumpSeq is ++me_dbiseqs[i].
func (r *dbiRegistry) bumpSeq(i int) uint32 { return r.seqs[i].Add(1) }

// nameOf is me_dbxs[i].md_name (a copy is not needed: names never change
// in place).
func (r *dbiRegistry) nameOf(i int) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i < len(r.slots) {
		return r.slots[i].name
	}
	return nil
}

// finishNew is mdb_dbis_update for one DB_NEW slot: publish it with flags
// (commit) or close it (abort).
func (r *dbiRegistry) finishNew(i int, keep bool, flags uint16) {
	if !keep {
		r.close(DBI(i))
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < len(r.slots) && r.slots[i].name != nil {
		r.slots[i].flags = uint16(uint32(flags) & format.PersistentFlags)
		r.slots[i].valid = true
	}
}

// close is mdb_dbi_close: forget a named handle; core and unknown handles
// are ignored, as in C.
func (r *dbiRegistry) close(dbi DBI) {
	i := int(dbi)
	if i < format.CoreDBs || i >= r.capacity() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < len(r.slots) && r.slots[i].name != nil {
		r.slots[i] = dbiSlot{}
		r.seqs[i].Add(1)
	}
}

// reopen reopens a handle closed by Drop in a nested transaction that
// aborted, in its old slot if that is still free; returns the slot's new
// sequence number.
func (r *dbiRegistry) reopen(i int, slot dbiSlot) (uint32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.slots) || r.slots[i].name != nil {
		return 0, false
	}
	r.slots[i] = slot
	return r.bumpSeq(i), true
}

// setComparator is mdb_set_compare / mdb_set_dupsort at the environment
// level.
func (r *dbiRegistry) setComparator(dbi DBI, cmp format.CmpFunc, dup bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := int(dbi)
	if i >= len(r.slots) || !r.slots[i].valid || dbi == FreeDBI {
		return syscall.EINVAL
	}
	if dup {
		r.slots[i].customDcmp = cmp
	} else {
		r.slots[i].customCmp = cmp
	}
	return nil
}

// comparatorsFor are the comparators of slot i with DB flags flags.
func comparatorsFor(slot *dbiSlot, i int, flags uint16) (key, dup format.Comparator, hasDup bool) {
	if i == format.FreeDBI {
		// me_dbxs[FREE_DBI].md_cmp = mdb_cmp_long
		return format.Long, format.Comparator{}, false
	}
	f := uint32(flags)
	if slot.customCmp != nil {
		key = format.Custom(slot.customCmp)
	} else {
		key = format.DefaultKeyCmp(f)
	}
	if f&format.DupSort != 0 {
		if slot.customDcmp != nil {
			return key, format.Custom(slot.customDcmp), true
		}
		dup, hasDup = format.DefaultDupCmp(f)
	}
	return key, dup, hasDup
}

// txnDB is a transaction's view of one DBI slot (mt_dbs[i],
// mt_dbflags[i], mt_dbiseqs[i], plus the comparators of mt_dbxs[i]).
type txnDB struct {
	// db is the MDB_db record (meaningful once not DB_STALE).
	db format.DB
	// state is mt_dbflags; 0 means "not looked at yet".
	state uint8
	// hasDup tells whether dupCmp is set (MDB_DUPSORT).
	hasDup bool
	// gen is the generation of the transaction the view belongs to; a
	// view of an older generation counts as not looked at (lazy reset).
	gen    uint32
	seq    uint32
	keyCmp format.Comparator
	dupCmp format.Comparator
	// nameID is a fingerprint of the database's name (0 for the core
	// DBs): tells whether a view and a registration are the same DB.
	nameID uint64
}

// dupOrder is the duplicate order of a MDB_DUPSORT database (for its
// sub-cursors).
func (e *txnDB) dupOrder() format.Comparator {
	if e.hasDup {
		return e.dupCmp
	}
	return format.Memn
}

// nameID is a fingerprint of a database name (FNV-1a, never 0).
func nameID(name []byte) uint64 {
	h := uint64(0xcbf29ce484222325)
	for _, b := range name {
		h ^= uint64(b)
		h *= 0x100000001b3
	}
	return h | 1
}

// checkDBName: names are C strings, arbitrary bytes without NUL (EINVAL).
func checkDBName(name []byte) error {
	if bytes.IndexByte(name, 0) >= 0 {
		return syscall.EINVAL
	}
	return nil
}

// dbNameKey is the key of the named database name in the main DB: the
// name in LMDB 0.9; in 1.0 the name with its terminating NUL.
func dbNameKey(l *format.Layout, name []byte) ([]byte, error) {
	if err := checkDBName(name); err != nil {
		return nil, err
	}
	if !l.DBNameNUL {
		return name, nil
	}
	key := make([]byte, len(name)+1)
	copy(key, name)
	return key, nil
}

// dbe is the transaction's entry for slot i, reset if it belongs to an
// earlier generation (the lazy mdb_txn_renew0 reset).
func (t *Txn) dbe(i int) *txnDB {
	e := &t.dbs[i]
	if e.gen != t.gen {
		*e = txnDB{db: format.EmptyDB(0), gen: t.gen}
		t.ndbs = max(t.ndbs, i+1)
	}
	return e
}

// initDBs fills the view for a transaction starting on snapshot coreDBs,
// as mdb_txn_renew0: the core records come from the meta page, every
// other slot is looked at lazily.
func (t *Txn) initDBs(coreDBs *[2]format.DB) {
	r := &t.env.dbis
	if cap(t.dbs) < r.capacity() {
		t.dbs = make([]txnDB, r.capacity())
	}
	t.dbs = t.dbs[:r.capacity()]
	t.ndbs = 0
	t.gen++
	if t.gen == 0 {
		// The generation wrapped: forget every view for real.
		clear(t.dbs)
		t.gen = 1
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := 0; i < format.CoreDBs; i++ {
		e := t.dbe(i)
		e.db = coreDBs[i]
		e.keyCmp, e.dupCmp, e.hasDup = comparatorsFor(&r.slots[i], i, coreDBs[i].Flags)
		e.state = dbValid | dbUsrValid
		if i == format.FreeDBI {
			e.state = dbValid
		}
		e.seq = r.seqOf(i)
	}
}

// dbiEntry is TXN_DBI_EXIST(txn, dbi, validity) plus TXN_DBI_CHANGED: the
// transaction's entry for dbi, taking a (stale) view of the environment
// slot the first time. EINVAL if not valid, MDB_BAD_DBI if closed or
// reused since.
func (t *Txn) dbiEntry(dbi DBI, validity uint8) (*txnDB, error) {
	i := int(dbi)
	if i >= len(t.dbs) {
		return nil, syscall.EINVAL
	}
	e := t.dbe(i)
	if e.state == 0 {
		if err := t.firstUse(i, e); err != nil {
			return nil, err
		}
	}
	if e.state&validity == 0 {
		return nil, syscall.EINVAL
	}
	if e.seq != t.env.dbis.seqOf(i) {
		return nil, format.BadDBI
	}
	return e, nil
}

// firstUse takes a stale view of the environment's slot i.
func (t *Txn) firstUse(i int, e *txnDB) error {
	r := &t.env.dbis
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i >= len(r.slots) || !r.slots[i].valid {
		return syscall.EINVAL
	}
	s := &r.slots[i]
	e.db = format.EmptyDB(s.flags)
	e.state = dbValid | dbUsrValid | dbStale
	e.seq = r.seqOf(i)
	e.keyCmp, e.dupCmp, e.hasDup = comparatorsFor(s, i, s.flags)
	e.nameID = 0
	if s.name != nil {
		e.nameID = nameID(s.name)
	}
	return nil
}

// freshDBI is dbiEntry with a stale record refreshed from the
// transaction's snapshot of the main DB.
func (t *Txn) freshDBI(dbi DBI, validity uint8) (*txnDB, error) {
	e, err := t.dbiEntry(dbi, validity)
	if err != nil {
		return nil, err
	}
	return e, t.refreshed(dbi, e)
}

// refreshed is the refresh of freshDBI for an entry dbiEntry returned.
func (t *Txn) refreshed(dbi DBI, e *txnDB) error {
	if e.state&dbStale == 0 {
		return nil
	}
	return t.refreshStale(int(dbi), e)
}

// refreshStale is the DB_STALE branch of mdb_page_search: re-read a named
// DB's record from the main DB of this snapshot.
func (t *Txn) refreshStale(i int, e *txnDB) error {
	r := &t.env.dbis
	r.mu.RLock()
	if e.seq != r.seqOf(i) || i >= len(r.slots) || r.slots[i].name == nil {
		r.mu.RUnlock()
		return format.BadDBI
	}
	name := r.slots[i].name
	r.mu.RUnlock()
	db, found, err := t.findNamedDB(name)
	if err != nil {
		return err
	}
	if !found {
		return format.BadDBI
	}
	// The txn may not know this DBI, or another process may have dropped
	// and recreated the DB with other flags.
	if uint32(e.db.Flags)&format.PersistentFlags != uint32(db.Flags) {
		return format.Incompatible
	}
	e.db = db
	e.state &^= dbStale
	return nil
}

// findNamedDB searches the main DB for name and returns its MDB_db record
// if it is a named DB. MDB_NOTFOUND if the main DB is empty; found false
// if there is no exact match; MDB_INCOMPATIBLE if the key holds plain
// data.
func (t *Txn) findNamedDB(name []byte) (db format.DB, found bool, err error) {
	// mdb_cursor_set: a zero-length key (DB name) is invalid.
	if len(name) == 0 {
		return db, false, format.BadValSize
	}
	main := t.dbe(format.MainDBI)
	mc := &t.nameCur
	mc.init(main.db, main.keyCmp, 0, t.l)
	if err := mc.pageSearch(t, name, 0); err != nil {
		return db, false, err
	}
	f, exact, err := mc.nodeSearch(name)
	if err != nil {
		return db, false, err
	}
	if !f || !exact {
		return db, false, nil
	}
	leaf, err := mc.currentNode()
	if err != nil {
		return db, false, err
	}
	if leaf.Flags()&(format.FDupData|format.FSubData) != format.FSubData {
		return db, false, format.Incompatible
	}
	data, err := nodeRead(t, leaf)
	if err != nil {
		return db, false, err
	}
	db, ok := format.ReadDB(data)
	if !ok {
		return db, false, format.Corrupted
	}
	return db, true, nil
}

// openDB is mdb_dbi_open without MDB_CREATE: the handle of an existing
// database (nil name: the main DB).
func (t *Txn) openDB(name []byte) (DBI, error) {
	if name == nil {
		return MainDBI, nil
	}
	key, err := dbNameKey(t.l, name)
	if err != nil {
		return 0, err
	}
	r := &t.env.dbis
	r.mu.Lock()
	unused := -1
	for i := format.CoreDBs; i < len(r.slots); i++ {
		s := &r.slots[i]
		switch {
		case s.name == nil:
			if unused < 0 {
				unused = i
			}
		case bytes.Equal(s.name, key):
			r.mu.Unlock()
			t.adoptRegistration(i, key)
			return DBI(i), nil
		}
	}
	if dbi, ok := t.reopenLiveView(key); ok {
		r.mu.Unlock()
		return dbi, nil
	}
	if unused < 0 && len(r.slots) >= r.capacity() {
		r.mu.Unlock()
		return 0, format.DBsFull
	}
	// Cannot mix named databases with some main DB flags.
	main := t.dbe(format.MainDBI)
	if uint32(main.db.Flags)&(format.DupSort|format.IntegerKey) != 0 {
		r.mu.Unlock()
		return 0, format.NotFound
	}
	db, found, err := t.findNamedDB(key)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	if !found {
		r.mu.Unlock()
		return 0, format.NotFound
	}
	i := r.register(unused, dbiSlot{
		name:  key,
		flags: uint16(uint32(db.Flags) & format.PersistentFlags),
		valid: true,
	})
	seq := r.bumpSeq(i)
	e := t.dbe(i)
	e.keyCmp, e.dupCmp, e.hasDup = comparatorsFor(&r.slots[i], i, db.Flags)
	r.mu.Unlock()
	e.db = db
	e.state = dbValid | dbUsrValid
	e.seq = seq
	e.nameID = nameID(key)
	return DBI(i), nil
}

// register stores slot in the free slot unused (or a new one) and returns
// its index. The caller holds r.mu.
func (r *dbiRegistry) register(unused int, slot dbiSlot) int {
	if unused >= 0 {
		r.slots[unused] = slot
		return unused
	}
	r.slots = append(r.slots, slot)
	return len(r.slots) - 1
}

// reopenLiveView: name is not open in the environment, but this
// transaction may still have a live view of it in a slot whose handle was
// closed under it (Drop in a nested transaction, or CloseDBI). Opening it
// again keeps that view: within a write transaction the record in the main
// DB is only written back at commit, so reading it would lose the
// transaction's changes (C does, KNOWN_C_BUGS.md #19). The caller holds
// the registry lock.
func (t *Txn) reopenLiveView(name []byte) (DBI, bool) {
	r := &t.env.dbis
	id := nameID(name)
	for i := format.CoreDBs; i < t.ndbs; i++ {
		e := t.dbe(i)
		if e.state&dbValid == 0 || e.nameID != id || (i < len(r.slots) && r.slots[i].name != nil) {
			continue
		}
		slot := dbiSlot{
			name:  name,
			flags: uint16(uint32(e.db.Flags) & format.PersistentFlags),
			valid: e.state&dbNew == 0,
		}
		switch {
		case i < len(r.slots):
			r.slots[i] = slot
		case i == len(r.slots):
			r.slots = append(r.slots, slot)
		default:
			return 0, false
		}
		e.seq = r.bumpSeq(i)
		return DBI(i), true
	}
	return 0, false
}

// adoptRegistration: name is open in the environment in slot i. If this
// transaction has a usable view of the same database from an earlier
// generation of the slot (closed by Drop in a nested transaction that
// aborted, or registered again by one), it carries over; a view of a
// database this transaction dropped, or of another name, is forgotten.
// Returns false in that case.
func (t *Txn) adoptRegistration(i int, name []byte) bool {
	seq := t.env.dbis.seqOf(i)
	e := t.dbe(i)
	if e.state == 0 || e.seq == seq {
		return true
	}
	if e.state&dbValid != 0 && e.nameID == nameID(name) {
		e.seq = seq
		return true
	}
	*e = txnDB{db: format.EmptyDB(0), gen: t.gen}
	return false
}

// setComparator is mdb_set_compare / mdb_set_dupsort with a transaction:
// install cmp for the handle (environment-wide, as C's me_dbxs) and use it
// in this transaction from now on.
func (t *Txn) setComparator(dbi DBI, cmp format.CmpFunc, dup bool) error {
	e, err := t.dbiEntry(dbi, dbUsrValid)
	if err != nil {
		return err
	}
	r := &t.env.dbis
	r.mu.Lock()
	defer r.mu.Unlock()
	i := int(dbi)
	if i >= len(r.slots) {
		return syscall.EINVAL
	}
	if dup {
		r.slots[i].customDcmp = cmp
	} else {
		r.slots[i].customCmp = cmp
	}
	e.keyCmp, e.dupCmp, e.hasDup = comparatorsFor(&r.slots[i], i, e.db.Flags)
	return nil
}

// DBIFlags is mdb_dbi_flags: the persistent flags of a user DB.
func (t *Txn) DBIFlags(dbi DBI) (uint32, error) {
	e, err := t.dbiEntry(dbi, dbUsrValid)
	if err != nil {
		return 0, err
	}
	return uint32(e.db.Flags) & format.PersistentFlags, nil
}
