# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# A case's cycle (M1 slice plan §2.1 steps 3-5, §2.3, §2.6; P24b), sourced
# by installed-cycle.sh once adcd runs and the operator is bootstrapped (the
# driver's variables: work, prefix, node, fixtures, project, task, case,
# case_file, run_env, sock, here):
#   cycle_case       creates the case's project (its profile) and task (its
#                    T0 task's title and requirement, its hidden/ files
#                    attached as the acceptance), then waits, at most 25
#                    minutes (a gate's bounded retries wait 1+2+4+8 minutes
#                    before its terminal hold), until the task ends (ended);
#                    a hold that is not a runner's retry ends the wait at
#                    once, so an intermediate one ends it early and the
#                    case's exact holds then fail it: a flaky failure,
#                    never a pass;
#   cycle_check SEQ  once adcd replayed its store (its log's head SEQ): reads
#                    the task again, the event stream from seq 1 to SEQ and
#                    adc dev capabilities, and checks the case's expect
#                    block (cycle-expect.mjs); sets summary to its row.

# shellcheck disable=SC2154 # the driver's variables, above
cycle_case() {
  step "project create (the case's profile)"
  adc project create --expect 0 --reason installed-cycle --json --scope-file "$work/profile.json" >"$work/out/project.out" 2>&1 ||
    die "project create failed: $(excerpt "$work/out/project.out" 400)"
  prj="$(sed -n 's/.*"project":"\(prj_[a-z0-9]*\)".*/\1/p' "$work/out/project.out" | head -n 1)"
  [[ "$prj" == prj_* ]] || die "project create: no project id: $(excerpt "$work/out/project.out" 400)"

  step "task create ($task, its hidden/ attached as its acceptance)"
  local t="$fixtures/t0/$project/tasks/$task" att=() hidden f second
  "$node" -e 'const fs = require("node:fs"); const [task, prj, out] = process.argv.slice(1); const t = JSON.parse(fs.readFileSync(task, "utf8"));
    fs.writeFileSync(out, JSON.stringify({project: prj, title: t.title, requirement: t.requirement}))' "$t/task.json" "$prj" "$work/spec.json"
  # Each acceptance file at its repository path, where the gate overlays it (adc task create's --attach REPO_PATH=FILE).
  # Listed whole first (#226 entry 203): a find that fails refuses, never fewer attachments.
  hidden="$(cd "$t/hidden" && find . -type f | LC_ALL=C sort)" || die "the task's hidden files could not be listed"
  while IFS= read -r f; do
    [[ -n "$f" ]] || continue
    att+=(--attach "${f#./}=$t/hidden/${f#./}")
  done <<<"$hidden"
  adc task create --expect 0 --reason installed-cycle --json --scope-file "$work/spec.json" "${att[@]}" >"$work/out/task.out" 2>&1 ||
    die "task create failed: $(excerpt "$work/out/task.out" 400)"
  tsk="$(sed -n 's/.*"task":"\(tsk_[a-z0-9]*\)".*/\1/p' "$work/out/task.out" | head -n 1)"
  [[ "$tsk" == tsk_* ]] || die "task create: no task id: $(excerpt "$work/out/task.out" 400)"

  until="$(json "$case_file" d.expect.until)"
  step "waiting for task $tsk to end ($until)"
  local deadline=$((SECONDS + 1500))
  until ended; do
    ((SECONDS < deadline)) || die "timed out waiting for the task to end: $(excerpt "$work/out/show.json" 600)"
    sleep 2
  done
  second="$(json "$case_file" 'd.expect.second_task_admitted === false')" || die "the case's second_task_admitted could not be read"
  if [[ "$second" == true ]]; then
    step "a second task while the first is held: it stays unadmitted (I22)"
    adc task create --expect 0 --reason installed-cycle --json --scope-file "$work/spec.json" "${att[@]}" >"$work/out/task2.out" 2>&1 ||
      die "the second task create failed: $(excerpt "$work/out/task2.out" 400)"
    tsk2="$(sed -n 's/.*"task":"\(tsk_[a-z0-9]*\)".*/\1/p' "$work/out/task2.out" | head -n 1)"
    [[ "$tsk2" == tsk_* ]] || die "the second task create: no task id"
    sleep 15 # three of the reconcile loop's 5-second passes (daemon.reconcileEvery), each of which admits a task when none is admitted
    adc task show "$tsk2" --json >"$work/out/second.json" 2>&1 || die "adc task show (the second task) failed: $(excerpt "$work/out/second.json" 300)"
  fi
}

