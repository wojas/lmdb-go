// Write transactions (rlmdb core/txn_rw.rs).
//
//   - beginWrite = mdb_txn_begin + the write branch of mdb_txn_renew0 (line
//     2758): lock the in-process writer state and the shared writer mutex,
//     take the last committed txnid from the lock file, read its meta
//     page, and start txn last+1 from it.
//   - Changes go to owned page copies (dirty.go); every read of the
//     transaction sees them because Txn.page looks there first.
//   - Commit = mdb_txn_commit (commit.go): write named DB records into the
//     main DB, save the free list, write the dirty pages, sync, write the
//     meta page, publish the txnid.
//   - Abort = mdb_txn_abort / mdb_txn_end: discard the dirty pages, close
//     handles of databases created by the transaction, unlock.
//
// A write transaction must end on the OS thread that began it (the Linux
// writer mutex is thread-owned); the caller locks the goroutine to its
// thread (runtime.LockOSThread) for the transaction's life.

package engine

import (
	"bytes"
	"errors"
	"fmt"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// mt_flags of a write transaction.
const (
	txnFinished uint32 = 0x01 // MDB_TXN_FINISHED
	txnError    uint32 = 0x02 // MDB_TXN_ERROR: only abort is allowed
	txnDirty    uint32 = 0x04 // MDB_TXN_DIRTY: write a meta page even without dirty pages
	txnSpills   uint32 = 0x08 // MDB_TXN_SPILLS: the txn (or a parent) spilled
	txnHasChild uint32 = 0x10 // MDB_TXN_HAS_CHILD
	// txnBlocked is MDB_TXN_BLOCKED: most operations are illegal.
	txnBlocked = txnFinished | txnError | txnHasChild
)

// ErrWriterUnlock wraps the error of the writer lock's unlock at the end of
// a top-level write transaction (see Txn.end): with the thread-owned locks
// (Linux and FreeBSD robust mutexes, Windows mutexes) the transaction ended
// on another OS thread than the one that began it.
var ErrWriterUnlock = errors.New("writer lock not released")

// droppedDBI is a handle closed by Drop in this transaction (or a
// committed child): slot index, the environment slot and its sequence
// number before the close, for an aborting nested transaction to reopen.
type droppedDBI struct {
	i    int
	slot dbiSlot
	seq  uint32
}

// wstate is the write-only part of MDB_txn, plus the free-page state C
// keeps in MDB_env while a writer runs (me_pghead, me_pglast).
type wstate struct {
	flags uint32
	// freePgs is mt_free_pgs: pages this transaction stopped using,
	// unsorted until mdb_freelist_save writes them.
	freePgs format.IDL
	// loose is mt_loose_pgs: dirty pages freed again, reused first (last
	// freed first); they stay in the dirty list, marked P_LOOSE.
	loose []uint64
	// dirtyRoom is mt_dirty_room.
	dirtyRoom int
	// pghead is me_pghead: pages reclaimed from the free DB, descending;
	// nil until the first record is read (C's NULL).
	pghead format.IDL
	// pglast is me_pglast.
	pglast uint64
	// pgOldest is me_pgoldest, copied from the environment.
	pgOldest uint64
	// pbufPad is me_pbuf->mp_pad (see writerState).
	pbufPad uint16
	// cursors are the cursors of this transaction (mt_cursors); a nested
	// transaction shares its parent's arena.
	cursors *arena
	// baseMain is the main DB record of the snapshot a top-level
	// transaction began on (to tell which named DBs exist again after an
	// abort).
	baseMain    format.DB
	hasBaseMain bool
	// baseMapsize is mm_mapsize of the meta page the transaction began
	// from.
	baseMapsize uint64
	dropped     []droppedDBI
	// dirty is mt_u.dirty_list.
	dirty *dirtyPages
	// spilled is mt_spill_pgs.
	spilled spillSet
	// parents are, for a nested transaction, the dirty and spilled pages
	// of its parent, grandparent, ... (nearest first).
	parents []layer
	// parent is the parent of a nested transaction, child the active
	// nested transaction of this one, spareChild the memory of its last
	// child, for the next one (beginNested).
	parent     *Txn
	child      *Txn
	spareChild *Txn
	// cursorBackup is the parent's cursors as they were when this nested
	// transaction began (mc_backup).
	cursorBackup []backupSlot
	// rc, rx are the scratch read cursors of readOp; reclaimCur that of
	// reclaimRun.
	rc, rx     rcursor
	reclaimCur rcursor
	// pageWriter writes the batches of pages, runs is the list of the
	// pages to write.
	pageWriter sys.PageWriter
	runs       []flushEntry
	// metaBuf is the meta page writeMeta writes. It lives here because
	// os.File.WriteAt keeps a reference to its buffer on Windows: a local
	// array escaped to the heap there, one allocation per commit.
	metaBuf [format.MetaSize]byte
	// pgheadSpare is the memory of the last pghead (C frees me_pghead at
	// the end of each transaction), mopBuf and idsBuf freelistSave's
	// scratch buffers.
	pgheadSpare format.IDL
	mopBuf      []uint64
	idsBuf      []byte
	// reclaimKey and saveKey hold the free DB keys of reclaimRun and
	// freelistSave (see txnidBytes).
	reclaimKey, saveKey [8]byte
	// splitPtrs are the node offsets of pageSplit, by recursion depth
	// (splitDepth: one level per tree level).
	splitPtrs  [format.CursorStack][]int
	splitDepth int
	// scratch holds the temporary page copies of splits and merges.
	scratch scratchStack
	// Key and data scratch of the rebalancing code: lowestKeyBelow's
	// result, nodeMove's moved key and data and its new separators. Each
	// holds one value at a time (nodeMove does not recurse, and a key is
	// used before the next one is read).
	lowKey, moveKey, moveData, sepKey []byte
	// dupOld, dupSub and dupRec are the scratch of a DUPSORT put
	// (dupPrepare; C's me_pbuf): the node's old data, the sub-page being
	// built, the record of a new sub-DB. One put uses them at a time: the
	// sub-tree it then writes to has no duplicates.
	dupOld, dupSub []byte
	dupRec         [format.DBSize]byte
	// unlock releases the writer locks (top-level transactions).
	locked bool
}

// beginWrite is mdb_txn_begin(env, NULL, 0). The transaction is the
// environment's writerState.txn, reused by every top-level write
// transaction as C reuses me_txn0: callers must not use a Txn after its
// Commit or Abort (package lmdb drops its reference then, as lmdb-go did
// with C's).
func (e *Env) beginWrite() (*Txn, error) {
	if e.isReadOnly() {
		return nil, syscall.EACCES
	}
	// Keeps the environment (its lock region) from being torn down by a
	// concurrent Close until the transaction holds its own map.
	pin, err := e.pin()
	if err != nil {
		return nil, err
	}
	defer pin.Release()
	e.writerMu.Lock()
	if err := e.lockWriter(pin); err != nil {
		e.writerMu.Unlock()
		return nil, err
	}
	unlock := func() {
		_ = e.lock.UnlockWriter() // same goroutine as LockWriter: cannot fail
		e.writerMu.Unlock()
	}
	// The current map: SetMapSize may have replaced the pinned one before
	// the writer lock was ours; Close may have happened (EINVAL).
	m, err := e.pin()
	if err != nil {
		unlock()
		return nil, err
	}
	var last uint64
	var meta format.Meta
	if e.lockFile {
		last = e.lock.TxnID()
		if e.writer.hasLastMeta && e.writer.lastMeta.TxnID == last {
			// This process wrote the newest meta page.
			meta = e.writer.lastMeta
		} else {
			meta, err = e.readMetaIn(m, int(last&1))
		}
	} else {
		meta, err = e.pickMetaIn(m)
		last = meta.TxnID
	}
	if err != nil {
		m.Release()
		unlock()
		return nil, err
	}
	t := e.writer.txn
	if t == nil {
		t = &Txn{w: &wstate{cursors: &arena{}, dirty: newDirtyPages(e.pageSize), spilled: spillSet{}}}
		e.writer.txn = t
	}
	// Field by field: dbs and gen are reused, the scratch cursors are
	// initialised before each use.
	t.env, t.pageSize, t.l, t.dmap = e, e.pageSize, e.layout, m
	t.slot, t.state = -1, txnActive
	t.owner++
	if err := t.setSnapshot(last+1, &meta); err != nil {
		t.releaseMap()
		unlock()
		return nil, err
	}
	// The arena, dirty list and spill set were emptied by the previous
	// transaction's end.
	w := t.w
	w.flags = 0
	w.freePgs = w.freePgs[:0]
	w.loose = w.loose[:0]
	w.dirtyRoom = int(e.dirtyLimit.Load())
	w.pghead, w.pglast = nil, 0
	w.pgOldest = e.writer.pgOldest
	w.pbufPad = e.writer.pbufPad
	w.baseMain = meta.DBs[format.MainDBI]
	w.hasBaseMain = true
	w.baseMapsize = meta.MapSize
	clear(w.dropped)
	w.dropped = w.dropped[:0]
	w.parents, w.parent, w.child, w.cursorBackup = nil, nil, nil, nil
	w.locked = true
	return t, nil
}

// lockWriter takes the shared writer lock (LockRegion.LockWriter) for a
// goroutine that holds writerMu; m is a map it pinned before it began to
// wait. If the lock's owner died, the repair (mdb_mutex_failed) publishes
// the newest meta page's txnid, read through m: Close may have cleared the
// current map while we waited, and the repair must complete all the same,
// or the mutex is left unrecoverable for every process (C repairs it
// before it refuses this process's transaction, as here: the caller fails
// with EINVAL after Close once it holds the lock).
func (e *Env) lockWriter(m *sys.DataMap) error {
	if e.writer.newestTxnID == nil {
		e.writer.newestTxnID = func() (uint64, error) {
			meta, err := e.pickMetaIn(e.writer.waitMap)
			return meta.TxnID, err
		}
	}
	e.writer.waitMap = m
	err := e.lock.LockWriter(e.writer.newestTxnID)
	e.writer.waitMap = nil
	return err
}

// end is mdb_txn_end for a top-level write transaction: export (keep) or
// close the handles of databases this transaction created
// (mdb_dbis_update), drop the dirty pages and cursors, unlock.
//
// The error is the writer mutex's unlock failing, wrapped with
// ErrWriterUnlock: the transaction ended on another OS thread than the one
// that began it (Linux: EPERM; Windows: ERROR_NOT_OWNER). The mutex
// stays held by the beginning thread; C ignores this (UNLOCK_MUTEX). The
// in-process lock is released all the same, so that once that thread ends
// (its goroutine exits while still pinned) the next writer recovers the
// mutex from its dead owner instead of every writer of the process
// waiting forever.
func (t *Txn) end(keep bool) error {
	w := t.w
	if w.flags&txnFinished != 0 {
		return nil
	}
	if w.parent != nil {
		if keep {
			t.endNested(endCommit)
		} else {
			t.endNested(endAbort)
		}
		return nil
	}
	r := &t.env.dbis
	for i := format.CoreDBs; i < t.ndbs; i++ {
		if e := t.dbe(i); e.state&dbNew != 0 {
			r.finishNew(i, keep, e.db.Flags)
		}
	}
	w.cursors.reset()
	// The pool keeps as many spare pages as this transaction used.
	w.dirty.recycle()
	clear(w.spilled)
	if !keep && w.hasBaseMain {
		t.closeVanishedHandles(w.baseMain)
	}
	w.freePgs = w.freePgs[:0]
	w.loose = w.loose[:0]
	w.dropPghead()
	w.pglast = 0
	w.flags = txnFinished
	t.forgetPages()
	if w.locked {
		w.locked = false
		e := t.env
		e.writer.pgOldest = w.pgOldest
		e.writer.pbufPad = w.pbufPad
		// Handles fail with EINVAL until the next write transaction takes
		// the table up again (initDBs).
		t.dbs, t.ndbs = t.dbs[:0], 0
		// Once the writer locks are released, t belongs to the next write
		// transaction (writerState.txn): take the map off it before. The
		// map is released last: that may tear the environment down (a
		// write transaction left open across Close), and the unlock needs
		// the lock region.
		m := t.dmap
		t.dmap = nil
		err := e.lock.UnlockWriter()
		e.writerMu.Unlock()
		if testHookWriterUnlocked != nil {
			testHookWriterUnlocked(e)
		}
		m.Release()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrWriterUnlock, err)
		}
		return nil
	}
	return nil
}

