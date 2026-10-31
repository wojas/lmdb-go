// What connects lmdb-go's API (env.go, txn.go, cursor.go) to the engine
// (internal/engine, the Go port of rlmdb's core): opening, the settings
// made before Open, error conversion, and the reuse of read transactions
// and cursors.  In lmdb-go v1 these were calls into the C library (mdb.c)
// and lmdbgo.c.

package lmdb

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/internal/sys"
)

var errClosed = errors.New("environment is already closed")

// open is mdb_env_open (Env.Open), with closeLock held.
func (env *Env) open(path string, flags uint, mode os.FileMode) error {
	if env.closed {
		return errClosed
	}
	if env.opened || uint(uint32(flags)) != flags {
		// mdb_env_open on an open environment, or flags C cannot hold.
		return &OpError{Op: "mdb_env_open", Errno: syscall.EINVAL}
	}
	if flags&NoLock == 0 && !SharedLockingAvailable() {
		return ErrNoSharedLocking
	}
	sniffed, err := SniffFormat(path, flags)
	switch {
	case errors.Is(err, ErrFormatUnsupported):
		return err
	case IsErrno(err, VersionMismatch):
		// mdb_env_read_header's verdict (the engine, as rlmdb, would say
		// MDB_INVALID).
		return &OpError{Op: "mdb_env_open", Errno: VersionMismatch}
	case err == nil && sniffed.valid() && env.reqFormat.valid() && sniffed != env.reqFormat:
		return ErrFormatConflict
	}
	// Other sniffing errors (not an LMDB file, I/O errors) are left for the
	// engine to report as mdb_env_open does.

	opts := env.opts
	opts.Flags |= uint32(flags)
	opts.Format = format.Format(env.reqFormat)
	opts.Mode = mode
	opts.OpenLock = openLock
	e, err := engine.Open(path, opts)
	if err != nil {
		return operr("mdb_env_open", err)
	}
	env.eng.Store(e)
	env.opened = true
	return nil
}

// engine returns the open environment, or the error for an Env that is not
// open.  The caller holds closeLock.
func (env *Env) engine() (*engine.Env, error) {
	if e := env.eng.Load(); e != nil {
		return e, nil
	}
	if env.closed {
		return nil, errClosed
	}
	return nil, errNotOpen
}

// readerList is mdb_reader_list, with closeLock held: fn gets the lines one
// at a time, as C's message function does, formatted as C formats them.
func (env *Env) readerList(fn func(string) error) error {
	const header = "    pid     thread     txnid\n"
	e := env.eng.Load()
	switch {
	case e == nil && env.closed:
		return errClosed
	case e == nil && !env.created:
		// A zero Env: mdb_reader_list(NULL, ...) fails.
		return &OpError{Op: "mdb_reader_list", Errno: syscall.EINVAL}
	case e == nil || !e.HasLockFile():
		// Not open yet, or NoLock: there is no lock file.
		return fn("(no reader locks)\n")
	}
	readers := e.ReaderList()
	if len(readers) == 0 {
		return fn("(no active readers)\n")
	}
	if err := fn(header); err != nil {
		return err
	}
	for _, r := range readers {
		line := format.FormatReaderList([]format.ReaderInfo{r})
		if err := fn(line[len(header):]); err != nil {
			return err
		}
	}
	return nil
}

// copyFD is mdb_env_copyfd2 (op names the C function).
func (env *Env) copyFD(op string, fd uintptr, flags uint) error {
	e, err := env.copySource()
	if err != nil {
		return err
	}
	return operr(op, e.CopyFD(fd, uint32(flags)))
}

// copy is mdb_env_copy2 (op names the C function).
func (env *Env) copy(op, path string, flags uint) error {
	e, err := env.copySource()
	if err != nil {
		return err
	}
	return operr(op, e.Copy(path, uint32(flags)))
}

// copySource is the open environment for a copy.  The copy runs without
// closeLock: a plain copy waits for the writer lock, and a writer's commit
// must not wait for a Close queued behind the copy.  The engine keeps what
// the copy uses until its read transaction ends; after a Close the copy
// fails (EINVAL).
func (env *Env) copySource() (*engine.Env, error) {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	return env.engine()
}

// setFlags is mdb_env_set_flags: on an open environment, or remembered
// for Open.
func (env *Env) setFlags(flags uint, on bool) error {
	env.closeLock.Lock()
	defer env.closeLock.Unlock()
	if e := env.eng.Load(); e != nil {
		return operr("mdb_env_set_flags", e.SetFlags(uint32(flags), on))
	}
	if env.closed {
		return errClosed
	}
	// Before Open, as in C: only the changeable flags (CHANGEABLE).
	if flags&^uint(format.Changeable) != 0 {
		return &OpError{Op: "mdb_env_set_flags", Errno: syscall.EINVAL}
	}
	if on {
		env.opts.Flags |= uint32(flags)
	} else {
		env.opts.Flags &^= uint32(flags)
	}
	return nil
}

