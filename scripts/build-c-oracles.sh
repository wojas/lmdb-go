#!/bin/sh
# Build the C oracles of rlmdb's interop crate for the libc of the machine
# it runs on (glibc or musl): the C 0.9 and C 1.0 peers (cpeer, cpeer10),
# the C tools (mdb_stat, mdb_dump, mdb_load, mdb_copy and their *10
# versions) and layout_dump / layout_dump10, from the C sources rlmdb
# vendors (tests/lmdb-c/0.9: LMDB 0.9.37; tests/lmdb-c/1.0: LMDB 1.0.3;
# see tests/lmdb-c/README.md there). On macOS, rlmdb's
# interop build provides the same binaries; this script is for the Linux
# containers, where those macOS binaries do not run.
#
# Usage: scripts/build-c-oracles.sh [out-dir]     (default /tmp/c-oracles)
# Run it inside the container, e.g.
#   RLMDB_DIR=<rlmdb checkout> scripts/linux-test.sh sh -c \
#     'scripts/build-c-oracles.sh && LMDB_ORACLE_BIN=/tmp/c-oracles/bin ...'
# (in Alpine first: apk add -q build-base).
#
# Environment:
#   RLMDB_SRC   the rlmdb checkout (its repository root), with the vendored
#               C sources (tests/lmdb-c/0.9, tests/lmdb-c/1.0) and the C
#               peer (interop/c); default /rlmdb, where linux-test.sh
#               mounts RLMDB_DIR
#   LMDB10_DIR  another directory with the LMDB 1.0 sources, instead of
#               tests/lmdb-c/1.0 (as rlmdb's interop/build.rs takes it)
#   CC          the C compiler (default cc)
#
# The flags are those of interop/build.rs (the upstream Makefile's). Only
# out-dir is written; keep it on the container's tmpfs.
set -eu
out=${1:-/tmp/c-oracles}
src=${RLMDB_SRC:-/rlmdb}
cc=${CC:-cc}
cflags="-pthread -O2 -g -W -Wall -Wno-unused-parameter"
lmdb09=$src/tests/lmdb-c/0.9
lmdb10=${LMDB10_DIR:-$src/tests/lmdb-c/1.0}
peer=$src/interop/c
peer_srcs="cpeer.c cpeer_data.c cpeer_loops.c cpeer_misc.c cpeer_pagewalk.c dataset.c"
tools="mdb_stat mdb_dump mdb_load mdb_copy"

for f in "$lmdb09/mdb.c" "$lmdb10/mdb.c" "$peer/cpeer.c"; do
  [ -f "$f" ] || { echo "build-c-oracles: $f not found (set RLMDB_SRC)" >&2; exit 2; }
done
mkdir -p "$out/bin" "$out/obj09" "$out/obj10"

# compile <objdir> <include dir> <source>...
compile() {
  objdir=$1 inc=$2
  shift 2
  for s in "$@"; do
    o=$objdir/$(basename "$s" .c).o
    $cc $cflags -I"$inc" -c "$s" -o "$o"
  done
}

# C 0.9 (the library objects once, linked into every program).
compile "$out/obj09" "$lmdb09" "$lmdb09/mdb.c" "$lmdb09/midl.c"
lib09="$out/obj09/mdb.o $out/obj09/midl.o"
for t in $tools; do
  compile "$out/obj09" "$lmdb09" "$lmdb09/$t.c"
  $cc -pthread -o "$out/bin/$t" "$out/obj09/$t.o" $lib09
done
mkdir -p "$out/obj09/peer"
for s in $peer_srcs; do compile "$out/obj09/peer" "$lmdb09" "$peer/$s"; done
# shellcheck disable=SC2046
$cc -pthread -o "$out/bin/cpeer" $(for s in $peer_srcs; do echo "$out/obj09/peer/${s%.c}.o"; done) $lib09
# layout_dump includes mdb.c itself.
$cc $cflags -I"$lmdb09" -o "$out/bin/layout_dump" "$peer/layout_dump.c" "$out/obj09/midl.o"

# C 1.0 (its liblmdb also holds module.o, which needs -ldl).
compile "$out/obj10" "$lmdb10" "$lmdb10/mdb.c" "$lmdb10/midl.c" "$lmdb10/module.c"
lib10="$out/obj10/mdb.o $out/obj10/midl.o $out/obj10/module.o"
for t in $tools; do
  compile "$out/obj10" "$lmdb10" "$lmdb10/$t.c"
  $cc -pthread -o "$out/bin/${t}10" "$out/obj10/$t.o" $lib10 -ldl
done
mkdir -p "$out/obj10/peer"
for s in $peer_srcs; do compile "$out/obj10/peer" "$lmdb10" "$peer/$s"; done
# shellcheck disable=SC2046
$cc -pthread -o "$out/bin/cpeer10" $(for s in $peer_srcs; do echo "$out/obj10/peer/${s%.c}.o"; done) $lib10 -ldl
$cc $cflags -I"$lmdb10" -o "$out/bin/layout_dump10" "$peer/layout_dump.c" "$out/obj10/midl.o"

echo "build-c-oracles: built in $out/bin:" $(ls "$out/bin")
