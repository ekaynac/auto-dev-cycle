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
#   container  ROOTLESS podman (D37), for hosts without usable user
#              namespaces for bubblewrap. Rootful runtimes and docker are not
#              supported (an engine-default init process would keep injected
#              proxy credentials readable; see docs/reviews/2026-10-03-pr8-astra.md)
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
# Self-test only: weaken exactly one property so each detection check can
# prove its own red (scripts/test-sandbox-selftest.sh).
mutation="${ADC_TEST_SANDBOX_MUTATION:-}"
if [[ -n "$mutation" ]]; then
  [[ "${ADC_TEST_UNSANDBOXED:-}" == 1 && "$backend" == bwrap ]] \
    || die "ADC_TEST_SANDBOX_MUTATION needs ADC_TEST_UNSANDBOXED=1 and --backend bwrap (sandbox self-test only)"
  [[ "$mutation" =~ ^(expose-home|expose-state|repo-alias|repo-writable|extra-mount)$ ]] || die "unknown mutation: $mutation" 64
fi

home="$(realpath -m -- "$HOME")"
goroot="$(realpath -m -- "$("$repo_root/scripts/bootstrap-go.sh" --print-goroot)")"
[[ -x "$goroot/bin/go" ]] || die "Go toolchain missing at $goroot; run 'make toolchain'"
node_bin="$(command -v node || true)"
[[ -n "$node_bin" ]] || die "node not found on PATH"
node_bin="$(realpath -e -- "$node_bin")"
gomodcache="$(realpath -m -- "$("$goroot/bin/go" env GOMODCACHE)")"
scratch_root="$(realpath -m -- "${ADC_TEST_SCRATCH:-${XDG_CACHE_HOME:-$HOME/.cache}/adc-test}")"

covers() { [[ "$2" == "$1" || "$2" == "$1"/* ]]; }   # covers A B: A is B or an ancestor of B
# Protected roots: user state, config and data (controller state lives
# there) and credential directories. No mount source may be one of them, lie
# inside one, or contain one.
state_dir="$(realpath -m -- "${XDG_STATE_HOME:-$home/.local/state}")"
config_dir="$(realpath -m -- "${XDG_CONFIG_HOME:-$home/.config}")"
data_dir="$(realpath -m -- "${XDG_DATA_HOME:-$home/.local/share}")"
protected_roots=("$state_dir" "$config_dir" "$data_dir")
for d in .ssh .gnupg .aws .azure .docker .kube .netrc .git-credentials .claude .codex .pki .password-store; do
  protected_roots+=("$(realpath -m -- "$home/$d")")
done
check_source() {  # check_source <label> <canonical path>
  [[ "$2" == / ]] && die "$1 mount source is /; refused"
  covers "$2" "$home" && die "$1 mount source $2 covers \$HOME; refused"
  { covers "$2" "$repo_root" || covers "$repo_root" "$2"; } && die "$1 mount source $2 overlaps the repository; refused"
  local p
  for p in "${protected_roots[@]}"; do
    { covers "$2" "$p" || covers "$p" "$2"; } && die "$1 mount source $2 overlaps protected $p; refused"
  done
  return 0
}
check_source "Go toolchain" "$goroot"
check_source "node" "$node_bin"
check_source "Go module cache" "$gomodcache"
check_source "scratch" "$scratch_root"
for p in "$goroot" "$node_bin" "$gomodcache"; do
  { covers "$scratch_root" "$p" || covers "$p" "$scratch_root"; } && die "scratch $scratch_root overlaps $p; refused"
done

mkdir -p "$scratch_root"
chmod 700 "$scratch_root"
[[ -L "$scratch_root/gocache" ]] && die "scratch $scratch_root/gocache is a symlink; refused"
mkdir -p "$scratch_root/gocache"
gocache="$(realpath -e -- "$scratch_root/gocache")"
[[ "$gocache" == "$scratch_root/gocache" ]] || die "scratch gocache resolves outside $scratch_root; refused"
check_source "Go build cache" "$gocache"
work="$(mktemp -d "$scratch_root/work.XXXXXX")"
sentinel="$(mktemp -d /tmp/adc-sentinel-XXXXXX)"
sentinel_inode="$(stat -c %i "$sentinel")"
# Canaries mark protected places: the home, the user state directory (where
# controller state lives; always present, created if missing), the config and
# data directories when they exist, and the repository. Paths are canonical,
# so a symlinked directory gets its canary in the real target.
canary_value="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
mkdir -p "$state_dir"
protected_canaries=() protected_kinds=()
for kd in "home:$home" "state:$state_dir" "config:$config_dir" "data:$data_dir"; do
  [[ -d "${kd#*:}" ]] || continue
  protected_kinds+=("${kd%%:*}")
  protected_canaries+=("$(mktemp "${kd#*:}/.adc-sandbox-canary-XXXXXX")")
done
repo_canary="$(mktemp "$repo_root/.adc-sandbox-canary-XXXXXX")"
trap 'rm -rf "$work" "$sentinel"; rm -f "${protected_canaries[@]}" "$repo_canary"' EXIT
for c in "${protected_canaries[@]}" "$repo_canary"; do printf '%s\n' "$canary_value" >"$c"; done
canary_names=""
for c in "${protected_canaries[@]}"; do canary_names+="protected:$c "; done
canary_of() {  # canary_of <kind>: the canary planted for that kind
  local i
  for i in "${!protected_kinds[@]}"; do
    if [[ "${protected_kinds[$i]}" == "$1" ]]; then echo "${protected_canaries[$i]}"; return 0; fi
  done
  die "no $1 canary was planted"
}
canary_names+="repo:$repo_canary"

mutation_args=()
case "$mutation" in
  expose-home | expose-state)
    c="$(canary_of "${mutation#expose-}")"; mutation_args=(--ro-bind "$c" "/mut/exposed/${c##*/}") ;;
  repo-alias) mutation_args=(--ro-bind "$repo_root" /mut/repo) ;;
  repo-writable) mutation_args=(--bind "$repo_root" /src) ;;
  extra-mount) mutation_args=(--ro-bind /usr/share /mut/extra) ;;
