// Package data is the data generator of the benchmarks: a line-by-line
// port of rlmdb's bench/src/data.rs, which is in turn identical (by
// construction) to the generator at the top of bench/c/cbench.c. The
// same seeds and formulas produce the same keys, values, index streams and
// permutations as the C and Rust sides, so every implementation builds
// identical databases and does the same operations in the same order;
// Datacheck hashes everything for a comparison with rlmdb-bench.
//
//   - Keys are 16 bytes. Random keys: two big-endian splitmix64 outputs of
//     the key number; sequential keys: the big-endian key number followed
//     by 8 hash bytes (so they sort by number).
//   - Values are slices of a fixed pseudo-random pool of 192 KiB, at offset
//     (i * 64) & 0xffff for item i: zero-copy on every side.
//   - Index streams and permutations are splitmix64(salt + j) based, one
//     salt per stream (S*).
//   - The ls group (Lightning Stream style records) has a data set of its
//     own, generated record by record from the record number (LSKey,
//     LSValue): DNS-name-like keys of 20-40 bytes and values made of a
//     24-byte Lightning Stream header and a 40-100 byte payload from the
//     pool.
package data

import "encoding/binary"

// KeyLen is the length of every key.
const KeyLen = 16

// ValLen is the length of an ordinary value.
const ValLen = 100

const (
	seedKeys   = 0x5eed_0000_0000_0001
	seedPool   = 0x5eed_0000_0000_0002
	seedDups   = 0x5eed_0000_0000_0003
	seedLSKeys = 0x5eed_0000_0000_0004
	seedLSHdr  = 0x5eed_0000_0000_0005
)

// The salts of the index streams and permutations.
const (
	SGet       uint64 = 0x1000_0000_0000_0000 // stream of rand_get
	SOverwrite uint64 = 0x2000_0000_0000_0000 // permutation of rand_overwrite
	SDelete    uint64 = 0x3000_0000_0000_0000 // permutation of del_rand
	SDupGet    uint64 = 0x4000_0000_0000_0000 // streams of dup_get_both (keys; ^ 1: duplicates)
	SDupScan   uint64 = 0x5000_0000_0000_0000 // permutation of dup_next_dup
	SOvfGet    uint64 = 0x6000_0000_0000_0000 // stream of get_4k/get_64k
	SReader    uint64 = 0x7000_0000_0000_0000 // streams of readers8_get (+ thread << 48)
	SMixed     uint64 = 0x8000_0000_0000_0000 // stream of mixed_writer
	SLSUpdate  uint64 = 0x9000_0000_0000_0000 // stream of ls_update
	// SGoAPI is the stream of the goapi group (Go only, not in rlmdb).
	SGoAPI uint64 = 0xa000_0000_0000_0000
)

const poolSize = 3 * 65536

// Splitmix64 is splitmix64 used as a hash.
func Splitmix64(x uint64) uint64 {
	z := x + 0x9e37_79b9_7f4a_7c15
	z = (z ^ (z >> 30)) * 0xbf58_476d_1ce4_e5b9
	z = (z ^ (z >> 27)) * 0x94d0_49bb_1331_11eb
	return z ^ (z >> 31)
}

// Pool returns the value pool (little-endian splitmix64(seedPool + w)
// words).
func Pool() []byte {
	p := make([]byte, poolSize)
	for w := 0; w < poolSize/8; w++ {
		binary.LittleEndian.PutUint64(p[8*w:], Splitmix64(seedPool+uint64(w)))
	}
	return p
}

// ValOff is the offset of the value bytes of item i in the pool.
func ValOff(i uint64) int { return int((i * 64) & 0xffff) }

// Value is the len value bytes of item i: a slice of the pool.
func Value(pool []byte, i uint64, n int) []byte {
	off := ValOff(i)
	return pool[off : off+n : off+n]
}

// Keys holds keys of KeyLen bytes back to back, so that a key is a
// sub-slice of one allocation without pointers (cheap for the garbage
// collector, and what the C and Rust sides do).
type Keys []byte

// Key returns key i.
func (k Keys) Key(i int) []byte { return k[i*KeyLen : (i+1)*KeyLen : (i+1)*KeyLen] }

// Len is the number of keys.
func (k Keys) Len() int { return len(k) / KeyLen }

// RandKeys returns n random keys.
func RandKeys(n int) Keys {
	k := make(Keys, n*KeyLen)
	for i := 0; i < n; i++ {
		key2(k.Key(i), Splitmix64(seedKeys+2*uint64(i)), Splitmix64(seedKeys+2*uint64(i)+1))
	}
	return k
}

