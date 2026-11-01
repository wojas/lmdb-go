// Names of the named semaphores (macOS; OpenBSD, NetBSD and DragonFly in
// version 1) and the Windows mutexes: mdb_hash_val, mdb_pack85 and
// mdb_hash_enc of mdb.c 0.9.36 (lines 4823-4880), mdb_hash, mdb_pack85 and
// mdb_env_mname_init / MUTEXNAME of mdb.c 1.0 (lines 5920-5945); and the
// key of the BSDs' SysV semaphore set (ftok, mdb.c 1.0 line 6098). rlmdb
// os/shared_lock.rs and os/windows.rs (hash_val, pack85, hash_enc,
// NAME_HASH_LEN, mutex_name_v10, lock_file_id_bytes, ftok_key). Plain
// computation, so it is built and tested on every platform.

package lockfile

// hashInit is MDB_HASH_INIT, the FNV-1a 64-bit offset basis.
const hashInit = 0xcbf29ce484222325

// hashVal is mdb_hash_val: 64-bit FNV-1a over b, starting from h (1.0's
// mdb_hash is the same function, started from hashInit).
func hashVal(b []byte, h uint64) uint64 {
	for _, c := range b {
		h ^= uint64(c)
		h += h<<1 + h<<4 + h<<5 + h<<7 + h<<8 + h<<40
	}
	return h
}

// a85 is mdb_a85, the digits of the base-85 encoding (modified RFC 1924).
const a85 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"

// pack85V09 is mdb_pack85 of LMDB 0.9: five base-85 digits of l, least
// significant first.
func pack85V09(l uint64, out []byte) []byte {
	for i := 0; i < 5; i++ {
		out = append(out, a85[l%85])
		l /= 85
	}
	return out
}

// pack85V10 is mdb_pack85 of LMDB 1.0: up to ten base-85 digits of l,
// least significant first, stopping early when the rest is 0 (so a hash of
// 0 has no digits at all).
func pack85V10(l uint64, out []byte) []byte {
	for i := 0; i < 10 && l != 0; i++ {
		out = append(out, a85[l%85])
		l /= 85
	}
	return out
}

// hashEnc is mdb_hash_enc: the 10-character encoding of the FNV-1a hash of
// b, pack85(h) then pack85(h >> 32).
func hashEnc(b []byte) string {
	h := hashVal(b, hashInit)
	out := pack85V09(h, make([]byte, 0, 10))
	return string(pack85V09(h>>32, out))
}

// semNamesV09 are the names LMDB 0.9's exclusive opener writes into
// mti_rmname and mti_wmname: "/MDBr" and "/MDBw" followed by
// mdb_hash_enc of the lock file's identity. With short (NetBSD's
// MDB_SHORT_SEMNAMES, mdb.c 0.9.36 lines 4999-5011) the encoding is cut to
// nine characters, for names of 14 characters.
func semNamesV09(id []byte, short bool) (r, w string) {
	enc := hashEnc(id)
	if short {
		enc = enc[:9]
	}
	return "/MDBr" + enc, "/MDBw" + enc
}

// semNameV10 is MUTEXNAME(env, ch) of LMDB 1.0 after mdb_env_mname_init:
// "/MDB", ch ('r' or 'w'), then mdb_pack85 of mti_mutexid.
func semNameV10(mutexID uint64, ch byte) string {
	return string(pack85V10(mutexID, []byte{'/', 'M', 'D', 'B', ch}))
}

// lockFileID is what C hashes for the semaphore names: struct { dev_t dev;
// ino_t ino; } of the lock file. ino_t has 8 bytes on every platform with
// named semaphores, so ino is at offset 8; dev_t has devSize bytes, 4 on
// macOS, OpenBSD and DragonFly (then 4 bytes of padding follow) and 8 on
// NetBSD. LMDB 0.9 leaves the padding uninitialised, so its names are not
// reproducible, which is why every later opener reads them from the lock
// file; LMDB 1.0 zeroes the struct first (mdb.c 1.0 line 6075) but still
// stores the hash rather than have others recompute it. Here the padding
// is zero.
func lockFileID(dev uint64, devSize int, ino uint64) []byte {
	b := make([]byte, 16)
	if devSize == 8 {
		ne.PutUint64(b[0:], dev)
	} else {
		ne.PutUint32(b[0:], uint32(dev))
	}
	ne.PutUint64(b[8:], ino)
	return b
}

