#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The clean-install row of the M0 exit check (docs/design/m0/00): install
# adcd, adc and adc-agent for this host from dist/ into an empty prefix,
# then run the installed binaries outside the source tree, with a scrubbed
# environment, and check what they report: each its own name, this
# platform, one commit, and one schema-set digest (equal to --expect when
# given).
#
# Usage: scripts/install-check.sh [--dist DIR] [--expect sha256:HEX] PREFIX
set -euo pipefail

die() { echo "install-check: $*" >&2; exit 1; }

dist="dist"
expect=""
expect_given=""
while [[ $# -gt 1 ]]; do
  case "$1" in
    --dist) dist="$2"; shift 2 ;;
    --expect) expect="$2"; expect_given=1; shift 2 ;;
    *) die "unknown argument $1" ;;
  esac
done
[[ $# -eq 1 ]] || die "usage: install-check.sh [--dist DIR] [--expect sha256:HEX] PREFIX"
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
for b in adcd adc adc-agent; do
  [[ -f "$src/$b" ]] || die "$src/$b is missing"
  install -m 0755 -- "$src/$b" "$prefix/bin/$b"
  [[ -f "$prefix/bin/$b" && ! -L "$prefix/bin/$b" ]] || die "$prefix/bin/$b is not a regular file"
done

prefix="$(cd "$prefix" && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" <<<"$2"; }
commit="" schemaset=""
for b in adcd adc adc-agent; do
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
echo "install-check: adcd, adc and adc-agent installed into $prefix ($platform, commit $commit, schema set $schemaset)"
