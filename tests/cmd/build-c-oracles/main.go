// Command build-c-oracles builds the C oracles of the tests from the C
// sources rlmdb vendors, on any OS: what scripts/build-c-oracles.sh and
// scripts/build-c-writeops.sh build in the Linux containers, as one Go
// program, so that it also runs on Windows (no sh there), where it builds
// with MinGW-w64's gcc as rlmdb's interop/build.rs does.
//
// It builds, against C LMDB 0.9 (tests/lmdb-c/0.9, LMDB 0.9.36) and C LMDB
// 1.0 (tests/lmdb-c/1.0, the pinned mdb.RE/1.0 commit; names ending in 10):
// the interop peer cpeer, the C tools mdb_stat, mdb_dump, mdb_load and
// mdb_copy, layout_dump, the write-differential script runner writeops,
// and writeops10_ndebug (writeops against a 1.0 library built with
// -DNDEBUG, rlmdb KNOWN_C_BUGS.md row 37). The output directory then serves
// as LMDB_ORACLE_BIN (tests/lockfile) and RLMDB_BIN (tests/engine).
//
// Usage (in the tests module):
//
//	go run ./cmd/build-c-oracles [-cc compiler] [-os goos | -windows] <rlmdb root> <out dir>
//
// <rlmdb root> is the rlmdb checkout (its repository root: tests/lmdb-c,
// tests/compat, interop/c, rlmdb/tests/c/writeops.c). The compiler is
// -cc, else $CC, else gcc on Windows and cc elsewhere; it may carry
// arguments ("cc -arch x86_64", "zig cc -target x86_64-openbsd"). -os
// names the system the programs are for, as a GOOS (default: the one this
// runs on), for cross builds; -windows is -os windows, for example with
// x86_64-w64-mingw32-gcc from another OS.
//
// The flags are those of rlmdb's interop/build.rs (the upstream Makefile's
// -O2 -g -W -Wall -Wno-unused-parameter, and -pthread except on Windows).
// On Windows the programs that are not upstream's get tests/compat on the
// include path (rlmdb's POSIX shims) and MinGW's C99 printf
// (-D__USE_MINGW_ANSI_STDIO=1); nothing links -ldl or -pthread, and the
// executables end in .exe. C 1.0's programs link -ldl for dlopen
// (module.c), except where there is no libdl: Windows (LoadLibrary),
// OpenBSD and NetBSD (dlopen is in every dynamically linked program
// there, from ld.so or libc). Only <out dir> is written.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

func main() {
	ccFlag := flag.String("cc", "", "C compiler (default $CC, else gcc on Windows and cc elsewhere)")
	goos := flag.String("os", runtime.GOOS, "the system to build for, as a GOOS")
	windows := flag.Bool("windows", false, "build Windows executables (-os windows)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: build-c-oracles [-cc compiler] [-os goos | -windows] <rlmdb root> <out dir>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	if *windows {
		*goos = "windows"
	}
	cc := *ccFlag
	if cc == "" {
		cc = os.Getenv("CC")
	}
	if cc == "" {
		cc = "cc"
		if *goos == "windows" {
			cc = "gcc"
		}
	}
	b := &builder{cc: strings.Fields(cc), windows: *goos == "windows", libdl: hasLibdl(*goos),
		root: flag.Arg(0), out: flag.Arg(1)}
	if err := b.run(); err != nil {
		fmt.Fprintln(os.Stderr, "build-c-oracles:", err)
		os.Exit(1)
	}
}

// hasLibdl reports whether programs for goos link dlopen from a libdl:
// not on Windows, OpenBSD and NetBSD, which have none.
func hasLibdl(goos string) bool {
	switch goos {
	case "windows", "openbsd", "netbsd":
		return false
	}
	return true
}

type builder struct {
	cc       []string
	windows  bool
	libdl    bool // link -ldl for C 1.0's module.c (hasLibdl)
	root     string
	out      string
	mu       sync.Mutex
	firstErr error
}

// The sources of the C peer, in interop/c (rlmdb interop/build.rs
// CPEER_SOURCES).
var peerSources = []string{"cpeer.c", "cpeer_data.c", "cpeer_loops.c", "cpeer_misc.c", "cpeer_pagewalk.c", "dataset.c"}

// The upstream tools built for both releases.
var tools = []string{"mdb_stat", "mdb_dump", "mdb_load", "mdb_copy"}

// release is one C release: its source directory, the library's sources,
// the suffix of its program names and its extra link arguments.
type release struct {
	tag    string // "09" or "10": the object directory
	dir    string
	lib    []string
	suffix string
	libs   []string
}

