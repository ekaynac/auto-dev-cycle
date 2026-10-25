# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# The product sandbox's root for an installed cycle case (M1 slice plan
# §2.1 "Isolation", P24b; #226 entries 188, 189): adcd's --sandbox-root,
# a private directory holding Podman's HOME, XDG_*, storage and runroot,
# never the user's own Podman. Its runs/ is a directory on a filesystem of
# its own (/dev/shm), as internal/sandbox's ownDisk requires. Its storage
# holds the profile's pinned image, pulled by digest (a prerequisite,
# outside any task). Podman's storage remembers its runroot, so a root is
# reused whole or not at all: one shared by the cases of one
# `make installed-cycle-stub` (--sandbox-root; the first case pulls into
# it, and each case's adcd sweeps what an earlier one left at its start),
# or the case's own.
#
#   host_arch                   GOARCH of this host (arm64, amd64), or fails
#   rootless_podman PODMAN      fails unless PODMAN is a rootless Podman
#   sandbox_root [DIR]          makes DIR (or a fresh /tmp/adcXXXXXX) a
#                               sandbox root, unless it is one: short
#                               (Podman's runroot, <root>/r, is at most 50
#                               characters), canonical, its runs/ a link to a
#                               directory of /dev/shm; prints the root. A root
#                               it makes records its owner (.owner: the
#                               calling shell's pid and start time), the
#                               shell whose exit removes it (a given DIR is
#                               its caller's, and records none)
#   root_podman ROOT ARGS...    podman ($PODMAN, the caller's) over the root,
#                               as the sandbox runs it
#   root_image ROOT PIN ARCH    pulls PIN into the root unless it holds it,
#                               then prints its instance's id; fails 2 when
#                               that is not linux/ARCH's
#   sandbox_root_remove ROOT    removes the root's containers, its storage,
#                               its runs/ and itself
#   sandbox_root_sweep ROOT     removes the root's containers only
#   sandbox_roots_stale         removes every /tmp/adcXXXXXX root whose owner
#                               is gone (a run killed before its own cleanup:
#                               SIGKILL runs no trap); a root with no .owner,
#                               or a live owner, is never touched
#   work_dirs_stale PARENT...   the same for the drivers' work directories
#                               (PARENT/adc-cycle.*), what still runs on one
#                               stopped first: only the processes it recorded
#                               (record_pid WORK PID)
#   root_canary_path ROOT NAME  canary_path over the root and its runs/ (3:
#                               runs/ unresolved or the search never ran)
#   root_secret_scan ROOT SECRET
#                               no form of SECRET (secrets.sh's forms) in any
#                               file under the root or its runs/, searched in
#                               the root's user namespace: a container's
#                               config and environment (storage/
#                               overlay-containers) and its run files
#                               (runs/, which must resolve: runs_required);
#                               prints each file that holds one and
#                               fails 1; 2: grep failed; 3: runs/ unresolved
#                               or the search never ran
#
# Ownership (the lead's ruling on Mythos #435 M1): a leftover's owner is a
# pid with its start time (.owner, .pids; owner_of), not a locked manifest
# like scripts/test-sandbox.sh's. Several drivers' runs share this host; a
# lock held by no one cannot say whether its owner died or never took it,
# and a bare pid may be reused. Only the pid with the start time it had
# tells a dead owner from a live one, which is what a sweep must know
# before it kills or removes anything (the restart failures of #435, lesson
# 7). The two schemes stay apart on purpose.

host_arch() {
  case "$(uname -m)" in
    aarch64 | arm64) echo arm64 ;;
    x86_64 | amd64) echo amd64 ;;
    *) return 1 ;;
  esac
}

rootless_podman() {
  [[ -x "$1" ]] && [[ "$("$1" info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" == true ]]
}

# owner_of PID: PID's start time (field 22 of /proc/PID/stat), so a reused
# pid is not taken for the owner; nothing when PID is gone.
owner_of() {
  local stat
  stat="$(cat "/proc/$1/stat" 2>/dev/null)" || return 1
  stat="${stat##*) }"
  echo "$1 $(cut -d' ' -f20 <<<"$stat")"
}

