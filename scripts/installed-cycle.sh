#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The installed cycle harness (M1 slice plan §2.1, §2.3, §2.6; P24b), installed
# at <prefix>/lib/adc/scripts/installed-cycle.sh and run from outside any
# checkout, with the prefix's files only:
#
#   installed-cycle.sh PREFIX --routes stub --project go-mini --case CASE
#     [--expect candidate] [--sandbox-root DIR] [--podman PATH] [--setup-only] [--keep]
#
# Each case runs in a temporary directory, under env -i with a temporary
# HOME, XDG_* and state directory; the real ~/.config/adc must be
# unchanged after it (every path's inode, size, mtime and mode, and every
# file's sha256; absent before, absent after). adcd accepts its state and
# run directories only on ext2/ext3/ext4, xfs, btrfs or tmpfs, so the
# temporary directory is a private one (0700) under the first of $TMPDIR
# (else /tmp), $XDG_RUNTIME_DIR and /dev/shm on one of those; with none, the
# case cannot run (exit 2) and adcd is never started. It builds the base remote from the
# T0 project (scripts/lib/base-remote.sh). CASE preflight (P24b) then starts
# the stub provider with the case's scenario on loopback, writes the
# registry with the stub's address, starts the installed adcd and
# bootstraps the operator, and checks that much: the base remote's tree is
# the one go-mini's measured baseline binds, the stub answers its scenario
# and its usage query, and the operator's token reads the controller.
#
# Any other CASE (fixtures/m1/cases/CASE/case.json, --expect candidate:
# checkpoint (a)'s) builds its base remote with its base_overlay (every
# file and symlink of it in main, a NAME.hostile file as NAME) and writes
# its profile (the profile_overlay merged, a {canary} in it replaced by a
# canary minted for this run, recorded in out/canary: §2.7
# hostile-gate-output's gate command, not the driver, writes it into its
# results mount; then the run's base remote, and the stub as its one
# destination); --setup-only stops there (make ci). It then needs rootless
# Podman (--podman, default /usr/bin/podman), /dev/shm apart from the state
# directory's filesystem, and the profile's pinned image for this host's
# platform in the sandbox root (scripts/lib/sandbox-root.sh: --sandbox-root,
# one root the cases of a set share, else the case's own; the image pulled
# by digest into it once); a missing one refuses the case, exit 2, named
# (#226 entry 188). Then adcd runs the product sandbox under that root,
# holding a stub credential minted for the run unless the case withholds it,
# and the case runs (scripts/lib/cycle-case.sh): project and task create,
# the task read until it ends, and, once adcd replayed its store, the case's
# expect block (scripts/lib/cycle-expect.mjs). In stub mode review stays
# inactive (#226 entry 189): a case that expects the candidate ends with its
# gate completed and its review held review_inactive. Its last line is its
# summary row.
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
# shellcheck source=scripts/lib/sandbox-root.sh
. "$here/lib/sandbox-root.sh"

cannot() {
  echo "installed-cycle: could_not_run: $*" >&2
  exit 2
}
usage() {
  cannot "usage: installed-cycle.sh PREFIX --routes stub --project go-mini --case CASE [--expect candidate] [--sandbox-root DIR] [--podman PATH] [--setup-only] [--keep]
   or: installed-cycle.sh PREFIX --routes live --project repo --issue N --until candidate --bench FILE --registry FILE --groups FILE --acceptance REPO_PATH=FILE... --canary TEXT [--sandbox-root DIR] [--podman PATH] [--keep]"
}

[[ $# -ge 1 ]] || usage
prefix="$1"
shift
routes="" project="" case="" keep=0 expect="" shared="" setup_only=0 PODMAN=/usr/bin/podman
issue="" until_="" bench="" registry="" groups="" acceptance=() live_canary=""
while [[ $# -gt 0 ]]; do
  # A value flag given last, with no value, is a usage refusal (exit 2), never a failed shift (Opus #435 L1).
  case "$1" in --routes | --project | --case | --expect | --sandbox-root | --podman | --issue | --until | --bench | --registry | --groups | --acceptance | --canary) [[ $# -ge 2 ]] || usage ;; esac
  case "$1" in
    --routes) routes="$2" && shift 2 ;;
    --project) project="$2" && shift 2 ;;
    --case) case="$2" && shift 2 ;;
    --expect) expect="$2" && shift 2 ;;
    --sandbox-root) shared="$2" && shift 2 ;;
    --podman) PODMAN="$2" && shift 2 ;;
    --setup-only) setup_only=1 && shift ;;
    --issue) issue="$2" && shift 2 ;;
    --until) until_="$2" && shift 2 ;;
    --bench) bench="$2" && shift 2 ;;
    --registry) registry="$2" && shift 2 ;;
    --groups) groups="$2" && shift 2 ;;
    --acceptance) acceptance+=("$2") && shift 2 ;;
    --canary) live_canary="$2" && shift 2 ;;
    --keep) keep=1 && shift ;;
    *) usage ;;
  esac
