#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Well-formedness check for the go-mini T0 fixture (fixtures/t0/README.md rules 1-4).
# For every task it works in a fresh temporary git copy of repo/ and prints one line per check.
# Exits non-zero if any check deviates.
set -euo pipefail

FIXTURE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIXTURE_JSON="$FIXTURE_DIR/fixture.json"
REPO_DIR="$FIXTURE_DIR/repo"
TASKS_DIR="$FIXTURE_DIR/tasks"
GO_FALLBACK_BIN="$HOME/.local/go/bin"
KINDS="feature bugfix refactor failing_then_green"

for tool in jq git; do
	command -v "$tool" >/dev/null || { echo "FAIL setup: $tool not found" >&2; exit 2; }
done
if ! command -v go >/dev/null && [[ -x "$GO_FALLBACK_BIN/go" ]]; then
	PATH="$GO_FALLBACK_BIN:$PATH"
fi
command -v go >/dev/null || { echo "FAIL setup: go not found" >&2; exit 2; }

# Offline, local toolchain, no workspace from any parent directory (rule 4).
export GOTOOLCHAIN=local GOFLAGS=-mod=mod GOPROXY=off GOWORK=off GOSUMDB=off

want_version="$(jq -r '.toolchain.go' "$FIXTURE_JSON")"
mapfile -t GATE_CMD < <(jq -r '.gate.command[]' "$FIXTURE_JSON")
RESULTS_PATH="$(jq -r '.results_path' "$FIXTURE_JSON")"
ADAPTER="$(jq -r '.result_adapter' "$FIXTURE_JSON")"

failures=0
WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "$WORK_ROOT"' EXIT

report() { # report <ok:0|1> <message>
	if [[ "$1" == 0 ]]; then
		echo "PASS $2"
	else
		echo "FAIL $2"
		failures=$((failures + 1))
	fi
}

check_eq() { # check_eq <label> <got> <want>
	if [[ "$2" == "$3" ]]; then report 0 "$1: $2"; else report 1 "$1: got $2, want $3"; fi
}

# --- fixture-level checks ---------------------------------------------------
got_version="$(go env GOVERSION)"
check_eq "toolchain version" "${got_version#go}" "$want_version"
check_eq "result adapter" "$ADAPTER" "go_test_json"
check_eq "results path declared" "$([[ -n "$RESULTS_PATH" && "$RESULTS_PATH" != null ]] && echo yes || echo no)" "yes"
check_eq "dependency recipe" "$(jq -r '.dependency_recipe.command' "$FIXTURE_JSON")" "null"

# fresh_copy <dest> <task_dir>: repo/ plus the task's visible overlay, committed as the base.
fresh_copy() {
	local dest="$1" task_dir="$2"
	cp -R "$REPO_DIR" "$dest"
	if [[ -d "$task_dir/visible" ]]; then
		cp -R "$task_dir/visible/." "$dest/"
	fi
	git -C "$dest" init -q
	git -C "$dest" add -A
	git -C "$dest" -c user.name=fixture -c user.email=fixture@invalid commit -q -m base
}

copy_hidden() { # copy_hidden <dest> <task_dir>
	cp -R "$2/hidden/." "$1/"
}

# run_gate <repo> -> prints pass|fail; writes the JSON stream to RESULTS_PATH inside the repo.
# Also verifies the stream exists, is non-empty and every line parses as a JSON object.
run_gate() {
	local repo="$1" out rc=0
	out="$repo/$RESULTS_PATH"
	mkdir -p "$(dirname "$out")"
	(cd "$repo" && "${GATE_CMD[@]}" >"$out" 2>"$repo/.gate-stderr") || rc=$?
	if [[ ! -s "$out" ]]; then
		echo "nostream"
		return
	fi
	# -R reads raw lines, so each line must be exactly one JSON go test event.
	if ! jq -e -R 'fromjson | if type == "object" and has("Action") then true else error("not a go test event") end' \
		"$out" >/dev/null 2>&1; then
		echo "badjson"
		return
	fi
	local fails
	fails="$(jq -r 'select(.Action == "fail" or .Action == "build-fail") | .Action' "$out" | wc -l)"
	if [[ "$rc" == 0 && "$fails" == 0 ]]; then echo pass; else echo fail; fi
}

# test_names <file...>: top-level Test functions declared in the given Go test files.
test_names() {
	sed -n 's/^func \(Test[A-Za-z0-9_]*\)(t \*testing\.T).*/\1/p' "$@"
}

