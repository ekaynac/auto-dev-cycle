#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs a test command inside the test sandbox (docs/design/m0/11 "Tests run
# inside a sandbox"): the real home and state do not exist, the repository is
# read-only, one scratch area is writable, there is no network beyond the
# sandbox's own loopback, and the environment is an explicit allowlist.
#
# Usage: scripts/test-sandbox.sh [--backend auto|bwrap|container|none]
#                                [--env NAME]... -- COMMAND [ARG]...
#
# Backends, in `auto` order. Each is chosen only after a probe run with the
# exact configuration succeeds; a probe failure is a setup failure, never a
# test failure.
#   bwrap      bubblewrap with every namespace unshared (default on Linux)
#   container  a ROOTLESS podman or docker runtime (D37); a rootful daemon is
#              refused
#   none       no containment; refused unless ADC_TEST_UNSANDBOXED=1. Exists
#              only so the sandbox self-test can prove its checks go red.
#
# Mounts inside: /src (repo, ro), /scratch/work (this run's private work dir),
# /scratch/gocache (Go build cache, shared between runs and therefore
# untrusted; the self-test uses a fresh one), /opt/go (ro), /opt/node/bin/node
# (ro, the binary only), /gomodcache (ro). Every source is canonicalised and
# refused if it is /, covers $HOME, or overlaps the repository.
#
# Supplementary breach detection (the boundary is the namespaces, not this):
# a host sentinel under /tmp must stay an empty directory with the same inode,
# and canaries planted in $HOME and the repository root must be unchanged.
#
# Exit codes: the command's own, or 1 on a detected breach, 2 when no sandbox
# could be set up (could_not_run), 64 on usage errors.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# Multi-arch index digest of ubuntu:24.04, the container backend's base.
container_image="docker.io/library/ubuntu@sha256:a853f94d226358a79c740cfc7bce0c289748f3fe3488d921d038ccd752c61b60"
deny_env_re='(_TOKEN|_KEY|_SECRET|_PASSWORD)$|^(ANTHROPIC_|OPENAI_|OPENROUTER_|CLAUDE_|AWS_|GH_|GITHUB_)|^(CODEX_HOME|SSH_AUTH_SOCK)$'

die() { printf 'test-sandbox: %s\n' "$1" >&2; exit "${2:-2}"; }

backend=auto
pass_env=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --backend) backend="${2:?}"; shift 2 ;;
    --env)
      [[ "${2:?}" =~ ^[A-Z][A-Z0-9_]*$ ]] || die "invalid --env name: $2" 64
      [[ "$2" =~ $deny_env_re ]] && die "--env $2 looks like a credential; refused" 64
      [[ "$2" =~ ^(ADC_TEST_|ADC_SANDBOX_|GO|CGO_|PATH$|HOME$|TMPDIR$|LANG$|PWD$) ]] \
        && die "--env $2 would override a sandbox variable; refused" 64
      pass_env+=("$2"); shift 2 ;;
    --) shift; break ;;
    *) die "unknown argument: $1" 64 ;;
  esac
done
[[ $# -gt 0 ]] || die "no command given" 64
case "$backend" in
  auto | bwrap | container) ;;
  none) [[ "${ADC_TEST_UNSANDBOXED:-}" == 1 ]] || die "backend 'none' requires ADC_TEST_UNSANDBOXED=1 (sandbox self-test only)" ;;
  *) die "unknown backend: $backend" 64 ;;
esac

home="$(realpath -m -- "$HOME")"
goroot="$(realpath -m -- "$("$repo_root/scripts/bootstrap-go.sh" --print-goroot)")"
[[ -x "$goroot/bin/go" ]] || die "Go toolchain missing at $goroot; run 'make toolchain'"
node_bin="$(command -v node || true)"
[[ -n "$node_bin" ]] || die "node not found on PATH"
node_bin="$(realpath -e -- "$node_bin")"
gomodcache="$(realpath -m -- "$("$goroot/bin/go" env GOMODCACHE)")"
scratch_root="$(realpath -m -- "${ADC_TEST_SCRATCH:-${XDG_CACHE_HOME:-$HOME/.cache}/adc-test}")"

