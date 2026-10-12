#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Well-formedness check for the php-mini T0 fixture (fixtures/t0/README.md, rules 1-4).
#
# For every task under tasks/, in a fresh git-initialised copy of repo/:
#   1. base: visible tests give expected.base_visible; with the hidden tests in, expected.base_hidden;
#   2. after: git apply reference.patch + hidden tests, the gate passes;
#   3. the patch never edits a test file;
#   4. the gate runs with --network none after the dependency recipe and writes parseable JUnit XML
#      with one <testcase> per test.
# The dependency recipe runs in the composer image (network allowed for that step only); the gate runs
# in the php image. Both run as the invoking user. Prints one line per check; exits non-zero on any deviation.
#
# A probe proves that a --network none container has no network. If such a container cannot run, the check
# fails, unless ADC_T0_ALLOW_NETWORK=1 is set (then the gate runs with the default network and the offline
# property is reported as SKIP). failing_then_green: "base with hidden" must fail in a hidden test file, and
# nothing may fail that did not already fail at base visible. A skipped test never counts as a pass.
#
# Requires: bash, git, docker (or a docker-compatible CLI), python3.
#
# Without docker the check fails, unless ADC_T0_SKIP_WITHOUT_DOCKER=1 is set: then it reports the whole
# fixture as SKIP and exits 0. CI sets it on runners that cannot run containers (the PHP toolchain is
# deferred past M6, 01 §4.1); a skip is printed, never counted as a pass.

set -euo pipefail

FIXTURE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly FIXTURE_DIR
readonly RESULTS_MOUNT="/results"
readonly DOCKER="${DOCKER:-docker}"

if ! command -v "$DOCKER" >/dev/null 2>&1 && [[ "${ADC_T0_SKIP_WITHOUT_DOCKER:-}" == 1 ]]; then
    echo "SKIP php-mini: '$DOCKER' is not installed and ADC_T0_SKIP_WITHOUT_DOCKER=1 (PHP toolchain is post-M6, 01 §4.1)"
    exit 0
fi

for tool in git python3 "$DOCKER"; do
    command -v "$tool" >/dev/null 2>&1 || { echo "FAIL setup: '$tool' is not installed" >&2; exit 2; }
done

failures=0
checks=0

pass() { checks=$((checks + 1)); echo "PASS $1"; }
fail() { checks=$((checks + 1)); failures=$((failures + 1)); echo "FAIL $1"; }

# json_get <file> <dotted.key.path>: prints the value (a list one item per line); a missing key prints nothing.
json_get() {
    python3 - "$1" "$2" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    v = json.load(f)
for key in sys.argv[2].split("."):
    v = v.get(key) if isinstance(v, dict) else None
if isinstance(v, list):
    print("\n".join(str(x) for x in v))
elif v is not None:
    print(v)
PY
}

FIXTURE_JSON="$FIXTURE_DIR/fixture.json"
PHP_IMAGE="$(json_get "$FIXTURE_JSON" "toolchain.images.$(json_get "$FIXTURE_JSON" gate.image)")"
COMPOSER_IMAGE="$(json_get "$FIXTURE_JSON" "toolchain.images.$(json_get "$FIXTURE_JSON" toolchain.dependency_recipe.image)")"
RESULTS_PATH="$(json_get "$FIXTURE_JSON" gate.results_path)"
ADAPTER="$(json_get "$FIXTURE_JSON" gate.result_adapter)"
mapfile -t RECIPE_CMD < <(json_get "$FIXTURE_JSON" toolchain.dependency_recipe.command)
mapfile -t GATE_CMD_RAW < <(json_get "$FIXTURE_JSON" gate.command)
GATE_CMD=()
for arg in "${GATE_CMD_RAW[@]}"; do GATE_CMD+=("${arg//\{results_path\}/$RESULTS_PATH}"); done

# Fixture-level checks.
for image in "$PHP_IMAGE" "$COMPOSER_IMAGE"; do
    if [[ "$image" =~ @sha256:[0-9a-f]{64}$ ]]; then pass "fixture: image pinned by digest: ${image%%@*}"
    else fail "fixture: image not pinned by digest: $image"; fi
