#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Installs the Go toolchain pinned by go.mod's `toolchain` directive into a
# per-user prefix (default ~/.local/go), from the official tarball, after
# verifying its SHA-256 against go.dev's published checksum. No system
# packages, no sudo (docs/design/m0/01, docs/plan/04 §8).
#
# Usage: scripts/bootstrap-go.sh [--print-goroot | --print-version]
#   ADC_GO_PREFIX   absolute install prefix (default: $HOME/.local/go). It must
#                   be absent, empty, or an existing Go install; anything else
#                   is refused, never replaced.
set -euo pipefail

# Never let a `go` command fetch a different toolchain (GOTOOLCHAIN=auto would
# bypass the checksum check below and report the wrong version).
export GOTOOLCHAIN=local

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
prefix="${ADC_GO_PREFIX:-$HOME/.local/go}"

die() { printf 'bootstrap-go: %s\n' "$*" >&2; exit 1; }

want="$(awk '$1 == "toolchain" { print $2 }' "$repo_root/go.mod")"
[[ "$want" =~ ^go[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || die "go.mod has no valid toolchain directive (got '${want}')"
[[ "$prefix" == /* ]] || die "ADC_GO_PREFIX must be an absolute path (got '$prefix')"
prefix="${prefix%/}"
# A prefix must sit at least two levels deep and never be the home itself.
[[ "$prefix" == /*/* && "$prefix" != "${HOME%/}" ]] || die "ADC_GO_PREFIX '$prefix' is too shallow; refusing"

case "${1:-}" in
  "") ;;
  --print-goroot) printf '%s\n' "$prefix"; exit 0 ;;
  --print-version) printf '%s\n' "$want"; exit 0 ;;
  *) die "unknown argument: $1 (usage: bootstrap-go.sh [--print-goroot | --print-version])" ;;
esac

# The installed version comes from the tree's VERSION file, not from running go.
installed_version() { [[ -f "$prefix/VERSION" ]] && head -n1 "$prefix/VERSION"; }

if [[ -e "$prefix" ]]; then
  if [[ -f "$prefix/VERSION" && -x "$prefix/bin/go" ]]; then
    have="$(installed_version)"
    if [[ "$have" == "$want" ]]; then
      printf 'bootstrap-go: %s already installed at %s\n' "$want" "$prefix"
      exit 0
    fi
    printf 'bootstrap-go: replacing %s with %s at %s\n' "$have" "$want" "$prefix"
  elif [[ -d "$prefix" && -z "$(ls -A "$prefix")" ]]; then
    rmdir "$prefix"
  else
    die "$prefix exists and is not a Go install; refusing to replace it"
  fi
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

# Stage next to the prefix, so the final swap is a rename on one filesystem.
parent="$(dirname "$prefix")"
mkdir -p "$parent"
tmp="$(mktemp -d "$parent/.bootstrap-go.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

curl --proto '=https' --tlsv1.2 -fsSL 'https://go.dev/dl/?mode=json&include=all' -o "$tmp/index.json" \
  || die "could not fetch the go.dev release index"
sum="$(jq -r --arg f "$file" '[.[].files[] | select(.filename == $f) | .sha256][0] // empty' "$tmp/index.json")"
[[ "$sum" =~ ^[0-9a-f]{64}$ ]] || die "no published checksum for $file"

curl --proto '=https' --tlsv1.2 -fsSL "https://go.dev/dl/$file" -o "$tmp/$file" || die "download failed: $file"
printf '%s  %s\n' "$sum" "$tmp/$file" | sha256sum -c --quiet - || die "checksum mismatch for $file"

mkdir -p "$tmp/unpack"
tar -C "$tmp/unpack" -xzf "$tmp/$file"
[[ -x "$tmp/unpack/go/bin/go" ]] || die "tarball has no go/bin/go"
[[ "$(head -n1 "$tmp/unpack/go/VERSION")" == "$want" ]] || die "tarball VERSION is not $want"

if [[ -e "$prefix" ]]; then
  mv "$prefix" "$tmp/old"
fi
mv "$tmp/unpack/go" "$prefix"

got="$("$prefix/bin/go" env GOVERSION || true)"
if [[ "$got" != "$want" ]]; then
  rm -rf "$prefix"
  if [[ -e "$tmp/old" ]]; then mv "$tmp/old" "$prefix"; fi
  die "installed '$got', expected $want; previous install restored"
fi
printf 'bootstrap-go: installed %s at %s\n' "$got" "$prefix"
