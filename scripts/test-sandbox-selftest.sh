#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The test sandbox's self-test (docs/design/m0/11, M0 exit check
# "Containment"). Runs the containment tests in test/sandbox twice, with a
# fresh private scratch area, decoy credentials and decoy proxy settings in the
# host environment, and a listener on the host's loopback:
#   1. inside the sandbox: every check must pass and the wrapper must see no
#      breach;
#   2. with no containment (backend none): every check must FAIL and the
#      wrapper must report a breach. This proves each check can see the red it
#      guards against. The unsandboxed run writes only into a decoy under /tmp
#      and a probe file it removes again.
#
# Usage: scripts/test-sandbox-selftest.sh [--backend auto|bwrap]
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
backend=auto
case "${1:-}" in
  "") ;;
  --backend) backend="${2:?}" ;;
  *) printf 'usage: %s [--backend auto|bwrap]\n' "$0" >&2; exit 64 ;;
esac

# Checks whose red needs nothing from the host but the harness itself.
checks=(
  TestSentinelUnreachable/go_direct_write
  TestSentinelUnreachable/child_shell_write
  TestSentinelUnreachable/node_write
  TestSentinelUnreachable/symlink_write
  TestRealHomeAbsent
  TestRepoReadOnly
  TestNoProtectedOrRepoAlias
  TestExactEnv
  TestNoForeignProcessEnv
  TestNoCredentialEnv
  TestNoHostLoopback
)
# Checks whose red needs outside access from the host, each proven separately.
net_checks=(TestNoExternalTCP TestNoDNS)
# Checks proven red only by a mutation (a host's own mounts vary, so the
# unsandboxed control cannot prove them).
mutation_checks=(TestOnlyExpectedMounts)
# Single-property mutations (bwrap only): mutation=the exact set of checks it
# must turn red; every other check must still pass.
mutations=(
  expose-home=TestNoProtectedOrRepoAlias+TestOnlyExpectedMounts
  expose-state=TestNoProtectedOrRepoAlias+TestOnlyExpectedMounts
  repo-alias=TestNoProtectedOrRepoAlias+TestOnlyExpectedMounts
  repo-writable=TestRepoReadOnly+TestOnlyExpectedMounts   # the writable bind is a second mount at /src
  extra-mount=TestOnlyExpectedMounts
  pseudo-mount=TestOnlyExpectedMounts
  type-change=TestOnlyExpectedMounts
  missing-mount=TestOnlyExpectedMounts
)

out="$(mktemp -d)"
listener_pid=""
# The provisioned module cache is read-only (go mod download), so make it
# writable before removing it.
trap 'chmod -R u+w "$out" 2>/dev/null; rm -rf "$out"; [[ -n "$listener_pid" ]] && kill "$listener_pid" 2>/dev/null; true' EXIT

# A host-side TCP listener on 127.0.0.1; it prints its port, then serves.
node -e 'require("net").createServer(s => s.end()).listen(0, "127.0.0.1", function () { console.log(this.address().port) })' \
  >"$out/port" &
listener_pid=$!
for _ in $(seq 50); do [[ -s "$out/port" ]] && break; sleep 0.1; done
port="$(head -n1 "$out/port")"
[[ "$port" =~ ^[0-9]+$ ]] || { echo "setup: the host loopback listener did not start" >&2; exit 2; }

fail=0
host_dns=0 host_tcp=0
if getent hosts example.com >/dev/null 2>&1; then host_dns=1; fi
if timeout 3 bash -c '</dev/tcp/1.1.1.1/443' 2>/dev/null; then host_tcp=1; fi

