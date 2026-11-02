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
#   LINUX_TEST_PLATFORM  e.g. linux/amd64 to run x86-64 under emulation,
#                        linux/s390x for a 64-bit big-endian machine (with
#                        LINUX_TEST_IMAGE=golang:1.25-trixie: the bookworm
#                        images have no s390x variant)
#                        (default: the VM's native architecture)
#   LINUX_TEST_MEMORY    memory limit of the container, swap included
#                        (default 2g; 0 disables)
#   LINUX_TEST_CPUS      CPUs the container may use (default 8, or the
#                        Docker host's number if it has fewer; 0: no limit)
#   LINUX_TEST_TMPFS     size of the tmpfs at /tmp (default: the memory
#                        limit, 4g without one; 0 disables)
#   RLMDB_DIR            rlmdb checkout (its repository root, with the
#                        vendored C sources in tests/lmdb-c) to mount
#                        read-only at /rlmdb (optional; for the C oracles:
#                        scripts/build-c-oracles.sh, build-c-writeops.sh)
#
# /tmp, where tests and benchmarks create environments, is a tmpfs: the
# alternatives in the VM are its virtual disk and the virtiofs host mount,
# neither of which is a local file system. fsync timings mean nothing here.
#
# Every test suite and benchmark must run on a machine with 2 GB of RAM, so
# by default the container is one: 2 GB without swap for everything in it
# (the Go toolchain, test binaries, C peers, the files in /tmp, which are
# memory, and the page cache, which the kernel shrinks to stay within the
# limit), and at most 8 CPUs, since parallel compilers, test binaries and
# fuzzing workers multiply with the CPUs (GOMAXPROCS is set to match: the
# modules' go version predates Go's own reading of the CPU limit). When the
# command ends, the script reports the container's memory peak (page cache
# included) and fails the run if the kernel's OOM killer ended a process
# in it.
#
# Foreign platforms run through the VM kernel's binfmt_misc handlers
# (Lima's Rosetta for linux/amd64, QEMU user emulation for the others). On
# a rootless Docker daemon on Linux 6.7 or later, a privileged container
# that mounts binfmt_misc (rlmdb's cross runs, tonistiigi/binfmt) creates
# an instance for the daemon's user namespace, which from then on replaces
# the VM's handlers for every container of the daemon and holds entries
# only while some container has it mounted; afterwards containers of every
# foreign platform fail with "exec format error" until the daemon
# restarts. So when a container of LINUX_TEST_PLATFORM cannot start for
# that reason, the script registers the missing handler, and Rosetta if
# the VM has it, in that instance from a small privileged container,
# lmdb-go-binfmt, which keeps the instance mounted and stays running for
# later runs and other users of the daemon. It only adds entries: an
# existing one is never changed or removed. QEMU user mode has no robust
# futexes (a mutex whose owner died is never marked EOWNERDEAD, in C too):
# for the platforms it emulates (all but linux/amd64, which Rosetta runs)
# the script sets LMDB_TEST_NO_ROBUST_MUTEX=1, and the tests of owner-death
# recovery skip (rlmdb's INTEROP_NO_ROBUST_MUTEX). Set it by hand for a
# linux/amd64 run through qemu-x86_64 (no Rosetta).
set -eu
repo=$(cd "$(dirname "$0")/.." && pwd)
image=${LINUX_TEST_IMAGE:-golang:1.25-bookworm}
memory=${LINUX_TEST_MEMORY-2g}
case $memory in ""|0) memory= ;; esac
tmpfs=${LINUX_TEST_TMPFS-${memory:-4g}}
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
cpus=${LINUX_TEST_CPUS-8}
case $cpus in ""|0) cpus= ;; esac
if [ -n "$cpus" ]; then
  ncpu=$(docker info --format '{{.NCPU}}' 2>/dev/null) || ncpu=
  if [ -n "$ncpu" ] && [ "$ncpu" -lt "$cpus" ]; then cpus=$ncpu; fi
fi

# memory_run: the command line in the container under a memory limit ($0
# of the shell, for the messages): the command runs as a child of the
# shell, which forwards SIGINT and SIGTERM to it (docker passes them to the
# shell, process 1), and then reports the cgroup's memory peak and OOM kills
# (cgroup v2; v1 as a fallback). A run in which the OOM killer ended a
# process fails even if the command itself succeeded. The report uses shell
# builtins only: when /tmp holds the memory, a new process would be killed.
memory_run='exec 3<&0
"$@" <&3 3<&- &
pid=$!
trap "kill -INT $pid 2>/dev/null" INT
trap "kill -TERM $pid 2>/dev/null" TERM
while :; do
  wait "$pid"
  rc=$?
  kill -0 "$pid" 2>/dev/null || break
done
cg=/sys/fs/cgroup events=$cg/memory.events peakf=$cg/memory.peak
if [ ! -r $events ]; then
  events=$cg/memory/memory.oom_control peakf=$cg/memory/memory.max_usage_in_bytes
fi
oom=0 peak=
if [ -r $events ]; then
  while read -r k v; do [ "$k" != oom_kill ] || oom=$v; done < $events
