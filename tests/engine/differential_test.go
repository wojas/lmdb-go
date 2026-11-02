// The lock-step write-differential test (port of rlmdb's
// tests/common/differential.rs and write_differential*.rs): the same
// script of puts, deletes, cursor moves, nested transactions and drops on
// several cursors of one write transaction runs in C (rlmdb's
// tests/c/writeops.c, built against C 0.9.36 as writeops and C 1.0 as
// writeops10) and in Go, each in a fresh environment. After every step the
// position of every cursor must agree; the committed environments must
// have the same contents (mdb_dump), pass C's page audit (cpeer
// check-freelist), and hold the same pages (pagecmp, page by page).

package engine

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	eng "github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
	"github.com/PowerDNS/lmdb-go/tests/internal/coracle"
)

// oracle is the C side of a round.
type oracle struct {
	writeops string // writeops or writeops10
	dump     string // mdb_dump or mdb_dump10
	peer     string // cpeer or cpeer10
	f        format.Format
	pageSize int // 0: the default
	// dirtyLimit lowers the Go side's dirty-page limit so its transactions
	// spill where C's do not (0: C's limit). Spilling must not change what
	// is written.
	dirtyLimit int
	// dataFile, if set, is the data file both sides start from instead of
	// a new environment (a 0.9 file with a page size C 0.9 cannot create
	// but opens); both open it as it is, without a page size request
	// (rlmdb's Oracle::data_file).
	dataFile []byte
}

// oracle09 is C 0.9.36. It skips t where C's pages are not this process's
// (requireSamePageSize): 0.9 takes the OS page size.
func oracle09(t testing.TB) *oracle {
	o := &oracle{writeops: tool(t, "writeops"), dump: "mdb_dump", peer: "cpeer", f: format.V09}
	requireSamePageSize(t)
	return o
}

func oracle10(t testing.TB, ps int) *oracle {
	return &oracle{writeops: tool(t, "writeops10"), dump: "mdb_dump10", peer: "cpeer10", f: format.V10, pageSize: ps}
}

// oracle10NDebug is oracle10 with writeops10_ndebug, writeops against a
// C 1.0 library built with -DNDEBUG (as release builds are), for scripts
// on which the upstream build trips an assertion although C's arithmetic
// is right (rlmdb KNOWN_C_BUGS.md #37).
func oracle10NDebug(t testing.TB, ps int) *oracle {
	o := oracle10(t, ps)
	o.writeops = tool(t, "writeops10_ndebug")
	return o
}

func hexs(b []byte) string { return fmt.Sprintf("%x", b) }

func unhex(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := range b {
		v, _ := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		b[i] = byte(v)
	}
	return b
}

// rng is the xorshift generator of the Rust harness. With src set (the
// fuzz target) it hands out src two bytes at a time first.
type rng struct {
	s   uint64
	src []byte
}

func (r *rng) next() uint64 {
	if len(r.src) >= 2 {
		x := uint64(r.src[0]) | uint64(r.src[1])<<8
		r.src = r.src[2:]
		return x
	}
	x := r.s
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.s = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) below(n int) int { return int(r.next() % uint64(n)) }

// profile is what a script draws from.
type profile struct {
	dbflags      uint32
	keys         int
	dupfixedSize int
	big          bool
	longKeys     bool
	nested       bool
	drops        bool
	// appenddupNodemax: no MDB_APPENDDUP put whose 1.0 leaf node exceeds
	// it (KNOWN_C_BUGS.md #32, 1.0 small pages only).
	appenddupNodemax int
}

func genKey(r *rng, p *profile) []byte {
	if p.dbflags&format.IntegerKey != 0 {
		return binary.NativeEndian.AppendUint64(nil, uint64(r.below(p.keys))*7)
	}
	n := r.below(p.keys)
	k := []byte(fmt.Sprintf("key%04d", n))
	if p.longKeys {
		k = append(k, strings.Repeat("y", 200+(n*37)%280)...)
	} else if n%7 == 0 {
		k = append(k, strings.Repeat("x", 50+n%150)...)
	}
	return k
}

func genData(r *rng, p *profile) []byte {
	if p.dupfixedSize > 0 {
		v := uint64(r.below(400))
		d := binary.BigEndian.AppendUint64(nil, v)
		for len(d) < p.dupfixedSize {
			d = append(d, 'f')
		}
		if p.dupfixedSize < 8 {
			// The low bytes, which differ.
			return d[8-p.dupfixedSize : 8]
		}
		return d[:p.dupfixedSize]
	}
	if p.dbflags&format.DupSort != 0 {
		n := r.below(300)
		l := 4 + (n*13)%120
		d := []byte(fmt.Sprintf("%05d", n))
		for len(d) < l {
			d = append(d, 'd')
		}
		return d[:l]
	}
	var l int
	if p.big && r.below(12) == 0 {
		l = 1000 + r.below(40000)
	} else {
		mx := 40
		if r.below(4) == 0 {
			mx = 900
		}
		l = 1 + r.below(mx)
	}
	d := make([]byte, l)
	for i := range d {
		d[i] = 'v'
	}
	d[0] = byte(r.below(256))
	return d
}

