#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds a project's prepared Go image (P32, 00 §8 item 2; #226 entry 97)
# from fixtures/m1/images/go/Containerfile, labelled with the manifest the
# sandbox checks (dev.adc.prepared: the toolchain key in the profile's
# toolchain.images and the sha256 of each lockfile its
# dependency_recipe.lockfiles names).
#
# PROJECT is one of:
#   auto-dev-cycle  this repository (go.mod, go.sum)
#   go-mini         checkpoint (a)'s stub project, T0's fixtures/t0/go-mini/repo
#                   (go.mod only: it has no dependencies)
# Both build from the same Containerfile, base and toolchain; only the
# lockfiles and the warmed packages differ.
#
# Usage:
#   scripts/prepare-go-image.sh manifest PROJECT        print the label's JSON for PROJECT's lockfiles
#   scripts/prepare-go-image.sh build PROJECT NAME [PLATFORMS]
#       build the manifest list NAME (default platforms: linux/amd64,linux/arm64)
#
# The build needs network (the base image and the Go module proxy) and
# podman. It never pushes; see the project's profile
# (fixtures/m1/profiles/PROJECT.json) for the pin after it.
set -euo pipefail

toolchain=go
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
containerfile="$here/fixtures/m1/images/go/Containerfile"

die() { printf 'prepare-go-image: %s\n' "$*" >&2; exit 1; }

# project PROJECT: sets repo, lockfiles (the profile's, in its order) and
# warm_required (a project with dependencies must warm some).
project() {
  case "$1" in
    auto-dev-cycle) repo="$here" lockfiles=(go.mod go.sum) warm_required=1 ;;
    go-mini) repo="$here/fixtures/t0/go-mini/repo" lockfiles=(go.mod) warm_required=0 ;;
    *) die "unknown project '$1' (auto-dev-cycle or go-mini)" ;;
  esac
}

# manifest: compact JSON, lockfiles in a fixed order.
manifest() {
  local out sum f sep=""
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
    [[ $# -eq 2 ]] || die "usage: prepare-go-image.sh manifest PROJECT"
    project "$2"
    manifest
    ;;
  build)
    [[ $# -eq 3 || $# -eq 4 ]] || die "usage: prepare-go-image.sh build PROJECT NAME [PLATFORMS]"
    project "$2"
    name="$3" platforms="${4:-linux/amd64,linux/arm64}"
    # The sandbox's rule for a pin (sandbox.Host.Resolve): a registry host
    # (with '.' or ':', or localhost) before the first '/'; no tag or digest.
    registry="${name%%/*}"
    if [[ "$name" != */* || ! "$name" =~ ^[a-z0-9./:_-]+$ || "${name#*/}" == *:* ]] ||
      [[ "$registry" != *[.:]* && "$registry" != localhost ]]; then
      die "NAME is a fully qualified repository name, without a tag or digest (got '$name')"
    fi
    # The label must describe the lockfiles as committed at the base.
    dirty="$(git -C "$repo" status --porcelain -- "${lockfiles[@]}")" || die "$repo is not in a git checkout"
    [[ -z "$dirty" ]] || die "uncommitted changes to ${lockfiles[*]} in $repo"
    head="$(git -C "$repo" rev-parse HEAD)" || die "cannot read HEAD in $repo"
    printf 'prepare-go-image: preparing %s from %s at %s\n' "$2" "$repo" "$head"
    label="$(manifest)"
    ctx="$(mktemp -d)"
    trap 'rm -rf -- "$ctx"' EXIT
    # The context holds the lockfiles the label names (under lock/), and
    # warm.txt: the third-party packages the project's tests import, whose
    # builds the image caches (none for a project without dependencies).
    mkdir -- "$ctx/lock"
    for f in "${lockfiles[@]}"; do cp -- "$repo/$f" "$ctx/lock/$f"; done
    go="$("$here/scripts/bootstrap-go.sh" --print-goroot)/bin/go"
    [[ -x "$go" ]] || die "the pinned Go is not installed; run 'make toolchain'"
    GOTOOLCHAIN=local GOENV=off GOFLAGS=-mod=readonly CGO_ENABLED=0 "$go" -C "$repo" list -deps -test \
      -f '{{if and (not .Standard) .Module (not .Module.Main)}}{{.ImportPath}}{{end}}' ./... |
      LC_ALL=C sort -u >"$ctx/warm.txt"
    [[ -s "$ctx/warm.txt" || "$warm_required" == 0 ]] || die "no third-party packages listed in $repo"
    podman build --pull=missing --timestamp=0 --platform="$platforms" --manifest="$name" \
      --label="dev.adc.prepared=$label" -f "$containerfile" "$ctx"
    printf 'prepare-go-image: built %s for %s with dev.adc.prepared=%s\n' "$name" "$platforms" "$label"
    ;;
  *) die "usage: prepare-go-image.sh manifest PROJECT | build PROJECT NAME [PLATFORMS]" ;;
esac
