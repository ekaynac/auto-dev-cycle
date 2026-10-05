#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fails if a tracked source file lacks the Apache-2.0 SPDX header, as a comment
# line in its first five lines (docs/design/m0/01 "Licence and headers").
# Covered: .go .sh .bash .mjs .cjs .js .ts .py .php .toml .yml .yaml .mk,
# Makefile, Dockerfile, Containerfile. JSON and Markdown cannot or need not
# carry comments and are not covered, nor is pnpm-lock.yaml, which pnpm
# rewrites whole.
#
# Usage: scripts/check-spdx.sh [FILE]...   (default: every tracked source file)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# A comment marker (# // /* * <!-- --) then the identifier, alone on the line.
header_re='^[[:space:]]*(#|//|/\*|\*|<!--|--)[[:space:]]*SPDX-License-Identifier: Apache-2\.0[[:space:]]*(\*/|-->)?[[:space:]]*$'
source_re='(\.(go|sh|bash|mjs|cjs|js|ts|py|php|toml|ya?ml|mk)|(^|/)(Makefile|Dockerfile|Containerfile))$'

files=()
if [[ $# -gt 0 ]]; then
  files=("$@")
else
  # -z: no quoting of unusual names; a failing git must fail the check.
  listing="$(mktemp)"
  trap 'rm -f "$listing"' EXIT
  git -C "$repo_root" ls-files -z >"$listing" || { echo "check-spdx: git ls-files failed" >&2; exit 2; }
  while IFS= read -r -d '' f; do
    [[ "$f" =~ $source_re ]] || continue
    [[ "$f" =~ (^|/)pnpm-lock\.yaml$ ]] && continue
    [[ -e "$repo_root/$f" ]] || continue # tracked but deleted in the working tree
    files+=("$repo_root/$f")
  done <"$listing"
  [[ ${#files[@]} -gt 0 ]] || { echo "check-spdx: no source files selected; refusing to pass on nothing" >&2; exit 2; }
fi

missing=0
for f in "${files[@]}"; do
  # Read the lines first: with pipefail, grep -q stopping early would kill
  # head with SIGPIPE on a long line and fail a file that has the header.
  top="$(head -n 5 -- "$f")"
  if ! grep -qE "$header_re" <<<"$top"; then
    printf 'check-spdx: no "SPDX-License-Identifier: Apache-2.0" comment line in the first five lines: %s\n' "${f#"$repo_root"/}" >&2
    missing=$((missing + 1))
  fi
done
if [[ "$missing" -gt 0 ]]; then
  printf 'check-spdx: %d file(s) without the header\n' "$missing" >&2
  exit 1
fi
printf 'check-spdx: %d file(s) carry the header\n' "${#files[@]}"
