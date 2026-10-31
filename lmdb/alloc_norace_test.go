//go:build !race

package lmdb

// raceEnabled: the race detector changes allocation counts (TestAllocs).
const raceEnabled = false