covers() { [[ "$2" == "$1" || "$2" == "$1"/* ]]; }   # covers A B: A is B or an ancestor of B
check_source() {  # check_source <label> <canonical path>
  [[ "$2" == / ]] && die "$1 mount source is /; refused"
  covers "$2" "$home" && die "$1 mount source $2 covers \$HOME; refused"
  { covers "$2" "$repo_root" || covers "$repo_root" "$2"; } && die "$1 mount source $2 overlaps the repository; refused"
  return 0
}
check_source "Go toolchain" "$goroot"
check_source "node" "$node_bin"
check_source "Go module cache" "$gomodcache"
check_source "scratch" "$scratch_root"
for p in "$goroot" "$node_bin" "$gomodcache"; do
  { covers "$scratch_root" "$p" || covers "$p" "$scratch_root"; } && die "scratch $scratch_root overlaps $p; refused"
done

mkdir -p "$scratch_root/gocache"
chmod 700 "$scratch_root"
work="$(mktemp -d "$scratch_root/work.XXXXXX")"
sentinel="$(mktemp -d /tmp/adc-sentinel-XXXXXX)"
sentinel_inode="$(stat -c %i "$sentinel")"
home_canary="$(mktemp "$home/.adc-sandbox-canary-XXXXXX")"
repo_canary="$(mktemp "$repo_root/.adc-sandbox-canary-XXXXXX")"
canary_value="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
printf '%s\n' "$canary_value" >"$home_canary"
printf '%s\n' "$canary_value" >"$repo_canary"
trap 'rm -rf "$work" "$sentinel"; rm -f "$home_canary" "$repo_canary"' EXIT

# The environment inside the sandbox: this list and the --env names, nothing else.
inner_env=(
  PATH=/opt/go/bin:/opt/node/bin:/usr/bin:/bin
  HOME=/home/sandbox
  TMPDIR=/tmp
  LANG=C.UTF-8
  GOROOT=/opt/go
  GOCACHE=/scratch/gocache
  GOMODCACHE=/gomodcache
  GOFLAGS=-mod=readonly
  GOPROXY=off
  GOTOOLCHAIN=local
  GOTELEMETRY=off
  CGO_ENABLED=0
  ADC_TEST_WORK=/scratch/work
  ADC_TEST_PASSENV="${pass_env[*]}"
  ADC_SANDBOX_SENTINEL="$sentinel"
  ADC_SANDBOX_HOST_HOME="$home"
  ADC_SANDBOX_HOST_REPO="$repo_root"
  ADC_SANDBOX_CANARY="${home_canary##*/} ${repo_canary##*/}"
)
for name in "${pass_env[@]}"; do inner_env+=("$name=${!name-}"); done

run_bwrap() {
  local args=(--unshare-all --die-with-parent --new-session --clearenv
    --ro-bind /usr /usr --proc /proc --dev /dev
    --tmpfs /tmp --tmpfs /home --dir /home/sandbox
    --ro-bind "$repo_root" /src
    --bind "$work" /scratch/work --bind "$scratch_root/gocache" /scratch/gocache
    --ro-bind "$goroot" /opt/go --ro-bind "$node_bin" /opt/node/bin/node
    --chdir /src)
  local p kv
  for p in /bin /lib /lib64 /sbin; do
    if [[ -L "$p" ]]; then args+=(--symlink "$(readlink "$p")" "$p")
    elif [[ -d "$p" ]]; then args+=(--ro-bind "$p" "$p"); fi
  done
  for p in /etc/ld.so.cache /etc/ld.so.conf /etc/ld.so.conf.d /etc/alternatives \
           /etc/passwd /etc/group /etc/nsswitch.conf /etc/localtime; do
    [[ -e "$p" ]] && args+=(--ro-bind "$p" "$p")
  done
  if [[ -d "$gomodcache" ]]; then args+=(--ro-bind "$gomodcache" /gomodcache); fi
  for kv in "${inner_env[@]}"; do args+=(--setenv "${kv%%=*}" "${kv#*=}"); done
  bwrap "${args[@]}" -- "$@"
}

# container_engine: prints a usable ROOTLESS engine, or nothing.
container_engine() {
  if command -v podman >/dev/null && [[ "$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" == true ]]; then
    echo podman
  elif command -v docker >/dev/null && docker info --format '{{range .SecurityOptions}}{{.}} {{end}}' 2>/dev/null | grep -q 'name=rootless'; then
    echo docker
  fi
}

run_container() {
  local engine
  engine="$(container_engine)"
  [[ -n "$engine" ]] || return 125
  local args=(run --rm --init --network none --cap-drop ALL
    --security-opt no-new-privileges --read-only --pids-limit 4096
    --tmpfs "/tmp:rw,exec,nosuid,nodev,size=4g"
    --tmpfs "/home/sandbox:rw,nosuid,nodev,size=1g"
    -v "$repo_root:/src:ro" -v "$work:/scratch/work:rw" -v "$scratch_root/gocache:/scratch/gocache:rw"
    -v "$goroot:/opt/go:ro" -v "$node_bin:/opt/node/bin/node:ro"
    -w /src)
  # Rootless podman maps the host user with keep-id; rootless docker maps
  # container root to the host user, so it runs as root inside its userns.
  if [[ "$engine" == podman ]]; then args+=(--userns=keep-id --http-proxy=false); fi
  if [[ -d "$gomodcache" ]]; then args+=(-v "$gomodcache:/gomodcache:ro"); fi
  # The engine adds variables of its own (proxies, HOSTNAME, image ENV);
  # env -i inside the container leaves exactly the allowlist.
  "$engine" "${args[@]}" "$container_image" /usr/bin/env -i "${inner_env[@]}" "$@"
}

run_none() {
  # Host environment kept on purpose: this is the "no containment" control.
  (cd "$repo_root" && env "${inner_env[@]/#PATH=*/PATH=$goroot/bin:$PATH}" \
    GOROOT="$goroot" GOCACHE="$scratch_root/gocache" GOMODCACHE="$gomodcache" HOME="$HOME" \
    TMPDIR=/tmp ADC_TEST_WORK="$work" "$@")
}

probe() { "run_$1" /bin/true >/dev/null 2>&1; }  # the exact configuration, a harmless command

if [[ "$backend" == auto ]]; then
  if command -v bwrap >/dev/null && probe bwrap; then backend=bwrap
  elif probe container; then backend=container
  else die "no sandbox backend works: bubblewrap cannot create namespaces and no rootless podman/docker can run the sandbox; tests refuse to run unsandboxed"
  fi
elif [[ "$backend" != none ]]; then
  probe "$backend" || die "backend $backend cannot start the sandbox on this host (a container backend must be rootless)"
fi
[[ "$backend" == container ]] && printf 'test-sandbox: container backend via %s (rootless)\n' "$(container_engine)" >&2

rc=0
case "$backend" in
  bwrap) run_bwrap "$@" || rc=$? ;;
  container) run_container "$@" || rc=$? ;;
  none) run_none "$@" || rc=$? ;;
esac

breach() { printf 'test-sandbox: containment breach (backend %s): %s\n' "$backend" "$1" >&2; exit 1; }
[[ -d "$sentinel" && ! -L "$sentinel" ]] || breach "the host sentinel $sentinel was removed or replaced"
[[ "$(stat -c %i "$sentinel")" == "$sentinel_inode" ]] || breach "the host sentinel $sentinel was replaced"
entries="$(find "$sentinel" -mindepth 1 -print -quit)" || breach "the host sentinel $sentinel could not be inspected"
[[ -z "$entries" ]] || breach "the host sentinel $sentinel was written"
for c in "$home_canary" "$repo_canary"; do
  [[ -f "$c" && "$(cat "$c")" == "$canary_value" ]] || breach "the canary $c was changed or removed"
done
exit "$rc"
