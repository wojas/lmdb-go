package engine

// The writer locks and what happens around them: the end of a write
// transaction, the next writer's reuse of the environment's write Txn.

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// nextWriter is a write transaction that begins (began), waits for
// proceed, then puts c=3 and commits; a panic is returned as an error.
func nextWriter(env *Env, began, proceed chan struct{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	w, err := env.BeginTxn(nil, 0)
	if err != nil {
		return err
	}
	close(began)
	<-proceed
	if err := w.Put(MainDBI, []byte("c"), []byte("3"), 0); err != nil {
		w.Abort()
		return err
	}
	return w.Commit()
}

// The next write transaction begins as soon as the last one released the
// writer locks, in the same Txn (the environment reuses it, as C reuses
// me_txn0): the ending one must not touch it any more (review finding,
// round 2, P1-1: it released its map after unlocking, which dropped the
// next writer's map reference and left it without a map, so that it
// panicked, and leaked its own reference, so that the environment was
// never torn down). Deterministic: the next writer begins inside the
// first one's end, after the unlock.
func TestWriterEndLeavesNextWriterAlone(t *testing.T) {
	for _, f := range []format.Format{format.V09, format.V10} {
		for _, flags := range []uint32{0, format.NoLock} {
			t.Run(fmt.Sprintf("%v/flags=%#x", f, flags), func(t *testing.T) {
				env, dir := openTest(t, f, Options{Flags: flags})
				putKV(t, env, "a", "1")
				began, proceed := make(chan struct{}), make(chan struct{})
				done := make(chan error, 1)
				var once sync.Once
				testHookWriterUnlocked = func(e *Env) {
					if e != env {
						return
					}
					once.Do(func() {
						go func() { done <- nextWriter(env, began, proceed) }()
						<-began
					})
				}
				defer func() { testHookWriterUnlocked = nil }()
				putKV(t, env, "b", "2")
				close(proceed)
				if err := <-done; err != nil {
					t.Fatalf("next writer: %v", err)
				}
				r := beginR(t, env)
				for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}, {"c", "3"}} {
					if v, err := r.Get(MainDBI, []byte(kv[0])); err != nil || string(v) != kv[1] {
						t.Fatalf("Get %s: %q, %v", kv[0], v, err)
					}
				}
				r.Abort()
				mustAudit(t, env)
				// Every map reference is gone with the transactions: Close
				// tears the environment down, and the path opens again.
				env.Close()
				env2, err := Open(dir, Options{Flags: flags | format.NoSync, OpenLock: OpenMemLock})
				if err != nil {
					t.Fatalf("reopen: %v", err)
				}
				env2.Close()
			})
		}
	}
}
