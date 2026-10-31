// Layout of the lock file, MDB_txninfo plus the reader table, in both
// lock-file versions: rlmdb format/lockfile.rs (LockLayout, LOCK_V09,
// LOCK_V10); mdb.c's MDB_txbody, MDB_txninfo, MDB_reader and
// MDB_LOCK_FORMAT (0.9.36 lines 668-790; LMDB 1.0, mdb.RE/1.0 at 4865523,
// lines 865-1017 with MDB_lock_desc).
//
//	Version 1 (LMDB 0.9), MDB_txninfo:
//	  mt1 (padded to a multiple of CACHELINE = 64):
//	    u32  mtb_magic        MDB_MAGIC
//	    u32  mtb_format       MDB_LOCK_FORMAT (0x10001 on Unix)
//	    [MNAME_LEN] mtb_rmutex | mtb_rmname
//	    u64  mtb_txnid        last committed txn (aligned to 8)
//	    u32  mtb_numreaders   high-water mark of used reader slots
//	  mt2 (MNAME_LEN padded to a multiple of 64):
//	    [MNAME_LEN] mt2_wmutex | mt2_wmname
//	  MDB_reader mti_readers[maxreaders]
//
//	Version 2 (LMDB 1.0), MDB_txninfo:
//	  mt1 (padded to a multiple of 64):
//	    u32  mtb_magic
//	    u32  mtb_format       2 + (MDB_lock_desc << 12), per platform
//	    u64  mtb_txnid
//	    u32  mtb_numreaders
//	    pthread_mutex_t mtb_rmutex | u64 mtb_mutexid (aligned)
//	  mt2 (Linux only; sizeof(pthread_mutex_t) padded to a multiple of 64):
//	    pthread_mutex_t mt2_wmutex
//	  MDB_reader mti_readers[maxreaders]
//
//	MDB_reader, both versions (64 bytes each):
//	  u64  mr_txnid  (!0 when the slot is owned but not in a txn)
//	  i32  mr_pid    (0 when the slot is free)
//	  u64  mr_tid    (pthread_t, at offset 16)
//
// MNAME_LEN is sizeof(pthread_mutex_t) with mutexes (glibc: 40 on x86-64,
// 48 on aarch64; musl: 40) and 32 with named semaphores (macOS). The
// numbers, which tests check against C's layout_dump on each platform:
//
//	                      macOS v1  macOS v2    glibc arm64 v1  v2          40-byte mutex v1  v2
//	mti_format            0x10001   0x28570002  0x10001         0x2cd82002  0x10001           0x2c2f6002
//	txnid, numreaders     40, 48    8, 16       56, 64          8, 16       48, 56            8, 16
//	reader lock off, size 8, 32     24, 8       8, 48           24, 48      8, 40             24, 40
//	writer lock           64        none        128             128         64                64
//	mti_readers           128       64          192             192         128               128
//
// ("40-byte mutex": glibc x86-64 and musl on both architectures.)
//
// The layouts are computed with C's formulas from the platform's inputs
// (platform below), so every platform's layout can be checked on any host.

package lockfile

