#!/bin/sh
# Build the C oracles of tests/engine for this platform: rlmdb's writeops
# (the write-differential script runner) and cpeer (the interop peer, for
# its check-freelist and fill), and the C tools mdb_dump, mdb_copy and
# mdb_stat, each against C LMDB 0.9 and C LMDB 1.0 (names *10), as rlmdb's
# interop/build.rs builds them, plus writeops10_ndebug (writeops against a
# 1.0 library built with -DNDEBUG, for the one test on which C 1.0's
# assertions abort: rlmdb KNOWN_C_BUGS.md #37). The C sources are the ones
# rlmdb vendors: tests/lmdb-c/0.9 (LMDB 0.9.37) and tests/lmdb-c/1.0 (LMDB
# 1.0.3; see tests/lmdb-c/README.md there).
#
# Usage: scripts/build-c-writeops.sh <rlmdb-root> <out-dir>
#   <rlmdb-root>  the rlmdb checkout (its repository root: tests/lmdb-c,
#                 rlmdb/tests/c/writeops.c, interop/c)
#   <out-dir>     where the binaries go (e.g. /tmp/coracles in the
#                 container; never inside a checkout)
# Environment:
#   LMDB10_DIR    another directory with the LMDB 1.0 sources, instead of
#                 tests/lmdb-c/1.0 (as rlmdb's interop/build.rs takes it)
#   CC            the C compiler (default cc; e.g. "cc -arch x86_64" on
#                 macOS for oracles of a GOARCH=amd64 test binary)
#
# In the Linux container (scripts/linux-test.sh), for example:
#   RLMDB_DIR=<rlmdb root> scripts/linux-test.sh sh -c \
#     'scripts/build-c-writeops.sh /rlmdb /tmp/coracles &&
#      cd tests && RLMDB_BIN=/tmp/coracles go test ./engine'
# (in Alpine first: apk add -q build-base). On macOS, for a GOARCH=amd64
# test binary under Rosetta:
#   CC="cc -arch x86_64" scripts/build-c-writeops.sh <rlmdb root> /tmp/coracles-x86 &&
#   (cd tests && GOARCH=amd64 RLMDB_BIN=/tmp/coracles-x86 go test -timeout 30m ./engine)
# (emulated, the tests take 7 to 9 minutes: hence the timeout).
set -eu
r=$1
out=$2
l09=$r/tests/lmdb-c/0.9
l10=${LMDB10_DIR:-$r/tests/lmdb-c/1.0}
peer=$r/interop/c
writeops=$r/rlmdb/tests/c/writeops.c
cc=${CC:-cc}
cflags="-O2 -g -pthread -W -Wall -Wno-unused-parameter"
for f in "$l09/mdb.c" "$l10/mdb.c" "$peer/cpeer.c" "$writeops"; do
  [ -f "$f" ] || { echo "build-c-writeops: $f not found" >&2; exit 2; }
done
mkdir -p "$out"
obj=$(mktemp -d)
trap 'rm -rf "$obj"' EXIT
peer_srcs="$peer/cpeer.c $peer/cpeer_data.c $peer/cpeer_loops.c $peer/cpeer_misc.c $peer/cpeer_pagewalk.c $peer/dataset.c"

# LMDB 0.9
lib09="$l09/mdb.c $l09/midl.c"
# shellcheck disable=SC2086
$cc $cflags -I"$l09" -o "$out/writeops" "$writeops" $lib09
# shellcheck disable=SC2086
$cc $cflags -I"$l09" -I"$peer" -o "$out/cpeer" $peer_srcs $lib09
for t in mdb_dump mdb_copy mdb_stat; do
  # shellcheck disable=SC2086
  $cc $cflags -I"$l09" -o "$out/$t" "$l09/$t.c" $lib09
done

# LMDB 1.0 (its liblmdb also holds module.o, which needs -ldl)
lib10="$l10/mdb.c $l10/midl.c $l10/module.c"
# shellcheck disable=SC2086
$cc $cflags -I"$l10" -o "$out/writeops10" "$writeops" $lib10 -ldl
# shellcheck disable=SC2086
$cc $cflags -I"$l10" -I"$peer" -o "$out/cpeer10" $peer_srcs $lib10 -ldl
for t in mdb_dump mdb_copy mdb_stat; do
  # shellcheck disable=SC2086
  $cc $cflags -I"$l10" -o "$out/${t}10" "$l10/$t.c" $lib10 -ldl
done
# writeops10_ndebug: only mdb.c without assertions, as interop/build.rs.
$cc $cflags -DNDEBUG -I"$l10" -c "$l10/mdb.c" -o "$obj/mdb.o"
$cc $cflags -I"$l10" -o "$out/writeops10_ndebug" "$writeops" "$obj/mdb.o" "$l10/midl.c" "$l10/module.c" -ldl
echo "built in $out"
