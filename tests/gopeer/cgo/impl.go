// What differs between the implementations, the cgo side: lmdb-go v1.9.4
// (C LMDB 0.9.35). Hand-written; tests/gen-port.sh does not copy this file
// to ../port, which has its own impl.go.

package peer

import lmdb "github.com/PowerDNS/lmdb-go/tests/cgolmdb"

// Impl names the implementation, as gopeer's --impl option does.
const Impl = "cgo"

// implName describes the implementation, for messages.
func implName() string { return "lmdb-go v1 (cgo, " + lmdb.VersionString() + ")" }

// supportsFormat10 tells whether this implementation opens LMDB 1.0
// environments: LMDB 0.9 does not (--format 1.0 is a usage error, as for
// cpeer).
const supportsFormat10 = false

// readerMetaRace tells whether a read transaction can copy the roots of a
// newer commit than the txnid it pins, so that the next snapshot looks
// older (rlmdb KNOWN_C_BUGS.md #31): C LMDB's can.
const readerMetaRace = true

// setFormat asks for the LMDB 0.9 (v10 false) or 1.0 format before Open.
// Only 0.9 gets here (parseFormat refuses 1.0), which lmdb-go v1 always
// opens.
func setFormat(env *lmdb.Env, v10 bool) error { return nil }

// isFormat10 tells whether an open env is an LMDB 1.0 environment.
func isFormat10(env *lmdb.Env) bool { return false }

// setPageSize sets the page size of a new LMDB 1.0 environment; never
// called here (parseArgs refuses --pagesize without LMDB 1.0).
func setPageSize(env *lmdb.Env, size int) error { return nil }