seen_kinds=""
for task_dir in "$TASKS_DIR"/*/; do
	task_dir="${task_dir%/}"
	dir_id="$(basename "$task_dir")"
	tj="$task_dir/task.json"
	if ! jq -e . "$tj" >/dev/null 2>&1; then
		report 1 "$dir_id task.json parses"
		continue
	fi
	id="$(jq -r '.id' "$tj")"
	kind="$(jq -r '.kind' "$tj")"
	exp_bv="$(jq -r '.expected.base_visible' "$tj")"
	exp_bh="$(jq -r '.expected.base_hidden' "$tj")"
	exp_after="$(jq -r '.expected.after_all' "$tj")"
	mapfile -t hidden < <(jq -r '.acceptance.hidden_tests[]' "$tj")
	mapfile -t added < <(jq -r '.acceptance.visible_tests_added // [] | .[]' "$tj")
	patch="$task_dir/reference.patch"

	check_eq "$id id matches directory" "$id" "$dir_id"
	check_eq "$id kind is known" "$([[ " $KINDS " == *" $kind "* ]] && echo yes || echo no)" "yes"
	seen_kinds+=" $kind"
	if [[ "$kind" == failing_then_green ]]; then want_bv=fail; else want_bv=pass; fi
	check_eq "$id expected.base_visible fits kind" "$exp_bv" "$want_bv"
	check_eq "$id expected.base_hidden" "$exp_bh" "fail"
	check_eq "$id expected.after_all" "$exp_after" "pass"

	# hidden/ holds exactly the declared hidden tests
	declared="$(printf '%s\n' "${hidden[@]}" | sort)"
	present="$(cd "$task_dir/hidden" && find . -type f | sed 's|^\./||' | sort)"
	check_eq "$id hidden/ matches acceptance.hidden_tests" "$([[ "$declared" == "$present" ]] && echo yes || echo no)" "yes"
	if [[ -d "$task_dir/visible" ]]; then
		declared_v="$(printf '%s\n' "${added[@]}" | sort)"
		present_v="$(cd "$task_dir/visible" && find . -type f | sed 's|^\./||' | sort)"
		check_eq "$id visible/ matches acceptance.visible_tests_added" \
			"$([[ "$declared_v" == "$present_v" ]] && echo yes || echo no)" "yes"
	fi

	# Rule 3: the patch never edits a test file (or a hidden/visible test path).
	# Run outside any repository: inside a git subdirectory, git apply filters paths by the cwd.
	touched="$(cd "$WORK_ROOT" && git apply --numstat "$patch" | cut -f3)" || touched=""
	renamed="$(sed -n 's/^\(rename\|copy\) \(from\|to\) //p' "$patch")"
	all_touched="$(printf '%s\n%s\n' "$touched" "$renamed" | sed '/^$/d' | sort -u)"
	check_eq "$id rule3 patch touches files" "$([[ -n "$all_touched" ]] && echo yes || echo no)" "yes"
	bad="$(printf '%s\n' "$all_touched" | grep -E '(_test\.go$|(^|/)testdata/)' || true)"
	for f in "${hidden[@]}" "${added[@]}"; do
		if printf '%s\n' "$all_touched" | grep -qxF "$f"; then bad+=" $f"; fi
	done
	check_eq "$id rule3 patch edits no test file" "${bad:-none}" "none"

	# Rule 1: base visible, then base with hidden tests.
	base="$WORK_ROOT/$id-base"
	fresh_copy "$base" "$task_dir"
	r="$(run_gate "$base")"
	check_eq "$id rule1 base visible" "$r" "$exp_bv"
	copy_hidden "$base" "$task_dir"
	r="$(run_gate "$base")"
	check_eq "$id rule1 base with hidden" "$r" "$exp_bh"

	# Rule 2 + 4: fresh copy, apply the patch, add hidden tests, run the gate offline.
	after="$WORK_ROOT/$id-after"
	fresh_copy "$after" "$task_dir"
	if git -C "$after" apply --check "$patch" 2>/dev/null && git -C "$after" apply "$patch"; then
		report 0 "$id rule2 reference.patch applies"
	else
		report 1 "$id rule2 reference.patch applies"
		continue
	fi
	copy_hidden "$after" "$task_dir"
	r="$(run_gate "$after")"
	check_eq "$id rule2+4 gate after patch (offline)" "$r" "$exp_after"
	stream="$after/$RESULTS_PATH"
	check_eq "$id rule4 $ADAPTER stream at $RESULTS_PATH" \
		"$([[ -s "$stream" ]] && jq -e . "$stream" >/dev/null 2>&1 && echo valid || echo invalid)" "valid"

	# Every hidden and visible test function reports a per-test pass event.
	mapfile -t visible_tests < <(git -C "$after" ls-files '*_test.go')
	mapfile -t names < <(cd "$after" && test_names "${visible_tests[@]}" "${hidden[@]}" | sort -u)
	missing=""
	for n in "${names[@]}"; do
		if [[ -z "$(jq -r --arg n "$n" 'select(.Test == $n and .Action == "pass") | .Test' "$stream")" ]]; then
			missing+=" $n"
		fi
	done
	check_eq "$id rule4 per-test pass events (${#names[@]} tests)" "${missing:-none}" "none"
done

for k in $KINDS; do
	check_eq "task kind $k present" "$([[ " $seen_kinds " == *" $k "* ]] && echo yes || echo no)" "yes"
done

if [[ "$failures" -gt 0 ]]; then
	echo "go-mini: $failures check(s) FAILED"
	exit 1
fi
echo "go-mini: all checks passed"