// dupScratch is *buf resliced to n zero bytes (grown to at least a page,
// size ps, if needed).
func (w *wstate) dupScratch(buf *[]byte, n, ps int) []byte {
	b := *buf
	if cap(b) < n {
		b = make([]byte, n, max(n, ps))
	} else {
		b = b[:n]
		clear(b)
	}
	*buf = b
	return b
}

// dropPghead sets pghead back to nil (no free DB record read), keeping
// its memory for the next transaction's.
func (w *wstate) dropPghead() {
	if cap(w.pghead) > cap(w.pgheadSpare) {
		w.pgheadSpare = w.pghead[:0]
	}
	w.pghead = nil
}

// emptyPghead is an empty, non-nil pghead, in the memory dropPghead kept.
func (w *wstate) emptyPghead() format.IDL {
	b := w.pgheadSpare[:0]
	w.pgheadSpare = nil
	if b == nil {
		b = format.IDL{}
	}
	return b
}

// wroteMeta: after mdb_env_write_meta wrote meta and published its txnid,
// remember it for the next write transaction.
func (t *Txn) wroteMeta(meta format.Meta) {
	if t.w.locked {
		t.env.writer.lastMeta = meta
		t.env.writer.hasLastMeta = t.env.lockFile
	}
}

// closeVanishedHandles: after an abort, with this transaction's own pages
// dropped so reads see the state it reverts to (whose main DB record is
// main), close the handles that name a database which does not exist
// there. C closes only DB_NEW handles (KNOWN_C_BUGS.md #19).
func (t *Txn) closeVanishedHandles(main format.DB) {
	r := &t.env.dbis
	var candidates []int
	for i := format.CoreDBs; i < t.ndbs; i++ {
		e := t.dbe(i)
		if e.state&dbValid != 0 && e.state&dbNew == 0 && e.seq == r.seqOf(i) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return
	}
	t.dbe(format.MainDBI).db = main
	for _, i := range candidates {
		name := r.nameOf(i)
		if name == nil {
			continue
		}
		if _, found, err := t.findNamedDB(name); err != nil || !found {
			r.close(DBI(i))
		}
	}
}

