#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The installed cycle harness (M1 slice plan §2.1, §2.6; P24), installed at
# <prefix>/lib/adc/scripts/installed-cycle.sh and run from outside any
# checkout, with the prefix's files only:
#
#   installed-cycle.sh PREFIX --routes stub --project go-mini --case CASE [--keep]
#
# Each case runs in a temporary directory, under env -i with a temporary
# HOME, XDG_* and state directory; the real ~/.config/adc must be
# unchanged after it (every path's inode, size, mtime and mode, and every
# file's sha256; absent before, absent after). adcd accepts its state and
# run directories only on ext2/ext3/ext4, xfs, btrfs or tmpfs, so the
# temporary directory is a private one (0700) under the first of $TMPDIR
# (else /tmp), $XDG_RUNTIME_DIR and /dev/shm on one of those; with none, the
# case cannot run (exit 2) and adcd is never started. It builds the base remote from the
# T0 project (scripts/lib/base-remote.sh), starts the stub provider with
# the case's scenario on loopback, writes the registry with the stub's
# address, starts the installed adcd and bootstraps the operator. Then:
#   - CASE preflight (P24b) checks that much: the base remote's tree is the
#     one go-mini's measured baseline binds, the stub answers its scenario
#     and its usage query, and the operator's token reads the controller;
#   - any other CASE (fixtures/m1/cases/CASE/case.json) builds its base
#     remote with its base_overlay and writes its profile (the profile_overlay
#     merged, a {canary} in it replaced by a canary minted for this run,
#     out/canary: §2.7 hostile-gate-output's gate writes it into its results
#     mount); it then needs project and
#     task create (P22a-2), admission (P22a-3), launch (P22a-5), review
#     (P22a-6) and task wait and show (P22a-7): until they land it exits 2,
#     naming them, and claims nothing.
# Afterwards, every case stops adcd, verifies its log (adc dev verify-log,
# whose head is seq N > 0), restarts it (it must report its event log
# verified to seq N, with no new bootstrap secret: it replayed the same
# store), stops it again, checks that no path under the work directory, or
# beside it, is named as one of the case's canary_paths (§2.7), and runs the shared secret scan
# (scripts/lib/secrets.sh) over the whole temporary directory, both runs'
# logs (out/adcd-1.log, out/adcd-2.log) included. --routes live is P25's. Every wait is
# bounded. Exit 0 only when every check holds; 1 when one fails; 2 when the
# case cannot run.
set -euo pipefail

SECRETS_TAG=installed-cycle
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/secrets.sh
. "$here/lib/secrets.sh"
# shellcheck source=scripts/lib/base-remote.sh
. "$here/lib/base-remote.sh"

cannot() {
  echo "installed-cycle: could_not_run: $*" >&2
  exit 2
}
usage() { cannot "usage: installed-cycle.sh PREFIX --routes stub --project go-mini --case CASE [--keep]"; }

[[ $# -ge 1 ]] || usage
prefix="$1"
shift
routes="" project="" case="" keep=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --routes) routes="${2:-}" && shift 2 ;;
    --project) project="${2:-}" && shift 2 ;;
    --case) case="${2:-}" && shift 2 ;;
    --keep) keep=1 && shift ;;
    *) usage ;;
  esac
done
[[ -n "$routes" && -n "$project" && -n "$case" ]] || usage
[[ "$routes" == stub ]] || cannot "--routes $routes: live routes are P25's"
[[ "$project" =~ ^[a-z0-9][a-z0-9-]{0,63}$ && "$case" =~ ^[a-z0-9][a-z0-9-]{0,63}$ ]] || usage
[[ "$project" == go-mini ]] || cannot "--project $project: only go-mini has cycle cases"
prefix="$(cd "$prefix" && pwd)" || cannot "no prefix $1"
lib="$prefix/lib/adc"
node="$lib/node/bin/node"
fixtures="$lib/fixtures"
for f in "$prefix/bin/adcd" "$prefix/bin/adc" "$node"; do [[ -x "$f" ]] || cannot "$f is not installed"; done
[[ -f "$fixtures/m1/profiles/$project.json" && -d "$fixtures/t0/$project/repo" ]] || cannot "no project $project in $fixtures"
case_file=""
if [[ "$case" != preflight ]]; then
  case_file="$fixtures/m1/cases/$case/case.json"
  [[ -f "$case_file" ]] || cannot "no case $case"
