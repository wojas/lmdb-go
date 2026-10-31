// What differs between the implementations, the port side. Hand-written,
// next to the files tests/gen-port.sh generates from ../cgo.

package workloads

import lmdb "github.com/PowerDNS/lmdb-go/lmdb"

// Impl names the implementation: "port" (the Go implementation).
const Impl = "port"

// Describe says what this implementation is, for reports.
func Describe() string { return "the Go implementation (" + lmdb.VersionString() + ")" }
