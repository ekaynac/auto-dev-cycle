#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Well-formedness check for the py-mini T0 fixture (fixtures/t0/README.md, rules 1-4).
#
# For every task under tasks/, in a fresh git-initialised copy of repo/:
#   1. base: the visible tests give expected.base_visible; with the hidden tests copied in,
#      the gate gives expected.base_hidden, and the failures are where they should be;
#   2. after: `git apply reference.patch` + hidden tests copied in: the gate passes;
#   3. reference.patch never touches a test file;
#   4. the gate runs with no network after the dependency recipe and writes per-test
#      results in the declared adapter format (JUnit XML).
#
# failing_then_green: its failing visible test never lives in repo/ (every other task starts
# green). It sits at tasks/<id>/visible/<in-repo path>, is listed in
# "acceptance.visible_tests_added", and is copied into the repo before that task's base
# commit. At base exactly those files fail; no other task may add visible tests.
#
# Only the dependency recipe may use the network. The gate runs under `bwrap --unshare-net`
# (or `unshare -rn`). With neither available the check fails, unless
# ADC_FIXTURE_ALLOW_NETWORK=1 is set (then the offline property is reported as SKIP).
#
# Usage: fixtures/t0/py-mini/check.sh            (PYTHON=python3.12 by default)
set -euo pipefail
export LC_ALL=C

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PYTHON="${PYTHON:-python3.12}"
readonly here PYTHON

failures=0
ok() { printf 'ok    %s\n' "$*"; }
bad() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }
skip() { printf 'SKIP  %s\n' "$*"; }
die() { printf 'FAIL  %s\n' "$*"; echo "py-mini: aborted" >&2; exit 1; }

command -v "$PYTHON" >/dev/null || die "interpreter $PYTHON not found"
command -v git >/dev/null || die "git not found"

work="$(mktemp -d "${TMPDIR:-/tmp}/py-mini-check.XXXXXX")"
trap 'rm -rf "$work"' EXIT

# json <file> <key>...: print the value at that key path; a list prints one item per line,
# a missing key prints nothing. Fails on unreadable or malformed JSON.
json() {
  "$PYTHON" - "$@" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as fh:
    v = json.load(fh)
for key in sys.argv[2:]:
    v = v.get(key) if isinstance(v, dict) else None
if isinstance(v, list):
    print("\n".join(str(x) for x in v))
elif v is not None:
    print(v)
PY
}

# words: join stdin lines into one sorted, space-separated line.
words() { sort | tr '\n' ' ' | sed 's/ $//'; }

# ---------------------------------------------------------------- fixture.json
fixture="$here/fixture.json"
if [[ "$(json "$fixture" name)" == py-mini ]]; then ok "fixture.json: name is py-mini"
else bad "fixture.json: name is not py-mini"; fi
adapter="$(json "$fixture" gate result_adapter)"
if [[ "$adapter" == junit_xml ]]; then ok "fixture.json: result adapter is junit_xml"
else bad "fixture.json: result adapter is '$adapter', expected junit_xml"; fi
want_version="$(json "$fixture" toolchain language_version)"
results_name="$(json "$fixture" gate results_path)"
mapfile -t gate_cmd < <(json "$fixture" gate command)
mapfile -t recipe_cmd < <(json "$fixture" toolchain dependency_recipe command)
mapfile -t lockfiles < <(json "$fixture" toolchain dependency_recipe lockfiles)
mapfile -t declared_tasks < <(json "$fixture" tasks | sort)
mapfile -t task_dirs < <(find "$here/tasks" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort)
if [[ "${declared_tasks[*]}" == "${task_dirs[*]}" ]]; then ok "fixture.json: task list matches tasks/"
else bad "fixture.json: tasks ${declared_tasks[*]} differ from tasks/ ${task_dirs[*]}"; fi
if [[ " ${gate_cmd[*]} " == *"{results_path}"* ]]; then ok "fixture.json: gate command writes {results_path}"
else bad "fixture.json: gate command does not reference {results_path}"; fi