fi
for t in git curl timeout; do command -v "$t" >/dev/null || cannot "$t is required"; done

# json FILE EXPR: EXPR of the JSON document in FILE (d), by the installed Node.
json() { "$node" -e 'const d = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8")); console.log(eval(process.argv[2]) ?? "")' "$1" "$2"; }
# within SECONDS WHAT COMMAND...: poll COMMAND every 0.1 s, at most SECONDS.
within() {
  local deadline=$((SECONDS + $1)) what="$2"
  shift 2
  until "$@"; do
    ((SECONDS < deadline)) || die "timed out waiting for $what"
    sleep 0.1
  done
}

# The real configuration, which no case may touch: every path's inode, size,
# mtime and mode, and every file's sha256, or "absent".
config="${HOME:-/nonexistent}/.config/adc"
snapshot() {
  if [[ -e "$config" || -L "$config" ]]; then
    find "$config" -printf '%p %i %s %T@ %m\n' | LC_ALL=C sort
    find "$config" -type f -exec sha256sum -- {} + | LC_ALL=C sort
  else
    echo absent
  fi
}
before="$(snapshot)" || cannot "$config cannot be read"

# The work directory holds adcd's state and run directories: only a
# filesystem adcd accepts (internal/daemon/server.go: aclFilesystems), and
# no fallback to any other.
fs_known() {
  case "$(stat -f -c %T -- "$1" 2>/dev/null)" in
    ext2/ext3 | xfs | btrfs | tmpfs) return 0 ;;
    *) return 1 ;;
  esac
}
parent="" tried=""
for d in "${TMPDIR:-/tmp}" "${XDG_RUNTIME_DIR:-}" /dev/shm; do
  [[ -n "$d" ]] || continue
  if [[ -d "$d" && -w "$d" ]] && fs_known "$d"; then
    parent="$d"
    break
  fi
  tried+=" $d ($(stat -f -c %T -- "$d" 2>/dev/null || echo missing))"
done
[[ -n "$parent" ]] || cannot "no writable temporary directory on ext2/ext3/ext4, xfs, btrfs or tmpfs, which adcd's state needs; tried:$tried"
work="$(mktemp -d -p "$parent")" || cannot "mktemp in $parent failed"
cleanup() {
  local p i
  for p in "${stub:-}" "${adcd:-}"; do
    [[ -n "$p" ]] || continue
    kill "$p" 2>/dev/null || continue
    for ((i = 0; i < 100; i++)); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  done
  if ((keep)); then echo "installed-cycle: kept $work" >&2; else rm -rf "$work"; fi
}
trap cleanup EXIT
fs_known "$work" || die "$work is not on ext2/ext3/ext4, xfs, btrfs or tmpfs"
cd "$work"
umask 077
mkdir home state out
run_env=(env -i PATH=/usr/bin:/bin HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config" XDG_DATA_HOME="$work/home/.local/share"
  XDG_STATE_HOME="$work/home/.local/state" XDG_CACHE_HOME="$work/home/.cache" XDG_RUNTIME_DIR="$work/home/run" TMPDIR="$work")
sock="$work/state/adcd.sock"
step() { echo "installed-cycle: $case: $*"; }
adc() { "${run_env[@]}" ADC_SOCKET="$sock" timeout 60 "$prefix/bin/adc" "$@"; }

task="go-mini-feature-1" scenario="go-mini-feature-1" overlay=""
if [[ -n "$case_file" ]]; then
  task="$(json "$case_file" d.task)" scenario="$(json "$case_file" d.scenario)" want="$(json "$case_file" d.project)"
  [[ "$want" == "$project" ]] || cannot "case $case is $want's, not $project's"
  o="$(json "$case_file" d.base_overlay)"
  if [[ -n "$o" ]]; then overlay="$(dirname "$case_file")/$o"; fi
fi

