//go:build unix && cgo

// lockfile.SysVKey against the host's ftok (rlmdb os/shared_lock.rs
// ftok_matches_libc): on macOS, whose libc ftok is FreeBSD's, and on Linux,
// where glibc and musl compute the same, for files on the test's file
// system.

package lockfiletest

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/lockfile"
)

func TestSysVKeyMatchesLibcFtok(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		path := filepath.Join(dir, "lock"+string(rune('a'+i))+".mdb")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			t.Fatal(err)
		}
		want := libcFtok(path, 'M')
		if want == -1 {
			t.Fatalf("ftok(%s) failed", path)
		}
		if got := lockfile.SysVKey(uint64(st.Dev), uint64(st.Ino)); got != want {
			t.Fatalf("%s (dev %#x, ino %#x): SysVKey %#x, libc ftok %#x", path, st.Dev, st.Ino, got, want)
		}
	}
}