// maxKeySize is mdb_env_get_maxkeysize as rlmdb's C API has it: the open
// environment's limit; before Open 511, the LMDB 0.9 limit, unless LMDB 1.0
// was requested, then the 1.0 limit of the page size a new environment
// would get (C 1.0 returns 0 before the open).
func (env *Env) maxKeySize() int {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	if e := env.eng.Load(); e != nil {
		return e.MaxKeySize()
	}
	if env.reqFormat == V10 {
		psize := env.opts.PageSize
		if psize == 0 {
			psize = min(sys.OSPageSize(), format.LayoutV10.MaxPageSize)
		}
		return format.LayoutV10.MaxKeySize(psize)
	}
	return format.MaxKeySize09
}

// readTxn returns the state of an ended read transaction to begin another
// one in (engine.Env.BeginRead reuses its memory), or a new one.
func (env *Env) readTxn() *engine.Txn {
	if t, _ := env.txns.Get().(*engine.Txn); t != nil {
		return t
	}
	return new(engine.Txn)
}

// readCursor returns the state of a closed read cursor to open another
// one in (engine.Txn.OpenCursorInto), or a new one.
func (env *Env) readCursor() *engine.Cursor {
	if c, _ := env.cursors.Get().(*engine.Cursor); c != nil {
		return c
	}
	return new(engine.Cursor)
}

// ended reports that txn's transaction is over: it was committed or
// aborted, or ended with its parent (endChildren).  The tok check stands
// for those ends: should a handle miss one, its _txn may serve another
// transaction by now, which it must leave alone.
func (txn *Txn) ended() bool {
	t := txn._txn
	return t == nil || t.Owner() != txn.tok
}

// endChildren, at the end of txn, ends the handles of its active nested
// transaction and of theirs: the engine ended those with txn, as C's
// mdb_txn_end does, and reuses their state for later children.  They fail
// from now on, and their finalizers have nothing left to do.
func (txn *Txn) endChildren() {
	c := txn.child
	txn.child = nil
	for c != nil && c._txn != nil {
		c._txn = nil
		runtime.SetFinalizer(c, nil)
		next := c.child
		c.child = nil
		c = next
	}
}

// ready checks that c can run the operation op (a C function name for
// errors): it is open and its transaction has not ended.  If the
// transaction was reset and renewed since c was positioned, c starts over
// unpositioned in the new snapshot (rlmdb's C API; C would keep pointers
// into pages of the old snapshot).
func (c *Cursor) ready(op string) error {
	if c._c == nil || c.txn.ended() {
		return errInvalid(op)
	}
	if c.gen != c.txn.gen {
		if err := c.txn._txn.OpenCursorInto(c._c, c._c.DBI()); err != nil {
			return operr(op, err)
		}
		c.gen = c.txn.gen
	}
	return nil
}

// multipleOfOne completes MDB_GET_MULTIPLE: on a key with a single value
// there is no page of duplicates, and C leaves data alone (holding the
// value its caller has just read).  The page is that one value.
func (c *Cursor) multipleOfOne(val []byte) ([]byte, error) {
	if val != nil || c._c.HasDups() {
		return val, nil
	}
	_, val, err := c._c.Get(eb[:0], eb[:0], format.GetCurrent)
	return val, err
}

// operr wraps an error of the engine for the C function op, as operrno
// wraps C's return codes.  The engine returns format.Errno values for
// LMDB's codes and syscall.Errno values (possibly wrapped, e.g. in an
// *os.PathError) for system errors, exactly where C returns them.
func operr(op string, err error) error {
	switch e := err.(type) {
	case nil:
		return nil
	case format.Errno:
		return &OpError{Op: op, Errno: Errno(e)}
	case syscall.Errno:
		return &OpError{Op: op, Errno: e}
	}
	if errors.Is(err, errNoSharedLocks) {
		return ErrNoSharedLocking
	}
	return operrno(op, format.Code(err))
}

// The errors that end most scans and lookups, allocated once.  OpError
// values are never modified by this package.
var (
	errGetNotFound       = &OpError{Op: "mdb_get", Errno: NotFound}
	errCursorGetNotFound = &OpError{Op: "mdb_cursor_get", Errno: NotFound}
)

// getErr is operr for mdb_get.
func getErr(err error) error {
	if err == error(format.NotFound) {
		return errGetNotFound
	}
	return operr("mdb_get", err)
}

// cursorGetErr is operr for mdb_cursor_get.
func cursorGetErr(err error) error {
	if err == error(format.NotFound) {
		return errCursorGetNotFound
	}
	return operr("mdb_cursor_get", err)
}

// errInvalid is EINVAL from op: what rlmdb's C API returns for handles that
// are no longer valid (C dereferences them).
func errInvalid(op string) error {
	return &OpError{Op: op, Errno: syscall.EINVAL}
}

// cString is name as C sees a C string made from it: up to the first NUL
// byte, and never NULL.
func cString(name string) []byte {
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return eb[:0]
	}
	return []byte(name)
}

// copyPair copies a key and a value into one allocation (lmdb-go v1 made
// two): the key comes first, capped so that appending to it cannot
// overwrite the value.  Neither result is nil, as with C.GoBytes.
func copyPair(k, v []byte) ([]byte, []byte) {
	b := make([]byte, len(k)+len(v))
	n := copy(b, k)
	copy(b[n:], v)
	return b[:n:n], b[n:]
}
