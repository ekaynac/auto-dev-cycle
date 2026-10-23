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
# BASE_REF=<commit> (auto-dev-cycle only; #226 entry 172): the lockfiles
# (go.mod, go.sum) and the warmed packages are a task base's, taken from a
# git archive of that commit of this repository, while the adapter is still
# the current clean HEAD's dist, checked against HEAD as without it. The
# label's lockfile hashes are then the base's, which the sandbox checks
# against the task's base (dependency_recipe.lockfiles), so a task created
# on an older base (#170's 0d3e81d) keeps its base and its go.mod, and its
# hidden tests need no re-check. A ref that is no commit here is refused
# before any podman call; a dirty checkout is refused as always. Without
# BASE_REF nothing changes.
#
# After a new image is pinned (fixtures/m1/profiles/auto-dev-cycle.json,
# toolchain.images.go), #170's VISIBLE baseline is re-recorded in it as
# #294 did (#226 entry 92): gate.Measure in the new pinned image, the
# profile's gate command over base 0d3e81d's tree, with no acceptance
# overlay, into fixtures/m1/issues/170/baseline-report.json and
# baseline-stream.json, and baseline.json's image fields (README
# "Re-recording"; TestGateIssue170 binds them to the pin). The hidden tests
# are never run there: they stay in the lead's store, and their expected
# results (baseline.json) hold while the base and its lockfiles do.
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
# image per platform, the pinned Node must start in the image of podman's
# native platform (podman info), which PLATFORMS must name, and each
# image's /opt/adc/lib must have the label's adapter digest. Each image is
# run by its own ID (the list's instance digest, found in podman's images),
# as podman 4.9 runs a local list's native image whatever --platform says.
# Once the build has begun, any failure removes NAME (podman manifest rm)
# and its images (podman rmi), or says what is left, and says not to push
# it (Opus #390 M1); a build that made no list leaves nothing.
#
# Node's start is checked only for the native platform. An emulated
# platform's run (qemu binfmt) once crashed and hung for hours (#226 entry
# 141), so it is skipped, and the skip printed: that platform's Node bytes
# are still checked, against scripts/node-pins in the context and by the
# adapter digest of what is copied out of its image. Every podman call but
# the build is cut after PREPARE_GO_IMAGE_TIMEOUT seconds (default 300),
# and killed 10 s later if it ignores the cut; the script then fails, and
# the containers it made are removed by name. The build itself is not cut:
# its length depends on the network and on emulation.
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
# and jq. It never pushes. The lead's commands, on a clean checkout, one
# chain, so a failed step pushes nothing (the previous build's NAME
# removed if podman holds it; the tag is the README's form, and the digest
# files go outside the checkout, which build requires clean):
#   make dist &&
#   { ! podman manifest exists ghcr.io/ekaynac/adc-prepared-go-mini || podman manifest rm ghcr.io/ekaynac/adc-prepared-go-mini; } &&
#   { ! podman manifest exists ghcr.io/ekaynac/adc-prepared-go || podman manifest rm ghcr.io/ekaynac/adc-prepared-go; } &&
#   scripts/prepare-go-image.sh build go-mini        ghcr.io/ekaynac/adc-prepared-go-mini &&
#   scripts/prepare-go-image.sh build auto-dev-cycle ghcr.io/ekaynac/adc-prepared-go &&
#   tag="go1.27.1-$(date -u +%F)" digests="$(mktemp -d)" &&
#   podman manifest push --all --digestfile "$digests/go-mini" ghcr.io/ekaynac/adc-prepared-go-mini "docker://ghcr.io/ekaynac/adc-prepared-go-mini:$tag" &&
#   podman manifest push --all --digestfile "$digests/go"      ghcr.io/ekaynac/adc-prepared-go      "docker://ghcr.io/ekaynac/adc-prepared-go:$tag" &&
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

step_timeout="${PREPARE_GO_IMAGE_TIMEOUT:-300}"
[[ "$step_timeout" =~ ^[1-9][0-9]{0,5}$ ]] || die "PREPARE_GO_IMAGE_TIMEOUT is a number of seconds (got '$step_timeout')"