// SeqKeys returns n keys in ascending order.
func SeqKeys(n int) Keys {
	k := make(Keys, n*KeyLen)
	for i := 0; i < n; i++ {
		key2(k.Key(i), uint64(i), Splitmix64(uint64(i)))
	}
	return k
}

func key2(out []byte, a, b uint64) {
	binary.BigEndian.PutUint64(out[:8], a)
	binary.BigEndian.PutUint64(out[8:], b)
}

// RandIdx returns count indices in [0, n) from stream salt.
func RandIdx(salt uint64, count, n int) []uint32 {
	v := make([]uint32, count)
	for j := range v {
		v[j] = uint32(Splitmix64(salt+uint64(j)) % uint64(n))
	}
	return v
}

// Perm returns a permutation of [0, n) (Fisher-Yates from stream salt).
func Perm(salt uint64, n int) []uint32 {
	p := make([]uint32, n)
	for j := range p {
		p[j] = uint32(j)
	}
	for j := n - 1; j > 0; j-- {
		k := Splitmix64(salt+uint64(j)) % (uint64(j) + 1)
		p[j], p[k] = p[k], p[j]
	}
	return p
}

// DupData writes duplicate j of DUPSORT key i (ValLen bytes) into out.
func DupData(pool []byte, i, j uint64, out *[ValLen]byte) {
	binary.BigEndian.PutUint64(out[:8], Splitmix64(seedDups+i*16+j))
	copy(out[8:], Value(pool, i*16+j, ValLen-8))
}

// LSScale is the number of records of the ls data set per key of --n.
const LSScale = 5

// LSHdrLen is the length of the Lightning Stream header that starts every
// ls value: timestamp of the last change (big-endian nanoseconds since the
// Unix epoch), transaction id (big-endian), version (0), number of extra
// 8-byte blocks (0), flags (2 bytes, bit 0: deleted), 4 bytes padding.
const LSHdrLen = 24

// LSTxnID is the offset of the transaction id in the header.
const LSTxnID = 8

// LSKeyMax is the length of the longest ls key.
const LSKeyMax = 40

// LSValMax is the length of the longest ls value (header and payload).
const LSValMax = LSHdrLen + 100

// LSSyncTxnID is the transaction id of the last sync: every record of a
// freshly built ls data set has a header transaction id at or below it,
// so a scan for changed records (ls_scan) finds none.
const LSSyncTxnID = 1_000_000

// lsTSBase is the timestamp of transaction id 0, in nanoseconds since the
// Unix epoch.
const lsTSBase = 1_700_000_000_000_000_000

// lsNsPerTxn is the time between two transaction ids: the timestamp of a
// header lies in its transaction id's window.
const lsNsPerTxn = 2_000_000_000

var lsTLDs = [8]string{"com", "net", "org", "nl", "de", "uk", "io", "eu"}

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

// lsShape is the shape word of ls record i: key length, top-level domain,
// zone label length, payload length.
func lsShape(i uint64) uint64 { return Splitmix64(seedLSKeys + 64*i) }

// LSKey writes the key of ls record i into out and returns its length
// (20-40 bytes). The key is <host>.<zone>.<tld>: the host label is random
// letters followed by i in 6 base-36 digits, so keys are unique for
// i < 36^6 (the host label ends at the first dot) and sort in random
// order. Letter j is splitmix64(seedLSKeys + 64 * i + 1 + j).
func LSKey(i uint64, out *[LSKeyMax]byte) int {
	shape := lsShape(i)
	n := 20 + int(shape%21)
	tld := lsTLDs[(shape>>16)%8]
	letters := n - 8 - len(tld)
	zone := 3 + int((shape>>24)%uint64(letters-3))
	host := letters - zone
	letter := func(j int) byte {
		return 'a' + byte(Splitmix64(seedLSKeys+64*i+1+uint64(j))%26)
	}
	for j := 0; j < host; j++ {
		out[j] = letter(j)
	}
	v := i
	for j := host + 5; j >= host; j-- {
		out[j] = base36[v%36]
		v /= 36
	}
	out[host+6] = '.'
	for j := 0; j < zone; j++ {
		out[host+7+j] = letter(host + j)
	}
	out[n-len(tld)-1] = '.'
	copy(out[n-len(tld):n], tld)
	return n
}

