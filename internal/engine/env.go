// The environment: data file, lock file, memory map, meta pages (rlmdb
// core/env.rs; mdb_env_open, mdb_env_open2, mdb_env_read_header,
// mdb_env_init_meta, mdb_env_close0, mdb_env_stat, mdb_env_info).
//
// Open follows mdb_env_open (mdb.c line 5102):
//
//  1. Resolve file names (mdb_fname_init): path/data.mdb and path/lock.mdb,
//     or with MDB_NOSUBDIR path and path-lock.
//  2. Decide the data-file format (SniffFormat: magic and version of the
//     first meta page; not in C, which handles one format). An existing
//     file keeps its format, a new one gets the requested format or the
//     default. This happens before the lock file is touched, so opening a
//     file of the other format never re-initialises a peer's lock file.
//  3. Unless read-only or MDB_NOLOCK, set up the lock file first. Read-only
//     environments open the data file first (it must exist).
//  4. Open the data file (O_RDWR|O_CREAT, or O_RDONLY).
//  5. mdb_env_open2: read both meta pages with pread; an empty file is a
//     new environment whose meta pages are written at once, with the OS
//     page size (1.0: or the one asked for) capped at the format's
//     maximum. A 1.0 file using encryption or checksums is refused. The map
//     size is the configured one, else the one recorded in the meta,
//     raised to cover all pages in use. Map the file read-only.
//  6. Open the O_DSYNC descriptor for meta-page writes (me_mfd).
//  7. If we initialised the lock table, publish the current txnid and
//     downgrade to a shared lock (mdb_env_share_locks).

package engine

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

// Options are the settings of mdb_env_create + mdb_env_set_* for Open.
type Options struct {
	// MapSize is mdb_env_set_mapsize; 0 uses the size recorded in the file
	// (1 MiB for a new one). It is raised if the file holds more data.
	MapSize int64
	// MaxReaders is mdb_env_set_maxreaders (0: 126). An existing lock file
	// keeps its size.
	MaxReaders uint32
	// MaxDBs is mdb_env_set_maxdbs.
	MaxDBs uint32
	// Flags are the MDB_* environment flags. Bits that are not 0.9
	// environment flags fail with EINVAL (also 1.0's MDB_PREVSNAPSHOT).
	Flags uint32
	// Format requests a data-file format (0: none). An existing file keeps
	// its format; asking for the other is MDB_VERSION_MISMATCH. A new
	// environment gets this format, else DefaultFormat().
	Format format.Format
	// PageSize is 1.0's mdb_env_set_pagesize for a new 1.0 environment (0:
	// the OS page size). EINVAL for 0.9 or a size that is not a power of
	// two from 256 to 65536.
	PageSize int
	// Mode is the file mode for new files (0: 0644).
	Mode os.FileMode
	// OpenLock opens the lock file (lockfile.Open). Required unless
	// MDB_NOLOCK is set.
	OpenLock OpenLockFunc
}

// Env is an open LMDB environment (MDB_env). Its methods may be called
// from any goroutine.
type Env struct {
	// dmap is the current data map. Transactions take a reference on the
	// map current when they begin, so SetMapSize can install a new map
	// without invalidating slices held by live transactions.
	dmap  atomic.Pointer[sys.DataMap]
	mapMu sync.Mutex

	metaFile *os.File // me_mfd: O_DSYNC, nil for read-only
	dataFile *os.File
	// lock is the reader table and writer lock: the lock file's
	// (lockFile), or an in-memory one (MemLock) without a lock file
	// (MDB_NOLOCK, or a read-only file system), see AGENTS.md rule 2a.
	lock     LockRegion
	lockFile bool
	// mem is lock without a lock file: the in-memory region, under NoLock
	// shared by every handle of the data file in the process (registry.go).
	mem *MemLock
	// slots are the idle reader slots this process keeps for its next
	// read transactions (slotpool.go).
	slots slotPool

	dbis dbiRegistry

	flags atomic.Uint32
	// fatal is MDB_FATAL_ERROR: a meta page write failed; every new
	// transaction fails with MDB_PANIC.
	fatal atomic.Bool

	// writerMu serialises the write transactions of this handle and guards
	// writer; the lock region's writer lock (taken after it) serialises
	// them with other processes' (the lock file's mutex) or, under NoLock,
	// with the other handles' of the file in this process (the shared
	// in-memory region).
	writerMu sync.Mutex
	writer   writerState

	pageSize int
	mapSize  atomic.Int64
	// dirtyLimit is MDB_IDL_UM_MAX, lowered by tests to make small
	// transactions spill.
	dirtyLimit atomic.Int64
	maxReaders uint32
	layout     *format.Layout
	nodeMax    int
	maxKey     int
	path       string
	regKey     envID

	closeOnce sync.Once
	// closed is set by Close: no new transactions, no Renew.
	closed atomic.Bool
	// liveMaps counts the data maps not yet unmapped. The environment is
	// torn down (lock region, files, registry entry) when it drops to 0,
	// which happens only after Close: see Close.
	liveMaps     atomic.Int32
	teardownOnce sync.Once
}

