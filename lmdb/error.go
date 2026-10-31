package lmdb

import (
	"os"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// OpError is an error returned by the LMDB API.  Not all errors returned by
// lmdb-go have type OpError but typically they do.  The Errno field will
// either have type Errno or syscall.Errno.
type OpError struct {
	Op    string
	Errno error
}

// Error implements the error interface.
func (err *OpError) Error() string {
	return err.Op + ": " + err.Errno.Error()
}

// The most common error codes do not need to be handled explicity.  Errors can
// be checked through helper functions IsNotFound, IsMapFull, etc, Otherwise
// they should be checked using the IsErrno function instead of direct
// comparison because they will typically be wrapped with an OpError.
const (
	// Error codes defined by LMDB.  See the list of LMDB return codes for more
	// information about each
	//
	//		http://symas.com/mdb/doc/group__errors.html

	KeyExist        Errno = Errno(format.KeyExist)
	NotFound        Errno = Errno(format.NotFound)
	PageNotFound    Errno = Errno(format.PageNotFound)
	Corrupted       Errno = Errno(format.Corrupted)
	Panic           Errno = Errno(format.Panic)
	VersionMismatch Errno = Errno(format.VersionMismatch)
	Invalid         Errno = Errno(format.Invalid)
	MapFull         Errno = Errno(format.MapFull)
	DBsFull         Errno = Errno(format.DBsFull)
	ReadersFull     Errno = Errno(format.ReadersFull)
	TLSFull         Errno = Errno(format.TLSFull)
	TxnFull         Errno = Errno(format.TxnFull)
	CursorFull      Errno = Errno(format.CursorFull)
	PageFull        Errno = Errno(format.PageFull)
	MapResized      Errno = Errno(format.MapResized)
	Incompatible    Errno = Errno(format.Incompatible)
	BadRSlot        Errno = Errno(format.BadRSlot)
	BadTxn          Errno = Errno(format.BadTxn)
	BadValSize      Errno = Errno(format.BadValSize)
	BadDBI          Errno = Errno(format.BadDBI)

	// Error codes introduced by LMDB 1.0 (as in lmdb-go v2).  They are
	// always defined; this implementation returns EnvEncryption and
	// BadChecksum for LMDB 1.0 environments that use encryption or page
	// checksums (not implemented), DBIsBusy from Txn.Drop of the root
	// database of a 1.0 environment, and none of the others.
	Problem       Errno = Errno(format.Problem)
	BadChecksum   Errno = Errno(format.BadChecksum)
	CryptoFail    Errno = Errno(format.CryptoFail)
	EnvEncryption Errno = Errno(format.EnvEncryption)
	TxnPending    Errno = Errno(format.TxnPending)
	CantRollback  Errno = Errno(format.CantRollback)
	DBIsBusy      Errno = Errno(format.DBIsBusy)
	ShortWrite    Errno = Errno(format.ShortWrite)
	EnvBusy       Errno = Errno(format.EnvBusy)
	IsReadonly    Errno = Errno(format.IsReadonly)
	AddrBusy      Errno = Errno(format.AddrBusy)
)

// Errno is an error type that represents the (unique) errno values defined by
// LMDB.  Other errno values (such as EINVAL) are represented with type
// syscall.Errno.  On Windows, LMDB return codes are translated into portable
// syscall.Errno constants (e.g. syscall.EINVAL, syscall.EACCES, etc.).
//
// Most often helper functions such as IsNotFound may be used instead of
// dealing with Errno values directly.
//
//	lmdb.IsNotFound(err)
//	lmdb.IsErrno(err, lmdb.TxnFull)
//	lmdb.IsErrnoSys(err, syscall.EINVAL)
//	lmdb.IsErrnoFn(err, os.IsPermission)
type Errno int32

// minimum and maximum values produced for the Errno type. syscall.Errnos of
// other values may still be produced.
const minErrno, maxErrno int32 = int32(KeyExist), int32(AddrBusy)

func (e Errno) Error() string {
	return format.Strerror(int(e))
}

// _operrno is for use by tests that can't import C
func _operrno(op string, ret int) error {
	return operrno(op, ret)
}

// IsNotFound returns true if the key requested in Txn.Get or Cursor.Get does
// not exist or if the Cursor reached the end of the database without locating
// a value (EOF).
func IsNotFound(err error) bool {
	return IsErrno(err, NotFound)
}

// IsNotExist returns true the path passed to the Env.Open method does not
// exist.
func IsNotExist(err error) bool {
	return IsErrnoFn(err, os.IsNotExist)
}

// IsMapFull returns true if the environment map size has been reached.
func IsMapFull(err error) bool {
	return IsErrno(err, MapFull)
}

// IsMapResized returns true if the environment has grown too large for the
// current map after being resized by another process.
func IsMapResized(err error) bool {
	return IsErrno(err, MapResized)
}

// IsErrno returns true if err's errno is the given errno.
func IsErrno(err error, errno Errno) bool {
	return IsErrnoFn(err, func(err error) bool { return err == errno })
}

// IsErrnoSys returns true if err's errno is the given errno.
func IsErrnoSys(err error, errno syscall.Errno) bool {
	return IsErrnoFn(err, func(err error) bool { return err == errno })
}

// IsErrnoFn calls fn on the error underlying err and returns the result.  If
// err is an *OpError then err.Errno is passed to fn.  Otherwise err is passed
// directly to fn.
func IsErrnoFn(err error, fn func(error) bool) bool {
	if err == nil {
		return false
	}
	if err, ok := err.(*OpError); ok {
		return fn(err.Errno)
	}
	return fn(err)
}
