/*
Package lmdb provides the API of lmdb-go, the Go bindings to the lmdb C API, on
an implementation of LMDB in Go.  The API is
fairly low level and is designed to provide a minimal interface that prevents
misuse to a reasonable extent.  When in doubt refer to the C documentation as a
reference.

	http://www.lmdb.tech/doc/
	http://www.lmdb.tech/doc/starting.html
	http://www.lmdb.tech/doc/modules.html

# Implementation

The storage engine is a port to Go of rlmdb, the Rust rewrite of LMDB, which
follows LMDB (Lightning Memory-Mapped Database, by Howard Chu, Symas
Corporation) function by function.  Data files and lock files are binary
compatible with C LMDB: C, Rust and Go processes can use one environment at
the same time.  The API, its types, constants, errors and documentation are
those of lmdb-go v1, so that programs switch implementations without code
changes; the API for the LMDB 1.0 data format is that of lmdb-go v2.
DEVIATIONS.md lists where behaviour differs from lmdb-go v1 on C LMDB.

Everything but the two locks that LMDB shares between processes (the writer
lock and the reader-table lock: pthread mutexes on Linux, POSIX semaphores on
macOS) is Go.  The locks are libc objects, taken in package internal/lockfile:
on Linux through cgo; on macOS through cgo in a cgo build (the default) and
through libSystem directly in a build without cgo.  A Linux program built
without cgo (CGO_ENABLED=0) can only open environments with the NoLock flag;
Env.Open without it returns ErrNoSharedLocking.  SharedLockingAvailable tells
which kind of build this is.

# LMDB versions

Both on-disk formats of LMDB are read and written: that of the 0.9.x series and
that of the 1.0.x series, which are mutually incompatible.  The format of an
Env is decided at Open: an existing database is always opened in its on-disk
format (see SniffFormat), while a new database uses the format requested with
Env.SetFormat or the process-wide default (V09 unless changed with
SetDefaultFormat or the LMDBGO_DEFAULT_FORMAT environment variable).
Databases of both formats can be open simultaneously in one process.  lmdb-go
never converts a database between formats; migrating means dumping with an
environment of one format and loading into another.

# Environment

An LMDB environment holds named databases (key-value stores).  An environment
is represented as one file on the filesystem (though often a corresponding lock
file exists).

LMDB recommends setting an environment's size as large as possible at the time
of creation.  On filesystems that support sparse files this should not
adversely affect disk usage.  Resizing an environment is possible but must be
handled with care when concurrent access is involved.

Note that the package lmdb forces all Env objects to be opened with the NoTLS
(MDB_NOTLS) flag.  Without this flag LMDB would not be practically usable in Go
(in the author's opinion).  However, even for environments opened with this
flag there are caveats regarding how transactions are used (see Caveats below).

# Databases

A database in an LMDB environment is an ordered key-value store that holds
arbitrary binary data.  Typically the keys are unique but duplicate keys may be
allowed (DupSort), in which case the values for each duplicate key are ordered.

A single LMDB environment can have multiple named databases.  But there is also
a 'root' (unnamed) database that can be used to store data.  Use caution
storing data in the root database when named databases are in use.  The root
database serves as an index for named databases.

A database is referenced by an opaque handle known as its DBI which must be
opened inside a transaction with the OpenDBI or OpenRoot methods.  DBIs may be
closed but it is not required.  Typically, applications acquire handles for all
their databases immediately after opening an environment and retain them for
the lifetime of the process.

# Transactions

View (readonly) transactions in LMDB operate on a snapshot of the database at
the time the transaction began.  The number of simultaneously active view
transactions is bounded and configured when the environment is initialized.

Update (read-write) transactions are serialized in LMDB.  Attempts to create
update transactions block until a lock may be obtained.  Update transactions
can create subtransactions which may be rolled back independently from their
parent.

The lmdb package supplies managed and unmanaged transactions. Managed
transactions do not require explicit calling of Abort/Commit and are provided
through the Env methods Update, View, and RunTxn.  The BeginTxn method on Env
creates an unmanaged transaction but its use is not advised in most
applications.

To provide ACID guarantees, a readonly transaction must acquire a "lock" in the
LMDB environment to ensure that data it reads is consistent over the course of
the transaction's lifetime, and that updates happening concurrently will not be
seen.  If a reader does not release its lock then stale data, which has been
overwritten by later transactions, cannot be reclaimed by LMDB -- resulting in
a rapid increase in file size.

Long-running read transactions may cause increase an applications storage
requirements, depending on the application write workload.  But, typically the
complete failure of an application to terminate a read transactions will result
in continual increase file size to the point where the storage volume becomes
full or a quota has been reached.

There are steps an application may take to greatly reduce the possibility of
unterminated read transactions.  The first safety measure is to avoid the use
of Env.BeginTxn, which creates unmanaged transactions, and always use Env.View
or Env.Update to create managed transactions that are (mostly) guaranteed to
terminate.  If Env.BeginTxn must be used try to defer a call to the Txn's Abort
method (this is useful even for update transactions).

	txn, err := env.BeginTxn(nil, 0)
	if err != nil {
		// ...
	}
	defer txn.Abort() // Safe even if txn.Commit() is called later.

Because application crashes and signals from the operation system may cause
unexpected termination of a readonly transaction before Txn.Abort may be called
it is also important that applications clear any readers held for dead OS
processes when they start.

	numStale, err := env.ReaderCheck()
	if err != nil {
		// ...
	}
	if numStale > 0 {
		log.Printf("Released locks for %d dead readers", numStale)
	}

If an application gets accessed by multiple programs concurrently it is also a
good idea to periodically call Env.ReaderCheck during application execution.
However, note that Env.ReaderCheck cannot find readers opened by the
application itself which have since leaked.  Because of this, the lmdb package
uses a finalizer to abort unreachable Txn objects.  But of course, applications
must still be careful not to leak unterminated Txn objects in a way such that
they fail get garbage collected.

# Caveats

Write transactions (those created without the Readonly flag) must be created in
a goroutine that has been locked to its thread by calling the function
runtime.LockOSThread.  Futhermore, all methods on such transactions must be
called from the goroutine which created them.  This is a fundamental limitation
of LMDB even when using the NoTLS flag (which the package always uses): the
writer lock shared with other processes belongs to the OS thread that took it
(on Linux a robust pthread mutex).  The
Env.Update method assists the programmer by calling runtime.LockOSThread
automatically but it cannot sufficiently abstract write transactions to make
them completely safe in Go.

A goroutine must never create a write transaction if the application programmer
cannot determine whether the goroutine is locked to an OS thread.  This is a
consequence of goroutine restrictions on write transactions and limitations in
the runtime's thread locking implementation.  In such situations updates
desired by the goroutine in question must be proxied by a goroutine with a
known state (i.e.  "locked" or "unlocked").  See the included examples for more
details about dealing with such situations.
*/
package lmdb

import "github.com/PowerDNS/lmdb-go/internal/format"

// Version return the major, minor, and patch version numbers of the LMDB
// implementation and a string representation of the version: 0, 9, 85
// (see versionString).
//
// See mdb_version.
func Version() (major, minor, patch int, s string) {
	return format.VersionMajor, format.VersionMinor, format.VersionPatch, versionString
}

// VersionString returns a string representation of the LMDB implementation's version.
//
// See mdb_version.
func VersionString() string {
	return versionString
}

// versionString is what mdb_version returns.  The numbers are 0.9.85: the 0.9
// series, whose API and default data format this is, with a patch level
// above every C release so that a program can tell this implementation from C
// LMDB, as rlmdb's C library reports them ("LMDB 0.9.85: (rlmdb <version>)",
// rlmdb DUAL_FORMAT.md).  Env.EngineVersion says which C release an open
// environment's data format matches.
const versionString = "LMDB 0.9.85: (lmdb-go, Go port of rlmdb)"
