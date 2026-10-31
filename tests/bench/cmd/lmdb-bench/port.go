//go:build port

package main

import portwl "github.com/PowerDNS/lmdb-go/tests/bench/port"

// The port joins the binary once its lmdb package compiles (stage 2
// removes this file's build constraint).
func init() { impls = append(impls, impl{portwl.Impl, portwl.Describe(), portwl.Run}) }
