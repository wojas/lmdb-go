// LMDB's return codes (rlmdb error.rs, MdbError and strerror).
//
// C LMDB functions return 0, one of LMDB's own negative codes (Errno
// here), or an errno value (syscall.Errno here). The two kinds stay
// distinguishable and convert losslessly back to a C int, because the
// public API must return exactly what C LMDB returns. Error texts are
// byte-for-byte mdb_strerror's.

package format

import (
	"errors"
	"syscall"
)

// Errno is one of LMDB's own return codes (MDB_KEYEXIST ... MDB_BAD_DBI,
// and LMDB 1.0's MDB_PROBLEM ... MDB_ADDR_BUSY).
type Errno int32

// The LMDB return codes, lmdb.h "errors".
const (
	KeyExist        Errno = -30799 // MDB_KEYEXIST
	NotFound        Errno = -30798 // MDB_NOTFOUND
	PageNotFound    Errno = -30797 // MDB_PAGE_NOTFOUND
	Corrupted       Errno = -30796 // MDB_CORRUPTED
	Panic           Errno = -30795 // MDB_PANIC
	VersionMismatch Errno = -30794 // MDB_VERSION_MISMATCH
	Invalid         Errno = -30793 // MDB_INVALID
	MapFull         Errno = -30792 // MDB_MAP_FULL
	DBsFull         Errno = -30791 // MDB_DBS_FULL
	ReadersFull     Errno = -30790 // MDB_READERS_FULL
	TLSFull         Errno = -30789 // MDB_TLS_FULL
	TxnFull         Errno = -30788 // MDB_TXN_FULL
	CursorFull      Errno = -30787 // MDB_CURSOR_FULL
	PageFull        Errno = -30786 // MDB_PAGE_FULL
	MapResized      Errno = -30785 // MDB_MAP_RESIZED
	Incompatible    Errno = -30784 // MDB_INCOMPATIBLE
	BadRSlot        Errno = -30783 // MDB_BAD_RSLOT
	BadTxn          Errno = -30782 // MDB_BAD_TXN
	BadValSize      Errno = -30781 // MDB_BAD_VALSIZE
	BadDBI          Errno = -30780 // MDB_BAD_DBI
	// LastErrCode09 is MDB_LAST_ERRCODE of LMDB 0.9.
	LastErrCode09 = BadDBI

	// LMDB 1.0's additions, with their upstream names and values.
	Problem       Errno = -30779 // MDB_PROBLEM
	BadChecksum   Errno = -30778 // MDB_BAD_CHECKSUM
	CryptoFail    Errno = -30777 // MDB_CRYPTO_FAIL
	EnvEncryption Errno = -30776 // MDB_ENV_ENCRYPTION
	TxnPending    Errno = -30775 // MDB_TXN_PENDING
	CantRollback  Errno = -30774 // MDB_CANT_ROLLBACK
	DBIsBusy      Errno = -30773 // MDB_DBIS_BUSY
	ShortWrite    Errno = -30772 // MDB_SHORT_WRITE
	EnvBusy       Errno = -30771 // MDB_ENV_BUSY
	IsReadonly    Errno = -30770 // MDB_IS_READONLY
	AddrBusy      Errno = -30769 // MDB_ADDR_BUSY
	// LastErrCode10 is MDB_LAST_ERRCODE of LMDB 1.0.
	LastErrCode10 = AddrBusy
)

var errTexts = [...]string{
	"MDB_KEYEXIST: Key/data pair already exists",
	"MDB_NOTFOUND: No matching key/data pair found",
	"MDB_PAGE_NOTFOUND: Requested page not found",
	"MDB_CORRUPTED: Located page was wrong type",
	"MDB_PANIC: Update of meta page failed or environment had fatal error",
	"MDB_VERSION_MISMATCH: Database environment version mismatch",
	"MDB_INVALID: File is not an LMDB file",
	"MDB_MAP_FULL: Environment mapsize limit reached",
	"MDB_DBS_FULL: Environment maxdbs limit reached",
	"MDB_READERS_FULL: Environment maxreaders limit reached",
	"MDB_TLS_FULL: Thread-local storage keys full - too many environments open",
	"MDB_TXN_FULL: Transaction has too many dirty pages - transaction too big",
	"MDB_CURSOR_FULL: Internal error - cursor stack limit reached",
	"MDB_PAGE_FULL: Internal error - page has no more space",
	"MDB_MAP_RESIZED: Database contents grew beyond environment mapsize",
	"MDB_INCOMPATIBLE: Operation and DB incompatible, or DB flags changed",
	"MDB_BAD_RSLOT: Invalid reuse of reader locktable slot",
	"MDB_BAD_TXN: Transaction must abort, has a child, or is invalid",
	"MDB_BAD_VALSIZE: Unsupported size of key/DB name/data, or wrong DUPFIXED size",
	"MDB_BAD_DBI: The specified DBI handle was closed/changed unexpectedly",
	"MDB_PROBLEM: Unexpected problem - txn should abort",
	"MDB_BAD_CHECKSUM: Page checksum mismatch",
	"MDB_CRYPTO_FAIL: Page encryption or decryption failed",
	"MDB_ENV_ENCRYPTION: Environment encryption mismatch",
	"MDB_TXN_PENDING: Transaction already prepared, must abort or commit",
	"MDB_CANT_ROLLBACK: Environment can't rollback last transaction",
	"MDB_DBIS_BUSY: Can't drop main DBI while other DBIs are open",
	"MDB_SHORT_WRITE: Fewer bytes were written than requested",
	"MDB_ENV_BUSY: Environment is busy, can't use previous snapshot",
	"MDB_IS_READONLY: Can't write in readonly txn or environment",
	"MDB_ADDR_BUSY: Requested map address is unavailable",
}

// Error returns mdb_strerror's text for the code.
func (e Errno) Error() string { return Strerror(int(e)) }

// Strerror is mdb_strerror: LMDB's text for its own codes (both
// releases'), "Successful return: 0" for 0, the C library's text for
// errno values.
func Strerror(code int) string {
	switch {
	case code == 0:
		return "Successful return: 0"
	case code >= int(KeyExist) && code <= int(LastErrCode10):
		return errTexts[code-int(KeyExist)]
	case code < 0:
		return "Invalid error code"
	}
	return syscall.Errno(code).Error()
}

// Code returns the C int for err: an Errno's value, a syscall.Errno's
// number (also wrapped), and EIO for anything else.
func Code(err error) int {
	if err == nil {
		return 0
	}
	var e Errno
	if errors.As(err, &e) {
		return int(e)
	}
	var se syscall.Errno
	if errors.As(err, &se) {
		return int(se)
	}
	return int(syscall.EIO)
}

// FromCode is the inverse of Code: an Errno for LMDB's codes, a
// syscall.Errno otherwise, nil for 0.
func FromCode(code int) error {
	switch {
	case code == 0:
		return nil
	case code < 0:
		return Errno(code)
	}
	return syscall.Errno(code)
}