fi
if [ -r $peakf ]; then read -r peak < $peakf; fi
if [ -n "$peak" ]; then
  echo "linux-test.sh: memory peak $((peak >> 20)) MiB of $0 (with /tmp and the page cache)" >&2
fi
if [ "$oom" -gt 0 ]; then
  echo "linux-test.sh: the OOM killer ended $oom process(es): the run needs more than $0 of memory" >&2
  [ "$rc" -ne 0 ] || rc=137
fi
exit "$rc"'

# binfmt_ensure <platform>: make containers of <platform> start (see the
# header). The registration runs in the holder container with the qemu
# name of the architecture and Rosetta's registration line (that of Lima's
# VM; empty without Rosetta) as arguments.
binfmt_register='bf=/proc/sys/fs/binfmt_misc
{ [ -e $bf/register ] || mount -t binfmt_misc binfmt_misc $bf; } || exit 1
add() {
  [ -e "$bf/$1" ] || printf "%s" "$2" > $bf/register || [ -e "$bf/$1" ] ||
    { echo "linux-test.sh: could not register $1" >&2; return 1; }
}
if [ -n "$2" ]; then add rosetta "$2" || exit 1; fi
if [ "$1" != x86_64 ] || [ -z "$2" ]; then
  add "qemu-$1" "$(cat "/usr/lib/binfmt.d/qemu-$1.conf")" || exit 1
fi'
binfmt_ensure() {
  if out=$(docker run --rm --platform "$1" "$image" true 2>&1) ||
      ! printf '%s' "$out" | grep -q 'exec format error'; then
    return 0
  fi
  case ${1#*/} in
    amd64) q=x86_64 ;;
    arm64) q=aarch64 ;;
    *) q=${1#*/} ;;
  esac
  dockerfile=$(printf '%s\n' "FROM debian:bookworm-slim" \
    "RUN apt-get update && apt-get install -y --no-install-recommends qemu-user-static && rm -rf /var/lib/apt/lists/*")
  helper="lmdb-go-binfmt:$(printf '%s' "$dockerfile" | cksum | cut -d' ' -f1)"
  if ! docker image inspect "$helper" >/dev/null 2>&1; then
    printf '%s\n' "$dockerfile" | docker build -q -t "$helper" - >&2
  fi
  rosetta=/mnt/lima-rosetta
  rosetta_mount= rosetta_line=
  if docker run --rm --mount "type=bind,source=$rosetta,target=/r,readonly" \
      "$helper" test -x /r/rosetta >/dev/null 2>&1; then
    rosetta_mount="--mount type=bind,source=$rosetta,target=$rosetta,readonly"
    rosetta_line=':rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:'$rosetta/rosetta:OCF
  fi
  if [ "$(docker inspect -f '{{.State.Running}}' lmdb-go-binfmt 2>/dev/null)" = true ]; then
    docker exec lmdb-go-binfmt sh -c "$binfmt_register" sh "$q" "$rosetta_line"
  else
    docker rm lmdb-go-binfmt >/dev/null 2>&1 || true
    # shellcheck disable=SC2086
    docker run -d --name lmdb-go-binfmt --privileged $rosetta_mount "$helper" \
      sh -c "$binfmt_register"' && exec sleep infinity' sh "$q" "$rosetta_line" >/dev/null
  fi
  i=0
  until docker run --rm --platform "$1" "$image" true >/dev/null 2>&1; do
    i=$((i + 1))
    if [ $i -ge 30 ]; then
      echo "linux-test.sh: containers of $1 still do not start:" >&2
      docker run --rm --platform "$1" "$image" true >&2 || true
      docker logs lmdb-go-binfmt >&2 || true
      exit 1
    fi
    sleep 1
  done
}
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
no_robust=${LMDB_TEST_NO_ROBUST_MUTEX:-}
if [ -n "${LINUX_TEST_PLATFORM:-}" ]; then
  binfmt_ensure "$LINUX_TEST_PLATFORM"
  case ${LINUX_TEST_PLATFORM#*/} in
    amd64 | arm64) ;;
    *) no_robust=1 ;;
  esac
fi
if [ -n "$memory" ]; then set -- sh -c "$memory_run" "$memory" "$@"; fi
# shellcheck disable=SC2086
exec docker run --rm -i $platform \
  ${memory:+--memory=$memory --memory-swap=$memory} \
  ${cpus:+--cpus=$cpus -e GOMAXPROCS=$cpus} \
  $tmpfs_mount \
  -v "$repo:/work" \
  ${git_mount:+-v "$git_mount:$git_mount:ro"} \
  ${RLMDB_DIR:+-v "$RLMDB_DIR:/rlmdb:ro"} \
  -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0='*' \
  -v "$cache_vol:/root/.cache" \
  -v lmdb-go-modcache:/go/pkg/mod \
  -e GOFLAGS="${GOFLAGS:-}" \
  ${no_robust:+-e LMDB_TEST_NO_ROBUST_MUTEX=1} \
  -w /work \
  "$image" "$@"
