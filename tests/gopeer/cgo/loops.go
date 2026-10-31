// Long-running commands for cross-process concurrency tests (rlmdb
// interop/c/cpeer_loops.c):
//
//	hold-reader  pins a snapshot (a read transaction) for a while: MVCC test.
//	hold-env     keeps the environment open without a read transaction.
//	hold-writer  holds the writer lock: an open write transaction.
//	writer-loop  commits many small random write transactions.
//	reader-loop  repeatedly opens read transactions and checks invariants
//	             that every committed state of writer-loop satisfies.
//
// writer-loop data (README.md, "writer-loop"), in DB <db> (or main):
//
//	"w%06u" (r)  -> value "r:v:len:" + filler, total len bytes, where v is
//	                the #commits value of the writing transaction and the
//	                filler is SplitMix64 seeded with r << 32 ^ v
//	"#count"     -> decimal number of "w" keys
//	"#dups"      -> decimal number of values in the dups DB
//	"#commits"   -> decimal number of committed loop transactions
//
// and in DUPSORT DB "<db>.dups" (or "dups" for the main DB):
//
//	"d%04u" (r)  -> 12-byte values t:u64le, fmix32(lo(t) ^ hi(t) ^ r):u32le
//
// A reader checks that every value is well formed and that the counters
// match what it sees, in every snapshot.

package peer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"time"

	lmdb "github.com/PowerDNS/lmdb-go/tests/cgolmdb"
)

// waitStdin waits until deadline (in nowSeconds), or until a line or EOF
// arrives on stdin.
func waitStdin(deadline float64) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if n <= 0 || err != nil || bytes.IndexByte(buf[:n], '\n') >= 0 {
				return
			}
		}
	}()
	left := deadline - nowSeconds()
	if left <= 0 {
		return
	}
	select {
	case <-done:
	case <-time.After(time.Duration(left * 1e9)):
	}
}

// wait waits secs seconds, or with --until-stdin until a line on stdin.
func (o *opts) wait(start, secs float64) {
	if o.untilStdin {
		waitStdin(start + secs)
	} else {
		sleepSeconds(secs)
	}
}

// ---- hold-reader ----

func cmdHoldReader(o *opts) int {
	secs := parseSeconds(o.posArg(1))
	if len(o.pos) != 2 && len(o.pos) != 5 {
		die(exitUsage, "hold-reader takes <env> <seconds> [<db|-> <count> <seed>]")
	}
	if len(o.pos) == 5 {
		parseDatasetArgs(o, 2)
	}
	env := openEnv(o, lmdb.Readonly)
	txn := beginRead(env)
	start := nowSeconds()
	printf("READY txnid=%d pid=%d\n", txn.ID(), os.Getpid())
	o.wait(start, secs)
	// The snapshot must still show the data as of READY, no matter what
	// other processes committed meanwhile.
	rc := 0
	if len(o.pos) == 5 {
		rc = verifyInTxn(txn, &o.ds, false)
	}
	printf("released txnid=%d held=%.3f\n", txn.ID(), nowSeconds()-start)
	txn.Abort()
	env.Close()
	return rc
}

// ---- hold-env ----

// cmdHoldEnv keeps the environment open (read-only) without a read
// transaction. While this process holds its shared lock on the lock file,
// later openers are not exclusive and do not reset the reader table, so
// the slots of dead processes stay until mdb_reader_check() clears them.
func cmdHoldEnv(o *opts) int {
	secs := parseSeconds(o.posArg(1))
	env := openEnv(o, lmdb.Readonly)
	start := nowSeconds()
	printf("READY pid=%d\n", os.Getpid())
	o.wait(start, secs)
	printf("released held=%.3f\n", nowSeconds()-start)
	env.Close()
	return 0
}

// ---- hold-writer ----

// cmdHoldWriter begins a write transaction, puts "#held" = "<pid>" into
// the main DB (not committed), prints READY and waits; then aborts. A test
// kills this process with SIGKILL while it holds the writer lock: the next
// writer must recover the lock (robust mutexes on Linux), and "#held" must
// never be seen.
func cmdHoldWriter(o *opts) int {
	secs := parseSeconds(o.posArg(1))
	env := openEnv(o, 0)
	txn := beginWrite(env)
	dbi, err := txn.OpenRoot(0)
	check(err, "mdb_dbi_open")
	check(txn.Put(dbi, []byte("#held"), []byte(strconv.Itoa(os.Getpid())), 0), "mdb_put")
	start := nowSeconds()
	printf("READY txnid=%d pid=%d\n", txn.ID(), os.Getpid())
	o.wait(start, secs)
	printf("released txnid=%d held=%.3f\n", txn.ID(), nowSeconds()-start)
	abortWrite(txn)
	env.Close()
	return 0
}