// abortWrite is mdb_txn_abort of a write transaction (an active nested
// transaction is aborted first, as in C).
func (t *Txn) abortWrite() {
	if t.w.child != nil {
		t.w.child.abortWrite()
	}
	_ = t.end(false) // mdb_txn_abort reports nothing; see end
}

// commitWrite is mdb_txn_commit of a write transaction. On failure the
// transaction is aborted. For a nested transaction: make its changes part
// of the parent (nothing is written); a failed child is aborted and the
// parent marked failed, as in C.
func (t *Txn) commitWrite() error {
	w := t.w
	if w.parent != nil {
		if w.flags&(txnFinished|txnError) != 0 {
			t.endNested(endFailed)
			return format.BadTxn
		}
		if w.flags&txnHasChild != 0 {
			// Commit the active child first, as C does.
			if err := w.child.commitWrite(); err != nil {
				t.endNested(endFailed)
				return err
			}
		}
		t.endNested(endCommit)
		return nil
	}
	if w.flags&txnFinished != 0 {
		return format.BadTxn
	}
	if w.flags&txnHasChild != 0 {
		if err := w.child.commitWrite(); err != nil {
			_ = t.end(false)
			return err
		}
	}
	err := t.commit0()
	if uerr := t.end(err == nil); err == nil {
		// Committed, but the writer mutex is still held (see end).
		err = uerr
	}
	return err
}

