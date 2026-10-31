package lmdb

// Environment lifetime and the locks around write transactions:
// concurrent writers, copies and Close (review findings, round 2). Not in
// lmdb-go v1's tests.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
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

// A copy waits for the writer locks, which a writer holds; Close comes;
// then the writer commits. None of them may wait for another forever
// (review finding, round 2, P1-2: the copy held the Env's close lock
// while it waited for the writer, Close queued for it exclusively, and the
// writer's commit needed it shared behind Close). Copies and transaction
// ends do not take the close lock any more; the writer's commit after
// Close aborts with EINVAL, the copy fails with EINVAL.
func TestCopyCloseWithWriter(t *testing.T) {
	for _, f := range []Format{V09, V10} {
		for _, flags := range lockModes() {
			for _, fd := range []bool{false, true} {
				t.Run(fmt.Sprintf("%v/flags=%#x/fd=%v", f, flags, fd), func(t *testing.T) {
					testCopyCloseWithWriter(t, f, flags, fd)
				})
			}
		}
	}
}

func testCopyCloseWithWriter(t *testing.T, f Format, flags uint, fd bool) {
	env, _ := openFormat(t, f, flags)
	started, finish := make(chan struct{}), make(chan struct{})
	wrote := make(chan error, 1)
	go func() {
		wrote <- env.Update(func(txn *Txn) error {
			close(started)
			<-finish
			return txn.Put(1, []byte("k"), []byte("v"), 0)
		})
	}()
	<-started
	copied := make(chan error, 1)
	target := t.TempDir()
	if fd {
		out, err := os.Create(filepath.Join(target, "data.mdb"))
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		go func() { copied <- env.CopyFD(out.Fd()) }()
	} else {
		go func() { copied <- env.Copy(target) }()
	}
	// The copy waits for the writer lock.
	waitGoroutine(t, "engine.(*Env).copyPlain", "sync.Mutex.Lock")
	closed := make(chan error, 1)
	go func() { closed <- env.Close() }()
	// Close is done, or waits for the copy.
	for deadline := time.Now().Add(10 * time.Second); len(closed) == 0; time.Sleep(time.Millisecond) {
		if goroutineWaits("lmdb.(*Env).close", "sync.RWMutex.Lock") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Close neither returns nor waits:\n%s", allStacks())
		}
	}
	close(finish)
	timeout := time.After(10 * time.Second)
	for _, step := range []struct {
		name string
		ch   chan error
		want error
	}{{"writer", wrote, syscall.EINVAL}, {"Close", closed, nil}, {"copy", copied, syscall.EINVAL}} {
		select {
		case err := <-step.ch:
			if step.want == nil && err != nil || step.want != nil && !IsErrnoSys(err, step.want.(syscall.Errno)) {
				t.Errorf("%s: %v, want %v", step.name, err, step.want)
			}
		case <-timeout:
			t.Fatalf("%s hangs:\n%s", step.name, allStacks())
		}
	}
}