// ---- shared writer-loop data ----

const (
	dupKeys = 64    // key space of the dups DB
	wlBig   = 20000 // occasional value forcing overflow pages
)

func dupsDBName(db string) string {
	if db == "" {
		return "dups"
	}
	return db + ".dups"
}

// wlValue writes "r:v:len:" followed by SplitMix64 filler into buf, len(buf)
// bytes in total (len >= 40).
func wlValue(r uint32, v uint64, buf []byte) {
	state := uint64(r)<<32 ^ v
	head := fmt.Appendf(buf[:0], "%d:%d:%d:", r, v, len(buf))
	off := len(head)
	for off < len(buf) {
		x := splitmix64(&state)
		for b := 0; b < 8 && off < len(buf); b, off = b+1, off+1 {
			buf[off] = byte(x >> (8 * b))
		}
	}
}

func dupCheck(t uint64, r uint32) uint32 {
	return fmix32(uint32(t) ^ uint32(t>>32) ^ r)
}

func dupValue(t uint64, r uint32, buf []byte) {
	binary.LittleEndian.PutUint64(buf, t)
	binary.LittleEndian.PutUint32(buf[8:], dupCheck(t, r))
}

// getCounter reads a decimal counter; 0 if absent. strtoull semantics:
// the leading digits.
func getCounter(txn *lmdb.Txn, dbi lmdb.DBI, name string) uint64 {
	data, err := txn.Get(dbi, []byte(name))
	if lmdb.IsNotFound(err) {
		return 0
	}
	check(err, "mdb_get(counter)")
	if len(data) == 0 || len(data) >= 32 {
		die(exitMismatch, "counter %s has size %d", name, len(data))
	}
	var v uint64
	for _, c := range data {
		if c < '0' || c > '9' {
			break
		}
		v = v*10 + uint64(c-'0')
	}
	return v
}

func putCounter(txn *lmdb.Txn, dbi lmdb.DBI, name string, v uint64) {
	check(txn.Put(dbi, []byte(name), strconv.AppendUint(nil, v, 10), 0), "mdb_put")
}

// ---- writer-loop ----

type wloop struct {
	main, dups lmdb.DBI
	rng        uint64
	keys       uint32
	vbuf       []byte
}

func (w *wloop) rnd(n uint32) uint32 {
	return uint32(splitmix64(&w.rng) % uint64(n))
}

// ops does a random batch of puts and deletes in txn (top-level or
// nested); the counters are updated in the same transaction, so they stay
// consistent with the data whether the transaction commits or aborts.
func (w *wloop) ops(txn *lmdb.Txn, version uint64) {
	count := getCounter(txn, w.main, "#count")
	ndups := getCounter(txn, w.main, "#dups")
	nops := 1 + w.rnd(8)
	cur, err := txn.OpenCursor(w.dups)
	check(err, "mdb_cursor_open")
	kbuf := make([]byte, 0, 16)
	for op := uint32(0); op < nops; op++ {
		r, sel := w.rnd(w.keys), w.rnd(10)
		switch {
		case sel < 5:
			// insert or overwrite w<r>
			n := wlBig
			if w.rnd(40) != 0 {
				n = 40 + int(w.rnd(100))
			}
			key := fmt.Appendf(kbuf[:0], "w%06d", r)
			data := w.vbuf[:n]
			wlValue(r, version, data)
			err := txn.Put(w.main, key, data, lmdb.NoOverwrite)
			if isErr(err, lmdb.KeyExist) {
				err = txn.Put(w.main, key, data, 0)
			} else if err == nil {
				count++
			}
			check(err, "mdb_put(w)")
		case sel < 7:
			// delete w<r> if present
			key := fmt.Appendf(kbuf[:0], "w%06d", r)
			err := txn.Del(w.main, key, nil)
			if err == nil {
				count--
			} else if !lmdb.IsNotFound(err) {
				check(err, "mdb_del(w)")
			}
		case sel < 9:
			// add a value to d<r>
			r %= dupKeys
			key := fmt.Appendf(kbuf[:0], "d%04d", r)
			var vb [12]byte
			dupValue(splitmix64(&w.rng), r, vb[:])
			err := txn.Put(w.dups, key, vb[:], lmdb.NoDupData)
			if err == nil {
				ndups++
			} else if !isErr(err, lmdb.KeyExist) {
				check(err, "mdb_put(d)")
			}
		default:
			// delete one value of d<r>, or all of them
			r %= dupKeys
			key := fmt.Appendf(kbuf[:0], "d%04d", r)
			_, _, err := cur.Get(key, nil, lmdb.Set)
			if lmdb.IsNotFound(err) {
				continue
			}
			check(err, "mdb_cursor_get(d)")
			c, err := cur.Count()
			check(err, "mdb_cursor_count")
			if w.rnd(4) == 0 {
				check(cur.Del(lmdb.NoDupData), "mdb_cursor_del(MDB_NODUPDATA)")
				ndups -= c
			} else {
				for skip := w.rnd(uint32(c)); skip > 0; skip-- {
					_, _, err := cur.Get(nil, nil, lmdb.NextDup)
					check(err, "mdb_cursor_get(MDB_NEXT_DUP)")
				}
				check(cur.Del(0), "mdb_cursor_del")
				ndups--
			}
		}
	}
	cur.Close()
	putCounter(txn, w.main, "#count", count)
	putCounter(txn, w.main, "#dups", ndups)
}

