#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# make dist's lib/adc for each server platform (docs/design/m0/15 S2): the pinned Node (dist-node.sh) and the
# fake adapter with py-mini (dist-adapters.sh). Any failure fails it. Then, for every platform, bin/node must
# match its pin (by sum; nothing runs), modes are normalised (u=rwX,go=rX) and lib/adc/SHA256SUMS lists every
# regular file in it, for install-check to verify.
#
# Usage: scripts/dist-lib.sh [--pins FILE] DISTDIR PLATFORM...
set -euo pipefail

die() { echo "dist-lib: $*" >&2; exit 1; }

here="$(cd "$(dirname "$0")" && pwd)"
pins=()
if [[ "${1:-}" == "--pins" ]]; then pins=(--pins "$2"); shift 2; fi
[[ $# -ge 2 ]] || die "usage: dist-lib.sh [--pins FILE] DISTDIR PLATFORM..."
dist="$1"
shift
for p in "$@"; do
  lib="$dist/${p%/*}-${p#*/}/lib/adc"
  "$here/dist-node.sh" "${pins[@]}" "$p" "$lib/node" || die "packaging node for $p failed"
  "$here/dist-adapters.sh" "$lib" || die "packaging the adapters for $p failed"
done
for p in "$@"; do
  lib="$dist/${p%/*}-${p#*/}/lib/adc"
  "$here/dist-node.sh" "${pins[@]}" --verify "$p" "$lib/node" || die "$p: bin/node does not match its pin"
  chmod -R u=rwX,go=rX -- "$lib"
  (cd "$lib" && find . -type f ! -path ./SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >SHA256SUMS) ||
    die "$p: writing SHA256SUMS failed"
done