// writerState is the per-environment state of the single write
// transaction that C keeps in MDB_env.
type writerState struct {
	// pgOldest is me_pgoldest.
	pgOldest uint64
	// pbufPad is me_pbuf->mp_pad: the key size field of C's scratch page
	// for MDB_DUPSORT puts, which outlives transactions; a sub-page made
	// from a single item of a non-DUPFIXED database inherits it.
	pbufPad uint16
	// lastMeta is the meta page this process's last write transaction
	// wrote: while the lock file still names its txnid it is the newest,
	// and the next write transaction begins from it without reading it.
	lastMeta    format.Meta
	hasLastMeta bool
	// txn is the top-level write transaction, reused by every one as C
	// reuses me_txn0 (nil until the first): with it its DB table (me_maxdbs
	// long; its generation makes reuse O(1)), dirty list and page pool,
	// cursor arena, spill set and page lists, so that a small write
	// transaction allocates nothing.
	txn *Txn
	// newestTxnID is the callback of LockWriter, made once (a closure
	// passed through an interface escapes: one allocation per call).
	newestTxnID func() (uint64, error)
	// waitMap is the map the goroutine waiting in LockWriter pinned before
	// it began to wait (see lockWriter).
	waitMap *sys.DataMap
}

// canonicalPath is the absolute form of the data file's path with
// symlinks resolved: of the file, or of its directory while the file does
// not exist.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(abs); err == nil {
		return p, nil
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", sys.Errno(err)
	}
	return filepath.Join(dir, filepath.Base(abs)), nil
}

// fileNames is mdb_fname_init: the data and lock file names.
func fileNames(path string, noSubdir bool) (data, lock string) {
	if noSubdir {
		return path, path + "-lock"
	}
	return filepath.Join(path, "data.mdb"), filepath.Join(path, "lock.mdb")
}

