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
# unchanged after it (inode and mtime). It builds the base remote from the
# T0 project (scripts/lib/base-remote.sh), starts the stub provider with
# the case's scenario on loopback, writes the registry with the stub's
# address, starts the installed adcd and bootstraps the operator. Then:
#   - CASE preflight (P24b) checks that much: the base remote's tree is the
#     one go-mini's measured baseline binds, the stub answers its scenario
#     and its usage query, and the operator's token reads the controller;
#   - any other CASE (fixtures/m1/cases/CASE/case.json) needs project and
#     task create (P22a-2), admission (P22a-3), launch (P22a-5), review
#     (P22a-6) and task wait and show (P22a-7): until they land it exits 2,
#     naming them, and claims nothing.
# Afterwards, every case stops adcd, verifies its log (adc dev verify-log),
# restarts it (its projections equal a replay, or it refuses to start),
# stops it again, and runs the shared secret scan (scripts/lib/secrets.sh)
# over the whole temporary directory. --routes live is P25's. Every wait is
# bounded. Exit 0 only when every check holds; 1 when one fails; 2 when the
# case cannot run.
set -euo pipefail

SECRETS_TAG=installed-cycle
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/secrets.sh
. "$here/lib/secrets.sh"
# shellcheck source=lib/base-remote.sh
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

# The real configuration, which no case may touch.
config="${HOME:-/nonexistent}/.config/adc"
before="$(stat -c '%i %Y' "$config" 2>/dev/null || echo absent)"

work="$(mktemp -d)"
cleanup() {
  local p
  for p in "${stub:-}" "${adcd:-}"; do [[ -z "$p" ]] || kill "$p" 2>/dev/null || true; done
  if ((keep)); then echo "installed-cycle: kept $work" >&2; else rm -rf "$work"; fi
}
trap cleanup EXIT
cd "$work"
umask 077
mkdir home state out
run_env=(env -i PATH=/usr/bin:/bin HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config" XDG_DATA_HOME="$work/home/.local/share"
  XDG_STATE_HOME="$work/home/.local/state" XDG_CACHE_HOME="$work/home/.cache" XDG_RUNTIME_DIR="$work/home/run" TMPDIR="$work")
sock="$work/state/adcd.sock"
step() { echo "installed-cycle: $case: $*"; }
adc() { "${run_env[@]}" ADC_SOCKET="$sock" timeout 60 "$prefix/bin/adc" "$@"; }

task="go-mini-feature-1" scenario="go-mini-feature-1"
if [[ -n "$case_file" ]]; then
  task="$(json "$case_file" d.task)" scenario="$(json "$case_file" d.scenario)"
fi

step "the base remote of $project's $task"
tree="$(base_remote "$fixtures/t0/$project/repo" "$fixtures/t0/$project/tasks/$task" "" "$work/remote.git")" || die "the base remote failed"
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
"$node" -e 'const fs = require("node:fs"); const d = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  for (const e of d.executors) if (e.destination.id === "stub") e.destination.endpoint = process.argv[2];
  fs.writeFileSync(process.argv[3], JSON.stringify(d))' "$fixtures/m1/config/registry-stub.json" "$base" "$work/registry.json"

step "the installed adcd"
"${run_env[@]}" "$prefix/bin/adcd" run --state-dir "$work/state" --resource-groups "$fixtures/m1/config/groups.json" \
  --registry "$work/registry.json" >"$work/out/adcd.log" 2>&1 &
adcd=$!
started() { kill -0 "$adcd" 2>/dev/null || die "adcd exited: $(head -c 400 "$work/out/adcd.log")"; grep -q '^adcd: started' "$work/out/adcd.log"; }
within 60 "adcd to start" started

step "bootstrapping the operator"
boot="$(<"$work/state/bootstrap.secret")"
known "$boot"
adc admin bootstrap --state-dir "$work/state" >"$work/out/bootstrap.out" 2>&1 || die "the bootstrap failed: $(head -c 400 "$work/out/bootstrap.out")"
tok="$(<"$work/home/.config/adc/token")"
known "$tok"

if [[ -z "$case_file" ]]; then
  step "preflight: the stub answers its scenario and its usage query; the operator reads the controller"
  reply="$(curl -sS --max-time 30 -H 'content-type: application/json' \
    -d '{"model":"stub-reviewer-1","messages":[{"role":"user","content":"preflight"}],"stream":false}' "$base/chat/completions")" ||
    die "the stub provider did not answer"
  id="$(sed -n 's/^{"id":"\([^"]*\)".*/\1/p' <<<"$reply")"
  [[ -n "$id" && "$reply" == *'\"decision\":\"go\"'* ]] || die "the stub's reply is not its scenario's: $(head -c 300 <<<"$reply")"
  curl -sS --max-time 30 -f "$base/usage/$id" >/dev/null || die "the stub's usage query failed for $id"
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
  wait "$adcd" || die "adcd stopped with an error: $(tail -c 400 "$work/out/adcd.log")"
  adcd=""
}
stop
adc dev verify-log --state "$work/state" >"$work/out/verify.out" 2>&1 || die "the log does not verify: $(head -c 400 "$work/out/verify.out")"
"${run_env[@]}" "$prefix/bin/adcd" run --state-dir "$work/state" --resource-groups "$fixtures/m1/config/groups.json" \
  --registry "$work/registry.json" >"$work/out/adcd.log" 2>&1 &
adcd=$!
within 60 "adcd to restart" started
stop
kill "$stub" 2>/dev/null || true
wait "$stub" 2>/dev/null || true
stub=""

step "no secret outside the operator's token file"
scan "" "$boot" "$work"
scan "$work/home/.config/adc/token" "$tok" "$work"
after="$(stat -c '%i %Y' "$config" 2>/dev/null || echo absent)"
[[ "$after" == "$before" ]] || die "the real ~/.config/adc changed: $before, now $after"

if [[ -n "${status:-}" ]]; then
  cannot "$case $why"
fi
step "ok"
