// Command gopeer is the Go peer of rlmdb's interop harness (rlmdb
// rust/interop/README.md, "The peer command protocol"):
//
//	gopeer <command> <args...> [options...] [--impl cgo|port]
//
// One binary, two implementations: --impl cgo runs the protocol on the
// pinned cgo lmdb-go v1.9.4 (C LMDB 0.9.35; a C 0.9 peer, next to rlmdb's
// cpeer on C 0.9.36), --impl port (the default) on the Go implementation.
// The peer itself is package ./cgo; ./port is the same code generated for
// the port (tests/gen-port.sh). See README.md.
package main

//go:generate sh ../gen-port.sh .

import (
	"fmt"
	"os"

	cgopeer "github.com/PowerDNS/lmdb-go/tests/gopeer/cgo"
)

// impls maps --impl values to the peer on that implementation; port.go
// adds the port.
var impls = map[string]func(args []string) int{
	cgopeer.Impl: cgopeer.Main,
}

func main() {
	impl, args := "port", make([]string, 0, len(os.Args))
	for k := 1; k < len(os.Args); k++ {
		if os.Args[k] == "--impl" && k+1 < len(os.Args) {
			k++
			impl = os.Args[k]
			continue
		}
		args = append(args, os.Args[k])
	}
	run, ok := impls[impl]
	if !ok {
		fmt.Fprintf(os.Stderr, "gopeer: --impl %s: unknown implementation (cgo or port)\n", impl)
		os.Exit(2)
	}
	os.Exit(run(args))
}