// Open opens the environment at path (mdb_env_open; see the file comment
// for the sequence).
func Open(path string, opts Options) (env *Env, err error) {
	flags := opts.Flags
	maxReaders := opts.MaxReaders
	if maxReaders == 0 {
		maxReaders = format.DefaultReaders
	}
	if flags&^(format.Changeable|format.Changeless) != 0 {
		return nil, syscall.EINVAL
	}
	mode := opts.Mode
	if mode == 0 {
		mode = 0o644
	}
	readOnly := flags&format.ReadOnly != 0
	if readOnly {
		// Silently ignore WRITEMAP when we're only getting read access.
		flags &^= format.WriteMap
	}
	noSubdir := flags&format.NoSubdir != 0
	noLock := flags&format.NoLock != 0
	if !noLock && opts.OpenLock == nil {
		return nil, syscall.EINVAL
	}
	dataPath, lockPath := fileNames(path, noSubdir)

	// With a lock file this refuses a second open before the lock file is
	// touched (closing it would drop the first handle's record locks).
	regKey, err := register(path, noSubdir, noLock)
	if err != nil {
		return nil, err
	}
	e := &Env{regKey: regKey, path: path}
	defer func() {
		if err != nil {
			e.release()
		}
	}()

	// Decide the format before the lock file is touched.
	found, hasFound, err := sniffDataFile(dataPath)
	if err != nil {
		return nil, err
	}
	f, err := chooseFormat(found, hasFound, opts.Format)
	if err != nil {
		return nil, err
	}
	layout := f.Layout()
	newPageSize := 0
	if opts.PageSize != 0 {
		if layout.Format != format.V10 || !validSetPageSize(opts.PageSize, layout) {
			return nil, syscall.EINVAL
		}
		newPageSize = opts.PageSize
	}

	if !readOnly && !noLock {
		if e.lock, err = opts.OpenLock(lockPath, mode, maxReaders, false, f); err != nil {
			return nil, err
		}
	}
	if readOnly {
		e.dataFile, err = os.OpenFile(dataPath, os.O_RDONLY, 0)
	} else {
		e.dataFile, err = os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, mode)
	}
	if err != nil {
		return nil, sys.Errno(err)
	}
	// The NoLock handles of one data file in this process share the
	// in-memory region; with a lock file the data file must not be open
	// in the process under another name either.
	shared, err := registerFile(&e.regKey, e.dataFile)
	if err != nil {
		return nil, err
	}
	if readOnly && !noLock {
		if e.lock, err = opts.OpenLock(lockPath, mode, maxReaders, true, f); err != nil {
			return nil, err
		}
	}
	// Without a lock file (MDB_NOLOCK, or OpenLock returned nil for a
	// read-only file system) readers are tracked in memory: NoLock removes
	// only the cross-process locking (AGENTS.md rule 2a). The table has no
	// reader limit, as C's MDB_NOLOCK has none; under NoLock it is the one
	// every handle of the file in this process shares (registry.go).
	e.lockFile = e.lock != nil
	if !e.lockFile {
		e.mem = shared
		if e.mem == nil {
			// A read-only file system: the only handle of the file.
			e.mem = NewMemLock(0)
		}
		e.lock = e.mem
	}

	// mdb_env_open2
	meta, meta0, haveHeader, err := readHeader(e.dataFile, layout)
	// Another NoLock handle of the process may be setting up the same new
	// file (its size grows while the meta pages are written). Without a
	// complete header, wait for that under the file's writer lock and read
	// again; an empty file is then set up under the lock, so one handle
	// writes the meta pages and the others see them. (A goroutine with a
	// write transaction of the file in another handle always finds a
	// complete header, so it never waits here.)
	var setup *MemLock // holds its writer lock until the header is there
	if shared != nil && (err != nil || !haveHeader) {
		setup = shared
		_ = setup.LockWriter(nil)
		defer func() {
			if setup != nil {
				_ = setup.UnlockWriter()
			}
		}()
		meta, meta0, haveHeader, err = readHeader(e.dataFile, layout)
	}
	if err != nil {
		return nil, err
	}
	var pageSize int
	newEnv := !haveHeader
	if haveHeader {
		pageSize = int(meta.PageSize())
	} else {
		// me_os_psize, which mdb_env_set_pagesize replaces in 1.0.
		pageSize = newPageSize
		if pageSize == 0 {
			pageSize = sys.OSPageSize()
		}
		pageSize = min(pageSize, layout.MaxPageSize)
		meta = format.NewEnvMeta(layout, uint32(pageSize), format.DefaultMapSize, flags)
	}
	if pageSize&(pageSize-1) != 0 || pageSize < format.MinPageSize || pageSize > layout.MaxPageSize {
		return nil, format.Invalid
	}
	if layout.Format == format.V10 && !newEnv {
		if err := checkV10Features(e.dataFile, pageSize, meta0.EnvFlags()); err != nil {
			return nil, err
		}
	}
	mapSize := opts.MapSize
	if mapSize == 0 {
		mapSize = int64(meta.MapSize)
	}
	// Make sure mapsize >= committed data size, even when using
	// mm_mapsize, which could be broken in old files (ITS#7789).
	if meta.LastPgno >= uint64(1<<62)/uint64(pageSize) {
		return nil, format.Invalid
	}
	mapSize = max(mapSize, int64(meta.LastPgno+1)*int64(pageSize))
	meta.MapSize = uint64(mapSize)

	if newEnv {
		// mdb_env_init_meta: write both meta pages before mapping, so the
		// file is valid even if the rest of the open fails.
		buf := make([]byte, 2*pageSize)
		meta.WriteInitialPages(buf, pageSize, layout)
		if err := sys.WriteAt(e.dataFile, buf, 0); err != nil {
			return nil, err
		}
	}
	if setup != nil {
		_ = setup.UnlockWriter()
		setup = nil
	}
	e.pageSize = pageSize
	e.flags.Store(flags)
	m, err := e.newMap(mapSize)
	if err != nil {
		return nil, err
	}
	e.dmap.Store(m)
	if !readOnly {
		// Synchronous fd for meta writes. Needed even with
		// MDB_NOSYNC/MDB_NOMETASYNC, in case these get reset.
		if e.metaFile, err = sys.OpenDSync(dataPath); err != nil {
			return nil, err
		}
	}

	e.maxReaders = maxReaders
	if e.lockFile {
		e.maxReaders = e.lock.MaxReaders()
	}
	e.slots.init(e.maxReaders, e.lock.MaxReaders())
	e.dbis.init(opts.MaxDBs)
	e.mapSize.Store(mapSize)
	e.dirtyLimit.Store(format.IDLUMMax)
	e.layout = layout
	e.nodeMax = layout.NodeMax(pageSize)
	e.maxKey = layout.MaxKeySize(pageSize)

	if e.lock.IsExclusive() {
		newest, err := e.pickMeta()
		if err != nil {
			return nil, err
		}
		if err := e.lock.Downgrade(newest.TxnID); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// release drops the environment's reference on its current map (Close,
// or a failed Open). The teardown follows when no transaction holds a map
// any more, at once if none does.
func (e *Env) release() {
	if m := e.dmap.Swap(nil); m != nil {
		m.Release()
	} else if e.liveMaps.Load() == 0 {
		e.teardown()
	}
}

// newMap maps size bytes of the data file; the environment counts the map
// until it is unmapped (mapFreed).
func (e *Env) newMap(size int64) (*sys.DataMap, error) {
	m, err := sys.MapReadOnly(e.dataFile, int(size), e.flags.Load()&format.NoReadahead != 0, format.NumMetas*e.pageSize)
	if err != nil {
		return nil, err
	}
	e.liveMaps.Add(1)
	m.SetOnFree(e.mapFreed)
	return m, nil
}

// mapFreed: a data map was unmapped. While the environment is open it
// holds its current map, and SetMapSize maps the new one before it
// releases the old one, so the count reaches 0 only after Close, when the
// last transaction is gone.
func (e *Env) mapFreed() {
	if e.liveMaps.Add(-1) == 0 {
		e.teardown()
	}
}

// teardown closes what the environment owns, in mdb_env_close0's order
// (the maps are unmapped already): the data file's descriptors, then the
// lock file, and only then the registry entry. No transaction is left, so
// nothing uses the lock region any more.
func (e *Env) teardown() {
	e.teardownOnce.Do(func() {
		if e.metaFile != nil {
			e.metaFile.Close()
		}
		if e.dataFile != nil {
			e.dataFile.Close()
		}
		if e.lock != nil {
			// The pooled slots first (the lock file's Close would clear
			// them too, the in-memory table's would not).
			e.slots.drain(e.lock.ReleaseSlot)
			e.lock.Close()
		}
		unregister(e.regKey)
	})
}

// Close closes the environment (mdb_env_close): no transaction can begin
// or Renew from now on (EINVAL). C requires every transaction to have
// ended before; here transactions still alive keep working until they end
// (lmdb-go's finalizers may end them much later), and their snapshots
// stay protected: the environment keeps its lock file (their reader
// slots, and the lock that tells other processes this one is alive), its
// files and its registry entry (so that the same path cannot be opened
// again in this process, with a reader table of its own) until the last
// of them has ended, and is torn down then. Every transaction holds a
// reference on a data map from its begin to its end, Reset included, and
// takes it before it touches the lock region; the teardown runs when the
// last map is unmapped (mapFreed).
func (e *Env) Close() error {
	e.closed.Store(true)
	e.closeOnce.Do(e.release)
	return nil
}

// pin takes a reference on the current map, which keeps the environment
// from being torn down until it is released; EINVAL after Close.
func (e *Env) pin() (*sys.DataMap, error) {
	m, err := e.acquireMap()
	if err != nil {
		return nil, err
	}
	if e.closed.Load() {
		m.Release()
		return nil, syscall.EINVAL
	}
	return m, nil
}

// --- meta pages --------------------------------------------------------------

// The meta pages are read through a data map the reader holds a
// reference on: a transaction through its own map, anything else through
// the current map with a reference taken for the read (readMeta,
// pickMeta). Loading Env.dmap without a reference would race with
// SetMapSize and Close, which release the map and unmap it once its last
// reference goes.

// readMeta reads meta page i (0 or 1) into a private copy, through the
// current map. Other processes may be rewriting it; the caller decides
// whether the copy is consistent.
func (e *Env) readMeta(i int) (format.Meta, error) {
	m, err := e.acquireMap()
	if err != nil {
		return format.Meta{}, err
	}
	defer m.Release()
	return e.readMetaIn(m, i)
}

// readMetaIn is readMeta through m, on which the caller holds a reference.
func (e *Env) readMetaIn(m *sys.DataMap, i int) (format.Meta, error) {
	var buf [format.MetaSize]byte
	if !m.ReadMeta(i*e.pageSize+e.layout.MetaOffset(), buf[:]) {
		return format.Meta{}, format.Corrupted
	}
	meta, ok := format.ReadMeta(buf[:])
	if !ok || meta.Validate(e.layout) != nil {
		return format.Meta{}, format.Corrupted
	}
	return meta, nil
}

// metaIDsIn is mm_last_pg and mm_txnid of meta page i, read through m (on
// which the caller holds a reference), not validated: what
// mdb_env_write_meta keeps of the page it overwrites (to restore it if the
// write fails).
func (e *Env) metaIDsIn(m *sys.DataMap, i int) (lastPgno, txnid uint64, ok bool) {
	var buf [16]byte
	if !m.ReadMeta(i*e.pageSize+e.layout.MetaOffset()+format.MetaLastPgno, buf[:]) {
		return 0, 0, false
	}
	a, _ := format.Get64(buf[:], 0)
	b, _ := format.Get64(buf[:], 8)
	return a, b, true
}

// pickMeta is mdb_env_pick_meta: the newer of the two meta pages, through
// the current map.
func (e *Env) pickMeta() (format.Meta, error) {
	m, err := e.acquireMap()
	if err != nil {
		return format.Meta{}, err
	}
	defer m.Release()
	return e.pickMetaIn(m)
}

// pickMetaIn is pickMeta through m, on which the caller holds a reference.
func (e *Env) pickMetaIn(m *sys.DataMap) (format.Meta, error) {
	var metas [2]format.Meta
	var err error
	if metas[0], err = e.readMetaIn(m, 0); err != nil {
		return format.Meta{}, err
	}
	if metas[1], err = e.readMetaIn(m, 1); err != nil {
		return format.Meta{}, err
	}
	return metas[format.PickNewest(&metas)], nil
}

// acquireMap returns the current data map with a reference taken.
func (e *Env) acquireMap() (*sys.DataMap, error) {
	for {
		m := e.dmap.Load()
		if m == nil {
			return nil, syscall.EINVAL
		}
		if m.TryAcquire() {
			return m, nil
		}
	}
}

func (e *Env) isReadOnly() bool { return e.flags.Load()&format.ReadOnly != 0 }

// --- public API --------------------------------------------------------------

// Stat is MDB_stat: statistics of a database.
type Stat struct {
	PageSize      uint32
	Depth         uint32
	BranchPages   uint64
	LeafPages     uint64
	OverflowPages uint64
	Entries       uint64
}

func statOf(pageSize int, db *format.DB) Stat {
	return Stat{
		PageSize:      uint32(pageSize),
		Depth:         uint32(db.Depth),
		BranchPages:   db.BranchPages,
		LeafPages:     db.LeafPages,
		OverflowPages: db.OverflowPages,
		Entries:       db.Entries,
	}
}

// Info is MDB_envinfo.
type Info struct {
	// MapAddr is mm_address of the newest meta page (non-zero only for C
	// environments created with MDB_FIXEDMAP); this implementation never
	// maps at a fixed address.
	MapAddr    uint64
	MapSize    int64
	LastPgno   uint64
	LastTxnID  uint64
	MaxReaders uint32
	NumReaders uint32
}

// Stat is mdb_env_stat: the main DB of the newest snapshot.
func (e *Env) Stat() (Stat, error) {
	m, err := e.pickMeta()
	if err != nil {
		return Stat{}, err
	}
	return statOf(e.pageSize, &m.DBs[format.MainDBI]), nil
}

// Info is mdb_env_info.
func (e *Env) Info() (Info, error) {
	pin, err := e.pin()
	if err != nil {
		return Info{}, err
	}
	defer pin.Release()
	m, err := e.pickMetaIn(pin)
	if err != nil {
		return Info{}, err
	}
	in := Info{
		MapAddr:    m.Address,
		MapSize:    e.mapSize.Load(),
		LastPgno:   m.LastPgno,
		LastTxnID:  m.TxnID,
		MaxReaders: e.maxReaders,
	}
	// C: me_numreaders is 0 without a lock file (me_txns == NULL).
	if e.lockFile {
		in.NumReaders = e.lock.NumReaders()
	}
	return in, nil
}

// Sync is mdb_env_sync: flush the data file (fdatasync, F_FULLFSYNC on
// macOS); skipped with MDB_NOSYNC unless force. EACCES when read-only.
func (e *Env) Sync(force bool) error {
	flags := e.flags.Load()
	if flags&format.ReadOnly != 0 {
		return syscall.EACCES
	}
	if force || flags&format.NoSync == 0 {
		return sys.Fdatasync(e.dataFile)
	}
	return nil
}

// Flags is mdb_env_get_flags.
func (e *Env) Flags() uint32 { return e.flags.Load() & (format.Changeable | format.Changeless) }

// SetFlags is mdb_env_set_flags: only MDB_NOSYNC, MDB_NOMETASYNC,
// MDB_MAPASYNC and MDB_NOMEMINIT can change (EINVAL otherwise).
func (e *Env) SetFlags(flags uint32, on bool) error {
	if flags&^format.Changeable != 0 {
		return syscall.EINVAL
	}
	for {
		old := e.flags.Load()
		nf := old &^ flags
		if on {
			nf = old | flags
		}
		if e.flags.CompareAndSwap(old, nf) {
			return nil
		}
	}
}

// Path is mdb_env_get_path.
func (e *Env) Path() string { return e.path }

// MaxReaders is mdb_env_get_maxreaders.
func (e *Env) MaxReaders() uint32 { return e.maxReaders }

// MaxKeySize is mdb_env_get_maxkeysize: 511 for LMDB 0.9; for 1.0 it
// follows from the page size.
func (e *Env) MaxKeySize() int { return e.maxKey }

// Format is the environment's data-file format.
func (e *Env) Format() format.Format { return e.layout.Format }

// PageSize is the page size of the data file.
func (e *Env) PageSize() int { return e.pageSize }

// MapSize is the size of the memory map (me_mapsize).
func (e *Env) MapSize() int64 { return e.mapSize.Load() }

// HasLockFile reports whether the environment uses a lock file (false with
// MDB_NOLOCK or on a read-only file system). Without one, C's
// mdb_reader_list prints "(no reader locks)\n".
func (e *Env) HasLockFile() bool { return e.lockFile }

// DataFile is the data file (mdb_env_get_fd).
func (e *Env) DataFile() *os.File { return e.dataFile }

// SetDirtyLimit lowers the dirty-list capacity for later write
// transactions so tests can make small transactions spill (not C API).
func (e *Env) SetDirtyLimit(pages int) { e.dirtyLimit.Store(int64(max(pages, 16))) }

// SetMapSize is mdb_env_set_mapsize on an open environment: map the data
// file with size bytes (0: the size in the newest meta page), raised to
// cover all pages in use. The next commit persists it. Read transactions
// may be live: they keep the map they began with. EINVAL while this
// process has a write transaction, as in C. Also the way to adopt a larger
// size after another process grew the database (MDB_MAP_RESIZED).
func (e *Env) SetMapSize(size int64) error {
	if !e.writerMu.TryLock() {
		return syscall.EINVAL
	}
	defer e.writerMu.Unlock()
	// The writer lock keeps other SetMapSize calls out; only Close can
	// replace the map meanwhile (with nil).
	cur, err := e.pin()
	if err != nil {
		return err
	}
	defer cur.Release()
	e.writer.hasLastMeta = false
	meta, err := e.pickMetaIn(cur)
	if err != nil {
		return err
	}
	if size == 0 {
		size = int64(meta.MapSize)
	}
	// Silently round up to minimum if the size is too small.
	size = max(size, int64(meta.LastPgno+1)*int64(e.pageSize))
	e.mapMu.Lock()
	defer e.mapMu.Unlock()
	m, err := e.newMap(size)
	if err != nil {
		return err
	}
	if !e.dmap.CompareAndSwap(cur, m) {
		// Closed meanwhile.
		m.Release()
		return syscall.EINVAL
	}
	cur.Release() // the environment's reference
	e.mapSize.Store(size)
	return nil
}

// ReaderList is mdb_reader_list's entries (nil without a lock file, where
// C prints "(no reader locks)"; the in-memory table of NoLock is not
// listed, as C has none).
func (e *Env) ReaderList() []format.ReaderInfo {
	if !e.lockFile {
		return nil
	}
	m, err := e.pin()
	if err != nil {
		return nil
	}
	defer m.Release()
	return e.lock.ReaderList()
}

// ReaderCheck is mdb_reader_check: clear slots of dead processes (0
// without a lock file, as C).
func (e *Env) ReaderCheck() (int, error) {
	if !e.lockFile {
		return 0, nil
	}
	m, err := e.pin()
	if err != nil {
		return 0, err
	}
	defer m.Release()
	return e.lock.ReaderCheck()
}

// CloseDBI is mdb_dbi_close: later use of the handle in any transaction
// fails with MDB_BAD_DBI.
func (e *Env) CloseDBI(dbi DBI) { e.dbis.close(dbi) }

// SetCompare is mdb_set_compare at the environment level: order the keys
// of dbi with cmp (nil: the default) in transactions that start using the
// handle afterwards.
func (e *Env) SetCompare(dbi DBI, cmp format.CmpFunc) error {
	return e.dbis.setComparator(dbi, cmp, false)
}

// SetDupsort is mdb_set_dupsort at the environment level.
func (e *Env) SetDupsort(dbi DBI, cmp format.CmpFunc) error {
	return e.dbis.setComparator(dbi, cmp, true)
}

// FreePages is the number of pages on the free list of the newest
// snapshot (the "Free pages" line of mdb_stat -f).
func (e *Env) FreePages() (uint64, error) {
	txn, err := e.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		return 0, err
	}
	defer txn.Abort()
	c, err := txn.OpenCursor(FreeDBI)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	var pages uint64
	for {
		_, v, err := c.Get(nil, nil, format.Next)
		if errors.Is(err, format.NotFound) {
			return pages, nil
		}
		if err != nil {
			return 0, err
		}
		n, ok := format.EncodedLen(v)
		if !ok || pages+n < pages {
			return 0, format.Corrupted
		}
		pages += n
	}
}

