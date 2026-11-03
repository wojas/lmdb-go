package engine

// Open refuses counts of named databases and reader slots beyond the
// limits (rlmdb rust_env.rs max_dbs_beyond_the_limit_is_refused and
// max_readers_beyond_the_limit_is_refused; KNOWN_C_BUGS.md rows 54 and 60)
// before it creates any file.

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func TestOpenRefusesCountsBeyondTheLimits(t *testing.T) {
	for _, c := range []struct {
		name string
		opts Options
	}{
		{"max_dbs", Options{MaxDBs: format.MaxDBsLimit + 1}},
		{"max_dbs", Options{MaxDBs: 1<<32 - 2}},
		{"max_dbs", Options{MaxDBs: 1<<32 - 1}},
		{"max_readers", Options{MaxReaders: format.MaxReadersLimit + 1}},
		{"max_readers", Options{MaxReaders: 1<<32 - 1}},
	} {
		dir := t.TempDir()
		c.opts.OpenLock = OpenMemLock
		env, err := Open(dir, c.opts)
		if err == nil {
			env.Close()
		}
		if !errors.Is(err, syscall.EINVAL) {
			t.Errorf("%s %+v: %v, want EINVAL", c.name, c.opts, err)
		}
		for _, f := range []string{"data.mdb", "lock.mdb"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				t.Errorf("%s %+v: %s created", c.name, c.opts, f)
			}
		}
	}
	// At the limits (and well within) it opens.
	env, _ := openTest(t, format.V09, Options{MaxDBs: format.MaxDBsLimit, MaxReaders: format.MaxReadersLimit})
	if env.MaxReaders() != format.MaxReadersLimit {
		t.Errorf("MaxReaders %d", env.MaxReaders())
	}
	w := beginW(t, env)
	defer w.Abort()
	if _, err := w.OpenDBI([]byte("one"), format.Create); err != nil {
		t.Fatal(err)
	}
}
