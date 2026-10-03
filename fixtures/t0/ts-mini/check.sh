#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Well-formedness check for the ts-mini T0 fixture (fixtures/t0/README.md, rules 1-4).
# For every task, in fresh temporary copies of repo/ (each a new git repository):
#   1. base: the visible tests give expected.base_visible; with the hidden tests, expected.base_hidden;
#   2. after: reference.patch applies with `git apply`, and with the hidden tests the gate passes;
#   3. the patch never edits a test file;
#   4. the gate runs offline (a bwrap network namespace, checked by a probe) and writes a parseable
#      junit XML results file.
# "Base with hidden" must fail in the hidden tests themselves: at least one test case that the hidden
# files add fails, and nothing fails that did not already fail at base visible. A skipped test case is
# never a pass. Prints one line per check and exits non-zero on any deviation.
#
# Environment: without a working bwrap the check fails, unless ADC_T0_ALLOW_NETWORK=1 is set (then the
# gate runs with the host network and the offline property is reported as SKIP).

set -euo pipefail
export LC_ALL=C # comm needs the byte order that python's sorted() writes

FIXTURE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly FIXTURE_DIR
readonly REPO_DIR="$FIXTURE_DIR/repo"
readonly TASKS_DIR="$FIXTURE_DIR/tasks"
readonly FIXTURE_JSON="$FIXTURE_DIR/fixture.json"
readonly GIT_ID=(-c user.name=t0-check -c user.email=t0-check@example.invalid -c commit.gpgsign=false)

failures=0
work_root=""

cleanup() {
  if [[ -n "$work_root" && -d "$work_root" ]]; then
    rm -rf "$work_root"
  fi
}
trap cleanup EXIT

pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }
skip() { printf 'SKIP  %s\n' "$*"; }

require_tool() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'FAIL  required tool not found: %s\n' "$1"
    exit 2
  fi
}

# json_get FILE PATH [DEFAULT]: prints the value at a dotted key PATH (e.g. gate.command) of a JSON
# file; a list prints NUL-separated; a missing key prints DEFAULT (JSON) or fails without one.
json_get() {
  python3 - "$@" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    v = json.load(f)
for key in sys.argv[2].split("."):
    if isinstance(v, dict) and key in v:
        v = v[key]
    elif len(sys.argv) > 3:
        v = json.loads(sys.argv[3])
        break
    else:
        sys.exit(f"{sys.argv[1]}: missing key {sys.argv[2]}")
if isinstance(v, list):
    sys.stdout.write("\0".join(str(x) for x in v))
elif isinstance(v, bool):
    sys.stdout.write("true" if v else "false")
elif v is None:
    sys.stdout.write("")
else:
    sys.stdout.write(str(v))
PY
}

# read_list FILE PATH ARRAY_NAME [DEFAULT]: reads a JSON list into the named array.
read_list() {
  local -n out=$3
  out=()
  local tmp
  tmp="$(mktemp)"
  if ! json_get "$1" "$2" "${@:4}" >"$tmp"; then
    rm -f "$tmp"
    return 1
  fi
  if [[ -s "$tmp" ]]; then
    # shellcheck disable=SC2034  # out is a nameref; the caller's array is the consumer.
    mapfile -d '' -t out <"$tmp"
  fi
  rm -f "$tmp"
}

# junit_summary FILE ALL_OUT FAILED_OUT: prints "<tests> <failures+errors> <skipped>" or exits
# non-zero if the XML is not junit. Writes every test case's key (its enclosing suite names and its
# own name, joined by " > ") to ALL_OUT and the failing ones to FAILED_OUT, one per line, sorted.
# The file is produced locally by the fixture's own gate run, so the stdlib parser is adequate here.
junit_summary() {
  python3 - "$@" <<'PY'
import sys
import xml.etree.ElementTree as ET
root = ET.parse(sys.argv[1]).getroot()
if root.tag not in ("testsuites", "testsuite"):
    sys.exit(f"unexpected root element {root.tag}")
cases = []
def walk(node, path):
    for child in node:
        if child.tag == "testsuite":
            walk(child, path + [child.get("name", "")])
        elif child.tag == "testcase":
            cases.append((" > ".join(path + [child.get("name", "")]), child))
walk(root, [root.get("name", "")] if root.tag == "testsuite" else [])
failed = [k for k, c in cases if c.find("failure") is not None or c.find("error") is not None]
skipped = sum(1 for _, c in cases if c.find("skipped") is not None)
with open(sys.argv[2], "w", encoding="utf-8") as out:
    out.writelines(k + "\n" for k in sorted(k for k, _ in cases))
with open(sys.argv[3], "w", encoding="utf-8") as out:
    out.writelines(k + "\n" for k in sorted(failed))
print(len(cases), len(failed), skipped)
PY
}