// --- formats -----------------------------------------------------------------

// SniffFormat is the data-file format of the environment at path (a
// directory, or with MDB_NOSUBDIR the data file), from the magic and
// version of its first meta page, without opening the environment. ok is
// false if there is no data file yet or it is empty; VersionMismatch for
// the version 2 files of lmdb-js prerelease builds; Invalid otherwise.
func SniffFormat(path string, flags uint32) (f format.Format, ok bool, err error) {
	dataPath, _ := fileNames(path, flags&format.NoSubdir != 0)
	return sniffDataFile(dataPath)
}

func sniffDataFile(dataPath string) (format.Format, bool, error) {
	f, err := os.Open(dataPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, sys.Errno(err)
	}
	defer f.Close()
	var head [format.SniffLen]byte
	n, err := sys.ReadAt(f, head[:], 0)
	if err != nil {
		return 0, false, err
	}
	return format.SniffFormat(head[:n])
}

// chooseFormat is the format to open with: the existing file's, which must
// be the requested one if any (MDB_VERSION_MISMATCH: no conversion in
// place), else the requested one, else DefaultFormat().
func chooseFormat(found format.Format, hasFound bool, requested format.Format) (format.Format, error) {
	switch {
	case hasFound && requested != 0 && found != requested:
		return 0, format.VersionMismatch
	case hasFound:
		return found, nil
	case requested != 0:
		if requested.Layout() == nil {
			return 0, syscall.EINVAL
		}
		return requested, nil
	}
	return DefaultFormat(), nil
}

