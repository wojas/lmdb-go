package lockfile

import (
	"os"
	"testing"
)

// skipUnderQEMU skips a test of robust mutexes where
// LMDB_TEST_NO_ROBUST_MUTEX says there are none: QEMU user emulation has
// no robust futexes (in C too), so a dead owner is never reported and the
// next locker waits forever (scripts/linux-test.sh sets it for the
// platforms QEMU runs; rlmdb's INTEROP_NO_ROBUST_MUTEX).
func skipUnderQEMU(t *testing.T) {
	t.Helper()
	if os.Getenv("LMDB_TEST_NO_ROBUST_MUTEX") != "" {
		t.Skip("no robust mutexes here (LMDB_TEST_NO_ROBUST_MUTEX: QEMU user emulation)")
	}
}
