#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The installed smoke of M0 step 9 (docs/design/m0/14 PR 12), on an
# installed prefix (after scripts/install-check.sh): the installed adcd and
# adc used the way an operator would, under bwrap with only the system's
# /usr, the prefix (at /p) and this script mounted, a private /tmp and no
# network, so no source tree, home or real ~/.config/adc is reachable.
#
# It starts adcd on a new state directory, checks that a second adcd is
# refused, bootstraps the first operator (once), enrols a supervisor whose
# token goes to --token-out, grants it, and has it take the seat, add a
# note and release the seat; a scoped read returns only granted records;
# stale, refused and deferred commands give their registered reasons and
# exit codes; intake (P22a-2): the operator creates M1's py-mini project
# (its profile mounted read-only at /py-mini.json) and a task with an
# attachment, and cancels it, which the board shows in Done; adc doctor
# warns a supervisor run as the daemon uid and not an operator; the event
# stream shows the events in seq order. Then the
# grant and the supervisor's token are revoked, the revoked bearer gets
# HTTP 401, adcd stops cleanly on SIGTERM, its log verifies, and no secret
# appears in any log, output or state file besides the two token files.
# Then result 9 (14 PR 12; 15 S3): the installed adc-harness runs
# py-mini-feature-1 through the installed Node and fake adapter, with PATH
# limited to the prefix and the base system directories and any system node
# masked; its --json report must show a submitted result and a usage record,
# and it must print nothing on stderr. Every wait is bounded. Not covered
# yet: the CLI reads and `adc events --follow` (PR 10b), which are read here
# over HTTP.
#
# Usage: scripts/installed-smoke.sh PREFIX
set -euo pipefail

# The shared secret scan (scripts/lib/secrets.sh: secrets, die, known,
# scan, adc dev scan's), mounted at /secrets.sh inside the sandbox: the --inside run
# sources that, the outer run the file beside this script.
SECRETS_TAG=installed-smoke
secrets_lib="$(dirname "${BASH_SOURCE[0]}")/lib/secrets.sh"
[[ "${1:-}" == --inside && "${ADC_SMOKE_SANDBOX:-}" == 1 ]] && secrets_lib=/secrets.sh
# shellcheck source=scripts/lib/secrets.sh
. "$secrets_lib"

