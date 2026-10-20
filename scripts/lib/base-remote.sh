# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# The base remote of an installed cycle case (M1 slice plan §2.6, P24),
# with fixtures/t0/*/check.sh's semantics:
#   base_remote REPO TASK_DIR OVERLAY DEST
# copies REPO (a T0 project's repo/), then TASK_DIR's visible/ overlay (a
# failing_then_green task's), then OVERLAY (a case's base_overlay; "" for
# none), commits the result as the first commit of a bare DEST on main, and
# prints that commit's tree. Any failed step fails it (it never commits a
# partial tree, whether or not the caller runs under errexit), and its work
# directory is removed on every path. Git runs with no system or global
# config, none of the caller's GIT_* repository variables, and a fixed
# author and date, so the tree and the commit depend on the files alone.

base_remote() {
  local repo="$1" task="$2" overlay="$3" dest="$4" work
  [[ -d "$repo" && -d "$task" && ! -e "$dest" ]] || { echo "base_remote: REPO and TASK_DIR must exist, DEST must not" >&2; return 2; }
  [[ -z "$overlay" || -d "$overlay" ]] || { echo "base_remote: OVERLAY is not a directory" >&2; return 2; }
  work="$(mktemp -d)" || return 1
  # Each step exits on failure itself: a caller's `tree="$(base_remote ...)" || die`
  # runs this where bash ignores errexit, even one set inside.
  (
    unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_NAMESPACE GIT_CEILING_DIRECTORIES
    export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=adc GIT_AUTHOR_EMAIL=adc@localhost \
      GIT_COMMITTER_NAME=adc GIT_COMMITTER_EMAIL=adc@localhost GIT_AUTHOR_DATE=2026-01-01T00:00:00Z GIT_COMMITTER_DATE=2026-01-01T00:00:00Z
    cp -R "$repo/." "$work/" || exit 1
    if [[ -d "$task/visible" ]]; then cp -R "$task/visible/." "$work/" || exit 1; fi
    if [[ -n "$overlay" ]]; then cp -R "$overlay/." "$work/" || exit 1; fi
    git -C "$work" init -q --initial-branch=main || exit 1
    git -C "$work" add -A || exit 1
    git -C "$work" commit -q -m base || exit 1
    git clone -q --bare "$work" "$dest" || exit 1
    git -C "$dest" rev-parse 'main^{tree}'
  )
  local rc=$?
  rm -rf "$work"
  return "$rc"
}
