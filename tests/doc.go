// Package tests holds the tests and benchmarks that compare the Go
// implementation of LMDB (the parent module, imported as
// github.com/PowerDNS/lmdb-go/lmdb) with the original cgo lmdb-go v1
// (the pinned copy in ./cgolmdb, imported as
// github.com/PowerDNS/lmdb-go/tests/cgolmdb), side by side in one binary.
//
// This is a separate module so that the C sources of the copy, and cgo,
// never reach users of the parent module. Run it with
//
//	cd tests && go test ./...
package tests
