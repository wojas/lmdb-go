// Copying an environment: mdb_env_copy2 (mdb.c line 9788) and
// mdb_env_copyfd2 (line 9773), plain (mdb_env_copyfd0, line 9674) or
// compacting (mdb_env_copyfd1, line 9543, with mdb_env_cwalk, line 9387)
// (rlmdb core/copy.rs).
//
// Plain copy: the data file as it is, up to the last page of the current
// snapshot. The two meta pages are copied while holding the writer lock
// (so no commit can change them halfway), after starting the read
// transaction that pins the snapshot; then the other pages follow without
// the lock. The result is byte for byte what C's mdb_copy writes.
//
// Compacting copy: only the pages the snapshot uses, renumbered from page
// 2 in depth-first order (each page after the pages below it, so a tree's
// root comes last), with an empty free DB: meta page 0 is a new
// environment's (txnid 0), meta page 1 holds the main DB (txnid 1, unless
// the environment is empty) and the main root is the last page. If the
// count of pages does not add up the copy fails with MDB_INCOMPATIBLE.
// C fills two 1 MiB buffers alternately and a second thread writes them;
// pages are copied without their unused middle part (mdb_page_copy), which
// keeps whatever an earlier page left there. The writer here is a plain
// loop over the same two buffers, handed over at the same points, so even
// those bytes match C's output.

package engine

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// wbuf is MDB_WBUF: the size of each write buffer of a compacting copy.
const wbuf = 1 << 20

// plainChunk is the read size of a plain copy.
const plainChunk = 4 << 20

// maxCwalkNesting is the deepest valid cwalk nesting: main DB, named DB,
// sub-DB of duplicates. C recurses without a limit.
const maxCwalkNesting = 3

// Copy is mdb_env_copy2: copy the environment to a new data file: path is
// the directory to put data.mdb in (it must exist), or with MDB_NOSUBDIR
// the file name. The file must not exist (O_EXCL, mode 0666 before the
// umask). No lock file is created. flags may hold MDB_CP_COMPACT.
func (e *Env) Copy(path string, flags uint32) error {
	target := path
	if e.flags.Load()&format.NoSubdir == 0 {
		target = filepath.Join(path, "data.mdb")
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return sys.Errno(err)
	}
	err = e.CopyTo(f, flags)
	if cerr := f.Close(); err == nil {
		err = sys.Errno(cerr)
	}
	return err
}

// CopyFD is mdb_env_copyfd2: write a copy to the file descriptor fd (a
// file, pipe or socket) at its current position. fd stays open and is not
// synced.
func (e *Env) CopyFD(fd uintptr, flags uint32) error {
	nfd, err := syscall.Dup(int(fd))
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(nfd), "copy")
	defer f.Close()
	return e.CopyTo(fdWriter{f}, flags)
}

// fdWriter writes with write(2), keeping the descriptor's position (os.File
// on a dup'ed descriptor does that; this only maps errors).
type fdWriter struct{ f *os.File }

func (w fdWriter) Write(b []byte) (int, error) {
	n, err := w.f.Write(b)
	return n, sys.Errno(err)
}

// CopyTo writes a copy of the environment's data file to w.
func (e *Env) CopyTo(w io.Writer, flags uint32) error {
	if flags&^format.CopyCompact != 0 {
		return syscall.EINVAL
	}
	if flags&format.CopyCompact != 0 {
		return e.copyCompact(w)
	}
	return e.copyPlain(w)
}

