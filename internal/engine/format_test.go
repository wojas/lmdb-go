package engine

// Format selection at open (rlmdb DUAL_FORMAT.md §4.2, tests/default_format.rs
// and format_v10.rs).

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

func TestFormatSelection(t *testing.T) {
	dir09, dir10 := t.TempDir(), t.TempDir()
	open := func(dir string, opts Options) (*Env, error) {
		opts.Flags |= format.NoLock
		return Open(dir, opts)
	}
	// New environments: 0.9 by default, 1.0 on request with a page size.
	env, err := open(dir09, Options{})
	must(t, err)
	if env.Format() != format.V09 || env.MaxKeySize() != 511 {
		t.Fatal(env.Format(), env.MaxKeySize())
	}
	env.Close()
	_, err = open(t.TempDir(), Options{Format: format.V09, PageSize: 4096})
	expectErr(t, err, syscall.EINVAL, "page size with 0.9")
	_, err = open(t.TempDir(), Options{Format: format.V10, PageSize: 3000})
	expectErr(t, err, syscall.EINVAL, "page size not a power of two")
	_, err = open(t.TempDir(), Options{Format: format.V10, PageSize: 128})
	expectErr(t, err, syscall.EINVAL, "page size too small")
	env, err = open(dir10, Options{Format: format.V10, PageSize: 1024})
	must(t, err)
	if env.Format() != format.V10 || env.PageSize() != 1024 || env.MaxKeySize() != 442 {
		t.Fatal(env.Format(), env.PageSize(), env.MaxKeySize())
	}
	env.Close()
	// Existing files keep their format; asking for the other one fails.
	for _, c := range []struct {
		dir  string
		want format.Format
	}{{dir09, format.V09}, {dir10, format.V10}} {
		f, ok, err := SniffFormat(c.dir, 0)
		if err != nil || !ok || f != c.want {
			t.Fatal(f, ok, err)
		}
		env, err := open(c.dir, Options{})
		must(t, err)
		if env.Format() != c.want {
			t.Fatal(env.Format())
		}
		env.Close()
		other := format.V10
		if c.want == format.V10 {
			other = format.V09
		}
		_, err = open(c.dir, Options{Format: other})
		expectErr(t, err, format.VersionMismatch, "the other format")
	}
	// The process default and the environment variable apply to new files
	// only.
	SetDefaultFormat(format.V10)
	env, err = open(t.TempDir(), Options{})
	must(t, err)
	if env.Format() != format.V10 {
		t.Fatal(env.Format())
	}
	env.Close()
	env, err = open(dir09, Options{})
	must(t, err)
	if env.Format() != format.V09 {
		t.Fatal("default applied to an existing file")
	}
	env.Close()
	SetDefaultFormat(0)
	for _, v := range []string{"1.0", "10"} {
		t.Setenv(DefaultFormatEnv, v)
		if DefaultFormat() != format.V10 {
			t.Fatal(v, DefaultFormat())
		}
	}
	for _, v := range []string{"0.9", "09", "9"} {
		t.Setenv(DefaultFormatEnv, v)
		if DefaultFormat() != format.V09 {
			t.Fatal(v, DefaultFormat())
		}
	}
	t.Setenv(DefaultFormatEnv, "bogus")
	if DefaultFormat() != format.V09 {
		t.Fatal(DefaultFormat())
	}
	// Not an LMDB file.
	bad := t.TempDir()
	must(t, os.WriteFile(filepath.Join(bad, "data.mdb"), make([]byte, 4096), 0o644))
	_, err = open(bad, Options{})
	expectErr(t, err, format.Invalid, "zeros")
}

func TestV10FeaturesRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(page0 []byte, ps int)
		want error
	}{
		{"encrypted", func(p []byte, ps int) {
			// MDB_ENCRYPT in mm_flags (mm_dbs[0].md_flags).
			f, _ := format.Get16(p, 24+24+4)
			format.Put16(p, 24+24+4, f|uint16(format.Encrypt))
		}, format.EnvEncryption},
		{"checksums", func(p []byte, ps int) { format.Put16(p, ps-2, 32) }, format.BadChecksum},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			env, err := Open(dir, Options{Format: format.V10, PageSize: 4096, Flags: format.NoLock})
			must(t, err)
			env.Close()
			path := filepath.Join(dir, "data.mdb")
			data, err := os.ReadFile(path)
			must(t, err)
			c.mut(data[:4096], 4096)
			must(t, os.WriteFile(path, data, 0o644))
			_, err = Open(dir, Options{Flags: format.NoLock})
			expectErr(t, err, c.want, c.name)
		})
	}
}
