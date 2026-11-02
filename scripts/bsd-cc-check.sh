#!/bin/sh
# Compile-check the BSD builds with cgo, from macOS or Linux, without a BSD
# machine: the cgo lock files (internal/lockfile: shared_cgo.go on FreeBSD,
# sem_cgo.go and sysv_cgo.go on OpenBSD, NetBSD and DragonFly) and the C
# programs of the tests are compiled against each BSD's own headers and
# linked against its C library. Nothing runs on the target.
#
# Usage: scripts/bsd-cc-check.sh [goos/goarch ...]
#   default: freebsd/amd64 freebsd/arm64 netbsd/amd64 netbsd/arm64
#            openbsd/amd64 openbsd/arm64 dragonfly/amd64
#
# For each target, with CGO_ENABLED=1:
#   * the root module: go vet ./..., go build of every package and command,
#     go test -c of every package;
#   * the tests module: go vet ./... and go test -c of every package (the
#     cgo reference tests/cgolmdb, C LMDB 0.9.36 plus fixes, included);
#   * with RLMDB_DIR: the C oracles (tests/cmd/build-c-oracles: C 0.9 and
#     1.0 peers, tools, layout_dump, writeops) and the layout probe
#     (internal/lockfile/cprobe/layout_probe.c, which includes C's mdb.c);
#     afterwards TestLayoutsMatchCompiledC compares the lock-file layouts
#     internal/lockfile computes for each target with the probes', that
#     is, with C's on that target.
#
# Compilers: zig cc (0.17 or later) for FreeBSD, NetBSD and OpenBSD, whose
# C libraries zig ships (headers of FreeBSD 14, NetBSD 10.1, OpenBSD 7.8);
# clang with a sysroot for DragonFly, which zig has no libc for.
#
# Environment:
#   ZIG          zig (default zig)
#   CLANG        clang for sysroot targets (default clang; Apple's will do)
#   LLD          ld.lld for sysroot targets (default: from PATH, else
#                Homebrew's lld, which zig's formula installs)
#   BSD_SDK_DIR  where sysroots live (default $HOME/sdk); DragonFly uses
#                the newest $BSD_SDK_DIR/dragonfly-<version>-amd64, a
#                release's usr/include, usr/lib and lib (see below)
#   RLMDB_DIR    rlmdb checkout (its repository root) for the C oracles
#                and the layout probe; skipped without
#   OUT          work directory (default: a new temporary one, removed at
#                the end; set it to keep the binaries and objects)
#
# Two link details. Go's external linker on freebsd/arm64 insists on GNU
# ld.bfd (Go issue 35197), which zig is not: that target links with Go's
# internal linker (its C symbols are FreeBSD amd64's, which links
# externally). On OpenBSD Go passes the linker -nopie (and on arm64
# --no-execute-only), which zig does not accept: a small shim hands zig
# -no-pie instead and drops --no-execute-only. The binaries are for
# checking only; build on the BSD to run them.
#
# The DragonFly sysroot (about 42 MB): DragonFly publishes no base
# tarballs, so take the files from a release image, without keeping it,
# then make its absolute symlinks relative and drop static libraries but
# libgcc's, so that links use the shared C library:
#   v=6.4.2 d=$HOME/sdk/dragonfly-$v-amd64; mkdir -p "$d" && cd "$d" &&
#   curl -sS https://mirrors.nycbug.org/pub/DragonflyBSD/iso-images/dfly-x86_64-${v}_REL.iso.bz2 |
#     bzip2 -dc | bsdtar -xf - --exclude usr/lib/debug usr/include usr/lib lib &&
#   find usr lib -type l | while read -r l; do t=$(readlink "$l"); case $t in
#     /*) ln -sfn "$(dirname "$l" | sed 's|[^/][^/]*|..|g')$t" "$l" ;; esac; done &&
#   rm -rf usr/lib/gcc47 usr/lib/i18n usr/lib/gcc80/plugin usr/lib/gcc80/profile \
#     usr/lib/gcc80/libstdc++* usr/lib/gcc80/libgomp* usr/lib/gcc80/libgcov* usr/lib/gcc80/libssp* &&
#   find usr/lib -name '*.a' ! -path 'usr/lib/gcc80/*' -delete
set -eu
repo=$(cd "$(dirname "$0")/.." && pwd)
zig=${ZIG:-zig}
clang=${CLANG:-clang}
lld=${LLD:-}
if [ -z "$lld" ]; then
  lld=$(command -v ld.lld 2>/dev/null ||
    ls -d "$(brew --prefix 2>/dev/null)"/opt/lld*/bin/ld.lld 2>/dev/null | sort | tail -1 || true)
