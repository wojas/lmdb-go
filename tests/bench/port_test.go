package bench

import portwl "github.com/PowerDNS/lmdb-go/tests/bench/port"

// The port's side of the benchmarks.
func init() { sides = append(sides, side{portwl.Impl, portwl.Run}) }