# The probe exits 0 only when a TCP connection to a public address fails.
readonly NET_PROBE='const s = require("node:net").connect({ host: "1.1.1.1", port: 443 });
s.setTimeout(3000, () => process.exit(0));
s.on("connect", () => process.exit(1));
s.on("error", () => process.exit(0));'

offline_mode=""
detect_offline() {
  if command -v bwrap >/dev/null 2>&1 \
    && bwrap --unshare-net --ro-bind / / --dev /dev --proc /proc true >/dev/null 2>&1; then
    if bwrap --unshare-net --ro-bind / / --dev /dev --proc /proc -- node -e "$NET_PROBE" >/dev/null 2>&1; then
      offline_mode="bwrap"
      pass "offline: gate runs in a bwrap network namespace (probe found no network)"
    else
      fail "offline: the bwrap gate sandbox still reaches the network"
      exit 1
    fi
  elif [[ "${ADC_T0_ALLOW_NETWORK:-0}" == "1" ]]; then
    offline_mode="none"
    skip "offline: bwrap unavailable; gate runs with the host network (ADC_T0_ALLOW_NETWORK=1)"
  else
    fail "offline: bwrap cannot drop the network; set ADC_T0_ALLOW_NETWORK=1 to run without isolation"
    exit 1
  fi
}

# run_gate DIR LABEL: runs the gate in DIR; sets gate_result to pass|fail, or error when the
# results file is missing, unparseable or inconsistent with the exit status, or when a test case was
# skipped in a run that would otherwise pass. Leaves the case keys in $work_root/cases.{all,failed}.
gate_result=""
run_gate() {
  local dir=$1 label=$2 status=0 log="$work_root/gate.log"
  rm -f "$dir/$RESULTS_PATH"
  : >"$work_root/cases.all"
  : >"$work_root/cases.failed"
  if [[ "$offline_mode" == "bwrap" ]]; then
    (cd "$dir" && bwrap --unshare-net --ro-bind / / --dev /dev --proc /proc --tmpfs /tmp \
      --bind "$dir" "$dir" --chdir "$dir" -- "${GATE_CMD[@]}") >"$log" 2>&1 || status=$?
  else
    (cd "$dir" && "${GATE_CMD[@]}") >"$log" 2>&1 || status=$?
  fi
  if [[ $status -eq 0 ]]; then gate_result="pass"; else gate_result="fail"; fi

  local summary
  if [[ ! -s "$dir/$RESULTS_PATH" ]]; then
    fail "$label: results file $RESULTS_PATH not produced (gate exit $status)"
    sed 's/^/      | /' "$log" | tail -n 20
    gate_result="error"
    return 0
  fi
  if ! summary="$(junit_summary "$dir/$RESULTS_PATH" "$work_root/cases.all" "$work_root/cases.failed" 2>&1)"; then
    fail "$label: $RESULTS_PATH is not parseable junit XML: $summary"
    gate_result="error"
    return 0
  fi
  local tests bad skipped
  read -r tests bad skipped <<<"$summary"
  if [[ "$tests" -eq 0 ]]; then
    fail "$label: $RESULTS_PATH holds no test cases"
    gate_result="error"
    return 0
  fi
  if [[ "$gate_result" == "pass" && "$bad" -ne 0 ]]; then
    fail "$label: gate exit 0 but $RESULTS_PATH reports $bad failing test cases"
    gate_result="error"
    return 0
  fi
  if [[ "$gate_result" == "fail" && "$bad" -eq 0 ]]; then
    fail "$label: gate exit $status but $RESULTS_PATH reports no failing test case"
    sed 's/^/      | /' "$log" | tail -n 20
    gate_result="error"
    return 0
  fi
  if [[ "$gate_result" == "pass" && "$skipped" -ne 0 ]]; then
    fail "$label: $skipped skipped test case(s); a skipped test is not a pass"
    gate_result="error"
    return 0
  fi
  pass "$label: junit results parse ($tests test cases, $bad failing, $skipped skipped)"
}

# fresh_copy NAME: copies repo/ into a new git repository and prints its path.
fresh_copy() {
  local dir="$work_root/$1"
  mkdir -p "$dir"
  cp -a "$REPO_DIR/." "$dir/"
  git -C "$dir" init -q
  git -C "$dir" "${GIT_ID[@]}" add -A
  git -C "$dir" "${GIT_ID[@]}" commit -q -m "base"
  printf '%s' "$dir"
}

# copy_tests SRC_ROOT DIR FILES...: copies listed test files from SRC_ROOT into DIR.
copy_tests() {
  local src=$1 dir=$2 f
  shift 2
  for f in "$@"; do
    mkdir -p "$dir/$(dirname "$f")"
    cp "$src/$f" "$dir/$f"
  done
}