if [[ "${1:-}" != --inside ]]; then
  [[ $# -eq 1 ]] || die "usage: installed-smoke.sh PREFIX"
  prefix="$(cd "$1" && pwd)"
  [[ -x "$prefix/bin/adcd" && -x "$prefix/bin/adc" ]] || die "adcd and adc are not installed in $prefix"
  [[ -x "$prefix/bin/adc-harness" && -x "$prefix/lib/adc/node/bin/node" ]] || die "adc-harness and lib/adc are not installed in $prefix"
  for t in bwrap curl timeout; do command -v "$t" >/dev/null || die "$t is required"; done
  self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  profile="$(dirname "$self")/../fixtures/m1/profiles/py-mini.json"
  [[ -f "$profile" ]] || die "no M1 py-mini profile at $profile"
  sys=(--ro-bind /usr /usr)
  for d in bin sbin lib lib64 lib32; do
    if [[ -L "/$d" ]]; then sys+=(--symlink "$(readlink "/$d")" "/$d")
    elif [[ -d "/$d" ]]; then sys+=(--ro-bind "/$d" "/$d"); fi
  done
  # No system Node: one under /usr is masked, so only the installed one can run.
  for n in /usr/bin/node /usr/local/bin/node; do
    if [[ -e "$n" ]]; then sys+=(--ro-bind /dev/null "$n"); fi
  done
  exec env -i PATH=/usr/bin:/bin timeout --foreground --kill-after=10 300 bwrap --unshare-all --die-with-parent "${sys[@]}" \
    --ro-bind "$prefix" /p --ro-bind "$self" /smoke.sh --ro-bind "$(dirname "$self")/lib/secrets.sh" /secrets.sh --ro-bind "$profile" /py-mini.json --tmpfs /tmp --proc /proc --dev /dev \
    --chdir /tmp --setenv HOME /tmp --setenv TMPDIR /tmp --setenv ADC_SMOKE_SANDBOX 1 -- bash /smoke.sh --inside
fi
# Only ever inside the sandbox: it would otherwise write to the real /tmp.
[[ -e /smoke.sh && "${ADC_SMOKE_SANDBOX:-}" == 1 ]] || die "--inside runs only in the sandbox; run installed-smoke.sh PREFIX"

# Inside the sandbox from here on. The release's adc runs the secret scans (secrets.sh's known and scan).
adc_scan=/p/bin/adc
cd /tmp || die "no /tmp in the sandbox"
umask 077
mkdir state op sup out
mkdir -p sup/.config/adc
sock=/tmp/state/adcd.sock out=/tmp/out
step() { echo "installed-smoke: $*"; }

# typeid PREFIX: a fresh TypeID (a UUIDv7 in lowercase Crockford base32).
typeid() {
  local hex bits=00 id="" i n alphabet=0123456789abcdefghjkmnpqrstvwxyz
  hex="$(printf '%012x' "$(date +%s%3N)")$(od -An -N10 -tx1 /dev/urandom | tr -d ' \n')"
  hex="${hex:0:12}7${hex:13:3}$(printf '%x' $(((16#${hex:16:1} & 3) | 8)))${hex:17}"
  for ((i = 0; i < 32; i++)); do
    n=$((16#${hex:i:1}))
    bits+="$((n >> 3 & 1))$((n >> 2 & 1))$((n >> 1 & 1))$((n & 1))"
  done
  for ((i = 0; i < 26; i++)); do id+="${alphabet:$((2#${bits:i*5:5})):1}"; done
  echo "${1}_$id"
}

# adc_as HOME NAME ARGS...: run the installed adc as the actor whose token
# is under HOME; rc is its exit code, out/NAME.{out,err} what it printed.
adc_as() {
  local home="$1" name="$2"
  shift 2
  rc=0
  HOME="$home" ADC_SOCKET="$sock" timeout 60 /p/bin/adc "$@" >"$out/$name.out" 2>"$out/$name.err" </dev/null || rc=$?
}
# want CODE NAME [REASON]: the last adc exited CODE, with REASON in its JSON.
want() {
  [[ $rc -eq $1 ]] || die "$2: exit $rc, want $1: $(head -c 400 "$out/$2.err") $(head -c 400 "$out/$2.out")"
  [[ -z "${3:-}" ]] || grep -q "\"reason\":\"$3\"" "$out/$2.out" || die "$2: no reason $3 in its result: $(head -c 400 "$out/$2.out")"
}
# last_seq NAME: the seq of the last event an applied command recorded,
# which is the version of the object it created.
last_seq() { sed -n 's/.*"last_seq":\([0-9]*\).*/\1/p' "$out/$1.out"; }
# get NAME TOKEN PATH: GET PATH over the private socket with TOKEN as the
# bearer (from stdin, never argv); code is the HTTP status.
get() {
  code="$(printf 'Authorization: Bearer %s\n' "$2" | curl -sS --max-time 30 --unix-socket "$sock" -H @- \
    -o "$out/$1.out" -w '%{http_code}' "http://adcd$3")" || die "$1: GET $3 failed"
}
# within SECONDS WHAT COMMAND...: poll COMMAND every 0.1 s, at most SECONDS.
within() {
  local deadline=$((SECONDS + $1)) what="$2"
  shift 2
  until "$@"; do
    ((SECONDS < deadline)) || die "timed out waiting for $what"
    sleep 0.1
  done
}
alive() { kill -0 "$adcd" 2>/dev/null || die "adcd exited: $(head -c 400 "$out/adcd.log")"; }
started() { alive && grep -q '^adcd: started' "$out/adcd.log"; }

step "starting the installed adcd"
/p/bin/adcd run --state-dir /tmp/state >"$out/adcd.log" 2>&1 &
adcd=$!
within 60 "adcd to start" started
rc=0
timeout 60 /p/bin/adcd run --state-dir /tmp/state >"$out/adcd2.log" 2>&1 || rc=$?
if [[ $rc -ne 2 ]] || ! grep -q 'could_not_run/controller_already_running' "$out/adcd2.log"; then
  die "a second adcd: exit $rc, want 2 with could_not_run/controller_already_running"
fi

step "bootstrapping the first operator, once"
boot="$(<state/bootstrap.secret)"
known "$boot"
adc_as /tmp/op bootstrap admin bootstrap --state-dir /tmp/state
want 0 bootstrap
[[ ! -e state/bootstrap.secret && -f op/.config/adc/token ]] || die "the bootstrap left its secret or wrote no token"
rc=0
printf '%s\n' "$boot" | HOME=/tmp/op ADC_SOCKET="$sock" timeout 60 /p/bin/adc admin bootstrap --secret-stdin \
  >"$out/bootstrap2.out" 2>"$out/bootstrap2.err" || rc=$?
want 1 bootstrap2
grep -q "failed/confirmation_refused" "$out/bootstrap2.err" || die "a second bootstrap is not refused confirmation_refused"

step "enrolling a supervisor with --token-out"
sup="$(typeid act)" grant="$(typeid grt)" mine="$(typeid prj)" other="$(typeid prj)"
adc_as /tmp/op enrol admin enrol-agent --expect 0 --reason smoke --token-out /tmp/sup/.config/adc/token --json \
  --scope "{\"actor\":\"$sup\",\"name\":\"smoke supervisor\",\"declared_agent\":\"installed-smoke\"}"
want 0 enrol
[[ "$(stat -c %a sup/.config/adc/token)" == 600 ]] || die "the supervisor's token file is not 0600"
op_tok="$(<op/.config/adc/token)" sup_tok="$(<sup/.config/adc/token)"
known "$op_tok"
known "$sup_tok"

step "granting the supervisor the seat and notes, and reads in one project"
expires="$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)"
adc_as /tmp/op grant grant create --expect 0 --reason smoke --json --scope "{\"id\":\"$grant\",\"actor\":\"$sup\",\
\"projects\":\"*\",\"actions\":[\"seat.take\",\"seat.release\",\"note.add\"],\"reads\":{\"projects\":[\"$mine\"],\
\"kinds\":[\"note\"]},\"expires_at\":\"$expires\",\"confirmation_overrides\":{}}"
want 0 grant

step "the supervisor takes the seat and adds a note; stale, refused and deferred commands"
note() { echo "{\"project\":\"$1\",\"body\":\"$2\"}"; }
adc_as /tmp/sup take seat take --expect 0 --reason smoke --json
want 0 take
adc_as /tmp/sup stale seat take --expect 0 --reason smoke --json
want 4 stale stale_expected_state
adc_as /tmp/sup note notes add --expect 0 --reason smoke --json --scope "$(note "$mine" "smoke: granted")"
want 0 note
adc_as /tmp/sup operator-only grant revoke --expect "$(last_seq grant)" --reason smoke --json --scope "{\"grant\":\"$grant\"}"
want 1 operator-only operator_only
adc_as /tmp/op opnote notes add --expect 0 --reason smoke --json --scope "$(note "$other" "smoke: the operator's")"
want 0 opnote
adc_as /tmp/op deferred task request-review --expect 0 --reason smoke --json
want 2 deferred action_deferred

step "intake: a project and a task; the reconcile loop admits it; a second task is cancelled (P22a-2, P22a-4)"
# field NAME KEY: the first "KEY":"value" or "KEY":number in NAME's output.
field() { sed -n "s/.*\"$2\":\"\{0,1\}\([a-z0-9_]*\).*/\1/p" "$out/$1.out" | head -n 1; }
adc_as /tmp/op project project create --expect 0 --reason smoke --json --scope "$(cat /py-mini.json)"
want 0 project
prj="$(field project project)"
[[ "$prj" == prj_* ]] || die "project create: no project id: $(head -c 400 "$out/project.out")"
printf '{"project":"%s","title":"smoke","requirement":"r"}' "$prj" >/tmp/spec.json
printf 'def test_smoke():\n    pass\n' >/tmp/test_smoke.py
# create NAME: an operator's task create, its output under NAME; tsk and
# first are its task and its first seq.
create() {
  adc_as /tmp/op "$1" task create --expect 0 --reason smoke --json --scope-file /tmp/spec.json --attach tests/test_smoke.py=/tmp/test_smoke.py
  want 0 "$1"
  tsk="$(field "$1" task)" first="$(field "$1" first_seq)"
  [[ "$tsk" == tsk_* && -n "$first" ]] || die "task create: no task id: $(head -c 400 "$out/$1.out")"
  grep -q '"state":"ready"' "$out/$1.out" || die "task create: not ready: $(head -c 400 "$out/$1.out")"
}
create task
admitted() {
  adc_as /tmp/op board board --json
  [[ $rc -eq 0 ]] && grep -q '"admitted":true' "$out/board.out"
}
within 30 "the reconcile loop to admit the task" admitted
create task2
adc_as /tmp/op cancel task cancel --expect "$first" --reason smoke --json --scope "{\"task\":\"$tsk\"}"
want 0 cancel
adc_as /tmp/op board board --json
want 0 board
if ! grep -q '"column":"done"' "$out/board.out" || ! grep -q '"cancelled":true' "$out/board.out" || ! grep -q '"admitted":true' "$out/board.out"; then
  die "board: the first task is not admitted, or the second not cancelled in Done: $(head -c 400 "$out/board.out")"
fi

step "a scoped read returns only granted records"
get notes "$sup_tok" /v1/notes
[[ $code == 200 ]] || die "the supervisor's notes read: HTTP $code"
if ! grep -q "smoke: granted" "$out/notes.out" || grep -q "$other" "$out/notes.out"; then
  die "the supervisor's notes read is not exactly its granted project's"
fi
get whoami "$sup_tok" /v1/whoami
if [[ $code != 200 ]] || ! grep -q "\"$sup\"" "$out/whoami.out"; then die "the supervisor's whoami: HTTP $code"; fi

step "adc doctor warns a supervisor run as the daemon uid, and not an operator (04 D31)"
adc_as /tmp/sup doctor doctor --state-dir /tmp/state --json
want 0 doctor
f='{"check":"%s","detail":"[^"]*"}'
# shellcheck disable=SC2059
grep -q "\"warnings\":\[$(printf "$f,$f" daemon_uid state_dir)\],\"passed\":\[\],\"unseen\":\[$(printf "$f,$f" operator_token operator_token_elsewhere)\]" "$out/doctor.out" ||
  die "adc doctor as the daemon uid: not exactly the daemon_uid and state_dir warnings and an unseen operator token: $(head -c 600 "$out/doctor.out")"
adc_as /tmp/op doctor-op doctor --json
want 0 doctor-op
grep -q '"kind":"operator","listener":"private_socket","warnings":\[\],"passed":\[\],"unseen":\[\]' "$out/doctor-op.out" ||
  die "adc doctor checked an operator token: $(head -c 400 "$out/doctor-op.out")"

step "the event stream shows the events in seq order"
printf 'Authorization: Bearer %s\n' "$op_tok" | curl -sSN --max-time 120 --unix-socket "$sock" -H @- \
  -o "$out/events.out" -w '%{http_code}' http://adcd/v1/events >"$out/events.code" 2>"$out/events.err" &
stream=$!
streamed() { kill -0 "$stream" 2>/dev/null || die "the event stream ended: HTTP $(<"$out/events.code") $(head -c 400 "$out/events.err")"; grep -qs "\"project\":\"$other\"" "$out/events.out"; }
within 60 "the operator's note on the event stream" streamed
kill "$stream" 2>/dev/null || true
wait "$stream" 2>/dev/null || true
seqs="$(sed -n 's/^id: *//p' "$out/events.out")"
[[ -n "$seqs" && "$seqs" == "$(sort -n <<<"$seqs")" && -z "$(uniq -d <<<"$seqs")" ]] || die "the event stream is not in seq order"
for k in actor.created/v1 token.issued/v1 grant.created/v1 seat.taken/v1 note.added/v1; do
  grep -q "\"kind\":\"$k\"" "$out/events.out" || die "the event stream has no $k"
done

step "releasing the seat, revoking the grant and the supervisor's token"
get seat "$op_tok" /v1/seat
seat_v="$(grep -o '"version":[0-9]*' "$out/seat.out" | cut -d: -f2)"
if [[ $code != 200 || ! $seat_v =~ ^[0-9]+$ ]] || ! grep -q "\"holder\":\"$sup\"" "$out/seat.out"; then
  die "the operator's seat read: HTTP $code, not held by the supervisor"
fi
adc_as /tmp/sup release seat release --expect "$seat_v" --reason smoke --json
want 0 release
adc_as /tmp/op revoke-grant grant revoke --expect "$(last_seq grant)" --reason smoke --json --scope "{\"grant\":\"$grant\"}"
want 0 revoke-grant
adc_as /tmp/op revoke-token admin revoke-token --expect "$(last_seq enrol)" --reason smoke --json --scope "{\"token\":\"${sup_tok%%.*}\"}"
want 0 revoke-token
get revoked "$sup_tok" /v1/whoami
[[ $code == 401 ]] || die "the revoked bearer: HTTP $code, want 401"
adc_as /tmp/sup revoked-cli seat take --expect 0 --reason smoke
want 1 revoked-cli
grep -q "unauthenticated" "$out/revoked-cli.err" || die "the revoked token's command is not refused unauthenticated"

step "stopping adcd"
kill -TERM "$adcd"
stopped() { ! kill -0 "$adcd" 2>/dev/null; }
within 60 "adcd to stop" stopped
rc=0
wait "$adcd" || rc=$?
if [[ $rc -ne 0 ]] || [[ "$(tail -n 1 "$out/adcd.log")" != "adcd: stopped" ]]; then
  die "adcd stopped with exit $rc: $(head -c 400 "$out/adcd.log")"
fi
[[ ! -e "$sock" ]] || die "the stopped adcd left its socket"
timeout 60 /p/bin/adc dev verify-log --state /tmp/state >"$out/verify.out" 2>&1 || die "the stopped adcd's log does not verify"

step "no secret outside the token files"
scan "" "$boot" /tmp
scan /tmp/op/.config/adc/token "$op_tok" /tmp
scan /tmp/sup/.config/adc/token "$sup_tok" /tmp

step "result 9: py-mini-feature-1 through the installed harness, Node and fake adapter"
lib=/p/lib/adc
hpath="$lib/node/bin:/p/bin:/usr/bin:/bin"
[[ "$(PATH="$hpath" command -v node)" == "$lib/node/bin/node" ]] || die "node does not resolve to the installed one"
for n in /usr/bin/node /usr/local/bin/node; do [[ ! -s "$n" ]] || die "a system node is reachable at $n"; done
cp -R "$lib/fixtures/t0/py-mini/repo" /tmp/py-mini
timeout 60 env -i PATH="$hpath" HOME=/tmp "$lib/node/bin/node" \
  -p 'require(process.argv[1]).requirement' "$lib/fixtures/t0/py-mini/tasks/py-mini-feature-1/task.json" >/tmp/prompt.md ||
  die "reading py-mini-feature-1's requirement failed"
rc=0
env -i PATH="$hpath" HOME=/tmp TMPDIR=/tmp ADC_FAKE_SCENARIO=safe-output timeout --kill-after=10 150 /p/bin/adc-harness run --json \
  --start-timeout 30s --handshake-timeout 30s --result-timeout 60s --shutdown-timeout 10s \
  --prompt /tmp/prompt.md --workspace /tmp/py-mini --env ADC_FAKE_SCENARIO -- "$lib/node/bin/node" "$lib/adapters/fake/src/main.ts" \
  >"$out/harness.out" 2>"$out/harness.err" </dev/null || rc=$?
[[ $rc -eq 0 ]] || die "adc-harness: exit $rc: $(head -c 400 "$out/harness.err") $(head -c 400 "$out/harness.out")"
[[ ! -s "$out/harness.err" ]] || die "adc-harness wrote to stderr: $(head -c 400 "$out/harness.err")"
grep -q '"submitted":{"call":"cal_[0-9a-z]*","outcome":"found","reason":"completed_with_findings","kind":"patch","blob":"sha256:[0-9a-f]\{64\}"}' "$out/harness.out" ||
  die "adc-harness submitted no patch result: $(head -c 600 "$out/harness.out")"
grep -q '"usage":\[{"id":"msr_' "$out/harness.out" || die "adc-harness recorded no usage: $(head -c 600 "$out/harness.out")"
step "ok: bootstrap, enrolment, grant, seat, notes, reads, intake, the event stream, revocation and the fake adapter run (result 9) through the installed binaries"
