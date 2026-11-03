#!/bin/sh
#
# Runs the tests against rlmdb (https://github.com/wojas/rlmdb), a rewrite of
# LMDB 0.9 in Rust whose C API is a drop-in liblmdb, instead of the bundled
# LMDB sources: builds rlmdb's liblmdb and runs `go test -tags rlmdb`
# dynamically linked against it.
#
# Usage: RLMDB=<rlmdb checkout> ./rlmdb-test.sh [go test args...]
#        RLMDB_LIB_DIR=<dir with liblmdb.so/.dylib> ./rlmdb-test.sh [...]
#   default go test args: ./...
#
# RLMDB: the library is built with `cargo build --release -p rlmdb-capi`.
# RLMDB_LIB_DIR: use an already built library from that directory instead.
#

set -e

usage() {
    echo "USAGE: RLMDB=<rlmdb checkout> $0 [go test args...]"
    echo "       RLMDB_LIB_DIR=<directory with liblmdb> $0 [go test args...]"
    exit 1
}

if [ -z "$RLMDB_LIB_DIR" ]; then
    [ -n "$RLMDB" ] || usage
    (cd "$RLMDB" && cargo build --release -p rlmdb-capi)
    target_dir=$(cd "$RLMDB" && cargo metadata --format-version 1 --no-deps |
        sed -n 's/.*"target_directory":"\([^"]*\)".*/\1/p')
    [ -n "$target_dir" ] || { echo "ERROR: cannot find cargo's target directory"; exit 1; }
    RLMDB_LIB_DIR="$target_dir/release"
fi
RLMDB_LIB_DIR=$(cd "$RLMDB_LIB_DIR" && pwd)

case "$(uname -s)" in
    Darwin) lib=liblmdb.dylib ;;
    *) lib=liblmdb.so ;;
esac
[ -f "$RLMDB_LIB_DIR/$lib" ] || { echo "ERROR: no $lib in $RLMDB_LIB_DIR"; exit 1; }
echo "Linking against $RLMDB_LIB_DIR/$lib"

# The test binaries must find the library at run time too: on Linux through
# an rpath; on macOS its install name usually is its absolute path already,
# and DYLD_LIBRARY_PATH covers the rest (an rpath there makes ld warn about
# duplicates, as cgo passes CGO_LDFLAGS more than once).
CGO_LDFLAGS="-L$RLMDB_LIB_DIR${CGO_LDFLAGS:+ $CGO_LDFLAGS}"
case "$(uname -s)" in
    Darwin) DYLD_LIBRARY_PATH="$RLMDB_LIB_DIR${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}"
        export DYLD_LIBRARY_PATH ;;
    *) CGO_LDFLAGS="$CGO_LDFLAGS -Wl,-rpath,$RLMDB_LIB_DIR" ;;
esac
export CGO_LDFLAGS

[ $# -gt 0 ] || set -- ./...
# -count=1: the test cache does not know about the shared library, so a
# rebuilt rlmdb would not rerun cached tests.
exec go test -count=1 -tags rlmdb "$@"
