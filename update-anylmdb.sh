#!/bin/sh
#
# Refresh the anylmdb C sources vendored in lmdb/ from an anylmdb checkout
# (https://github.com/wojas/anylmdb). anylmdb bundles both LMDB 0.9 and 1.0
# and dispatches per environment based on the database format, so this
# replaces the old single-version LMDB vendoring (update-lmdb.sh).
set -eu

src="${1:-}"
if [ -z "$src" ] || [ ! -f "$src/anylmdb.h" ]; then
    echo "USAGE: $0 <path-to-anylmdb-checkout>"
    exit 1
fi

cd "$(dirname "$0")"

for f in $(make -C "$src" -s vendor-list); do
    cp "$src/$f" lmdb/
done

echo "Vendored anylmdb $(git -C "$src" describe --always --dirty 2>/dev/null || echo '(unknown revision)') into lmdb/"
echo "NOTE: run 'go test ./...' and update CHANGES.md."