// checkUsable is MDB_TXN_BLOCKED -> MDB_BAD_TXN.
func (t *Txn) checkUsable() error {
	if t.w.flags&txnBlocked != 0 {
		return format.BadTxn
	}
	return nil
}

// failUnlessClean: after a put or delete failed with err, unless err is
// one of the errors returned before anything changed, the tree may be half
// changed, so the transaction may only be aborted now (MDB_TXN_ERROR).
func (t *Txn) failUnlessClean(err error) {
	switch err {
	case format.KeyExist, format.NotFound, format.BadValSize, format.Incompatible,
		format.BadTxn, format.BadDBI, syscall.EINVAL:
		return
	}
	t.w.flags |= txnError
}

// userDBI is TXN_DBI_EXIST(txn, dbi, DB_USRVALID) for a write operation,
// then the stale-record refresh of mdb_cursor_init.
func (t *Txn) userDBI(dbi DBI) (*txnDB, error) {
	e, err := t.dbiEntry(dbi, dbUsrValid)
	if err != nil {
		return nil, err
	}
	if err := t.checkUsable(); err != nil {
		return nil, err
	}
	return e, t.refreshed(dbi, e)
}

// hasParent reports a nested transaction.
func (t *Txn) hasParent() bool { return t.w.parent != nil }

// --- page lookup in write transactions -----------------------------------------

// locate is the mt_parent walk of mdb_page_get: at each level the spill
// list is checked before the dirty list. Returns the dirty buffer (nil if
// spilled or clean) and the level (1 = this transaction, 2 = its parent,
// ...; 0 = clean).
func (t *Txn) locate(pgno uint64) ([]byte, int) { return t.locateIn(pgno, true) }

func (t *Txn) locateIn(pgno uint64, ownDirty bool) ([]byte, int) {
	w := t.w
	if len(w.spilled) != 0 && w.spilled.contains(pgno) {
		return nil, 1
	}
	if ownDirty {
		if b := w.dirty.get(pgno); b != nil {
			return b, 1
		}
	}
	for i := range w.parents {
		ly := &w.parents[i]
		if len(ly.spilled) != 0 && ly.spilled.contains(pgno) {
			return nil, i + 2
		}
		if b := ly.dirty.get(pgno); b != nil {
			return b, i + 2
		}
	}
	return nil, 0
}

// pageLevel is the level output of mdb_page_get.
func (t *Txn) pageLevel(pgno uint64) int {
	_, level := t.locate(pgno)
	return level
}

// dirtyBuf is the written buffer of pgno (dirty here or in an ancestor).
func (t *Txn) dirtyBuf(pgno uint64) []byte {
	b, _ := t.locate(pgno)
	return b
}

// pageNotDirtyHere is Txn.page for a page known not to be in this
// transaction's own dirty list.
func (t *Txn) pageNotDirtyHere(pgno uint64) (format.Page, error) {
	if b, _ := t.locateIn(pgno, false); b != nil {
		return pageOf(b[:t.pageSize], t.l)
	}
	return t.mappedPage(pgno)
}