func cmdWriterLoop(o *opts) int {
	db := o.dbArg(1)
	deadline := nowSeconds() + parseSeconds(o.posArg(2))
	var commits, aborts, ncommits, naborts, last uint64
	w := &wloop{rng: parseU64(o.posArg(3), "seed"), keys: max(o.keys, 1), vbuf: make([]byte, wlBig)}
	env := openEnv(o, 0)

	txn := beginWrite(env)
	var err error
	w.main, err = openDB(txn, db, lmdb.Create)
	check(err, "mdb_dbi_open")
	w.dups, err = txn.OpenDBI(dupsDBName(db), lmdb.Create|lmdb.DupSort)
	check(err, "mdb_dbi_open(dups)")
	commitWrite(txn)

	// Always commit at least once, so "0 seconds" initialises the DBs.
	for commits == 0 || (nowSeconds() < deadline && (o.maxCommits == 0 || commits < o.maxCommits)) {
		txn := beginWrite(env)
		version := getCounter(txn, w.main, "#commits") + 1
		w.ops(txn, version)
		if w.rnd(6) == 0 {
			// A nested transaction on top, committed or aborted.
			child := beginChild(env, txn)
			w.ops(child, version)
			if w.rnd(2) != 0 {
				check(child.Commit(), "mdb_txn_commit(child)")
				ncommits++
			} else {
				child.Abort()
				naborts++
			}
		}
		if commits > 0 && w.rnd(10) == 0 {
			abortWrite(txn)
			aborts++
			continue
		}
		putCounter(txn, w.main, "#commits", version)
		commitWrite(txn)
		last = version
		if commits == 0 {
			printf("READY\n")
		}
		commits++
	}
	printf("commits=%d aborts=%d nested_commits=%d nested_aborts=%d last_commit=%d\n",
		commits, aborts, ncommits, naborts, last)
	env.Close()
	return 0
}

// ---- reader-loop ----

func rlFail(format string, a ...any) int {
	stdout.Flush()
	fmt.Fprintf(os.Stderr, "gopeer: reader-loop: %s\n", fmt.Sprintf(format, a...))
	return exitMismatch
}

// keyNumber parses the number in a key "<prefix><digits>" of exactly n
// bytes (strtoul: all of it must be digits).
func keyNumber(key []byte, prefix byte, n int) (uint32, bool) {
	if len(key) != n || key[0] != prefix {
		return 0, false
	}
	v, err := strconv.ParseUint(string(key[1:]), 10, 32)
	return uint32(v), err == nil
}

// parseHead parses the "r:v:len:" head of a "w" value (C: sscanf of
// "%u:%llu:%zu:" on its first 63 bytes).
func parseHead(data []byte) (r uint32, v uint64, n int, ok bool) {
	head := data[:min(len(data), 63)]
	var nums [3]uint64
	for k := range nums {
		digits := 0
		for len(head) > 0 && head[0] >= '0' && head[0] <= '9' {
			nums[k] = nums[k]*10 + uint64(head[0]-'0')
			head = head[1:]
			digits++
		}
		if digits == 0 || len(head) == 0 || head[0] != ':' {
			return 0, 0, 0, false
		}
		head = head[1:]
	}
	if nums[0] > 0xffffffff {
		return 0, 0, 0, false
	}
	return uint32(nums[0]), nums[1], int(nums[2]), true
}

