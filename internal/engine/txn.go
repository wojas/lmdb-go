// Transactions: the snapshot, page lookup, and read transactions (rlmdb
// core/txn.rs; mdb_txn_begin, mdb_txn_renew0, mdb_txn_reset,
// mdb_txn_abort, mdb_page_get, mdb_get).
//
// Beginning a read transaction (mdb_txn_renew0, mdb.c line 2758):
//
//  1. Take a reader slot: an idle one this process owns from the pool
//     (slotpool.go), else claim one (LockRegion.AcquireSlot). As with
//     MDB_NOTLS, every transaction has its own slot; Reset keeps it for
//     Renew, the end makes it idle and pools it (C's MDB_NOTLS releases
//     it).
//  2. Publish the last committed txnid (mti_txnid) in the slot, re-reading
//     until stable. From then on writers do not reuse pages this snapshot
//     needs.
//  3. Read the meta page of that txnid (txnid & 1) and take the free/main
//     DB records and last_pgno from it.
//
// Step 3 races with writers: a writer committing N+2 rewrites the meta
// page our N uses. C accepts the torn read; here mti_txnid is re-checked
// after the copy and the snapshot retried if it moved (KNOWN_C_BUGS.md
// #31), so a process's snapshots never go back. If the snapshot's pages
// extend past the map (another process grew the database beyond our map
// size), begin fails with MDB_MAP_RESIZED.

package engine

