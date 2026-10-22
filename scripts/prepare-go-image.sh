#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds a project's prepared Go image (P32, 00 §8 item 2; #226 entry 97)
# from fixtures/m1/images/go/Containerfile, labelled with the manifest the
# sandbox checks (dev.adc.prepared: the toolchain key in the profile's
# toolchain.images, the sha256 of each lockfile its
# dependency_recipe.lockfiles names and, per platform, the digest of the
# builder's adapter the image carries; P22a-5d).
#
# PROJECT is one of:
#   auto-dev-cycle  this repository (go.mod, go.sum)
#   go-mini         checkpoint (a)'s stub project, T0's fixtures/t0/go-mini/repo
#                   (go.mod only: it has no dependencies)
# Both build from the same Containerfile, base and toolchain; only the
# lockfiles and the warmed packages differ.
#
# The adapter is make dist's lib/adc closure of each platform
# (sandbox.AdapterClosure: node/bin/node, adapters/http-chat,
# adapters/contract, gen/ts, node_modules), checked against the dist's
# SHA256SUMS, with node/bin/node checked against scripts/node-pins, copied
# with its symlinks and execute bits under adapter/linux-ARCH/ in the
# context, and digested by the dist's own adc (adc dev adapter-digest) for
# the label. build requires a clean checkout whose HEAD the dist's adc was
# built from, and a NAME podman does not hold yet (podman build --manifest
# would add to an existing list); after the build, the list must hold one
# image per platform, and the pinned Node must start in each.
#
# Usage:
#   scripts/prepare-go-image.sh [--pins FILE] manifest PROJECT        print the label's lockfile part for PROJECT
#   scripts/prepare-go-image.sh [--pins FILE] context PROJECT DIST DIR [PLATFORMS]
#       assemble lock/ and adapter/ of the build context from the dist
#       directory DIST into the new directory DIR, and print the label
#   scripts/prepare-go-image.sh [--pins FILE] build PROJECT NAME [PLATFORMS]
#       build the manifest list NAME from dist/ (default platforms: linux/amd64,linux/arm64)
# --pins FILE: the Node pins (default scripts/node-pins), as dist-lib.sh takes them.
#
# The build needs network (the base image and the Go module proxy), podman
# and jq. It never pushes. The lead's commands, on a clean checkout (the
# rm only for a NAME podman already holds, e.g. from the previous build;
# the tag is the README's form, and the digest files go outside the
# checkout, which build requires clean):
#   make dist
#   podman manifest rm ghcr.io/ekaynac/adc-prepared-go-mini
#   podman manifest rm ghcr.io/ekaynac/adc-prepared-go
#   scripts/prepare-go-image.sh build go-mini        ghcr.io/ekaynac/adc-prepared-go-mini
#   scripts/prepare-go-image.sh build auto-dev-cycle ghcr.io/ekaynac/adc-prepared-go
#   tag="go1.27.1-$(date -u +%F)" digests="$(mktemp -d)"
#   podman manifest push --all --digestfile "$digests/go-mini" ghcr.io/ekaynac/adc-prepared-go-mini "docker://ghcr.io/ekaynac/adc-prepared-go-mini:$tag"
#   podman manifest push --all --digestfile "$digests/go"      ghcr.io/ekaynac/adc-prepared-go      "docker://ghcr.io/ekaynac/adc-prepared-go:$tag"
#   head "$digests"/*
# Each digest file holds the pushed list's index digest (sha256:…), the same
# whatever the tag. The pin, NAME@sha256:…, goes in the project's profile
# (fixtures/m1/profiles/PROJECT.json, toolchain.images.go).
set -euo pipefail

toolchain=go
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
containerfile="$here/fixtures/m1/images/go/Containerfile"
closure=(node/bin/node adapters/http-chat adapters/contract gen/ts node_modules) # sandbox.AdapterClosure
pins="$here/scripts/node-pins"
if [[ "${1:-}" == "--pins" ]]; then
  pins="${2:?--pins FILE}"
  shift 2
fi

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

