# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# The live route (M1 slice plan §2.3, checkpoint (a)'s exit record; P25, D2),
# sourced by installed-cycle.sh for --routes live --project repo --issue N
# (the driver's variables: work, root, prefix, lib, node, fixtures, issue,
# issue_dir, profile_file, registry, acceptance, live_canary, bench,
# run_env, sock, here):
#   live_setup  reads fixtures/m1/issues/N/task.json and checks the
#               acceptance (live_acceptance); takes every registry
#               credential from the caller's environment, by the variable
#               adc dev credential-env names (gateway.EnvName; a missing
#               one refuses the run, exit 2, named, never printed); writes
#               the profile (this repository's, its allowed destinations the
#               registry's), the run's own expect block, out/canary and
#               out/canary-forms, each form redacted from every excerpt
#               (known_text), and reads GitHub main's SHA; the profile's gate
#               command is the issue's baseline.json command, measured in the
#               profile's gate environment, or the run is refused;
#   live_case   creates the project and the task (create_task: the copies
#               attached at their repository paths) and reads the task
#               until it ends, at most 90 minutes;
#   live_bench  once the driver stopped adcd and its restart on the same
#               state replayed and verified the log (#226 entry 199),
#               records the benchmark (adc dev bench record) into
#               out/bench.json;
#   live_after  checks that no credential appears under the work directory
#               or the sandbox root, that the egress capture holds the
#               task's requirement and every reviewer call's body, that the
#               canary is in no artifact of the run (live_canary_scan), and
#               that GitHub main's SHA is unchanged;
#   live_write_bench, the driver's last step, after every check: publishes
#               out/bench.json at --bench, whole or not at all, never over a
#               file.
# No credential, the canary or an acceptance store path is ever a child
# process's argument (/proc/PID/cmdline: Opus #440 H1, M2); the driver's own
# argv holds the store paths (--acceptance), never a child's (Opus #440 r2
# N1), and no output names them: a child is given only the copies. Every
# excerpt the driver prints goes through excerpt (secrets.sh), which redacts
# the credentials and the canary's forms before it cuts (Astra #440 H6).

# live_canary_known: writes out/canary (the acceptance's canary, mode 0600
# by the driver's umask) and out/canary-forms (every form it may take: adc
# dev scan --print-forms --canary, the one rule; #226 entry 206), and
# registers each form, not the raw text alone, with known_text: an excerpt
# of a JSON output (show.json, an adapter's stderr) never prints an escaped
# canary (Opus #441 L1).
# shellcheck disable=SC2154 # the driver's variables, above
live_canary_known() {
  local f
  printf '%s\n' "$live_canary" >"$work/out/canary"
  "$adc_scan" dev scan --print-forms --canary "$work/out/canary" >"$work/out/canary-forms" 2>/dev/null || die "the canary's forms could not be made"
  while IFS= read -r f; do known_text "$f"; done <"$work/out/canary-forms"
}