step "the base remote of $project's $task"
tree="$(base_remote "$fixtures/t0/$project/repo" "$fixtures/t0/$project/tasks/$task" "$overlay" "$work/remote.git")" || die "the base remote failed"
if [[ -n "$overlay" ]]; then
  while IFS= read -r -d '' f; do
    git --git-dir="$work/remote.git" cat-file -e "main:${f#"$overlay"/}" 2>/dev/null || die "the base remote lacks the case's ${f#"$overlay"/}"
  done < <(find "$overlay" -type f -print0)
fi
if [[ -n "$case_file" ]]; then
  step "the case's profile, its canary minted for this run"
  canary="ADC-CANARY-$case-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  printf '%s\n' "$canary" >"$work/out/canary"
  # The profile_overlay merged over the profile (RFC 7396), {canary} replaced in every string.
  "$node" -e 'const fs = require("node:fs"); const [profile, file, canary, out] = process.argv.slice(1);
    const merge = (t, p) => { if (p === null || typeof p !== "object" || Array.isArray(p)) return p;
      const r = t !== null && typeof t === "object" && !Array.isArray(t) ? { ...t } : {};
      for (const [k, v] of Object.entries(p)) { if (v === null) delete r[k]; else r[k] = merge(r[k], v); } return r; };
    const sub = (v) => typeof v === "string" ? v.replaceAll("{canary}", canary) : Array.isArray(v) ? v.map(sub)
      : v !== null && typeof v === "object" ? Object.fromEntries(Object.entries(v).map(([k, x]) => [k, sub(x)])) : v;
    const c = JSON.parse(fs.readFileSync(file, "utf8"));
    fs.writeFileSync(out, JSON.stringify(sub(merge(JSON.parse(fs.readFileSync(profile, "utf8")), c.profile_overlay ?? {}))))' \
    "$fixtures/m1/profiles/$project.json" "$case_file" "$canary" "$work/profile.json" || die "the case's profile failed"
  if grep -q '{canary}' "$case_file" && ! grep -qF "$canary" "$work/profile.json"; then die "the case's profile does not carry its canary"; fi
fi
if [[ "$project" == go-mini && -z "$case_file" ]]; then
  want="$(json "$fixtures/m1/stub/go-mini/baseline.json" d.base_tree)"
  [[ "$tree" == "$want" ]] || die "the base remote's tree is $tree, the measured baseline's $want"
fi

step "the stub provider with scenario $scenario"
"${run_env[@]}" "$node" "$fixtures/m1/stub-provider/server.mjs" --scenario "$fixtures/m1/stub-provider/scenarios/$scenario.json" \
  --listen 127.0.0.1:0 --log "$work/out/stub.jsonl" --barriers "$work/out" >"$work/out/stub.out" 2>"$work/out/stub.err" &
stub=$!
listening() { kill -0 "$stub" 2>/dev/null || die "the stub provider exited: $(head -c 400 "$work/out/stub.err")"; grep -q '^listening ' "$work/out/stub.out"; }
within 30 "the stub provider" listening
base="$(sed -n 's/^listening //p' "$work/out/stub.out")"
# shellcheck disable=SC2016 # the single-quoted program is JavaScript for Node: its ${...} are template literals, not shell expansions
n="$("$node" -e 'const fs = require("node:fs"); const d = JSON.parse(fs.readFileSync(process.argv[1], "utf8")); let n = 0;
  for (const e of d.executors) if (e.destination.id === "stub") { e.destination.endpoint = process.argv[2]; n++; }
  fs.writeFileSync(process.argv[3], JSON.stringify(d)); console.log(`${n} ${d.executors.length}`)' "$fixtures/m1/config/registry-stub.json" "$base" "$work/registry.json")"
[[ "$n" =~ ^([1-9][0-9]*)\ ([0-9]+)$ && "${BASH_REMATCH[1]}" == "${BASH_REMATCH[2]}" ]] ||
  die "the registry's executors are not all the stub's (rewritten, of all): $n"

