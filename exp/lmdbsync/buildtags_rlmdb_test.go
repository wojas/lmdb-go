//go:build rlmdb

package lmdbsync

// buildTags is passed to `go build -tags` for test helper binaries so that
// they use the same LMDB library as the test itself.
const buildTags = "rlmdb"