// checkWValue checks one "w" record.
func checkWValue(key, data, buf []byte) int {
	r, v, n, ok := parseHead(data)
	if !ok {
		return rlFail("malformed value for key %s", key)
	}
	if kr, ok := keyNumber(key, 'w', 7); !ok || kr != r {
		return rlFail("value of key %s names r=%d", key, r)
	}
	if n != len(data) || n > wlBig {
		return rlFail("value of key %s has size %d, header says %d", key, len(data), n)
	}
	wlValue(r, v, buf[:n])
	if !bytes.Equal(buf[:n], data) {
		return rlFail("value of key %s (v=%d) is corrupt", key, v)
	}
	return 0
}

// checkSnapshot checks all invariants in one snapshot, walking the main DB
// backwards if reverse; returns its #commits.
func checkSnapshot(txn *lmdb.Txn, mdbi, ddbi lmdb.DBI, reverse bool, buf []byte) (uint64, int) {
	commits := getCounter(txn, mdbi, "#commits")
	count := getCounter(txn, mdbi, "#count")
	ndups := getCounter(txn, mdbi, "#dups")
	var seen, dseen uint64

	first, step := uint(lmdb.First), uint(lmdb.Next)
	if reverse {
		first, step = lmdb.Last, lmdb.Prev
	}
	cur, err := txn.OpenCursor(mdbi)
	check(err, "mdb_cursor_open")
	key, data, err := cur.Get(nil, nil, first)
	for ; err == nil; key, data, err = cur.Get(nil, nil, step) {
		if len(key) != 7 || key[0] != 'w' {
			continue // counters, named-DB records
		}
		if rc := checkWValue(key, data, buf); rc != 0 {
			cur.Close()
			return 0, rc
		}
		seen++
	}
	if !lmdb.IsNotFound(err) {
		check(err, "mdb_cursor_get(main)")
	}
	cur.Close()
	if seen != count {
		return 0, rlFail("snapshot #commits=%d: %d w-keys, #count says %d", commits, seen, count)
	}

	// Every value of every dups key, key by key: NEXT_DUP within a key,
	// NEXT_NODUP to the next key.
	cur, err = txn.OpenCursor(ddbi)
	check(err, "mdb_cursor_open")
	defer cur.Close()
	key, data, err = cur.Get(nil, nil, lmdb.First)
	for ; err == nil; key, data, err = cur.Get(nil, nil, lmdb.NextNoDup) {
		r, ok := keyNumber(key, 'd', 5)
		if !ok {
			return 0, rlFail("bad dups key %s", key)
		}
		c, err := cur.Count()
		check(err, "mdb_cursor_count")
		k := uint64(0)
		for {
			if len(data) != 12 {
				return 0, rlFail("dup value of size %d under d%04d", len(data), r)
			}
			t := binary.LittleEndian.Uint64(data)
			if binary.LittleEndian.Uint32(data[8:]) != dupCheck(t, r) {
				return 0, rlFail("corrupt dup value under d%04d", r)
			}
			k++
			if _, data, err = cur.Get(nil, nil, lmdb.NextDup); err != nil {
				break
			}
		}
		if !lmdb.IsNotFound(err) {
			check(err, "mdb_cursor_get(MDB_NEXT_DUP)")
		}
		if k != c {
			return 0, rlFail("d%04d: mdb_cursor_count %d, walked %d", r, c, k)
		}
		dseen += k
	}
	if !lmdb.IsNotFound(err) {
		check(err, "mdb_cursor_get(dups)")
	}
	st, err := txn.Stat(ddbi)
	check(err, "mdb_stat")
	if dseen != ndups || st.Entries != ndups {
		return 0, rlFail("snapshot #commits=%d: %d dup values, mdb_stat %d, #dups says %d",
			commits, dseen, st.Entries, ndups)
	}
	return commits, 0
}