is_test_path() {
  local p=$1
  [[ "$p" == test/* || "$p" == */test/* || "$p" == tests/* || "$p" == */tests/* \
    || "$p" == *.test.ts || "$p" == *.spec.ts || "$p" == *.test.js || "$p" == *.spec.js ]]
}

check_fixture() {
  local kind deps=""
  for kind in dependencies devDependencies optionalDependencies peerDependencies; do
    deps+="$(json_get "$REPO_DIR/package.json" "$kind" '{}' | tr -d '{}')"
  done
  if [[ -z "$deps" ]]; then
    pass "fixture: repo/package.json declares no dependencies"
  else
    fail "fixture: repo/package.json declares dependencies; the dependency recipe is empty"
  fi
  local recipe=()
  read_list "$FIXTURE_JSON" dependency_recipe.command recipe
  if [[ ${#recipe[@]} -eq 0 ]]; then
    pass "fixture: dependency recipe is empty (nothing to install)"
  else
    fail "fixture: dependency recipe has a command but this fixture must need none"
  fi
  if [[ "$(json_get "$FIXTURE_JSON" gate.result_adapter)" == "junit_xml" ]]; then
    pass "fixture: result adapter is junit_xml"
  else
    fail "fixture: result adapter is not junit_xml"
  fi
}

check_task() {
  local task_dir=$1 task json kind
  task="$(basename "$task_dir")"
  json="$task_dir/task.json"
  if [[ ! -f "$json" ]] || ! python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$json" 2>/dev/null; then
    fail "$task: task.json missing or not valid JSON"
    return 0
  fi

  local id base_visible base_hidden after_all
  id="$(json_get "$json" id)"
  kind="$(json_get "$json" kind)"
  base_visible="$(json_get "$json" expected.base_visible)"
  base_hidden="$(json_get "$json" expected.base_hidden)"
  after_all="$(json_get "$json" expected.after_all)"
  local hidden=() visible_added=()
  read_list "$json" acceptance.hidden_tests hidden
  read_list "$json" acceptance.visible_tests_added visible_added '[]'

  # task.json shape
  local shape_ok=1
  [[ "$id" == "$task" ]] || { fail "$task: id '$id' does not match the directory name"; shape_ok=0; }
  case "$kind" in
    feature | bugfix | refactor) [[ "$base_visible" == "pass" ]] \
      || { fail "$task: kind $kind must have base_visible=pass"; shape_ok=0; } ;;
    failing_then_green) [[ "$base_visible" == "fail" ]] \
      || { fail "$task: failing_then_green must have base_visible=fail"; shape_ok=0; } ;;
    *) fail "$task: unknown kind '$kind'"; shape_ok=0 ;;
  esac
  [[ "$base_hidden" == "fail" ]] || { fail "$task: expected.base_hidden must be fail"; shape_ok=0; }
  [[ "$after_all" == "pass" ]] || { fail "$task: expected.after_all must be pass"; shape_ok=0; }
  [[ ${#hidden[@]} -gt 0 ]] || { fail "$task: no hidden tests declared"; shape_ok=0; }
  local f
  for f in "${hidden[@]}"; do
    [[ -f "$task_dir/hidden/$f" ]] || { fail "$task: hidden test $f missing under hidden/"; shape_ok=0; }
    [[ ! -e "$REPO_DIR/$f" ]] || { fail "$task: hidden test $f already exists in repo/"; shape_ok=0; }
  done
  for f in "${visible_added[@]}"; do
    [[ -f "$task_dir/visible/$f" ]] || { fail "$task: visible test $f missing under visible/"; shape_ok=0; }
  done
  [[ -f "$task_dir/reference.patch" ]] || { fail "$task: reference.patch missing"; shape_ok=0; }
  [[ $shape_ok -eq 1 ]] || return 0
  pass "$task: task.json well-formed (kind $kind)"

  # Rule 3: the patch never edits a test file.
  local touched=() p bad_paths=()
  mapfile -t touched < <(sed -n -E 's|^diff --git a/(.*) b/(.*)$|\1\n\2|p' "$task_dir/reference.patch" | sort -u)
  if [[ ${#touched[@]} -eq 0 ]]; then
    fail "$task: reference.patch touches no files"
    return 0
  fi
  for p in "${touched[@]}"; do
    if is_test_path "$p"; then bad_paths+=("$p"); fi
    for f in "${hidden[@]}" "${visible_added[@]}"; do
      [[ "$p" != "$f" ]] || bad_paths+=("$p")
    done
  done
  if [[ ${#bad_paths[@]} -eq 0 ]]; then
    pass "$task: rule 3 reference.patch edits no test file (${touched[*]})"
  else
    fail "$task: rule 3 reference.patch edits test files: ${bad_paths[*]}"
  fi

  # Rule 1: base state.
  local base
  base="$(fresh_copy "$task-base")"
  copy_tests "$task_dir/visible" "$base" "${visible_added[@]}"
  run_gate "$base" "$task: rule 4 base visible"
  if [[ "$gate_result" == "$base_visible" ]]; then
    pass "$task: rule 1 base visible tests $gate_result (expected $base_visible)"
  else
    fail "$task: rule 1 base visible tests $gate_result (expected $base_visible)"
  fi
  mv "$work_root/cases.all" "$work_root/visible.all"
  mv "$work_root/cases.failed" "$work_root/visible.failed"
  copy_tests "$task_dir/hidden" "$base" "${hidden[@]}"
  run_gate "$base" "$task: rule 4 base hidden"
  if [[ "$gate_result" != "$base_hidden" ]]; then
    fail "$task: rule 1 base with hidden tests $gate_result (expected $base_hidden)"
  else
    # The hidden files add the cases absent from the base visible run. At least one of them must
    # fail, and every other failure must already have failed at base visible.
    local hidden_failed others
    hidden_failed="$(comm -23 "$work_root/cases.all" "$work_root/visible.all" \
      | comm -12 - "$work_root/cases.failed" | wc -l)"
    others="$(comm -23 "$work_root/cases.failed" "$work_root/visible.failed" \
      | comm -12 - "$work_root/visible.all" | tr '\n' ';')"
    if [[ "$hidden_failed" -gt 0 && -z "$others" ]]; then
      pass "$task: rule 1 base with hidden tests $gate_result, $hidden_failed hidden test case(s) failing"
    else
      fail "$task: rule 1 base with hidden tests: $hidden_failed hidden test case(s) failing (want > 0); new failures elsewhere: [$others]"
    fi
  fi

  # Rule 2: after the reference patch.
  local after msg
  after="$(fresh_copy "$task-after")"
  copy_tests "$task_dir/visible" "$after" "${visible_added[@]}"
  if ! msg="$(git -C "$after" apply --check "$task_dir/reference.patch" 2>&1)"; then
    fail "$task: rule 2 reference.patch does not apply: $msg"
    return 0
  fi
  git -C "$after" apply "$task_dir/reference.patch"
  pass "$task: rule 2 reference.patch applies with git apply"
  copy_tests "$task_dir/hidden" "$after" "${hidden[@]}"
  run_gate "$after" "$task: rule 4 after"
  if [[ "$gate_result" == "$after_all" ]]; then
    pass "$task: rule 2 after patch, visible + hidden tests $gate_result (expected $after_all)"
  else
    fail "$task: rule 2 after patch, visible + hidden tests $gate_result (expected $after_all)"
    sed 's/^/      | /' "$work_root/gate.log" | tail -n 30
  fi
}

main() {
  require_tool git
  require_tool node
  require_tool python3

  # Type stripping is on by default only from the declared minimum version (22.18).
  local min_version have_version
  min_version="$(json_get "$FIXTURE_JSON" toolchain.minimum_version)"
  have_version="$(node -p 'process.versions.node')"
  if node -e '
    const [have, want] = process.argv.slice(1).map((v) => v.split(".").map(Number));
    for (let i = 0; i < want.length; i++) {
      if ((have[i] ?? 0) !== want[i]) process.exit((have[i] ?? 0) > want[i] ? 0 : 1);
    }' "$have_version" "$min_version"; then
    pass "toolchain: node $have_version (>= $min_version)"
  else
    fail "toolchain: node $have_version is older than $min_version, the first version that strips TypeScript types by default"
    exit 1
  fi

  GATE_CMD=()
  read_list "$FIXTURE_JSON" gate.command GATE_CMD
  RESULTS_PATH="$(json_get "$FIXTURE_JSON" gate.results_path)"
  readonly GATE_CMD RESULTS_PATH

  work_root="$(mktemp -d "${TMPDIR:-/tmp}/ts-mini-check.XXXXXX")"
  check_fixture
  detect_offline

  local task_dir count=0
  for task_dir in "$TASKS_DIR"/*/; do
    [[ -d "$task_dir" ]] || continue
    check_task "${task_dir%/}"
    count=$((count + 1))
  done
  if [[ $count -eq 0 ]]; then
    fail "fixture: no tasks found under tasks/"
  fi

  if [[ $failures -eq 0 ]]; then
    printf 'OK    ts-mini: %d tasks well-formed\n' "$count"
  else
    printf 'FAILED ts-mini: %d check(s) failed\n' "$failures"
    exit 1
  fi
}

main "$@"
