#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Installs the Go toolchain pinned by go.mod's `toolchain` directive into a
# per-user prefix (default ~/.local/go), from the official tarball, after
# verifying its SHA-256 against go.dev's published checksum. No system
# packages, no sudo (docs/design/m0/01, docs/plan/04 §8).
#
# Usage: scripts/bootstrap-go.sh [--print-goroot]
#   ADC_GO_PREFIX   install prefix (default: $HOME/.local/go)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
prefix="${ADC_GO_PREFIX:-$HOME/.local/go}"

die() { printf 'bootstrap-go: %s\n' "$*" >&2; exit 1; }

want="$(awk '$1 == "toolchain" { print $2 }' "$repo_root/go.mod")"
[[ "$want" =~ ^go[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || die "go.mod has no valid toolchain directive (got '${want}')"

if [[ "${1:-}" == "--print-goroot" ]]; then
  printf '%s\n' "$prefix"
  exit 0
fi

if [[ -x "$prefix/bin/go" ]]; then
  have="$("$prefix/bin/go" env GOVERSION 2>/dev/null || true)"
  if [[ "$have" == "$want" ]]; then
    printf 'bootstrap-go: %s already installed at %s\n' "$want" "$prefix"
    exit 0
  fi
  printf 'bootstrap-go: replacing %s with %s at %s\n' "${have:-unknown}" "$want" "$prefix"
fi

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

file="${want}.${os}-${arch}.tar.gz"
for tool in curl jq tar sha256sum; do
  command -v "$tool" >/dev/null || die "required tool not found: $tool"
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL 'https://go.dev/dl/?mode=json&include=all' -o "$tmp/index.json" \
  || die "could not fetch the go.dev release index"
sum="$(jq -r --arg f "$file" '[.[].files[] | select(.filename == $f) | .sha256][0] // empty' "$tmp/index.json")"
[[ "$sum" =~ ^[0-9a-f]{64}$ ]] || die "no published checksum for $file"

curl -fsSL "https://go.dev/dl/$file" -o "$tmp/$file" || die "download failed: $file"
printf '%s  %s\n' "$sum" "$tmp/$file" | sha256sum -c --quiet - || die "checksum mismatch for $file"

mkdir -p "$tmp/unpack"
tar -C "$tmp/unpack" -xzf "$tmp/$file"
[[ -x "$tmp/unpack/go/bin/go" ]] || die "tarball has no go/bin/go"

mkdir -p "$(dirname "$prefix")"
if [[ -e "$prefix" ]]; then
  mv "$prefix" "$tmp/old"
fi
mv "$tmp/unpack/go" "$prefix"

got="$("$prefix/bin/go" env GOVERSION)"
[[ "$got" == "$want" ]] || die "installed $got, expected $want"
printf 'bootstrap-go: installed %s at %s\n' "$got" "$prefix"
