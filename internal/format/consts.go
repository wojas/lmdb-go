// The numeric constants of C LMDB 0.9 (lmdb.h and mdb.c, 0.9.37): flag
// bits, cursor operation numbers and format limits. Port of rlmdb's
// consts.rs. Everything has exactly the value C uses; the on-disk and
// lock-file formats depend on several of them, so they must never be
// "tidied up".

package format

// Environment flags (mdb_env_open), lmdb.h "mdb_env Environment Flags".
const (
	FixedMap     uint32 = 0x01      // MDB_FIXEDMAP: accepted, ignored
	NoSubdir     uint32 = 0x4000    // MDB_NOSUBDIR
	NoSync       uint32 = 0x10000   // MDB_NOSYNC
	ReadOnly     uint32 = 0x20000   // MDB_RDONLY
	NoMetaSync   uint32 = 0x40000   // MDB_NOMETASYNC
	WriteMap     uint32 = 0x80000   // MDB_WRITEMAP: accepted, ignored
	MapAsync     uint32 = 0x100000  // MDB_MAPASYNC: accepted, ignored
	NoTLS        uint32 = 0x200000  // MDB_NOTLS: always the behaviour here
	NoLock       uint32 = 0x400000  // MDB_NOLOCK
	NoReadahead  uint32 = 0x800000  // MDB_NORDAHEAD
	NoMemInit    uint32 = 0x1000000 // MDB_NOMEMINIT: accepted, ignored
	PrevSnapshot uint32 = 0x2000000 // MDB_PREVSNAPSHOT (1.0): rejected

	// Changeable are the env flags that may be changed on an open
	// environment (CHANGEABLE).
	Changeable = NoSync | NoMetaSync | MapAsync | NoMemInit
	// Changeless are the env flags that can only be given at open time
	// (CHANGELESS).
	Changeless = FixedMap | NoSubdir | ReadOnly | WriteMap | NoTLS | NoLock | NoReadahead
)

// Database flags (mdb_dbi_open), lmdb.h "mdb_dbi_open Database Flags".
const (
	ReverseKey uint32 = 0x02    // MDB_REVERSEKEY
	DupSort    uint32 = 0x04    // MDB_DUPSORT
	IntegerKey uint32 = 0x08    // MDB_INTEGERKEY
	DupFixed   uint32 = 0x10    // MDB_DUPFIXED
	IntegerDup uint32 = 0x20    // MDB_INTEGERDUP
	ReverseDup uint32 = 0x40    // MDB_REVERSEDUP
	Create     uint32 = 0x40000 // MDB_CREATE

	// Valid marks a valid entry in the environment's DBI table (MDB_VALID).
	Valid uint32 = 0x8000
	// PersistentFlags are the DB flags stored in MDB_db.md_flags.
	PersistentFlags uint32 = 0xffff &^ Valid
	// ValidDBFlags are the flags mdb_dbi_open accepts (VALID_FLAGS).
	ValidDBFlags = ReverseKey | DupSort | IntegerKey | DupFixed | IntegerDup | ReverseDup | Create
)

// Write flags (mdb_put, mdb_cursor_put), lmdb.h "mdb_put Write Flags".
const (
	NoOverwrite uint32 = 0x10    // MDB_NOOVERWRITE
	NoDupData   uint32 = 0x20    // MDB_NODUPDATA
	Current     uint32 = 0x40    // MDB_CURRENT
	Reserve     uint32 = 0x10000 // MDB_RESERVE
	Append      uint32 = 0x20000 // MDB_APPEND
	AppendDup   uint32 = 0x40000 // MDB_APPENDDUP
	Multiple    uint32 = 0x80000 // MDB_MULTIPLE

	// CopyCompact is MDB_CP_COMPACT (mdb_env_copy2).
	CopyCompact uint32 = 0x01
)

// Encrypt is MDB_ENCRYPT, LMDB 1.0's env flag recorded in mm_flags of an
// encrypted environment (refused here).
const Encrypt uint32 = 0x2000

// CursorOp is an MDB_cursor_op value.
type CursorOp uint32

