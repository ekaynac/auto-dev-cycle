#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs a test command inside the test sandbox (docs/design/m0/11 "Tests run
# inside a sandbox"): the real home and state do not exist, the repository is
# read-only, one scratch directory is writable, there is no network beyond
# loopback, and the environment is an explicit allowlist.
#
# Usage: scripts/test-sandbox.sh [--backend auto|bwrap|container|none]
#                                [--env NAME]... -- COMMAND [ARG]...
#
# Backends, in `auto` order:
#   bwrap      bubblewrap with every namespace unshared (default on Linux)
#   container  a rootless podman container, else docker with an unprivileged
#              user, no capabilities and no network (hosts without userns)
#   none       no containment; refused unless ADC_TEST_UNSANDBOXED=1. Exists
#              only so the sandbox self-test can prove its checks go red.
#
# Every run plants a sentinel directory on the host under /tmp and fails if
# anything inside the sandbox reached it (exit 1, "containment breach").
# Exit 2 means no sandbox backend could run (could_not_run).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Multi-arch index digest of ubuntu:24.04, the container backend's base.
container_image="docker.io/library/ubuntu@sha256:a853f94d226358a79c740cfc7bce0c289748f3fe3488d921d038ccd752c61b60"
scratch_root="${ADC_TEST_SCRATCH:-${XDG_CACHE_HOME:-$HOME/.cache}/adc-test}"
deny_env_re='(_TOKEN|_KEY|_SECRET|_PASSWORD)$|^(ANTHROPIC_|OPENAI_|OPENROUTER_|CLAUDE_|AWS_|GH_|GITHUB_)|^(CODEX_HOME|SSH_AUTH_SOCK)$'

die() { printf 'test-sandbox: %s\n' "$1" >&2; exit "${2:-2}"; }

backend=auto
pass_env=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --backend) backend="${2:?}"; shift 2 ;;
    --env)
      [[ "${2:?}" =~ ^[A-Z_][A-Z0-9_]*$ ]] || die "invalid --env name: $2" 64
      [[ "$2" =~ $deny_env_re ]] && die "--env $2 looks like a credential; refused" 64
      pass_env+=("$2"); shift 2 ;;
    --) shift; break ;;
    *) die "unknown argument: $1" 64 ;;
  esac
done
[[ $# -gt 0 ]] || die "no command given" 64

goroot="$("$repo_root/scripts/bootstrap-go.sh" --print-goroot)"
[[ -x "$goroot/bin/go" ]] || die "Go toolchain missing at $goroot; run 'make toolchain'"
node_bin="$(command -v node || true)"
[[ -n "$node_bin" ]] || die "node not found on PATH"
node_prefix="$(dirname "$(dirname "$(readlink -f "$node_bin")")")"
gomodcache="$("$goroot/bin/go" env GOMODCACHE)"

bwrap_works() {
  command -v bwrap >/dev/null && bwrap --unshare-all --ro-bind / / true 2>/dev/null
}
engine_works() {
  command -v "$1" >/dev/null && "$1" info >/dev/null 2>&1
}

if [[ "$backend" == auto ]]; then
  if bwrap_works; then backend=bwrap
  elif engine_works podman || engine_works docker; then backend=container
  else die "no sandbox backend (bubblewrap, podman or docker) works on this host; tests refuse to run unsandboxed"
  fi
fi
case "$backend" in
  bwrap) bwrap_works || die "bubblewrap cannot create namespaces on this host" ;;
  container) engine_works podman || engine_works docker || die "neither podman nor docker is usable" ;;
  none) [[ "${ADC_TEST_UNSANDBOXED:-}" == 1 ]] || die "backend 'none' requires ADC_TEST_UNSANDBOXED=1 (sandbox self-test only)" ;;
  *) die "unknown backend: $backend" 64 ;;
esac

