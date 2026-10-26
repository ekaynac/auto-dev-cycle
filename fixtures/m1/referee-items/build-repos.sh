#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds the referee items' base repositories (adc dev referee, stage 2 P-A): for each T0 fixture, the bare
# repository repos/<fixture>.git beside this script. Its branch base is fixtures/t0/<fixture>/repo as one commit
# (check.sh's fresh_copy); each failing_then_green task's branch, named for the task, adds the task's visible/
# tests on top (check.sh's base for that task). Identity and dates are fixed, so the commit ids are the ones the
# items name as their base. Prints each branch and its commit id.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
t0="$(cd "$here/../../t0" && pwd)"
out="$here/repos"
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
export GIT_AUTHOR_NAME=adc-referee GIT_AUTHOR_EMAIL=adc-referee@example.invalid GIT_AUTHOR_DATE=2026-01-01T00:00:00Z
export GIT_COMMITTER_NAME=adc-referee GIT_COMMITTER_EMAIL=adc-referee@example.invalid GIT_COMMITTER_DATE=2026-01-01T00:00:00Z

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
rm -rf "$out"
mkdir -p "$out"
for fixture in go-mini php-mini py-mini ts-mini; do
  tree="$work/$fixture"
  mkdir "$tree"
  cp -R "$t0/$fixture/repo/." "$tree/"
  git -C "$tree" init -q -b base
  git -C "$tree" add -A
  git -C "$tree" commit -q -m base
  for task in "$t0/$fixture/tasks"/*/; do
    [[ -d "$task/visible" ]] || continue
    git -C "$tree" checkout -q -b "$(basename "$task")" base
    cp -R "$task/visible/." "$tree/"
    git -C "$tree" add -A
    git -C "$tree" commit -q -m "$(basename "$task")"
  done
  git clone -q --bare "$tree" "$out/$fixture.git"
  git -C "$out/$fixture.git" for-each-ref --format="$fixture %(refname:short) %(objectname)" refs/heads
done
