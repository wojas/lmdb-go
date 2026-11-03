package engine

// The transactions' DBI tables (dbi.go: initDBs, reach): sized by the
// handles the environment has, not by MaxDBs, grown when a transaction
// touches a later handle, and kept by a reused Txn. The handles behave as
// they did with tables of MaxDBs slots: MDB_DBS_FULL at MaxDBs, handles
// opened elsewhere usable at once, those of aborted creations closed.

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// createDBs creates n named databases "db<i>" in one write transaction,
// each with the key "k" holding its name, and returns their handles.
func createDBs(t *testing.T, txn *Txn, from, n int) []DBI {
	t.Helper()
	dbis := make([]DBI, 0, n)
	for i := from; i < from+n; i++ {
		name := []byte(fmt.Sprintf("db%d", i))
		dbi, err := txn.OpenDBI(name, format.Create)
		must(t, err)
		must(t, txn.Put(dbi, []byte("k"), name, 0))
		dbis = append(dbis, dbi)
	}
	return dbis
}

func checkDB(t *testing.T, txn *Txn, dbi DBI, i int) {
	t.Helper()
	v, err := txn.Get(dbi, []byte("k"))
	if err != nil || string(v) != fmt.Sprintf("db%d", i) {
		t.Fatalf("db%d (handle %d): %q, %v", i, dbi, v, err)
	}
}

// TestDBITableIsSizedByTheOpenHandles: with MaxDBs at 65,536 a transaction's
// table has minDBTable slots, not 65,538 of about 100 bytes (6.8 MiB), until
// the environment has more handles; a reused Txn keeps what its table grew
// to, and begins without allocating.
func TestDBITableIsSizedByTheOpenHandles(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 65536})
	r := beginR(t, env)
	if len(r.dbs) != minDBTable {
		t.Fatalf("read txn table: %d slots", len(r.dbs))
	}
	r.Abort()
	w := beginW(t, env)
	if len(w.dbs) != minDBTable {
		t.Fatalf("write txn table: %d slots", len(w.dbs))
	}
	dbis := createDBs(t, w, 0, 100)
	if len(w.dbs) < 102 || len(w.dbs) > 256 {
		t.Fatalf("write txn table after 100 handles: %d slots", len(w.dbs))
	}
	must(t, w.Commit())

	var txn Txn
	must(t, env.BeginRead(&txn))
	if len(txn.dbs) != 102 {
		t.Fatalf("read txn table with 102 handles: %d slots", len(txn.dbs))
	}
	for i, dbi := range dbis {
		checkDB(t, &txn, dbi, i)
	}
	txn.Abort()
	if raceEnabled {
		return
	}
	key := []byte("k")
	if n := testing.AllocsPerRun(100, func() {
		must(t, env.BeginRead(&txn))
		if _, err := txn.Get(dbis[99], key); err != nil {
			t.Fatal(err)
		}
		txn.Abort()
	}); n != 0 {
		t.Errorf("a reused Txn: %v allocations per begin", n)
	}
}

// TestDBITableGrowsForHandlesOpenedLater: a transaction whose table predates
// a handle uses it as before (the table grows): a handle a read transaction
// opened is published at once and works in a transaction that began earlier
// on a snapshot holding the database; one whose database is not in the
// snapshot is MDB_BAD_DBI there, one beyond MaxDBs EINVAL.
func TestDBITableGrowsForHandlesOpenedLater(t *testing.T) {
	env, _ := openTest(t, format.V09, Options{MaxDBs: 200})
	w := beginW(t, env)
	createDBs(t, w, 0, 40)
	must(t, w.Commit())
	// Close every handle: the environment's table keeps its 42 slots, all
	// but the core ones free, and the next opens reuse them.
	for i := format.CoreDBs; i < 42; i++ {
		env.CloseDBI(DBI(i))
	}
	old := beginR(t, env) // on the snapshot with the 40 databases
	defer old.Abort()
	w = beginW(t, env)
	later := createDBs(t, w, 40, 60) // slots 2 to 41, then 42 to 61
	must(t, w.Commit())

	// Handles of the 40 reopened in another read transaction work in old.
	r := beginR(t, env)
	for i := 0; i < 40; i++ {
		dbi, err := r.OpenDBI([]byte(fmt.Sprintf("db%d", i)), 0)
		must(t, err)
		if int(dbi) < 62 {
			t.Fatalf("db%d reopened in slot %d", i, dbi)
		}
		checkDB(t, old, dbi, i)
		checkDB(t, r, dbi, i)
	}
	r.Abort()
	if len(old.dbs) < 102 {
		t.Fatalf("the old table did not grow: %d slots", len(old.dbs))
	}
	// The 60 created after old began: not in its snapshot.
	for _, dbi := range later {
		if _, err := old.Get(dbi, []byte("k")); err != format.BadDBI {
			t.Fatalf("handle %d of a newer database: %v, want MDB_BAD_DBI", dbi, err)
		}
	}
	for _, dbi := range []DBI{150, 201, 202, 1 << 20} {
		want := error(syscall.EINVAL)
		if _, err := old.Get(dbi, []byte("k")); err != want {
			t.Fatalf("handle %d: %v, want EINVAL", dbi, err)
		}
	}
}