# ended waits for the task with adc task wait --until closed,held (its
# one-task read without the bench ledger, every second, at most 5 s; read
# again until the deadline) into out/show.json, and reports whether it
# ended: closed; its gate completed and its review held review_inactive
# (stub mode's end: nothing more runs); or a node of it but a review held,
# on a hold that is not a runner's bounded retry ("runner: retry N until
# ...", which clears itself). cycle_check reads the whole task, its bench
# included, once. Each read first checks that adcd still runs: one that
# exited fails the case at once (started: its exit status and its log's
# excerpt), never at the deadline (Mythos #435 L1).
ended() {
  local rc=0
  kill -0 "$adcd" 2>/dev/null || started ""
  adc task wait "$tsk" --until closed,held --timeout 5s --every 1s --json >"$work/out/show.json" 2>"$work/out/show.err" || rc=$?
  # 5: neither state within the 5 s, read again; 6, with closed,held asked: its card is unplaced (no column), which no wait ends.
  if ((rc == 6)); then die "task $tsk's card is unplaced: $(excerpt "$work/out/show.err" 300)"; fi
  ((rc == 0)) || return 1
  "$node" -e 'const fs = require("node:fs"); const s = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    const nodes = s.card?.nodes ?? []; const holds = (n) => (n.holds ?? []).map((h) => h.hold);
    const closed = s.card?.column === "done_candidate" || s.card?.column === "done";
    const gated = nodes.some((n) => n.kind === "gate" && n.state === "completed") && nodes.some((n) => n.kind === "review" && holds(n).includes("review_inactive"));
    const held = nodes.some((n) => n.kind !== "review" && (n.holds ?? []).some((h) => !/^runner: retry /.test(h.signal_instance ?? "")));
    process.exit(closed || gated || held ? 0 : 1)' "$work/out/show.json"
}

# shellcheck disable=SC2154 # the driver's variables, above
cycle_check() {
  local head="$1"
  [[ -z "${events:-}" ]] || { kill "$events" 2>/dev/null; wait "$events" 2>/dev/null || true; }
  step "the task, the event stream to seq $head, the capabilities"
  adc task show "$tsk" --json >"$work/out/show.json" 2>"$work/out/show.err" || die "adc task show failed: $(excerpt "$work/out/show.err" 400)"
  "${run_env[@]}" ADC_SOCKET="$sock" timeout 120 "$prefix/bin/adc" events --follow --json >"$work/out/events.jsonl" 2>"$work/out/events.err" &
  events=$!
  record_pid "$work" "$events" || cannot "the event reader's start time cannot be read (its sweep record)"
  # shellcheck disable=SC2317,SC2329  # invoked by within
  streamed() {
    kill -0 "$events" 2>/dev/null || die "the event stream ended: $(excerpt "$work/out/events.err" 300)"
    [[ "$(tail -n 1 "$work/out/events.jsonl" | sed -n 's/^{"seq":\([0-9]*\),.*/\1/p')" == "$head" ]]
  }
  within 60 "the event stream to reach seq $head" streamed
  kill "$events" 2>/dev/null
  wait "$events" 2>/dev/null || true
  events=""
  adc dev capabilities --json >"$work/out/capabilities.json" 2>&1 || die "adc dev capabilities failed: $(excerpt "$work/out/capabilities.json" 300)"
  step "the case's expect block"
  local rc=0
  summary="$("$node" "$here/lib/cycle-expect.mjs" "$case_file" "$work/out" "$case")" || rc=$?
  if ((rc != 0)); then die "the case's expectations do not hold:"$'\n'"$summary"; fi
}