// dataLocBytes is the data of the node under cursor c (after a put, the
// item just written): inline in a dirty leaf, or on its overflow pages.
func (t *Txn) dataLocBytes(c cref) ([]byte, error) {
	page, err := t.topPage(c)
	if err != nil {
		return nil, err
	}
	ki := t.cur(c).kiTop()
	node, ok := page.Node(ki)
	if !ok {
		return nil, format.Corrupted
	}
	if node.IsBigData() {
		pg, ok := node.OverflowPgno()
		if !ok {
			return nil, format.Corrupted
		}
		run, err := t.dirtyMut(pg)
		if err != nil {
			return nil, err
		}
		start := t.l.PageHdr
		n := node.DataSize()
		if start+n > len(run) {
			return nil, format.Corrupted
		}
		return run[start : start+n : start+n], nil
	}
	pb, err := t.dirtyMut(t.cur(c).pgTop())
	if err != nil {
		return nil, err
	}
	pb = pb[:t.pageSize]
	s, e, ok := nodeDataRange(t.l, pb, ki)
	if !ok {
		return nil, format.Corrupted
	}
	return pb[s:e:e], nil
}

// --- databases --------------------------------------------------------------------

// createDB is mdb_dbi_open (mdb.c line 9955) in a write transaction: the
// handle of database name (nil: the main DB), created with the persistent
// bits of flags if it does not exist and flags has MDB_CREATE. Errors as
// C: MDB_NOTFOUND, MDB_DBS_FULL, MDB_INCOMPATIBLE, EINVAL for unknown
// flags. For the main DB, persistent flags are added to its record.
func (t *Txn) createDB(name []byte, flags uint32) (DBI, error) {
	if flags&^format.ValidDBFlags != 0 {
		return 0, syscall.EINVAL
	}
	if err := t.checkUsable(); err != nil {
		return 0, err
	}
	if name == nil {
		t.updateMainFlags(flags)
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
		if s.name == nil {
			if unused < 0 {
				unused = i
			}
			continue
		}
		if bytes.Equal(s.name, key) {
			r.mu.Unlock()
			if !t.adoptRegistration(i, key) && flags&format.Create != 0 {
				// Dropped in this transaction while the name stayed open
				// elsewhere: create it again in this slot.
				if err := t.recreateDB(i, key, flags); err != nil {
					return 0, err
				}
			}
			return DBI(i), nil
		}
	}
	if dbi, ok := t.reopenLiveView(key); ok {
		r.mu.Unlock()
		return dbi, nil
	}
	// The registry stays locked until the slot is registered; nothing
	// below touches it (the main DB is never stale).
	defer r.mu.Unlock()
	if unused < 0 && len(r.slots) >= r.capacity() {
		return 0, format.DBsFull
	}
	if uint32(t.dbe(format.MainDBI).db.Flags)&(format.DupSort|format.IntegerKey) != 0 {
		if flags&format.Create != 0 {
			return 0, format.Incompatible
		}
		return 0, format.NotFound
	}
	db, state, err := t.findOrCreateNamedDB(key, flags)
	if err != nil {
		return 0, err
	}
	// Got info, register DBI in this txn and the environment.
	i := r.register(unused, dbiSlot{
		name:  key,
		flags: uint16(uint32(db.Flags) & format.PersistentFlags),
		valid: state&dbNew == 0,
	})
	seq := r.bumpSeq(i)
	e := t.dbe(i)
	e.keyCmp, e.dupCmp, e.hasDup = comparatorsFor(&r.slots[i], i, db.Flags)
	e.db = db
	e.state = state
	e.seq = seq
	e.nameID = nameID(key)
	return DBI(i), nil
}

// findOrCreateNamedDB looks the name up in the main DB and, if missing and
// flags has MDB_CREATE, stores an empty record.
func (t *Txn) findOrCreateNamedDB(key []byte, flags uint32) (format.DB, uint8, error) {
	db, found, err := t.findNamedDB(key)
	if err != nil && err != format.NotFound {
		return db, 0, err
	}
	if found {
		return db, dbValid | dbUsrValid, nil
	}
	if flags&format.Create == 0 {
		return db, 0, format.NotFound
	}
	db = format.EmptyDB(uint16(flags & format.PersistentFlags))
	slot := t.openSlot(format.MainDBI, true)
	rec := db.Bytes()
	_, err = t.putAt(mainRef(slot), key, putBytes(rec[:]), uint32(format.FSubData))
	t.closeSlot(slot)
	if err != nil {
		return db, 0, err
	}
	return db, dbNew | dbValid | dbUsrValid | dbDirty, nil
}

// recreateDB is the create branch of mdb_dbi_open for a name open in slot
// i although this transaction dropped the database.
func (t *Txn) recreateDB(i int, key []byte, flags uint32) error {
	db, state, err := t.findOrCreateNamedDB(key, flags)
	if err != nil {
		return err
	}
	r := &t.env.dbis
	r.mu.RLock()
	e := t.dbe(i)
	e.keyCmp, e.dupCmp, e.hasDup = comparatorsFor(&r.slots[i], i, db.Flags)
	r.mu.RUnlock()
	e.db = db
	e.state = state
	e.seq = r.seqOf(i)
	e.nameID = nameID(key)
	return nil
}

