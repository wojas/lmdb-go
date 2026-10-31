package lmdb

// Linux: robust writer mutexes whose owner dies (EOWNERDEAD).

import (
	"fmt"
	"syscall"
	"testing"
	"time"
)

// A writer of this process waits for the writer mutex, which another
// process holds; the environment is closed; the holder dies. The waiting
// writer must repair the mutex (mdb_mutex_failed) before it fails for the
// closed environment, so that the other processes can still write
// (review finding, round 2, P1-3: the repair failed with EINVAL after
// Close and left the mutex ENOTRECOVERABLE for every process). Three
// processes: this one, a helper that keeps the environment open and writes
// at the end, and one that holds the writer mutex and is killed. The
// engine's TestOwnerDeathRepairAfterClose is the deterministic version.
func TestCloseDuringOwnerDeathRecovery(t *testing.T) {
	requireLockFile(t)
	for _, f := range []Format{V09, V10} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			env, path := openFormat(t, f, 0)
			holder := startHelper(t, "hold-env", path)
			owner := startHelper(t, "hold-writer", path)
			result := make(chan error, 1)
			go func() { result <- env.Update(func(*Txn) error { return nil }) }()
			// The writer has pinned its map and waits in the libc lock call.
			waitGoroutine(t, "lockfile.(*Region).LockWriter", "")
			if err := env.Close(); err != nil {
				t.Fatal(err)
			}
			owner.kill()
			select {
			case err := <-result:
				if !IsErrnoSys(err, syscall.EINVAL) {
					t.Fatalf("the closed environment's writer: %v, want EINVAL", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the closed environment's writer hangs")
			}
			holder.stdin.Write([]byte("\n"))
			if line := holder.line(t, 10*time.Second); line != "RESULT <nil>" {
				t.Fatalf("the other process's write after the repair: %s", line)
			}
		})
	}
}
