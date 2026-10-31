//go:build port

package bench

import portwl "github.com/PowerDNS/lmdb-go/tests/bench/port"

// The port joins the benchmarks once its lmdb package compiles (stage 2
// removes this file's build constraint).
func init() { sides = append(sides, side{portwl.Impl, portwl.Run}) }