# pod ARGS…: podman ARGS, cut after step_timeout seconds (then killed 10 s
# later if it ignores the cut). A cut call says so on stderr and returns
# 124 (137 if killed: a call whose process SIGKILL ended for another cause,
# Node by the OOM killer say, returns 137 too, so the message says both);
# the caller fails on any non-zero status.
pod() {
  local rc=0
  timeout --kill-after=10 "$step_timeout" podman "$@" || rc=$?
  if ((rc == 124 || rc == 137)); then
    printf 'prepare-go-image: podman %s was cut after %ss or killed (exit %s)\n' "$1" "$step_timeout" "$rc" >&2
  fi
  return "$rc"
}

# instances NAME: one "OS/ARCH ID" line for each image of the list NAME:
# its instance digest (podman manifest inspect) and the one image podman's
# storage holds under it (podman images): the image whose digest it is (a
# local build's) or one of whose RepoDigests ends in @it (a pull's, whose
# own digest may be its list's), each image once whatever its tags (Opus
# #397 L2); fails when there is not exactly one. Each image is run by that
# ID: podman (4.9) creates a container of a local list's native image
# whatever --platform says.
instances() {
  local inspect images p id
  inspect="$(pod manifest inspect "$1")" && images="$(pod images --all --format json)" || return 1
  jq -r --slurpfile images <(printf '%s' "$images") '
    .manifests[] | (.digest // "") as $d
    | ([$images[0][] | select($d != "" and (.Digest == $d or any(.RepoDigests[]?; endswith("@" + $d)))) | .Id] | unique) as $ids
    | .platform.os + "/" + .platform.architecture + " " + (if ($ids | length) == 1 then $ids[0] else "-" end)' <<<"$inspect" |
    while read -r p id; do
      [[ "$id" =~ ^[0-9a-f]{64}$ ]] || return 1
      printf '%s %s\n' "$p" "$id"
    done
}

# project PROJECT: sets repo, lockfiles (the profile's, in its order) and
# warm_required (a project with dependencies must warm some). With
# BASE_REF, auto-dev-cycle's repo is a git archive of that commit
# (base_dir, removed on exit), never the working tree.
base_dir=""
project() {
  case "$1" in
    auto-dev-cycle) repo="$here" lockfiles=(go.mod go.sum) warm_required=1 ;;
    go-mini) repo="$here/fixtures/t0/go-mini/repo" lockfiles=(go.mod) warm_required=0 ;;
    *) die "unknown project '$1' (auto-dev-cycle or go-mini)" ;;
  esac
  [[ -n "${BASE_REF:-}" ]] || return 0
  [[ "$1" == auto-dev-cycle ]] || die "BASE_REF is auto-dev-cycle's only (got project '$1')"
  base_commit="$(git -C "$here" rev-parse --verify --quiet "${BASE_REF}^{commit}")" || die "BASE_REF '$BASE_REF' is no commit in $here"
  base_dir="$(mktemp -d)" || die "cannot make a directory for $BASE_REF's tree"
  git -C "$here" archive --format=tar "$base_commit" | tar -C "$base_dir" -xf - || die "cannot archive $base_commit"
  repo="$base_dir"
}

# rmbase removes base_dir, BASE_REF's archive, if any.
rmbase() { [[ -z "$base_dir" ]] || rm -rf -- "$base_dir"; }

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

# native: sets arch, this host's platform's architecture (amd64 or arm64).
native() {
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "no dist platform for this host ($(uname -m))" ;;
  esac
}

