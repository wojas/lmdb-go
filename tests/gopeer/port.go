package main

import portpeer "github.com/PowerDNS/lmdb-go/tests/gopeer/port"

// The port's side of the peer.
func init() { impls[portpeer.Impl] = portpeer.Main }
