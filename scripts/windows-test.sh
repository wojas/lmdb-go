#!/bin/sh
# Run a command in a copy of this checkout on a Windows machine, over ssh to
# its OpenSSH server (cmd.exe as the remote shell). Modelled on rlmdb's
# scripts/windows-test.sh.
#
# Usage: scripts/windows-test.sh [command...]
#   default: go test ./...
# Environment:
#   WINDOWS_HOST     ssh destination (required), e.g. an alias from ~/.ssh/config
#   WINDOWS_PORT     ssh port (default: what the ssh config says)
#   WINDOWS_WORKDIR  work directory on the Windows side, relative to the
#                    remote home or absolute, with / or \ (default
#                    lmdbgo-work; e.g. T:/lmdbgo-work for another drive)
#   WINDOWS_ENV      script in the work directory that sets up the toolchain
#                    for the command (default env.cmd), see below
#   WINDOWS_FULL=1   send every file instead of the changes since last time
#   RLMDB_DIR        an rlmdb checkout: its C sources (tests/lmdb-c,
#                    tests/compat, interop/c, rlmdb/tests/c at its HEAD) are
#                    sent too, into <workdir>\rlmdb, for the C oracles
#   WINDOWS_ORACLES=1  build the C oracles there first (cmd/build-c-oracles
#                    of the tests module, with the gcc of WINDOWS_ENV) into
#                    <workdir>\oracles; needs the rlmdb sources
#
# The checkout is sent as it is on disk (tracked files and new files that
# are not ignored, through a temporary index; the real index is not
# touched) with `git archive`, as a zip, and unpacked with the `tar` that
# Windows 10 ships, into <workdir>\src. Only files that changed since the
# previous run (whose tree is remembered in this checkout's git directory)
# are sent; files deleted here are deleted there. The files get the time
# of unpacking (tar -m: exFAT drives refuse some of the zip's times). The rlmdb sources are
# sent again only when rlmdb's HEAD changed. No git is needed on Windows,
# and no credentials go there.
#
# <workdir>\<WINDOWS_ENV> is run (`call`) before the command, from
# <workdir>. It is not part of this repository: it names the machine's
# toolchain, for example (with a Go zip unpacked into <workdir>\go and
# MinGW-w64, e.g. MSYS2's mingw64, under the work directory), keeping
# everything Go writes (module and build caches, telemetry counters, the
# tests' temporary files) in the work directory:
#
#   set GOPATH=%CD%\gopath
#   set GOCACHE=%CD%\gocache
#   set GOENV=off
#   set GOTOOLCHAIN=local
#   set APPDATA=%CD%\appdata
#   if not exist tmp mkdir tmp
#   set TMP=%CD%\tmp
#   set TEMP=%CD%\tmp
#   set CGO_ENABLED=1
#   set CC=gcc
#   set PATH=%CD%\go\bin;%CD%\mingw64\bin;%PATH%
#
# (The first run downloads golang.org/x/sys through the Go module proxy
# into %GOPATH%\pkg\mod.) Then RLMDB_SRC is set to <workdir>\rlmdb and, if
# <workdir>\oracles holds the oracles, LMDB_ORACLE_BIN and RLMDB_BIN to it
# (tests/lockfile, tests/engine), and the command runs in <workdir>\src,
# under cmd.exe. Examples:
#
#   scripts/windows-test.sh 'go vet ./... && go test ./...'
#   scripts/windows-test.sh 'set CGO_ENABLED=0&& go test ./...'
#   RLMDB_DIR=<rlmdb checkout> WINDOWS_ORACLES=1 scripts/windows-test.sh \
#     'cd tests && go test ./...'
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
host=${WINDOWS_HOST:?set WINDOWS_HOST to the ssh destination of the Windows machine}
workdir=${WINDOWS_WORKDIR:-lmdbgo-work}
envfile=${WINDOWS_ENV:-env.cmd}
ssh_opts=
[ -n "${WINDOWS_PORT:-}" ] && ssh_opts="-o Port=$WINDOWS_PORT"
# The work directory as cmd.exe wants it (backslashes) and as scp does.
wd_cmd=$(printf '%s' "$workdir" | tr / '\\')
wd_scp=$(printf '%s' "$workdir" | tr '\\' /)
if [ $# -eq 0 ]; then
	set -- go test ./...
fi

cd "$here"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/windows-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# The tree of the checkout as it is on disk.
cp "$(git rev-parse --git-path index)" "$tmp/index" 2>/dev/null || :
GIT_INDEX_FILE="$tmp/index" git add -A .
tree=$(GIT_INDEX_FILE="$tmp/index" git write-tree)

# What the other side has: the trees sent last time to this host and work
# directory, if any (this checkout's, and rlmdb's sources).
key=$(printf '%s' "$host:$workdir" | git hash-object --stdin)
state=$(git rev-parse --git-path "windows-test-$key")
rstate=$(git rev-parse --git-path "windows-test-rlmdb-$key")
prev=
if [ -z "${WINDOWS_FULL:-}" ] && [ -f "$state" ]; then
	prev=$(cat "$state")
	git cat-file -e "$prev^{tree}" 2>/dev/null || prev=
fi

if [ -n "$prev" ]; then
	git diff --name-only --no-renames --diff-filter=d "$prev" "$tree" >"$tmp/changed"
	git diff --name-only --no-renames --diff-filter=D "$prev" "$tree" >"$tmp/deleted"
else
	git ls-tree -r --name-only "$tree" >"$tmp/changed"
	: >"$tmp/deleted"
fi
echo "windows-test: $(wc -l <"$tmp/changed" | tr -d ' ') changed, $(wc -l <"$tmp/deleted" | tr -d ' ') deleted file(s)"

# rlmdb's C sources, when asked for and not there yet.
rtree=
if [ -n "${RLMDB_DIR:-}" ]; then
	rpaths="tests/lmdb-c tests/compat interop/c rlmdb/tests/c"
	# shellcheck disable=SC2086 # fixed list of paths without spaces
	rtree=$(git -C "$RLMDB_DIR" rev-parse "HEAD:" 2>/dev/null) ||
		{ echo "windows-test: $RLMDB_DIR is not an rlmdb checkout" >&2; exit 2; }
	if [ -n "${WINDOWS_FULL:-}" ] || [ ! -f "$rstate" ] || [ "$(cat "$rstate")" != "$rtree" ]; then
		# shellcheck disable=SC2086
		git -C "$RLMDB_DIR" archive --format=zip -o "$tmp/rlmdb.zip" HEAD $rpaths
		echo "windows-test: sending rlmdb's C sources ($(git -C "$RLMDB_DIR" rev-parse --short HEAD))"
	fi
fi

# The remote script: delete, unpack, set up, (build the oracles,) run.
{
	printf '@echo off\r\n'
	printf 'cd /d "%%~dp0"\r\n'
	if [ -z "$prev" ]; then
		printf 'if exist src rmdir /s /q src\r\n'
	fi
	printf 'if not exist src mkdir src\r\n'
	while IFS= read -r f; do
		printf 'if exist "src\\%s" del /f /q "src\\%s"\r\n' "$(echo "$f" | tr / '\\')" "$(echo "$f" | tr / '\\')"
	done <"$tmp/deleted"
	if [ -s "$tmp/changed" ]; then
		printf 'tar -xmf windows-test.zip -C src || exit /b 125\r\n'
	fi
	if [ -f "$tmp/rlmdb.zip" ]; then
		printf 'if exist rlmdb rmdir /s /q rlmdb\r\n'
		printf 'mkdir rlmdb\r\n'
		printf 'tar -xmf rlmdb.zip -C rlmdb || exit /b 125\r\n'
	fi
	printf 'call "%s" || exit /b 125\r\n' "$envfile"
	printf 'set "RLMDB_SRC=%%~dp0rlmdb"\r\n'
	printf 'set "LMDBGO_ORACLES=%%~dp0oracles"\r\n'
	if [ -n "${WINDOWS_ORACLES:-}" ]; then
		printf 'pushd src\\tests\r\n'
		printf 'go run ./cmd/build-c-oracles "%%RLMDB_SRC%%" "%%LMDBGO_ORACLES%%" || exit /b 1\r\n'
		printf 'popd\r\n'
	fi
	printf 'if exist "%%LMDBGO_ORACLES%%\\cpeer10.exe" set "LMDB_ORACLE_BIN=%%LMDBGO_ORACLES%%"\r\n'
	printf 'if exist "%%LMDBGO_ORACLES%%\\writeops.exe" set "RLMDB_BIN=%%LMDBGO_ORACLES%%"\r\n'
	printf 'cd src\r\n'
	printf '%s\r\n' "$*"
} >"$tmp/windows-test.cmd"

files="$tmp/windows-test.cmd"
if [ -s "$tmp/changed" ]; then
	# shellcheck disable=SC2046 # one path per line, no spaces in this repository
	git archive --format=zip -o "$tmp/windows-test.zip" "$tree" $(cat "$tmp/changed")
	files="$files $tmp/windows-test.zip"
fi
[ -f "$tmp/rlmdb.zip" ] && files="$files $tmp/rlmdb.zip"
# shellcheck disable=SC2086
ssh $ssh_opts "$host" "if not exist \"$wd_cmd\" mkdir \"$wd_cmd\""
# shellcheck disable=SC2086
scp -q $ssh_opts $files "$host:$wd_scp/"
# The other side has these trees once the script got past unpacking (exit
# code 125 means it did not).
rm -f "$state"
[ -f "$tmp/rlmdb.zip" ] && rm -f "$rstate"
rc=0
# shellcheck disable=SC2086
ssh $ssh_opts "$host" "\"$wd_cmd\\windows-test.cmd\"" || rc=$?
if [ "$rc" -ne 125 ]; then
	printf '%s\n' "$tree" >"$state"
	[ -n "$rtree" ] && printf '%s\n' "$rtree" >"$rstate"
fi
exit "$rc"