# live_acceptance: checks each --acceptance REPO_PATH=FILE against the
# issue's attachments (each named once, its sha256 the recorded digest),
# copies each into the work directory's acceptance/ (by redirection: from
# here on no child's argv names FILE), sets live_pairs to the copies'
# REPO_PATH=COPY, writes out/acceptance-digests (the canary scan's intended
# copies), and checks that the canary is a line of one of them.
# shellcheck disable=SC2154 # the driver's variables, above
live_acceptance() {
  local t="$issue_dir/task.json" a name file digest names sum want got i=0 rc
  names="$(json "$t" '(d.attachments ?? []).map((a) => a.name).sort().join("\n")')" || cannot "issue $issue's task.json cannot be read"
  [[ -n "$names" ]] || cannot "issue $issue's task.json names no acceptance attachment"
  local given=()
  live_pairs=()
  mkdir "$work/acceptance"
  : >"$work/out/acceptance-digests"
  for a in "${acceptance[@]}"; do
    [[ "$a" == *=* ]] || cannot "--acceptance: an entry without '=' (REPO_PATH=FILE)"
    name="${a%%=*}" file="${a#*=}"
    [[ -n "$name" && -f "$file" && -r "$file" ]] || cannot "--acceptance $name: REPO_PATH=FILE, FILE a readable file"
    digest="$("$node" -e 'const d = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
      console.log((d.attachments ?? []).find((a) => a.name === process.argv[2])?.digest ?? "")' "$t" "$name")" ||
      cannot "issue $issue's task.json cannot be read"
    [[ -n "$digest" ]] || cannot "--acceptance $name: issue $issue's task.json names no such attachment"
    # The copy by redirection: from here on only the work directory's copy is named to any process.
    i=$((i + 1))
    cat <"$file" >"$work/acceptance/$i"
    sum="$(sha256sum <"$work/acceptance/$i" | cut -d' ' -f1)" || cannot "--acceptance $name: its copy cannot be hashed"
    [[ "sha256:$sum" == "$digest" ]] || cannot "--acceptance $name: its file is not the recorded $digest"
    given+=("$name") live_pairs+=("$name=$work/acceptance/$i")
    printf '%s\n' "$digest" >>"$work/out/acceptance-digests"
  done
  # Both lists acquired first, each checked (#226 entry 203).
  got="$(printf '%s\n' "${given[@]}" | LC_ALL=C sort)" || cannot "the --acceptance names cannot be sorted"
  want="$(LC_ALL=C sort <<<"$names")" || cannot "issue $issue's attachments cannot be sorted"
  [[ "$got" == "$want" ]] || cannot "--acceptance must name each of issue $issue's attachments once: ${names//$'\n'/ }"
  # The canary, a line of the acceptance: grep's pattern on its stdin, each copy on another descriptor (Opus #440 M2);
  # grep's 1 is "not this file", any other status a search that failed.
  local found=0
  for ((a = 1; a <= i; a++)); do
    rc=0
    printf '%s\n' "$live_canary" | grep -qxF -f - 3<"$work/acceptance/$a" /dev/fd/3 || rc=$?
    case "$rc" in
      0) found=1 ;;
      1) ;;
      *) cannot "the acceptance files could not be searched for the canary (exit $rc)" ;;
    esac
  done
  ((found)) || cannot "the canary is no line of the acceptance files"
}

# shellcheck disable=SC2154 # the driver's variables, above
# live_mirror checks --repo-mirror DIR, when given (the repository is private:
# neither git ls-remote nor adcd's credential-less clone can read GitHub's):
# a bare git repository of the caller's, its refs/heads/main exactly the
# issue's base commit (baseline.json's base). DIR then stands for GitHub:
# the run's profile's repo.url, and the main read before and after the run.
# shellcheck disable=SC2154 # the driver's variables, above
live_mirror() {
  local base bare main
  repo_label="GitHub main"
  [[ -n "$mirror" ]] || return 0
  repo_label="the mirror's main"
  [[ -d "$mirror" && -O "$mirror" ]] || cannot "--repo-mirror $mirror: a bare git repository of yours"
  mirror="$(cd -- "$mirror" && pwd -P)" || cannot "--repo-mirror $mirror cannot be entered"
  bare="$(mgit rev-parse --is-bare-repository 2>/dev/null)" || bare=""
  [[ "$bare" == true ]] || cannot "--repo-mirror $mirror: a bare git repository of yours"
  mgit rev-parse -q --verify refs/heads/main >/dev/null 2>&1 || cannot "--repo-mirror $mirror holds no refs/heads/main"
  base="$(json "$issue_dir/baseline.json" d.base 2>/dev/null)" || base=""
  [[ "$base" =~ ^[0-9a-f]{40}$ ]] || cannot "issue $issue records no base commit (baseline.json's base)"
  mgit cat-file -e "$base^{commit}" 2>/dev/null || cannot "--repo-mirror $mirror lacks issue $issue's base commit $base"
  # Its main is the base exactly (Opus #445 r2 R1): a descendant would give the run another main than the issue's.
  main="$(mgit rev-parse -q --verify refs/heads/main 2>/dev/null)" || main=""
  [[ "$main" == "$base" ]] || cannot "--repo-mirror $mirror: its main is $main, not issue $issue's base commit $base"
}
# git on the mirror, with no configuration but the repository's own.
mgit() { env -i PATH=/usr/bin:/bin GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null git --git-dir="$mirror" "$@"; }

