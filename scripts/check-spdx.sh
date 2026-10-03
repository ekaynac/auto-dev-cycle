#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fails if a tracked source file lacks the Apache-2.0 SPDX header in its first
# five lines (docs/design/m0/01 "Licence and headers").
#
# Usage: scripts/check-spdx.sh [FILE]...   (default: every tracked source file)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
header='SPDX-License-Identifier: Apache-2.0'
source_re='(\.(go|sh|mjs|cjs|js|ts|ya?ml)|(^|/)Makefile)$'

if [[ $# -gt 0 ]]; then
  files=("$@")
else
  mapfile -t files < <(git -C "$repo_root" ls-files | grep -E "$source_re")
  files=("${files[@]/#/$repo_root/}")
fi

missing=0
for f in "${files[@]}"; do
  if ! head -n 5 -- "$f" | grep -qF "$header"; then
    printf 'check-spdx: missing "%s" in the first five lines: %s\n' "$header" "${f#"$repo_root"/}" >&2
    missing=$((missing + 1))
  fi
done
if [[ "$missing" -gt 0 ]]; then
  printf 'check-spdx: %d file(s) without the header\n' "$missing" >&2
  exit 1
fi
printf 'check-spdx: %d file(s) carry the header\n' "${#files[@]}"