func appenddupOK(p *profile, f uint32, k, d []byte) uint32 {
	even := func(n int) int { return (n + 1) &^ 1 }
	if p.appenddupNodemax > 0 && f == format.AppendDup && 8+even(len(k))+even(len(d)) > p.appenddupNodemax {
		return 0
	}
	return f
}

func genOp(r *rng, p *profile, ncur int) string {
	getOps := []format.CursorOp{format.First, format.Last, format.Next, format.Prev, format.Next, format.Prev,
		format.Set, format.SetKey, format.SetRange, format.NextNoDup, format.PrevNoDup, format.NextDup,
		format.PrevDup, format.FirstDup, format.LastDup, format.GetBoth, format.GetBothRange}
	putFlags := []uint32{0, 0, 0, format.NoOverwrite, format.NoDupData, format.Current, format.Append, format.AppendDup}
	c := r.below(ncur)
	switch x := r.below(20); {
	case x <= 4:
		f := putFlags[r.below(len(putFlags))]
		if f == format.Current {
			f = 0
		}
		k, d := genKey(r, p), genData(r, p)
		return fmt.Sprintf("p:%s:%s:%d", hexs(k), hexs(d), appenddupOK(p, f, k, d))
	case x <= 8:
		f := putFlags[r.below(len(putFlags))]
		if f == format.Current && p.dbflags&format.DupSort != 0 {
			f = 0
		}
		k, d := genKey(r, p), genData(r, p)
		return fmt.Sprintf("cp:%d:%s:%s:%d", c, hexs(k), hexs(d), appenddupOK(p, f, k, d))
	case x <= 10:
		if p.dbflags&format.DupSort != 0 && r.below(2) == 0 {
			k := genKey(r, p)
			return fmt.Sprintf("d:%s:%s", hexs(k), hexs(genData(r, p)))
		}
		return fmt.Sprintf("d:%s", hexs(genKey(r, p)))
	case x <= 12:
		f := uint32(0)
		if p.dbflags&format.DupSort != 0 && r.below(3) == 0 {
			f = format.NoDupData
		}
		return fmt.Sprintf("cd:%d:%d", c, f)
	default:
		op := getOps[r.below(len(getOps))]
		dupOp := op == format.NextDup || op == format.PrevDup || op == format.FirstDup ||
			op == format.LastDup || op == format.GetBoth || op == format.GetBothRange
		switch {
		case p.dbflags&format.DupSort == 0 && dupOp:
			return fmt.Sprintf("g:%d:%d", c, format.Next)
		case op == format.Set || op == format.SetKey || op == format.SetRange:
			return fmt.Sprintf("g:%d:%d:%s", c, op, hexs(genKey(r, p)))
		case op == format.GetBoth || op == format.GetBothRange:
			k := genKey(r, p)
			return fmt.Sprintf("g:%d:%d:%s:%s", c, op, hexs(k), hexs(genData(r, p)))
		}
		return fmt.Sprintf("g:%d:%d", c, op)
	}
}

// gen produces the ops of a round: a prefill of puts, then random ops
// (with rlmdb's repairs of cursors C would mishandle).
type gen struct {
	r       *rng
	p       *profile
	ncur    int
	prefill int
	heal    int // cursor to heal with MDB_FIRST, -1: none
	reset   []int
	uninit  []bool
}

func (g *gen) next(prevOp, prevLine string, hasPrev bool, depth int) string {
	if hasPrev {
		parts := strings.Split(prevLine, " | ")[1:]
		now := make([]bool, len(parts))
		for i, c := range parts {
			now[i] = strings.Contains(c, " rc=22 ")
		}
		if strings.HasPrefix(prevOp, "d:") || strings.HasPrefix(prevOp, "cd:") {
			for n, u := range now {
				if u && !(n >= len(g.uninit) || g.uninit[n]) {
					g.reset = append(g.reset, n)
				}
			}
		}
		g.uninit = now
	}
	if len(g.reset) > 0 {
		n := g.reset[0]
		g.reset = g.reset[1:]
		return fmt.Sprintf("g:%d:%d:%s", n, format.SetRange, hexs(make([]byte, 8)))
	}
	if hasPrev {
		f := strings.Split(prevOp, ":")
		fields := strings.Fields(prevLine)
		failed := len(fields) < 2 || fields[1] != "rc=0"
		if f[0] == "g" && failed && g.p.dbflags&format.DupSort != 0 {
			switch f[2] {
			case "2", "3", "15", "16", "17":
				g.heal, _ = strconv.Atoi(f[1])
			}
		}
	}
	if g.heal >= 0 {
		n := g.heal
		g.heal = -1
		return fmt.Sprintf("g:%d:%d", n, format.First)
	}
	r, p := g.r, g.p
	if g.prefill > 0 {
		g.prefill--
		return fmt.Sprintf("p:%s:%s:0", hexs(genKey(r, p)), hexs(genData(r, p)))
	}
	if p.nested {
		switch r.below(50) {
		case 0:
			if depth < 3 {
				return "nb"
			}
		case 1:
			if depth > 0 {
				return "nc"
			}
		case 2:
			if depth > 0 {
				return "na"
			}
		}
	}
	if p.drops {
		switch r.below(300) {
		case 0:
			return "dr:0"
		case 1:
			if depth == 0 {
				return "dr:1"
			}
		}
	}
	op := genOp(r, p, g.ncur*(depth+1))
	// MDB_CURRENT only on a cursor that is on an item (KNOWN_C_BUGS.md
	// #13).
	f := strings.Split(op, ":")
	if f[0] == "cp" && f[4] == strconv.Itoa(int(format.Current)) {
		if !(hasPrev && strings.Contains(prevLine, fmt.Sprintf("| c%s rc=0 ", f[1]))) {
			return strings.Join(f[:4], ":") + ":0"
		}
	}
	return op
}