# platforms LIST: sets plats, the comma-separated LIST sorted (the label's
# key order), each linux/amd64 or linux/arm64, once.
platforms() {
  local p n
  IFS=, read -ra plats <<<"$1"
  n=${#plats[@]}
  ((n > 0)) || die "PLATFORMS names no platform"
  for p in "${plats[@]}"; do
    [[ "$p" == linux/amd64 || "$p" == linux/arm64 ]] || die "unsupported platform '$p' (linux/amd64 or linux/arm64)"
  done
  mapfile -t plats < <(printf '%s\n' "${plats[@]}" | LC_ALL=C sort -u)
  ((${#plats[@]} == n)) || die "PLATFORMS names a platform twice (got '$1')"
}

# host_adc DIST: sets adc, the dist's adc for this host.
host_adc() {
  local arch
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "no dist platform for this host ($(uname -m))" ;;
  esac
  adc="$1/linux-$arch/adc"
  [[ -f "$adc" && -x "$adc" ]] || die "$adc is missing: run 'make dist' on this clean commit"
}

# manifest [ADAPTER]: compact JSON, lockfiles in a fixed order, then the
# adapter object when given.
manifest() {
  local out sum f sep=""
  out="{\"toolchain\":\"$toolchain\",\"lockfiles\":{"
  for f in "${lockfiles[@]}"; do
    [[ -f "$repo/$f" && ! -L "$repo/$f" ]] || die "$repo/$f is not a regular file"
    sum="$(sha256sum -- "$repo/$f")" || die "cannot hash $repo/$f"
    out+="$sep\"$f\":\"sha256:${sum%% *}\""
    sep=","
  done
  printf '%s}%s}\n' "$out" "${1:+,\"adapter\":$1}"
}

# pin PLATFORM FIELD: the field of PLATFORM's line in the Node pins (2: the
# version, 5: bin/node's sha256), if exactly one line names it.
pin() {
  local v
  v="$(awk -v p="$1" -v n="$2" '$1 == p { print $n }' "$pins")" || die "cannot read $pins"
  [[ -n "$v" && "$v" != *$'\n'* ]] || die "$pins does not pin one Node for $1"
  printf '%s\n' "$v"
}

# context DIST DIR: puts the lockfiles the label names (under lock/) and
# each platform's adapter closure (under adapter/linux-ARCH/) into DIR, and
# sets label and digests (PLATFORM -> its adapter digest). Node must be the
# one the pins name.
declare -A digests
context() {
  local dist="$1" dir="$2" p f lib dst sum src want adapter="{" sep=""
  mkdir -- "$dir/lock" "$dir/adapter"
  for f in "${lockfiles[@]}"; do cp -- "$repo/$f" "$dir/lock/$f"; done
  for p in "${plats[@]}"; do
    lib="$dist/linux-${p#linux/}/lib/adc" dst="$dir/adapter/linux-${p#linux/}"
    [[ -d "$lib" ]] || die "$lib is missing: run 'make dist' on this clean commit"
    (cd "$lib" && sha256sum --check --strict --quiet SHA256SUMS >&2) ||
      die "$lib does not match its SHA256SUMS: run 'make dist' on this clean commit"
    for f in "${closure[@]}"; do
      mkdir -p -- "$dst/$(dirname -- "$f")"
      cp -PR --preserve=mode -- "$lib/$f" "$dst/$f" || die "copying $f of $lib failed"
    done
    [[ -f "$dst/node/bin/node" && ! -L "$dst/node/bin/node" ]] || die "node/bin/node of $lib is not a regular file"
    sum="$(sha256sum -- "$dst/node/bin/node")" || die "cannot hash node/bin/node of $lib"
    want="$(pin "$p" 5)" || exit 1
    [[ "${sum%% *}" == "$want" ]] || die "node/bin/node of $lib is not the Node $pins pins for $p"
    sum="$("$adc" dev adapter-digest "$dst")" || die "the adapter of $p cannot be digested"
    src="$("$adc" dev adapter-digest "$lib")" || die "the adapter of $lib cannot be digested"
    [[ "$sum" =~ ^sha256:[0-9a-f]{64}$ && "$sum" == "$src" ]] || die "the adapter copied for $p differs from $lib's"
    adapter+="$sep\"$p\":\"$sum\""
    digests[$p]="$sum"
    sep=","
  done
  label="$(manifest "$adapter}")"
}

case "${1:-}" in
  manifest)
    [[ $# -eq 2 ]] || die "usage: prepare-go-image.sh manifest PROJECT"
    project "$2"
    manifest
    ;;
  context)
    [[ $# -eq 4 || $# -eq 5 ]] || die "usage: prepare-go-image.sh context PROJECT DIST DIR [PLATFORMS]"
    project "$2"
    platforms "${5:-linux/amd64,linux/arm64}"
    host_adc "$3"
    ctx="$4"
    mkdir -- "$ctx" || die "$ctx must be a new directory"
    trap '[[ -n "${label:-}" ]] || rm -rf -- "$ctx"' EXIT
    context "$3" "$ctx"
    printf '%s\n' "$label"
    ;;
  build)
    [[ $# -eq 3 || $# -eq 4 ]] || die "usage: prepare-go-image.sh build PROJECT NAME [PLATFORMS]"
    project "$2"
    name="$3"
    platforms "${4:-linux/amd64,linux/arm64}"
    # The sandbox's rule for a pin (sandbox.Host.Resolve): a registry host
    # (with '.' or ':', or localhost) before the first '/'; no tag or digest.
    registry="${name%%/*}"
    if [[ "$name" != */* || ! "$name" =~ ^[a-z0-9./:_-]+$ || "${name#*/}" == *:* ]] ||
      [[ "$registry" != *[.:]* && "$registry" != localhost ]]; then
      die "NAME is a fully qualified repository name, without a tag or digest (got '$name')"
    fi
    # The label must describe the lockfiles as committed at the base, and
    # the adapter of the release built from this commit.
    dirty="$(git -C "$here" status --porcelain)" || die "$here is not a git checkout"
    [[ -z "$dirty" ]] || die "uncommitted changes in $here"
    head="$(git -C "$here" rev-parse HEAD)" || die "cannot read HEAD in $here"
    host_adc "$here/dist"
    version="$("$adc" version)" || die "$adc does not run"
    [[ "$version" == *"(commit $head,"* && "$version" != *-dirty* ]] ||
      die "$adc is not built from $head ($version): run 'make dist' on this clean commit"
    # podman build --manifest adds to an existing list (#373 M1): NAME must
    # name nothing yet, so the list holds only this build's images.
    for kind in manifest image; do
      rc=0
      podman "$kind" exists "$name" || rc=$?
      ((rc == 1)) || die "$name already exists in podman's storage, or podman failed: run 'podman manifest rm $name' first"
    done
    printf 'prepare-go-image: preparing %s from %s at %s\n' "$2" "$repo" "$head"
    ctx="$(mktemp -d)"
    trap 'rm -rf -- "$ctx"' EXIT
    context "$here/dist" "$ctx"
    # warm.txt: the third-party packages the project's tests import, whose
    # builds the image caches (none for a project without dependencies).
    go="$("$here/scripts/bootstrap-go.sh" --print-goroot)/bin/go"
    [[ -x "$go" ]] || die "the pinned Go is not installed; run 'make toolchain'"
    GOTOOLCHAIN=local GOENV=off GOFLAGS=-mod=readonly CGO_ENABLED=0 "$go" -C "$repo" list -deps -test \
      -f '{{if and (not .Standard) .Module (not .Module.Main)}}{{.ImportPath}}{{end}}' ./... |
      LC_ALL=C sort -u >"$ctx/warm.txt"
    [[ -s "$ctx/warm.txt" || "$warm_required" == 0 ]] || die "no third-party packages listed in $repo"
    podman build --pull=missing --timestamp=0 --platform="$(IFS=,; echo "${plats[*]}")" --manifest="$name" \
      --label="dev.adc.prepared=$label" -f "$containerfile" "$ctx"
    # The new list holds exactly one image per platform; in each the pinned
    # Node starts, as the platform's, and /opt/adc/lib, copied out of it,
    # has the label's adapter digest for the platform (Mythos #373 M1: the
    # label pins what COPY put in the image, not only the context).
    listed="$(podman manifest inspect "$name" | jq -r '.manifests[] | .platform.os + "/" + .platform.architecture' | LC_ALL=C sort)" ||
      die "cannot list the images of $name (podman manifest inspect, jq)"
    [[ "$listed" == "$(printf '%s\n' "${plats[@]}")" ]] ||
      die "$name lists ${listed//$'\n'/ } rather than one image for each of ${plats[*]}: run 'podman manifest rm $name' and build again"
    for p in "${plats[@]}"; do
      want="$(pin "$p" 2) ${p#linux/}"
      want="${want/%amd64/x64}"
      got="$(podman run --rm --pull=never --network=none --platform="$p" "$name" \
        /opt/adc/lib/node/bin/node -p 'process.version + " " + process.arch')" || die "Node does not start in $name for $p"
      [[ "$got" == "$want" ]] || die "Node in $name for $p reports '$got', not '$want'"
      out="$ctx/image-${p#linux/}"
      c="$(podman create --pull=never --network=none --platform="$p" "$name" true)" || die "cannot create a container of $name for $p"
      rc=0
      podman cp "$c:/opt/adc/lib" "$out" || rc=$?
      podman rm "$c" >/dev/null || die "cannot remove the container $c"
      ((rc == 0)) || die "cannot copy /opt/adc/lib out of $name for $p"
      got="$("$adc" dev adapter-digest "$out")" || die "the adapter in $name for $p cannot be digested"
      [[ "$got" == "${digests[$p]}" ]] || die "the adapter in $name for $p is $got, not the label's ${digests[$p]}"
    done
    printf 'prepare-go-image: built %s for %s with dev.adc.prepared=%s\n' "$name" "${plats[*]}" "$label"
    ;;
  *) die "usage: prepare-go-image.sh [--pins FILE] manifest PROJECT | context PROJECT DIST DIR [PLATFORMS] | build PROJECT NAME [PLATFORMS]" ;;
esac