sandbox_root() {
  local root="${1:-}" runs owned=0
  if [[ -z "$root" ]]; then
    root="$(mktemp -d /tmp/adcXXXXXX)" || return 1
    owner_of $$ >"$root/.owner" || return 1
    owned=1
  else
    mkdir -p "$root" && chmod 700 "$root" || return 1
  fi
  root="$(realpath -e "$root")" || return 1
  ((${#root} + 2 <= 50)) || { echo "sandbox_root: $root is too long for Podman's runroot" >&2; return 1; }
  if [[ ! -L "$root/runs" ]]; then
    runs="$(mktemp -d /dev/shm/adcrXXXXXX)" || return 1
    if ((owned)); then cp "$root/.owner" "$runs/.owner" || return 1; fi
    ln -s "$runs" "$root/runs" || return 1
  fi
  [[ -n "$(runs_of "$root")" ]] || { echo "sandbox_root: $root/runs is not a directory /dev/shm/adcrXXXXXX" >&2; return 1; }
  echo "$root"
}

# runs_of ROOT: the directory ROOT/runs links to, only when it is one
# sandbox_root makes (/dev/shm/adcrXXXXXX, a directory); else nothing, so
# no removal ever follows the link elsewhere. The cleanup's: permissive.
# The scans use runs_required instead.
runs_of() {
  local runs
  runs="$(readlink "$1/runs" 2>/dev/null)" || return 0
  if [[ "$runs" =~ ^/dev/shm/adcr[A-Za-z0-9]{6}$ && -d "$runs" && ! -L "$runs" ]]; then echo "$runs"; fi
}

# runs_required ROOT: the directory ROOT/runs links to, which every root
# sandbox_root makes has; a link that cannot be read, or names anything but
# a real directory /dev/shm/adcrXXXXXX (a loop, another path), is 3: a scan
# that would leave runs/ out never runs (Astra #441 r2 High 1).
runs_required() {
  local runs
  runs="$(readlink "$1/runs")" || runs=""
  [[ "$runs" =~ ^/dev/shm/adcr[A-Za-z0-9]{6}$ && -d "$runs" && ! -L "$runs" ]] || { echo "runs_required: $1/runs does not resolve to a directory /dev/shm/adcrXXXXXX" >&2; return 3; }
  echo "$runs"
}

# stale DIR: DIR is ours and records an owner (.owner) that is gone.
stale() {
  local owner pid
  [[ -d "$1" && ! -L "$1" && -O "$1" && -f "$1/.owner" ]] || return 1
  owner="$(<"$1/.owner")" pid="${owner%% *}"
  [[ "$pid" =~ ^[0-9]+$ && "$(owner_of "$pid")" != "$owner" ]]
}

sandbox_roots_stale() {
  local r tmp
  for r in /tmp/adc??????; do
    if stale "$r"; then sandbox_root_remove "$r"; fi
  done
  # A runs/ directory whose root is already gone (Opus #435 L2): removed in
  # a scratch root's user namespace (a run's files may be a container's).
  # The scratch root records its owner like any other, so a sweep killed
  # here leaves it to the next one (Opus #435 r2 N1).
  for r in /dev/shm/adcr??????; do
    stale "$r" || continue
    tmp="$(mktemp -d /tmp/adcXXXXXX)" || continue
    owner_of $$ >"$tmp/.owner" || { rm -rf "$tmp"; continue; }
    root_podman "$tmp" unshare rm -rf "$r" 2>/dev/null || rm -rf "$r" 2>/dev/null || true
    root_podman "$tmp" unshare rm -rf "$tmp/storage" "$tmp/r" 2>/dev/null || true
    rm -rf "$tmp"
  done
}

# root_canary_path ROOT NAME: canary_path (base-remote.sh, sourced beside
# this file) over ROOT and its runs/ directory, each run in the root's user
# namespace (a run's files may be a container's): the first path named NAME
# under either, or beside either (their parents, at depth 1), printed, and
# exit 1 when there is one (§2.7's canary_paths: an escape past an
# attempt's directory outlives its teardown; Opus #435 M2; one search,
# Mythos #435 M1). 2: bad input; any other: the search could not run.
root_canary_path() {
  local root="$1" name="$2" runs d rc
  [[ -d "$root" && -n "$name" && "$name" != */* ]] || { echo "root_canary_path: ROOT must exist, NAME be a plain name" >&2; return 2; }
  runs="$(runs_required "$root")" || return 3
  for d in "$root" "$runs"; do
    rc=0
    # shellcheck disable=SC2016 # $1 and $2 are the inner bash's arguments
    root_scanned "$root" "$(declare -f canary_path)"'; canary_path "$1" "$2"' "$d" "$name" </dev/null || rc=$?
    ((rc == 0)) || return "$rc"
  done
}

# root_scanned ROOT SCRIPT ARGS...: runs SCRIPT (bash, its ARGS as $1...) in
# the root's user namespace, its stdin passed on, and returns SCRIPT's own
# status, which a last line of its output reports ("adc-scanned N"); that
# line is removed from the output it prints. A launch that never ran SCRIPT
# (podman unshare failing, no line) is 3, never a finding or a clean search
# (Astra #440 H3).
root_scanned() {
  local root="$1" script="$2" out rc=0 last n
  shift 2
  # SCRIPT in a subshell, so its own exit still reaches the line that reports it.
  # shellcheck disable=SC2016 # the inner bash's own expansions
  out="$(root_podman "$root" unshare bash -c '( '"$script"$'\n'' ); rc=$?; printf "adc-scanned %d\n" "$rc"' -- "$@")" || rc=$?
  last="${out##*$'\n'}"
  [[ $rc == 0 && "$last" =~ ^adc-scanned\ ([0-9]+)$ ]] || { echo "root_scanned: the search did not run in $root (exit $rc)" >&2; return 3; }
  n="${BASH_REMATCH[1]}"
  if [[ "$out" == *$'\n'* ]]; then printf '%s\n' "${out%$'\n'*}"; fi
  return "$n"
}

# root_secret_scan ROOT SECRET (Opus #438 N1): see the header. Devices and
# fifos are skipped (a container's run files may hold one). 1: a file holds
# a form of SECRET (printed); 2: grep failed; 3: the search never ran.
root_secret_scan() {
  local root="$1" secret="$2" runs found rc=0
  [[ -d "$root" && -n "$secret" ]] || { echo "root_secret_scan: ROOT must exist, SECRET be given" >&2; return 2; }
  runs="$(runs_required "$root")" || return 3
  # Inside: grep's "nothing found" (1) is the clean result (0), a match is 1, anything else grep's failure (2).
  # shellcheck disable=SC2016 # the inner bash's own expansions
  found="$(forms "$secret" | root_scanned "$root" 'rc=0; grep -rlaF -D skip -f - -- "$@" || rc=$?; case $rc in 0) exit 1 ;; 1) exit 0 ;; *) exit 2 ;; esac' "$root" "$runs")" || rc=$?
  case "$rc" in
    0) ;;
    1) printf '%s\n' "$found"; return 1 ;;
    *) return "$rc" ;;
  esac
}

# work_dirs_stale PARENT...: removes each PARENT/adc-cycle.* work directory
# whose owner is gone, after stopping what still runs on it: only the
# processes its driver started and recorded (.pids: pid and start time,
# record_pid), each still that process (a killed driver's children outlive
# it); never a process found by a pattern (the host rule: several runs
# share this host).
work_dirs_stale() {
  local p w pid start
  for p in "$@"; do
    for w in "$p"/adc-cycle.*; do
      stale "$w" || continue
      if [[ -f "$w/.pids" ]]; then
        while read -r pid start; do
          [[ "$pid" =~ ^[0-9]+$ && "$(owner_of "$pid")" == "$pid $start" ]] && kill -9 "$pid" 2>/dev/null
        done <"$w/.pids"
      fi
      rm -rf "$w" 2>/dev/null || true
    done
  done
}

# record_pid WORK PID: records PID (with its start time) in WORK/.pids, for
# work_dirs_stale should WORK's driver be killed before its own cleanup.
record_pid() { owner_of "$2" >>"$1/.pids" 2>/dev/null || true; }

root_podman() {
  local root="$1"
  shift
  mkdir -p "$root/home" "$root/xdg" && chmod 700 "$root/home" "$root/xdg" || return 1
  env -i HOME="$root/home" PATH=/usr/bin:/bin XDG_RUNTIME_DIR="$root/xdg" XDG_CONFIG_HOME="$root/home/.config" \
    XDG_DATA_HOME="$root/home/.local/share" XDG_STATE_HOME="$root/home/.local/state" XDG_CACHE_HOME="$root/home/.cache" \
    "$PODMAN" --root "$root/storage" --runroot "$root/r" --cgroup-manager=cgroupfs --events-backend=none "$@"
}

root_image() {
  local root="$1" pin="$2" arch="$3" got
  if ! root_podman "$root" image exists "$pin" 2>/dev/null; then
    root_podman "$root" pull --quiet --arch "$arch" "$pin" >/dev/null || return 1
  fi
  got="$(root_podman "$root" image inspect --format '{{.Os}}/{{.Architecture}} {{.Id}}' "$pin")" || return 1
  [[ "$got" == "linux/$arch "* ]] || return 2
  echo "${got#* }"
}

sandbox_root_sweep() {
  [[ -d "$1/storage" ]] || return 0
  root_podman "$1" rm --all --force --time 0 >/dev/null 2>&1 || true
}

sandbox_root_remove() {
  local root="$1" runs
  [[ -d "$root" ]] || return 0
  runs="$(runs_of "$root")"
  if [[ -d "$root/storage" ]]; then
    sandbox_root_sweep "$root"
    root_podman "$root" system reset --force >/dev/null 2>&1 || true
    root_podman "$root" unshare rm -rf "$root/storage" "$root/r" ${runs:+"$runs"} 2>/dev/null || true
  fi
  [[ -z "$runs" ]] || rm -rf "$runs" 2>/dev/null || true
  rm -rf "$root"
}