mkdir -p "$scratch_root/gocache"
work="$(mktemp -d "$scratch_root/work.XXXXXX")"
sentinel="$(mktemp -d /tmp/adc-sentinel-XXXXXX)"
trap 'rm -rf "$work" "$sentinel"' EXIT

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
  ADC_TEST_SANDBOX="$backend"
  ADC_SANDBOX_SENTINEL="$sentinel"
  ADC_SANDBOX_HOST_HOME="$HOME"
)
for name in "${pass_env[@]}"; do inner_env+=("$name=${!name-}"); done

run_bwrap() {
  local args=(--unshare-all --die-with-parent --new-session --clearenv
    --ro-bind /usr /usr --proc /proc --dev /dev
    --tmpfs /tmp --tmpfs /home --dir /home/sandbox
    --ro-bind "$repo_root" /src --bind "$scratch_root" /scratch
    --ro-bind "$goroot" /opt/go --ro-bind "$node_prefix" /opt/node
    --chdir /src)
  local p
  for p in /bin /lib /lib64 /sbin; do
    if [[ -L "$p" ]]; then args+=(--symlink "$(readlink "$p")" "$p")
    elif [[ -d "$p" ]]; then args+=(--ro-bind "$p" "$p"); fi
  done
  for p in /etc/ld.so.cache /etc/ld.so.conf /etc/ld.so.conf.d /etc/alternatives \
           /etc/passwd /etc/group /etc/nsswitch.conf /etc/localtime; do
    [[ -e "$p" ]] && args+=(--ro-bind "$p" "$p")
  done
  if [[ -d "$gomodcache" ]]; then args+=(--ro-bind "$gomodcache" /gomodcache); fi
  for kv in "${inner_env[@]}"; do
    args+=(--setenv "${kv%%=*}" "${kv#*=}")
  done
  args+=(--setenv ADC_TEST_WORK "/scratch/${work##*/}")
  bwrap "${args[@]}" -- "$@"
}

run_container() {
  local engine=docker userns=(--user "$(id -u):$(id -g)")
  if engine_works podman; then engine=podman userns=(--userns=keep-id); fi
  local args=(run --rm --init --network none --cap-drop ALL
    --security-opt no-new-privileges --read-only --pids-limit 4096
    "${userns[@]}"
    --tmpfs "/tmp:rw,exec,nosuid,nodev,size=4g"
    --tmpfs "/home/sandbox:rw,nosuid,nodev,size=1g"
    -v "$repo_root:/src:ro" -v "$scratch_root:/scratch:rw"
    -v "$goroot:/opt/go:ro" -v "$node_prefix:/opt/node:ro"
    -w /src)
  if [[ -d "$gomodcache" ]]; then args+=(-v "$gomodcache:/gomodcache:ro"); fi
  for kv in "${inner_env[@]}"; do args+=(-e "$kv"); done
  args+=(-e "ADC_TEST_WORK=/scratch/${work##*/}")
  printf 'test-sandbox: container backend via %s\n' "$engine" >&2
  "$engine" "${args[@]}" "$container_image" "$@"
}

run_none() {
  # Host environment kept on purpose: this is the "no containment" control.
  (cd "$repo_root" && env GOROOT="$goroot" PATH="$goroot/bin:$PATH" GOTOOLCHAIN=local CGO_ENABLED=0 \
    ADC_TEST_SANDBOX=none ADC_SANDBOX_SENTINEL="$sentinel" ADC_SANDBOX_HOST_HOME="$HOME" \
    ADC_TEST_WORK="$work" "$@")
}

rc=0
case "$backend" in
  bwrap) run_bwrap "$@" || rc=$? ;;
  container) run_container "$@" || rc=$? ;;
  none) run_none "$@" || rc=$? ;;
esac

if [[ -n "$(find "$sentinel" -mindepth 1 -print -quit)" ]]; then
  printf 'test-sandbox: containment breach: the host sentinel %s was written (backend %s)\n' "$sentinel" "$backend" >&2
  exit 1
fi
exit "$rc"
