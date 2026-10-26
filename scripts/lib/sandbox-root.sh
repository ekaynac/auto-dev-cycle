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
#                               calling shell's pid, start time and PID
#                               namespace), the
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
#                               no form of SECRET in any file under the root
#                               or its runs/ (adc dev scan, secrets.sh's
#                               adc_scan), searched in the root's user
#                               namespace: a container's config and
#                               environment (storage/overlay-containers)
#                               and its run files (runs/, which must
#                               resolve: runs_required); prints each file
#                               that holds one and fails 1; 2: the search
#                               did not complete; 3: runs/ unresolved or the
#                               search never ran
#
# Ownership (the lead's ruling on Mythos #435 M1): a leftover's owner is a
# pid with its start time and PID namespace (.owner, .pids; owner_of), not a
# locked manifest
# like scripts/test-sandbox.sh's. Several drivers' runs share this host; a
# lock held by no one cannot say whether its owner died or never took it,
# and a bare pid may be reused. Only the pid with the start time it had
# tells a dead owner from a live one, which is what a sweep must know
# before it kills or removes anything (the restart failures of #435, lesson
# 7). The two schemes stay apart on purpose. A pid means something only in
# its own PID namespace: a sweep from another one (a bwrap or a container
# sharing /tmp or /dev/shm) never reads its record as gone (Opus #443 r2 B1).

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
# pid is not taken for the owner, and its PID namespace, so a pid of another
# namespace is never taken for one of this; nothing, and 1, unless all were
# read.
owner_of() {
  local stat start ns
  stat="$(cat "/proc/$1/stat" 2>/dev/null)" || return 1
  stat="${stat##*) }"
  # The start time read, and numeric, or no owner at all (Opus #443 H1a).
  start="$(cut -d' ' -f20 <<<"$stat")" || return 1
  [[ "$start" =~ ^[0-9]+$ ]] || return 1
  ns="$(readlink "/proc/$1/ns/pid")" || return 1
  [[ "$ns" =~ ^pid:\[[0-9]+\]$ ]] || return 1
  echo "$1 $start $ns"
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
  runs="$(runs_of "$root")" || return 1
  [[ -n "$runs" ]] || { echo "sandbox_root: $root/runs is not a directory /dev/shm/adcrXXXXXX" >&2; return 1; }
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

# runs_required ROOT: runs_of ROOT, which every root sandbox_root makes
# resolves; nothing (a link that cannot be read, a loop, another path) is 3:
# a scan that would leave runs/ out never runs (Astra #441 r2 High 1; one
# validator, Astra #441 r3 M4).
runs_required() {
  local runs
  runs="$(runs_of "$1")" || return 3
  [[ -n "$runs" ]] || { echo "runs_required: $1/runs does not resolve to a directory /dev/shm/adcrXXXXXX" >&2; return 3; }
  echo "$runs"
}

# stale DIR: DIR is ours and records an owner (.owner) that is gone.
stale() {
  local owner pid ns here now
  [[ -d "$1" && ! -L "$1" && -O "$1" && -f "$1/.owner" ]] || return 1
  # Stale only on evidence (Opus #443 H1a): a well-formed record whose pid is
  # gone, or whose start time was read and differs. A record that cannot be
  # read or is malformed, or a start time that cannot be read, keeps it.
  # Readable first: an unreadable file in $(<…) ends the whole shell.
  [[ -r "$1/.owner" ]] || return 1
  owner="$(<"$1/.owner")" || return 1
  [[ "$owner" =~ ^([0-9]+)\ [0-9]+\ (pid:\[[0-9]+\])$ ]] || return 1
  pid="${BASH_REMATCH[1]}" ns="${BASH_REMATCH[2]}"
  # Only a record of this PID namespace says anything here: another's pid
  # is absent from this /proc, or another process (Opus #443 r2 B1).
  here="$(readlink /proc/self/ns/pid)" || return 1
  [[ "$ns" == "$here" ]] || return 1
  [[ -e "/proc/$pid" ]] || return 0
  now="$(owner_of "$pid")" || return 1
  [[ "$now" != "$owner" ]]
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

# root_canary_path ROOT NAME: canary_path's two name searches (adc dev scan
# --name) over ROOT and its runs/ directory, each run in the root's user
# namespace (a run's files may be a container's): every path named NAME
# under either, or beside either (their parents, at depth 1), printed, and
# exit 1 when there is one (§2.7's canary_paths: an escape past an
# attempt's directory outlives its teardown; Opus #435 M2; one search,
# Mythos #435 M1). 2: bad input; any other: the search could not run.
root_canary_path() {
  local root="$1" name="$2" runs d rc
  [[ -d "$root" && -n "$name" && "$name" != */* ]] || { echo "root_canary_path: ROOT must exist, NAME be a plain name" >&2; return 2; }
  [[ "${adc_scan:-}" == /* && -x "${adc_scan:-}" ]] || { echo "root_canary_path: no scan program (adc_scan)" >&2; return 3; }
  runs="$(runs_required "$root")" || return 3
  for d in "$root" "$runs"; do
    rc=0
    # Under D, then beside it; the first that finds or fails ends the script with its status.
    # shellcheck disable=SC2016 # $1, $2 and $3 are the inner bash's arguments
    root_scanned "$root" '"$1" dev scan --name "$2" -- "$3" && "$1" dev scan --name "$2" --depth 1 -- "${3%/*}"' "$adc_scan" "$name" "$d" </dev/null || rc=$?
    case "$rc" in
      0) ;;
      1) return 1 ;;
      *) return 3 ;;
    esac
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

# root_secret_scan ROOT SECRET (Opus #438 N1): see the header. The secret
# reaches adc dev scan on its stdin (through podman unshare). Its walk
# follows no link (a container's layers hold absolute and dangling ones:
# each is recorded, never resolved past the check against an OWN, which a
# root has none of), refuses what it cannot read, and reads no device,
# fifo or socket (a container's run files may hold one). 1: a file holds a
# form of SECRET (printed); 2: the search did not complete; 3: it never
# ran.
root_secret_scan() {
  local root="$1" secret="$2" runs found rc=0
  [[ -d "$root" && -n "$secret" ]] || { echo "root_secret_scan: ROOT must exist, SECRET be given" >&2; return 2; }
  [[ "${adc_scan:-}" == /* && -x "${adc_scan:-}" ]] || { echo "root_secret_scan: no scan program (adc_scan)" >&2; return 2; }
  runs="$(runs_required "$root")" || return 3
  # shellcheck disable=SC2016 # the inner bash's own expansions
  found="$(printf '%s\n' "$secret" | root_scanned "$root" '"$@"' "$adc_scan" dev scan --secrets - -- "$root" "$runs")" || rc=$?
  case "$rc" in
    0) ;;
    1) printf '%s\n' "$found"; return 1 ;;
    3) return 3 ;;
    *) return 2 ;;
  esac
}

# work_dirs_stale PARENT...: removes each PARENT/adc-cycle.* work directory
# whose owner is gone, after stopping what still runs on it: only the
# processes its driver started and recorded (.pids: owner_of's lines,
# record_pid), each still that process (a killed driver's children outlive
# it); never a process found by a pattern (the host rule: several runs
# share this host).
work_dirs_stale() {
  local p w rec pid now
  for p in "$@"; do
    for w in "$p"/adc-cycle.*; do
      stale "$w" || continue
      if [[ -f "$w/.pids" ]]; then
        while read -r rec; do
          [[ "$rec" =~ ^([0-9]+)\ [0-9]+\ pid:\[[0-9]+\]$ ]] || continue
          pid="${BASH_REMATCH[1]}"
          # A record that cannot be read now is not that process: never killed.
          now="$(owner_of "$pid")" || continue
          [[ "$now" == "$rec" ]] && kill -9 "$pid" 2>/dev/null
        done <"$w/.pids"
      fi
      rm -rf "$w" 2>/dev/null || true
    done
  done
}

# record_pid WORK PID: records PID (owner_of's line) in WORK/.pids, for
# work_dirs_stale should WORK's driver be killed before its own cleanup. A
# PID already gone has nothing to record; one whose start time cannot be
# read fails it.
record_pid() {
  local rec
  [[ -e "/proc/$2" ]] || return 0
  rec="$(owner_of "$2")" || return 1
  # The write alone is best effort: .pids serves only a later sweep, after
  # this run is killed without its cleanup (SIGKILL); a line lost leaves
  # that process to end by itself (Opus #443 L4).
  printf '%s\n' "$rec" >>"$1/.pids" 2>/dev/null || true
}

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
  # runs_of never fails (nothing, when runs/ does not resolve); a removal has nothing to refuse.
  runs="$(runs_of "$root")" || runs=""
  if [[ -d "$root/storage" ]]; then
    sandbox_root_sweep "$root"
    root_podman "$root" system reset --force >/dev/null 2>&1 || true
    root_podman "$root" unshare rm -rf "$root/storage" "$root/r" ${runs:+"$runs"} 2>/dev/null || true
  fi
  [[ -z "$runs" ]] || rm -rf "$runs" 2>/dev/null || true
  rm -rf "$root"
}
