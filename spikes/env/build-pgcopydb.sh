#!/bin/bash
# build-pgcopydb.sh DIR: builds pgcopydb as deploy/install.sh does (the v0.18
# tag plus deploy/pgcopydb/*.patch) in DIR and prints the binary's path.
# Point the tests at it: UPWELL_IT_PGCOPYDB (integration) and UPWELL_PGCOPYDB
# (dev/e2e.sh); or install it as /usr/local/bin/pgcopydb.
set -euo pipefail
DIR=${1:?usage: build-pgcopydb.sh DIR}
HERE=$(cd "$(dirname "$0")/../.." && pwd)
BUILD=$(sed -n 's/^PGCOPYDB_BUILD="\(.*\)"/\1/p' "$HERE/deploy/install.sh")
TAG=$(sed -n 's/^PGCOPYDB_TAG="\(.*\)"/\1/p' "$HERE/deploy/install.sh")
rm -rf "$DIR"
git clone -q --depth 1 --branch "$TAG" "${PGCOPYDB_REPO:-https://github.com/dimitri/pgcopydb}" "$DIR"
for p in "$HERE"/deploy/pgcopydb/*.patch; do git -C "$DIR" apply "$p"; done
VERSION="$BUILD" make -C "$DIR" -j"$(nproc)" >"$DIR/build.log" 2>&1 || { tail -20 "$DIR/build.log"; exit 1; }
"$DIR/src/bin/pgcopydb/pgcopydb" --version 2>/dev/null | grep -q "version $BUILD" || { echo "built binary does not report $BUILD" >&2; exit 1; }
echo "$DIR/src/bin/pgcopydb/pgcopydb"