# ---------------------------------------------------------------- dependency recipe (network allowed)
venv="$work/venv"
"$PYTHON" -m venv "$venv"
have_version="$("$venv/bin/python" -c 'import sys; print("%d.%d" % sys.version_info[:2])')"
if [[ "$have_version" == "$want_version" ]]; then ok "toolchain: python $have_version"
else bad "toolchain: python $have_version, fixture declares $want_version"; fi
for lock in "${lockfiles[@]}"; do
  if [[ -f "$here/repo/$lock" ]]; then ok "dependency recipe: lockfile $lock present"
  else bad "dependency recipe: lockfile $lock missing"; fi
done
recipe_dir="$work/recipe"
cp -a "$here/repo/." "$recipe_dir"
if (cd "$recipe_dir" && PATH="$venv/bin:$PATH" "${recipe_cmd[@]}" -q --disable-pip-version-check >"$work/recipe.log" 2>&1); then
  ok "dependency recipe: ${recipe_cmd[*]}"
else
  cat "$work/recipe.log" >&2
  die "dependency recipe failed: ${recipe_cmd[*]}"
fi

# ---------------------------------------------------------------- offline wrapper (rule 4)
offline=()
if command -v bwrap >/dev/null && bwrap --dev-bind / / --unshare-net -- true 2>/dev/null; then
  offline=(bwrap --dev-bind / / --unshare-net --)
elif command -v unshare >/dev/null && unshare -rn true 2>/dev/null; then
  offline=(unshare -rn)