// cRun is writeops running as a child process, one op at a time.
type cRun struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr strings.Builder
}

func startC(t testing.TB, o *oracle, dir string, dbflags uint32, ncur int, mapSize int) *cRun {
	cmd := exec.Command(o.writeops, dir, "t", strconv.Itoa(int(dbflags)), strconv.Itoa(ncur),
		strconv.Itoa(mapSize), strconv.Itoa(o.pageSize))
	c := &cRun{cmd: cmd}
	cmd.Stderr = &c.stderr
	var err error
	if c.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.stdout = bufio.NewReaderSize(out, 1<<20)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *cRun) step(op string) (string, bool) {
	if c.stdin == nil {
		return "", false
	}
	if _, err := io.WriteString(c.stdin, op+"\n"); err != nil {
		return "", false
	}
	line, err := c.stdout.ReadString('\n')
	if err != nil || line == "" {
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

func (c *cRun) finish() (string, string) {
	if c.stdin != nil {
		c.stdin.Close()
		c.stdin = nil
	}
	rest, _ := io.ReadAll(c.stdout)
	_ = c.cmd.Wait()
	lines := strings.Split(strings.TrimSpace(string(rest)), "\n")
	return lines[len(lines)-1], c.stderr.String()
}

// code is the C return code for the Go engine's err, as a C process
// prints it (the C runtime's errno numbers on Windows).
func code(err error) int { return coracle.CErrno(format.Code(err)) }

// probe is MDB_GET_CURRENT (+ count) on a cursor, formatted like
// writeops.c.
func probe(c *eng.Cursor, i int, dupsort bool) string {
	k, d, err := c.Get(nil, nil, format.GetCurrent)
	rc := code(err)
	ks, ds := "-", "-"
	if err == nil {
		ks, ds = hexs(k), hexs(d)
	}
	s := fmt.Sprintf(" | c%d rc=%d k=%s d=%s", i, rc, ks, ds)
	if rc == 0 && dupsort {
		n, err := c.Count()
		if err != nil {
			s += fmt.Sprintf(" n=%d:0", code(err))
		} else {
			s += fmt.Sprintf(" n=0:%d", n)
		}
	}
	return s
}

func line(name string, rc int, cur []*eng.Cursor, dupsort bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s rc=%d", name, rc)
	for i, c := range cur {
		b.WriteString(probe(c, i, dupsort))
	}
	return b.String()
}

// maskCounts removes the duplicate counts (" n=..") from a step's output:
// C's cached counts go stale (KNOWN_C_BUGS.md #14), Go's do not; the page
// audit checks the counts in the records instead.
func maskCounts(l string) string {
	parts := strings.Split(l, " | ")
	for i, p := range parts {
		parts[i], _, _ = strings.Cut(p, " n=")
	}
	return strings.Join(parts, " | ")
}

func itemOf(l string, n int) (string, string, bool) {
	parts := strings.Split(l, " | ")
	if n+1 >= len(parts) {
		return "", "", false
	}
	p := parts[n+1]
	_, k, _ := strings.Cut(p, " k=")
	k, _, _ = strings.Cut(k, " ")
	_, d, _ := strings.Cut(p, " d=")
	d, _, _ = strings.Cut(d, " ")
	return k, d, k != "-"
}

func countOf(l string, n int) (string, bool) {
	parts := strings.Split(l, " | ")
	if n+1 >= len(parts) {
		return "", false
	}
	_, c, ok := strings.Cut(parts[n+1], " n=")
	return c, ok
}

// staleSubcursors: KNOWN_C_BUGS.md #18 (see the Rust harness).
func staleSubcursors(op, cl, rl string) []int {
	if !strings.HasPrefix(op, "cd:") && !strings.HasPrefix(op, "d:") {
		return nil
	}
	cp, rp := strings.Split(cl, " | "), strings.Split(rl, " | ")
	if len(cp) != len(rp) || cp[0] != rp[0] {
		return nil
	}
	var stale []int
	key := func(x string) string { k, _, _ := strings.Cut(x, " d="); return k }
	for n := 1; n < len(cp); n++ {
		c, r := cp[n], rp[n]
		if c == r {
			continue
		}
		ok := strings.Contains(r, " rc=0 ") && (strings.Contains(c, " rc=-30798 ") ||
			(strings.Contains(c, " rc=0 ") && key(c) == key(r)))
		if !ok {
			return nil
		}
		stale = append(stale, n-1)
	}
	return stale
}

// staleEOF: KNOWN_C_BUGS.md #28 (see the Rust harness).
func staleEOF(op, prev, cl, rl string, hasPrev bool) (int, bool) {
	f := strings.Split(op, ":")
	if f[0] != "g" {
		return 0, false
	}
	n, err1 := strconv.Atoi(f[1])
	opn, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return 0, false
	}
	cp, rp := strings.Split(cl, " | "), strings.Split(rl, " | ")
	if len(cp) != len(rp) || len(cp) <= n+1 {
		return 0, false
	}
	for i := 1; i < len(cp); i++ {
		if i-1 != n && cp[i] != rp[i] {
			return 0, false
		}
	}
	switch format.CursorOp(opn) {
	case format.Next, format.NextDup, format.NextNoDup, format.NextMultiple:
		return n, cp[0] == "g rc=-30798" && rp[0] == "g rc=0"
	case format.GetMultiple:
		if !hasPrev {
			return 0, false
		}
		pp := strings.Split(prev, " | ")
		if n+1 >= len(pp) {
			return 0, false
		}
		key := func(x string) string { k, _, _ := strings.Cut(x, " d="); return k }
		return n, cp[0] == "g rc=0" && rp[0] == "g rc=0" && cp[n+1] == pp[n+1] && key(cp[n+1]) == key(rp[n+1])
	}
	return 0, false
}

func knownCBug(op, cl, rl string) bool {
	return strings.HasPrefix(op, "cp:") && strings.HasSuffix(op, ":"+strconv.Itoa(int(format.Current))) &&
		strings.HasPrefix(rl, "cp rc=-30798") && cl != rl
}

// driver is the state of a round.
type driver struct {
	t       testing.TB
	o       *oracle
	p       *profile
	ncur    int
	g       *gen
	list    []string
	nops    int
	tag     string
	dupsort bool
	c       *cRun
	prevOp  string
	prevL   string
	hasPrev bool
	prevC   string
	prevR   string
	hasRaw  bool
	heal    []string
	i       int
	pending string
	stopped bool
}

func (d *driver) nextOp(depth int) (string, bool) {
	if d.g == nil {
		if d.i < len(d.list) {
			return d.list[d.i], true
		}
		return "", false
	}
	if d.i >= d.nops {
		if depth > 0 {
			return "nc", true
		}
		return "", false
	}
	if len(d.heal) > 0 {
		op := d.heal[0]
		d.heal = d.heal[1:]
		return op, true
	}
	return d.g.next(d.prevOp, d.prevL, d.hasPrev, depth), true
}

// compare compares C's and Go's output of a step; false if C took a
// known-buggy path and the round must stop.
func (d *driver) compare(op, cl, rlRaw string) bool {
	rl := maskCounts(rlRaw)
	if d.g != nil {
		// A cursor whose cached duplicate count C let go stale is put back
		// on its item with MDB_GET_BOTH (KNOWN_C_BUGS.md #14).
		for n := 0; n < len(strings.Split(cl, " | "))-1; n++ {
			c1, _ := countOf(cl, n)
			c2, _ := countOf(rlRaw, n)
			if c1 != c2 {
				if k, v, ok := itemOf(rlRaw, n); ok {
					d.heal = append(d.heal, fmt.Sprintf("g:%d:%d:%s:%s", n, format.GetBoth, k, v))
				}
			}
		}
	}
	d.prevC, d.prevR, d.hasRaw = cl, rlRaw, true
	cl = maskCounts(cl)
	if cl != rl && d.dupsort {
		if stale := staleSubcursors(op, cl, rl); stale != nil {
			for _, n := range stale {
				if k, v, ok := itemOf(rl, n); ok {
					d.heal = append(d.heal, fmt.Sprintf("g:%d:%d:%s:%s", n, format.GetBoth, k, v))
				}
			}
			d.prevOp, d.prevL, d.hasPrev = op, rl, true
			d.i++
			return true
		}
	}
	if cl != rl {
		if n, ok := staleEOF(op, d.prevL, cl, rl, d.hasPrev); ok {
			if k, v, ok := itemOf(rl, n); ok {
				if d.dupsort {
					d.heal = append(d.heal, fmt.Sprintf("g:%d:%d:%s:%s", n, format.GetBoth, k, v))
				} else {
					d.heal = append(d.heal, fmt.Sprintf("g:%d:%d:%s", n, format.Set, k))
				}
			}
			d.prevOp, d.prevL, d.hasPrev = op, rl, true
			d.i++
			return true
		}
	}
	if cl != rl {
		if knownCBug(op, cl, rl) {
			_, ca, _ := strings.Cut(cl, " | ")
			_, ra, _ := strings.Cut(rl, " | ")
			if ca != ra {
				return false
			}
		} else {
			if len(op) > 200 {
				op = op[:200]
			}
			d.t.Fatalf("%s: step %d %q differs\n  C:  %s\n  Go: %s", d.tag, d.i, op, cl, rl)
		}
	}
	d.prevOp, d.prevL, d.hasPrev = op, cl, true
	d.i++
	return true
}

func cursorOf(op string) (int, bool) {
	f := strings.Split(op, ":")
	switch f[0] {
	case "g", "cp", "cd", "cr":
		n, err := strconv.Atoi(f[1])
		return n, err == nil
	}
	return 0, false
}

func splitmix64(st *uint64) uint64 {
	*st += 0x9e3779b97f4a7c15
	z := *st
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func bulkPut(txn *eng.Txn, dbi eng.DBI, n, seed uint64, l int) error {
	st := seed
	value := make([]byte, l)
	for i := uint64(0); i < n; i++ {
		key := binary.LittleEndian.AppendUint64(nil, splitmix64(&st))
		for j := 0; j < l; j += 8 {
			var x [8]byte
			binary.LittleEndian.PutUint64(x[:], splitmix64(&st))
			copy(value[j:], x[:])
		}
		if err := txn.Put(dbi, key, value, 0); err != nil {
			return err
		}
	}
	return nil
}

// goStep runs one op on the Go side.
func (d *driver) goStep(txn *eng.Txn, dbi eng.DBI, cur []*eng.Cursor, op string) string {
	f := strings.Split(op, ":")
	var err error
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch f[0] {
	case "p":
		err = txn.Put(dbi, unhex(f[1]), unhex(f[2]), uint32(atoi(f[3])))
	case "cp":
		err = cur[atoi(f[1])].Put(unhex(f[2]), unhex(f[3]), uint32(atoi(f[4])))
	case "d":
		var data []byte
		if len(f) > 2 {
			data = unhex(f[2])
		}
		err = txn.Del(dbi, unhex(f[1]), data)
	case "cd":
		err = cur[atoi(f[1])].Del(uint32(atoi(f[2])))
	case "cr":
		// C deletes and puts (writeops.c), which is what the Go writer's
		// MDB_CURRENT does where C's in-place replacement goes wrong.
		err = cur[atoi(f[1])].Put(unhex(f[2]), unhex(f[3]), format.Current)
	case "g":
		var k, v []byte
		if len(f) > 3 {
			k = unhex(f[3])
		}
		if len(f) > 4 {
			v = unhex(f[4])
		}
		_, _, err = cur[atoi(f[1])].Get(k, v, format.CursorOp(atoi(f[2])))
	default:
		d.t.Fatalf("bad op %s", f[0])
	}
	return line(f[0], code(err), cur, d.dupsort)
}

// level runs ops on txn (nesting depth depth) until the ops run out, an
// nc/na ends it, or C stops. cur are the cursors usable in it: its own,
// then its ancestors'. Returns the end ("", "nc" or "na") and C's line.
func (d *driver) level(txn *eng.Txn, dbi *eng.DBI, depth int, cur *[]*eng.Cursor) (string, string) {
	for {
		op, ok := d.nextOp(depth)
		if !ok {
			if depth != 0 {
				d.t.Fatalf("%s: script ends in a nested txn", d.tag)
			}
			return "", ""
		}
		if n, ok := cursorOf(op); ok && d.hasRaw && strings.HasPrefix(op, "cd:") {
			c1, _ := countOf(d.prevC, n)
			c2, _ := countOf(d.prevR, n)
			if c1 != c2 {
				// KNOWN_C_BUGS.md #14: C's cached count is stale; its delete
				// would act on it.
				d.stopped = true
				return "stop", ""
			}
		}
		cl, ok := d.c.step(op)
		if !ok {
			_, errOut := d.c.finish()
			if !strings.Contains(errOut, "Assertion") && errOut != "" {
				d.t.Fatalf("%s: C writeops failed at step %d: %s", d.tag, d.i, errOut)
			}
			d.t.Logf("NOTE: %s: C died at step %d: %s", d.tag, d.i, strings.TrimSpace(errOut))
			d.stopped = true
			return "stop", ""
		}
		var rl string
		switch {
		case op == "nc" && depth > 0:
			return "nc", cl
		case op == "na" && depth > 0:
			return "na", cl
		case op == "nb":
			child, err := d.o.beginChild(txn)
			if err != nil {
				rl = line("nb", code(err), *cur, d.dupsort)
				break
			}
			ccur := make([]*eng.Cursor, 0, d.ncur+len(*cur))
			for i := 0; i < d.ncur; i++ {
				c, err := child.OpenCursor(*dbi)
				if err != nil {
					d.t.Fatal(err)
				}
				ccur = append(ccur, c)
			}
			ccur = append(ccur, *cur...)
			if !d.compare(op, cl, line("nb", 0, ccur, d.dupsort)) {
				d.stopped = true
				return "stop", ""
			}
			end, ecl := d.level(child, dbi, depth+1, &ccur)
			if end == "stop" {
				return "stop", ""
			}
			for _, c := range ccur[:d.ncur] {
				c.Close()
			}
			rc := 0
			if end == "nc" {
				rc = code(child.Commit())
			} else {
				child.Abort()
			}
			if !d.compare(end, ecl, line(end, rc, *cur, d.dupsort)) {
				d.stopped = true
				return "stop", ""
			}
			continue
		case op == "dr:0":
			rl = line("dr", code(txn.Drop(*dbi, false)), *cur, d.dupsort)
		case op == "dr:1":
			for _, c := range *cur {
				c.Close()
			}
			rc := code(txn.Drop(*dbi, true))
			var err error
			if *dbi, err = txn.OpenDBI([]byte("t"), d.p.dbflags|format.Create); err != nil {
				d.t.Fatal(err)
			}
			*cur = (*cur)[:0]
			for i := 0; i < d.ncur; i++ {
				c, err := txn.OpenCursor(*dbi)
				if err != nil {
					d.t.Fatal(err)
				}
				*cur = append(*cur, c)
			}
			rl = line("dr", rc, *cur, d.dupsort)
		case strings.HasPrefix(op, "b:"):
			f := strings.Split(op, ":")
			n, _ := strconv.ParseUint(f[1], 10, 64)
			seed, _ := strconv.ParseUint(f[2], 10, 64)
			l, _ := strconv.Atoi(f[3])
			rl = line("b", code(bulkPut(txn, *dbi, n, seed, l)), *cur, d.dupsort)
		default:
			rl = d.goStep(txn, *dbi, *cur, op)
		}
		if !d.compare(op, cl, rl) {
			d.stopped = true
			return "stop", ""
		}
	}
}

func (o *oracle) beginChild(txn *eng.Txn) (*eng.Txn, error) {
	return txn.Env().BeginTxn(txn, 0)
}

// runRound runs one round in lock step; true if every step and the
// committed result agreed (false: C stopped on a known C bug).
func runRound(t testing.TB, o *oracle, p *profile, ncur int, g *gen, nops int, list []string, tag string, mapSize int) bool {
	cdir, gdir := t.TempDir(), t.TempDir()
	if o.dataFile != nil {
		for _, dir := range []string{cdir, gdir} {
			if err := os.WriteFile(filepath.Join(dir, "data.mdb"), o.dataFile, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	env, err := eng.Open(gdir, eng.Options{
		Format: o.f, PageSize: o.pageSize, MapSize: int64(mapSize), MaxDBs: 8,
		Flags: format.NoSync, OpenLock: eng.OpenMemLock,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	if o.dirtyLimit > 0 {
		env.SetDirtyLimit(o.dirtyLimit)
	}
	txn, err := env.BeginTxn(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Abort()
	dbi, err := txn.OpenDBI([]byte("t"), p.dbflags|format.Create)
	if err != nil {
		t.Fatal(err)
	}
	cur := make([]*eng.Cursor, ncur)
	for i := range cur {
		if cur[i], err = txn.OpenCursor(dbi); err != nil {
			t.Fatal(err)
		}
	}
	d := &driver{t: t, o: o, p: p, ncur: ncur, g: g, nops: nops, list: list, tag: tag,
		dupsort: p.dbflags&format.DupSort != 0, c: startC(t, o, cdir, p.dbflags, ncur, mapSize)}
	if end, _ := d.level(txn, &dbi, 0, &cur); end == "stop" {
		d.c.finish()
		return false
	}
	cCommit, errOut := d.c.finish()
	for _, c := range cur {
		c.Close()
	}
	gCommit := fmt.Sprintf("commit rc=%d", code(txn.Commit()))
	if cCommit != gCommit {
		t.Fatalf("%s: commit: C %q, Go %q; C stderr %s", tag, cCommit, gCommit, errOut)
	}
	env.Close()
	// Same committed content, valid page usage, same pages.
	_, cd, _ := run(t, o.dumpTool(), "-s", "t", cdir)
	_, gd, _ := run(t, o.dumpTool(), "-s", "t", gdir)
	if normDump(cd) != normDump(gd) {
		// Dumped once more: a dump that changes although nothing wrote the
		// environment in between was misread, not miswritten (rlmdb f679226).
		redump := func(side, dir, first string) string {
			_, second, _ := run(t, o.dumpTool(), "-s", "t", dir)
			if normDump(second) == normDump(first) {
				return side + " dumped again: the same\n"
			}
			return side + " dumped again: different\n" + dumpDiff("first", normDump(first), "second", normDump(second))
		}
		again := redump("C", cdir, cd) + redump("Go", gdir, gd)
		t.Fatalf("%s: dumps differ (environments kept: C %s, Go %s)\n%s%s", tag,
			keepEnv(t, cdir), keepEnv(t, gdir), dumpDiff("C", normDump(cd), "Go", normDump(gd)), again)
	}
	if rc, out, errs := run(t, o.peer, "check-freelist", gdir); rc != 0 {
		t.Fatalf("%s: check-freelist of Go:\n%s\n%s", tag, out, errs)
	} else if rc, _, errs := run(t, o.peer, "check-freelist", cdir); rc != 0 {
		// KNOWN_C_BUGS.md #14: C's MDB_NODUPDATA cursor delete subtracts a
		// stale count; only md_entries may be off, so files differ there.
		t.Logf("NOTE: %s: C's page audit: %s", tag, strings.TrimSpace(errs))
		return true
	}
	if err := compareDataFiles(filepath.Join(cdir, "data.mdb"), filepath.Join(gdir, "data.mdb")); err != nil {
		t.Fatalf("%s: %v", tag, err)
	}
	// The Go engine reads C's environment as its own.
	if strings.Join(goDump(t, cdir, []byte("t"), false), "\n") != strings.Join(goDump(t, gdir, []byte("t"), false), "\n") {
		t.Fatalf("%s: Go reads C's environment differently", tag)
	}
	return true
}

func (o *oracle) dumpTool() string { return o.dump }

// normDump drops the mapaddr= header line that C 1.0's mdb_dump writes
// with the process's own map address (rlmdb KNOWN_C_BUGS.md, LMDB 1.0
// section, mdb_env_info).
func normDump(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "mapaddr=") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// keepEnv copies the environment in dir out of the test's temporary
// directory, which is removed when the test ends, for inspection, and returns
// where it went.
func keepEnv(t testing.TB, dir string) string {
	t.Helper()
	kept, err := os.MkdirTemp("", "lmdb-differential-")
	if err != nil {
		return dir + " (not kept: " + err.Error() + ")"
	}
	files, err := os.ReadDir(dir)
	for _, f := range files {
		var b []byte
		if b, err = os.ReadFile(filepath.Join(dir, f.Name())); err == nil {
			err = os.WriteFile(filepath.Join(kept, f.Name()), b, 0o644)
		}
		if err != nil {
			break
		}
	}
	if err != nil {
		return dir + " (not kept: " + err.Error() + ")"
	}
	return kept
}

// dumpDiff is where two dumps (the data lines of mdb_dump: odd lines keys,
// even lines values) differ, for a failure message: the line counts, then the
// lines between the common head and tail, at most 16 per side. Where both
// sides have a line there, it is shown from a little before the first
// character that differs (with its byte offset in the key or value),
// otherwise from its start; 160 characters at most.
func dumpDiff(aName, a, bName, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	head := 0
	for head < len(la) && head < len(lb) && la[head] == lb[head] {
		head++
	}
	tail := 0
	for tail < len(la)-head && tail < len(lb)-head && la[len(la)-1-tail] == lb[len(lb)-1-tail] {
		tail++
	}
	da, db := la[head:len(la)-tail], lb[head:len(lb)-tail]
	var out strings.Builder
	fmt.Fprintf(&out, "%s %d lines, %s %d; the same in the first %d and the last %d\n", aName, len(la), bName, len(lb), head, tail)
	side := func(name string, lines, other []string) {
		fmt.Fprintf(&out, "%s, %d lines:\n", name, len(lines))
		for i, line := range lines {
			if i == 16 {
				break
			}
			from := 0
			if i < len(other) {
				k := 0
				for k < len(line) && k < len(other[i]) && line[k] == other[i][k] {
					k++
				}
				// The dump's lines are a space and hex digits.
				at := max(k-1, 0) / 2
				fmt.Fprintf(&out, "%7d differs at byte %d:", head+i+1, at)
				// From 20 bytes before it: character 1 + 2*byte.
				if start := at - 20; start > 0 {
					from = 1 + 2*start
				}
			} else {
				fmt.Fprintf(&out, "%7d:", head+i+1)
			}
			end := min(from+160, len(line))
			pre, post := "", ""
			if from > 0 {
				pre = "..."
			}
			if end < len(line) {
				post = "..."
			}
			fmt.Fprintf(&out, " %s%s%s (%d chars)\n", pre, line[min(from, len(line)):end], post, len(line))
		}
	}
	side(aName, da, db)
	side(bName, db, da)
	return out.String()
}

// runProfile runs rounds rounds of n random ops (half a prefill of puts);
// at least half must agree in full.
func runProfile(t *testing.T, o *oracle, p profile, seed uint64, rounds, n int) {
	r := &rng{s: seed | 1}
	full := 0
	for round := 0; round < rounds; round++ {
		ncur := 1 + r.below(4)
		g := &gen{r: r, p: &p, ncur: ncur, prefill: n / 2, heal: -1}
		tag := fmt.Sprintf("%x-%d", seed, round)
		if runRound(t, o, &p, ncur, g, n+n/2, nil, tag, 64<<20) {
			full++
		}
	}
	t.Logf("%d of %d rounds compared in full", full, rounds)
	if full*2 < rounds {
		t.Fatalf("too few rounds compared in full: %d of %d", full, rounds)
	}
}

// profiles are the profiles of rlmdb's write_differential.rs.
func profiles(long bool) map[string]struct {
	p           profile
	seed        uint64
	rounds, ops int
} {
	type pr = struct {
		p           profile
		seed        uint64
		rounds, ops int
	}
	m := map[string]pr{
		"plain":           {profile{dbflags: 0, keys: 300, big: true}, 0xd1, 12, 600},
		"plain_small":     {profile{dbflags: 0, keys: 40, big: true}, 0xd2, 12, 400},
		"intkey":          {profile{dbflags: format.IntegerKey, keys: 500}, 0xd3, 8, 600},
		"revkey":          {profile{dbflags: format.ReverseKey, keys: 200, big: true}, 0xd8, 6, 600},
		"dupsort":         {profile{dbflags: format.DupSort, keys: 30}, 0xd4, 12, 800},
		"dupsort_wide":    {profile{dbflags: format.DupSort, keys: 200}, 0xd5, 8, 800},
		"revdup":          {profile{dbflags: format.DupSort | format.ReverseDup, keys: 25}, 0xd9, 8, 800},
		"dupfixed":        {profile{dbflags: format.DupSort | format.DupFixed, keys: 20, dupfixedSize: 16}, 0xd6, 12, 800},
		"intdup":          {profile{dbflags: format.DupSort | format.DupFixed | format.IntegerDup, keys: 20, dupfixedSize: 8}, 0xd7, 10, 800},
		"nested_plain":    {profile{dbflags: 0, keys: 300, big: true, nested: true, drops: true}, 0xe1, 10, 800},
		"nested_dupsort":  {profile{dbflags: format.DupSort, keys: 40, nested: true, drops: true}, 0xe2, 10, 800},
		"nested_dupfixed": {profile{dbflags: format.DupSort | format.DupFixed, keys: 20, dupfixedSize: 8, nested: true, drops: true}, 0xe3, 8, 800},
		// Odd item sizes, 1 byte in particular (P_LEAF2 pages where C's
		// mp_upper grows with each item).
		"dupfixed1": {profile{dbflags: format.DupSort | format.DupFixed, keys: 6, dupfixedSize: 1}, 0xe5, 8, 800},
		"dupfixed3": {profile{dbflags: format.DupSort | format.DupFixed, keys: 6, dupfixedSize: 3, nested: true}, 0xe6, 8, 800},
	}
	if long {
		m["deep_plain"] = pr{profile{dbflags: 0, keys: 3000, big: true, longKeys: true}, 0xda, 3, 3000}
		m["deep_dupsort"] = pr{profile{dbflags: format.DupSort, keys: 600, longKeys: true}, 0xdb, 3, 3000}
		m["deep_dupfixed"] = pr{profile{dbflags: format.DupSort | format.DupFixed, keys: 400, dupfixedSize: 24, longKeys: true}, 0xdc, 3, 3000}
		m["nested_deep"] = pr{profile{dbflags: 0, keys: 2000, big: true, longKeys: true, nested: true, drops: true}, 0xe4, 3, 3000}
	}
	return m
}

func TestDifferential09(t *testing.T) {
	o := oracle09(t)
	for name, pr := range profiles(!testing.Short()) {
		t.Run(name, func(t *testing.T) { runProfile(t, o, pr.p, pr.seed, pr.rounds, pr.ops) })
	}
}

// TestDifferential09Spill runs some profiles with a small dirty limit on
// the Go side: spilling must not change what is written.
func TestDifferential09Spill(t *testing.T) {
	o := oracle09(t)
	o.dirtyLimit = 64
	ps := profiles(false)
	for _, name := range []string{"plain", "dupsort", "nested_plain"} {
		pr := ps[name]
		t.Run(name, func(t *testing.T) { runProfile(t, o, pr.p, pr.seed+1, 4, pr.ops) })
	}
}

// nodemax10 is 1.0's me_nodemax for pages of ps bytes.
func nodemax10(ps int) int { return (((ps - 24) / 2) &^ 1) - 2 }

func TestDifferential10(t *testing.T) {
	tool(t, "writeops10")
	for _, ps := range []int{1024, 4096, 16384, 65536} {
		o := oracle10(t, ps)
		for name, pr := range profiles(false) {
			p := pr.p
			p.appenddupNodemax = nodemax10(ps)
			rounds := max(pr.rounds/2, 2)
			t.Run(fmt.Sprintf("%d/%s", ps, name), func(t *testing.T) {
				runProfile(t, o, p, pr.seed+uint64(ps), rounds, pr.ops)
			})
		}
	}
}

func TestMain(m *testing.M) {
	if src := os.Getenv(copyHelperSrc); src != "" {
		os.Exit(copyHelper(src))
	}
	if src := os.Getenv(readHelperSrc); src != "" {
		os.Exit(readHelper(src))
	}
	os.Exit(m.Run())
}
