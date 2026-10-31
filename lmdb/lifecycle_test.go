package lmdb

// Environment lifetime and the locks around write transactions:
// concurrent writers, copies and Close (review findings, round 2). Not in
// lmdb-go v1's tests.

import (
	"fmt"
	"sync"
	"testing"
)

// lockModes are the environment flags to test both locking modes with:
// NoLock, and the lock file where this build has one.
func lockModes() []uint {
	if testFlags(0)&NoLock != 0 {
		return []uint{NoLock}
	}
	return []uint{NoLock, 0}
}

// openFormat opens a new environment of format f with flags (plus NoSync)
// in a temporary directory, closed at the end of the test.
func openFormat(t *testing.T, f Format, flags uint) (*Env, string) {
	t.Helper()
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.SetMapSize(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := env.SetFormat(f); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	if err := env.Open(path, flags|NoSync, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Close() })
	return env, path
}

// Concurrent Updates: every writer reuses the environment's write
// transaction once the last one released the writer locks (review finding,
// round 2, P1-1: the ending writer released its map after the unlock, from
// the next writer's transaction; -race reported it, and writers panicked
// on a nil map). The engine's TestWriterEndLeavesNextWriterAlone is the
// deterministic version; run this one with -race.
func TestConcurrentUpdates(t *testing.T) {
	iters := 2000
	if testing.Short() || raceEnabled {
		iters = 300
	}
	for _, f := range []Format{V09, V10} {
		for _, flags := range lockModes() {
			t.Run(fmt.Sprintf("%v/flags=%#x", f, flags), func(t *testing.T) {
				env, _ := openFormat(t, f, flags)
				var wg sync.WaitGroup
				errs := make(chan error, 8)
				for g := 0; g < 8; g++ {
					wg.Add(1)
					go func(g int) {
						defer wg.Done()
						key := []byte(fmt.Sprint(g))
						for i := 0; i < iters; i++ {
							err := env.Update(func(txn *Txn) error {
								return txn.Put(1, key, []byte(fmt.Sprint(i)), 0)
							})
							if err != nil {
								errs <- fmt.Errorf("writer %d, update %d: %v", g, i, err)
								return
							}
						}
					}(g)
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					t.Fatal(err)
				}
				err := env.View(func(txn *Txn) error {
					for g := 0; g < 8; g++ {
						v, err := txn.Get(1, []byte(fmt.Sprint(g)))
						if err != nil {
							return err
						}
						if string(v) != fmt.Sprint(iters-1) {
							return fmt.Errorf("writer %d: %q", g, v)
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