// updateMainFlags is the main-DB branch of mdb_dbi_open: add persistent
// flags to its record (MDB_TXN_DIRTY makes sure they are committed) and
// pick its comparators again.
func (t *Txn) updateMainFlags(flags uint32) {
	e := t.dbe(format.MainDBI)
	f2 := uint16(flags & format.PersistentFlags)
	if e.db.Flags|f2 != e.db.Flags {
		e.db.Flags |= f2
		t.w.flags |= txnDirty
	}
	r := &t.env.dbis
	r.mu.RLock()
	e.keyCmp, e.dupCmp, e.hasDup = comparatorsFor(&r.slots[format.MainDBI], format.MainDBI, e.db.Flags)
	r.mu.RUnlock()
}

// Drop is mdb_drop: with del, delete database dbi from the environment
// and close its handle (for the main DB: empty it); otherwise empty it,
// keeping the handle. All its pages go on the free list. Emptying the
// main DB of an environment with named databases differs between the
// formats as between C 0.9 (their pages leak, KNOWN_C_BUGS.md #21) and
// C 1.0 (they are deleted first; MDB_DBIS_BUSY while one is open).
func (t *Txn) Drop(dbi DBI, del bool) error {
	if t.w == nil {
		return syscall.EACCES
	}
	if _, err := t.dbiEntry(dbi, dbUsrValid); err != nil {
		return err
	}
	if err := t.checkUsable(); err != nil {
		return err
	}
	if _, err := t.freshDBI(dbi, dbValid); err != nil {
		return err
	}
	return t.mdbDrop(int(dbi), del)
}

// --- writes ---------------------------------------------------------------------

// Put is mdb_put: store data under key in dbi. Flags: MDB_NOOVERWRITE,
// MDB_NODUPDATA, MDB_APPEND, MDB_APPENDDUP (EINVAL otherwise; MDB_RESERVE
// through PutReserve). Size limits as C.
func (t *Txn) Put(dbi DBI, key, data []byte, flags uint32) error {
	if t.w == nil {
		return syscall.EACCES
	}
	if flags&format.Reserve != 0 {
		return syscall.EINVAL
	}
	_, err := t.mdbPut(dbi, key, putBytes(data), flags)
	return err
}

// PutReserve is mdb_put with MDB_RESERVE: make room for n bytes under key
// and return them for the caller to fill in before the next change. Not
// for DUPSORT databases (EINVAL, KNOWN_C_BUGS.md #16).
func (t *Txn) PutReserve(dbi DBI, key []byte, n int, flags uint32) ([]byte, error) {
	if t.w == nil {
		return nil, syscall.EACCES
	}
	return t.mdbPut(dbi, key, putReserve(n), flags|format.Reserve)
}

// checkReserve: MDB_RESERVE is documented as not allowed with
// MDB_DUPSORT; C does not check and misbehaves.
func checkReserve(dbFlags uint16, flags uint32) error {
	if flags&format.Reserve != 0 && uint32(dbFlags)&format.DupSort != 0 {
		return syscall.EINVAL
	}
	return nil
}

// mdbPut is mdb_put (mdb.c line 9231): a temporary tracked cursor and
// mdb_cursor_put. Returns the reserved bytes for MDB_RESERVE.
func (t *Txn) mdbPut(dbi DBI, key []byte, data putData, flags uint32) ([]byte, error) {
	e, err := t.dbiEntry(dbi, dbUsrValid)
	if err != nil {
		return nil, err
	}
	if flags&^(format.NoOverwrite|format.NoDupData|format.Reserve|format.Append|format.AppendDup) != 0 {
		return nil, syscall.EINVAL
	}
	if err := t.checkUsable(); err != nil {
		return nil, err
	}
	if err := t.refreshed(dbi, e); err != nil {
		return nil, err
	}
	if err := checkReserve(e.db.Flags, flags); err != nil {
		return nil, err
	}
	slot := t.w.cursors.open(int(dbi), e.dupOrder(), uint32(e.db.Flags)&format.DupSort != 0, true)
	c := mainRef(slot)
	_, err = t.putAt(c, key, data, flags)
	var loc []byte
	if err == nil && flags&format.Reserve != 0 {
		loc, err = t.dataLocBytes(c)
	}
	t.closeSlot(slot)
	return loc, err
}

// Del is mdb_del: delete key from dbi; for a DUPSORT database only the
// pair key/data if data is not nil, else all of the key's items.
// MDB_NOTFOUND if there was nothing to delete.
func (t *Txn) Del(dbi DBI, key, data []byte) error {
	if t.w == nil {
		return syscall.EACCES
	}
	e, err := t.userDBI(dbi)
	if err != nil {
		return err
	}
	// Must ignore any data unless DUP_SORT.
	hasData := data != nil && uint32(e.db.Flags)&format.DupSort != 0
	if !hasData {
		data = nil
	}
	return t.del0(int(dbi), key, data, hasData, 0)
}

// --- handle cursors -------------------------------------------------------------

// cursorID names a write cursor: an arena slot and its generation.
type cursorID struct {
	slot int
	gen  uint32
}