import (
	"encoding/binary"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// ne reads integers as C stores them: in the CPU's byte order.
var ne = binary.NativeEndian

// Objects says how the two shared locks of an environment are kept in the
// lock file (MDB_USE_POSIX_MUTEX or MDB_USE_POSIX_SEM in mdb.c).
type Objects int

const (
	// PthreadMutexes: robust process-shared pthread_mutex_ts at RLock and
	// WLock (Linux; both versions).
	PthreadMutexes Objects = iota + 1
	// SemaphoreNames: named semaphores whose NUL-terminated names are stored
	// at RLock and WLock (macOS, version 1: mtb_rmname, mt2_wmname). The
	// names are read, never derived: C 0.9 hashes a buffer with
	// uninitialised padding to make them.
	SemaphoreNames
	// SemaphoreHash: named semaphores named after the 64-bit hash
	// mtb_mutexid at RLock (macOS, version 2): "/MDB", 'r' or 'w', then
	// mdb_pack85 of the hash (MUTEXNAME, mdb.c 1.0 lines 5920-5945). The
	// hash is read from the file; only the exclusive opener computes it.
	SemaphoreHash
)

// Layout says where the fields of one lock-file version are on this
// platform. mti_magic and mti_format are at MagicOffset and FormatOffset,
// and reader slots look the same, in both versions.
type Layout struct {
	// Format is the data format whose lock file this is.
	Format format.Format
	// Version is MDB_LOCK_VERSION: 1 or 2.
	Version uint32
	// LockFormat is MDB_LOCK_FORMAT, the value of mti_format. Version 2
	// encodes the platform in it.
	LockFormat uint32
	// Objects says how the shared locks are kept.
	Objects Objects
	// TxnID is the offset of mti_txnid (u64).
	TxnID int
	// NumReaders is the offset of mti_numreaders (u32).
	NumReaders int
	// RLock is the offset of the reader lock field: mti_rmutex (Linux),
	// mti_rmname (macOS, version 1) or mti_mutexid (macOS, version 2).
	RLock int
	// RLockSize is the size of the reader lock field.
	RLockSize int
	// WLock is the offset of the writer lock field mti_wmutex or
	// mti_wmname (RLockSize bytes), -1 where there is none (macOS,
	// version 2).
	WLock int
	// TxbodySize is sizeof(MDB_txbody).
	TxbodySize int
	// HeaderSize is the size of the mt1 union: MDB_txbody padded to a cache
	// line multiple.
	HeaderSize int
	// Readers is the offset of mti_readers[0].
	Readers int
}

// Constants of both versions.
const (
	// Magic is MDB_MAGIC, the value of mti_magic.
	Magic uint32 = 0xBEEFC0DE
	// MagicOffset is the offset of mti_magic (u32).
	MagicOffset = 0
	// FormatOffset is the offset of mti_format (u32).
	FormatOffset = 4
	// ReaderSize is sizeof(MDB_reader): one cache line.
	ReaderSize = 64
	// ReaderTxnID is the offset of mr_txnid (u64) in a reader slot.
	ReaderTxnID = 0
	// ReaderPID is the offset of mr_pid (pid_t, i32) in a reader slot.
	ReaderPID = 8
	// ReaderTID is the offset of mr_tid (pthread_t, 8 bytes) in a reader
	// slot.
	ReaderTID = 16
	// IdleTxnID is mr_txnid of a slot that is owned but not in a
	// transaction ((txnid_t)-1).
	IdleTxnID = ^uint64(0)

	cacheLine       = 64 // CACHELINE
	pidLock         = 1  // MDB_PIDLOCK: readers take a pid lock in the lock file
	lockVersionV1   = 1  // MDB_LOCK_VERSION of LMDB 0.9
	lockVersionV2   = 2  // MDB_LOCK_VERSION of LMDB 1.0
	lockVersionBits = 12 // MDB_LOCK_VERSION_BITS (1.0)
	semNameLen      = 32 // MNAME_LEN with MDB_USE_POSIX_SEM
)

// lockFormatV1 is MDB_LOCK_FORMAT of LMDB 0.9: MDB_LOCK_VERSION +
// (MDB_PIDLOCK << 16).
const lockFormatV1 = lockVersionV1 + pidLock<<16

// LockV09 and LockV10 are this platform's layouts of lock-file version 1
// (LMDB 0.9) and 2 (LMDB 1.0); nil where this build does not know them
// (Linux without cgo, where sizeof(pthread_mutex_t) is unknown).
var LockV09, LockV10 = hostPlatform.layouts()

// LayoutFor returns this platform's lock-file layout for the data format f
// (Layout::of in rlmdb): version 1 for LMDB 0.9, version 2 for LMDB 1.0.
// nil for an unknown format, or where this build does not know the layout.
func LayoutFor(f format.Format) *Layout {
	switch f {
	case format.V09:
		return LockV09
	case format.V10:
		return LockV10
	}
	return nil
}

// FileSize is the size of a lock file with room for maxReaders slots
// (rsize in mdb_env_setup_locks: (maxreaders-1) * sizeof(MDB_reader) +
// sizeof(MDB_txninfo)).
func (l *Layout) FileSize(maxReaders uint32) int64 {
	return int64(l.Readers) + int64(maxReaders)*ReaderSize
}

// ReadersInFile is the number of reader slots in an existing lock file of
// size bytes, as mdb_env_setup_locks computes me_maxreaders for it; 0 if
// the file is too short for the header and one slot.
func (l *Layout) ReadersInFile(size int64) int64 {
	if size < int64(l.Readers) {
		return 0
	}
	return (size - int64(l.Readers)) / ReaderSize
}

// ReaderOffset is the offset of reader slot i.
func (l *Layout) ReaderOffset(i int) int { return l.Readers + i*ReaderSize }

// platform holds what the lock-file layout of a C build depends on.
type platform struct {
	// namedSemaphores: MDB_USE_POSIX_SEM (macOS); else robust mutexes.
	namedSemaphores bool
	// mutexSize, mutexAlign: sizeof and alignof(pthread_mutex_t), for
	// mutexes; 0 if unknown.
	mutexSize, mutexAlign int
	// hashAlign is alignof(mdb_hash_t) (unsigned long long), for named
	// semaphores in version 2.
	hashAlign int
	// pidSize, pthreadSize: sizeof(pid_t), sizeof(pthread_t).
	pidSize, pthreadSize int
}

func alignUp(n, a int) int { return (n + a - 1) / a * a }

// log2Mod is LOG2_MOD(p2, n) of mdb.c 1.0 (line 779): log2(p2) % n for a
// power of two p2 and 0 < n < 8, computed the way C does.
func log2Mod(p2, n int) int { return 7 - 86/(p2%((1<<n)-1)+11) }

// lockTypePosixSem is MDB_LOCK_TYPE with MDB_USE_POSIX_SEM (mdb.c 1.0 line
// 980): 4 + ALIGNOF2(mdb_hash_t)/8 % 2.
func lockTypePosixSem(hashAlign int) int { return 4 + hashAlign/8%2 }

// lockTypePosixMutex is MDB_LOCK_TYPE with MDB_USE_POSIX_MUTEX (mdb.c 1.0
// line 987): only the size and alignment of pthread_mutex_t count,
// 10 + LOG2_MOD(ALIGNOF2(pthread_mutex_t), 5) + sizeof(pthread_mutex_t) / 4 % 22 * 5.
func lockTypePosixMutex(size, align int) int { return 10 + log2Mod(align, 5) + size/4%22*5 }

// lockDescInputs is what MDB_lock_desc (mdb.c 1.0 lines 1004-1017) is
// computed from.
type lockDescInputs struct {
	cacheline, pidSize, pthreadSize, txbodySize, lockType, txnidSize, readerSize int
	pidlock                                                                      bool
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// lockDesc is MDB_lock_desc, term by term as in mdb.c 1.0.
func lockDesc(d lockDescInputs) uint32 {
	cacheTerm := 0
	if d.cacheline != 64 {
		cacheTerm = 1 + log2Mod(d.cacheline>>b2i(d.cacheline > 64), 5)
	}
	return uint32(cacheTerm +
		6*(d.pidSize/4%3) +
		18*(d.pthreadSize/4%5) +
		90*(d.txbodySize/d.cacheline%3) +
		270*(d.lockType%120) +
		b2i(d.txnidSize == 8)<<15 +
		b2i(d.readerSize > d.cacheline)<<16 +
		b2i(d.pidlock)<<17)
}

// lockFormatV2 is MDB_LOCK_FORMAT of LMDB 1.0 (line 969): the version in
// the low MDB_LOCK_VERSION_BITS, MDB_lock_desc above.
func lockFormatV2(d lockDescInputs) uint32 {
	return lockVersionV2%(1<<lockVersionBits) + lockDesc(d)<<lockVersionBits
}

// layouts computes both lock-file layouts of the platform; nil, nil if the
// platform's mutex size is unknown.
func (p platform) layouts() (v09, v10 *Layout) {
	if !p.namedSemaphores && (p.mutexSize <= 0 || p.mutexAlign <= 0) {
		return nil, nil
	}
	// Version 1.
	mnameLen, nameAlign, objects := p.mutexSize, p.mutexAlign, PthreadMutexes
	if p.namedSemaphores {
		mnameLen, nameAlign, objects = semNameLen, 1, SemaphoreNames
	}
	rlock := alignUp(8, nameAlign)
	txnid := alignUp(rlock+mnameLen, 8)
	txbody := alignUp(txnid+8+4, max(8, nameAlign))
	header := alignUp(txbody, cacheLine)
	v09 = &Layout{
		Format:     format.V09,
		Version:    lockVersionV1,
		LockFormat: lockFormatV1,
		Objects:    objects,
		TxnID:      txnid,
		NumReaders: txnid + 8,
		RLock:      rlock,
		RLockSize:  mnameLen,
		WLock:      header,
		TxbodySize: txbody,
		HeaderSize: header,
		Readers:    header + alignUp(mnameLen, cacheLine),
	}

	// Version 2: txnid and numreaders moved in front of the reader lock.
	rsize, ralign, objects, lockType := p.mutexSize, p.mutexAlign, PthreadMutexes, 0
	if p.namedSemaphores {
		rsize, ralign, objects = 8, p.hashAlign, SemaphoreHash
		lockType = lockTypePosixSem(p.hashAlign)
	} else {
		lockType = lockTypePosixMutex(p.mutexSize, p.mutexAlign)
	}
	const v2TxnID, v2NumReaders = 8, 16
	rlock = alignUp(v2NumReaders+4, ralign)
	txbody = alignUp(rlock+rsize, max(8, ralign))
	header = alignUp(txbody, cacheLine)
	wlock, readers := header, header+alignUp(p.mutexSize, cacheLine)
	if p.namedSemaphores {
		// No mt2: the writer semaphore is found by name.
		wlock, readers = -1, header
	}
	v10 = &Layout{
		Format:  format.V10,
		Version: lockVersionV2,
		LockFormat: lockFormatV2(lockDescInputs{
			cacheline:   cacheLine,
			pidSize:     p.pidSize,
			pthreadSize: p.pthreadSize,
			txbodySize:  txbody,
			lockType:    lockType,
			txnidSize:   8,
			readerSize:  ReaderSize,
			pidlock:     pidLock != 0,
		}),
		Objects:    objects,
		TxnID:      v2TxnID,
		NumReaders: v2NumReaders,
		RLock:      rlock,
		RLockSize:  rsize,
		WLock:      wlock,
		TxbodySize: txbody,
		HeaderSize: header,
		Readers:    readers,
	}
	return v09, v10
}

// HeaderSnapshot is a decoded copy of a lock-file header, for diagnostics
// and tests (live access goes through Region).
type HeaderSnapshot struct {
	Magic      uint32 // mti_magic
	Format     uint32 // mti_format
	TxnID      uint64 // mti_txnid
	NumReaders uint32 // mti_numreaders
	// RName and WName are mti_rmname and mti_wmname up to their NUL
	// (SemaphoreNames only).
	RName, WName []byte
	// MutexID is mti_mutexid, the hash the semaphore names are derived
	// from (SemaphoreHash only).
	MutexID uint64
}

// ReadHeader decodes the header of a lock file of layout l from a copy of
// its first bytes; false if buf is too short.
func ReadHeader(buf []byte, l *Layout) (HeaderSnapshot, bool) {
	if len(buf) < l.Readers {
		return HeaderSnapshot{}, false
	}
	h := HeaderSnapshot{
		Magic:      ne.Uint32(buf[MagicOffset:]),
		Format:     ne.Uint32(buf[FormatOffset:]),
		TxnID:      ne.Uint64(buf[l.TxnID:]),
		NumReaders: ne.Uint32(buf[l.NumReaders:]),
	}
	switch l.Objects {
	case SemaphoreNames:
		h.RName = cName(buf[l.RLock : l.RLock+l.RLockSize])
		h.WName = cName(buf[l.WLock : l.WLock+l.RLockSize])
	case SemaphoreHash:
		h.MutexID = ne.Uint64(buf[l.RLock:])
	}
	return h, true
}

// ReaderSnapshot is a decoded copy of one reader slot.
type ReaderSnapshot struct {
	TxnID uint64 // mr_txnid
	PID   int32  // mr_pid
	TID   uint64 // mr_tid
}

// ReadReader decodes slot i from a copy of a lock file of layout l; false
// if buf is too short.
func ReadReader(buf []byte, l *Layout, i int) (ReaderSnapshot, bool) {
	if i < 0 || int64(i) >= l.ReadersInFile(int64(len(buf))) {
		return ReaderSnapshot{}, false
	}
	s := buf[l.ReaderOffset(i):]
	return ReaderSnapshot{
		TxnID: ne.Uint64(s[ReaderTxnID:]),
		PID:   int32(ne.Uint32(s[ReaderPID:])),
		TID:   ne.Uint64(s[ReaderTID:]),
	}, true
}

// cName returns b up to its first NUL (all of b if there is none).
func cName(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return append([]byte(nil), b[:i]...)
		}
	}
	return append([]byte(nil), b...)
}