// Cursor operations, in enum order (MDB_cursor_op).
const (
	First          CursorOp = 0  // MDB_FIRST
	FirstDup       CursorOp = 1  // MDB_FIRST_DUP
	GetBoth        CursorOp = 2  // MDB_GET_BOTH
	GetBothRange   CursorOp = 3  // MDB_GET_BOTH_RANGE
	GetCurrent     CursorOp = 4  // MDB_GET_CURRENT
	GetMultiple    CursorOp = 5  // MDB_GET_MULTIPLE
	Last           CursorOp = 6  // MDB_LAST
	LastDup        CursorOp = 7  // MDB_LAST_DUP
	Next           CursorOp = 8  // MDB_NEXT
	NextDup        CursorOp = 9  // MDB_NEXT_DUP
	NextMultiple   CursorOp = 10 // MDB_NEXT_MULTIPLE
	NextNoDup      CursorOp = 11 // MDB_NEXT_NODUP
	Prev           CursorOp = 12 // MDB_PREV
	PrevDup        CursorOp = 13 // MDB_PREV_DUP
	PrevNoDup      CursorOp = 14 // MDB_PREV_NODUP
	Set            CursorOp = 15 // MDB_SET
	SetKey         CursorOp = 16 // MDB_SET_KEY
	SetRange       CursorOp = 17 // MDB_SET_RANGE
	PrevMultiple   CursorOp = 18 // MDB_PREV_MULTIPLE
	cursorOpsCount          = 19
)

// Valid reports whether op is one of the operations above.
func (op CursorOp) Valid() bool { return op < cursorOpsCount }

// Format constants from mdb.c.
const (
	// Magic is MDB_MAGIC, at the start of the meta and of the lock file.
	Magic uint32 = 0xBEEFC0DE
	// DataVersion09 is MDB_DATA_VERSION of LMDB 0.9 (1.0's is 3, see
	// Layout.DataVersion).
	DataVersion09 uint32 = 1
	// MaxKeySize09 is MDB_MAXKEYSIZE, the largest key (and MDB_DUPSORT
	// item) of an LMDB 0.9 environment; LMDB 1.0 computes it from the page
	// size (Layout.MaxKeySize).
	MaxKeySize09 = 511
	// MaxDataSize is MAXDATASIZE: node data sizes are stored in 32 bits.
	MaxDataSize = 0xffffffff
	// MinKeys is MDB_MINKEYS, the minimum number of keys a page must hold.
	MinKeys = 2
	// DefaultMapSize is the map size of a freshly created environment.
	DefaultMapSize = 1048576
	// DefaultReaders is the default size of the reader table.
	DefaultReaders = 126
	// MaxDBsLimit is the most named databases an environment can be
	// configured for (mdb_env_set_maxdbs; rlmdb MAX_DBS_LIMIT, not in C):
	// 1,048,576. Every transaction has a slot per database, so a larger
	// count is of no practical use; C takes any count and breaks above
	// about 35 million (rlmdb KNOWN_C_BUGS.md row 54).
	MaxDBsLimit = 1 << 20
	// MaxReadersLimit is the most reader slots a new lock file can be
	// configured for (mdb_env_set_maxreaders; rlmdb MAX_READERS_LIMIT, not
	// in C): 1,048,576, a lock file of 64 MiB (a slot is 64 bytes in both
	// lock-file versions). C takes any count above zero and makes a 256 GiB
	// lock file at four billion (rlmdb KNOWN_C_BUGS.md row 60). An existing
	// lock file with a larger table is still attached to.
	MaxReadersLimit = 1 << 20
	// CursorStack is the maximum depth of a B-tree (and of a cursor's
	// page stack).
	CursorStack = 32
	// NumMetas is the number of meta pages at the start of the data file.
	NumMetas = 2
	// CoreDBs is the number of databases stored in the meta page.
	CoreDBs = 2
	// FreeDBI is the handle of the free-page database.
	FreeDBI = 0
	// MainDBI is the handle of the main (unnamed) database.
	MainDBI = 1
	// PInvalid is P_INVALID, an invalid page number; the root of an empty
	// tree.
	PInvalid uint64 = ^uint64(0)
	// IDLLogN is MDB_IDL_LOGN (midl.h).
	IDLLogN = 16
	// IDLUMSize is MDB_IDL_UM_SIZE, the capacity of a dirty list.
	IDLUMSize = 1 << (IDLLogN + 1)
	// IDLUMMax is MDB_IDL_UM_MAX, the maximum number of dirty pages in one
	// write transaction.
	IDLUMMax = IDLUMSize - 1
	// MinPageSize is the smallest page size of any format: what 1.0's
	// mdb_env_set_pagesize takes, below which a page cannot hold a header
	// and a meta. Layout.MinPageSize is the one of each format.
	MinPageSize = 256
)

// The version mdb_version reports: 0.9.90, a patch level above every real
// 0.9 release so a program can tell this implementation from C LMDB (as
// rlmdb's 0.9.85, rlmdb DUAL_FORMAT.md §4.4; 90 reads as "go").
const (
	VersionMajor = 0
	VersionMinor = 9
	VersionPatch = 90
)
