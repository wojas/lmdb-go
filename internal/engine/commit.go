// Committing a write transaction: mdb_txn_commit (mdb.c line 3791),
// mdb_page_flush (line 3394) and mdb_env_write_meta (line 3935) (rlmdb
// core/commit.rs).
//
// The order matters for crash safety and concurrent readers, and is C's:
//
//  1. store the MDB_db record of every named DB changed in this
//     transaction in the main DB (F_SUBDATA nodes);
//  2. save the free list (freelistSave);
//  3. write all dirty pages in page order, contiguous runs batched;
//  4. fdatasync the data file (F_FULLFSYNC on macOS), unless MDB_NOSYNC;
//  5. write the tail of meta page txnid % 2 (from mm_mapsize on) through
//     the O_DSYNC descriptor (unless MDB_NOSYNC/MDB_NOMETASYNC): only now
//     does the new snapshot exist on disk;
//  6. publish the txnid in the lock file, so new readers pick it up.
//
// A transaction without dirty pages commits without writing anything.

package engine

import (
	"os"
	"runtime"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// commitPages is MDB_COMMIT_PAGES: max pages per batched write.
const commitPages = 64

// maxWrite is MAX_WRITE: max bytes per write call.
const maxWrite = 0x40000000

// commit0 is _mdb_txn_commit for a top-level transaction, up to (not
// including) mdb_txn_end.
func (t *Txn) commit0() error {
	w := t.w
	if w.flags&(txnFinished|txnError|txnHasChild) != 0 {
		return format.BadTxn
	}
	// mdb_cursors_close.
	w.cursors.clear()
	if w.dirty.isEmpty() && w.flags&(txnDirty|txnSpills) == 0 {
		return nil
	}
	if err := t.updateDBRecords(); err != nil {
		return err
	}
	if err := t.freelistSave(); err != nil {
		return err
	}
	w.dropPghead()
	if err := t.pageFlush(); err != nil {
		return err
	}
	if err := t.env.Sync(false); err != nil {
		return err
	}
	return t.writeMeta()
}

// updateDBRecords writes the record of each named DB this transaction
// changed into the main DB. The cursor is tracked (C's is not,
// KNOWN_C_BUGS.md #11).
func (t *Txn) updateDBRecords() error {
	n := t.ndbs
	if n <= format.CoreDBs {
		return nil
	}
	slot := t.openSlot(format.MainDBI, true)
	mc := mainRef(slot)
	var err error
	for i := format.CoreDBs; i < n && err == nil; i++ {
		e := t.dbe(i)
		if e.state&dbDirty == 0 {
			continue
		}
		if e.seq != t.env.dbis.seqOf(i) {
			err = format.BadDBI
			break
		}
		name := t.env.dbis.nameOf(i)
		if name == nil {
			err = format.BadDBI
			break
		}
		rec := e.db.Bytes()
		_, err = t.putAt(mc, name, putBytes(rec[:]), uint32(format.FSubData))
	}
	t.closeSlot(slot)
	return err
}

// flushEntry is a prepared dirty page: page number, slot, bytes to write.
type flushEntry struct {
	pgno uint64
	slot pageSlot
	size int
}

// prepareFlush prepares dirty page page for writing as mdb_page_flush
// does: clears P_DIRTY and finalises the header for the format (1.0 stamps
// mp_txnid). Returns how many bytes to write (the whole run for overflow
// pages).
func prepareFlush(l *format.Layout, ps int, page []byte, flags uint16, txnid uint64) int {
	setFlags(l, page, flags&^format.PDirty)
	l.SetTxnID(page, txnid)
	n := 1
	if flags&format.POverflow != 0 {
		if v, ok := overflowPages(l, page); ok {
			n = int(v)
		}
	}
	return n * ps
}

// pageFlush is mdb_page_flush(txn, 0) at commit: write every dirty page,
// clearing P_DIRTY in each first. Loose pages (and P_KEEP ones, which lose
// the mark) stay dirty and in the list; written pages leave it.
func (t *Txn) pageFlush() error {
	l, ps, txnid := t.l, t.pageSize, t.txnid
	runs := t.w.runs[:0]
	t.w.dirty.forEach(func(pgno uint64, slot pageSlot, page []byte) {
		flags := pFlags(l, page)
		// Don't flush this page yet.
		if flags&(format.PLoose|format.PKeep) != 0 {
			setFlags(l, page, flags&^format.PKeep)
			return
		}
		runs = append(runs, flushEntry{pgno, slot, prepareFlush(l, ps, page, flags, txnid)})
	})
	t.w.runs = runs
	return t.writeRuns(runs)
}

// writePages is the writing part of mdb_page_flush for the dirty pages
// pgnos (ascending), used by mdb_page_spill.
func (t *Txn) writePages(pgnos []uint64) error {
	l, ps, txnid := t.l, t.pageSize, t.txnid
	runs := t.w.runs[:0]
	for _, pgno := range pgnos {
		slot, ok := t.w.dirty.find(pgno)
		if !ok {
			return format.Corrupted
		}
		page := t.w.dirty.at(slot, pgno)
		runs = append(runs, flushEntry{pgno, slot, prepareFlush(l, ps, page, pFlags(l, page), txnid)})
	}
	t.w.runs = runs
	return t.writeRuns(runs)
}

// writeRuns writes the prepared dirty pages runs (ascending), contiguous
// runs batched into one write of up to commitPages pages, then drops them
// from the dirty list (mt_dirty_room grows accordingly).
func (t *Txn) writeRuns(runs []flushEntry) error {
	ps := t.pageSize
	f := t.env.dataFile
	d := t.w.dirty
	var batch [commitPages][]byte
	n := 0
	var wpos uint64
	wsize := 0
	nextPos := ^uint64(0)
	for _, r := range runs {
		pos := r.pgno * uint64(ps)
		if pos != nextPos || n == commitPages || wsize+r.size > maxWrite {
			if err := t.writeBatch(f, batch[:n], wpos); err != nil {
				return err
			}
			n, wpos, wsize = 0, pos, 0
		}
		buf := d.at(r.slot, r.pgno)
		if buf == nil || r.size > len(buf) {
			return format.Corrupted
		}
		batch[n] = buf[:r.size]
		n++
		nextPos = pos + uint64(r.size)
		wsize += r.size
	}
	if err := t.writeBatch(f, batch[:n], wpos); err != nil {
		return err
	}
	// Drop the written pages from the dirty list (both ascending).
	j := 0
	d.retain(func(pgno uint64, _ []byte) bool {
		for j < len(runs) && runs[j].pgno < pgno {
			j++
		}
		return j >= len(runs) || runs[j].pgno != pgno
	})
	t.w.dirtyRoom += len(runs)
	return nil
}

// writeBatch writes one batch of contiguous pages at pos (C's pwritev).
// The syscall package has no pwritev; a batch of several buffers is
// gathered into one reused buffer and written with one pwrite.
func (t *Txn) writeBatch(f *os.File, bufs [][]byte, pos uint64) error {
	switch len(bufs) {
	case 0:
		return nil
	case 1:
		return sys.WriteAt(f, bufs[0], int64(pos))
	}
	b := t.w.flushBuf[:0]
	for _, x := range bufs {
		b = append(b, x...)
	}
	t.w.flushBuf = b
	return sys.WriteAt(f, b, int64(pos))
}

// writeMeta is mdb_env_write_meta: write the new meta page (txnid % 2)
// and publish the txnid. A failed write makes the environment unusable
// (MDB_FATAL_ERROR; later transactions fail with MDB_PANIC), after trying
// to put the old txnid back so the half-written page is not used.
func (t *Txn) writeMeta() error {
	e := t.env
	ps := t.pageSize
	txnid := t.txnid
	toggle := int(txnid & 1)
	flags := e.flags.Load()
	// Persist any increases of mapsize config. Meta page toggle^1 is the
	// one this transaction began from.
	mapsize := max(t.w.baseMapsize, uint64(e.mapSize.Load()))
	// metab: what to write back if the write fails.
	oldLast, oldTxnid, haveOld := e.metaIDsIn(t.dmap, toggle)
	meta := format.Meta{
		Magic:    format.Magic,
		Version:  t.l.DataVersion,
		MapSize:  mapsize,
		DBs:      [2]format.DB{t.dbe(format.FreeDBI).db, t.dbe(format.MainDBI).db},
		LastPgno: t.nextPgno - 1,
		TxnID:    txnid,
	}
	var buf [format.MetaSize]byte
	meta.Write(buf[:])
	off := int64(toggle*ps + t.l.MetaOffset() + format.MetaMapSize)
	// Write to the SYNC fd unless MDB_NOSYNC/MDB_NOMETASYNC.
	syncFD := flags&(format.NoSync|format.NoMetaSync) == 0
	f := e.dataFile
	if syncFD && e.metaFile != nil {
		f = e.metaFile
	}
	if err := sys.WriteAt(f, buf[format.MetaMapSize:], off); err != nil {
		// On a failure, the page cache still contains the new data. Write
		// some old data back, to prevent it from being used.
		if haveOld {
			meta.LastPgno, meta.TxnID = oldLast, oldTxnid
			meta.Write(buf[:])
			_ = sys.WriteAt(e.dataFile, buf[format.MetaMapSize:], off)
		}
		e.fatal.Store(true)
		return err
	}
	if runtime.GOOS == "darwin" && syncFD && e.metaFile != nil {
		// O_DSYNC does not flush the drive cache on macOS.
		if err := sys.Fdatasync(e.metaFile); err != nil {
			return err
		}
	}
	e.lock.SetTxnID(txnid)
	t.wroteMeta(meta)
	return nil
}