// SysVKey is the key under which the exclusive opener of a version 2 lock
// file on OpenBSD, NetBSD and DragonFly creates (or finds again) the SysV
// semaphore set: ftok(lockfile, 'M') (MDB_USE_SYSV_SEM, mdb.c 1.0 line
// 6098), computed from the lock file's device and inode numbers (its
// fstat; the path is not at hand where the set is made, as in rlmdb). The
// BSD libcs' ftok computes
//
//	FreeBSD, NetBSD, macOS: (unsigned)id << 24 | (st_dev & 0xff) << 16 | (st_ino & 0xffff)
//	OpenBSD:                (id & 0xff) << 24  | (st_dev & 0xff) << 16 | (st_ino & 0xffff)
//	DragonFly:              id << 24           | (st_dev & 0xff) << 16 | (st_ino & 0xffff)
//
// (gen/ftok.c of each libc; tests/lockfile compares this with the host's
// ftok), which agree for any id below 128, such as 'M'. The value is
// positive and below 2^31, so it is the same key_t whether key_t is a long
// (the BSDs) or a 32-bit int (macOS). Only the exclusive opener uses it;
// every other opener takes the set's id from the lock file.
func SysVKey(dev, ino uint64) int64 {
	return int64('M')<<24 | int64(dev&0xff)<<16 | int64(ino&0xffff)
}

// Windows: the names of the two kernel mutexes. "Global\MDB" is
// MUTEXNAME_PREFIX of LMDB 1.0 on Windows and the start of 0.9's names.
const winMutexPrefix = `Global\MDB`

// pack85Win32 is mdb_pack85 of LMDB 0.9 on Windows: five base-85 digits of
// l, least significant first. Its parameter is an unsigned long, which has
// 32 bits on Windows, so l is truncated first.
func pack85Win32(l uint32, out []byte) []byte { return pack85V09(uint64(l), out) }

// hashEncWin32 is mdb_hash_enc of LMDB 0.9 on Windows: pack85(h) then
// pack85(h >> 32), each of 32 bits only (so the names differ from the ones
// hashEnc would give for the same bytes).
func hashEncWin32(b []byte) string {
	h := hashVal(b, hashInit)
	out := pack85Win32(uint32(h), make([]byte, 0, 10))
	return string(pack85Win32(uint32(h>>32), out))
}

// mutexNamesV09 are the names LMDB 0.9's exclusive opener writes into
// mti_rmname and mti_wmname on Windows: "Global\MDBr" and
// "Global\MDBw" followed by mdb_hash_enc of the lock file's identity.
// Unlike the macOS semaphore names they are reproducible (the identity has
// no padding), but they are read from the file all the same.
func mutexNamesV09(id []byte) (r, w string) {
	enc := hashEncWin32(id)
	return winMutexPrefix + "r" + enc, winMutexPrefix + "w" + enc
}

// mutexNameV10 is MUTEXNAME(env, ch) of LMDB 1.0 on Windows:
// "Global\MDB", ch ('r' or 'w'), then mdb_pack85 of mti_mutexid (up to
// ten digits, a hash of 0 giving none).
func mutexNameV10(mutexID uint64, ch byte) string {
	return string(pack85V10(mutexID, append([]byte(winMutexPrefix), ch)))
}

// lockFileIDWin32 is what C hashes for the Windows mutex names: struct {
// DWORD volume; DWORD nhigh; DWORD nlow; } of the lock file, from
// GetFileInformationByHandle (12 bytes, no padding).
func lockFileIDWin32(volume, indexHigh, indexLow uint32) []byte {
	b := make([]byte, 12)
	ne.PutUint32(b[0:], volume)
	ne.PutUint32(b[4:], indexHigh)
	ne.PutUint32(b[8:], indexLow)
	return b
}