// TestDBITableOfNestedTransactions: children create databases past the
// parent's table; a commit hands their handles to the parent, an abort
// closes them, and a database a child dropped is reopened when it aborts
// (KNOWN_C_BUGS.md row 20), also in a slot the parent's table did not
// reach.
func TestDBITableOfNestedTransactions(t *testing.T) {
	env, _ := openTest(t, format.V10, Options{MaxDBs: 300})
	p := beginW(t, env)
	defer p.Abort()
	base := createDBs(t, p, 0, 5)
	c, err := env.BeginTxn(p, 0)
	must(t, err)
	committed := createDBs(t, c, 5, 50)
	must(t, c.Commit())
	for i, dbi := range append(base, committed...) {
		checkDB(t, p, dbi, i)
	}
	c, err = env.BeginTxn(p, 0)
	must(t, err)
	aborted := createDBs(t, c, 55, 100)
	for i, dbi := range aborted {
		checkDB(t, c, dbi, 55+i)
	}
	c.Abort()
	for _, dbi := range aborted {
		if _, err := p.Get(dbi, []byte("k")); err == nil {
			t.Fatalf("handle %d of an aborted creation works in the parent", dbi)
		}
	}
	// A grandchild drops a database whose slot the parent has not looked
	// at: the parent had 155 slots at most, the drop comes in a later one
	// opened in the children.
	c, err = env.BeginTxn(p, 0)
	must(t, err)
	created := createDBs(t, c, 155, 100)
	g, err := env.BeginTxn(c, 0)
	must(t, err)
	must(t, g.Drop(created[99], true))
	g.Abort()
	checkDB(t, c, created[99], 254)
	must(t, c.Commit())
	checkDB(t, p, created[99], 254)
	must(t, p.Commit())
	r := beginR(t, env)
	defer r.Abort()
	checkDB(t, r, created[99], 254)
	checkDB(t, r, committed[49], 54)
}

// TestDBsFullAtMaxDBs: MDB_DBS_FULL comes at MaxDBs named databases, as
// with tables of MaxDBs slots, in one transaction and across several, and
// for MaxDBs 0 at the first named database.
func TestDBsFullAtMaxDBs(t *testing.T) {
	for _, max := range []int{0, 1, minDBTable - format.CoreDBs, minDBTable, 40} {
		env, _ := openTest(t, format.V09, Options{MaxDBs: uint32(max)})
		w := beginW(t, env)
		createDBs(t, w, 0, max/2)
		must(t, w.Commit())
		w = beginW(t, env)
		createDBs(t, w, max/2, max-max/2)
		_, err := w.OpenDBI([]byte("one more"), format.Create)
		expectErr(t, err, format.DBsFull, fmt.Sprintf("MaxDBs %d", max))
		must(t, w.Commit())
		r := beginR(t, env)
		_, err = r.OpenDBI([]byte("one more"), 0)
		expectErr(t, err, format.DBsFull, fmt.Sprintf("MaxDBs %d, read", max))
		if max > 0 {
			dbi, err := r.OpenDBI([]byte(fmt.Sprintf("db%d", max-1)), 0)
			must(t, err)
			checkDB(t, r, dbi, max-1)
		}
		r.Abort()
		env.Close()
	}
}

// TestDBITableConcurrentOpens (run it with -race): one goroutine creates
// databases one commit at a time and another opens them in read
// transactions, publishing their handles at once, while readers that
// reuse their Txn use every handle published so far. Each reader's table
// grows as handles appear.
func TestDBITableConcurrentOpens(t *testing.T) {
	const n = 120
	env, _ := openTest(t, format.V09, Options{MaxDBs: 65536})
	var handles [n]atomic.Uint32 // handle+1 of db<i> once published
	var published atomic.Int32   // db0..db<published-1> are published
	var created atomic.Int32
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			txn, err := env.BeginTxn(nil, 0)
			if err != nil {
				errs <- err
				return
			}
			name := []byte(fmt.Sprintf("db%d", i))
			dbi, err := txn.OpenDBI(name, format.Create)
			if err == nil {
				err = txn.Put(dbi, []byte("k"), name, 0)
			}
			if err == nil {
				err = txn.Commit()
			} else {
				txn.Abort()
			}
			if err != nil {
				errs <- err
				return
			}
			created.Store(int32(i + 1))
		}
	}()
	wg.Add(1)
	go func() {
		// Opens each database in a read transaction once it exists, as
		// lmdb-go programs open handles in View (published at once).
		defer wg.Done()
		var txn Txn
		for i := 0; i < n; {
			if int(created.Load()) <= i {
				runtime.Gosched()
				continue
			}
			if err := env.BeginRead(&txn); err != nil {
				errs <- err
				return
			}
			dbi, err := txn.OpenDBI([]byte(fmt.Sprintf("db%d", i)), 0)
			txn.Abort()
			if err != nil {
				errs <- fmt.Errorf("open db%d: %w", i, err)
				return
			}
			handles[i].Store(uint32(dbi) + 1)
			i++
			published.Store(int32(i))
		}
	}()
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var txn Txn
			for int(published.Load()) < n {
				if err := env.BeginRead(&txn); err != nil {
					errs <- err
					return
				}
				for i := 0; i < int(published.Load()); i++ {
					dbi := DBI(handles[i].Load() - 1)
					v, err := txn.Get(dbi, []byte("k"))
					if err == format.BadDBI || err == format.NotFound {
						// Published after this snapshot was taken, or
						// in it but not yet committed when it began.
						continue
					}
					if err != nil || string(v) != fmt.Sprintf("db%d", i) {
						txn.Abort()
						errs <- fmt.Errorf("db%d (handle %d): %q, %v", i, dbi, v, err)
						return
					}
				}
				txn.Abort()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	r := beginR(t, env)
	defer r.Abort()
	for i := 0; i < n; i++ {
		checkDB(t, r, DBI(handles[i].Load()-1), i)
	}
	if len(r.dbs) > 4*n {
		t.Fatalf("table of %d slots for %d handles", len(r.dbs), n+2)
	}
}
