package lmdb

import (
	"os"
	"testing"
)

// testFlags are the flags the tests open environments with: flags, plus
// NoLock where this build has no shared locking (Linux with CGO_ENABLED=0,
// where only NoLock environments open), or when LMDB_TEST_NOLOCK=1 asks for
// that in a build with shared locking. Not in lmdb-go v1's tests (see
// DEVIATIONS.md).
func testFlags(flags uint) uint {
	if !SharedLockingAvailable() || os.Getenv("LMDB_TEST_NOLOCK") == "1" {
		return flags | NoLock
	}
	return flags
}

// requireLockFile skips a test that needs the environment's lock file
// (reader table, cross-process locking) when testFlags adds NoLock.
func requireLockFile(t testing.TB) {
	t.Helper()
	if testFlags(0)&NoLock != 0 {
		t.Skip("needs a lock file: a cgo build without LMDB_TEST_NOLOCK=1")
	}
}