// rlMetaRace: a snapshot (txnid t, in *txn) showed fewer #commits (low)
// than maxCommits, seen at txnid n. Tolerate this only as C LMDB's reader meta
// race (rlmdb KNOWN_C_BUGS.md #31), with cpeer's rules: mdb_txn_renew0
// pins n in the reader slot, then copies the roots from meta page n & 1
// without checking that it still holds n. A reader delayed there over two
// commits copies the roots of a commit m = n + 2 (or n + 4, ...) whose
// writer may not have published m yet; the next snapshot is then m - 1,
// older than the data just seen. So t must be an odd number of commits
// after n, and the first fresh snapshot after t (m, once published) must
// be back at maxCommits. Only on lmdb-go v1, whose C library has the race
// (readerMetaRace); the port, like rlmdb, re-checks the meta page
// and fails on any step back. On success *txn is the snapshot that is
// back; its #commits is returned.
func rlMetaRace(env *lmdb.Env, txn **lmdb.Txn, mdbi, ddbi lmdb.DBI, n, maxCommits, low uint64,
	buf []byte) (uint64, int) {
	t := uint64((*txn).ID())
	if !readerMetaRace {
		return 0, rlFail("snapshot went back in time: #commits %d (txnid %d) after %d (txnid %d)",
			low, t, maxCommits, n)
	}
	id, commits, tries := t, low, 0
	// The first fresh snapshot at once, then after 1 ms, 2 ms, ... (0.5 s
	// in all) for the writer of m to finish its meta sync and publish m.
	for t > n && (t-n)%2 == 1 && id == t && tries < 10 {
		if tries > 0 {
			sleepSeconds(0.001 * float64(int(1)<<(tries-1)))
		}
		(*txn).Abort()
		*txn = beginRead(env)
		var rc int
		if commits, rc = checkSnapshot(*txn, mdbi, ddbi, false, buf); rc != 0 {
			return 0, rc
		}
		tries++
		id = uint64((*txn).ID())
	}
	if id > t && commits >= maxCommits {
		stdout.Flush()
		fmt.Fprintf(os.Stderr, "gopeer: reader-loop: meta race: #commits %d (txnid %d) after %d "+
			"(txnid %d), back to %d (txnid %d)\n", low, t, maxCommits, n, commits, id)
		return commits, 0
	}
	return 0, rlFail("snapshot went back in time: #commits %d (txnid %d) after %d "+
		"(txnid %d); %d fresh snapshots, the last #commits %d (txnid %d)",
		low, t, maxCommits, n, tries, commits, id)
}

func cmdReaderLoop(o *opts) int {
	db := o.dbArg(1)
	deadline := nowSeconds() + parseSeconds(o.posArg(2))
	var reads, races, first, last, lastID uint64
	buf := make([]byte, wlBig)
	env := openEnv(o, lmdb.Readonly)

	// Wait for writer-loop to create its DBs; handles opened in a
	// read-only transaction stay valid after it commits.
	var mdbi, ddbi lmdb.DBI
	for {
		txn := beginRead(env)
		var err error
		mdbi, err = openDB(txn, db, 0)
		if err == nil {
			ddbi, err = txn.OpenDBI(dupsDBName(db), 0)
		}
		if err == nil {
			check(txn.Commit(), "mdb_txn_commit")
			break
		}
		txn.Abort()
		if !lmdb.IsNotFound(err) {
			check(err, "mdb_dbi_open")
		}
		if nowSeconds() > deadline {
			die(exitMismatch, "reader-loop: writer-loop DBs never appeared")
		}
		sleepSeconds(0.01)
	}

	rc := 0
	txn := beginRead(env)
	for {
		if reads > 0 {
			// Alternate between reset/renew and abort/begin.
			if reads%2 == 1 {
				txn.Reset()
				check(txn.Renew(), "mdb_txn_renew")
			} else {
				txn.Abort()
				txn = beginRead(env)
			}
		}
		var commits uint64
		if commits, rc = checkSnapshot(txn, mdbi, ddbi, reads%3 == 2, buf); rc != 0 {
			break
		}
		// #commits never goes down, except in C's meta race (#31).
		if commits < last {
			if commits, rc = rlMetaRace(env, &txn, mdbi, ddbi, lastID, last, commits, buf); rc != 0 {
				break
			}
			races++
		}
		if reads == 0 {
			first = commits
			printf("READY\n")
		}
		reads++
		last = commits
		lastID = uint64(txn.ID())
		if nowSeconds() >= deadline {
			break
		}
	}
	txn.Abort()
	if rc == 0 {
		printf("reads=%d first_commit=%d last_commit=%d meta_races=%d\n", reads, first, last, races)
	}
	env.Close()
	return rc
}
