//go:build port

package main

import portpeer "github.com/PowerDNS/lmdb-go/tests/gopeer/port"

// The port joins the binary once its lmdb package compiles (stage 2
// removes this file's build constraint).
func init() { impls[portpeer.Impl] = portpeer.Main }