live_setup() {
  local t="$issue_dir/task.json" ref var f
  step "issue $issue: its task, its acceptance, the registry's credentials"
  live_acceptance
  live_mirror

  # Every registry credential, by reference, from the caller's environment, by the variable gateway.EnvName gives it
  # (adc dev credential-env: names only; Astra #440 M7), as the driver received it across its re-exec, on fd 3
  # (caller_cred: NAME=VALUE; Opus #441 r3 H1).
  live_cred=() live_secret=()
  "${run_env[@]}" "$prefix/bin/adc" dev credential-env "$registry" >"$work/out/credential-env" 2>&1 ||
    cannot "the registry's credential references: $(excerpt "$work/out/credential-env" 300)"
  local e val
  # A last line without its newline is read too, never dropped.
  while read -r ref var || [[ -n "$ref$var" ]]; do
    [[ "$var" =~ ^ADC_CREDENTIAL_[A-Z0-9_]+$ ]] || cannot "adc dev credential-env named no variable for $ref"
    val=""
    for e in "${caller_cred[@]}"; do if [[ "${e%%=*}" == "$var" ]]; then val="${e#*=}"; fi; done
    [[ -n "$val" ]] || cannot "the registry's credential $ref is not in this environment ($var)"
    known "$val"
    live_cred+=("$var=$val") live_secret+=("$val")
  done <"$work/out/credential-env"

  step "the profile (this repository's, the registry's destinations allowed, the gate the issue's measured baseline's) and the expect block"
  local prj want refusal rc=0
  prj="$(json "$profile_file" d.id)" || cannot "the profile cannot be read"
  want="$(json "$t" d.project)" || cannot "issue $issue's task.json cannot be read"
  [[ -n "$prj" && "$prj" == "$want" ]] || cannot "issue $issue's task is project ${want}'s, the profile is $prj"
  # The gate's command is the one the issue's baseline was measured with (live run 4: the profile's ./... is not
  # green in the gate sandbox, so adcd found no baseline for it), in the environment the profile's gate names:
  # its platform, and the profile's pinned image (which pins the Go release). Otherwise the run is refused (exit 2, named).
  # shellcheck disable=SC2016 # the single-quoted program is JavaScript for Node: its ${...} are template literals
  refusal="$("$node" -e 'const fs = require("node:fs"); const [profile, registry, baseline, out, mirror] = process.argv.slice(1);
    const p = JSON.parse(fs.readFileSync(profile, "utf8")); const r = JSON.parse(fs.readFileSync(registry, "utf8"));
    const refuse = (why) => { console.log(why); process.exit(3); };
    let b;
    try { b = JSON.parse(fs.readFileSync(baseline, "utf8")); } catch { refuse("has no readable baseline.json"); }
    const c = b.command;
    if (!Array.isArray(c) || c.length === 0 || !c.every((a) => typeof a === "string" && a !== "")) refuse("its baseline.json has no command");
    const e = b.environment ?? {}; const envs = p.gate?.environments ?? [];
    if (envs.length === 0) refuse("the profile names no gate environment");
    for (const g of envs) {
      const rt = g.facts?.runtime ?? {}; const platform = `${rt.os}/${rt.arch}`;
      if (e.platform !== platform || e.image !== p.toolchain?.images?.go)
        refuse(`its baseline.json was measured in another environment (${e.image}, ${e.platform}) than the profile gate environment ${g.name} (${p.toolchain?.images?.go}, ${platform})`);
    }
    p.gate.command = c;
    p.data_policy.allowed_destinations = [...new Set(r.executors.map((e) => e.destination.id))].sort();
    if (mirror) p.repo.url = mirror;
    fs.writeFileSync(out, JSON.stringify(p))' "$profile_file" "$registry" "$issue_dir/baseline.json" "$work/profile.json" "$mirror")" || rc=$?
  case "$rc" in
    0) ;;
    3) cannot "issue $issue $refusal" ;;
    *) die "the profile failed" ;;
  esac
  live_canary_known
  # shellcheck disable=SC2016 # the single-quoted program is JavaScript for Node: its ${...} are template literals
  "$node" -e 'const fs = require("node:fs"); const [issue, out] = process.argv.slice(1);
    const r = (o, x) => ({ outcome: o, reason: x });
    fs.writeFileSync(out, JSON.stringify({ project: "repo", task: `issue-${issue}`, scenario: "live",
      control: "§2.3 assertion 2: the candidate, its review a qualified route; any other end fails it.",
      expect: { until: "candidate", merge: false, holds: [], canaries: [],
        nodes: { build: r("found", "completed_with_findings"), accept: r("empty", "completed_clean"), gate: r("empty", "completed_clean"), review: r("empty", "completed_clean") } } }))' \
    "$issue" "$work/live-expect.json" || die "the expect block failed"

  step "$repo_label's SHA, before the run"
  repo_url="$(json "$work/profile.json" d.repo.url)" || cannot "the profile cannot be read"
  main_before="$(env -i PATH=/usr/bin:/bin GIT_TERMINAL_PROMPT=0 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    timeout 60 git ls-remote -- "$repo_url" refs/heads/main | cut -f1)" || cannot "$repo_url cannot be read"
  [[ "$main_before" =~ ^[0-9a-f]{40}$ ]] || cannot "$repo_url names no main"
}