// DefaultFormatEnv is the environment variable that sets the format of new
// environments that do not ask for one: lmdb-go v2's LMDBGO_DEFAULT_FORMAT,
// with v2's values ("0.9", "09" or "9"; "1.0" or "10"). Experimental, as
// v2's and rlmdb's RLMDB_DEFAULT_FORMAT, whose semantics it has: a stray
// variable silently creates 1.0 files that C LMDB 0.9 cannot open; prefer
// SetDefaultFormat. Read at every open of a new environment; other values
// are ignored.
const DefaultFormatEnv = "LMDBGO_DEFAULT_FORMAT"

// formatFromEnv parses a value of DefaultFormatEnv as lmdb-go v2 does.
func formatFromEnv(v string) (format.Format, bool) {
	switch v {
	case "0.9", "09", "9":
		return format.V09, true
	case "1.0", "10":
		return format.V10, true
	}
	return 0, false
}

var processDefaultFormat atomic.Uint32

// SetDefaultFormat sets the format of new environments of this process
// that do not ask for one (0 removes it). It takes precedence over
// DefaultFormatEnv. An existing data file always keeps its format.
func SetDefaultFormat(f format.Format) { processDefaultFormat.Store(uint32(f)) }

// ProcessDefaultFormat is the format SetDefaultFormat set, or 0.
func ProcessDefaultFormat() format.Format { return format.Format(processDefaultFormat.Load()) }

