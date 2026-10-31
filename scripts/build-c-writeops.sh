#!/bin/sh
# Build the C oracles of tests/engine for this platform: rlmdb's writeops
# (the write-differential script runner) and cpeer (the interop peer, for
# its check-freelist and fill), and the C tools mdb_dump, mdb_copy and
# mdb_stat, each against C LMDB 0.9 and C LMDB 1.0 (names *10), as rlmdb's
# interop/build.rs builds them.
#
# Usage: scripts/build-c-writeops.sh <rlmdb-root> <lmdb10-liblmdb-dir> <out-dir>
#   <rlmdb-root>          the rlmdb checkout (libraries/liblmdb is C 0.9,
#                         rust/ holds writeops.c and the peer sources)
#   <lmdb10-liblmdb-dir>  libraries/liblmdb of the pinned LMDB 1.0 commit
#                         (git archive/show of 4865523, see rlmdb's
#                         interop/build.rs)
#   <out-dir>             where the binaries go (e.g. /tmp/coracles in the
#                         container; never inside a checkout)
#
# In the Linux container (scripts/linux-test.sh), for example:
#   git -C <lmdb repo> archive 4865523ff0e1514d26a675d9cfd1d3a4b2b6c4c3 \
#     libraries/liblmdb | RLMDB_DIR=<rlmdb root> scripts/linux-test.sh sh -c \
#     'mkdir -p /tmp/l10 && tar -x -C /tmp/l10 &&
#      scripts/build-c-writeops.sh /rlmdb /tmp/l10/libraries/liblmdb /tmp/coracles &&
#      cd tests && RLMDB_BIN=/tmp/coracles go test ./engine'
set -eu
r=$1
l10=$2
out=$3
l09=$r/libraries/liblmdb
peer=$r/rust/interop/c
writeops=$r/rust/rlmdb/tests/c/writeops.c
cc=${CC:-cc}
cflags="-O2 -g -pthread -W -Wall -Wno-unused-parameter"
mkdir -p "$out"
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
echo "built in $out"
