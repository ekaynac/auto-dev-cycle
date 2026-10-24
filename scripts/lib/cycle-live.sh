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
#               named once, its sha256 the recorded digest) and the canary
#               against them (a line of an acceptance file), takes every
#               registry credential from the caller's environment
#               (ADC_CREDENTIAL_<REF>; a missing one refuses the run, exit 2,
#               named, never printed), writes the profile (this repository's,
#               its allowed destinations the registry's) and the run's own
#               expect block, and reads GitHub main's SHA;
#   live_case   creates the project and the task (its acceptance attached at
#               its repository paths) and reads the task until it ends, at
#               most 90 minutes;
#   live_bench  once the driver stopped adcd and its restart on the same
#               state replayed and verified the log (#226 entry 199),
#               records the benchmark (adc dev bench record) into
#               out/bench.json;
#   live_after  checks that no credential appears under the work directory,
#               that the egress capture holds the task's requirement and
#               never the canary, and that GitHub main's SHA is unchanged;
#               then writes the benchmark to --bench.
#   redacted    an excerpt of a run's output with each acceptance file's path
#               and the canary replaced: the operator's store path and the
#               unpublished canary are never printed (credentials never reach
#               the output: they stay in the environment, and the scan fails
#               the run on any copy under the work directory).

# shellcheck disable=SC2154 # the driver's variables, above
# redacted FILE BYTES: FILE's first BYTES bytes, one line, redacted.
redacted() {
  local text a
  text="$(head -c "$2" "$1" 2>/dev/null | tr '\n' ' ')"
  for a in "${acceptance[@]}"; do text="${text//"${a#*=}"/<acceptance file>}"; done
  printf '%s' "${text//"$live_canary"/<canary>}"
}

# shellcheck disable=SC2154 # the driver's variables, above
live_setup() {
  local t="$issue_dir/task.json" a name file digest names
  step "issue $issue: its task, its acceptance, the registry's credentials"
  names="$(json "$t" '(d.attachments ?? []).map((a) => a.name).sort().join("\n")')"
  [[ -n "$names" ]] || cannot "issue $issue's task.json names no acceptance attachment"
  local given=()
  for a in "${acceptance[@]}"; do
    name="${a%%=*}" file="${a#*=}"
    [[ "$a" == *=* && -n "$name" && -f "$file" && -r "$file" ]] || cannot "--acceptance $name: REPO_PATH=FILE, FILE a readable file"
    digest="$("$node" -e 'const d = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
      console.log((d.attachments ?? []).find((a) => a.name === process.argv[2])?.digest ?? "")' "$t" "$name")"
    [[ -n "$digest" ]] || cannot "--acceptance $name: issue $issue's task.json names no such attachment"
    [[ "sha256:$(sha256sum <"$file" | cut -d' ' -f1)" == "$digest" ]] || cannot "--acceptance $name: its file is not the recorded $digest"
    given+=("$name")
  done
  [[ "$(printf '%s\n' "${given[@]}" | LC_ALL=C sort)" == "$(LC_ALL=C sort <<<"$names")" ]] ||
    cannot "--acceptance must name each of issue $issue's attachments once: $(tr '\n' ' ' <<<"$names")"
  local found=0
  for a in "${acceptance[@]}"; do grep -qxF -- "$live_canary" "${a#*=}" && found=1; done
  ((found)) || cannot "--canary is no line of the acceptance files"

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
  # shellcheck disable=SC2016 # the single-quoted program is JavaScript for Node: its ${...} are template literals
  "$node" -e 'const fs = require("node:fs"); const [issue, canary, out] = process.argv.slice(1);
    const r = (o, x) => ({ outcome: o, reason: x });
    fs.writeFileSync(out, JSON.stringify({ project: "repo", task: `issue-${issue}`, scenario: "live",
      control: "§2.3 assertion 2: the candidate, its review a qualified route; any other end fails it.",
      expect: { until: "candidate", merge: false, holds: [], canaries: [canary],
        nodes: { build: r("found", "completed_with_findings"), accept: r("empty", "completed_clean"), gate: r("empty", "completed_clean"), review: r("empty", "completed_clean") } } }))' \
    "$issue" "$live_canary" "$work/live-expect.json" || die "the expect block failed"
  # The acceptance's canary: the checker asserts it in neither saved operator read (adc task show, the event stream).
  printf '%s\n' "$live_canary" >"$work/out/canary"

  step "GitHub main's SHA, before the run"
  repo_url="$(json "$profile_file" d.repo.url)"
  main_before="$(env -i PATH=/usr/bin:/bin GIT_TERMINAL_PROMPT=0 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    timeout 60 git ls-remote -- "$repo_url" refs/heads/main | cut -f1)" || cannot "$repo_url cannot be read"
  [[ "$main_before" =~ ^[0-9a-f]{40}$ ]] || cannot "$repo_url names no main"
}

# shellcheck disable=SC2154 # the driver's variables, above
live_case() {
  local t="$issue_dir/task.json" att=() a
  step "project create (this repository's profile)"
  adc project create --expect 0 --reason installed-cycle --json --scope-file "$work/profile.json" >"$work/out/project.out" 2>&1 ||
    die "project create failed: $(redacted "$work/out/project.out" 400)"
  prj="$(sed -n 's/.*"project":"\(prj_[a-z0-9]*\)".*/\1/p' "$work/out/project.out" | head -n 1)"
  [[ "$prj" == prj_* ]] || die "project create: no project id: $(redacted "$work/out/project.out" 400)"
  step "task create (issue $issue, its acceptance attached at its repository paths)"
  "$node" -e 'const fs = require("node:fs"); const [task, prj, out] = process.argv.slice(1); const t = JSON.parse(fs.readFileSync(task, "utf8"));
    fs.writeFileSync(out, JSON.stringify({project: prj, title: t.title, requirement: t.requirement}))' "$t" "$prj" "$work/spec.json"
  for a in "${acceptance[@]}"; do att+=(--attach "$a"); done
  adc task create --expect 0 --reason installed-cycle --json --scope-file "$work/spec.json" "${att[@]}" >"$work/out/task.out" 2>&1 ||
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

# shellcheck disable=SC2154 # the driver's variables, above
live_after() {
  local v caps req main_after
  step "no credential under the work directory or the sandbox root; the egress capture: the requirement, never the canary"
  for v in "${live_secret[@]}"; do
    scan "" "$v" "$work"
    root_scan "$v"
  done
  caps="$work/state/captures"
  [[ -d "$caps" ]] || die "no egress capture: adcd ran without --capture-egress, or captured nothing"
  req="$(json "$issue_dir/task.json" 'd.requirement.split("\n")[0].slice(0, 60)')"
  grep -rqaF -- "$req" "$caps" || die "no captured request carries the task's requirement: the capture is not what was sent"
  if grep -rqaF -- "$live_canary" "$caps"; then die "the acceptance canary appears in a captured request"; fi
  step "GitHub main's SHA, after the run: unchanged"
  main_after="$(env -i PATH=/usr/bin:/bin GIT_TERMINAL_PROMPT=0 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    timeout 60 git ls-remote -- "$repo_url" refs/heads/main | cut -f1)" || die "$repo_url cannot be read after the run"
  [[ "$main_after" == "$main_before" ]] || die "GitHub main moved during the run: $main_before → $main_after"
  install -m 0644 "$work/out/bench.json" "$bench" || die "the benchmark cannot be written to $bench"
  step "the benchmark record: $bench"
}
