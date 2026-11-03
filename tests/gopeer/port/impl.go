// What differs between the implementations, the port side: the Go
// implementation, which opens both data formats with lmdb-go v2's API
// (Env.SetFormat, Env.Format, V09, V10) and sets the page size of new LMDB
// 1.0 environments (Env.SetPageSize, like rlmdb's
// EnvironmentBuilder::page_size and C 1.0's mdb_env_set_pagesize).
// Hand-written, next to the files tests/gen-port.sh generates from ../cgo.

package peer

import lmdb "github.com/PowerDNS/lmdb-go/lmdb"

// Impl names the implementation, as gopeer's --impl option does.
const Impl = "port"

// implName describes the implementation, for messages.
func implName() string { return "the Go implementation (" + lmdb.VersionString() + ")" }

// supportsFormat10 tells whether this implementation opens LMDB 1.0
// environments.
const supportsFormat10 = true

// readerMetaRace tells whether a read transaction can copy the roots of a
// newer commit than the txnid it pins (rlmdb KNOWN_C_BUGS.md row 31). The
// port follows rlmdb, which re-checks the meta page: it cannot race.
const readerMetaRace = false

// setFormat asks for the LMDB 0.9 (v10 false) or 1.0 format before Open.
func setFormat(env *lmdb.Env, v10 bool) error {
	if v10 {
		return env.SetFormat(lmdb.V10)
	}
	return env.SetFormat(lmdb.V09)
}

// isFormat10 tells whether an open env is an LMDB 1.0 environment.
func isFormat10(env *lmdb.Env) bool { return env.Format() == lmdb.V10 }

// delNullData tells whether Txn.Del(dbi, key, nil) is mdb_del with NULL
// data: the port's is (lmdb DEVIATIONS.md).
const delNullData = true

// setPageSize sets the page size of a new LMDB 1.0 environment before
// Open.
func setPageSize(env *lmdb.Env, size int) error { return env.SetPageSize(size) }