# host_adc DIST: sets adc, the dist's adc for this host.
host_adc() {
  native
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
    trap rmbase EXIT
    project "$2"
    manifest
    ;;
  context)
    [[ $# -eq 4 || $# -eq 5 ]] || die "usage: prepare-go-image.sh context PROJECT DIST DIR [PLATFORMS]"
    trap rmbase EXIT
    project "$2"
    platforms "${5:-linux/amd64,linux/arm64}"
    host_adc "$3"
    ctx="$4"
    mkdir -- "$ctx" || die "$ctx must be a new directory"
    trap 'rmbase; [[ -n "${label:-}" ]] || rm -rf -- "$ctx"' EXIT
    context "$3" "$ctx"
    printf '%s\n' "$label"
    ;;
  build)
    [[ $# -eq 3 || $# -eq 4 ]] || die "usage: prepare-go-image.sh build PROJECT NAME [PLATFORMS]"
    trap rmbase EXIT
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
    # The native platform is podman's, which may not be the kernel's (a
    # remote podman; Opus #390 L3): Node's start is checked only there, so
    # PLATFORMS must name it.
    host="$(pod info --format '{{.Host.OS}}/{{.Host.Arch}}')" || die "cannot read podman's platform (podman info)"
    [[ " ${plats[*]} " == *" $host "* ]] || die "PLATFORMS (${plats[*]}) lacks podman's native platform $host: no Node start could be checked"
    # podman build --manifest adds to an existing list (#373 M1): NAME must
    # name nothing yet, so the list holds only this build's images.
    for kind in manifest image; do
      rc=0
      pod "$kind" exists "$name" || rc=$?
      ((rc == 1)) || die "$name already exists in podman's storage, or podman failed: run 'podman manifest rm $name' first"
    done
    printf 'prepare-go-image: preparing %s from %s at %s%s\n' "$2" "$repo" "$head" "${base_commit:+, its lockfiles and packages at BASE_REF $base_commit}"
    ctx="$(mktemp -d)"
    built=""
    # Once the build has begun, a failure removes NAME, which may hold a
    # list whose checks failed or were never done (Opus #390 M1), and the
    # images this build made, which podman manifest rm leaves; an image that
    # was in podman's storage before the build (a cached build reuses it, a
    # pinned one among them) is kept, and named (Opus #397 M1). A build that
    # made no list leaves nothing.
    cleanup() {
      local rc=$? ex=0 found="" id
      local -a made=() had=()
      rm -rf -- "$ctx"
      rmbase
      ((rc != 0)) && [[ -n "$built" ]] || return 0
      pod manifest exists "$name" || ex=$?
      if ((ex == 1)); then
        printf 'prepare-go-image: %s failed before its list was made: nothing to remove\n' "$name" >&2
        return 0
      fi
      if ((ex == 0)) && found="$(instances "$name")"; then
        while read -r id; do
          if grep -qxF "sha256:$id" <<<"$before"; then had+=("$id"); else made+=("$id"); fi
        done < <(cut -d' ' -f2 <<<"$found" | LC_ALL=C sort -u)
      else
        found="" # instances' partial output names only some images: none are removed
      fi
      if [[ -z "$found" ]]; then
        printf 'prepare-go-image: %s failed and could not be removed: do not push it; run podman manifest rm %s, and podman rmi the images it made\n' "$name" "$name" >&2
      elif ! pod manifest rm "$name" >/dev/null; then
        printf 'prepare-go-image: %s failed and could not be removed: do not push it; run podman manifest rm %s%s\n' \
          "$name" "$name" "${made[*]:+, and podman rmi ${made[*]}}" >&2
      elif ((${#made[@]} > 0)) && ! pod rmi -- "${made[@]}" >/dev/null; then
        printf 'prepare-go-image: %s failed and is removed, but its images could not be: do not push it; run podman rmi %s\n' "$name" "${made[*]}" >&2
      else
        printf 'prepare-go-image: %s failed and is removed, with the images it made: do not push it\n' "$name" >&2
      fi
      if ((${#had[@]} > 0)); then
        printf 'prepare-go-image: kept %s, in podman'"'"'s storage before the build\n' "${had[*]}" >&2
      fi
    }
    trap cleanup EXIT
    context "$here/dist" "$ctx"
    # warm.txt: the third-party packages the project's tests import, whose
    # builds the image caches (none for a project without dependencies).
    go="$("$here/scripts/bootstrap-go.sh" --print-goroot)/bin/go"
    [[ -x "$go" ]] || die "the pinned Go is not installed; run 'make toolchain'"
    GOTOOLCHAIN=local GOENV=off GOFLAGS=-mod=readonly CGO_ENABLED=0 "$go" -C "$repo" list -deps -test \
      -f '{{if and (not .Standard) .Module (not .Module.Main)}}{{.ImportPath}}{{end}}' ./... |
      LC_ALL=C sort -u >"$ctx/warm.txt"
    [[ -s "$ctx/warm.txt" || "$warm_required" == 0 ]] || die "no third-party packages listed in $repo"
    # The images podman holds before the build: a failed build's cleanup
    # never removes one of them (Opus #397 M1).
    before="$(pod images --all --quiet --no-trunc)" || die "cannot list podman's images"
    built=1
    podman build --pull=missing --timestamp=0 --platform="$(IFS=,; echo "${plats[*]}")" --manifest="$name" \
      --label="dev.adc.prepared=$label" -f "$containerfile" "$ctx"
    # The new list holds exactly one image per platform; in the native
    # platform's the pinned Node starts, as the platform's (an emulated
    # platform's run is skipped: #226 entry 141); and in each, /opt/adc/lib,
    # copied out of it, has the label's adapter digest for the platform
    # (Mythos #373 M1: the label pins what COPY put in the image, not only
    # the context).
    found="$(instances "$name")" ||
      die "cannot find each image of $name in podman's storage (podman manifest inspect, podman images)"
    listed="$(cut -d' ' -f1 <<<"$found" | LC_ALL=C sort)"
    [[ "$listed" == "$(printf '%s\n' "${plats[@]}")" ]] ||
      die "$name lists ${listed//$'\n'/ } rather than one image for each of ${plats[*]}: run 'podman manifest rm $name' and build again"
    for p in "${plats[@]}"; do
      id="$(awk -v p="$p" '$1 == p { print $2 }' <<<"$found")"
      if [[ "$p" == "$host" ]]; then
        version="$(pin "$p" 2)" || exit 1
        want="$version ${p#linux/}"
        want="${want/%amd64/x64}"
        n="adc-prepare-$$-${p#linux/}"
        if ! got="$(pod run --rm --name="$n" --pull=never --network=none --platform="$p" "$id" \
          /opt/adc/lib/node/bin/node -p 'process.version + " " + process.arch')"; then
          pod rm --force --ignore --time=0 "$n" >/dev/null || printf 'prepare-go-image: the container %s may still run\n' "$n" >&2
          die "Node does not start in $name for $p"
        fi
        [[ "$got" == "$want" ]] || die "Node in $name for $p reports '$got', not '$want'"
      else
        printf "prepare-go-image: Node's start in %s for %s is not checked: the platform is emulated on this %s host (its bytes are checked: scripts/node-pins, and the adapter digest below)\n" "$name" "$p" "$host"
      fi
      out="$ctx/image-${p#linux/}"
      # Named, so it is removed whatever fails, a cut create included (Opus #390 L2).
      n="adc-prepare-$$-copy-${p#linux/}" rc=0
      pod create --name="$n" --pull=never --network=none --platform="$p" "$id" true >/dev/null || rc=$?
      if ((rc == 0)); then
        pod cp "$n:/opt/adc/lib" "$out" || rc=$?
      fi
      pod rm --force --ignore --time=0 "$n" >/dev/null || die "cannot remove the container $n"
      ((rc == 0)) || die "cannot create a container of $name for $p, or copy /opt/adc/lib out of it"
      got="$("$adc" dev adapter-digest "$out")" || die "the adapter in $name for $p cannot be digested"
      [[ "$got" == "${digests[$p]}" ]] || die "the adapter in $name for $p is $got, not the label's ${digests[$p]}"
    done
    printf 'prepare-go-image: built %s for %s with dev.adc.prepared=%s\n' "$name" "${plats[*]}" "$label"
    ;;
  *) die "usage: prepare-go-image.sh [--pins FILE] manifest PROJECT | context PROJECT DIST DIR [PLATFORMS] | build PROJECT NAME [PLATFORMS]" ;;
esac
