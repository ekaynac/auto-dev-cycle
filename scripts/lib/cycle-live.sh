# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# The live route (M1 slice plan §2.3, checkpoint (a)'s exit record; P25, D2),
# sourced by installed-cycle.sh for --routes live --project repo --issue N
# (the driver's variables: work, prefix, lib, node, fixtures, issue,
# issue_dir, profile_file, registry, acceptance, live_canary, bench,
# run_env, sock, here):
#   live_setup  reads fixtures/m1/issues/N/task.json, checks each
#               --acceptance REPO_PATH=FILE against its attachments (each
#               named once, its sha256 the recorded digest), copies each
#               into the work directory (acceptance/, by redirection: no
#               process's argv names FILE), and the canary against them (a
#               line of an acceptance file); takes every registry credential
#               from the caller's environment (ADC_CREDENTIAL_<REF>; a
#               missing one refuses the run, exit 2, named, never printed);
#               writes the profile (this repository's, its allowed
#               destinations the registry's), the run's own expect block,
#               out/canary and out/canary-forms (every form the canary may
#               take in a JSON document, cycle-expect.mjs --canary-forms),
#               and reads GitHub main's SHA;
#   live_case   creates the project and the task (the copies attached at
#               their repository paths) and reads the task until it ends, at
#               most 90 minutes;
#   live_bench  once the driver stopped adcd and its restart on the same
#               state replayed and verified the log (#226 entry 199),
#               records the benchmark (adc dev bench record) into
#               out/bench.json;
#   live_after  checks that no credential appears under the work directory
#               or the sandbox root, that the egress capture holds the
#               task's requirement and every reviewer call's body, and no
#               form of the canary (canary_in), and that GitHub main's SHA
#               is unchanged;
#   live_write_bench, the driver's last step, after every check: writes
#               out/bench.json to --bench, never over a file.
#   redacted    an excerpt of a run's output with each acceptance file's path
#               and the canary replaced: the operator's store path and the
#               unpublished canary are never printed (credentials never reach
#               the output: they stay in the environment, and the scan fails
#               the run on any copy under the work directory).
# No credential, canary or acceptance path is ever a child process's
# argument (/proc/PID/cmdline: Opus #440 H1, M2): they go to grep and node
# on stdin or in files of the work directory.