# run <label> <wrapper args...>: go test -json output, wrapper stderr, exit code.
# Each run used to compile the test's dependencies into an empty build cache,
# ten cold builds per self-test at full parallelism; with two CI jobs on one
# small VM, go's compiler got OOM-killed and runs reported nothing. Now the
# bubblewrap runs share one fresh scratch area (the unsandboxed control keeps
# its own, so it never writes a cache that sandboxed runs mount) and go builds
# one action at a time (-p 1): a cold build holds one compiler, not one per CPU.
run() {
  local label="$1"; shift
  local rc=0 scratch="$out/scratch-bwrap"
  [[ "$label" == control ]] && scratch="$out/scratch-control"
  env ANTHROPIC_API_KEY=decoy-not-a-key ADC_DECOY_TOKEN=decoy-not-a-token \
    FTP_PROXY=http://decoy:decoy@127.0.0.1:9 NO_PROXY=decoy.invalid \
    ADC_SELFTEST_LOOPBACK_PORT="$port" ADC_TEST_SCRATCH="$scratch" ADC_TEST_PROVISION="$out/provision" \
    "$repo_root/scripts/test-sandbox.sh" "$@" --env ADC_SELFTEST_LOOPBACK_PORT \
    -- go test -p 1 -count=1 -json ./test/sandbox/ >"$out/$label.json" 2>"$out/$label.err" || rc=$?
  printf '%s\n' "$rc" >"$out/$label.rc"
}

# action <label> <test>: the final action (pass|fail|skip) go test reported.
action() {
  jq -r --arg t "$2" 'select(.Test == $t and (.Action == "pass" or .Action == "fail" or .Action == "skip")) | .Action' \
    "$out/$1.json" 2>/dev/null | tail -n1
}

# results <label> <action>: every test (and subtest) with that final action, sorted.
results() {
  jq -r --arg a "$2" 'select(.Test != null and (.Action == "pass" or .Action == "fail" or .Action == "skip")) | [.Test, .Action] | @tsv' \
    "$out/$1.json" 2>/dev/null | awk -F'\t' '{last[$1] = $2} END {for (t in last) print t "\t" last[t]}' \
    | awk -F'\t' -v a="$2" '$2 == a {print $1}' | sort
}

