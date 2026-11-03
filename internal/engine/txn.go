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
//     DB records and last_pgno from it. C's read-only handles take the
//     newest meta page instead while mti_txnid is 0; here they do not (rlmdb
//     KNOWN_C_BUGS.md row 45).
//
// Step 3 races with writers: a writer committing N+2 rewrites the meta
// page our N uses. C accepts the torn read; here mti_txnid is re-checked
// after the copy and the snapshot retried if it moved (KNOWN_C_BUGS.md
// row 31), so a process's snapshots never go back. The same holds for a copy
// that fails validation: only if mti_txnid moved can a commit have torn
// it, and then the snapshot is retried too (a bounded number of times,
// maxTornCopies); if it did not move, the page is bad and begin fails
// (rlmdb 3811bea). If the snapshot's pages extend past the map (another
// process grew the database beyond our map size), begin fails with
// MDB_MAP_RESIZED.

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
	// allocations is the pages a write transaction (with its committed
	// children) allocated so far, each run counted once: part of walkLimit.
	allocations uint64
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

	// owner changes at every begin: a Txn serves one transaction after
	// another (the environment's write Txn, a parent's spare child, read
	// Txns reused through BeginRead), and a handle remembering Owner from
	// its begin can tell that its transaction is over.
	owner uint64
}

// ReadOnly reports a read-only transaction.
func (t *Txn) ReadOnly() bool { return t.w == nil }

// ID is mdb_txn_id: the transaction id; for a read transaction the id of
// the snapshot it reads (0 after Reset).
func (t *Txn) ID() uint64 { return t.txnid }

// Env is the environment of the transaction (mdb_txn_env).
func (t *Txn) Env() *Env { return t.env }

