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
#
# The clean shell (scripts/lib/clean-shell.sh: the drivers' one prelude),
# first, before any helper, tracing off before anything is traced; its path
# by parameter expansion alone.
set +o xtrace +o verbose
clean_shell_tag=installed-cycle-set
adc_here="${BASH_SOURCE[0]%/*}"
[[ "$adc_here" != "${BASH_SOURCE[0]}" ]] || adc_here=.
# shellcheck source=scripts/lib/clean-shell.sh
. "$adc_here/lib/clean-shell.sh"
unset adc_here
set -euo pipefail

die() { echo "installed-cycle-set: $*" >&2; exit 1; }
[[ $# -ge 2 && ( "$1" == setup || "$1" == run ) ]] || { echo "usage: installed-cycle-set.sh setup|run PREFIX CASE..." >&2; exit 64; }
mode="$1" prefix="$(cd "$2" && pwd)"
shift 2
driver="$prefix/lib/adc/scripts/installed-cycle.sh"
[[ -x "$driver" ]] || die "$driver is not installed"
SECRETS_TAG=installed-cycle-set
# shellcheck source=scripts/lib/secrets.sh
. "$prefix/lib/adc/scripts/lib/secrets.sh"
cwd="$(mktemp -d)"
root=""
PODMAN=/usr/bin/podman
# shellcheck source=scripts/lib/sandbox-root.sh
. "$prefix/lib/adc/scripts/lib/sandbox-root.sh"
# cleanup's inputs: root cwd (both set above, before its trap; Opus #441 M2)
# shellcheck disable=SC2317,SC2329  # invoked only by the EXIT trap
cleanup() {
  if [[ -n "$root" ]]; then sandbox_root_remove "$root"; fi
  rm -rf "$cwd"
}
trap cleanup EXIT
# A signal exits through the EXIT trap, so the set's root is removed then too.
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
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

# The set's one sandbox root, owned by this shell; first, the roots of runs
# killed before their own cleanup (SIGKILL runs no trap).
sandbox_roots_stale
root="$(sandbox_root "")" || die "the sandbox root cannot be made"
rows=() failed=0
for c in "$@"; do
  rc=0 t0=$SECONDS
  cycle --case "$c" --expect candidate --sandbox-root "$root" >"$cwd/$c.out" 2>&1 || rc=$?
  wall=$((SECONDS - t0))
  case "$rc" in
    0) rows+=("$(sed -n 's/^installed-cycle: summary: //p' "$cwd/$c.out") ${wall}s |") ;;
    2)
      grep 'could_not_run' "$cwd/$c.out" >&2 || tail -n 3 "$cwd/$c.out" >&2
      echo "installed-cycle-set: the stub set cannot run on this host: $c refused (exit 2)" >&2
      exit 2
      ;;
    *)
      failed=1
      echo "installed-cycle-set: $c failed (exit $rc); its last lines:" >&2
      tail -n 15 "$cwd/$c.out" >&2 || true
      rows+=("| $c | FAILED (exit $rc) | $({ grep -m 3 -E '^fail:|^installed-cycle: [a-z]' "$cwd/$c.out" || true; } | tr '\n' ' ' | cut -c1-300) | | | ${wall}s |")
      ;;
  esac
done
echo "| case | end | results | tokens in/out | requests builder/reviewer | wall |"
echo "|---|---|---|---|---|---|"
printf '%s\n' "${rows[@]}"
exit "$failed"
