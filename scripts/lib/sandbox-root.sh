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
#                               stopped first

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
  local root="${1:-}" runs
  if [[ -z "$root" ]]; then
    root="$(mktemp -d /tmp/adcXXXXXX)" || return 1
    owner_of $$ >"$root/.owner" || return 1
  else
    mkdir -p "$root" && chmod 700 "$root" || return 1
  fi
  root="$(realpath -e "$root")" || return 1
  ((${#root} + 2 <= 50)) || { echo "sandbox_root: $root is too long for Podman's runroot" >&2; return 1; }
  if [[ ! -L "$root/runs" ]]; then
    runs="$(mktemp -d /dev/shm/adcrXXXXXX)" || return 1
    ln -s "$runs" "$root/runs" || return 1
  fi
  [[ -d "$root/runs/" ]] || { echo "sandbox_root: $root/runs is dangling" >&2; return 1; }
  echo "$root"
}

# stale DIR: DIR is ours and records an owner (.owner) that is gone.
stale() {
  local owner pid
  [[ -d "$1" && ! -L "$1" && -O "$1" && -f "$1/.owner" ]] || return 1
  owner="$(<"$1/.owner")" pid="${owner%% *}"
  [[ "$pid" =~ ^[0-9]+$ && "$(owner_of "$pid")" != "$owner" ]]
}

sandbox_roots_stale() {
  local r
  for r in /tmp/adc??????; do
    if stale "$r"; then sandbox_root_remove "$r"; fi
  done
}

# work_dirs_stale PARENT...: removes each PARENT/adc-cycle.* work directory
# whose owner is gone, after stopping what still runs on it (an adcd whose
# state directory, or a stub provider whose log, lies under it: a killed
# driver's children outlive it).
work_dirs_stale() {
  local p w c
  for p in "$@"; do
    for w in "$p"/adc-cycle.*; do
      stale "$w" || continue
      for c in /proc/[0-9]*/cmdline; do # each of our processes whose command line names a path under it
        [[ -O "$c" ]] && tr '\0' ' ' <"$c" 2>/dev/null | grep -qF -- "$w/" && kill -9 "$(basename "$(dirname "$c")")" 2>/dev/null
      done
      rm -rf "$w" 2>/dev/null || "$PODMAN" unshare rm -rf "$w" 2>/dev/null || true
    done
  done
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
  runs="$(readlink "$root/runs" 2>/dev/null)" || runs=""
  if [[ -d "$root/storage" ]]; then
    sandbox_root_sweep "$root"
    root_podman "$root" system reset --force >/dev/null 2>&1 || true
    root_podman "$root" unshare rm -rf "$root/storage" "$root/r" ${runs:+"$runs"} 2>/dev/null || true
  fi
  [[ -z "$runs" ]] || rm -rf "$runs" 2>/dev/null || true
  rm -rf "$root"
}