esac

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
  ADC_SANDBOX_CANARY="$canary_names"
)
for name in "${pass_env[@]}"; do inner_env+=("$name=${!name-}"); done

run_bwrap() {
  local args=(--unshare-all --die-with-parent --new-session --clearenv
    --ro-bind /usr /usr --proc /proc --dev /dev
    --tmpfs /tmp --tmpfs /home --dir /home/sandbox
    --ro-bind "$repo_root" /src
    --bind "$work" /scratch/work --bind "$gocache" /scratch/gocache
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
  args+=("${mutation_args[@]}")
  # bwrap's own init (PID 1 inside) keeps the environment bwrap was started
  # with, readable through /proc/1/environ: start it with nothing.
  env -i PATH=/usr/bin:/bin "$(command -v bwrap)" "${args[@]}" -- "$@"
}

podman_rootless() {
  command -v podman >/dev/null && [[ "$(CONTAINERS_CONF="$(podman_conf)" podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" == true ]]
}

# A controlled containers.conf: with CONTAINERS_CONF set, podman ignores every
# system and user configuration file, so no configured mount, volume, device,
# environment or namespace default reaches the sandbox. The same settings are
# also passed as explicit flags.
podman_conf() {
  local conf="$scratch_root/containers.conf"
  cat >"$conf" <<'CONF'
[containers]
mounts = []
volumes = []
devices = []
env = []
env_host = false
http_proxy = false
init = false
default_capabilities = []
ipcns = "private"
pidns = "private"
utsns = "private"
cgroupns = "private"
netns = "none"
CONF
  printf '%s\n' "$conf"
}

run_container() {
  podman_rootless || return 125
  # --init=false explicitly: an init process would keep engine-injected
  # variables readable through /proc/1/environ. PID 1 is env -i, which execs
  # the command.
  local args=(run --rm --init=false --http-proxy=false --userns=keep-id
    --pid=private --ipc=private --uts=private --cgroupns=private
    --network none --cap-drop ALL
    --security-opt no-new-privileges --read-only --pids-limit 4096
    --tmpfs "/tmp:rw,exec,nosuid,nodev,size=4g"
    --tmpfs "/home/sandbox:rw,nosuid,nodev,size=1g"
    -v "$repo_root:/src:ro" -v "$work:/scratch/work:rw" -v "$gocache:/scratch/gocache:rw"
    -v "$goroot:/opt/go:ro" -v "$node_bin:/opt/node/bin/node:ro"
    -w /src)
  if [[ -d "$gomodcache" ]]; then args+=(-v "$gomodcache:/gomodcache:ro"); fi
  # The engine adds variables of its own (proxies, HOSTNAME, image ENV);
  # env -i inside the container leaves exactly the allowlist.
  CONTAINERS_CONF="$(podman_conf)" podman "${args[@]}" "$container_image" /usr/bin/env -i "${inner_env[@]}" "$@"
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
  else die "no sandbox backend works: bubblewrap cannot create namespaces and no rootless podman can run the sandbox; tests refuse to run unsandboxed"
  fi
elif [[ "$backend" != none ]]; then
  probe "$backend" || die "backend $backend cannot start the sandbox on this host (a container backend must be rootless)"
fi
[[ "$backend" == container ]] && printf 'test-sandbox: container backend via rootless podman\n' >&2

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
for c in "${protected_canaries[@]}" "$repo_canary"; do
  [[ -f "$c" && "$(cat "$c")" == "$canary_value" ]] || breach "the canary $c was changed or removed"
done
exit "$rc"
