#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs a test command inside the test sandbox (docs/design/m0/11 "Tests run
# inside a sandbox"): the real home and state do not exist, the repository is
# read-only, one scratch area is writable, there is no network beyond the
# sandbox's own loopback, and the environment is an explicit allowlist.
#
# Usage: scripts/test-sandbox.sh [--backend auto|bwrap|none]
#                                [--env NAME]... -- COMMAND [ARG]...
#
# Backends:
#   bwrap      bubblewrap with every namespace unshared (`auto`); chosen only
#              after a probe run with the exact configuration succeeds
#   none       no containment; refused unless ADC_TEST_UNSANDBOXED=1. Exists
#              only so the sandbox self-test can prove its checks go red.
# A rootless-container fallback for hosts without user namespaces (D37) is a
# separate change; until it lands, such hosts cannot run the tests.
#
# No arbitrary host directory is ever mounted. The sandbox sees exactly:
#   /src                the repository, read-only
#   /scratch/work       this run's work directory
#   /scratch/gocache    the Go build cache (shared between runs, so untrusted;
#                       the self-test uses a fresh one)
#   /opt/go, /opt/node/bin/node, /gomodcache
#                       PROVISIONED copies, read-only: the Go distribution
#                       subtrees, the node binary, and a module cache filled
#                       by `go mod download` (checked against go.sum)
# The scratch and provision areas are directories this script created: an
# existing directory without its marker (a credential or runtime directory,
# say) is refused, never mounted. Every path below them that is provisioned
# or mounted must consist of real directories: no symlink anywhere.
#
# Supplementary breach detection (the boundary is the namespaces, not this):
# a host sentinel under /tmp must stay an empty directory with the same inode,
# and canaries planted in $HOME, the user state/config/data directories and
# the repository root must be unchanged.
#
# Exit codes: the command's own, or 1 on a detected breach, 2 when no sandbox
# could be set up (could_not_run), 64 on usage errors.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
deny_env_re='(_TOKEN|_KEY|_SECRET|_PASSWORD)$|^(ANTHROPIC_|OPENAI_|OPENROUTER_|CLAUDE_|AWS_|GH_|GITHUB_)|^(CODEX_HOME|SSH_AUTH_SOCK)$'
marker_text="adc test-sandbox area v1"

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
  auto | bwrap) ;;
  none) [[ "${ADC_TEST_UNSANDBOXED:-}" == 1 ]] || die "backend 'none' requires ADC_TEST_UNSANDBOXED=1 (sandbox self-test only)" ;;
  *) die "unknown backend: $backend" 64 ;;
esac
# Self-test only: weaken exactly one property so each detection check can
# prove its own red (scripts/test-sandbox-selftest.sh).
mutation="${ADC_TEST_SANDBOX_MUTATION:-}"
if [[ -n "$mutation" ]]; then
  [[ "${ADC_TEST_UNSANDBOXED:-}" == 1 && "$backend" == bwrap ]] \
    || die "ADC_TEST_SANDBOX_MUTATION needs ADC_TEST_UNSANDBOXED=1 and --backend bwrap (sandbox self-test only)"
  [[ "$mutation" =~ ^(expose-home|expose-state|repo-alias|repo-writable|extra-mount|pseudo-mount|type-change|missing-mount)$ ]] || die "unknown mutation: $mutation" 64
fi

