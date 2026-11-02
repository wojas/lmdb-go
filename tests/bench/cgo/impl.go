// What differs between the implementations, the cgo side. Hand-written;
// tests/gen-port.sh does not copy this file to ../port, which has its own.

package workloads

import lmdb "github.com/PowerDNS/lmdb-go/tests/cgolmdb"

// Impl names the implementation: "cgo" (the pinned reference, lmdb-go
// v1.9.4's Go code on C LMDB 0.9.36 plus ITS#10615, ITS#10642 and
// ITS#10643, the same code as 0.9.37).
const Impl = "cgo"

// Describe says what this implementation is, for reports.
func Describe() string {
	return "lmdb-go v1.9.4's Go code at e5af975 (cgo, " + lmdb.VersionString() +
		", plus ITS#10615, ITS#10642, ITS#10643)"
}