# shellcheck disable=SC2154 # the driver's variables, above
live_case() {
  create_task "$issue_dir/task.json" "${live_pairs[@]}"
  step "waiting for task $tsk to end (closed, or held)"
  local deadline=$((SECONDS + 5400))
  until ended; do
    ((SECONDS < deadline)) || die "timed out waiting for the task to end: $(excerpt "$work/out/show.json" 600)"
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
    die "adc dev bench record failed: $(excerpt "$work/out/bench.err" 400)"
}

# live_canary_scan: no artifact of the run holds the canary (adc dev scan
# --canary: every form, JSON decoded; Astra #440 H4, H6; #226 entry 206):
# the work directory, its logs, outputs, state and egress capture included
# (--links-inside: a link out of it is a scan error), and the sandbox root
# and its runs/ (in the root's user namespace, its links neither followed nor reported),
# but for the intended copies (out/canary and out/canary-forms by their
# paths, and any file that is an acceptance attachment by its sha256). A
# scan that cannot read a file, or never ran, fails the run.
# shellcheck disable=SC2154 # the driver's variables, above
live_canary_scan() {
  local rc=0 runs
  "$adc_scan" dev scan --canary "$work/out/canary" --own "$work/out/canary" --own "$work/out/canary-forms" \
    --own-digests "$work/out/acceptance-digests" --links-inside -- "$work" >"$work/out/canary-hits" 2>"$work/out/canary-scan.err" || rc=$?
  case "$rc" in
    0) ;;
    1) die "the acceptance canary appears in: $(excerpt "$work/out/canary-hits" 600)" ;;
    *) die "the work directory could not be searched for the canary (exit $rc)" ;;
  esac
  # The root's runs/ is required (runs_required, Astra #441 r2 High 1): one that does not resolve is a scan error.
  runs="$(runs_required "$root")" || die "the sandbox root's runs/ could not be resolved for the canary search"
  rc=0
  # shellcheck disable=SC2016 # the inner bash's own expansions
  root_scanned "$root" '"$@"' "$adc_scan" dev scan --canary "$work/out/canary" --own-digests "$work/out/acceptance-digests" -- "$root" "$runs" \
    </dev/null >"$work/out/canary-hits" 2>"$work/out/canary-scan.err" || rc=$?
  case "$rc" in
    0) ;;
    1) die "the acceptance canary appears in the sandbox root: $(excerpt "$work/out/canary-hits" 600)" ;;
    *) die "the sandbox root could not be searched for the canary (exit $rc)" ;;
  esac
}

