#!/bin/sh
# Run a command for this repository inside a Linux (Debian, glibc)
# container: the Linux lock-file layout (process-shared robust pthread
# mutexes in the lock file) and 4 KiB pages differ from macOS.
#
# Usage: scripts/linux-test.sh [command args...]
#   default: go test ./...
# Environment:
#   LINUX_TEST_IMAGE     image (default golang:1.25-bookworm, has gcc for
#                        the cgo test oracles)
#   LINUX_TEST_PLATFORM  e.g. linux/amd64 to run x86-64 under emulation
#                        (default: the VM's native architecture)
#   LINUX_TEST_TMPFS     size of the tmpfs at /tmp (default 4g; 0 disables)
#   RLMDB_DIR            rlmdb checkout (the lmdb repository, holding rust/)
#                        to mount read-only at /rlmdb, with its git
#                        directory (optional; for the interop peers and C
#                        tools: scripts/build-c-oracles.sh)
#
# /tmp, where tests and benchmarks create environments, is a tmpfs: the
# alternatives in the VM are its virtual disk and the virtiofs host mount,
# neither of which is a local file system. fsync timings mean nothing here.
set -eu
repo=$(cd "$(dirname "$0")/.." && pwd)
image=${LINUX_TEST_IMAGE:-golang:1.25-bookworm}
tmpfs=${LINUX_TEST_TMPFS-4g}
case $tmpfs in ""|0) tmpfs_mount= ;; *) tmpfs_mount="--tmpfs /tmp:exec,size=$tmpfs" ;; esac
platform=${LINUX_TEST_PLATFORM:+--platform $LINUX_TEST_PLATFORM}
# No Docker Desktop credential helper here: scratch config, Lima socket.
export DOCKER_CONFIG=${DOCKER_CONFIG:-/tmp/lmdb-go-dockercfg}
mkdir -p "$DOCKER_CONFIG"
[ -f "$DOCKER_CONFIG/config.json" ] || echo '{}' > "$DOCKER_CONFIG/config.json"
if [ -z "${DOCKER_HOST:-}" ] && [ -S "$HOME/.lima/docker/sock/docker.sock" ]; then
  export DOCKER_HOST="unix://$HOME/.lima/docker/sock/docker.sock"
fi
if [ $# -eq 0 ]; then set -- go test ./...; fi
arch=$(printf '%s' "${LINUX_TEST_PLATFORM:-native}" | tr '/' '-')
cache_vol="lmdb-go-cache-$arch-$(printf '%s' "$repo" | cksum | cut -d' ' -f1)"
# A linked worktree's .git names the main repository's git directory by
# absolute path; mount it read-only at the same path.
git_mount=
if git_common=$(cd "$repo" && git rev-parse --git-common-dir 2>/dev/null); then
  case $git_common in /*) ;; *) git_common=$repo/$git_common ;; esac
  git_common=$(cd "$git_common" && pwd)
  case $git_common/ in "$repo"/*) ;; *) git_mount=$git_common ;; esac
fi
# RLMDB_DIR is a linked worktree too: mount its repository's git directory
# the same way, so that scripts/build-c-oracles.sh can `git show` the LMDB
# 1.0 sources there (unless it is the one mounted already).
rlmdb_git_mount=
if [ -n "${RLMDB_DIR:-}" ] && rlmdb_git=$(cd "$RLMDB_DIR" && git rev-parse --git-common-dir 2>/dev/null); then
  case $rlmdb_git in /*) ;; *) rlmdb_git=$RLMDB_DIR/$rlmdb_git ;; esac
  rlmdb_git=$(cd "$rlmdb_git" && pwd)
  case $rlmdb_git/ in "$RLMDB_DIR"/*|"$git_mount"/) ;; *) rlmdb_git_mount=$rlmdb_git ;; esac
fi
# shellcheck disable=SC2086
exec docker run --rm -i $platform \
  $tmpfs_mount \
  -v "$repo:/work" \
  ${git_mount:+-v "$git_mount:$git_mount:ro"} \
  ${RLMDB_DIR:+-v "$RLMDB_DIR:/rlmdb:ro"} \
  ${rlmdb_git_mount:+-v "$rlmdb_git_mount:$rlmdb_git_mount:ro"} \
  -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0='*' \
  -v "$cache_vol:/root/.cache" \
  -v lmdb-go-modcache:/go/pkg/mod \
  -e GOFLAGS="${GOFLAGS:-}" \
  -w /work \
  "$image" "$@"
