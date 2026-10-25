#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The clean-install row of the M0 exit check (docs/design/m0/00): install
# adcd, adc, adc-harness and adc-agent for this host from dist/ into an empty prefix,
# then run the installed binaries outside the source tree, with a scrubbed
# environment, and check what they report: each its own name, this
# platform, one commit, and one schema-set digest (equal to --expect when
# given). lib/adc (15 S2) is installed too: the pinned Node, whose bin/node
# is re-verified against its pin on the installed file and must report the
# pinned version, the fake adapter, which must load with it, the http-chat
# adapter, which must answer describe with it, py-mini, and the reviewer
# qualification's suite and the adapters workspace files its adapter
# digest covers (adcd's start reads them: qualify.Reviewers).
#
# Usage: scripts/install-check.sh [--dist DIR] [--expect sha256:HEX] [--node-pins FILE] PREFIX
set -euo pipefail

die() { echo "install-check: $*" >&2; exit 1; }

dist="dist"
pins="$(cd "$(dirname "$0")" && pwd)/node-pins"
expect=""
expect_given=""
while [[ $# -gt 1 ]]; do
  case "$1" in
    --dist) dist="$2"; shift 2 ;;
    --expect) expect="$2"; expect_given=1; shift 2 ;;
    --node-pins) pins="$2"; shift 2 ;;
    *) die "unknown argument $1" ;;
  esac