// lsPayloadLen is the payload length of ls record i (40-100 bytes; the
// same for every value the record gets).
func lsPayloadLen(i uint64) int { return 40 + int((lsShape(i)>>40)%61) }

// lsHeader writes a Lightning Stream header with timestamp ts and
// transaction id txnid, flags 0, into out[:LSHdrLen].
func lsHeader(out []byte, ts, txnid uint64) {
	binary.BigEndian.PutUint64(out[:8], ts)
	binary.BigEndian.PutUint64(out[LSTxnID:LSTxnID+8], txnid)
	clear(out[16:LSHdrLen])
}

// LSValue writes the value of ls record i as built into out and returns
// its length: transaction id 1 + h % LSSyncTxnID (all at or below the last
// sync) and a timestamp in that transaction's window, where h is
// splitmix64(seedLSHdr + i); payload from the pool at ValOff(i).
func LSValue(pool []byte, i uint64, out *[LSValMax]byte) int {
	h := Splitmix64(seedLSHdr + i)
	txnid := 1 + h%LSSyncTxnID
	ts := lsTSBase + txnid*lsNsPerTxn + (h>>32)%lsNsPerTxn
	lsHeader(out[:], ts, txnid)
	plen := lsPayloadLen(i)
	copy(out[LSHdrLen:LSHdrLen+plen], Value(pool, i, plen))
	return LSHdrLen + plen
}

// LSUpdateValue writes the new value of ls record i written by put j of
// repetition it of ls_update into out and returns its length: transaction
// id LSSyncTxnID + 1 + it, timestamp j ns into that transaction's window,
// the same payload length as before, payload at ValOff(i + it + 1).
func LSUpdateValue(pool []byte, i, it, j uint64, out *[LSValMax]byte) int {
	txnid := LSSyncTxnID + 1 + it
	lsHeader(out[:], lsTSBase+txnid*lsNsPerTxn+j, txnid)
	plen := lsPayloadLen(i)
	copy(out[LSHdrLen:LSHdrLen+plen], Value(pool, i+it+1, plen))
	return LSHdrLen + plen
}

// LSTxnIDOf returns the transaction id in the Lightning Stream header of
// v, or false if v is too short to hold a header.
func LSTxnIDOf(v []byte) (uint64, bool) {
	if len(v) < LSHdrLen {
		return 0, false
	}
	return binary.BigEndian.Uint64(v[LSTxnID : LSTxnID+8]), true
}

// fnv is FNV-1a over b, continuing from h.
func fnv(h uint64, b []byte) uint64 {
	for _, c := range b {
		h ^= uint64(c)
		h *= 0x100_0000_01b3
	}
	return h
}

// words is v in native byte order, as the C side hashes its uint32_t
// arrays.
func words(v []uint32) []byte {
	b := make([]byte, 4*len(v))
	for k, w := range v {
		binary.NativeEndian.PutUint32(b[4*k:], w)
	}
	return b
}

// Datacheck is a hash of everything the generator produces for n: the
// datacheck group of cbench.c and data.rs's datacheck compute the same,
// so equal hashes mean the same data on all sides.
func Datacheck(n int) uint64 {
	h := uint64(0xcbf2_9ce4_8422_2325)
	h = fnv(h, RandKeys(n))
	h = fnv(h, SeqKeys(n))
	pool := Pool()
	h = fnv(h, pool)
	h = fnv(h, words(RandIdx(SGet, n, n)))
	h = fnv(h, words(Perm(SDelete, n)))
	var buf [ValLen]byte
	for i := uint64(0); i < 100; i++ {
		for j := uint64(0); j < 10; j++ {
			DupData(pool, i, j, &buf)
			h = fnv(h, buf[:])
		}
	}
	// The ls data set: every key and value (each preceded by its length
	// byte), the stream of ls_update and the values of its first rep.
	nls := LSScale * n
	var k [LSKeyMax]byte
	var v [LSValMax]byte
	for i := uint64(0); i < uint64(nls); i++ {
		kl := LSKey(i, &k)
		h = fnv(h, []byte{byte(kl)})
		h = fnv(h, k[:kl])
		vl := LSValue(pool, i, &v)
		h = fnv(h, []byte{byte(vl)})
		h = fnv(h, v[:vl])
	}
	upd := RandIdx(SLSUpdate, nls/100, nls)
	h = fnv(h, words(upd))
	for j, i := range upd {
		vl := LSUpdateValue(pool, uint64(i), 0, uint64(j), &v)
		h = fnv(h, v[:vl])
	}
	return h
}
