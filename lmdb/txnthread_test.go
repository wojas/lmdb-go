package lmdb

// Tests of ErrTxnWrongThread: a write transaction committed on another OS
// thread than the one that began it (DEVIATIONS.md "Write transactions and
// threads").

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The error wraps ErrTxnWrongThread and the mutex's errno, which
// IsErrnoSys sees as it sees an OpError's.
func TestErrTxnWrongThread_wraps(t *testing.T) {
	var err error = &txnThreadError{errno: errNotOwner}
	if !errors.Is(err, ErrTxnWrongThread) || !errors.Is(err, errNotOwner) {
		t.Errorf("errors.Is: %v, %v", errors.Is(err, ErrTxnWrongThread), errors.Is(err, errNotOwner))
	}
	var se syscall.Errno
	if !errors.As(err, &se) || se != errNotOwner {
		t.Errorf("errors.As syscall.Errno: %v", se)
	}
	if !IsErrnoSys(err, errNotOwner) || IsErrnoSys(err, syscall.EINVAL) {
		t.Error("IsErrnoSys does not see the errno")
	}
	want := ErrTxnWrongThread.Error() + ": " + errNotOwner.Error()
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err, want)
	}
}

// On the systems whose writer lock belongs to a thread (Linux and FreeBSD
// robust mutexes, Windows mutexes), Commit on another thread commits and
// returns the error; the mutex is recovered once the beginning thread
// ends.
func TestTxn_Commit_wrongThread(t *testing.T) {
	requireLockFile(t)
	switch runtime.GOOS {
	case "linux", "freebsd", "windows":
	default:
		t.Skip("the writer lock is a semaphore, which belongs to no thread")
	}
	if os.Getenv("LMDB_TEST_NO_ROBUST_MUTEX") != "" {
		t.Skip("no robust mutexes here (LMDB_TEST_NO_ROBUST_MUTEX: QEMU user emulation)")
	}
	env := setup(t)
	defer clean(env, t)
	db, err := openRoot(env, 0)
	if err != nil {
		t.Fatal(err)
	}

	// The goroutine that begins the transaction exits locked to its
	// thread, which ends the thread and so releases the mutex as a dead
	// owner's.
	began := make(chan *Txn)
	release := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		runtime.LockOSThread()
		txn, err := env.BeginTxn(nil, 0)
		if err != nil {
			t.Error(err)
			close(began)
			return
		}
		if err := txn.Put(db, []byte("k"), []byte("v"), 0); err != nil {
			t.Error(err)
		}
		began <- txn
		<-release
	}()
	txn := <-began
	if txn == nil {
		t.FailNow()
	}
	errc := make(chan error)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		errc <- txn.Commit()
	}()
	err = <-errc
	close(release)
	<-ended

	if !errors.Is(err, ErrTxnWrongThread) {
		t.Fatalf("Commit on another thread: %v, want ErrTxnWrongThread", err)
	}
	if !errors.Is(err, errNotOwner) || !IsErrnoSys(err, errNotOwner) {
		t.Errorf("Commit on another thread: %v does not wrap %v", err, errNotOwner)
	}
	if !strings.Contains(err.Error(), "another OS thread") {
		t.Errorf("message does not name the cause: %q", err)
	}
	// Committed; the next writer recovers the mutex of the ended thread.
	go func() {
		errc <- env.Update(func(txn *Txn) error {
			v, err := txn.Get(db, []byte("k"))
			if err != nil {
				return err
			}
			if string(v) != "v" {
				t.Errorf("committed value %q", v)
			}
			return txn.Put(db, []byte("k2"), []byte("v2"), 0)
		})
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Update after the wrong-thread commit: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the writer mutex of the ended thread was not recovered")
	}
}