fi
probe='import socket,sys
s=socket.socket(); s.settimeout(3)
try: s.connect(("1.1.1.1", 53))
except OSError: sys.exit(0)
sys.exit(1)'
if ((${#offline[@]})); then
  if "${offline[@]}" "$venv/bin/python" -c "$probe"; then
    ok "rule 4: gate sandbox has no network (${offline[0]})"
  else
    bad "rule 4: gate sandbox (${offline[0]}) still reaches the network"
  fi
elif [[ "${ADC_FIXTURE_ALLOW_NETWORK:-}" == 1 ]]; then
  skip "rule 4: no network namespace tool; gate runs with the host network (ADC_FIXTURE_ALLOW_NETWORK=1)"
else
  die "rule 4: neither bwrap nor unshare can drop the network; set ADC_FIXTURE_ALLOW_NETWORK=1 to run anyway"
fi

# ---------------------------------------------------------------- gate runner
# run_gate <workspace> <label>: runs the gate offline and sets
#   outcome   pass | fail | error   (error = anything that is not a clean pass or plain test failures)
#   failed    space-separated repo paths of test files with a failing testcase
#   summary   one-line description
run_gate() {
  local ws="$1" label="$2" results="$work/results/$2/$results_name" rc=0 parsed
  mkdir -p "$(dirname "$results")"
  local cmd=("${gate_cmd[@]//\{results_path\}/$results}")
  (cd "$ws" && env PATH="$venv/bin:$PATH" PYTHONDONTWRITEBYTECODE=1 PYTHONHASHSEED=0 \
    "${offline[@]}" "${cmd[@]}" >"$work/results/$label/gate.log" 2>&1) || rc=$?
  if [[ ! -s "$results" ]]; then
    outcome=error failed="" summary="exit $rc, no $results_name written"
    return
  fi
  parsed="$("$PYTHON" - "$results" "$ws" <<'PY'
import os, sys
# The report is written by pytest in this run's own temp dir, not taken from outside;
# the stdlib parser does not resolve external entities.
import xml.etree.ElementTree as ET
root = ET.parse(sys.argv[1]).getroot()
ws = sys.argv[2]
cases = root.findall(".//testcase")
def path_of(classname):
    parts = classname.split(".")
    for n in range(len(parts), 0, -1):
        rel = "/".join(parts[:n]) + ".py"
        if os.path.isfile(os.path.join(ws, rel)):
            return rel
    return "?" + classname
fail = err = skipped = 0
failed = set()
for c in cases:
    if c.find("failure") is not None:
        fail += 1
        failed.add(path_of(c.get("classname", "")))
    if c.find("error") is not None:
        err += 1
    if c.find("skipped") is not None:
        skipped += 1
suite_errors = sum(int(s.get("errors", 0)) for s in root.iter("testsuite"))
print(len(cases), fail, max(err, suite_errors), skipped, " ".join(sorted(failed)))
PY
)" || { outcome=error failed="" summary="exit $rc, unreadable $results_name"; return; }
  local tests fails errs skips
  read -r tests fails errs skips failed <<<"$parsed"
  summary="exit $rc, $tests tests, $fails failed, $errs errors, $skips skipped"
  if ((rc == 0 && tests > 0 && fails == 0 && errs == 0)); then
    outcome=pass
  elif ((rc == 1 && fails > 0 && errs == 0)); then
    outcome=fail
  else
    outcome=error
  fi
}

# is_test_path <repo path>: true for anything the integrity guard treats as a test file.
is_test_path() {
  local p="$1" base="${1##*/}"
  [[ "$p" == tests/* || "$p" == */tests/* || "$base" == test_*.py || "$base" == *_test.py || "$base" == conftest.py ]]
}

# subset <a> <b>: every word of a is in b.
subset() {
  local x
  for x in $1; do [[ " $2 " == *" $x "* ]] || return 1; done
}

new_workspace() {
  local id="$1" ws="$work/ws/$1"
  mkdir -p "$ws"
  cp -a "$here/repo/." "$ws"
  if [[ -d "$here/tasks/$id/visible" ]]; then cp -a "$here/tasks/$id/visible/." "$ws"; fi
  git -C "$ws" init -q
  git -C "$ws" add -A
  git -C "$ws" -c user.name=fixture -c user.email=fixture@invalid -c commit.gpgsign=false commit -qm base
  printf '%s\n' "$ws"
}

copy_hidden() { cp -a "$1/hidden/." "$2"; }

# ---------------------------------------------------------------- tasks
for id in "${task_dirs[@]}"; do
  t="$here/tasks/$id"
  tj="$t/task.json"
  echo "-- $id"
  if [[ "$(json "$tj" id)" == "$id" ]]; then ok "$id: task.json id matches its directory"
  else bad "$id: task.json id differs from its directory"; fi
  kind="$(json "$tj" kind)"
  case "$kind" in
    feature | bugfix | refactor | failing_then_green) ok "$id: kind $kind" ;;
    *) bad "$id: unknown kind '$kind'" ;;
  esac
  exp_visible="$(json "$tj" expected base_visible)"
  exp_hidden="$(json "$tj" expected base_hidden)"
  exp_after="$(json "$tj" expected after_all)"
  visible_added="$(json "$tj" acceptance visible_tests_added | words)"
  visible_found=""
  if [[ -d "$t/visible" ]]; then visible_found="$(cd "$t/visible" && find . -type f -printf '%P\n' | words)"; fi
  hidden_declared="$(json "$tj" acceptance hidden_tests | words)"
  hidden_found="$(cd "$t/hidden" && find . -type f -printf '%P\n' | words)"

  [[ "$visible_added" == "$visible_found" ]] || bad "$id: acceptance.visible_tests_added [$visible_added] differ from visible/ [$visible_found]"
  for v in $visible_found; do
    is_test_path "$v" || bad "$id: added visible file $v is not at a test path"
    [[ ! -e "$here/repo/$v" ]] || bad "$id: added visible test $v already exists in repo/"
  done
  if [[ "$kind" == failing_then_green ]]; then
    if [[ "$exp_visible" == fail ]]; then ok "$id: expects a failing visible test at base"
    else bad "$id: failing_then_green must expect base_visible fail"; fi
    if [[ -n "$visible_added" ]]; then ok "$id: adds its failing visible test ($visible_added) outside repo/"
    else bad "$id: failing_then_green must list acceptance.visible_tests_added"; fi
  else
    if [[ "$exp_visible" == pass ]]; then ok "$id: expects green visible tests at base"
    else bad "$id: only failing_then_green may expect base_visible fail"; fi
    [[ -z "$visible_added" ]] || bad "$id: only failing_then_green may add visible tests"
  fi
  if [[ "$exp_hidden" == fail ]]; then ok "$id: expects the hidden tests to fail at base"
  else bad "$id: base_hidden must be fail for $kind"; fi
  [[ "$exp_after" == pass ]] || bad "$id: after_all must be pass"
  if [[ "$hidden_declared" == "$hidden_found" && -n "$hidden_found" ]]; then ok "$id: hidden tests match acceptance.hidden_tests ($hidden_found)"
  else bad "$id: acceptance.hidden_tests [$hidden_declared] differ from hidden/ [$hidden_found]"; fi
  for h in $hidden_found; do
    is_test_path "$h" || bad "$id: hidden file $h is not at a test path"
    [[ ! -e "$here/repo/$h" && ! -e "$t/visible/$h" ]] || bad "$id: hidden test $h overwrites a visible file"
  done

  # Rule 3: the patch never edits a test file.
  patch="$t/reference.patch"
  if [[ ! -s "$patch" ]]; then
    bad "$id: reference.patch missing or empty"
    continue
  fi
  mapfile -t touched < <(git apply --numstat "$patch" | cut -f3-)
  test_touch=""
  for p in "${touched[@]}"; do is_test_path "$p" && test_touch+=" $p"; done
  if [[ -z "$test_touch" && ${#touched[@]} -gt 0 ]]; then ok "rule 3 $id: patch touches no test file (${touched[*]})"
  else bad "rule 3 $id: patch touches test files:${test_touch:- (or nothing at all)}"; fi

  # Rule 1: base.
  ws="$(new_workspace "$id")"
  run_gate "$ws" "$id-base-visible"
  if [[ "$outcome" != "$exp_visible" ]]; then
    bad "rule 1 $id: base visible gave $outcome, expected $exp_visible ($summary)"
  elif [[ "$outcome" == fail ]] && ! { subset "$failed" "$visible_added" && subset "$visible_added" "$failed"; }; then
    bad "rule 1 $id: base visible failures in [$failed], expected exactly [$visible_added]"
  else
    ok "rule 1 $id: base visible $outcome ($summary)"
  fi
  visible_failed_at_base="$failed"

  copy_hidden "$t" "$ws"
  run_gate "$ws" "$id-base-hidden"
  if [[ "$outcome" != "$exp_hidden" ]]; then
    bad "rule 1 $id: base with hidden gave $outcome, expected $exp_hidden ($summary)"
  else
    hidden_hit=""
    for f in $failed; do [[ " $hidden_found " == *" $f "* ]] && hidden_hit+=" $f"; done
    others=""
    for f in $failed; do [[ " $hidden_found $visible_failed_at_base " == *" $f "* ]] || others+=" $f"; done
    if [[ "$outcome" == fail && ( -z "$hidden_hit" || -n "$others" ) ]]; then
      bad "rule 1 $id: base with hidden fails in [$failed]; want a hidden failure and nothing new elsewhere"
    else
      ok "rule 1 $id: base with hidden $outcome, failing:${hidden_hit} ($summary)"
    fi
  fi

  # Rule 2: after.
  git -C "$ws" checkout -q -- .
  git -C "$ws" clean -fdxq
  if git -C "$ws" apply --check "$patch" 2>"$work/apply.err" && git -C "$ws" apply "$patch"; then
    ok "rule 2 $id: reference.patch applies"
    copy_hidden "$t" "$ws"
    run_gate "$ws" "$id-after"
    if [[ "$outcome" == "$exp_after" ]]; then ok "rule 2 $id: after patch + hidden tests: $outcome ($summary)"
    else bad "rule 2 $id: after patch + hidden tests gave $outcome, expected $exp_after ($summary)"; fi
  else
    bad "rule 2 $id: reference.patch does not apply: $(head -n1 "$work/apply.err")"
  fi
done

echo "--"
if ((failures)); then
  echo "py-mini: $failures check(s) failed"
  exit 1
fi
echo "py-mini: all checks passed"
