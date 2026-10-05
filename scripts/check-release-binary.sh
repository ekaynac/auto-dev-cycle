#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Refuse a release binary built with the adcmutation tag (internal/mutation):
# only the separate mutation build may carry live mutation sites. It reads
# the build settings Go records in the binary, so it holds whatever set the
# tag (a flag, GOFLAGS, a persisted go env file).
#
# Usage: scripts/check-release-binary.sh GO FILE...
set -euo pipefail

die() { echo "check-release-binary: $*" >&2; exit 1; }

[[ $# -ge 2 ]] || die "usage: check-release-binary.sh GO FILE..."
go="$1"
shift
for f in "$@"; do
  info="$("$go" version -m "$f")" || die "$f: no Go build information"
  # A settings line is "<tab>build<tab>-tags=a,b".
  tags="$(awk -F'\t' '$2 == "build" && $3 ~ /^-tags=/ { sub(/^-tags=/, "", $3); print $3 }' <<<"$info")"
  if [[ ",$tags," == *",adcmutation,"* ]]; then
    die "$f was built with the adcmutation tag; release builds never carry mutation sites"
  fi
done