# Each run of adcd writes its own log: out/adcd-1.log, out/adcd-2.log.
run=0
start_adcd() {
  run=$((run + 1))
  log="$work/out/adcd-$run.log"
  "${run_env[@]}" "$prefix/bin/adcd" run --state-dir "$work/state" --resource-groups "$fixtures/m1/config/groups.json" \
    --registry "$work/registry.json" >"$log" 2>&1 &
  adcd=$!
}
started() { kill -0 "$adcd" 2>/dev/null || die "adcd exited: $(head -c 400 "$log")"; grep -q "^adcd: started${1:-}" "$log"; }
step "the installed adcd"
start_adcd
within 60 "adcd to start" started

step "bootstrapping the operator"
boot="$(<"$work/state/bootstrap.secret")"
known "$boot"
adc admin bootstrap --state-dir "$work/state" >"$work/out/bootstrap.out" 2>&1 || die "the bootstrap failed: $(head -c 400 "$work/out/bootstrap.out")"
tok="$(<"$work/home/.config/adc/token")"
known "$tok"

if [[ -z "$case_file" ]]; then
  step "preflight: the stub answers its scenario and its usage query; the operator reads the controller"
  reply="$(curl -q -sS --noproxy '*' --max-time 30 -H 'content-type: application/json' \
    -d '{"model":"stub-reviewer-1","messages":[{"role":"user","content":"preflight"}],"stream":false}' "$base/chat/completions")" ||
    die "the stub provider did not answer"
  id="$(sed -n 's/^{"id":"\([^"]*\)".*/\1/p' <<<"$reply")"
  [[ -n "$id" && "$reply" == *'\"decision\":\"go\"'* ]] || die "the stub's reply is not its scenario's: $(head -c 300 <<<"$reply")"
  curl -q -sS --noproxy '*' --max-time 30 -f "$base/usage/$id" >/dev/null || die "the stub's usage query failed for $id"
  adc board --json >"$work/out/board.out" 2>&1 || die "adc board failed: $(head -c 400 "$work/out/board.out")"
  grep -q '"resource":"board"' "$work/out/board.out" || die "adc board: $(head -c 400 "$work/out/board.out")"
else
  status=2
  why="needs project and task create (P22a-2), admission (P22a-3), launch (P22a-5), review (P22a-6) and task wait and show (P22a-7)"
fi

step "stopping adcd; its log verifies; a restart replays it"
stop() {
  kill -TERM "$adcd"
  stopped() { ! kill -0 "$adcd" 2>/dev/null; }
  within 60 "adcd to stop" stopped
  wait "$adcd" || die "adcd stopped with an error: $(tail -c 400 "$log")"
  adcd=""
}
stop
adc dev verify-log --state "$work/state" >"$work/out/verify.out" 2>&1 || die "the log does not verify: $(head -c 400 "$work/out/verify.out")"
seq="$(sed -n 's/^[a-z_]*\/[a-z_]*: head \([0-9][0-9]*\) .*/\1/p' "$work/out/verify.out" | head -n 1)"
if [[ -z "$seq" ]] || ((seq <= 0)); then
  die "verify-log reports no head past seq 0: $(head -c 400 "$work/out/verify.out")"
fi
secrets_before="$(find "$work" -name bootstrap.secret -printf '%p %i\n' | LC_ALL=C sort)"
start_adcd
within 60 "adcd to restart" started "; event log verified to seq $seq;"
stop
[[ "$(find "$work" -name bootstrap.secret -printf '%p %i\n' | LC_ALL=C sort)" == "$secrets_before" ]] ||
  die "the restart wrote a new bootstrap secret: it did not replay the store"
kill "$stub" 2>/dev/null || true
wait "$stub" 2>/dev/null || true
stub=""

if [[ -n "$case_file" ]]; then
  step "no canary path under the work directory or beside it"
  while IFS= read -r name; do
    [[ -z "$name" ]] && continue
    found="$(canary_path "$work" "$name")" || die "a canary path exists: $found"
  done < <(json "$case_file" '(d.expect.canary_paths ?? []).join("\n")')
fi
step "no secret outside the operator's token file"
scan "" "$boot" "$work"
scan "$work/home/.config/adc/token" "$tok" "$work"
after="$(snapshot)" || die "$config cannot be read"
[[ "$after" == "$before" ]] || die "the real ~/.config/adc changed"

if [[ -n "${status:-}" ]]; then
  cannot "$case $why"
fi
step "ok"
