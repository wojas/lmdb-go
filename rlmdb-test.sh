#!/bin/sh
#
# Runs the tests against rlmdb (https://github.com/wojas/rlmdb), a rewrite of
# LMDB 0.9 in Rust whose C API has liblmdb's symbols and ABI, instead of the
# bundled LMDB sources: builds rlmdb's librlmdb and runs `go test -tags rlmdb`
# dynamically linked against it.
#
# Usage: RLMDB=<rlmdb checkout> ./rlmdb-test.sh [go test args...]
#        RLMDB_LIB_DIR=<dir with librlmdb.so/.dylib> ./rlmdb-test.sh [...]
#        ./rlmdb-test.sh [...]
#   default go test args: ./...
#
# RLMDB: the library is built with `cargo build --release -p librlmdb`.
# RLMDB_LIB_DIR: use an already built library from that directory instead.
# Neither: use the installed rlmdb that pkg-config knows (rlmdb.pc).
#

set -e

usage() {
    echo "USAGE: RLMDB=<rlmdb checkout> $0 [go test args...]"
    echo "       RLMDB_LIB_DIR=<directory with librlmdb> $0 [go test args...]"
    echo "       $0 [go test args...]  (rlmdb installed, found by pkg-config)"
    exit 1
}

if [ -z "$RLMDB_LIB_DIR" ]; then
    if [ -n "$RLMDB" ]; then
        (cd "$RLMDB" && cargo build --release -p librlmdb)
        target_dir=$(cd "$RLMDB" && cargo metadata --format-version 1 --no-deps |
            sed -n 's/.*"target_directory":"\([^"]*\)".*/\1/p')
        [ -n "$target_dir" ] || { echo "ERROR: cannot find cargo's target directory"; exit 1; }
        RLMDB_LIB_DIR="$target_dir/release"
    elif pkg-config --exists rlmdb 2>/dev/null; then
        RLMDB_LIB_DIR=$(pkg-config --variable=libdir rlmdb)
    else
        usage
    fi
fi
RLMDB_LIB_DIR=$(cd "$RLMDB_LIB_DIR" && pwd)

case "$(uname -s)" in
    Darwin) lib=librlmdb.dylib ;;
    *) lib=librlmdb.so ;;
esac
[ -f "$RLMDB_LIB_DIR/$lib" ] || { echo "ERROR: no $lib in $RLMDB_LIB_DIR"; exit 1; }
echo "Linking against $RLMDB_LIB_DIR/$lib"

# The test binaries must find the library at run time too. Its name for the
# dynamic linker is librlmdb.so (SONAME) or @rpath/librlmdb.dylib (macOS
# install name, replaced with the installed path by rlmdb's
# scripts/install-libs.sh). On Linux an rpath finds it; on macOS
# DYLD_LIBRARY_PATH does (an rpath works there too, but makes ld warn about
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