# shellcheck disable=SC2154 # the driver's variables, above
live_after() {
  local v caps req main_after h digests n=0
  step "no credential under the work directory or the sandbox root; the egress capture: the requirement and every reviewer call; the canary nowhere"
  for v in "${live_secret[@]}"; do
    scan "" "$v" "$work"
    root_scan "$v"
  done
  caps="$work/state/captures"
  [[ -d "$caps" ]] || die "no egress capture: adcd ran without --capture-egress, or captured nothing"
  req="$(json "$issue_dir/task.json" 'd.requirement.split("\n")[0].slice(0, 60)')" || die "the issue's task.json cannot be read"
  # An empty requirement line would match every capture: no search at all.
  [[ -n "$req" ]] || die "the issue's requirement has an empty first line: nothing to find in the capture"
  grep -rqaF -- "$req" "$caps" || die "no captured request carries the task's requirement: the capture is not what was sent"
  # Every reviewer call's body is kept (Opus #440 L3; the checker requires its blob.capture_kept): its bytes in the capture store.
  # The digests acquired whole first (#226 entry 203): a read that fails refuses, never fewer bodies checked.
  digests="$("$node" -e 'const fs = require("node:fs"); const [show, ev] = process.argv.slice(1); const s = JSON.parse(fs.readFileSync(show, "utf8"));
    const calls = new Set((s.bench?.leases ?? []).filter((l) => l.role === "reviewer").flatMap((l) => l.calls.map((c) => c.id)));
    for (const line of fs.readFileSync(ev, "utf8").split("\n")) { if (!line) continue; const e = JSON.parse(line);
      if (e.kind === "blob.capture_kept/v1" && calls.has(e.payload?.call)) console.log(e.payload.digest); }' "$work/out/show.json" "$work/out/events.jsonl")" ||
    die "the reviewer calls' captured bodies could not be listed"
  while IFS= read -r h; do
    [[ -n "$h" ]] || continue
    h="${h#sha256:}" n=$((n + 1))
    [[ "$h" =~ ^[0-9a-f]{64}$ && -f "$caps/blobs/sha256/${h:0:2}/${h:2:2}/$h" ]] || die "a reviewer call's captured body ($h) is not in the capture store"
  done <<<"$digests"
  ((n > 0)) || die "no reviewer call's body was captured"
  live_canary_scan
  step "$repo_label's SHA, after the run: unchanged"
  main_after="$(env -i PATH=/usr/bin:/bin GIT_TERMINAL_PROMPT=0 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    timeout 60 git ls-remote -- "$repo_url" refs/heads/main | cut -f1)" || die "$repo_url cannot be read after the run"
  [[ "$main_after" == "$main_before" ]] || die "$repo_label moved during the run: $main_before → $main_after"
}

# live_write_bench: out/bench.json published at --bench, the driver's last
# step (Opus #440 M1), as adc's own writeOnce publishes (internal/cli/bench.go;
# Astra #440 M8): a temporary file in --bench's directory, written, made
# 0644 and synced, then linked at --bench, which fails if anything is there
# (L2); the temporary file is removed whatever happens, so --bench is the
# whole record or absent.
# shellcheck disable=SC2154 # the driver's variables, above
live_write_bench() {
  local tmp dir rc=0
  dir="$(dirname "$bench")" || die "the benchmark's directory cannot be named"
  tmp="$(mktemp -p "$dir" .bench-XXXXXXXX)" || die "the benchmark cannot be written beside $bench"
  { cat <"$work/out/bench.json" >"$tmp" && chmod 0644 "$tmp" && sync "$tmp" && ln -- "$tmp" "$bench"; } 2>/dev/null || rc=$?
  rm -f "$tmp"
  ((rc == 0)) || die "the benchmark cannot be written to $bench (it exists, or its directory is not writable)"
  step "the benchmark record: $bench"
}
