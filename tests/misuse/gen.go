// Package misuse compares the Go implementation with the pinned cgo
// reference (lmdb-go v1.9.4's Go code on C LMDB 0.9.36 plus ITS#10615,
// ITS#10642 and ITS#10643) for handles used after they ended: cursors after
// Close or after the end of their transaction, transactions after Commit,
// Abort or Reset, and copies of closed cursors (misuse_test.go). lmdb-go
// v1 passed NULL or freed C handles to C there; the port returns EINVAL
// (lmdb/DEVIATIONS.md, "Handles used after they ended").
//
// The cases are written once, in ./cgo against the cgo reference; ./port
// is generated from it for the port by tests/gen-port.sh.
package misuse

//go:generate sh ../gen-port.sh .
