package main

import portwl "github.com/PowerDNS/lmdb-go/tests/bench/port"

// The port's side of the benchmark.
func init() { impls = append(impls, impl{portwl.Impl, portwl.Describe(), portwl.Run}) }