# shellcheck disable=SC2154 # the driver's variables, above
# redacted FILE BYTES: FILE's first BYTES bytes, one line, redacted.
redacted() {
  local text a
  text="$(head -c "$2" "$1" 2>/dev/null | tr '\n' ' ')"
  for a in "${acceptance[@]}"; do text="${text//"${a#*=}"/<acceptance file>}"; done
  printf '%s' "${text//"$live_canary"/<canary>}"
}

# live_acceptance: checks each --acceptance REPO_PATH=FILE against the
# issue's attachments (each named once, its sha256 the recorded digest),
# copies each into the work directory's acceptance/ (by redirection: from
# here on no process's argv names FILE), sets live_attach to the copies'
# --attach arguments, and checks that the canary is a line of one of them.
# shellcheck disable=SC2154 # the driver's variables, above
live_acceptance() {
  local t="$issue_dir/task.json" a name file digest names i=0
  names="$(json "$t" '(d.attachments ?? []).map((a) => a.name).sort().join("\n")')"
  [[ -n "$names" ]] || cannot "issue $issue's task.json names no acceptance attachment"
  local given=()
  live_attach=()
  mkdir "$work/acceptance"
  for a in "${acceptance[@]}"; do
    name="${a%%=*}" file="${a#*=}"
    [[ "$a" == *=* && -n "$name" && -f "$file" && -r "$file" ]] || cannot "--acceptance $name: REPO_PATH=FILE, FILE a readable file"
    digest="$("$node" -e 'const d = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
      console.log((d.attachments ?? []).find((a) => a.name === process.argv[2])?.digest ?? "")' "$t" "$name")"
    [[ -n "$digest" ]] || cannot "--acceptance $name: issue $issue's task.json names no such attachment"
    # The copy by redirection: from here on only the work directory's copy is named to any process.
    i=$((i + 1))
    cat <"$file" >"$work/acceptance/$i"
    [[ "sha256:$(sha256sum <"$work/acceptance/$i" | cut -d' ' -f1)" == "$digest" ]] || cannot "--acceptance $name: its file is not the recorded $digest"
    given+=("$name") live_attach+=(--attach "$name=$work/acceptance/$i")
  done
  [[ "$(printf '%s\n' "${given[@]}" | LC_ALL=C sort)" == "$(LC_ALL=C sort <<<"$names")" ]] ||
    cannot "--acceptance must name each of issue $issue's attachments once: $(tr '\n' ' ' <<<"$names")"
  # The canary, a line of the acceptance: grep's pattern on its stdin, each copy on another descriptor (Opus #440 M2).
  local found=0
  for ((a = 1; a <= i; a++)); do
    if printf '%s\n' "$live_canary" | grep -qxF -f - 3<"$work/acceptance/$a" /dev/fd/3; then found=1; fi
  done
  ((found)) || cannot "the canary is no line of the acceptance files"
}

# shellcheck disable=SC2154 # the driver's variables, above
live_setup() {
  local t="$issue_dir/task.json"
  step "issue $issue: its task, its acceptance, the registry's credentials"
  live_acceptance

  # Every registry credential, by reference, from the caller's environment
  # (gateway.EnvName: ADC_CREDENTIAL_<REF upper-cased, '-' as '_'>).
  live_cred=() live_secret=()
  local ref var
  while IFS= read -r ref; do
    [[ -n "$ref" ]] || continue
    [[ "$ref" != *_* ]] || cannot "the registry's credential reference $ref has no environment variable"
    var="ADC_CREDENTIAL_$(tr 'a-z-' 'A-Z_' <<<"$ref")"
    [[ -n "${!var:-}" ]] || cannot "the registry's credential $ref is not in this environment ($var)"
    known "${!var}"
    live_cred+=("$var=${!var}") live_secret+=("${!var}")
  done < <(json "$registry" '[...new Set(d.executors.map((e) => e.credential_ref).filter((r) => r))].join("\n")')

  step "the profile (this repository's, the registry's destinations allowed) and the expect block"
  local prj
  prj="$(json "$profile_file" d.id)"
  [[ "$prj" == "$(json "$t" d.project)" ]] || cannot "issue $issue's task is project $(json "$t" d.project)'s, the profile is $prj"
  "$node" -e 'const fs = require("node:fs"); const [profile, registry, out] = process.argv.slice(1);
    const p = JSON.parse(fs.readFileSync(profile, "utf8")); const r = JSON.parse(fs.readFileSync(registry, "utf8"));
    p.data_policy.allowed_destinations = [...new Set(r.executors.map((e) => e.destination.id))].sort();
    fs.writeFileSync(out, JSON.stringify(p))' "$profile_file" "$registry" "$work/profile.json" || die "the profile failed"
  # The acceptance's canary (the checker asserts it in neither saved operator read: adc task show, the event stream),
  # and every form it may take in a JSON document (Opus #440 H2), both read from files by node.
  printf '%s\n' "$live_canary" >"$work/out/canary"
  "$node" "$here/lib/cycle-expect.mjs" --canary-forms "$work/out" || die "the canary's forms failed"
  # shellcheck disable=SC2016 # the single-quoted program is JavaScript for Node: its ${...} are template literals
  "$node" -e 'const fs = require("node:fs"); const [issue, out, dir] = process.argv.slice(1);
    const canary = fs.readFileSync(`${dir}/canary`, "utf8").replace(/\n$/, "");
    const r = (o, x) => ({ outcome: o, reason: x });
    fs.writeFileSync(out, JSON.stringify({ project: "repo", task: `issue-${issue}`, scenario: "live",
      control: "§2.3 assertion 2: the candidate, its review a qualified route; any other end fails it.",
      expect: { until: "candidate", merge: false, holds: [], canaries: [canary],
        nodes: { build: r("found", "completed_with_findings"), accept: r("empty", "completed_clean"), gate: r("empty", "completed_clean"), review: r("empty", "completed_clean") } } }))' \
    "$issue" "$work/live-expect.json" "$work/out" || die "the expect block failed"

  step "GitHub main's SHA, before the run"
  repo_url="$(json "$profile_file" d.repo.url)"
  main_before="$(env -i PATH=/usr/bin:/bin GIT_TERMINAL_PROMPT=0 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    timeout 60 git ls-remote -- "$repo_url" refs/heads/main | cut -f1)" || cannot "$repo_url cannot be read"
  [[ "$main_before" =~ ^[0-9a-f]{40}$ ]] || cannot "$repo_url names no main"
}

# shellcheck disable=SC2154 # the driver's variables, above
live_case() {
  local t="$issue_dir/task.json"
  step "project create (this repository's profile)"
  adc project create --expect 0 --reason installed-cycle --json --scope-file "$work/profile.json" >"$work/out/project.out" 2>&1 ||
    die "project create failed: $(redacted "$work/out/project.out" 400)"
  prj="$(sed -n 's/.*"project":"\(prj_[a-z0-9]*\)".*/\1/p' "$work/out/project.out" | head -n 1)"
  [[ "$prj" == prj_* ]] || die "project create: no project id: $(redacted "$work/out/project.out" 400)"
  step "task create (issue $issue, its acceptance attached at its repository paths)"
  "$node" -e 'const fs = require("node:fs"); const [task, prj, out] = process.argv.slice(1); const t = JSON.parse(fs.readFileSync(task, "utf8"));
    fs.writeFileSync(out, JSON.stringify({project: prj, title: t.title, requirement: t.requirement}))' "$t" "$prj" "$work/spec.json"
  adc task create --expect 0 --reason installed-cycle --json --scope-file "$work/spec.json" "${live_attach[@]}" >"$work/out/task.out" 2>&1 ||
    die "task create failed: $(redacted "$work/out/task.out" 400)"
  tsk="$(sed -n 's/.*"task":"\(tsk_[a-z0-9]*\)".*/\1/p' "$work/out/task.out" | head -n 1)"
  [[ "$tsk" == tsk_* ]] || die "task create: no task id: $(redacted "$work/out/task.out" 400)"

  step "waiting for task $tsk to end (closed, or held)"
  local deadline=$((SECONDS + 5400))
  until ended; do
    ((SECONDS < deadline)) || die "timed out waiting for the task to end: $(redacted "$work/out/show.json" 600)"
    sleep 10
  done
}

# live_bench records the benchmark (adc dev bench record) into
# out/bench.json. The driver calls it only after it stopped adcd and started
# it again on the same state, the start's replay verifying every projection
# against the log (#226 entry 199): a refused start has already failed the
# run with its excerpt, so no benchmark is ever recorded without it.
# shellcheck disable=SC2154 # the driver's variables, above
live_bench() {
  step "the benchmark (adc dev bench record), from the restarted adcd"
  "${run_env[@]}" ADC_SOCKET="$sock" timeout 300 "$prefix/bin/adc" dev bench record --task "$tsk" --out "$work/out/bench.json" \
    --registry "$work/registry.json" --routes live --until candidate --root "$lib" \
    --fixture fixtures/m1/profiles/auto-dev-cycle.json --fixture "fixtures/m1/issues/$issue" >"$work/out/bench.summary" 2>"$work/out/bench.err" ||
    die "adc dev bench record failed: $(redacted "$work/out/bench.err" 400)"
}

# canary_in DIR...: prints the files under the DIRs holding any form of the
# canary (out/canary-forms: raw, JSON-escaped, trimmed; Opus #440 H2), the
# forms grep's patterns from a file, never an argument.
canary_in() {
  grep -rlaF -f "$work/out/canary-forms" -- "$@"
}

# shellcheck disable=SC2154 # the driver's variables, above
live_after() {
  local v caps req main_after h n=0 rc=0
  step "no credential under the work directory or the sandbox root; the egress capture: the requirement and every reviewer call, never the canary"
  for v in "${live_secret[@]}"; do
    scan "" "$v" "$work"
    root_scan "$v"
  done
  caps="$work/state/captures"
  [[ -d "$caps" ]] || die "no egress capture: adcd ran without --capture-egress, or captured nothing"
  req="$(json "$issue_dir/task.json" 'd.requirement.split("\n")[0].slice(0, 60)')"
  grep -rqaF -- "$req" "$caps" || die "no captured request carries the task's requirement: the capture is not what was sent"
  # Every reviewer call's body is kept (Opus #440 L3; the checker requires its blob.capture_kept): its bytes in the capture store.
  while IFS= read -r h; do
    h="${h#sha256:}" n=$((n + 1))
    [[ "$h" =~ ^[0-9a-f]{64}$ && -f "$caps/blobs/sha256/${h:0:2}/${h:2:2}/$h" ]] || die "a reviewer call's captured body ($h) is not in the capture store"
  done < <("$node" -e 'const fs = require("node:fs"); const [show, ev] = process.argv.slice(1); const s = JSON.parse(fs.readFileSync(show, "utf8"));
    const calls = new Set((s.bench?.leases ?? []).filter((l) => l.role === "reviewer").flatMap((l) => l.calls.map((c) => c.id)));
    for (const line of fs.readFileSync(ev, "utf8").split("\n")) { if (!line) continue; const e = JSON.parse(line);
      if (e.kind === "blob.capture_kept/v1" && calls.has(e.payload?.call)) console.log(e.payload.digest); }' "$work/out/show.json" "$work/out/events.jsonl")
  ((n > 0)) || die "no reviewer call's body was captured"
  canary_in "$caps" >"$work/out/canary-hits" || rc=$?
  ((rc <= 1)) || die "the egress capture could not be searched for the canary (exit $rc)"
  [[ ! -s "$work/out/canary-hits" ]] || die "the acceptance canary appears in a captured request"
  step "GitHub main's SHA, after the run: unchanged"
  main_after="$(env -i PATH=/usr/bin:/bin GIT_TERMINAL_PROMPT=0 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    timeout 60 git ls-remote -- "$repo_url" refs/heads/main | cut -f1)" || die "$repo_url cannot be read after the run"
  [[ "$main_after" == "$main_before" ]] || die "GitHub main moved during the run: $main_before → $main_after"
}

# live_write_bench: out/bench.json to --bench, the driver's last step (Opus
# #440 M1): never over a file, even one made since the start (L2: set -C).
# shellcheck disable=SC2154 # the driver's variables, above
live_write_bench() {
  (umask 022 && set -C && cat <"$work/out/bench.json" >"$bench") 2>/dev/null || die "the benchmark cannot be written to $bench (it exists, or its directory is not writable)"
  step "the benchmark record: $bench"
}