// active is the innermost active transaction of t (a parent's cursors
// work through its active nested transaction, as in C's
// mdb_cursor_shadow).
func (t *Txn) active() *Txn {
	for t.w != nil && t.w.child != nil {
		t = t.w.child
	}
	return t
}

// openWriteCursor is mdb_cursor_open in a write transaction: a tracked
// cursor on dbi. EINVAL for the free DB, as in C.
func (t *Txn) openWriteCursor(dbi DBI) (cursorID, error) {
	if _, err := t.dbiEntry(dbi, dbValid); err != nil {
		return cursorID{}, err
	}
	if err := t.checkUsable(); err != nil {
		return cursorID{}, err
	}
	if dbi == FreeDBI {
		return cursorID{}, syscall.EINVAL
	}
	if _, err := t.freshDBI(dbi, dbValid); err != nil {
		return cursorID{}, err
	}
	slot := t.openSlot(int(dbi), true)
	return cursorID{slot, t.w.cursors.slots[slot].gen}, nil
}

// closeWriteCursor is mdb_cursor_close. Closing a cursor of an ancestor
// through a nested transaction does nothing, as in C.
func (t *Txn) closeWriteCursor(id cursorID) {
	t = t.active()
	if t.w == nil || t.w.flags&txnFinished != 0 {
		return
	}
	if slot, err := t.cursorSlot(id); err == nil {
		if t.w.cursors.slots[slot].owner == t.w.cursors.depth {
			t.closeSlot(slot)
		}
	}
}

// cursorSlot is the slot of a cursor handle usable in this transaction:
// one opened by it, or, in a nested transaction, by an ancestor.
func (t *Txn) cursorSlot(id cursorID) (int, error) {
	a := t.w.cursors
	s := a.slot(id.slot)
	if s == nil || s.gen != id.gen || !s.tracked || s.owner > a.depth {
		return 0, syscall.EINVAL
	}
	return id.slot, nil
}

// cursorState is the state of the main cursor of a handle.
func (t *Txn) cursorState(id cursorID) (*wcursor, error) {
	t = t.active()
	if t.w.flags&txnFinished != 0 {
		return nil, syscall.EINVAL
	}
	slot, err := t.cursorSlot(id)
	if err != nil {
		return nil, err
	}
	return &t.w.cursors.slots[slot].main, nil
}

// checkCursorDB: the handle of a cursor's database was closed, e.g. by
// Drop: operations that would search its tree fail with MDB_BAD_DBI, as
// C's mdb_page_search does for a DB_STALE handle whose sequence number
// changed.
func (t *Txn) checkCursorDB(slot int) error {
	dbi := t.w.cursors.slots[slot].main.dbi
	e := t.dbe(dbi)
	if e.state&dbStale != 0 && e.seq != t.env.dbis.seqOf(dbi) {
		return format.BadDBI
	}
	return nil
}

// writeCursorGet is mdb_cursor_get on a write cursor.
func (t *Txn) writeCursorGet(c *Cursor, key, data []byte, op format.CursorOp) ([]byte, []byte, error) {
	t = t.active()
	if t.w.flags&txnFinished != 0 {
		return nil, nil, syscall.EINVAL
	}
	slot, err := t.cursorSlot(c.id)
	if err != nil {
		return nil, nil, err
	}
	if err := t.checkUsable(); err != nil {
		return nil, nil, err
	}
	switch op {
	case format.GetCurrent, format.FirstDup, format.LastDup, format.GetMultiple:
	default:
		if err := t.checkCursorDB(slot); err != nil {
			return nil, nil, err
		}
	}
	cr := mainRef(slot)
	e := t.dbe(t.cur(cr).dbi)
	hasX, err := t.readCursors(cr, &c.mc, &c.mx)
	if err != nil {
		return nil, nil, err
	}
	var x *rcursor
	if hasX {
		x = &c.mx
	}
	k, v, err := cursorGet(t, &c.mc, x, e.dupCmp, e.hasDup, key, data, op)
	t.writeBack(cr, &c.mc, &c.mx, hasX)
	return k, v, err
}

// writeCursorIsDB is 1.0's mdb_cursor_is_db on a write cursor.
func (t *Txn) writeCursorIsDB(id cursorID) (bool, error) {
	t = t.active()
	slot, err := t.cursorSlot(id)
	if err != nil {
		return false, err
	}
	var mc, mx rcursor
	if _, err := t.readCursors(mainRef(slot), &mc, &mx); err != nil {
		return false, err
	}
	return currentIsDB(&mc), nil
}