// DefaultFormat is the format a new environment gets when it does not ask
// for one: the process default, else DefaultFormatEnv if it names a
// format, else LMDB 0.9.
func DefaultFormat() format.Format {
	if f := ProcessDefaultFormat(); f != 0 {
		return f
	}
	if f, ok := formatFromEnv(os.Getenv(DefaultFormatEnv)); ok {
		return f
	}
	return format.V09
}

// validSetPageSize is the check of 1.0's mdb_env_set_pagesize.
func validSetPageSize(size int, l *format.Layout) bool {
	return size&(size-1) == 0 && size >= format.MinPageSize && size <= l.MaxPageSize
}

// checkV10Features refuses an existing 1.0 environment that uses what is
// not implemented, with 1.0's codes (mdb_env_open2): encryption
// (MDB_ENCRYPT in meta 0's mm_flags) and page checksums (a non-zero
// checksum size in the last two bytes of page 0).
func checkV10Features(f *os.File, pageSize int, meta0Flags uint16) error {
	if uint32(meta0Flags)&format.Encrypt != 0 {
		return format.EnvEncryption
	}
	var sum [2]byte
	n, err := sys.ReadAt(f, sum[:], int64(pageSize-2))
	if err != nil {
		return err
	}
	if n != 2 {
		return format.Invalid
	}
	if v, _ := format.Get16(sum[:], 0); v != 0 {
		return format.BadChecksum
	}
	return nil
}

// readHeader is mdb_env_read_header: read both meta pages with pread and
// return the newer valid one and page 0's; ok is false for an empty file
// (a new environment).
func readHeader(f *os.File, l *format.Layout) (best, first format.Meta, ok bool, err error) {
	size := l.MetaOffset() + format.MetaSize
	buf := make([]byte, size)
	var off int64
	for i := 0; i < format.NumMetas; i++ {
		n, err := sys.ReadAt(f, buf, off)
		if err != nil {
			return best, first, false, err
		}
		if n != size {
			if n == 0 && off == 0 {
				return best, first, false, nil
			}
			return best, first, false, format.Invalid
		}
		m, err := format.ReadMetaPage(buf, l)
		if err != nil {
			return best, first, false, err
		}
		if i == 0 {
			first = m
			best = m
		} else if m.TxnID > best.TxnID {
			best = m
		}
		// As C: the second meta page is at the first one's page size.
		off += int64(best.PageSize())
	}
	return best, first, true, nil
}