// Owner identifies the transaction t serves: it changes when t begins
// another one.
func (t *Txn) Owner() uint64 { return t.owner }

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
	t.owner++
	t.env = e
	t.pageSize = e.pageSize
	t.l = e.layout
	t.dmap = nil
	t.txnid, t.nextPgno, t.allocations = 0, 0, 0
	t.dbs = t.dbs[:0]
	t.w = nil
	t.slot = -1
	t.state = txnDone
	// The map first: while t holds a reference on it, the environment is
	// not torn down (Close), so the lock region stays usable.
	m, err := e.pin()
	if err != nil {
		return err
	}
	t.dmap = m
	s, cell, err := e.acquireSlot(t.poolCell)
	t.poolCell = cell
	if err != nil {
		t.releaseMap()
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

// maxTornCopies is how many copies of a meta page that fail validation
// one snapshot makes before it gives up (rlmdb MAX_TORN_COPIES).
//
// A reader copies the meta page of the txnid it published (mti_txnid, or
// without a lock file the committed txnid the file's handles share). A
// commit rewrites that page only once the published txnid has moved past
// it, so a copy can be torn only if the txnid moved by the time the copy
// is done. A copy that fails validation is therefore retried if the txnid
// moved, as a consistent copy is, and reported if it did not: then the
// page really is bad. (Validation checks magic and version, which no
// writer, ours or C's, rewrites, so a torn copy passes it today; the rule
// keeps a check of rewritten fields from failing a begin on a race.)
const maxTornCopies = 16

// testHookSnapshotMeta, if set (tests only), copies and validates the
// meta page of a read transaction's snapshot instead of readMetaIn.
var testHookSnapshotMeta func(e *Env, m *sys.DataMap, i int) (format.Meta, error)

// snapshotMeta copies and validates meta page i for a read transaction's
// snapshot (readMetaIn, or the test hook).
func (e *Env) snapshotMeta(m *sys.DataMap, i int) (format.Meta, error) {
	if testHookSnapshotMeta != nil {
		return testHookSnapshotMeta(e, m, i)
	}
	return e.readMetaIn(m, i)
}

// takeSnapshot is the read-only branch of mdb_txn_renew0, through the
// transaction's map.
func (t *Txn) takeSnapshot() error {
	e := t.env
	m := t.dmap
	var txnid uint64
	var meta format.Meta
	var err error
	if !e.lockFile {
		if meta, err = e.noLockSnapshot(m, t.slot); err != nil {
			return err
		}
		return t.setSnapshot(meta.TxnID, &meta)
	}
	torn := 0
	for {
		// The snapshot mti_txnid names, also while it is 0, where C's
		// read-only handles take the newest meta page instead: that can be
		// the first commit's, written but not yet published, or failed, and
		// writers build on mti_txnid (rlmdb KNOWN_C_BUGS.md row 45).
		txnid = e.lock.PublishCurrentTxnID(t.slot)
		if meta, err = e.snapshotMeta(m, int(txnid&1)); err != nil {
			if torn+1 < maxTornCopies && e.lock.TxnID() != txnid {
				// Torn by a commit meanwhile (maxTornCopies): again.
				torn++
				continue
			}
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

// noLockSnapshot selects the snapshot of a read transaction without a
// lock file (MDB_NOLOCK, or a read-only file system) through the
// transaction's map m, and publishes its txnid in slot of the in-memory
// reader table. rlmdb's RoTxn::take_snapshot (NO_LOCK), as of its
// commit ade31f0.
//
// C takes the newest meta page (mdb_env_pick_meta), and may copy a page
// a writer of the process is rewriting: the roots of txn N with the txnid
// of N+2, which the reader would publish while reading N's pages, so that
// later writers reuse them. Here the protocol of the lock file
// (mdb_txn_renew0, see takeSnapshot) runs with an in-process stand-in for
// mti_txnid, the in-memory region's TxnID: this process's writers set it
// where they set mti_txnid (writeMeta), and before rewriting a meta page
// they set it to the snapshot they began from, on the other page, so it
// never names the page being rewritten. The reader publishes it in its
// slot, copies its meta page and re-reads it: if it did not move, no
// writer of this process touched the page during the copy, and every
// writer that may reuse the snapshot's pages (from txnid+3 on, see
// FindOldest) begins after the re-check and sees the slot.
//
// Commits of other processes are not published to us (MDB_NOLOCK leaves
// keeping processes apart to the caller, as in C), but a reader must see
// the ones made while this process had no transaction, as C's does. So a
// reader that finds a newer meta page on the file adopts its txnid if no
// writer of this process holds the writer lock (TryLockWriter: readers
// never wait; the lock of the in-memory region, which the writers of every
// NoLock handle of the file take) and starts over. With a writer, the snapshot it has is kept: the
// copy is consistent, and the writer publishes its commit, which builds
// on the newest one, when it is done. A plain copy holds the writer lock
// while its transaction renews, so it adopts the newest txnid first
// (copyPlain).
func (e *Env) noLockSnapshot(m *sys.DataMap, slot int) (format.Meta, error) {
	torn := 0
	for {
		txnid := e.lock.PublishCurrentTxnID(slot)
		meta, err := e.snapshotMeta(m, int(txnid&1))
		if err != nil {
			if torn+1 < maxTornCopies && e.lock.TxnID() != txnid {
				// Torn by a commit of ours meanwhile (maxTornCopies):
				// again.
				torn++
				continue
			}
			return meta, err
		}
		if e.lock.TxnID() != txnid {
			// A commit of ours meanwhile: again.
			continue
		}
		if !e.newerCommitOnFile(m, txnid, &meta) || !e.mem.TryLockWriter() {
			return meta, nil
		}
		adopted, err := e.adoptNewestTxnID(m)
		_ = e.mem.UnlockWriter()
		if err != nil {
			return meta, err
		}
		if !adopted {
			return meta, nil
		}
	}
}

// newerCommitOnFile reports whether the data file holds a commit newer
// than snapshot txnid, whose meta page was copied into meta: that page
// holds another one, or the other meta page a newer one. Only possible
// without a lock file, where other processes do not publish their commits
// to us. rlmdb's newer_commit_on_file.
func (e *Env) newerCommitOnFile(m *sys.DataMap, txnid uint64, meta *format.Meta) bool {
	if meta.TxnID != txnid {
		return true
	}
	_, other, ok := e.metaIDsIn(m, int(txnid&1)^1)
	return ok && other > txnid
}

// adoptNewestTxnIDPinned is adoptNewestTxnID through the current map.
func (e *Env) adoptNewestTxnIDPinned() error {
	m, err := e.pin()
	if err != nil {
		return err
	}
	defer m.Release()
	_, err = e.adoptNewestTxnID(m)
	return err
}

// adoptNewestTxnID makes the newest meta page's txnid the in-process
// committed txnid if it is newer (commits of other processes, without a
// lock file). The caller holds the in-memory region's writer lock, so no
// writer of this process (of any handle of the file) rewrites a meta page
// meanwhile.
func (e *Env) adoptNewestTxnID(m *sys.DataMap) (bool, error) {
	newest, err := e.pickMetaIn(m)
	if err != nil {
		return false, err
	}
	if newest.TxnID <= e.lock.TxnID() {
		return false, nil
	}
	e.lock.SetTxnID(newest.TxnID)
	return true, nil
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
	// C takes mm_last_pg + 1 as it is. Every snapshot holds the meta pages:
	// a smaller last page would make a writer hand out meta page 1 as a new
	// page and overwrite it at commit, the largest value page 0. The open
	// checks the meta page it reads, but a transaction may take one written
	// since (rlmdb KNOWN_C_BUGS.md row 49).
	if meta.LastPgno < format.NumMetas-1 || meta.LastPgno == ^uint64(0) {
		return format.Corrupted
	}
	maxPgno := t.maxPgno()
	if maxPgno < t.nextPgno {
		// A map shorter than the map size covers the file as it was when it
		// was made (a read-only environment on Windows, see sys.MapData); a
		// map of the file as it is now holds every page of the snapshot,
		// which were written before it was published. The file may still
		// end before nextPgno (pages allocated and freed again were never
		// written; the snapshot does not use them), so the limit is then
		// the map size, C's me_maxpg. (rlmdb TxnCore::renew.)
		m, ok, err := t.env.renewShortMap(t.dmap)
		if err != nil {
			return err
		}
		if ok {
			t.releaseMap()
			t.dmap = m
			maxPgno = uint64(t.env.mapSize.Load()) / uint64(t.pageSize)
		}
	}
	if maxPgno < t.nextPgno {
		return format.MapResized
	}
	return nil
}

// walkLimit is how many leaf pages a cursor may step onto in one direction
// before its walk counts as one over a damaged tree (see walk): the pages
// of the snapshot, plus the allocations of a write transaction, each of
// which may put a leaf ahead of a walking cursor again. No sound tree has
// more leaves than the snapshot has pages; a damaged tree overshoots it by
// powers of the fan-out.
func (t *Txn) walkLimit() uint64 { return t.nextPgno + t.allocations }

// maxPgno is me_maxpg for this transaction's map.
func (t *Txn) maxPgno() uint64 { return uint64(t.dmap.Len() / t.pageSize) }

// Reset is mdb_txn_reset: release the snapshot but keep the reader slot,
// so that Renew is cheap. Only for read transactions.
func (t *Txn) Reset() {
	if t.w != nil || t.state != txnActive {
		return
	}
	// The map stays: it keeps the environment, and so the slot, usable
	// after a Close until the transaction ends (Renew replaces it).
	if t.slot >= 0 {
		t.env.lock.ResetSlot(t.slot)
	}
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
	// The current map (SetMapSize may have replaced the one t kept),
	// taken before the old one is released.
	m, err := t.env.pin()
	if err != nil {
		return err
	}
	t.releaseMap()
	t.dmap = m
	if err := t.takeSnapshot(); err != nil {
		t.Abort()
		return err
	}
	t.state = txnActive
	return nil
}

// forgetPages drops the pages of the transaction's scratch cursors (and of
// a write transaction's), which point into its map, when the transaction
// gives the map up: the Txn object outlives it (package lmdb's pool of read
// transactions, the environment's write transaction, a spare child), and
// must not keep pointers into a map that may be unmapped (rcursor.forget).
func (t *Txn) forgetPages() {
	t.nameCur.forget()
	t.getCur.forget()
	t.getX.forget()
	if w := t.w; w != nil {
		w.rc.forget()
		w.rx.forget()
		w.reclaimCur.forget()
	}
}

// releaseMap releases the transaction's map, which may unmap it, after
// its scratch cursors have dropped their pages (rcursor.forget).
func (t *Txn) releaseMap() {
	t.forgetPages()
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
	if t.slot >= 0 {
		// Before the map: its release may tear the environment down.
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