home="$(realpath -m -- "$HOME")"
goroot="$(realpath -m -- "$("$repo_root/scripts/bootstrap-go.sh" --print-goroot)")"
go_version="$("$repo_root/scripts/bootstrap-go.sh" --print-version)"
scratch_root="$(realpath -m -- "${ADC_TEST_SCRATCH:-${XDG_CACHE_HOME:-$HOME/.cache}/adc-test-sandbox}")"
provision_root="$(realpath -m -- "${ADC_TEST_PROVISION:-$scratch_root/provision}")"
# The host runs $goroot/bin/go (provisioning): it must lie outside every area
# the sandbox can reach, before it is used or even inspected.
for a in "$scratch_root" "$provision_root" "$repo_root"; do
  { [[ "$goroot" == "$a" || "$goroot" == "$a"/* || "$a" == "$goroot"/* ]]; } \
    && die "Go installation $goroot overlaps $a, which the sandbox can reach; refused"
done
[[ -x "$goroot/bin/go" && "$(head -n1 "$goroot/VERSION" 2>/dev/null)" == "$go_version" ]] \
  || die "Go $go_version missing at $goroot; run 'make toolchain'"
node_bin="$(command -v node || true)"
[[ -n "$node_bin" ]] || die "node not found on PATH"
node_bin="$(realpath -e -- "$node_bin")"
[[ -f "$node_bin" && ! -u "$node_bin" && ! -g "$node_bin" ]] || die "node at $node_bin is not a plain executable file"
covers() { [[ "$2" == "$1" || "$2" == "$1"/* ]]; }   # covers A B: A is B or an ancestor of B
# Protected roots: user state, config, data and runtime directories, and
# credential directories at their default or environment-selected places.
state_dir="$(realpath -m -- "${XDG_STATE_HOME:-$home/.local/state}")"
config_dir="$(realpath -m -- "${XDG_CONFIG_HOME:-$home/.config}")"
data_dir="$(realpath -m -- "${XDG_DATA_HOME:-$home/.local/share}")"
protected_roots=("$state_dir" "$config_dir" "$data_dir")
[[ -n "${XDG_RUNTIME_DIR:-}" ]] && protected_roots+=("$(realpath -m -- "$XDG_RUNTIME_DIR")")
for d in .ssh .gnupg .aws .azure .docker .kube .netrc .git-credentials .claude .codex .pki .password-store; do
  protected_roots+=("$(realpath -m -- "$home/$d")")
done
for v in CODEX_HOME CLAUDE_CONFIG_DIR GNUPGHOME DOCKER_CONFIG; do
  [[ -n "${!v:-}" ]] && protected_roots+=("$(realpath -m -- "${!v}")")
done
for v in KUBECONFIG AWS_CONFIG_FILE AWS_SHARED_CREDENTIALS_FILE GOOGLE_APPLICATION_CREDENTIALS; do
  [[ -n "${!v:-}" ]] && protected_roots+=("$(dirname "$(realpath -m -- "${!v%%:*}")")")
done

# claim_area <label> <dir>: the only directories ever mounted besides the
# repository. A new directory is created private and marked; an existing one
# is accepted only if it carries the marker, is ours, is private and is not a
# symlink. It may not be /, cover $HOME, overlap the repository, or overlap a
# protected root.
claim_area() {
  local label="$1" d="$2" p
  [[ "$d" == / ]] && die "$label $d is /; refused"
  covers "$d" "$home" && die "$label $d covers \$HOME; refused"
  { covers "$d" "$repo_root" || covers "$repo_root" "$d"; } && die "$label $d overlaps the repository; refused"
  for p in "${protected_roots[@]}"; do
    { covers "$d" "$p" || covers "$p" "$d"; } && die "$label $d overlaps protected $p; refused"
  done
  if [[ -e "$d" || -L "$d" ]]; then
    [[ -d "$d" && ! -L "$d" && -f "$d/.adc-test-area" && ! -L "$d/.adc-test-area" \
       && "$(cat "$d/.adc-test-area")" == "$marker_text" ]] \
      || die "$label $d exists but was not created by test-sandbox; refused"
    [[ "$(stat -c %u "$d")" == "$(id -u)" && "$(stat -c %a "$d")" == 700 ]] \
      || die "$label $d is not a private directory of this user; refused"
  else
    mkdir -p "$(dirname "$d")"
    mkdir -m 700 "$d"
    printf '%s\n' "$marker_text" >"$d/.adc-test-area"
  fi
}
# The repository is mounted too: it may not contain the home or overlap a
# protected root (a credential directory relocated into the checkout, say).
covers "$repo_root" "$home" && die "repository $repo_root contains \$HOME; refused"
for p in "${protected_roots[@]}"; do
  { covers "$repo_root" "$p" || covers "$p" "$repo_root"; } && die "repository $repo_root overlaps protected $p; refused"
done
claim_area scratch "$scratch_root"
if covers "$scratch_root" "$provision_root"; then
  [[ "$provision_root" == "$scratch_root/provision" ]] \
    || die "provision area $provision_root inside scratch must be $scratch_root/provision; refused"
else
  covers "$provision_root" "$scratch_root" && die "provision area $provision_root contains scratch; refused"
  claim_area provision "$provision_root"
fi
# inside_area <area> <path>: path lies below area through components that are
# not symlinks (existing ones are checked; missing ones are created later).
inside_area() {
  local area="$1" path="$2" cur part parts
  [[ "$path" == "$area"/* ]] || die "$path is not inside $area; refused"
  cur="$area"
  IFS=/ read -r -a parts <<<"${path#"$area"/}"
  for part in "${parts[@]}"; do
    cur="$cur/$part"
    if [[ -L "$cur" ]]; then die "$cur is a symlink; refused"; fi
  done
}
if covers "$scratch_root" "$provision_root" && [[ "$provision_root" != "$scratch_root" ]]; then
  inside_area "$scratch_root" "$provision_root"
fi
mkdir -p "$provision_root"

# Provisioning (outside the sandbox; the only step with network).
toolchain="$provision_root/go-$go_version"
inside_area "$provision_root" "$toolchain"
inside_area "$provision_root" "$toolchain.tmp"
if [[ ! -f "$toolchain/.complete" ]]; then
  rm -rf "$toolchain.tmp"
  mkdir -p "$toolchain.tmp"
  for part in bin pkg src lib VERSION go.env; do   # the distribution only, never the whole prefix
    [[ -e "$goroot/$part" ]] && cp -a --reflink=auto -- "$goroot/$part" "$toolchain.tmp/"
  done
  touch "$toolchain.tmp/.complete"
  rm -rf "$toolchain" && mv "$toolchain.tmp" "$toolchain"
fi
node_sum="$(sha256sum "$node_bin" | cut -c1-16)"
node_copy="$provision_root/node-$node_sum"
inside_area "$provision_root" "$node_copy/bin/node"
inside_area "$provision_root" "$node_copy/bin/node.tmp"
if [[ ! -x "$node_copy/bin/node" ]]; then
  mkdir -p "$node_copy/bin"
  cp -- "$node_bin" "$node_copy/bin/node.tmp" && chmod 0555 "$node_copy/bin/node.tmp" && mv "$node_copy/bin/node.tmp" "$node_copy/bin/node"
fi
modcache="$provision_root/modcache"
inside_area "$provision_root" "$modcache"
sum_digest="$({ cat "$repo_root/go.mod"; if [[ -f "$repo_root/go.sum" ]]; then cat "$repo_root/go.sum"; fi; } | sha256sum | cut -c1-16)"
if [[ ! -f "$modcache/.complete-$sum_digest" ]]; then
  mkdir -p "$modcache"
  (cd "$repo_root" && env -u GOFLAGS -u GONOSUMDB -u GONOSUMCHECK -u GOINSECURE -u GOPRIVATE -u GONOPROXY \
     GOMODCACHE="$modcache" GOSUMDB=sum.golang.org GOTOOLCHAIN=local GOTELEMETRY=off \
     "$goroot/bin/go" mod download) || die "could not prepare the module cache (go mod download)"
  touch "$modcache/.complete-$sum_digest"
fi

gocache="$scratch_root/gocache"
inside_area "$scratch_root" "$gocache"
mkdir -p "$gocache"
work="$(mktemp -d "$scratch_root/work.XXXXXX")"
# Re-check every mount source after provisioning, right before use.
for src in "$toolchain" "$node_copy/bin/node" "$modcache"; do inside_area "$provision_root" "$src"; done
for src in "$gocache" "$work"; do inside_area "$scratch_root" "$src"; done
for w in "$gocache" "$work"; do
  for r in "$provision_root" "$toolchain" "$node_copy" "$modcache"; do
    { covers "$w" "$r" || covers "$r" "$w"; } && die "writable $w overlaps provisioned $r; refused"
  done
done
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

# The filesystem type each bind must show inside: that of its real source.
fstype() { findmnt -no FSTYPE --target "$1" | head -n1; }
command -v findmnt >/dev/null || die "findmnt (util-linux) is required"
mount_types="/src=$(fstype "$repo_root") /scratch/work=$(fstype "$work") /scratch/gocache=$(fstype "$gocache")"
mount_types+=" /opt/go=$(fstype "$toolchain") /opt/node/bin/node=$(fstype "$node_copy/bin/node") /gomodcache=$(fstype "$modcache")"
mount_types+=" /usr=$(fstype /usr)"
for p in /bin /lib /lib64 /sbin; do [[ -d "$p" && ! -L "$p" ]] && mount_types+=" $p=$(fstype "$p")"; done
for p in /etc/ld.so.cache /etc/ld.so.conf /etc/ld.so.conf.d /etc/alternatives /etc/passwd /etc/group /etc/nsswitch.conf /etc/localtime; do
  [[ -e "$p" ]] && mount_types+=" $p=$(fstype "$p")"
done

mutation_args=()
case "$mutation" in
  expose-home | expose-state)
    c="$(canary_of "${mutation#expose-}")"; mutation_args=(--ro-bind "$c" "/mut/exposed/${c##*/}") ;;
  repo-alias) mutation_args=(--ro-bind "$repo_root" /mut/repo) ;;
  repo-writable) mutation_args=(--bind "$repo_root" /src) ;;
  extra-mount) mutation_args=(--ro-bind /usr/share /mut/extra) ;;
  pseudo-mount) mutation_args=(--ro-bind /usr/share /dev/adc-mut) ;;   # hidden under an exempt-looking tree
  type-change) ;;                                                         # handled in run_bwrap: proc instead of the bind
  missing-mount) ;;                                                       # handled in run_bwrap: /gomodcache not mounted
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
  ADC_SANDBOX_MOUNT_TYPES="$mount_types"
)
for name in "${pass_env[@]}"; do inner_env+=("$name=${!name-}"); done

