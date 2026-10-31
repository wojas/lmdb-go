package tests

import (
	"testing"

	cgolmdb "github.com/PowerDNS/lmdb-go/tests/cgolmdb"
)

// TestCgoLMDBSmoke checks that the pinned cgo reference builds and works.
func TestCgoLMDBSmoke(t *testing.T) {
	env, err := cgolmdb.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	if err := env.Open(t.TempDir(), 0, 0o644); err != nil {
		t.Fatal(err)
	}
	err = env.Update(func(txn *cgolmdb.Txn) error {
		db, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		return txn.Put(db, []byte("k"), []byte("v"), 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	maj, min, patch, _ := cgolmdb.Version()
	t.Logf("cgo reference: C LMDB %d.%d.%d", maj, min, patch)
}