done
if [[ "$ADAPTER" == "junit_xml" ]]; then pass "fixture: result adapter is junit_xml"
else fail "fixture: result adapter is '$ADAPTER'"; fi
if [[ "$RESULTS_PATH" == "$RESULTS_MOUNT"/* ]]; then pass "fixture: results path is under $RESULTS_MOUNT"
else fail "fixture: results path '$RESULTS_PATH' is not under $RESULTS_MOUNT"; fi
if [[ -f "$FIXTURE_DIR/repo/composer.lock" ]]; then pass "fixture: composer.lock is committed"
else fail "fixture: composer.lock missing"; fi
if [[ ! -e "$FIXTURE_DIR/repo/vendor" ]]; then pass "fixture: no vendor/ in repo"
else fail "fixture: repo/vendor exists"; fi

WORK_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/php-mini-check.XXXXXX")"
cleanup() { rm -rf "$WORK_ROOT"; }
trap cleanup EXIT
mkdir -p "$WORK_ROOT/composer-home"

USER_SPEC="$(id -u):$(id -g)"

# Offline gate (rule 4). The probe exits 0 only when a TCP connection to a public address fails;
# docker itself exits 125 or more when it cannot start the container.
NETWORK_ARGS=(--network none)
OFFLINE_LABEL=" (offline gate)"
probe_rc=0
# shellcheck disable=SC2016 # the single-quoted $ variables are PHP, not shell
"$DOCKER" run --rm --network none --user "$USER_SPEC" "$PHP_IMAGE" \
    php -r 'exit(@fsockopen("1.1.1.1", 443, $errno, $errstr, 3) ? 1 : 0);' >"$WORK_ROOT/probe.log" 2>&1 || probe_rc=$?
if [[ $probe_rc -eq 0 ]]; then
    pass "fixture: gate runs offline (--network none; probe found no network)"
elif [[ $probe_rc -eq 1 ]]; then
    fail "fixture: a --network none container still reaches the network"
    exit 1
elif [[ "${ADC_T0_ALLOW_NETWORK:-}" == 1 ]]; then
    NETWORK_ARGS=()
    OFFLINE_LABEL=""
    echo "SKIP fixture: --network none unavailable (exit $probe_rc); gate runs with the default network (ADC_T0_ALLOW_NETWORK=1)"
else
    fail "fixture: cannot run a --network none container (exit $probe_rc); set ADC_T0_ALLOW_NETWORK=1 to run without isolation"
    tail -n 5 "$WORK_ROOT/probe.log" | sed 's/^/    | /'
    exit 1
fi

# run_recipe <repo dir>: dependency recipe, network allowed.
run_recipe() {
    "$DOCKER" run --rm --user "$USER_SPEC" \
        -e COMPOSER_HOME=/composer-home -e HOME=/tmp \
        -v "$WORK_ROOT/composer-home:/composer-home" -v "$1:/app" -w /app \
        "$COMPOSER_IMAGE" "${RECIPE_CMD[@]}" >"$WORK_ROOT/recipe.log" 2>&1
}

# run_gate <repo dir> <results dir>: gate offline; prints "pass" or "fail", or "error: <why>" (a run with
# skipped tests is never "pass"). Writes the repo paths of test files with a failing test case, one per
# line, to $WORK_ROOT/failed.txt.
run_gate() {
    local repo="$1" results="$2" rc=0
    rm -rf "$results"
    mkdir -p "$results"
    : >"$WORK_ROOT/failed.txt"
    "$DOCKER" run --rm "${NETWORK_ARGS[@]}" --user "$USER_SPEC" -e HOME=/tmp \
        -v "$repo:/app" -v "$results:$RESULTS_MOUNT" -w /app \
        "$PHP_IMAGE" "${GATE_CMD[@]}" >"$WORK_ROOT/gate.log" 2>&1 || rc=$?
    local junit="$results/${RESULTS_PATH#"$RESULTS_MOUNT"/}"
    if [[ ! -f "$junit" ]]; then
        echo "error: no junit file (gate exit $rc)"
        return
    fi
    python3 - "$junit" "$rc" "$WORK_ROOT/failed.txt" <<'PY'
import sys
# The file is produced by our own gate run; Python's expat does not expand external entities.
import xml.etree.ElementTree as ET
path, rc, failed_out = sys.argv[1], int(sys.argv[2]), sys.argv[3]
try:
    root = ET.parse(path).getroot()
except ET.ParseError as e:
    print(f"error: junit not parseable: {e}")
    sys.exit(0)
cases = root.findall(".//testcase")
bad = [c for c in cases if c.find("failure") is not None or c.find("error") is not None]
skipped = [c for c in cases if c.find("skipped") is not None]
files = sorted({c.get("file", "?").removeprefix("/app/") for c in bad})
with open(failed_out, "w", encoding="utf-8") as out:
    out.writelines(f + "\n" for f in files)
if not cases:
    print(f"error: junit has no testcases (gate exit {rc})")
elif rc == 0 and not bad and skipped:
    print(f"error: {len(skipped)} skipped test(s); a skipped test is not a pass")
elif rc == 0 and not bad:
    print("pass")
elif rc != 0 and bad:
    print("fail")
else:
    print(f"error: gate exit {rc} disagrees with junit ({len(bad)} failing of {len(cases)})")
PY
}

# expect_outcome <label> <expected> <actual>
expect_outcome() {
    if [[ "$3" == "$2" ]]; then pass "$1: $2"
    else fail "$1: expected $2, got $3"; tail -n 20 "$WORK_ROOT/gate.log" | sed 's/^/    | /'; fi
}

# copy_overlay <src dir> <repo dir>: copies the overlay tree in, printing each relative path.
copy_overlay() {
    [[ -d "$1" ]] || return 0
    (cd "$1" && find . -type f -printf '%P\n') | while IFS= read -r rel; do
        mkdir -p "$2/$(dirname "$rel")"
        cp "$1/$rel" "$2/$rel"
        echo "$rel"
    done
}

declare -A kinds_seen=()
shopt -s nullglob
task_dirs=("$FIXTURE_DIR"/tasks/*/)
[[ ${#task_dirs[@]} -gt 0 ]] || fail "fixture: no tasks"

for task_dir in "${task_dirs[@]}"; do
    task_dir="${task_dir%/}"
    tid="$(basename "$task_dir")"
    tjson="$task_dir/task.json"
    if [[ ! -f "$tjson" || ! -f "$task_dir/reference.patch" ]]; then
        fail "$tid: task.json or reference.patch missing"
        continue
    fi

    id="$(json_get "$tjson" id)"
    kind="$(json_get "$tjson" kind)"
    exp_visible="$(json_get "$tjson" expected.base_visible)"
    exp_hidden="$(json_get "$tjson" expected.base_hidden)"
    exp_after="$(json_get "$tjson" expected.after_all)"
    mapfile -t hidden < <(json_get "$tjson" acceptance.hidden_tests)
    mapfile -t visible_added < <(json_get "$tjson" acceptance.visible_tests_added)
    kinds_seen[$kind]=1

    if [[ "$id" == "$tid" ]]; then pass "$tid: id matches directory"
    else fail "$tid: id '$id' does not match directory"; fi
    case "$kind" in
        feature|bugfix|refactor) want_visible=pass ;;
        failing_then_green) want_visible=fail ;;
        *) fail "$tid: unknown kind '$kind'"; continue ;;
    esac
    if [[ "$exp_visible" == "$want_visible" && "$exp_hidden" == fail && "$exp_after" == pass ]]; then
        pass "$tid: expected outcomes fit kind $kind"
    else
        fail "$tid: expected outcomes ($exp_visible/$exp_hidden/$exp_after) do not fit kind $kind"
    fi

    declared="$(printf '%s\n' "${hidden[@]}" | sort)"
    present="$(if [[ -d "$task_dir/hidden" ]]; then (cd "$task_dir/hidden" && find . -type f -printf '%P\n'); fi | sort)"
    if [[ ${#hidden[@]} -gt 0 && "$declared" == "$present" ]]; then pass "$tid: hidden tests match task.json"
    else fail "$tid: hidden tests on disk do not match task.json"; fi
    declared="$(printf '%s\n' "${visible_added[@]}" | sed '/^$/d' | sort)"
    present="$(if [[ -d "$task_dir/visible" ]]; then (cd "$task_dir/visible" && find . -type f -printf '%P\n'); fi | sort)"
    if [[ "$declared" == "$present" ]]; then pass "$tid: added visible tests match task.json"
    else fail "$tid: added visible tests on disk do not match task.json"; fi

    # Rule 3: the patch never edits a test file.
    touched="$(python3 - "$task_dir/reference.patch" <<'PY'
import re, sys
paths = set()
with open(sys.argv[1], encoding="utf-8") as f:
    for line in f:
        m = re.match(r"^diff --git a/(\S+) b/(\S+)", line)
        if m:
            paths.update(m.groups())
        m = re.match(r"^(?:---|\+\+\+) [ab]/(\S+)", line)
        if m:
            paths.add(m.group(1))
for p in sorted(paths):
    print(p)
PY
)"
    test_edits="$(grep -E '^(tests/|phpunit\.xml)|Test\.php$' <<<"$touched" || true)"
    if [[ -z "$touched" ]]; then fail "$tid: reference.patch touches no file"
    elif [[ -n "$test_edits" ]]; then fail "$tid: reference.patch edits test files: $(tr '\n' ' ' <<<"$test_edits")"
    else pass "$tid: reference.patch edits no test file ($(tr '\n' ' ' <<<"$touched" | sed 's/ $//'))"; fi

    # Fresh copy of repo/ as a git repository.
    work="$WORK_ROOT/$tid"
    repo="$work/repo"
    mkdir -p "$work"
    cp -R "$FIXTURE_DIR/repo" "$repo"
    git -C "$repo" init -q
    git -C "$repo" add -A
    git -C "$repo" -c user.name=adc-fixture -c user.email=fixture@invalid -c commit.gpgsign=false \
        commit -q -m "php-mini base"

    if run_recipe "$repo"; then pass "$tid: dependency recipe (composer install from lock)"
    else fail "$tid: dependency recipe failed"; tail -n 20 "$WORK_ROOT/recipe.log" | sed 's/^/    | /'; continue; fi
    if git -C "$repo" diff --quiet -- composer.lock; then pass "$tid: composer.lock unchanged by the recipe"
    else fail "$tid: the recipe changed composer.lock"; fi

    # Rule 1: base.
    copy_overlay "$task_dir/visible" "$repo" >/dev/null
    expect_outcome "$tid: base visible$OFFLINE_LABEL" "$exp_visible" "$(run_gate "$repo" "$work/results")"
    visible_failed="$(cat "$WORK_ROOT/failed.txt")"
    copy_overlay "$task_dir/hidden" "$repo" >/dev/null
    expect_outcome "$tid: base with hidden tests$OFFLINE_LABEL" "$exp_hidden" "$(run_gate "$repo" "$work/results")"
    if [[ "$kind" == failing_then_green ]]; then
        # The visible overlay already fails the gate, so prove the hidden tests fail themselves, and
        # that nothing fails which did not fail at base visible.
        hidden_hit="" others=""
        while IFS= read -r f; do
            if printf '%s\n' "${hidden[@]}" | grep -qxF "$f"; then hidden_hit+=" $f"
            elif ! grep -qxF "$f" <<<"$visible_failed"; then others+=" $f"; fi
        done <"$WORK_ROOT/failed.txt"
        if [[ -n "$hidden_hit" && -z "$others" ]]; then
            pass "$tid: base with hidden tests fails in the hidden tests:$hidden_hit"
        else
            fail "$tid: base with hidden tests: failing hidden files [${hidden_hit# }] (want some), new failures elsewhere [${others# }]"
        fi
    fi

    # Rule 2: after.
    for rel in "${hidden[@]}"; do rm -f "$repo/$rel"; done
    if git -C "$repo" apply --check "$task_dir/reference.patch" 2>"$WORK_ROOT/apply.log" \
        && git -C "$repo" apply "$task_dir/reference.patch" 2>>"$WORK_ROOT/apply.log"; then
        pass "$tid: reference.patch applies with git apply"
    else
        fail "$tid: reference.patch does not apply: $(head -n 3 "$WORK_ROOT/apply.log" | tr '\n' ' ')"
        continue
    fi
    copy_overlay "$task_dir/hidden" "$repo" >/dev/null
    expect_outcome "$tid: after patch with hidden tests$OFFLINE_LABEL" "$exp_after" "$(run_gate "$repo" "$work/results")"
done

for k in feature bugfix refactor failing_then_green; do
    if [[ -n "${kinds_seen[$k]:-}" ]]; then pass "fixture: has a $k task"
    else fail "fixture: no $k task"; fi
done

echo "php-mini: $((checks - failures))/$checks checks passed"
[[ $failures -eq 0 ]]