# stream_ok <json file> <pass|fail>: the go test -json stream is complete,
# unambiguous and consistent: one package, every started test and subtest
# finishes with exactly one terminal event (pass/fail/skip), no terminal
# event without a start, exactly one package terminal event, and the package
# outcome is the wanted one and agrees with the tests (fail iff a test failed).
stream_ok() {
  jq -s -r --arg want "$2" --arg pkg "${3:-github.com/ekaynac/auto-dev-cycle/test/sandbox}" '
    def terminal: .Action == "pass" or .Action == "fail" or .Action == "skip";
    to_entries as $ev
    | ([ .[] | .Package ] | unique) as $pkgs
    | ([ $ev[] | select(.value.Test != null and .value.Action == "run") | {(.value.Test): .key} ] | add // {}) as $runat
    | ([ $ev[] | select(.value.Test != null and (.value | terminal)) | {(.value.Test): .key} ] | add // {}) as $endat
    | ([ $ev[] | select(.value.Test == null and (.value | terminal)) | .key ] | max // -1) as $pkgat
    | ([ .[] | select(.Test != null and .Action == "run") | .Test ] | sort) as $started
    | ([ .[] | select(.Test != null and terminal) | .Test ] | sort) as $finished
    | [ .[] | select(.Test == null and terminal) ] as $pkgend
    | ([ .[] | select(.Test != null and .Action == "fail") ] | length) as $failed
    | if $pkgs != [$pkg] then "package identity \($pkgs), want [\($pkg)]"
      elif $started != ($started | unique) then "a test started twice"
      elif $finished != $started then "started and finished tests differ"
      elif ($pkgend | length) != 1 then "package completion events: \($pkgend | length) (want 1)"
      elif ([ $runat | keys[] | select($runat[.] > $endat[.] or $endat[.] > $pkgat) ] | length) > 0 then "events out of order"
      elif $pkgend[0].Action != $want then "package outcome \($pkgend[0].Action), want \($want)"
      elif ($failed > 0) != ($want == "fail") then "package outcome disagrees with the test results"
      else "ok" end' "$1" 2>/dev/null | grep -qx ok
}

# diagnose <label>: why a run reported nothing for a test: its exit status, the
# wrapper's stderr and go's own non-test output (a build failure, or a
# compiler or test binary killed by a signal, shows up only there).
diagnose() {
  echo "        [$1] exit status $(cat "$out/$1.rc" 2>/dev/null || echo unknown); its stderr and go's non-test output:"
  { sed 's/^/        stderr: /' "$out/$1.err" 2>/dev/null || true; } | tail -n 20
  { jq -j 'select(.Test == null and .Output != null) | .Output' "$out/$1.json" 2>&1 || true; } | tail -n 20 | sed 's/^/        go: /'
}
declare -A diagnosed=()

expect() {  # expect <label> <wanted action> <test>...
  local label="$1" want="$2" t got; shift 2
  for t in "$@"; do
    got="$(action "$label" "$t")"
    if [[ "$got" == "$want" ]]; then
      printf '  ok    %-44s %s\n' "$t" "$got"
    else
      printf '  FAIL  %-44s wanted %s, got %s\n' "$t" "$want" "${got:-nothing}"
      fail=1
      if [[ -z "$got" && -z "${diagnosed[$label]:-}" ]]; then diagnosed[$label]=1; diagnose "$label"; fi
    fi
  done
}

echo "evidence validator: malformed streams are rejected"
ev() { printf '%s\n' "${@:2}" >"$out/ev-$1.json"; }
P='"Package":"github.com/ekaynac/auto-dev-cycle/test/sandbox"'
ev good "{$P,\"Action\":\"run\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"pass\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"pass\"}"
ev nopkg "{$P,\"Action\":\"run\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"pass\",\"Test\":\"TestA\"}"
ev dup "{$P,\"Action\":\"run\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"pass\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"fail\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"fail\"}"
ev unfinished "{$P,\"Action\":\"run\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"run\",\"Test\":\"TestB\"}" "{$P,\"Action\":\"pass\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"pass\"}"
ev failpass "{$P,\"Action\":\"run\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"fail\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"pass\"}"
ev failskip "{$P,\"Action\":\"run\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"fail\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"skip\"}"
ev noident '{"Action":"run","Test":"TestA"}' '{"Action":"pass","Test":"TestA"}' '{"Action":"pass"}'
ev order "{$P,\"Action\":\"pass\"}" "{$P,\"Action\":\"pass\",\"Test\":\"TestA\"}" "{$P,\"Action\":\"run\",\"Test\":\"TestA\"}"
for bad in nopkg dup unfinished failpass noident order; do
  if stream_ok "$out/ev-$bad.json" pass; then echo "  FAIL  malformed stream '$bad' accepted"; fail=1; else echo "  ok    malformed stream '$bad' rejected"; fi
done
if stream_ok "$out/ev-failskip.json" fail; then echo "  FAIL  malformed stream 'failskip' accepted"; fail=1; else echo "  ok    malformed stream 'failskip' rejected"; fi
if stream_ok "$out/ev-good.json" pass; then echo "  ok    a complete stream accepted"; else echo "  FAIL  a complete stream was rejected"; fail=1; fi

echo "sandboxed run (backend $backend): every check must pass"
run sandboxed --backend "$backend"
stream_ok "$out/sandboxed.json" pass || { echo "  FAIL  the sandboxed run's test stream is incomplete or ambiguous"; fail=1; }
expect sandboxed pass "${checks[@]}" "${net_checks[@]}" "${mutation_checks[@]}"
if [[ "$(cat "$out/sandboxed.rc")" != 0 ]]; then
  echo "  FAIL  the sandboxed run exited $(cat "$out/sandboxed.rc"):"; sed 's/^/        /' "$out/sandboxed.err"; fail=1
fi

echo "unsandboxed control: every check must fail (red-proof)"
ADC_TEST_UNSANDBOXED=1 run control --backend none
stream_ok "$out/control.json" fail || { echo "  FAIL  the control run's test stream is incomplete or inconsistent"; fail=1; }
[[ "$(cat "$out/control.rc")" == 1 ]] || { echo "  FAIL  the control run exited $(cat "$out/control.rc"), want 1"; fail=1; }
expect control fail "${checks[@]}"
for pair in TestNoExternalTCP:$host_tcp TestNoDNS:$host_dns; do
  if [[ "${pair#*:}" == 1 ]]; then
    expect control fail "${pair%%:*}"
  elif [[ -n "${CI:-}" ]]; then
    echo "  FAIL  ${pair%%:*}: the host itself has no such access, so its red cannot be proven (required in CI)"; fail=1
  else
    echo "  WARN  ${pair%%:*}: the host itself has no such access; its red is unproven on this run (required in CI)"
  fi
done
if grep -q 'containment breach' "$out/control.err"; then
  echo "  ok    the wrapper detected the breach"
else
  echo "  FAIL  the wrapper did not detect the breach"; fail=1
fi

bwrap_starts() { command -v bwrap >/dev/null && bwrap --unshare-all --ro-bind / / true 2>/dev/null; }
if [[ "$backend" == auto || "$backend" == bwrap ]] && bwrap_starts; then
  echo "single-property mutations (bwrap): each must turn its own check red"
  for m in "${mutations[@]}"; do
    name="${m%%=*}"
    IFS=+ read -r -a red <<<"${m#*=}"
    ADC_TEST_UNSANDBOXED=1 ADC_TEST_SANDBOX_MUTATION="$name" run "mut-$name" --backend bwrap
    echo "  [$name] must fail: ${red[*]}; everything else must pass"
    expect "mut-$name" fail "${red[@]}"
    green=()
    for c in "${checks[@]}" "${net_checks[@]}" "${mutation_checks[@]}"; do
      [[ " ${red[*]} " == *" $c "* ]] || green+=("$c")
    done
    expect "mut-$name" pass "${green[@]}"
    got_red="$(results "mut-$name" fail | { grep -v / || true; } | tr '\n' ' ')"
    want_red="$(printf '%s\n' "${red[@]}" | sort | tr '\n' ' ')"
    all_now="$( { results "mut-$name" pass; results "mut-$name" fail; results "mut-$name" skip; } | sort | tr '\n' ' ')"
    all_clean="$( { results sandboxed pass; results sandboxed fail; results sandboxed skip; } | sort | tr '\n' ' ')"
    if ! stream_ok "$out/mut-$name.json" fail; then
      echo "  FAIL  [$name] the test stream is incomplete or ambiguous"; fail=1
    elif [[ "$got_red" != "$want_red" ]]; then
      echo "  FAIL  [$name] failing set is [$got_red], want exactly [$want_red]"; fail=1
    elif [[ "$all_now" != "$all_clean" ]]; then
      echo "  FAIL  [$name] the set of reported tests differs from the clean run"; fail=1
    elif [[ "$(cat "$out/mut-$name.rc")" != 1 ]] || grep -q 'containment breach\|test-sandbox:' "$out/mut-$name.err"; then
      echo "  FAIL  [$name] exit $(cat "$out/mut-$name.rc") / wrapper message: $(head -n1 "$out/mut-$name.err")"; fail=1
    else
      echo "  ok    [$name] exactly its declared checks failed; the test set and exit status match"
    fi
  done
else
  echo "  FAIL  the mutation red-proofs need a working bubblewrap"; fail=1
fi

echo "refusals: only areas test-sandbox created are ever mounted"
# unexpected <label> <rc> <stderr file>: a refusal that failed some other way.
unexpected() {
  echo "  FAIL  $1: unexpected error (exit $2): $(head -n1 "$3")"; fail=1
  sed '1d; s/^/        /' "$3" | tail -n 20
}
refuse() {  # refuse <label> <expected message> <env assignments...>
  local label="$1" want="$2" rc=0; shift 2
  env ADC_TEST_PROVISION="$out/provision" "$@" "$repo_root/scripts/test-sandbox.sh" -- /bin/true 2>"$out/refuse.err" || rc=$?
  if [[ "$rc" == 0 ]]; then
    echo "  FAIL  $label: the wrapper ran"; fail=1
  elif grep -q "$want" "$out/refuse.err"; then
    echo "  ok    $label: $(head -n1 "$out/refuse.err")"
  else
    unexpected "$label" "$rc" "$out/refuse.err"
  fi
}
mkdir -p "$out/decoy-codex" "$out/xdg-state" "$out/xdg-runtime"
echo decoy >"$out/decoy-codex/auth.json"
refuse "an existing unmarked directory as scratch" "not created by test-sandbox" ADC_TEST_SCRATCH="$out/decoy-codex"
refuse "scratch inside the state directory" "overlaps protected" XDG_STATE_HOME="$out/xdg-state" ADC_TEST_SCRATCH="$out/xdg-state/x"
refuse "scratch inside the runtime directory" "overlaps protected" XDG_RUNTIME_DIR="$out/xdg-runtime" ADC_TEST_SCRATCH="$out/xdg-runtime/x"
refuse "scratch containing a relocated CODEX_HOME" "overlaps protected" CODEX_HOME="$out/outer/codex" ADC_TEST_SCRATCH="$out/outer"
refuse "scratch inside a relocated CODEX_HOME" "overlaps protected" CODEX_HOME="$out/decoy-codex" ADC_TEST_SCRATCH="$out/decoy-codex/x"
# A genuine provision area whose module cache was replaced by a symlink.
mkdir -m 700 "$out/prov-symlink" && printf 'adc test-sandbox area v1\n' >"$out/prov-symlink/.adc-test-area"
ln -s "$out/decoy-codex" "$out/prov-symlink/modcache"
rc=0
env ADC_TEST_PROVISION="$out/prov-symlink" ADC_TEST_SCRATCH="$out/scratch-prov-symlink" "$repo_root/scripts/test-sandbox.sh" -- /bin/true 2>"$out/refuse.err" || rc=$?
if [[ "$rc" == 0 ]]; then
  echo "  FAIL  a symlinked provisioned module cache was mounted"; fail=1
elif grep -q 'is a symlink; refused' "$out/refuse.err"; then
  echo "  ok    a symlinked provisioned child: $(head -n1 "$out/refuse.err")"
else
  unexpected "symlinked provisioned child" "$rc" "$out/refuse.err"
fi
refuse "a provision area inside the writable cache" "inside scratch must be" ADC_TEST_SCRATCH="$out/scratch-overlap" ADC_TEST_PROVISION="$out/scratch-overlap/gocache"
refuse "the Go installation inside the writable cache" "overlaps" ADC_TEST_SCRATCH="$out/scratch-hostgo" ADC_GO_PREFIX="$out/scratch-hostgo/gocache/host-go"
refuse "a relocated CODEX_HOME inside the repository" "overlaps protected" CODEX_HOME="$repo_root/.adc-test-codex-home"
if [[ "$(cat "$out/decoy-codex/auth.json")" == decoy ]]; then echo "  ok    the decoy was left untouched"; else echo "  FAIL  the decoy changed"; fail=1; fi

echo "refusals: a cache symlink in an otherwise valid scratch"
# A genuine (marked, private) scratch area with a symlink planted inside.
mkdir -m 700 "$out/scratch-symlink" && mkdir -p "$out/decoy-target"
printf 'adc test-sandbox area v1\n' >"$out/scratch-symlink/.adc-test-area"
ln -s "$out/decoy-target" "$out/scratch-symlink/gocache"
rc=0
ADC_TEST_PROVISION="$out/provision" ADC_TEST_SCRATCH="$out/scratch-symlink" "$repo_root/scripts/test-sandbox.sh" -- /bin/true 2>"$out/symlink.err" || rc=$?
if [[ "$rc" == 0 ]]; then
  echo "  FAIL  the wrapper ran with a symlinked cache"; fail=1
elif grep -q 'symlink; refused' "$out/symlink.err"; then
  echo "  ok    refused: $(head -n1 "$out/symlink.err")"
else
  unexpected "symlinked cache" "$rc" "$out/symlink.err"
fi

if [[ "$fail" != 0 ]]; then
  echo "test-sandbox self-test: FAILED" >&2
  exit 1
fi
echo "test-sandbox self-test: passed"