// copyPlain is mdb_env_copyfd0.
func (e *Env) copyPlain(w io.Writer) error {
	ps := e.pageSize
	buf := make([]byte, max(plainChunk, 2*ps))
	txn := new(Txn)
	if !e.lockFile {
		if err := e.BeginRead(txn); err != nil {
			return err
		}
		defer txn.Abort()
		if err := readExactAt(e.dataFile, buf[:2*ps], 0); err != nil {
			return err
		}
	} else {
		// Do the lock/unlock of the reader mutex before starting the write
		// lock, so read txns do not block writers.
		if err := e.BeginRead(txn); err != nil {
			return err
		}
		defer txn.Abort()
		txn.Reset()
		// Temporarily block writers until we snapshot the meta pages; the
		// read transaction must start after that.
		runtime.LockOSThread()
		e.writerMu.Lock()
		err := e.lock.LockWriter(func() (uint64, error) {
			m, err := e.pickMeta()
			return m.TxnID, err
		})
		if err == nil {
			err = txn.Renew()
			if err == nil {
				err = readExactAt(e.dataFile, buf[:2*ps], 0)
			}
			e.lock.UnlockWriter()
		}
		e.writerMu.Unlock()
		runtime.UnlockOSThread()
		if err != nil {
			return err
		}
	}
	if err := writeAll(w, buf[:2*ps]); err != nil {
		return err
	}
	fsize, err := sys.FileSize(e.dataFile)
	if err != nil {
		return err
	}
	end := min(int64(txn.nextPgno)*int64(ps), fsize)
	for pos := int64(2 * ps); pos < end; {
		n := int(min(end-pos, int64(len(buf))))
		if err := readExactAt(e.dataFile, buf[:n], pos); err != nil {
			return err
		}
		if err := writeAll(w, buf[:n]); err != nil {
			return err
		}
		pos += int64(n)
	}
	return nil
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// readExactAt preads exactly len(buf) bytes (EIO at end of file).
func readExactAt(f *os.File, buf []byte, pos int64) error {
	n, err := sys.ReadAt(f, buf, pos)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return syscall.EIO
	}
	return nil
}

// copyWriter is the double buffer of a compacting copy with the writer
// thread replaced by synchronous writes at the hand-over points.
type copyWriter struct {
	out      io.Writer
	bufs     []byte // mc_wbuf[0] and mc_wbuf[1], back to back
	wlen     [2]int
	toggle   int
	nextPgno uint64
	ps       int
	l        *format.Layout
	nesting  int
}

func (w *copyWriter) current() []byte {
	return w.bufs[w.toggle*wbuf : (w.toggle+1)*wbuf]
}

// flip is mdb_env_cthr_toggle(my, 1): write the filled buffer (and the
// overflow pages tail that follow it), then fill the other one.
func (w *copyWriter) flip(tail []byte) error {
	t := w.toggle
	if err := writeAll(w.out, w.bufs[t*wbuf:t*wbuf+w.wlen[t]]); err != nil {
		return err
	}
	if tail != nil {
		if err := writeAll(w.out, tail); err != nil {
			return err
		}
	}
	w.wlen[t] = 0
	w.toggle ^= 1
	w.wlen[w.toggle] = 0
	return nil
}

func (w *copyWriter) makeRoom() error {
	if w.wlen[w.toggle] >= wbuf {
		return w.flip(nil)
	}
	return nil
}

// putPage appends a copy of page src (mdb_page_copy) numbered mc_next_pgno
// and returns that number.
func (w *copyWriter) putPage(src []byte) (uint64, error) {
	if err := w.makeRoom(); err != nil {
		return 0, err
	}
	off := w.wlen[w.toggle]
	pgno := w.nextPgno
	dst := w.current()[off : off+w.ps]
	if err := copyUsed(w.l, dst, src, w.ps); err != nil {
		return 0, err
	}
	setPgno(dst, pgno)
	// 1.0's compacting copy writes every page as txn 1 (mdb.c 1.0 line
	// 11517); a no-op for 0.9.
	w.l.SetTxnID(dst, 1)
	w.nextPgno++
	w.wlen[w.toggle] += w.ps
	return pgno, nil
}

// putOverflow appends the overflow run (pages pages), renumbered.
func (w *copyWriter) putOverflow(run []byte, pages int) (uint64, error) {
	if err := w.makeRoom(); err != nil {
		return 0, err
	}
	off := w.wlen[w.toggle]
	pgno := w.nextPgno
	dst := w.current()[off : off+w.ps]
	copy(dst, run[:w.ps])
	setPgno(dst, pgno)
	w.l.SetTxnID(dst, 1)
	w.nextPgno += uint64(pages)
	w.wlen[w.toggle] += w.ps
	if pages > 1 {
		if err := w.flip(run[w.ps : pages*w.ps]); err != nil {
			return 0, err
		}
	}
	return pgno, nil
}