done
[[ "$PODMAN" == /* ]] || usage
live=0
case "$routes" in
  stub)
    [[ -n "$project" && -n "$case" && -z "$issue$until_$bench$registry$groups$live_canary" && ${#acceptance[@]} -eq 0 ]] || usage
    [[ "$project" =~ ^[a-z0-9][a-z0-9-]{0,63}$ && "$case" =~ ^[a-z0-9][a-z0-9-]{0,63}$ ]] || usage
    [[ "$project" == go-mini ]] || cannot "--project $project: only go-mini has cycle cases"
    ;;
  live)
    # §2.3's live route (P25, D2): this repository's issue N, until the candidate.
    live=1
    [[ "$project" == repo && "$issue" =~ ^[1-9][0-9]{0,6}$ && "$until_" == candidate && -z "$case$expect" && $setup_only == 0 ]] || usage
    [[ -n "$bench" && -n "$registry" && -n "$groups" && -n "$live_canary" && ${#acceptance[@]} -gt 0 ]] || usage
    [[ ${#live_canary} -ge 16 ]] || cannot "--canary must be the acceptance's unpublished canary line, at least 16 characters"
    case="live-issue-$issue"
    for f in "$registry" "$groups"; do [[ -f "$f" && -r "$f" ]] || cannot "$f is not a readable file"; done
    registry="$(realpath -e "$registry")" groups="$(realpath -e "$groups")"
    [[ -d "$(dirname "$bench")" && ! -e "$bench" ]] || cannot "--bench $bench: its directory must exist and it must not"
    bench="$(realpath -m "$bench")"
    ;;
  *) usage ;;
esac
prefix="$(cd "$prefix" && pwd)" || cannot "no prefix $1"
lib="$prefix/lib/adc"
node="$lib/node/bin/node"
fixtures="$lib/fixtures"
for f in "$prefix/bin/adcd" "$prefix/bin/adc" "$node"; do [[ -x "$f" ]] || cannot "$f is not installed"; done
case_file="" profile_file="$fixtures/m1/profiles/$project.json" issue_dir=""
if ((live)); then
  profile_file="$fixtures/m1/profiles/auto-dev-cycle.json" issue_dir="$fixtures/m1/issues/$issue"
  [[ -f "$profile_file" && -f "$issue_dir/task.json" ]] || cannot "no issue $issue in $fixtures/m1/issues, or no auto-dev-cycle profile"
else
  [[ -f "$profile_file" && -d "$fixtures/t0/$project/repo" ]] || cannot "no project $project in $fixtures"
  if [[ "$case" != preflight ]]; then
    case_file="$fixtures/m1/cases/$case/case.json"
    [[ -f "$case_file" ]] || cannot "no case $case"
    [[ "$expect" == candidate ]] || cannot "--expect candidate is required: checkpoint (a)'s cases run until the candidate (§2.3)"
  fi
fi
# cycle: the run creates a task (a stub case, or the live route); not the preflight.
cycle=0
if [[ -n "$case_file" ]] || ((live)); then cycle=1; fi
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
# The runs killed before their own cleanup (SIGKILL runs no trap) left their
# work directories and sandbox roots, each recording its owner: removed
# first, what still runs on them stopped (a live owner's are never touched).
work_dirs_stale "${TMPDIR:-/tmp}" "${XDG_RUNTIME_DIR:-/nonexistent}" /dev/shm
sandbox_roots_stale
work="$(mktemp -d -p "$parent" adc-cycle.XXXXXXXX)" || cannot "mktemp in $parent failed"
owner_of $$ >"$work/.owner" || cannot "$work/.owner cannot be written"
cleanup() {
  local p i
  for p in "${stub:-}" "${adcd:-}" "${events:-}"; do
    [[ -n "$p" ]] || continue
    kill "$p" 2>/dev/null || continue
    for ((i = 0; i < 100; i++)); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  done
  if ((keep)); then
    # Kept on purpose: no owner left to go stale, so no later run's sweep
    # removes them; a shared root's owner files are its owner's (the set's),
    # never this case's to remove (Mythos #435 L4).
    rm -f "$work/.owner"
    if [[ -n "${root:-}" ]] && ((private)); then rm -f "$root/.owner" "$root/runs/.owner"; fi
    echo "installed-cycle: kept $work${root:+ and the sandbox root $root}" >&2
  elif [[ -n "${root:-}" ]]; then
    if ((private)); then sandbox_root_remove "$root"; else sandbox_root_sweep "$root"; fi
  fi
  ((keep)) || rm -rf "$work"
}
trap cleanup EXIT
# A signal exits through the EXIT trap: the run's processes stopped, its
# private sandbox root and work directory removed, as on any other exit.
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
fs_known "$work" || die "$work is not on ext2/ext3/ext4, xfs, btrfs or tmpfs"
cd "$work"
umask 077
mkdir home state out
run_env=(env -i PATH=/usr/bin:/bin HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config" XDG_DATA_HOME="$work/home/.local/share"
  XDG_STATE_HOME="$work/home/.local/state" XDG_CACHE_HOME="$work/home/.cache" XDG_RUNTIME_DIR="$work/home/run" TMPDIR="$work")
sock="$work/state/adcd.sock"
step() { echo "installed-cycle: $case: $*"; }
adc() { "${run_env[@]}" ADC_SOCKET="$sock" timeout 60 "$prefix/bin/adc" "$@"; }

task="go-mini-feature-1" scenario="go-mini-feature-1" overlay="" tree=""
if ((live)); then
  # shellcheck source=scripts/lib/cycle-live.sh
  . "$here/lib/cycle-live.sh"
  live_setup
fi
if [[ -n "$case_file" ]]; then
  task="$(json "$case_file" d.task)" scenario="$(json "$case_file" d.scenario)" want="$(json "$case_file" d.project)"
  [[ "$want" == "$project" ]] || cannot "case $case is $want's, not $project's"
  o="$(json "$case_file" d.base_overlay)"
  if [[ -n "$o" ]]; then overlay="$(dirname "$case_file")/$o"; fi
fi

if ((!live)); then
  step "the base remote of $project's $task"
  tree="$(base_remote "$fixtures/t0/$project/repo" "$fixtures/t0/$project/tasks/$task" "$overlay" "$work/remote.git")" || die "the base remote failed"
fi
if [[ -n "$overlay" ]]; then
  while IFS= read -r -d '' f; do
    f="${f#"$overlay"/}" f="${f%.hostile}"
    git --git-dir="$work/remote.git" cat-file -e "main:$f" 2>/dev/null || die "the base remote lacks the case's $f"
  done < <(find "$overlay" \( -type f -o -type l \) -print0)
fi
if [[ -n "$case_file" ]]; then
  step "the case's profile, its canary minted for this run"
  canary="ADC-CANARY-$case-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  printf '%s\n' "$canary" >"$work/out/canary"
  # The profile_overlay merged over the profile (RFC 7396), {canary} replaced in every string; then the run's
  # own: its base remote, and the stub as the one allowed destination.
  "$node" -e 'const fs = require("node:fs"); const [profile, file, canary, remote, out] = process.argv.slice(1);
    const merge = (t, p) => { if (p === null || typeof p !== "object" || Array.isArray(p)) return p;
      const r = t !== null && typeof t === "object" && !Array.isArray(t) ? { ...t } : {};
      for (const [k, v] of Object.entries(p)) { if (v === null) delete r[k]; else r[k] = merge(r[k], v); } return r; };
    const sub = (v) => typeof v === "string" ? v.replaceAll("{canary}", canary) : Array.isArray(v) ? v.map(sub)
      : v !== null && typeof v === "object" ? Object.fromEntries(Object.entries(v).map(([k, x]) => [k, sub(x)])) : v;
    const c = JSON.parse(fs.readFileSync(file, "utf8"));
    const p = sub(merge(JSON.parse(fs.readFileSync(profile, "utf8")), c.profile_overlay ?? {}));
    p.repo.url = remote; p.data_policy.allowed_destinations = ["stub"];
    fs.writeFileSync(out, JSON.stringify(p))' \
    "$fixtures/m1/profiles/$project.json" "$case_file" "$canary" "$work/remote.git" "$work/profile.json" || die "the case's profile failed"
  if grep -q '{canary}' "$case_file" && ! grep -qF "$canary" "$work/profile.json"; then die "the case's profile does not carry its canary"; fi
fi
if [[ "$project" == go-mini && -z "$case_file" ]]; then
  want="$(json "$fixtures/m1/stub/go-mini/baseline.json" d.base_tree)"
  [[ "$tree" == "$want" ]] || die "the base remote's tree is $tree, the measured baseline's $want"
fi
if ((setup_only)); then
  step "setup checked (--setup-only): the base remote and the case's profile; the run is make installed-cycle-stub's"
  exit 0
fi

# A case's prerequisites (§2.1; #226 entries 188, 189): rootless Podman,
# a /dev/shm apart from the state directory's filesystem (the sandbox's
# runs/), and the profile's pinned image for this host's platform in the
# sandbox root's storage (the shared root, --sandbox-root, or the case's
# own). Each missing one refuses the case (exit 2), named, before adcd
# starts.
root="" private=0
if ((cycle)); then
  arch="$(host_arch)" || cannot "this host's platform ($(uname -m)) has no prepared image"
  rootless_podman "$PODMAN" || cannot "rootless Podman ($PODMAN) is required: the product sandbox runs every build, accept and gate"
  [[ -d /dev/shm && "$(stat -c %d /dev/shm)" != "$(stat -c %d "$work")" ]] ||
    cannot "the sandbox's runs/ needs /dev/shm on a filesystem apart from the state directory's ($work)"
  [[ -n "$shared" ]] || private=1
  root="$(sandbox_root "$shared")" || cannot "the sandbox root ${shared:-under /tmp} cannot be made"
  pin="$(json "$profile_file" d.toolchain.images.go)"
  step "the pinned image $pin for linux/$arch in the sandbox root $root"
  rc=0
  root_image "$root" "$pin" "$arch" >/dev/null 2>"$work/out/image.err" || rc=$?
  case "$rc" in
    0) ;;
    2) cannot "the pinned image $pin has no instance for linux/$arch" ;;
    *) cannot "the pinned image $pin cannot be pulled for linux/$arch: $(tr '\n' ' ' <"$work/out/image.err" | head -c 300)" ;;
  esac
fi

groups_file="$fixtures/m1/config/groups.json" capture=()
if ((live)); then
  cp "$registry" "$work/registry.json"
  groups_file="$groups" capture=(--capture-egress)
else
  step "the stub provider with scenario $scenario"
  # The log exists from the start (the stub appends on its first request), so a missing one is a broken run, never "no requests".
  : >"$work/out/stub.jsonl"
  "${run_env[@]}" "$node" "$fixtures/m1/stub-provider/server.mjs" --scenario "$fixtures/m1/stub-provider/scenarios/$scenario.json" \
    --listen 127.0.0.1:0 --log "$work/out/stub.jsonl" --barriers "$work/out" >"$work/out/stub.out" 2>"$work/out/stub.err" &
  stub=$!
  record_pid "$work" "$stub"
  listening() { kill -0 "$stub" 2>/dev/null || die "the stub provider exited: $(head -c 400 "$work/out/stub.err")"; grep -q '^listening ' "$work/out/stub.out"; }
  within 30 "the stub provider" listening
  base="$(sed -n 's/^listening //p' "$work/out/stub.out")"
  # shellcheck disable=SC2016 # the single-quoted program is JavaScript for Node: its ${...} are template literals, not shell expansions
  n="$("$node" -e 'const fs = require("node:fs"); const d = JSON.parse(fs.readFileSync(process.argv[1], "utf8")); let n = 0;
    for (const e of d.executors) if (e.destination.id === "stub") { e.destination.endpoint = process.argv[2]; n++; }
    fs.writeFileSync(process.argv[3], JSON.stringify(d)); console.log(`${n} ${d.executors.length}`)' "$fixtures/m1/config/registry-stub.json" "$base" "$work/registry.json")"
  [[ "$n" =~ ^([1-9][0-9]*)\ ([0-9]+)$ && "${BASH_REMATCH[1]}" == "${BASH_REMATCH[2]}" ]] ||
    die "the registry's executors are not all the stub's (rewritten, of all): $n"
fi

# Each run of adcd writes its own log: out/adcd-1.log, out/adcd-2.log. A
# case's adcd runs the product sandbox under its root, and holds the stub's
# credential (ADC_CREDENTIAL_STUB, minted per run), unless the case
# withholds it (§2.3 assertion 6). The live route's holds the registry's
# credentials from the caller's environment (live_setup: each named, never
# printed), and captures what it sends (an exit run, §8 item 4).
run=0 cred=() sandbox=()
if ((cycle)); then sandbox=(--sandbox-root "$root" --podman "$PODMAN"); fi
if ((live)); then
  cred=("${live_cred[@]}")
elif [[ -n "$case_file" ]]; then
  stub_cred="$(head -c 32 /dev/urandom | base64 -w0 | tr '+/' '-_' | tr -d =)"
  known "$stub_cred"
  [[ "$(json "$case_file" 'd.credential_withheld === true')" == true ]] || cred=(ADC_CREDENTIAL_STUB="$stub_cred")
fi
start_adcd() {
  run=$((run + 1))
  log="$work/out/adcd-$run.log"
  "${run_env[@]}" "${cred[@]}" "$prefix/bin/adcd" run --state-dir "$work/state" --resource-groups "$groups_file" \
    --registry "$work/registry.json" "${sandbox[@]}" "${capture[@]}" >"$log" 2>&1 &
  adcd=$!
  record_pid "$work" "$adcd"
}
# how STATUS: an exit status as the shell gives it, a signal's named (128+N:
# SIGTERM is "signal TERM"), so a process killed before it logs anything is
# told from one that failed (an empty log says neither).
how() { if (($1 > 128)); then echo "signal $(kill -l $(($1 - 128)) 2>/dev/null || echo $(($1 - 128)))"; else echo "exit $1"; fi; }
started() {
  local rc=0
  if ! kill -0 "$adcd" 2>/dev/null; then
    wait "$adcd" || rc=$?
    adcd=""
    die "adcd exited ($(how "$rc")): $(head -c 400 "$log")"
  fi
  grep -q "^adcd: started${1:-}" "$log"
}
step "the installed adcd"
start_adcd
within 60 "adcd to start" started

step "bootstrapping the operator"
boot="$(<"$work/state/bootstrap.secret")"
known "$boot"
adc admin bootstrap --state-dir "$work/state" >"$work/out/bootstrap.out" 2>&1 || die "the bootstrap failed: $(head -c 400 "$work/out/bootstrap.out")"
tok="$(<"$work/home/.config/adc/token")"
known "$tok"

if ((!cycle)); then
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
  # shellcheck source=scripts/lib/cycle-case.sh
  . "$here/lib/cycle-case.sh"
  if ((live)); then live_case; else cycle_case; fi
fi

step "stopping adcd; its log verifies; a restart replays it"
stop() {
  kill -TERM "$adcd"
  stopped() { ! kill -0 "$adcd" 2>/dev/null; }
  within 60 "adcd to stop" stopped
  local rc=0
  wait "$adcd" || rc=$?
  ((rc == 0)) || die "adcd stopped with an error ($(how "$rc")): $(tail -c 400 "$log")"
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
if ((cycle)); then cycle_check "$seq"; fi
# The live route's benchmark only from the restarted adcd (#226 entry 199).
if ((live)); then live_bench; fi
stop
[[ "$(find "$work" -name bootstrap.secret -printf '%p %i\n' | LC_ALL=C sort)" == "$secrets_before" ]] ||
  die "the restart wrote a new bootstrap secret: it did not replay the store"
kill "$stub" 2>/dev/null || true
wait "$stub" 2>/dev/null || true
stub=""

if [[ -n "$case_file" ]]; then
  step "no canary path under the work directory, the sandbox root and its runs/, or beside either"
  while IFS= read -r name; do
    [[ -z "$name" ]] && continue
    for where in work root; do
      rc=0
      if [[ "$where" == work ]]; then found="$(canary_path "$work" "$name")" || rc=$?; else found="$(root_canary_path "$root" "$name")" || rc=$?; fi
      case "$rc" in
        0) ;;
        1) die "a canary path exists: $found" ;;
        *) die "canary_path could not check $name under the $where (exit $rc)" ;;
      esac
    done
  done < <(json "$case_file" '(d.expect.canary_paths ?? []).join("\n")')
fi
step "no secret outside the operator's token file"
scan "" "$boot" "$work"
scan "$work/home/.config/adc/token" "$tok" "$work"
# The stub's per-run credential: adcd reads it from its environment only, so it is nowhere under the work directory (Mythos #435 M2),
# nor in the sandbox root and its runs/, where a container's config, environment and run files are (Opus #438 N1).
# root_scan SECRET: the latter, before the cleanup removes a private root.
root_scan() {
  local rc=0 found
  found="$(root_secret_scan "$root" "$1")" || rc=$?
  case "$rc" in
    0) ;;
    1) die "a secret appears in the sandbox root: $(tr '\n' ' ' <<<"$found")" ;;
    *) die "the sandbox root $root could not be searched for a secret (exit $rc)" ;;
  esac
}
if [[ -n "${stub_cred:-}" ]]; then
  scan "" "$stub_cred" "$work"
  root_scan "$stub_cred"
fi
if ((live)); then live_after; fi
after="$(snapshot)" || die "$config cannot be read"
[[ "$after" == "$before" ]] || die "the real ~/.config/adc changed"

step "ok"
if ((cycle)); then echo "installed-cycle: summary: $summary"; fi