done
[[ $# -eq 1 ]] || die "usage: install-check.sh [--dist DIR] [--expect sha256:HEX] [--node-pins FILE] PREFIX"
prefix="$1"
# An empty or malformed --expect (a failed lookup upstream) is an error, not
# "nothing to compare".
if [[ -n "$expect_given" && ! "$expect" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  die "--expect '$expect' is not a sha256: schema-set digest"
fi

case "$(uname -s)" in Linux) os=linux ;; *) die "unsupported OS $(uname -s)" ;; esac
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; *) die "unsupported architecture $(uname -m)" ;; esac
platform="$os/$arch"
src="$dist/$os-$arch"
[[ -d "$src" ]] || die "no build for $platform in $src (run make dist)"

# The prefix is new or empty: nothing already there may answer for us.
if [[ -e "$prefix" ]]; then
  [[ -d "$prefix" && -z "$(ls -A -- "$prefix")" ]] || die "$prefix exists and is not an empty directory"
fi
mkdir -p -- "$prefix/bin"
for b in adcd adc adc-harness adc-agent; do
  [[ -f "$src/$b" ]] || die "$src/$b is missing"
  install -m 0755 -- "$src/$b" "$prefix/bin/$b"
  [[ -f "$prefix/bin/$b" && ! -L "$prefix/bin/$b" ]] || die "$prefix/bin/$b is not a regular file"
done
read -r node_version node_sum < <(awk -v p="$platform" '$1 == p { print $2, $5 }' "$pins") || true
[[ "${node_version:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ && "${node_sum:-}" =~ ^[0-9a-f]{64}$ ]] || die "no Node pin for $platform in $pins"
[[ -d "$src/lib/adc" && ! -L "$src/lib/adc" ]] || die "$src/lib/adc is missing (run make dist)"
mkdir -p -- "$prefix/lib"
cp -R -- "$src/lib/adc" "$prefix/lib/adc"
# hostile-symlink's base link (P24c) is allowed only to its decoy, beside it in its case.
links="$(cd "$prefix/lib/adc" && find . -type l -printf '%p -> %l\n' | grep -vxF './fixtures/m1/cases/hostile-symlink/testdata/docs/key -> ../decoy/key' | sort)"
[[ "$links" == $'./node_modules/@adc/contract -> ../../adapters/contract\n./node_modules/@adc/contract-gen -> ../../gen/ts' ]] ||
  die "lib/adc holds other symlinks than the two @adc links and hostile-symlink's base link: $links"
lib="$prefix/lib/adc"
[[ -f "$lib/SHA256SUMS" && ! -L "$lib/SHA256SUMS" ]] || die "lib/adc has no SHA256SUMS"
(cd "$lib" && sha256sum --check --strict --quiet SHA256SUMS) || die "lib/adc does not match its SHA256SUMS"
[[ "$(cd "$lib" && sed 's/^[0-9a-f]\{64\}  //' SHA256SUMS | sort)" == "$(cd "$lib" && find . -type f ! -path ./SHA256SUMS | sort)" ]] ||
  die "lib/adc holds files its SHA256SUMS does not list"
for f in LICENSE NOTICE node/LICENSE fixtures/t0/py-mini/tasks/py-mini-feature-1/task.json fixtures/t0/go-mini/tasks/go-mini-feature-1/task.json \
  fixtures/m1/stub-provider/server.mjs scripts/installed-cycle.sh scripts/lib/clean-shell.sh scripts/lib/secrets.sh scripts/lib/base-remote.sh scripts/lib/sandbox-root.sh scripts/lib/cycle-case.sh scripts/lib/cycle-expect.mjs \
  fixtures/m1/review-qualification/suite.json adapters/package.json adapters/pnpm-workspace.yaml adapters/pnpm-lock.yaml; do
  [[ -f "$lib/$f" ]] || die "lib/adc/$f is not installed"
done

prefix="$(cd "$prefix" && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" <<<"$2"; }
commit="" schemaset=""
for b in adcd adc adc-harness adc-agent; do
  out="$(cd "$scratch" && env -i PATH=/usr/bin:/bin HOME="$scratch" "$prefix/bin/$b" version --json)" || die "$b version failed"
  [[ "$(field binary "$out")" == "$b" ]] || die "$b reports binary $(field binary "$out")"
  [[ "$(field platform "$out")" == "$platform" ]] || die "$b reports platform $(field platform "$out"), want $platform"
  s="$(field schema_set "$out")" c="$(field commit "$out")"
  [[ "$s" =~ ^sha256:[0-9a-f]{64}$ ]] || die "$b reports schema set '$s'"
  [[ "$c" =~ ^[0-9a-f]{7,64}$ ]] || die "$b reports commit '$c'"
  [[ -z "$schemaset" || "$s" == "$schemaset" ]] || die "$b reports schema set $s, others $schemaset"
  [[ -z "$commit" || "$c" == "$commit" ]] || die "$b reports commit $c, others $commit"
  schemaset="$s" commit="$c"
done
[[ -z "$expect" || "$schemaset" == "$expect" ]] || die "installed schema set $schemaset, expected $expect"
node="$prefix/lib/adc/node/bin/node"
[[ -f "$node" && ! -L "$node" ]] || die "$node is not a regular file"
echo "$node_sum  $node" | sha256sum --check --status - || die "the installed node does not match its pinned SHA-256"
v="$(cd "$scratch" && env -i PATH=/usr/bin:/bin HOME="$scratch" "$node" --version)" || die "the installed node does not run"
[[ "$v" == "$node_version" ]] || die "the installed node reports $v, pinned $node_version"
# The fake adapter and its dependencies resolve from the prefix alone.
(cd "$scratch" && env -i PATH=/usr/bin:/bin HOME="$scratch" "$node" \
  -e 'import(require("node:url").pathToFileURL(process.argv[1]).href).catch((e) => { console.error(e.message); process.exit(1); })' \
  "$prefix/lib/adc/adapters/fake/src/fake.ts") || die "the installed fake adapter does not load with the installed node"
# The http-chat adapter (P13) runs from the prefix alone: it answers describe and shuts down.
# The requests come from a file, not a pipe: an adapter may exit before a
# pipe's writer has written, and pipefail would then report the writer's
# SIGPIPE (141) as the adapter failing. Its stderr and exit status go into
# the error, so a failure is diagnosable.
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"describe"}' '{"jsonrpc":"2.0","id":2,"method":"shutdown"}' >"$scratch/http-chat.requests"
rc=0
described="$(cd "$scratch" && env -i PATH=/usr/bin:/bin HOME="$scratch" "$node" "$prefix/lib/adc/adapters/http-chat/src/main.ts" \
  <"$scratch/http-chat.requests" 2>"$scratch/http-chat.stderr")" || rc=$?
[[ $rc -eq 0 ]] ||
  die "the installed http-chat adapter does not run with the installed node (exit $rc): $(cat "$scratch/http-chat.stderr")"
[[ "$(field harness "$described")" == "http-chat" ]] || die "the installed http-chat adapter does not describe itself"
echo "install-check: adcd, adc, adc-harness and adc-agent installed into $prefix ($platform, commit $commit, schema set $schemaset), with node $node_version (sha256:$node_sum), the fake and http-chat adapters and py-mini"