func (b *builder) run() error {
	l09 := filepath.Join(b.root, "tests", "lmdb-c", "0.9")
	l10 := filepath.Join(b.root, "tests", "lmdb-c", "1.0")
	if d := os.Getenv("LMDB10_DIR"); d != "" {
		l10 = d
	}
	peer := filepath.Join(b.root, "interop", "c")
	writeops := filepath.Join(b.root, "rlmdb", "tests", "c", "writeops.c")
	for _, f := range []string{filepath.Join(l09, "mdb.c"), filepath.Join(l10, "mdb.c"), filepath.Join(peer, "cpeer.c"), writeops} {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("%s not found (is %s an rlmdb checkout?)", f, b.root)
		}
	}
	if err := os.MkdirAll(b.out, 0o755); err != nil {
		return err
	}
	var dl []string
	if b.libdl {
		dl = []string{"-ldl"}
	}
	rels := []release{
		{tag: "09", dir: l09, lib: []string{"mdb.c", "midl.c"}},
		{tag: "10", dir: l10, lib: []string{"mdb.c", "midl.c", "module.c"}, suffix: "10", libs: dl},
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(2, runtime.NumCPU()))
	for _, r := range rels {
		// The library objects once per release, then the programs.
		lib := b.objects(r, "lib", r.dir, r.lib, false, nil, sem)
		if len(lib) != len(r.lib) {
			break
		}
		for _, t := range tools {
			wg.Add(1)
			go func() {
				defer wg.Done()
				objs := b.objects(r, t, r.dir, []string{t + ".c"}, false, nil, sem)
				b.link(t+r.suffix, append(objs, lib...), r.libs, sem)
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			objs := b.objects(r, "cpeer", peer, peerSources, true, nil, sem)
			b.link("cpeer"+r.suffix, append(objs, lib...), r.libs, sem)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// layout_dump includes mdb.c itself: it only needs midl.o.
			objs := b.objects(r, "layout_dump", peer, []string{"layout_dump.c"}, true, nil, sem)
			b.link("layout_dump"+r.suffix, append(objs, lib[1]), nil, sem)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			objs := b.objects(r, "writeops", filepath.Dir(writeops), []string{filepath.Base(writeops)}, true, nil, sem)
			b.link("writeops"+r.suffix, append(objs, lib...), r.libs, sem)
			if r.tag != "10" {
				return
			}
			// writeops10_ndebug: only mdb.c without assertions.
			ndebug := b.objects(r, "lib_ndebug", r.dir, []string{"mdb.c"}, false, []string{"-DNDEBUG"}, sem)
			b.link("writeops10_ndebug", append(append(objs, ndebug...), lib[1:]...), r.libs, sem)
		}()
	}
	wg.Wait()
	if b.firstErr != nil {
		return b.firstErr
	}
	fmt.Println("build-c-oracles: built in", b.out)
	return nil
}

// fail records the first error.
func (b *builder) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.firstErr == nil {
		b.firstErr = err
	}
}

func (b *builder) cflags(r release, ours bool, extra []string) []string {
	f := []string{"-O2", "-g", "-W", "-Wall", "-Wno-unused-parameter", "-I" + r.dir}
	if !b.windows {
		f = append(f, "-pthread")
	} else if ours {
		f = append(f, "-I"+filepath.Join(b.root, "tests", "compat"), "-D__USE_MINGW_ANSI_STDIO=1")
	}
	return append(f, extra...)
}

// objects compiles srcs (in dir) for release r into obj/<tag>/<group> and
// returns the object files. ours marks programs that are not upstream's.
func (b *builder) objects(r release, group, dir string, srcs []string, ours bool, extra []string, sem chan struct{}) []string {
	objDir := filepath.Join(b.out, "obj", r.tag, group)
	if err := os.MkdirAll(objDir, 0o755); err != nil {
		b.fail(err)
		return nil
	}
	objs := make([]string, len(srcs))
	var wg sync.WaitGroup
	for i, s := range srcs {
		objs[i] = filepath.Join(objDir, strings.TrimSuffix(s, ".c")+".o")
		wg.Add(1)
		go func() {
			defer wg.Done()
			args := append(b.cflags(r, ours, extra), "-c", filepath.Join(dir, s), "-o", objs[i])
			b.exec(args, sem)
		}()
	}
	wg.Wait()
	return objs
}

// link links objs into the executable name (.exe on Windows).
func (b *builder) link(name string, objs, libs []string, sem chan struct{}) {
	exe := filepath.Join(b.out, name)
	if b.windows {
		exe += ".exe"
	}
	args := append([]string{"-o", exe}, objs...)
	if !b.windows {
		args = append(args, "-pthread")
	}
	b.exec(append(args, libs...), sem)
}

// exec runs the compiler with args, at most cap(sem) at a time.
func (b *builder) exec(args []string, sem chan struct{}) {
	sem <- struct{}{}
	defer func() { <-sem }()
	b.mu.Lock()
	failed := b.firstErr != nil
	b.mu.Unlock()
	if failed {
		return
	}
	cmd := exec.Command(b.cc[0], append(append([]string(nil), b.cc[1:]...), args...)...)
	cmd.Stderr = os.Stderr
	if out, err := cmd.Output(); err != nil {
		b.fail(fmt.Errorf("%s %s: %v%s", b.cc[0], strings.Join(args, " "), err, out))
	}
}
