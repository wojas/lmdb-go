package lmdb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestErrno_Error(t *testing.T) {
	operr := &OpError{"testop", fmt.Errorf("testmsg")}
	msg := operr.Error()
	if msg != "testop: testmsg" {
		t.Errorf("message: %q", msg)
	}
}

func BenchmarkErrno_Error(b *testing.B) {
	for i := 0; i < b.N; i++ {
		for _, errno := range []error{
			syscall.EINVAL,
			NotFound,
			MapResized,
			MapFull,
		} {
			operr := &OpError{"mdb_testop", errno}
			msg := operr.Error()
			if msg == "" {
				b.Fatal("empty message")
			}
		}

	}
}
func TestErrno(t *testing.T) {
	zeroerr := operrno("testop", 0)
	if zeroerr != nil {
		t.Errorf("errno(0) != nil: %#v", zeroerr)
	}
	syserr := _operrno("testop", int(syscall.EINVAL))
	if syserr.(*OpError).Errno != syscall.EINVAL { // fails if error is Errno(syscall.EINVAL)
		t.Errorf("errno(syscall.EINVAL) != syscall.EINVAL: %#v", syserr)
	}
	mdberr := _operrno("testop", int(KeyExist))
	if mdberr.(*OpError).Errno != KeyExist {
		t.Errorf("errno(ErrKeyExist) != ErrKeyExist: %#v", syserr)
	}
}

func TestIsErrno(t *testing.T) {
	err := NotFound
	if !IsErrno(err, err) {
		t.Errorf("expected match: %v", err)
	}

	operr := &OpError{
		Op:    "testop",
		Errno: err,
	}
	if !IsErrno(operr, err) {
		t.Errorf("expected match: %v", operr)
	}
}

func TestOpErrorUnwrap(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	err := env.View(func(txn *Txn) error {
		dbi, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		_, err = txn.Get(dbi, []byte("missing"))
		return err
	})
	var operr *OpError
	if !errors.As(err, &operr) {
		t.Fatalf("not an OpError: %#v", err)
	}
	if !errors.Is(err, NotFound) {
		t.Errorf("errors.Is(%v, NotFound) is false", err)
	}
	if errors.Is(err, MapFull) {
		t.Errorf("errors.Is(%v, MapFull) is true", err)
	}
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) is false", err)
	}
	var errno Errno
	if !errors.As(err, &errno) || errno != NotFound {
		t.Errorf("errors.As: %v", errno)
	}
	if wrapped := fmt.Errorf("wrapped: %w", err); !errors.Is(wrapped, NotFound) {
		t.Errorf("errors.Is(%v, NotFound) is false", wrapped)
	}

	// A system errno, through the os package's own errors.
	env2, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer env2.Close()
	err = env2.Open(filepath.Join(t.TempDir(), "missing"), testFlags(0), 0o644)
	if !errors.As(err, &operr) {
		t.Fatalf("not an OpError: %#v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(%v, os.ErrNotExist) is false", err)
	}
}
