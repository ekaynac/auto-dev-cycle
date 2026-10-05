#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The contract-fixture row of the M0 exit check, on an installed adc (after
# scripts/install-check.sh): run `adc dev fixtures --contract` in isolation,
# with only the prefix (at /p) and a private /tmp mounted, so no source tree,
# home or network is reachable (the binaries are static). The fixtures are
# green, and stay green under every named mutation, because a release build
# has no live mutation sites.
#
# Usage: scripts/installed-fixtures.sh PREFIX
set -euo pipefail

die() { echo "installed-fixtures: $*" >&2; exit 1; }

[[ $# -eq 1 ]] || die "usage: installed-fixtures.sh PREFIX"
prefix="$(cd "$1" && pwd)"
[[ -x "$prefix/bin/adc" ]] || die "$prefix/bin/adc is not installed"
command -v bwrap >/dev/null || die "bubblewrap (bwrap) is required to run the installed fixtures in isolation"

isolated() { # [--setenv NAME VALUE]... CMD...
  local opts=()
  while [[ "$1" == --setenv ]]; do opts+=("$1" "$2" "$3"); shift 3; done
  env -i PATH=/usr/bin:/bin bwrap --unshare-all --die-with-parent --ro-bind "$prefix" /p --tmpfs /tmp --proc /proc --dev /dev \
    --chdir /tmp --setenv HOME /tmp --setenv TMPDIR /tmp "${opts[@]}" -- "$@"
}

isolated /p/bin/adc dev fixtures --contract >/dev/null || die "the installed adc's contract fixtures are not green"
list="$(isolated /p/bin/adc dev fixtures --contract --list)" || die "the installed adc cannot list its contract fixtures"
[[ -n "$list" ]] || die "the installed adc lists no contract fixtures"
n=0
while read -r name mut; do
  isolated --setenv ADC_MUTATION "$mut" /p/bin/adc dev fixtures --contract --only "$name" >/dev/null \
    || die "the installed adc's fixture $name is not green under $mut; a release build must have no live mutation sites"
  n=$((n + 1))
done <<<"$list"
echo "installed-fixtures: $n contract fixtures green in isolation, and under each one's named mutation"