run_bwrap() {
  local args=(--unshare-all --die-with-parent --new-session --clearenv
    --ro-bind /usr /usr --proc /proc --dev /dev
    --tmpfs /tmp --tmpfs /home --dir /home/sandbox
    --ro-bind "$repo_root" /src
    --bind "$work" /scratch/work --bind "$gocache" /scratch/gocache
    --ro-bind "$toolchain" /opt/go --ro-bind "$node_copy/bin/node" /opt/node/bin/node
    --chdir /src)
  case "$mutation" in
    missing-mount) args+=(--dir /gomodcache) ;;
    type-change) args+=(--proc /gomodcache) ;;   # one mount, a filesystem no bind source can have
    *) args+=(--ro-bind "$modcache" /gomodcache) ;;
  esac
  local p kv
  for p in /bin /lib /lib64 /sbin; do
    if [[ -L "$p" ]]; then args+=(--symlink "$(readlink "$p")" "$p")
    elif [[ -d "$p" ]]; then args+=(--ro-bind "$p" "$p"); fi
  done
  for p in /etc/ld.so.cache /etc/ld.so.conf /etc/ld.so.conf.d /etc/alternatives \
           /etc/passwd /etc/group /etc/nsswitch.conf /etc/localtime; do
    [[ -e "$p" ]] && args+=(--ro-bind "$p" "$p")
  done
  for kv in "${inner_env[@]}"; do args+=(--setenv "${kv%%=*}" "${kv#*=}"); done
  args+=("${mutation_args[@]}")
  # bwrap's own init (PID 1 inside) keeps the environment bwrap was started
  # with, readable through /proc/1/environ: start it with nothing.
  env -i PATH=/usr/bin:/bin "$(command -v bwrap)" "${args[@]}" -- "$@"
}

run_none() {
  # Host environment kept on purpose: this is the "no containment" control.
  (cd "$repo_root" && env "${inner_env[@]/#PATH=*/PATH=$goroot/bin:$PATH}" \
    GOROOT="$goroot" GOCACHE="$gocache" GOMODCACHE="$modcache" HOME="$HOME" \
    TMPDIR=/tmp ADC_TEST_WORK="$work" "$@")
}

probe() { "run_$1" /bin/true >/dev/null 2>&1; }  # the exact configuration, a harmless command

if [[ "$backend" == auto ]]; then
  if command -v bwrap >/dev/null && probe bwrap; then backend=bwrap
  else die "no sandbox backend works: bubblewrap cannot create namespaces on this host; tests refuse to run unsandboxed"
  fi
elif [[ "$backend" != none ]]; then
  probe "$backend" || die "backend $backend cannot start the sandbox on this host"
fi

rc=0
case "$backend" in
  bwrap) run_bwrap "$@" || rc=$? ;;
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
