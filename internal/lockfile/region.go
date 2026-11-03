// The open lock file and its reader table: rlmdb core/reader.rs
// (LockRegion). C counterparts, kept exactly: mdb_env_setup_locks (0.9.36
// line 4882) with mdb_env_excl_lock (4741) and mdb_env_share_locks (4698);
// the reader-slot part of mdb_txn_renew0 (2758) and mdb_txn_end;
// mdb_find_oldest (2179); mdb_reader_pid (2718); mdb_reader_list;
// mdb_reader_check0 (10403); mdb_mutex_failed (10463); the lock-file part
// of mdb_env_close0 (5204). LMDB 1.0's versions of these functions are the
// same apart from the layout.

package lockfile

import (
	"errors"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// ErrNoSharedLocks is returned by Open in a Linux or BSD build without cgo:
// the shared writer and reader-table locks are pthread mutexes (Linux,
// FreeBSD) or semaphores (OpenBSD, NetBSD, DragonFly) there, reached
// through cgo. Without cgo only environments without a lock file
// (MDB_NOLOCK) can be used. (macOS and Windows builds have the locks with
// and without cgo.)
var ErrNoSharedLocks = errors.New("lockfile: shared locking on Linux and the BSDs needs a cgo build (CGO_ENABLED=1); without cgo use NoLock")

// SharedLocksAvailable reports whether this build can take the shared
// locks, i.e. whether Open can succeed: true on macOS and Windows, and on
// Linux and the BSDs in a cgo build.
func SharedLocksAvailable() bool { return sharedLocksAvailable }

// Region is an open lock file: its mapping, the two shared locks and this
// process's bookkeeping (mdb.c's me_lfd, me_txns, me_rmutex, me_wmutex,
// me_live_reader, me_close_readers). Its methods may be called from any
// goroutine, except that LockWriter and UnlockWriter pair up on one
// goroutine. After Close, no method may be called.
type Region struct {
	file *os.File
	// fd is the lock file's descriptor (a HANDLE on Windows).
	fd     uintptr
	m      *lockMap
	locks  *sharedLocks
	layout *Layout
	// maxReaders is the number of slots in the mapping (me_maxreaders).
	maxReaders uint32
	pid        int32

	mu sync.Mutex // guards the fields below
	// exclusive: this process still holds the exclusive lock on byte 0.
	exclusive bool
	// liveReader: this process holds the lock on byte pid
	// (me_live_reader; on Windows, where there is no such lock, it has
	// taken a reader slot).
	liveReader bool
	closed     bool

	// closeReaders is the highest mti_numreaders this process has seen
	// (me_close_readers): how many slots Close scans for its leftovers.
	closeReaders atomic.Uint32
	// writers counts LockWriter calls not yet matched by UnlockWriter.
	writers atomic.Int32
}

// Open opens (creating it if needed) and maps the lock file at path with
// the layout layout, as mdb_env_setup_locks does. It returns nil, nil for
// a read-only environment on a read-only file system: C then uses no lock
// file. mode is the permission of a new lock file (and, on macOS, of new
// semaphores); maxReaders the number of reader slots a new table gets.
//
// The first opener, the one that gets the exclusive lock on byte 0 (fcntl,
// on Windows LockFileEx),
// writes a fresh table of layout's version, whatever the file held, and
// keeps that lock until Downgrade. Every other opener waits for a shared
// lock on byte 0 and validates the table: a wrong magic is format.Invalid,
// a mti_format other than layout.LockFormat (the other version, or the
// same version of another platform or libc) is format.VersionMismatch. An
// existing file larger than needed gives more slots (MaxReaders), a
// smaller one fewer, as in C; only the first opener grows it.
//
// Errors are format.Errno values (LMDB's codes), syscall.Errno values
// (possibly wrapped in *os.PathError), or ErrNoSharedLocks.
func Open(path string, mode os.FileMode, maxReaders uint32, readOnly bool, layout *Layout) (*Region, error) {
	if !sharedLocksAvailable {
		return nil, ErrNoSharedLocks
	}
	if layout == nil || maxReaders == 0 {
		return nil, syscall.EINVAL
	}
	f, err := openLockFile(path, mode)
	if err != nil {
		if readOnly && isReadOnlyFS(err) {
			return nil, nil
		}
		return nil, err
	}
	r, err := setup(f, mode, maxReaders, layout)
	if err != nil {
		// Closing the descriptor drops our record locks.
		unlockLockFile(f.Fd())
		f.Close()
		return nil, err
	}
	return r, nil
}

// setup is the body of mdb_env_setup_locks after the open.
func setup(f *os.File, mode os.FileMode, maxReaders uint32, layout *Layout) (*Region, error) {
	fd := f.Fd()
	// mdb_env_excl_lock: exclusive if possible, else wait for shared.
	exclusive := lockByte(fd, 0, true, false) == nil
	if !exclusive {
		if err := lockByte(fd, 0, false, true); err != nil {
			return nil, err
		}
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	length := layout.FileSize(maxReaders)
	if size < length && exclusive {
		if err := f.Truncate(length); err != nil {
			return nil, err
		}
	} else {
		// Use the table of the existing file, whatever was asked for.
		n := layout.ReadersInFile(size)
		if n <= 0 || size > math.MaxInt {
			return nil, format.Invalid
		}
		length, maxReaders = size, uint32(min(n, math.MaxUint32))
	}
	m, err := mapLockFile(fd, int(length))
	if err != nil {
		return nil, err
	}
	var locks *sharedLocks
	if exclusive {
		// Nobody else uses the table: (re)initialise it.
		locks, err = createLocks(m, fd, mode, layout)
		if err == nil {
			m.storeUint32(MagicOffset, Magic)
			m.storeUint32(FormatOffset, layout.LockFormat)
			m.storeUint64(layout.TxnID, 0)
			m.storeUint32(layout.NumReaders, 0)
		}
	} else {
		switch {
		case m.loadUint32(MagicOffset) != Magic:
			err = format.Invalid
		case m.loadUint32(FormatOffset) != layout.LockFormat:
			err = format.VersionMismatch
		default:
			locks, err = attachLocks(m, layout)
		}
	}
	if err != nil {
		m.unmap()
		return nil, err
	}
	return &Region{
		file:       f,
		fd:         fd,
		m:          m,
		locks:      locks,
		layout:     layout,
		maxReaders: maxReaders,
		pid:        int32(os.Getpid()),
		exclusive:  exclusive,
	}, nil
}

// Layout is the region's lock-file layout.
func (r *Region) Layout() *Layout { return r.layout }

// IsExclusive reports whether this process still holds the exclusive lock
// on byte 0, i.e. it initialised the table and has not called Downgrade.
func (r *Region) IsExclusive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exclusive
}

// Downgrade publishes txnid as the last committed transaction (mti_txnid)
// and converts the exclusive lock on byte 0 into a shared one, letting
// other openers in (mdb_env_share_locks; the caller passes the txnid of
// the newest meta page).
func (r *Region) Downgrade(txnid uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m.storeUint64(r.layout.TxnID, txnid)
	if err := downgradeLock(r.fd); err != nil {
		return err
	}
	r.exclusive = false
	return nil
}

// MaxReaders is the number of reader slots in the table (me_maxreaders).
func (r *Region) MaxReaders() uint32 { return r.maxReaders }

// LockWriter blocks until this process owns the writer lock (me_wmutex),
// for a write transaction: other writers, threads or processes, C or Go,
// wait until UnlockWriter.
//
// The writer lock is a robust pthread mutex on Linux and FreeBSD and a
// named mutex on Windows, each of which belongs to the OS thread that
// locked it. LockWriter therefore pins the calling goroutine to its OS
// thread (runtime.LockOSThread; pins nest, so a caller that pinned already
// is fine) until UnlockWriter, which must be called by the same goroutine.
// On macOS and the other BSDs the lock is a semaphore and the pin is not
// needed, but it is kept for the same semantics everywhere. If the
// goroutine exits holding a mutex, its thread ends (Linux, FreeBSD,
// Windows), and the next locker anywhere recovers as from a dead process;
// but Go never ends the main thread, so a goroutine that exits there
// holding the lock leaves it held until the process exits.
//
// If the previous owner died holding the lock (Linux and FreeBSD,
// EOWNERDEAD; Windows, WAIT_ABANDONED; the SysV semaphores of version 2
// lock files on OpenBSD, NetBSD and DragonFly, the flag the holder left
// set), this repairs the state as mdb_mutex_failed does for the writer: it
// publishes newestTxnid() (the txnid of the newest meta page) in mti_txnid,
// so that the next writer cannot overwrite pages that meta refers to,
// clears the slots of dead readers, and marks the mutex consistent. If any
// step fails, the lock is released without being marked consistent, which
// leaves a robust mutex permanently unusable (ENOTRECOVERABLE) rather than
// letting a writer work on unrepaired state (a Windows mutex or a SysV
// semaphore is released normally, as C releases it: rlmdb KNOWN_C_BUGS.md
// row 40).
func (r *Region) LockWriter(newestTxnid func() (uint64, error)) error {
	runtime.LockOSThread()
	st, err := r.locks.w.lock()
	if err != nil {
		runtime.UnlockOSThread()
		return err
	}
	if st == lockOwnerDied {
		if err := r.repairWriter(newestTxnid); err != nil {
			_ = r.locks.w.unlock()
			runtime.UnlockOSThread()
			return err
		}
	}
	r.writers.Add(1)
	return nil
}

// repairWriter is mdb_mutex_failed for the writer mutex.
func (r *Region) repairWriter(newestTxnid func() (uint64, error)) error {
	txnid, err := newestTxnid()
	if err != nil {
		return err
	}
	r.SetTxnID(txnid)
	if _, err := r.readerCheck0(false); err != nil {
		return err
	}
	return r.locks.w.consistent()
}

// UnlockWriter releases the writer lock (UNLOCK_MUTEX(env->me_wmutex)) and
// the OS-thread pin of LockWriter. It must be called by the goroutine that
// called LockWriter; on Linux another thread gets EPERM (on Windows
// ERROR_NOT_OWNER) and the lock stays held.
func (r *Region) UnlockWriter() error {
	if err := r.locks.w.unlock(); err != nil {
		return err
	}
	r.writers.Add(-1)
	runtime.UnlockOSThread()
	return nil
}

// TxnID is mti_txnid: the last committed transaction.
func (r *Region) TxnID() uint64 { return r.m.loadUint64(r.layout.TxnID) }

// SetTxnID sets mti_txnid (by a writer after its commit, and when
// recovering from a dead writer).
func (r *Region) SetTxnID(txnid uint64) { r.m.storeUint64(r.layout.TxnID, txnid) }

// NumReaders is mti_numreaders as stored: the high-water mark of used
// slots (me_numreaders of mdb_env_info). It may exceed MaxReaders in a
// damaged file; the slot scans clamp it.
func (r *Region) NumReaders() uint32 { return r.m.loadUint32(r.layout.NumReaders) }

// numReaders is mti_numreaders clamped to the slots we have mapped, so a
// damaged value cannot make us read outside the table.
func (r *Region) numReaders() int { return int(min(r.NumReaders(), r.maxReaders)) }

func (r *Region) slotPID(i int) int32 { return r.m.loadInt32(r.layout.ReaderOffset(i) + ReaderPID) }

func (r *Region) setSlotPID(i int, pid int32) {
	r.m.storeInt32(r.layout.ReaderOffset(i)+ReaderPID, pid)
}

// slotTID is mr_tid of slot i: a pthread_t (8 bytes) on Unix, a DWORD on
// Windows.
func (r *Region) slotTID(i int) uint64 {
	off := r.layout.ReaderOffset(i) + ReaderTID
	if ReaderTIDSize == 4 {
		return uint64(r.m.loadUint32(off))
	}
	return r.m.loadUint64(off)
}

func (r *Region) setSlotTID(i int, tid uint64) {
	off := r.layout.ReaderOffset(i) + ReaderTID
	if ReaderTIDSize == 4 {
		r.m.storeUint32(off, uint32(tid))
		return
	}
	r.m.storeUint64(off, tid)
}

// SlotTxnID is mr_txnid of slot i.
func (r *Region) SlotTxnID(i int) uint64 {
	return r.m.loadUint64(r.layout.ReaderOffset(i) + ReaderTxnID)
}

// SetSlotTxnID sets mr_txnid of slot i (a reader pinning a snapshot).
func (r *Region) SetSlotTxnID(i int, txnid uint64) {
	r.m.storeUint64(r.layout.ReaderOffset(i)+ReaderTxnID, txnid)
}

// lockReaders takes the reader-table mutex (me_rmutex), pinned to the OS
// thread until unlockReaders, repairing the table if its previous owner
// died (LOCK_MUTEX with mdb_mutex_failed for the reader mutex: check all
// readers, mark consistent).
func (r *Region) lockReaders() error {
	runtime.LockOSThread()
	st, err := r.locks.r.lock()
	if err != nil {
		runtime.UnlockOSThread()
		return err
	}
	if st == lockOwnerDied {
		_, err = r.readerCheck0(true)
		if err == nil {
			err = r.locks.r.consistent()
		}
		if err != nil {
			r.unlockReaders()
			return err
		}
	}
	return nil
}

func (r *Region) unlockReaders() {
	_ = r.locks.r.unlock()
	runtime.UnlockOSThread()
}

// AcquireSlot claims a free reader slot for this process, as
// mdb_txn_renew0 does for a read transaction without one, and returns its
// index. The slot's txnid is IdleTxnID (owned, not in a transaction) until
// PublishCurrentTxnID. format.ReadersFull if every slot is taken.
//
// This is the only reader operation that takes the reader mutex; a slot,
// once owned, is reused for later transactions without it (Reset,
// Publish), as in C and rlmdb.
func (r *Region) AcquireSlot() (int, error) {
	r.mu.Lock()
	if !r.liveReader {
		// mdb_reader_pid(Pidset): advertise that this process is alive
		// (nothing on Windows).
		if err := setPIDLock(r.fd, r.pid); err != nil {
			r.mu.Unlock()
			return 0, err
		}
		r.liveReader = true
	}
	r.mu.Unlock()

	if err := r.lockReaders(); err != nil {
		return 0, err
	}
	defer r.unlockReaders()
	nr := r.numReaders()
	i := 0
	for i < nr && r.slotPID(i) != 0 {
		i++
	}
	if i >= int(r.maxReaders) {
		return 0, format.ReadersFull
	}
	// Claim the slot carefully, since other code uses the table without
	// the mutex: first reset it, next publish it in mti_numreaders. After
	// that it is safe for an environment close to touch it. Writing our
	// pid finally claims it.
	r.setSlotPID(i, 0)
	r.SetSlotTxnID(i, IdleTxnID)
	r.setSlotTID(i, threadID())
	if i == nr {
		nr++
		r.m.storeUint32(r.layout.NumReaders, uint32(nr))
	}
	for {
		cur := r.closeReaders.Load()
		if uint32(nr) <= cur || r.closeReaders.CompareAndSwap(cur, uint32(nr)) {
			break
		}
	}
	r.setSlotPID(i, r.pid)
	return i, nil
}

// SlotIsIdleAndOurs reports whether slot i is still this process's and
// idle (mr_pid == me_pid and mr_txnid == (txnid_t)-1); mdb_txn_renew0
// returns MDB_BAD_RSLOT otherwise.
func (r *Region) SlotIsIdleAndOurs(i int) bool {
	return r.slotPID(i) == r.pid && r.SlotTxnID(i) == IdleTxnID
}

// PublishCurrentTxnID stores the current mti_txnid in slot i and returns
// it, re-reading until both agree (ITS#7970: a writer may commit between
// our read and our store).
func (r *Region) PublishCurrentTxnID(i int) uint64 {
	for {
		t := r.TxnID()
		r.SetSlotTxnID(i, t)
		if r.TxnID() == t {
			return t
		}
	}
}

// ResetSlot marks slot i as owned but not in a transaction
// (mdb_txn_reset).
func (r *Region) ResetSlot(i int) { r.SetSlotTxnID(i, IdleTxnID) }

// ReleaseSlot gives slot i up (mdb_txn_end with MDB_END_SLOT).
func (r *Region) ReleaseSlot(i int) {
	r.SetSlotTxnID(i, IdleTxnID)
	r.setSlotPID(i, 0)
}

// FindOldest is mdb_find_oldest: the oldest snapshot any reader still
// uses, or txnid-1 if none is older.
func (r *Region) FindOldest(txnid uint64) uint64 {
	oldest := txnid - 1
	for i := r.numReaders() - 1; i >= 0; i-- {
		if r.slotPID(i) != 0 {
			oldest = min(oldest, r.SlotTxnID(i))
		}
	}
	return oldest
}

// ReaderList returns the occupied slots, as mdb_reader_list walks them
// (format.FormatReaderList prints them as C does).
func (r *Region) ReaderList() []format.ReaderInfo {
	var list []format.ReaderInfo
	for i := 0; i < r.numReaders(); i++ {
		pid := r.slotPID(i)
		if pid == 0 {
			continue
		}
		txnid := r.SlotTxnID(i)
		list = append(list, format.ReaderInfo{
			PID:    pid,
			Thread: r.slotTID(i),
			TxnID:  txnid,
			InTxn:  txnid != IdleTxnID,
		})
	}
	return list
}

// ReaderCheck is mdb_reader_check: it clears the slots of processes that
// died without cleaning up and returns how many it cleared.
func (r *Region) ReaderCheck() (int, error) { return r.readerCheck0(false) }

// pidAlive is mdb_reader_pid(Pidcheck): false only if the process is
// certainly dead (nobody holds the lock on byte pid; on Windows, the
// process does not exist any more).
func (r *Region) pidAlive(pid int32) bool { return pidAlive(r.fd, pid) }

// readerCheck0 is mdb_reader_check0; rlocked is set if the caller holds the
// reader mutex already.
func (r *Region) readerCheck0(rlocked bool) (int, error) {
	rdrs := r.numReaders()
	var checked []int32 // sorted, as mdb_pid_insert keeps them
	count := 0
	for i := 0; i < rdrs; i++ {
		pid := r.slotPID(i)
		if pid == 0 || pid == r.pid {
			continue
		}
		at := sort.Search(len(checked), func(k int) bool { return checked[k] >= pid })
		if at < len(checked) && checked[at] == pid {
			continue
		}
		checked = append(checked, 0)
		copy(checked[at+1:], checked[at:])
		checked[at] = pid
		if r.pidAlive(pid) {
			continue
		}
		// Stale reader found.
		j := i
		if !rlocked {
			runtime.LockOSThread()
			st, err := r.locks.r.lock()
			if err != nil {
				runtime.UnlockOSThread()
				return 0, err
			}
			if st == lockOwnerDied {
				// mdb_mutex_failed checks all readers itself.
				_, err = r.readerCheck0(true)
				if err == nil {
					err = r.locks.r.consistent()
				}
				if err != nil {
					r.unlockReaders()
					return 0, err
				}
				rdrs = 0
			} else if r.pidAlive(pid) {
				// Recheck: a new process may have reused the pid.
				j = rdrs
			}
		}
		for ; j < rdrs; j++ {
			if r.slotPID(j) == pid {
				r.setSlotPID(j, 0)
				count++
			}
		}
		if !rlocked {
			r.unlockReaders()
		}
	}
	return count, nil
}

// Header returns a copy of the lock-file header, read through the mapping
// (reading the file through another descriptor would drop this process's
// record locks when that descriptor is closed).
func (r *Region) Header() HeaderSnapshot {
	h, _ := ReadHeader(r.m.readBytes(0, r.layout.Readers), r.layout)
	return h
}

// Close is the lock-file part of mdb_env_close0: it clears the slots this
// process still owns, closes the shared locks (on macOS also removing the
// semaphores if no other process uses the environment), unmaps the table
// and closes the file, which releases this process's record locks (on
// Windows they are released explicitly first, as C does).
//
// If a goroutine still holds the writer lock (LockWriter without
// UnlockWriter) and the lock is a robust mutex in the file (Linux,
// FreeBSD), the mapping is left in place: the holding thread's robust list
// points into it, and the kernel reads that list when the thread exits.
// Other locks are not in the mapping (semaphores, Windows mutexes), so it
// is removed as C removes it; on Windows a mapped file cannot be deleted.
func (r *Region) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()

	// Without the reader mutex, as C: only our own slots, each cleared
	// atomically.
	for i := int(min(r.closeReaders.Load(), r.maxReaders)) - 1; i >= 0; i-- {
		if r.slotPID(i) == r.pid {
			r.setSlotPID(i, 0)
		}
	}
	// With semaphores, the last user removes them: that is, we can (still)
	// take the exclusive lock on byte 0.
	lastUser := r.layout.Objects.removedByLastUser() && lockByte(r.fd, 0, true, false) == nil
	r.locks.close(lastUser)
	var err error
	if r.writers.Load() == 0 || r.layout.Objects != PthreadMutexes {
		err = r.m.unmap()
	}
	// Windows releases the lock on byte 0 now rather than "not necessarily
	// at once" after the handle closes (mdb_env_close0).
	unlockLockFile(r.fd)
	if cerr := r.file.Close(); err == nil {
		err = cerr
	}
	return err
}