fi
sdk=${BSD_SDK_DIR:-$HOME/sdk}
if [ $# -eq 0 ]; then
  set -- freebsd/amd64 freebsd/arm64 netbsd/amd64 netbsd/arm64 \
    openbsd/amd64 openbsd/arm64 dragonfly/amd64
fi
if [ -n "${OUT:-}" ]; then
  work=$OUT
  mkdir -p "$work"
else
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
fi
mkdir -p "$work/bin" "$work/probe"

# The OpenBSD linker shim: zigcc-openbsd <zig target> <cc args...>.
cat > "$work/bin/zigcc-openbsd" <<EOF
#!/bin/sh
t=\$1
shift
n=\$#
while [ \$n -gt 0 ]; do
  a=\$1
  shift
  n=\$((n - 1))
  case \$a in
  -Wl,-nopie) set -- "\$@" -no-pie ;;
  -Wl,--no-execute-only) ;;
  *) set -- "\$@" "\$a" ;;
  esac
done
exec "$zig" cc -target "\$t" "\$@"
EOF
chmod +x "$work/bin/zigcc-openbsd"

failed=
fail() {
  echo "FAILED: $*" >&2
  failed="$failed
  $*"
}

# run <what> <command...>: runs the command, logging to the work
# directory, and records a failure.
run() {
  what=$1
  shift
  log="$work/log/$(echo "$what" | tr ' /' '__').log"
  mkdir -p "$work/log"
  if "$@" > "$log" 2>&1; then
    echo "ok      $what"
  else
    echo "FAIL    $what (log: $log)"
    sed 's/^/        /' "$log" | tail -20
    fail "$what"
  fi
}

# gotestc <module dir> <out dir> <ldflags>: go test -c of every package of
# the module, each into a directory of its own (two packages of the tests
# module are both called port).
gotestc() {
  (
    cd "$1" || exit 1
    for p in $(go list ./...); do
      go test -c -ldflags="$3" -o "$2/$(echo "$p" | tr / _)/" "$p" || exit 1
    done
  )
}

for target in "$@"; do
  goos=${target%/*}
  goarch=${target#*/}
  case $goarch in amd64) zarch=x86_64 ;; arm64) zarch=aarch64 ;; *) zarch=$goarch ;; esac
  ldflags=
  case $goos in
  freebsd | netbsd)
    cc="$zig cc -target $zarch-$goos"
    [ "$target" = freebsd/arm64 ] && ldflags=-linkmode=internal
    ;;
  openbsd)
    cc="$work/bin/zigcc-openbsd $zarch-openbsd"
    ;;
  dragonfly)
    root=$(ls -d "$sdk"/dragonfly-*-"$goarch" 2>/dev/null | sort | tail -1)
    if [ -z "$root" ]; then
      echo "skip    $target: no sysroot $sdk/dragonfly-*-$goarch"
      continue
    fi
    if [ -z "$lld" ]; then
      fail "$target: no ld.lld (set LLD)"
      continue
    fi
    # -Qunused-arguments: the linker flags must not make compiles fail
    # (runtime/cgo builds with -Werror).
    cc="$clang --target=$zarch-unknown-dragonfly --sysroot=$root -Qunused-arguments -fuse-ld=lld --ld-path=$lld"
    ;;
  *)
    fail "$target: unknown target"
    continue
    ;;
  esac
  out="$work/$goos-$goarch"
  export CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" CC="$cc"
  run "$target: go vet" sh -c "cd '$repo' && go vet ./..."
  run "$target: go build" sh -c "cd '$repo' && go build -ldflags='$ldflags' -o '$out/cmd/' ./..."
  run "$target: go test -c" gotestc "$repo" "$out/test" "$ldflags"
  run "$target: tests module: go vet" sh -c "cd '$repo/tests' && go vet ./..."
  run "$target: tests module: go test -c" gotestc "$repo/tests" "$out/tests-test" "$ldflags"
  if [ -n "${RLMDB_DIR:-}" ]; then
    # build-c-oracles itself runs here: a host build, without cgo.
    run "$target: C oracles" sh -c "cd '$repo/tests' && env -u GOOS -u GOARCH -u CC CGO_ENABLED=0 go run ./cmd/build-c-oracles -os '$goos' -cc '$cc' '$RLMDB_DIR' '$out/oracles'"
    for v in 0.9 1.0; do
      run "$target: layout probe $v" sh -c "$cc -c -w -I'$RLMDB_DIR/tests/lmdb-c/$v' -o '$work/probe/$goos-$goarch-$(echo $v | tr -d .).o' '$repo/internal/lockfile/cprobe/layout_probe.c'"
    done
  fi
  unset CGO_ENABLED GOOS GOARCH CC
done

if [ -n "${RLMDB_DIR:-}" ] && ls "$work"/probe/*.o > /dev/null 2>&1; then
  run "layouts against the probes (host)" sh -c "cd '$repo' && LMDB_TEST_LAYOUT_OBJS='$work/probe' go test -count=1 -run TestLayoutsMatchCompiledC -v ./internal/lockfile"
  grep "mti_format" "$work/log/layouts_against_the_probes_(host).log" | sed 's/^ *layout_probe_test.go:[0-9]*: /        /' || true
fi

if [ -n "$failed" ]; then
  echo "failed:$failed" >&2
  exit 1
fi
echo "all checks passed"
