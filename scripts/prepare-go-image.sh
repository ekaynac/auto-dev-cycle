#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds this repository's prepared Go image (P32, 00 §8 item 2) from
# fixtures/m1/images/go/Containerfile, labelled with the manifest the sandbox
# checks (dev.adc.prepared: the toolchain key in the profile's
# toolchain.images and the sha256 of each lockfile).
#
# Usage:
#   scripts/prepare-go-image.sh manifest REPO           print the label's JSON for REPO's lockfiles
#   scripts/prepare-go-image.sh build REPO NAME [PLATFORMS]
#       build the manifest list NAME (default platforms: linux/amd64,linux/arm64)
#
# The build needs network (the base image and the Go module proxy) and
# podman. It never pushes; see the profile's pin in
# fixtures/m1/profiles/auto-dev-cycle.json for the steps after it.
set -euo pipefail

toolchain=go
lockfiles=(go.mod go.sum)
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
containerfile="$here/fixtures/m1/images/go/Containerfile"

die() { printf 'prepare-go-image: %s\n' "$*" >&2; exit 1; }

# manifest REPO: compact JSON, lockfiles in a fixed order.
manifest() {
  local repo="$1" out sum f sep=""
  out="{\"toolchain\":\"$toolchain\",\"lockfiles\":{"
  for f in "${lockfiles[@]}"; do
    [[ -f "$repo/$f" && ! -L "$repo/$f" ]] || die "$repo/$f is not a regular file"
    sum="$(sha256sum -- "$repo/$f")" || die "cannot hash $repo/$f"
    out+="$sep\"$f\":\"sha256:${sum%% *}\""
    sep=","
  done
  printf '%s}}\n' "$out"
}

case "${1:-}" in
  manifest)
    [[ $# -eq 2 ]] || die "usage: prepare-go-image.sh manifest REPO"
    manifest "$2"
    ;;
  build)
    [[ $# -eq 3 || $# -eq 4 ]] || die "usage: prepare-go-image.sh build REPO NAME [PLATFORMS]"
    repo="$2" name="$3" platforms="${4:-linux/amd64,linux/arm64}"
    # The sandbox's rule for a pin (sandbox.Host.Resolve): a registry host
    # (with '.' or ':', or localhost) before the first '/'; no tag or digest.
    registry="${name%%/*}"
    if [[ "$name" != */* || ! "$name" =~ ^[a-z0-9./:_-]+$ || "${name#*/}" == *:* ]] ||
      [[ "$registry" != *[.:]* && "$registry" != localhost ]]; then
      die "NAME is a fully qualified repository name, without a tag or digest (got '$name')"
    fi
    # The label must describe the lockfiles as committed at the base.
    dirty="$(git -C "$repo" status --porcelain -- "${lockfiles[@]}")" || die "$repo is not a git checkout"
    [[ -z "$dirty" ]] || die "uncommitted changes to ${lockfiles[*]} in $repo"
    head="$(git -C "$repo" rev-parse HEAD)" || die "cannot read HEAD in $repo"
    printf 'prepare-go-image: preparing from %s at %s\n' "$repo" "$head"
    label="$(manifest "$repo")"
    ctx="$(mktemp -d)"
    trap 'rm -rf -- "$ctx"' EXIT
    # The context holds the lockfiles the label names, and warm.txt: the
    # third-party packages REPO's tests import, whose builds the image caches.
    for f in "${lockfiles[@]}"; do cp -- "$repo/$f" "$ctx/$f"; done
    go="$("$here/scripts/bootstrap-go.sh" --print-goroot)/bin/go"
    [[ -x "$go" ]] || die "the pinned Go is not installed; run 'make toolchain'"
    GOTOOLCHAIN=local GOENV=off GOFLAGS=-mod=readonly CGO_ENABLED=0 "$go" -C "$repo" list -deps -test \
      -f '{{if and (not .Standard) .Module (not .Module.Main)}}{{.ImportPath}}{{end}}' ./... |
      LC_ALL=C sort -u >"$ctx/warm.txt"
    [[ -s "$ctx/warm.txt" ]] || die "no third-party packages listed in $repo"
    podman build --pull=missing --timestamp=0 --platform="$platforms" --manifest="$name" \
      --label="dev.adc.prepared=$label" -f "$containerfile" "$ctx"
    printf 'prepare-go-image: built %s for %s with dev.adc.prepared=%s\n' "$name" "$platforms" "$label"
    ;;
  *) die "usage: prepare-go-image.sh manifest REPO | build REPO NAME [PLATFORMS]" ;;
esac
