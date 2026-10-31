// What differs between the implementations, the cgo side. Hand-written;
// tests/gen-port.sh does not copy this file to ../port, which has its own.

package workloads

import lmdb "github.com/PowerDNS/lmdb-go/tests/cgolmdb"

// Impl names the implementation: "cgo" (lmdb-go v1.9.4 on C LMDB 0.9.35).
const Impl = "cgo"

// Describe says what this implementation is, for reports.
func Describe() string { return "lmdb-go v1.9.4 (cgo, " + lmdb.VersionString() + ")" }