// copyCompact is mdb_env_copyfd1.
func (e *Env) copyCompact(out io.Writer) error {
	ps := e.pageSize
	l := e.layout
	txn, err := e.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		return err
	}
	defer txn.Abort()
	w := &copyWriter{out: out, bufs: make([]byte, 2*wbuf), nextPgno: format.NumMetas, ps: ps, l: l}
	// Meta page 0: a new environment (mdb_env_init_meta0), with the
	// source's recorded map address.
	meta := format.NewEnvMeta(l, uint32(ps), uint64(e.mapSize.Load()), e.flags.Load())
	m0, err := e.readMeta(0)
	if err != nil {
		return err
	}
	meta.Address = m0.Address
	meta1 := meta
	// Set metapage 1 with current main DB.
	main := txn.dbe(format.MainDBI).db
	root := main.Root
	newRoot := root
	if root != format.PInvalid {
		// Count free pages + freeDB pages. Subtract from last_pg to find
		// the new last_pg, which also becomes the new root.
		var freecount uint64
		c, err := txn.OpenCursor(FreeDBI)
		if err != nil {
			return err
		}
		for {
			_, v, err := c.Get(nil, nil, format.Next)
			if err == format.NotFound {
				break
			}
			if err != nil {
				return err
			}
			n, ok := format.EncodedLen(v)
			if !ok {
				return format.Corrupted
			}
			freecount += n
		}
		free := txn.dbe(format.FreeDBI).db
		freecount += free.BranchPages + free.LeafPages + free.OverflowPages
		newRoot = txn.nextPgno - 1 - freecount
		meta1.LastPgno = newRoot
		meta1.DBs[format.MainDBI] = main
		meta1.DBs[format.MainDBI].Root = newRoot
	} else {
		// When the DB is empty, handle it specially to fix any breakage
		// like page leaks from ITS#8174.
		meta1.DBs[format.MainDBI].Flags = main.Flags
	}
	if root != format.PInvalid || meta1.DBs[format.MainDBI].Flags != 0 {
		meta1.TxnID = 1 // use metapage 1
	}
	for i, m := range []format.Meta{meta, meta1} {
		page := w.current()[i*ps : (i+1)*ps]
		format.Put64(page, 0, uint64(i))
		format.Put16(page, l.FlagsOffset(), format.PMeta)
		m.Write(page[l.MetaOffset():])
	}
	w.wlen[0] = 2 * ps
	w.nesting = 1
	walked, err := w.cwalk(txn, root, false)
	if err != nil {
		return err
	}
	if walked != newRoot {
		// Page leak or corrupt DB.
		return format.Incompatible
	}
	return w.flip(nil)
}

// cwalk is mdb_env_cwalk: copy the tree rooted at root depth-first, each
// page after its children, and return the root's new number. With dupdata
// (a DUPSORT sub-DB) leaves are not scanned.
func (w *copyWriter) cwalk(txn *Txn, root uint64, dupdata bool) (uint64, error) {
	// Empty DB, nothing to do.
	if root == format.PInvalid {
		return root, nil
	}
	type branch struct {
		page []byte
		ki   int
	}
	var branches []branch
	descend := func(pgno uint64) (uint64, error) {
		for {
			page, err := txn.page(pgno)
			if err != nil {
				return 0, err
			}
			if page.IsLeaf() {
				return pgno, nil
			}
			if !page.IsBranch() {
				return 0, format.Corrupted
			}
			if len(branches)+1 >= format.CursorStack {
				return 0, format.CursorFull
			}
			n, ok := page.Node(0)
			if !ok {
				return 0, format.Corrupted
			}
			pgno = n.ChildPgno()
			branches = append(branches, branch{append([]byte(nil), page.B...), 0})
		}
	}
	leaf, err := descend(root)
	if err != nil {
		return 0, err
	}
	hasLeaf := true
	for {
		var newPgno uint64
		if hasLeaf {
			hasLeaf = false
			page, err := txn.page(leaf)
			if err != nil {
				return 0, err
			}
			var modified []byte
			if !page.IsLeaf2() && !dupdata {
				if modified, err = w.copyLeafPayloads(txn, page.B); err != nil {
					return 0, err
				}
			}
			if modified == nil {
				modified = page.B
			}
			if newPgno, err = w.putPage(modified); err != nil {
				return 0, err
			}
		} else {
			if len(branches) == 0 {
				return 0, format.Corrupted
			}
			b := &branches[len(branches)-1]
			b.ki++
			if b.ki < numKeys(w.l, b.page) {
				// Whenever we advance to a sibling branch page, we must
				// proceed all the way down to its first leaf.
				p := format.Page{B: b.page, L: w.l}
				n, ok := p.Node(b.ki)
				if !ok {
					return 0, format.Corrupted
				}
				if leaf, err = descend(n.ChildPgno()); err != nil {
					return 0, err
				}
				hasLeaf = true
				continue
			}
			bp := b.page
			branches = branches[:len(branches)-1]
			if newPgno, err = w.putPage(bp); err != nil {
				return 0, err
			}
		}
		if len(branches) == 0 {
			// Otherwise we're done.
			return newPgno, nil
		}
		// Update parent if there is one.
		b := &branches[len(branches)-1]
		if !setNodePgno(w.l, b.page, b.ki, newPgno) {
			return 0, format.Corrupted
		}
	}
}

