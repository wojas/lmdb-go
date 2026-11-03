#!/bin/sh
#
# Script to update our copy of the LMDB library and headers
# Releases can be found here: https://git.openldap.org/openldap/openldap/-/tags?sort=updated_desc&search=LMDB_
#

get_define() {
    grep "^#define $1" lmdb/lmdb.h  | head -1 | awk '{print $3}'
}

get_version() {
    echo "$(get_define MDB_VERSION_MAJOR).$(get_define MDB_VERSION_MINOR).$(get_define MDB_VERSION_PATCH)"
}

cur_version="$(get_version)"
echo "Current LMDB version: $cur_version"
echo

version="$1"

if [ -z "$version" ]; then
    echo "USAGE: $0 <desired-version>"
    echo "Check https://git.openldap.org/openldap/openldap/-/tags?sort=updated_desc&search=LMDB_ for available versions"
    exit 1
fi

set -ex

tmp_dir=$(mktemp -d -t lmdb-update-XXXXX)
echo "Temp dir: $tmp_dir"

curl -L "https://git.openldap.org/openldap/openldap/-/archive/LMDB_${version}/openldap-LMDB_${version}.tar.gz" | tar -C "$tmp_dir" -xvz
# The build constraint leaves the bundled sources out under -tags rlmdb
for f in mdb.c midl.c; do
    printf '//go:build !rlmdb\n\n' > "lmdb/$f"
    cat "$tmp_dir/openldap-LMDB_${version}/libraries/liblmdb/$f" >> "lmdb/$f"
done
cp "$tmp_dir/openldap-LMDB_${version}/libraries/liblmdb/lmdb.h" lmdb/lmdb.h
cp "$tmp_dir/openldap-LMDB_${version}/libraries/liblmdb/midl.h" lmdb/midl.h
cp "$tmp_dir/openldap-LMDB_${version}/libraries/liblmdb/CHANGES" CHANGES.lmdb.txt

if [ ! -z "$tmp_dir" ]; then
    echo "Removing temp dir: $tmp_dir"
    rm -rf "$tmp_dir"
fi

set +ex

for f in mdb.c midl.c; do
    head -1 "lmdb/$f" | grep -q '^//go:build !rlmdb$' || { echo "ERROR: build constraint missing from lmdb/$f"; exit 1; }
done

echo
new_version="$(get_version)"
echo "New LMDB version: $new_version"
echo
echo "NOTE: Do not forget to include the upstream changelog from $cur_version to $new_version from"
echo "      CHANGES.lmdb.txt in our CHANGES.md, and do not forget to test!"