// writeCursorCount is mdb_cursor_count on a write cursor.
func (t *Txn) writeCursorCount(c *Cursor) (uint64, error) {
	t = t.active()
	if t.w.flags&txnFinished != 0 {
		return 0, syscall.EINVAL
	}
	slot, err := t.cursorSlot(c.id)
	if err != nil {
		return 0, err
	}
	if err := t.checkUsable(); err != nil {
		return 0, err
	}
	cr := mainRef(slot)
	hasX, err := t.readCursors(cr, &c.mc, &c.mx)
	if err != nil {
		return 0, err
	}
	var x *rcursor
	if hasX {
		x = &c.mx
	}
	n, err := cursorCount(&c.mc, x)
	t.writeBack(cr, &c.mc, &c.mx, hasX)
	return n, err
}

// IsDB is 1.0's mdb_cursor_is_db: the cursor is on the record of a named
// database.
func (c *Cursor) IsDB() (bool, error) {
	if c.closed {
		return false, syscall.EINVAL
	}
	if c.txn.w != nil {
		return c.txn.writeCursorIsDB(c.id)
	}
	return currentIsDB(&c.mc), nil
}

// HasDups reports whether the cursor is on a key whose duplicates are in a
// sub-page or sub-DB (an F_DUPDATA node).
func (c *Cursor) HasDups() bool {
	if c.txn.w != nil {
		t := c.txn.active()
		slot, err := t.cursorSlot(c.id)
		if err != nil {
			return false
		}
		var mc, mx rcursor
		if _, err := t.readCursors(mainRef(slot), &mc, &mx); err != nil {
			return false
		}
		return currentHasDups(&mc)
	}
	return currentHasDups(&c.mc)
}

// writeTxn is the active write transaction of a write cursor, or EACCES
// for a read cursor (C 0.9's code for writes in a read transaction).
func (c *Cursor) writeTxn() (*Txn, int, error) {
	if c.closed {
		return nil, 0, syscall.EINVAL
	}
	if c.txn.w == nil {
		return nil, 0, syscall.EACCES
	}
	t := c.txn.active()
	if t.w.flags&txnFinished != 0 {
		return nil, 0, syscall.EINVAL
	}
	slot, err := t.cursorSlot(c.id)
	return t, slot, err
}

// putChecked validates the flags of mdb_cursor_put and runs it.
func (t *Txn) putChecked(slot int, key []byte, data putData, flags uint32) (int, error) {
	const valid = format.NoOverwrite | format.NoDupData | format.Current | format.Reserve |
		format.Append | format.AppendDup | format.Multiple
	if flags&^valid != 0 {
		return 0, syscall.EINVAL
	}
	if flags&format.Current == 0 {
		if err := t.checkCursorDB(slot); err != nil {
			return 0, err
		}
	}
	return t.putAt(mainRef(slot), key, data, flags&^mdbNoSpill)
}

// Put is mdb_cursor_put: store data under key and leave the cursor on the
// new item. Flags as Txn.Put, plus MDB_CURRENT: replace the item at the
// cursor position.
func (c *Cursor) Put(key, data []byte, flags uint32) error {
	if flags&(format.Reserve|format.Multiple) != 0 {
		return syscall.EINVAL
	}
	t, slot, err := c.writeTxn()
	if err != nil {
		return err
	}
	_, err = t.putChecked(slot, key, putBytes(data), flags)
	return err
}

// PutReserve is mdb_cursor_put with MDB_RESERVE.
func (c *Cursor) PutReserve(key []byte, n int, flags uint32) ([]byte, error) {
	if flags&format.Multiple != 0 {
		return nil, syscall.EINVAL
	}
	t, slot, err := c.writeTxn()
	if err != nil {
		return nil, err
	}
	if err := checkReserve(t.dbOf(mainRef(slot)).Flags, format.Reserve); err != nil {
		return nil, err
	}
	if _, err := t.putChecked(slot, key, putReserve(n), flags|format.Reserve); err != nil {
		return nil, err
	}
	return t.dataLocBytes(mainRef(slot))
}

// PutMulti is mdb_cursor_put with MDB_MULTIPLE: store the stride-byte
// items packed in page as duplicates of key (DUPFIXED only). Returns how
// many were stored.
func (c *Cursor) PutMulti(key, page []byte, stride int, flags uint32) (int, error) {
	if flags&format.Reserve != 0 {
		return 0, syscall.EINVAL
	}
	t, slot, err := c.writeTxn()
	if err != nil {
		return 0, err
	}
	count := 0
	if stride > 0 {
		count = len(page) / stride
	}
	return t.putChecked(slot, key, putMultiple(page, stride, count), flags|format.Multiple)
}

// Del is mdb_cursor_del: delete the item at the cursor; with
// MDB_NODUPDATA all duplicates of the current key.
func (c *Cursor) Del(flags uint32) error {
	t, slot, err := c.writeTxn()
	if err != nil {
		return err
	}
	if flags&^format.NoDupData != 0 {
		return syscall.EINVAL
	}
	return t.delAt(mainRef(slot), flags)
}

// testHookWriterUnlocked, if set (tests only), runs in the end of a
// top-level write transaction right after the writer locks are released,
// before the transaction's map is.
var testHookWriterUnlocked func(e *Env)
