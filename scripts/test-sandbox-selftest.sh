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
# Usage: scripts/test-sandbox-selftest.sh [--backend auto|bwrap|container]
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
backend=auto
case "${1:-}" in
  "") ;;
  --backend) backend="${2:?}" ;;
  *) printf 'usage: %s [--backend auto|bwrap|container]\n' "$0" >&2; exit 64 ;;
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
# Single-property mutations (bwrap only) and the check each must turn red.
mutations=(expose-protected:TestNoProtectedOrRepoAlias repo-alias:TestNoProtectedOrRepoAlias repo-writable:TestRepoReadOnly)

out="$(mktemp -d)"
listener_pid=""
trap 'rm -rf "$out"; [[ -n "$listener_pid" ]] && kill "$listener_pid" 2>/dev/null; true' EXIT

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
run() {
  local label="$1"; shift
  local rc=0
  env ANTHROPIC_API_KEY=decoy-not-a-key ADC_DECOY_TOKEN=decoy-not-a-token \
    FTP_PROXY=http://decoy:decoy@127.0.0.1:9 NO_PROXY=decoy.invalid \
    ADC_SELFTEST_LOOPBACK_PORT="$port" ADC_TEST_SCRATCH="$out/scratch-$label" \
    "$repo_root/scripts/test-sandbox.sh" "$@" --env ADC_SELFTEST_LOOPBACK_PORT \
    -- go test -count=1 -json ./test/sandbox/ >"$out/$label.json" 2>"$out/$label.err" || rc=$?
  printf '%s\n' "$rc" >"$out/$label.rc"
}

# action <label> <test>: the final action (pass|fail|skip) go test reported.
action() {
  jq -r --arg t "$2" 'select(.Test == $t and (.Action == "pass" or .Action == "fail" or .Action == "skip")) | .Action' \
    "$out/$1.json" 2>/dev/null | tail -n1
}

expect() {  # expect <label> <wanted action> <test>...
  local label="$1" want="$2" t got; shift 2
  for t in "$@"; do
    got="$(action "$label" "$t")"
    if [[ "$got" == "$want" ]]; then
      printf '  ok    %-44s %s\n' "$t" "$got"
    else
      printf '  FAIL  %-44s wanted %s, got %s\n' "$t" "$want" "${got:-nothing}"
      fail=1
    fi
  done
}

echo "sandboxed run (backend $backend): every check must pass"
run sandboxed --backend "$backend"
expect sandboxed pass "${checks[@]}" "${net_checks[@]}"
if [[ "$(cat "$out/sandboxed.rc")" != 0 ]]; then
  echo "  FAIL  the sandboxed run exited $(cat "$out/sandboxed.rc"):"; sed 's/^/        /' "$out/sandboxed.err"; fail=1
fi

echo "unsandboxed control: every check must fail (red-proof)"
ADC_TEST_UNSANDBOXED=1 run control --backend none
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

if [[ "$backend" == auto || "$backend" == bwrap ]] && command -v bwrap >/dev/null; then
  echo "single-property mutations (bwrap): each must turn its own check red"
  for m in "${mutations[@]}"; do
    ADC_TEST_UNSANDBOXED=1 ADC_TEST_SANDBOX_MUTATION="${m%%:*}" run "mut-${m%%:*}" --backend bwrap
    printf '  [%s] ' "${m%%:*}"; expect "mut-${m%%:*}" fail "${m#*:}"
  done
elif [[ -n "${CI:-}" ]]; then
  echo "  note  mutation red-proofs need bwrap; covered on bwrap hosts only"
fi

echo "refusals: a cache symlink in an otherwise valid scratch"
mkdir -p "$out/scratch-symlink" "$out/decoy-target"
ln -s "$out/decoy-target" "$out/scratch-symlink/gocache"
if ADC_TEST_SCRATCH="$out/scratch-symlink" "$repo_root/scripts/test-sandbox.sh" -- /bin/true 2>"$out/symlink.err"; then
  echo "  FAIL  the wrapper ran with a symlinked cache"; fail=1
elif grep -q 'symlink; refused' "$out/symlink.err"; then
  echo "  ok    refused: $(head -n1 "$out/symlink.err")"
else
  echo "  FAIL  unexpected error: $(head -n1 "$out/symlink.err")"; fail=1
fi

if [[ "$fail" != 0 ]]; then
  echo "test-sandbox self-test: FAILED" >&2
  exit 1
fi
echo "test-sandbox self-test: passed"