import (
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// Read transaction states.
const (
	txnActive = iota
	txnReset
	txnDone
)

// Txn is a transaction (MDB_txn): a read-only one, or a write transaction
// (w != nil), possibly nested. A Txn is used by one goroutine at a time.
// Read transactions are never bound to a thread (MDB_NOTLS semantics). A
// write transaction must end on the OS thread that began it (the Linux
// writer mutex is thread-owned).
type Txn struct {
	env      *Env
	pageSize int
	l        *format.Layout
	// dmap is the data map current when the transaction began; the
	// transaction holds a reference on it.
	dmap *sys.DataMap
	// txnid is mt_txnid.
	txnid uint64
	// nextPgno is mt_next_pgno: the first page not in this snapshot (write
	// transactions: the first page never allocated).
	nextPgno uint64
	// dbs is mt_dbs + mt_dbflags (+ seqs and comparators), one per slot.
	dbs []txnDB
	// gen is the generation of dbs (see Txn.dbe).
	gen uint32
	// ndbs is mt_numdbs: dbs[ndbs:] has not been looked at in this
	// generation, so loops over the table stop there (dbs is me_maxdbs
	// long).
	ndbs int

	// Read transactions: the reader slot (-1: none) and the state.
	slot  int
	state int
	// poolCell is the cell of Env.slots this transaction object used
	// last (see slotPool.get); kept when the object is reused.
	poolCell int

	// nameCur is the scratch cursor of findNamedDB, getCur and getX those
	// of Get.
	nameCur rcursor
	getCur  rcursor
	getX    rcursor

	// w is the write state of a write transaction (nil for read).
	w *wstate
}

// ReadOnly reports a read-only transaction.
func (t *Txn) ReadOnly() bool { return t.w == nil }

// ID is mdb_txn_id: the transaction id; for a read transaction the id of
// the snapshot it reads (0 after Reset).
func (t *Txn) ID() uint64 { return t.txnid }

// Env is the environment of the transaction (mdb_txn_env).
func (t *Txn) Env() *Env { return t.env }

// BeginTxn is mdb_txn_begin. flags may hold MDB_RDONLY for a read
// transaction; parent is a write transaction for a nested one (nil for a
// top-level transaction). Errors as C: EACCES for a write transaction in a
// read-only environment, EINVAL for a read-only parent, MDB_BAD_TXN for a
// read-only child or a parent that failed or has a child already.
func (e *Env) BeginTxn(parent *Txn, flags uint32) (*Txn, error) {
	if e.isReadOnly() && flags&format.ReadOnly == 0 {
		return nil, syscall.EACCES
	}
	if parent != nil {
		// Nested transactions: max 1 child, write txns only.
		if parent.w == nil {
			return nil, syscall.EINVAL
		}
		if flags&format.ReadOnly != 0 {
			return nil, format.BadTxn
		}
		return parent.beginNested()
	}
	if flags&format.ReadOnly != 0 {
		t := new(Txn)
		if err := e.BeginRead(t); err != nil {
			return nil, err
		}
		return t, nil
	}
	return e.beginWrite()
}

// BeginRead begins a read transaction in t, reusing its memory (t may be
// a zero Txn or one that ended). It is mdb_txn_begin(MDB_RDONLY) without
// allocation once t has been used.
func (e *Env) BeginRead(t *Txn) error {
	// Field by field: the scratch cursors need no zeroing (they are
	// initialised before each use), and zeroing them was a measurable
	// part of a begin.
	t.env = e
	t.pageSize = e.pageSize
	t.l = e.layout
	t.dmap = nil
	t.txnid, t.nextPgno = 0, 0
	t.dbs = t.dbs[:0]
	t.w = nil
	t.slot = -1
	t.state = txnDone
	if e.closed.Load() {
		return syscall.EINVAL
	}
	s, cell, err := e.acquireSlot(t.poolCell)
	t.poolCell = cell
	if err != nil {
		return err
	}
	t.slot = s
	if err := t.takeSnapshot(); err != nil {
		t.Abort()
		return err
	}
	t.state = txnActive
	return nil
}

// takeSnapshot is the read-only branch of mdb_txn_renew0.
func (t *Txn) takeSnapshot() error {
	e := t.env
	m, err := e.acquireMap()
	if err != nil {
		return err
	}
	t.dmap = m
	var txnid uint64
	var meta format.Meta
	if !e.lockFile {
		// No lock file: C takes the newest meta page (another process may
		// have committed under MDB_NOLOCK). The slot of the in-memory table
		// is published first and the meta checked again, as with the lock
		// file's mti_txnid, so a writer of this process never reuses pages
		// of this snapshot.
		for {
			if meta, err = e.pickMetaIn(m); err != nil {
				return err
			}
			e.lock.SetSlotTxnID(t.slot, meta.TxnID)
			again, err := e.pickMetaIn(m)
			if err != nil {
				return err
			}
			if again.TxnID == meta.TxnID {
				break
			}
		}
		txnid = meta.TxnID
		return t.setSnapshot(txnid, &meta)
	}
	for {
		txnid = e.lock.PublishCurrentTxnID(t.slot)
		if txnid == 0 && e.isReadOnly() {
			if meta, err = e.pickMetaIn(m); err != nil {
				return err
			}
			e.lock.SetSlotTxnID(t.slot, meta.TxnID)
			txnid = meta.TxnID
			break
		}
		if meta, err = e.readMetaIn(m, int(txnid&1)); err != nil {
			return err
		}
		// If no commit happened while we copied the meta page, no writer
		// can have been rewriting it.
		if e.lock.TxnID() == txnid {
			break
		}
	}
	return t.setSnapshot(txnid, &meta)
}

// setSnapshot installs the snapshot described by meta (the tail of
// mdb_txn_renew0).
func (t *Txn) setSnapshot(txnid uint64, meta *format.Meta) error {
	t.txnid = txnid
	t.nextPgno = meta.LastPgno + 1
	t.initDBs(&meta.DBs)
	if t.env.fatal.Load() {
		return format.Panic
	}
	if t.maxPgno() < t.nextPgno {
		return format.MapResized
	}
	return nil
}

// maxPgno is me_maxpg for this transaction's map.
func (t *Txn) maxPgno() uint64 { return uint64(t.dmap.Len() / t.pageSize) }

// Reset is mdb_txn_reset: release the snapshot but keep the reader slot,
// so that Renew is cheap. Only for read transactions.
func (t *Txn) Reset() {
	if t.w != nil || t.state != txnActive {
		return
	}
	if t.slot >= 0 && !t.env.closed.Load() {
		t.env.lock.ResetSlot(t.slot)
	}
	t.releaseMap()
	t.txnid = 0
	t.state = txnReset
}

// Renew is mdb_txn_renew: read the latest snapshot again, reusing the
// reader slot. MDB_BAD_RSLOT if the slot was taken (e.g. cleared by a
// reader check in another process); EINVAL if t is not a reset read
// transaction.
func (t *Txn) Renew() error {
	if t.w != nil || t.state != txnReset || t.env.closed.Load() {
		return syscall.EINVAL
	}
	if t.slot >= 0 && !t.env.lock.SlotIsIdleAndOurs(t.slot) {
		// Not ours any more: do not release it at the end.
		t.slot = -1
		t.state = txnDone
		return format.BadRSlot
	}
	if err := t.takeSnapshot(); err != nil {
		t.Abort()
		return err
	}
	t.state = txnActive
	return nil
}

func (t *Txn) releaseMap() {
	if t.dmap != nil {
		t.dmap.Release()
		t.dmap = nil
	}
}

// Abort is mdb_txn_abort: end the transaction, discarding the changes of
// a write transaction and releasing the reader slot of a read one.
func (t *Txn) Abort() {
	if t.w != nil {
		t.abortWrite()
		return
	}
	if t.state == txnDone && t.slot < 0 && t.dmap == nil {
		return
	}
	if t.slot >= 0 && !t.env.closed.Load() {
		t.poolCell = t.env.releaseSlot(t.slot, t.poolCell)
	}
	t.slot = -1
	t.releaseMap()
	t.state = txnDone
}

// Commit is mdb_txn_commit. For a read transaction it ends it (handles
// opened in it stay valid).
func (t *Txn) Commit() error {
	if t.w != nil {
		return t.commitWrite()
	}
	if t.state == txnDone {
		return syscall.EINVAL
	}
	t.Abort()
	return nil
}

// readUsable checks a read transaction is active (EINVAL otherwise, C's
// MDB_TXN_BLOCKED for a reset transaction is MDB_BAD_TXN).
func (t *Txn) usable() error {
	if t.w != nil {
		return t.checkUsable()
	}
	if t.state != txnActive {
		return format.BadTxn
	}
	return nil
}

// --- page lookup ------------------------------------------------------------

// page is mdb_page_get: page pgno of this snapshot: a dirty page of this
// transaction or an ancestor, else the mapped page. Meta pages are never
// returned (they change under readers), nor pages past the snapshot's end
// (MDB_PAGE_NOTFOUND).
func (t *Txn) page(pgno uint64) (format.Page, error) {
	if t.w != nil {
		if b := t.dirtyBuf(pgno); b != nil {
			return pageOf(b[:t.pageSize], t.l)
		}
	}
	return t.mappedPage(pgno)
}

func pageOf(b []byte, l *format.Layout) (format.Page, error) {
	p, ok := format.NewPage(b, l)
	if !ok {
		return p, format.Corrupted
	}
	return p, nil
}

// mappedPage is the mapped page pgno of the snapshot.
func (t *Txn) mappedPage(pgno uint64) (format.Page, error) {
	if pgno < format.NumMetas {
		return format.Page{}, format.Corrupted
	}
	if pgno >= t.nextPgno || pgno > uint64(1<<62)/uint64(t.pageSize) {
		return format.Page{}, format.PageNotFound
	}
	b, ok := t.dmap.Slice(int(pgno)*t.pageSize, t.pageSize)
	if !ok {
		return format.Page{}, format.PageNotFound
	}
	return format.Page{B: b, L: t.l}, nil
}

// overflowData is the data of an F_BIGDATA node: n bytes right after the
// header of the overflow run at pgno (mdb_node_read).
func (t *Txn) overflowData(pgno uint64, n int) ([]byte, error) {
	ps := t.pageSize
	npages := uint64(t.l.OverflowPagesFor(n, ps))
	var b []byte
	if t.w != nil {
		b = t.dirtyBuf(pgno)
	}
	if b == nil {
		if pgno < format.NumMetas {
			return nil, format.Corrupted
		}
		end := pgno + npages
		if end < pgno || end > t.nextPgno || end > uint64(1<<62)/uint64(ps) {
			return nil, format.PageNotFound
		}
		var ok bool
		if b, ok = t.dmap.Slice(int(pgno)*ps, int(npages)*ps); !ok {
			return nil, format.PageNotFound
		}
	}
	p, ok := format.NewPage(b, t.l)
	if !ok || !p.IsOverflow() {
		return nil, format.Corrupted
	}
	d, ok := p.OverflowData(n)
	if !ok {
		return nil, format.Corrupted
	}
	return d, nil
}

// --- reads -------------------------------------------------------------------

// Get is mdb_get: the value stored under key, or MDB_NOTFOUND. For
// DUPSORT databases, the first data item of the key. In a write
// transaction the value may live in a dirty page and is valid until the
// next change.
func (t *Txn) Get(dbi DBI, key []byte) ([]byte, error) {
	if err := t.usable(); err != nil {
		return nil, err
	}
	e, err := t.freshDBI(dbi, dbUsrValid)
	if err != nil {
		return nil, err
	}
	mc, mx := &t.getCur, &t.getX
	mc.init(e.db, e.keyCmp, 0, t.l)
	if !newXCursor(mx, e, t.l) {
		mx = nil
	}
	_, data, _, err := cursorSet(t, mc, mx, e.dupCmp, e.hasDup, key, nil, false, format.Set, true)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// OpenDBI is mdb_dbi_open: the handle of database name (nil: the main
// DB). In a write transaction flags may hold MDB_CREATE and DB flags (see
// createDB); in a read transaction the database must exist (MDB_NOTFOUND
// otherwise).
func (t *Txn) OpenDBI(name []byte, flags uint32) (DBI, error) {
	if t.w != nil {
		return t.createDB(name, flags)
	}
	if flags&^format.ValidDBFlags != 0 {
		return 0, syscall.EINVAL
	}
	if err := t.usable(); err != nil {
		return 0, err
	}
	dbi, err := t.openDB(name)
	if err == format.NotFound && flags&format.Create != 0 {
		// As C (rlmdb-capi): a main DB with these flags cannot hold named
		// DBs; otherwise creating needs a write transaction.
		if uint32(t.dbe(format.MainDBI).db.Flags)&(format.DupSort|format.IntegerKey) != 0 {
			return 0, format.Incompatible
		}
		return 0, syscall.EACCES
	}
	return dbi, err
}

// Stat is mdb_stat for a database of this transaction.
func (t *Txn) Stat(dbi DBI) (Stat, error) {
	if _, err := t.dbiEntry(dbi, dbValid); err != nil {
		return Stat{}, err
	}
	if err := t.usable(); err != nil {
		return Stat{}, err
	}
	e, err := t.freshDBI(dbi, dbValid)
	if err != nil {
		return Stat{}, err
	}
	return statOf(t.pageSize, &e.db), nil
}

// SetCompare is mdb_set_compare with a transaction: as in C the setting
// belongs to the environment's handle, and it takes effect in this
// transaction at once (nil: the default order). EINVAL for the free DB.
func (t *Txn) SetCompare(dbi DBI, cmp format.CmpFunc) error { return t.setComparator(dbi, cmp, false) }

// SetDupsort is mdb_set_dupsort with a transaction.
func (t *Txn) SetDupsort(dbi DBI, cmp format.CmpFunc) error { return t.setComparator(dbi, cmp, true) }

// Comparators are the comparators this transaction uses for dbi (what
// mdb_cmp and mdb_dcmp call); hasDup is false unless DUPSORT.
func (t *Txn) Comparators(dbi DBI) (key, dup format.Comparator, hasDup bool, err error) {
	e, err := t.dbiEntry(dbi, dbValid)
	if err != nil {
		return key, dup, false, err
	}
	return e.keyCmp, e.dupCmp, e.hasDup, nil
}
