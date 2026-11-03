//go:build rlmdb

package lmdb

import (
	"strings"
	"testing"
)

// TestRlmdbLinked checks that the rlmdb build tag really linked rlmdb's
// liblmdb and not the bundled sources or another liblmdb on the search path.
func TestRlmdbLinked(t *testing.T) {
	_, _, _, str := Version()
	t.Logf("LMDB version: %s", str)
	if !strings.Contains(str, "(rlmdb ") {
		t.Fatalf("built with -tags rlmdb, but the linked library is %q", str)
	}
}
