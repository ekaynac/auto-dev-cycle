#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Packages the fake adapter and the http-chat adapter (P13) with their workspace dependencies,
# fixtures/t0/py-mini and go-mini, the parts of fixtures/m1 the installed cycle harness runs (the profiles, groups.json and
# registry-stub.json, the stub provider's server.mjs and scenarios, the cases and stub/), the reviewer qualification's suite
# and records with the adapters workspace's manifests and lockfile (qualify.Load's root: adcd's start checks the review
# route's qualification against them; no run mounts lib/adc) and the harness itself (P24:
# scripts/installed-cycle.sh and scripts/lib), into OUTDIR (dist's lib/adc; docs/design/m0/15 S2). The repository's own files are its committed content (git archive HEAD: never
# an untracked .env or .venv, nor an uncommitted edit), with its LICENSE and NOTICE. The workspace packages keep their source paths
# (adapters/fake, adapters/http-chat, adapters/contract, gen/ts): Node strips TypeScript types only outside node_modules, so
# node_modules/@adc holds two relative symlinks to them. The third-party packages are the adapters' production
# closure as the lockfile installed it (make ts-deps), from a store pnpm finds unmodified, copied into
# node_modules; each must carry a licence file, declare a licence on the allowlist and hold no nested
# node_modules (which would shadow the flat layout). Nothing else.
#
# Usage: scripts/dist-adapters.sh OUTDIR
set -euo pipefail

die() { echo "dist-adapters: $*" >&2; exit 1; }

[[ $# -eq 1 ]] || die "usage: dist-adapters.sh OUTDIR"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$1"
git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "$root is not a git checkout"
[[ -d "$root/adapters/node_modules" ]] || die "the adapters workspace is not installed (make ts-deps)"
[[ ! -e "$out/adapters" && ! -e "$out/node_modules" ]] || die "$out already holds adapters"
(cd "$root/adapters" && pnpm store status >/dev/null) || die "pnpm store status failed: the store may hold modified packages"

mkdir -p -- "$out/node_modules/@adc"
committed=(LICENSE NOTICE adapters/fake/package.json adapters/fake/src adapters/http-chat/package.json adapters/http-chat/src
  adapters/contract/package.json adapters/contract/src
  gen/ts/package.json gen/ts/adapter-validators.cjs gen/ts/formats.cjs fixtures/t0/py-mini
  fixtures/t0/go-mini fixtures/m1/profiles fixtures/m1/config/groups.json fixtures/m1/config/registry-stub.json
  fixtures/m1/stub-provider/server.mjs fixtures/m1/stub-provider/scenarios fixtures/m1/cases fixtures/m1/stub
  fixtures/m1/review-qualification adapters/package.json adapters/pnpm-workspace.yaml adapters/pnpm-lock.yaml
  scripts/installed-cycle.sh scripts/lib)
for p in "${committed[@]}"; do
  git -C "$root" cat-file -e "HEAD:$p" 2>/dev/null || die "$p is not committed"
done
git -C "$root" archive --format=tar HEAD -- "${committed[@]}" | tar -C "$out" -xf - ||
  die "copying the committed files failed"
for f in LICENSE NOTICE adapters/fake/src/main.ts adapters/http-chat/src/main.ts adapters/contract/src/index.ts gen/ts/adapter-validators.cjs; do
  [[ -f "$out/$f" ]] || die "$f is missing"
done
[[ -f "$out/fixtures/t0/py-mini/tasks/py-mini-feature-1/task.json" ]] || die "py-mini-feature-1 is missing"
for f in fixtures/t0/go-mini/tasks/go-mini-feature-1/task.json fixtures/m1/stub-provider/server.mjs fixtures/m1/profiles/go-mini.json \
  fixtures/m1/review-qualification/suite.json adapters/pnpm-lock.yaml \
  scripts/installed-cycle.sh scripts/lib/secrets.sh scripts/lib/base-remote.sh; do
  [[ -f "$out/$f" ]] || die "$f is missing"
done
ln -s ../../adapters/contract "$out/node_modules/@adc/contract"
ln -s ../../gen/ts "$out/node_modules/@adc/contract-gen"

deps="$(cd "$root/adapters" && pnpm --filter @adc/fake --filter @adc/http-chat list --prod --depth Infinity --parseable)" || die "pnpm list failed"
third=0
while IFS= read -r d; do
  case "$d" in
    "$root/adapters/fake" | "$root/adapters/http-chat" | "$root/adapters/contract" | "$root/gen/ts") continue ;;
    "$root/adapters/node_modules/.pnpm/"*) ;;
    *) die "an unexpected production dependency: $d" ;;
  esac
  name="${d##*/node_modules/}"
  [[ "$name" =~ ^(@[a-z0-9_-][a-z0-9._-]*/)?[a-z0-9_-][a-z0-9._-]*$ && "$name" != @adc/* ]] || die "an unexpected package name: $name"
  [[ -n "$(find "$d" -maxdepth 1 -type f -iregex '.*/licen[cs]e[^/]*')" ]] || die "$name carries no licence file"
  licence="$(node -p 'require(process.argv[1]).license' "$d/package.json")" || die "$name has no readable package.json"
  case "$licence" in
    MIT | BSD-2-Clause | BSD-3-Clause | ISC | Apache-2.0) ;;
    *) die "$name declares the licence $licence, which is not on the allowlist" ;;
  esac
  [[ -z "$(find "$d" -mindepth 1 -name node_modules -print -quit)" ]] || die "$name holds a nested node_modules"
  [[ ! -e "$out/node_modules/$name" ]] || die "$name is listed twice"
  mkdir -p -- "$(dirname -- "$out/node_modules/$name")"
  cp -R -- "$d" "$out/node_modules/$name"
  third=$((third + 1))
done <<<"$deps"
((third > 0)) || die "pnpm listed no third-party dependency: is the workspace installed?"

# The two @adc links and hostile-symlink's base link (P24c: a link to the
# decoy beside it, inside its case) are the only symlinks packaged.
case_link=./fixtures/m1/cases/hostile-symlink/testdata/docs/key
[[ ! -L "$out/$case_link" || "$(readlink "$out/$case_link")" == ../decoy/key ]] || die "$case_link does not point to ../decoy/key"
links="$(cd "$out" && find . -type l ! -path "$case_link" | sort)"
[[ "$links" == $'./node_modules/@adc/contract\n./node_modules/@adc/contract-gen' ]] || die "unexpected symlinks: $links"
echo "dist-adapters: the fake and http-chat adapters, $third third-party packages, py-mini, go-mini, fixtures/m1 and the cycle harness into $out"
