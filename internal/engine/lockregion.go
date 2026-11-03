// The engine's view of the lock file (rlmdb core/reader.rs, LockRegion).
// internal/lockfile implements it; the engine never touches the lock file
// otherwise, so the two can be developed and tested apart.

package engine

import (
	"os"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// LockRegion is the open lock file of an environment: the reader table,
// the writer and reader mutexes shared with C, and this process's record
// locks. Each method keeps the semantics of the rlmdb method of the same
// name.
type LockRegion interface {
	// IsExclusive reports whether this process holds the exclusive lock on
	// the lock file (it initialised the table).
	IsExclusive() bool
	// Downgrade publishes txnid as mti_txnid and downgrades the exclusive
	// lock to a shared one (mdb_env_share_locks).
	Downgrade(txnid uint64) error
	// MaxReaders is the number of slots in the table (me_maxreaders).
	MaxReaders() uint32
	// LockWriter blocks until this process owns the writer mutex. It pins
	// the calling goroutine to its OS thread (the Linux mutex is owned by
	// a thread) until UnlockWriter, which the same goroutine must call.
	// newestTxnid repairs the state after a dead owner (mdb_mutex_failed):
	// it returns the txnid of the newest meta page.
	LockWriter(newestTxnid func() (uint64, error)) error
	// UnlockWriter unlocks the writer mutex and unpins the goroutine. It
	// fails (Linux: EPERM) when called on another thread than LockWriter;
	// the mutex then stays held by that thread.
	UnlockWriter() error
	// TxnID is mti_txnid, the last committed transaction.
	TxnID() uint64
	// SetTxnID sets mti_txnid (after a commit).
	SetTxnID(txnid uint64)
	// NumReaders is mti_numreaders as stored (mdb_env_info's
	// me_numreaders).
	NumReaders() uint32
	// AcquireSlot claims a free reader slot (under the reader mutex) and
	// returns its index; the slot's txnid is (txnid_t)-1.
	AcquireSlot() (int, error)
	// SlotIsIdleAndOurs reports whether slot i is still this process's
	// and idle (mdb_txn_renew0's MDB_BAD_RSLOT check).
	SlotIsIdleAndOurs(i int) bool
	// PublishCurrentTxnID stores mti_txnid in slot i, re-reading until both
	// agree (ITS#7970), and returns it.
	PublishCurrentTxnID(i int) uint64
	// SlotTxnID is the txnid in slot i.
	SlotTxnID(i int) uint64
	// SetSlotTxnID sets the txnid of slot i.
	SetSlotTxnID(i int, txnid uint64)
	// ResetSlot marks slot i owned but not in a transaction
	// (mdb_txn_reset).
	ResetSlot(i int)
	// ReleaseSlot gives slot i back (mdb_txn_end with MDB_END_SLOT).
	ReleaseSlot(i int)
	// FindOldest is mdb_find_oldest: the oldest snapshot any reader uses,
	// or txnid-1 if none is older.
	FindOldest(txnid uint64) uint64
	// ReaderList is the occupied slots (mdb_reader_list).
	ReaderList() []format.ReaderInfo
	// ReaderCheck clears slots of dead processes and returns how many
	// (mdb_reader_check).
	ReaderCheck() (int, error)
	// Close clears this process's slots, closes the shared mutexes, unmaps
	// and closes the lock file (the lock-file part of mdb_env_close0).
	Close() error
}

// OpenLockFunc opens the lock file at path for an environment of data
// format f, as lockfile.Open does (mdb_env_setup_locks): it returns nil,
// nil for a read-only environment on a read-only file system, which then
// runs without a lock file. A real lock file is claimed in claim once it is
// open, before it is locked (lockfile.OpenClaimed).
type OpenLockFunc func(path string, mode os.FileMode, maxReaders uint32, readOnly bool, f format.Format, claim LockClaim) (LockRegion, error)
