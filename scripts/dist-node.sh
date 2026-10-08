#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Packages the pinned Node runtime for one server platform (docs/design/m0/15 D6). The official tarball that
# scripts/node-pins names is downloaded into a cache outside the workspace, which must be a directory of this
# user's that no one else can write; a private copy of it is checked against its pinned SHA-256 (from Node's
# signed SHASUMS256.txt.asc) before anything is extracted, and bin/node is checked against its own pin. Only
# bin/node and Node's LICENSE are packaged, both regular files: no npm, no corepack.
#
# Usage: scripts/dist-node.sh [--pins FILE] PLATFORM OUTDIR
#        scripts/dist-node.sh [--pins FILE] --verify PLATFORM OUTDIR   (OUTDIR/bin/node matches its pin; nothing runs)
set -euo pipefail

die() { echo "dist-node: $*" >&2; exit 1; }

pins="$(cd "$(dirname "$0")" && pwd)/node-pins"
if [[ "${1:-}" == "--pins" ]]; then pins="$2"; shift 2; fi
verify_only=""
if [[ "${1:-}" == "--verify" ]]; then verify_only=1; shift; fi
[[ $# -eq 2 ]] || die "usage: dist-node.sh [--pins FILE] [--verify] PLATFORM OUTDIR"
platform="$1" out="$2"

case "$platform" in
  linux/amd64) arch=x64 ;;
  linux/arm64) arch=arm64 ;;
  *) die "no Node build is pinned for $platform" ;;
esac
read -r version tarball tarsum binsum < <(awk -v p="$platform" '$1 == p { print $2, $3, $4, $5 }' "$pins") || true
[[ "${version:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "the pinned version for $platform is malformed"
[[ "${tarball:-}" == "node-$version-linux-$arch.tar.xz" ]] || die "the pinned tarball for $platform is not node-$version-linux-$arch.tar.xz"
[[ "${tarsum:-}" =~ ^[0-9a-f]{64}$ ]] || die "the pinned tarball SHA-256 for $platform is malformed"
[[ "${binsum:-}" =~ ^[0-9a-f]{64}$ ]] || die "the pinned bin/node SHA-256 for $platform is malformed"

matches() { echo "$2  $1" | sha256sum --check --status -; }

if [[ -n "$verify_only" ]]; then
  if [[ -f "$out/bin/node" && ! -L "$out/bin/node" ]] && matches "$out/bin/node" "$binsum"; then exit 0; fi
  die "$out/bin/node does not match the pin for $platform"
fi

[[ -n "${ADC_DIST_CACHE:-}${XDG_CACHE_HOME:-}${HOME:-}" ]] || die "no cache directory: set ADC_DIST_CACHE, XDG_CACHE_HOME or HOME"
cache="${ADC_DIST_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/adc-dist}"
# shellcheck disable=SC2174 # the mode is meant for the cache directory itself, not its parents
mkdir -p -m 0700 -- "$cache"
[[ -d "$cache" && ! -L "$cache" && -O "$cache" ]] || die "the cache $cache is not a directory of this user's"
[[ -z "$(find "$cache" -maxdepth 0 -perm /022)" ]] || die "the cache $cache is group- or world-writable"
entry="$cache/$tarball"
[[ ! -L "$entry" ]] || die "the cached $tarball is a symlink"

work="$(mktemp -d)"
part=""
trap 'rm -rf -- "$work"; [[ -z "$part" ]] || rm -f -- "$part"' EXIT
if [[ ! -f "$entry" ]] || ! matches "$entry" "$tarsum"; then
  rm -f -- "$entry"
  part="$(mktemp -- "$entry.XXXXXX")"
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --max-time 600 \
    -o "$part" "https://nodejs.org/dist/$version/$tarball" || die "downloading $tarball failed"
  mv -f -- "$part" "$entry"
  part=""
fi

cp -- "$entry" "$work/$tarball"
matches "$work/$tarball" "$tarsum" || { rm -f -- "$entry"; die "$tarball does not match its pinned SHA-256"; }
top="${tarball%.tar.xz}"
tar -xJf "$work/$tarball" -C "$work" -- "$top/bin/node" "$top/LICENSE" || die "$tarball lacks bin/node or LICENSE"
for f in bin/node LICENSE; do
  [[ -f "$work/$top/$f" && ! -L "$work/$top/$f" ]] || die "$f in $tarball is not a regular file"
done
matches "$work/$top/bin/node" "$binsum" || die "bin/node in $tarball does not match its pinned SHA-256"
mkdir -p -- "$out/bin"
install -m 0755 -- "$work/$top/bin/node" "$out/bin/node"
install -m 0644 -- "$work/$top/LICENSE" "$out/LICENSE"
echo "dist-node: $platform: node $version, bin/node sha256:$binsum"
