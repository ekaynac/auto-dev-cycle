#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The test sandbox's self-test (docs/design/m0/11, M0 exit check
# "Containment"). Runs the containment tests in test/sandbox twice, with decoy
# credentials in the host environment:
#   1. inside the sandbox: every containment check must pass and the host
#      sentinel must stay untouched;
#   2. with no containment (backend none): every check must FAIL and the
#      wrapper must report the sentinel breach. This proves each check can see
#      the red it guards against. The sentinel is a decoy under /tmp, so the
#      unsandboxed run cannot damage anything real.
#
# Usage: scripts/test-sandbox-selftest.sh [--backend auto|bwrap|container]
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
backend=auto
case "${1:-}" in
  "") ;;
  --backend) backend="${2:?}" ;;
  *) printf 'usage: %s [--backend auto|bwrap|container]\n' "$0" >&2; exit 64 ;;
esac

checks=(
  TestSentinelUnreachable/go_direct_write
  TestSentinelUnreachable/child_shell_write
  TestSentinelUnreachable/node_write
  TestSentinelUnreachable/symlink_write
  TestRealHomeAbsent
  TestNoCredentialEnv
  TestNoNetwork
)

out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT
fail=0

# run <label> <wrapper args...>: go test -json output in $out/<label>.json,
# wrapper stderr in $out/<label>.err, exit code in $out/<label>.rc.
run() {
  local label="$1"; shift
  local rc=0
  env ANTHROPIC_API_KEY=decoy-not-a-key ADC_DECOY_TOKEN=decoy-not-a-token \
    "$repo_root/scripts/test-sandbox.sh" "$@" -- go test -count=1 -json ./test/sandbox/ \
    >"$out/$label.json" 2>"$out/$label.err" || rc=$?
  printf '%s\n' "$rc" >"$out/$label.rc"
}

# action <label> <test>: the final action (pass|fail|skip) go test reported.
action() {
  jq -r --arg t "$2" 'select(.Test == $t and (.Action == "pass" or .Action == "fail" or .Action == "skip")) | .Action' \
    "$out/$1.json" | tail -n1
}

expect() {  # expect <label> <wanted action>
  local t got
  for t in "${checks[@]}"; do
    got="$(action "$1" "$t")"
    if [[ "$got" == "$2" ]]; then
      printf '  ok    %-44s %s\n' "$t" "$got"
    else
      printf '  FAIL  %-44s wanted %s, got %s\n' "$t" "$2" "${got:-nothing}"
      fail=1
    fi
  done
}

echo "sandboxed run (backend $backend): every check must pass"
run sandboxed --backend "$backend"
expect sandboxed pass
if [[ "$(cat "$out/sandboxed.rc")" != 0 ]] || grep -q 'containment breach' "$out/sandboxed.err"; then
  echo "  FAIL  sandboxed run exited $(cat "$out/sandboxed.rc"):"; sed 's/^/        /' "$out/sandboxed.err"; fail=1
fi

echo "unsandboxed control: every check must fail (red-proof)"
ADC_TEST_UNSANDBOXED=1 run control --backend none
expect control fail
if grep -q 'containment breach' "$out/control.err"; then
  echo "  ok    host sentinel breach detected by the wrapper"
else
  echo "  FAIL  the wrapper did not detect the sentinel breach"; fail=1
fi

if [[ "$fail" != 0 ]]; then
  echo "test-sandbox self-test: FAILED" >&2
  exit 1
fi
echo "test-sandbox self-test: passed"