// copyLeafPayloads is the leaf part of mdb_env_cwalk: copy the overflow
// pages and sub-DBs of the nodes of leaf and return the leaf with their
// new numbers, or nil if it has no such nodes.
func (w *copyWriter) copyLeafPayloads(txn *Txn, leaf []byte) ([]byte, error) {
	l := w.l
	var cp []byte
	n := numKeys(l, leaf)
	for i := 0; i < n; i++ {
		node, ok := format.Page{B: leaf, L: l}.Node(i)
		if !ok {
			return nil, format.Corrupted
		}
		switch {
		case node.IsBigData():
			r, ok := node.OverflowRef()
			if !ok {
				return nil, format.Corrupted
			}
			omp, err := txn.page(r.Pgno)
			if err != nil {
				return nil, err
			}
			// 0.9 takes the run length from the page, 1.0 from op_pages.
			pages := int(omp.OverflowPages())
			if l.Format == format.V10 {
				pages = int(min(r.Pages, 1<<40))
			}
			run, err := overflowRun(txn, r.Pgno, pages, w.ps)
			if err != nil {
				return nil, err
			}
			nw, err := w.putOverflow(run, pages)
			if err != nil {
				return nil, err
			}
			if cp == nil {
				cp = append([]byte(nil), leaf...)
			}
			// The new page number; 1.0 also writes the run as txn 1 and
			// keeps op_pages.
			if !setNodeOverflowRef(l, cp, i, format.OverflowRef{Pgno: nw, TxnID: 1, Pages: r.Pages}) {
				return nil, format.Corrupted
			}
		case node.IsSubData():
			data, ok := node.InlineData()
			if !ok {
				return nil, format.Corrupted
			}
			db, ok := format.ReadDB(data)
			if !ok {
				return nil, format.Corrupted
			}
			if w.nesting >= maxCwalkNesting {
				return nil, format.Corrupted
			}
			w.nesting++
			walked, err := w.cwalk(txn, db.Root, node.IsDupData())
			w.nesting--
			if err != nil {
				return nil, err
			}
			db.Root = walked
			if cp == nil {
				cp = append([]byte(nil), leaf...)
			}
			s, e, ok := nodeDataRange(l, cp, i)
			if !ok || e-s < format.DBSize {
				return nil, format.Corrupted
			}
			db.Write(cp[s:e])
		}
	}
	return cp, nil
}

// overflowRun is the pages pages of the overflow run at pgno.
func overflowRun(txn *Txn, pgno uint64, pages, ps int) ([]byte, error) {
	if pages <= 0 || pgno+uint64(pages) < pgno || pgno+uint64(pages) > txn.nextPgno {
		return nil, format.PageNotFound
	}
	b, ok := txn.dmap.Slice(int(pgno)*ps, pages*ps)
	if !ok {
		return nil, format.PageNotFound
	}
	return b, nil
}

// copyUsed is mdb_page_copy: copy page src into dst except the unused
// space between the node pointers and the nodes, with the boundaries
// widened to 8-byte alignment; if less than 8 bytes are unused, or for
// P_LEAF2 pages, everything up to the (aligned) unused tail.
func copyUsed(l *format.Layout, dst, src []byte, ps int) error {
	if len(src) < l.PageHdr {
		return format.Corrupted
	}
	lower, upper, flags := pLower(l, src), pUpper(l, src), pFlags(l, src)
	if len(src) < ps || len(dst) < ps || lower > upper || upper > ps {
		return format.Corrupted
	}
	unused := (upper - lower) &^ 7
	if unused != 0 && flags&format.PLeaf2 == 0 {
		up := upper &^ 7
		lo := (lower + 7) &^ 7
		copy(dst[:lo], src[:lo])
		copy(dst[up:ps], src[up:ps])
	} else {
		copy(dst[:ps-unused], src[:ps-unused])
	}
	return nil
}
