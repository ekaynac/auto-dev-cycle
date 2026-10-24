#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The installed cycle harness's stub set (M1 slice plan §2.3, §2.6; P24b;
# #226 entries 188, 189), run by make from outside the checkout over an
# installed prefix:
#
#   installed-cycle-set.sh setup PREFIX CASE...
#       make ci's rows, which need no Podman: the preflight case; each CASE's
#       setup (--setup-only: its base remote and its profile); and the
#       refusal: a case whose prerequisite is missing (--podman naming no
#       Podman) exits 2 and names it.
#   installed-cycle-set.sh run PREFIX CASE...
#       the full set: the preflight case, then each CASE until the candidate
#       (--expect candidate), all over one sandbox root (the pinned image
#       pulled into it once, by the first case). A missing prerequisite
#       (rootless Podman, the pinned image for this host's platform) refuses
#       the set, exit 2, naming it, before any case runs past its setup.
#       It prints one summary row per case (a Markdown table) and exits 1
#       when any case failed.
set -euo pipefail

die() { echo "installed-cycle-set: $*" >&2; exit 1; }
[[ $# -ge 2 && ( "$1" == setup || "$1" == run ) ]] || { echo "usage: installed-cycle-set.sh setup|run PREFIX CASE..." >&2; exit 64; }
mode="$1" prefix="$(cd "$2" && pwd)"
shift 2
driver="$prefix/lib/adc/scripts/installed-cycle.sh"
[[ -x "$driver" ]] || die "$driver is not installed"
cwd="$(mktemp -d)"
root=""
# shellcheck disable=SC2317,SC2329  # invoked only by the EXIT trap
cleanup() {
  if [[ -n "$root" ]]; then
    # shellcheck source=scripts/lib/sandbox-root.sh
    PODMAN=/usr/bin/podman && . "$prefix/lib/adc/scripts/lib/sandbox-root.sh" && sandbox_root_remove "$root"
  fi
  rm -rf "$cwd"
}
trap cleanup EXIT
cycle() { (cd "$cwd" && "$driver" "$prefix" --routes stub --project go-mini "$@"); }

cycle --case preflight >"$cwd/preflight.out" 2>&1 || { tail -n 5 "$cwd/preflight.out" >&2; die "preflight failed"; }
echo "installed-cycle-set: preflight ok"

if [[ "$mode" == setup ]]; then
  for c in "$@"; do
    cycle --case "$c" --expect candidate --setup-only >"$cwd/$c.out" 2>&1 || { tail -n 5 "$cwd/$c.out" >&2; die "$c: its setup failed"; }
    echo "installed-cycle-set: $c: setup checked"
  done
  rc=0
  cycle --case candidate --expect candidate --podman /nonexistent/podman >"$cwd/refusal.out" 2>&1 || rc=$?
  if ((rc != 2)) || ! grep -q '^installed-cycle: could_not_run: rootless Podman (/nonexistent/podman) is required' "$cwd/refusal.out"; then
    tail -n 5 "$cwd/refusal.out" >&2
    die "a case without its Podman exited $rc, not 2 naming it"
  fi
  echo "installed-cycle-set: a missing prerequisite refuses the case, exit 2, named"
  exit 0
fi

root="$(mktemp -d /tmp/adcXXXXXX)"
rows=() failed=0
for c in "$@"; do
  rc=0
  cycle --case "$c" --expect candidate --sandbox-root "$root" >"$cwd/$c.out" 2>&1 || rc=$?
  case "$rc" in
    0) rows+=("$(sed -n 's/^installed-cycle: summary: //p' "$cwd/$c.out")") ;;
    2)
      grep 'could_not_run' "$cwd/$c.out" >&2 || tail -n 3 "$cwd/$c.out" >&2
      echo "installed-cycle-set: the stub set cannot run on this host: $c refused (exit 2)" >&2
      exit 2
      ;;
    *)
      failed=1
      rows+=("| $c | FAILED (exit $rc) | $(grep -m 3 -E '^fail:|^installed-cycle: [a-z]' "$cwd/$c.out" | grep -v ": $c: " | tr '\n' ' ' | cut -c1-300) | | | |")
      ;;
  esac
done
echo "| case | end | results | tokens in/out | requests builder/reviewer | seconds |"
echo "|---|---|---|---|---|---|"
printf '%s\n' "${rows[@]}"
exit "$failed"
