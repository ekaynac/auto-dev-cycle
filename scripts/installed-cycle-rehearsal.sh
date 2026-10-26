#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The live route's rehearsal (#440, Astra's step 0; #226 entry 201), run by
# make installed-cycle-rehearsal over a SCRATCH installed prefix (make's
# INSTALL_PREFIX, removed when make ends):
#
#   installed-cycle-rehearsal.sh PREFIX
#
# It runs the real driver's --routes live, from its setup to its last step,
# against the stub provider served as the live endpoint, so every line of
# the live route runs where no real credential, acceptance or provider is:
#   1. The scratch release becomes the rehearsal's: PREFIX/lib/adc/rehearsal
#      (the driver then labels every line "rehearsal (stub-qualified,
#      scratch)" and publishes its record only inside its work directory);
#      the live profile is go-mini's (its repository a local bare remote of
#      go-mini-feature-1's base); the issue (fixtures/m1/issues/999) is
#      go-mini-feature-1's task, its acceptance the task's hidden test with a
#      canary line added ("\tconst rehearsalCanary = \"ADC-<…>-HEX\"": a tab,
#      quotes and angle brackets, which JSON and Go escape).
#   2. One stub provider (scenario go-mini-feature-1) is the registry's one
#      destination (fixtures/m1/config/registry-stub.json, its endpoint the
#      stub's, its id "rehearsal": adc dev bench record refuses the stub's
#      destination on a live route), its credential minted here and given in
#      the environment only.
#   3. The reviewer route is qualified by the real procedure, in the scratch
#      release only: TestQualifyLive (-tags qualify), twice (two seeds),
#      against the same stub, which answers each suite prompt its expected
#      verdict (--answer-by-prompt: the suite's prompt digests, for the
#      reviewer's model alone; never the stub set's), its records written
#      under PREFIX's records/ and removed at the end.
#   4. The live run: exit 0, every line labelled, the record published at
#      the work directory's out/published/bench.json (valid JSON).
#   5. Five failing runs, each exiting non-zero with no record published: a
#      verify-log that fails, a restart that fails, a sandbox-root scan that
#      never runs, the canary in adcd's log, and the canary in a builder's
#      write_file argument, so in the next request's body as the real
#      adapter, gateway and capture send and keep it (JavaScript's escaping
#      inside Go's, its angle brackets \u003c and \u003e; Astra #440 H4):
#      the capture must be named among the hits. PREFIX/lib/adc/rehearsal
#      names the fault; wrappers around the installed adc and adcd, and a
#      podman wrapper (--podman), read it; for the last, the stub is started
#      again on its port with a scenario whose builder writes the canary.
#   6. The repository's fixtures/m1/review-qualification/ and every release
#      prefix outside the scratch one (dist/*/lib/adc) are byte-identical to
#      what they were before.
# It exits 0 when all hold, 1 when one does not.
set -euo pipefail

die() { echo "installed-cycle-rehearsal: FAILED: $*" >&2; exit 1; }
say() { echo "installed-cycle-rehearsal: rehearsal (stub-qualified, scratch): $*"; }
[[ $# -eq 1 && -d "$1" ]] || { echo "usage: installed-cycle-rehearsal.sh PREFIX" >&2; exit 64; }
prefix="$(cd "$1" && pwd)"
# The repository from $0, never BASH_SOURCE, which bash imports from the environment (Opus #443 L1).
repo="$(cd "$(dirname "$0")/.." && pwd)"
lib="$prefix/lib/adc"
node="$lib/node/bin/node"
issue=999
for f in "$prefix/bin/adc" "$prefix/bin/adcd" "$node" "$lib/scripts/installed-cycle.sh"; do [[ -x "$f" ]] || die "$f is not installed"; done
[[ ! -e "$lib/rehearsal" && ! -e "$lib/fixtures/m1/issues/$issue" ]] || die "$prefix is not a fresh scratch release"
PODMAN=/usr/bin/podman
# shellcheck source=scripts/lib/sandbox-root.sh
. "$lib/scripts/lib/sandbox-root.sh"
scratch="$(mktemp -d)" stub="" kept=() reviewer="" rc=0 w=""
cleanup() {
  local w
  if [[ -n "$stub" ]]; then kill "$stub" 2>/dev/null || true; wait "$stub" 2>/dev/null || true; fi
  for w in "${kept[@]}"; do
    case "$w" in
      /tmp/adc??????) sandbox_root_remove "$w" ;;
      *) rm -rf "$w" ;;
    esac
  done
  rm -rf "$scratch"
  # The rehearsal's qualification records (the stub reviewer's, by its executor id) and their reports.
  if [[ -n "${reviewer:-}" ]]; then rm -rf "$lib/fixtures/m1/review-qualification/records/"*"-${reviewer//_/-}"*; fi
}
trap cleanup EXIT

# What must not change: the repository's qualification fixtures and every release prefix but the scratch one.
untouched() { find "$repo/fixtures/m1/review-qualification" "$repo"/dist/*/lib/adc -type f -exec sha256sum -- {} + 2>/dev/null | LC_ALL=C sort; }
before="$(untouched)"

say "the scratch release: the issue, its acceptance and canary, the profile, the registry"
: >"$lib/rehearsal"
t="$repo/fixtures/t0/go-mini/tasks/go-mini-feature-1"
# shellcheck source=scripts/lib/base-remote.sh
. "$lib/scripts/lib/base-remote.sh"
base_remote "$lib/fixtures/t0/go-mini/repo" "$lib/fixtures/t0/go-mini/tasks/go-mini-feature-1" "" "$scratch/remote.git" >/dev/null || die "the base remote"
hex="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
[[ "$hex" =~ ^[0-9a-f]{16}$ ]] || die "no random canary"
canary="$(printf '\tconst rehearsalCanary = "ADC-<rehearsal>-%s"' "$hex")"
mkdir -p "$scratch/acceptance"
{ cat "$t/hidden/inventory/transfer_hidden_test.go"; printf '\n%s\n' "$canary"; } >"$scratch/acceptance/transfer_hidden_test.go"
(umask 077 && printf '%s\n' "$canary" >"$scratch/canary")
mkdir -p "$lib/fixtures/m1/issues/$issue"
"$node" -e 'const fs = require("node:fs"); const c = require("node:crypto");
  const [task, profile, accept, remote, out, outProfile] = process.argv.slice(1);
  const t = JSON.parse(fs.readFileSync(task, "utf8")); const p = JSON.parse(fs.readFileSync(profile, "utf8"));
  const digest = "sha256:" + c.createHash("sha256").update(fs.readFileSync(accept)).digest("hex");
  fs.writeFileSync(out, JSON.stringify({ project: p.id, title: t.title, requirement: t.requirement,
    attachments: [{ name: "inventory/transfer_hidden_test.go", digest }] }));
  p.repo.url = remote; fs.writeFileSync(outProfile, JSON.stringify(p));' \
  "$t/task.json" "$lib/fixtures/m1/profiles/go-mini.json" "$scratch/acceptance/transfer_hidden_test.go" "$scratch/remote.git" \
  "$lib/fixtures/m1/issues/$issue/task.json" "$lib/fixtures/m1/profiles/auto-dev-cycle.json"

say "the stub provider as the live endpoint, answering the qualification suite's prompts by their digests"
"$node" -e 'const s = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
  console.log(JSON.stringify({ model: "stub-reviewer-1", answers: Object.fromEntries(s.cases.map((c) => [c.prompt_digest, c.expect])) }))' \
  "$repo/fixtures/m1/review-qualification/suite.json" >"$scratch/answers.json"
# go-mini-feature-1's scenario, replayed the same by every run: the builder's replies by its conversation's turns, the reviewer's go persistent.
"$node" -e 'const fs = require("node:fs"); const s = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  s.calls["stub-builder-1"] = { turns: s.calls["stub-builder-1"].replies }; s.calls["stub-reviewer-1"] = { persistent: s.calls["stub-reviewer-1"].replies[0] };
  s.name += "-rehearsal"; fs.writeFileSync(process.argv[2], JSON.stringify(s));' \
  "$lib/fixtures/m1/stub-provider/scenarios/go-mini-feature-1.json" "$scratch/scenario.json"
"$node" "$lib/fixtures/m1/stub-provider/server.mjs" --scenario "$scratch/scenario.json" \
  --listen 127.0.0.1:0 --log "$scratch/stub.jsonl" --answer-by-prompt "$scratch/answers.json" >"$scratch/stub.out" 2>"$scratch/stub.err" &
stub=$!
for _ in $(seq 300); do grep -q '^listening ' "$scratch/stub.out" 2>/dev/null && break; sleep 0.1; done
endpoint="$(sed -n 's/^listening //p' "$scratch/stub.out")"
[[ -n "$endpoint" ]] || die "the stub provider did not listen: $(head -c 300 "$scratch/stub.err")"
"$node" -e 'const fs = require("node:fs"); const d = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  for (const e of d.executors) Object.assign(e.destination, { id: "rehearsal", endpoint: process.argv[2] }); fs.writeFileSync(process.argv[3], JSON.stringify(d));' \
  "$lib/fixtures/m1/config/registry-stub.json" "$endpoint" "$scratch/registry.json"
reviewer="$("$node" -e 'console.log(JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8")).executors.find((e) => e.role === "reviewer").id)' "$scratch/registry.json")"
ADC_CREDENTIAL_STUB="$(head -c 32 /dev/urandom | base64 -w0 | tr '+/' '-_' | tr -d =)"
export ADC_CREDENTIAL_STUB

say "the reviewer route qualified by the real procedure, twice, into the scratch release only"
records="$lib/fixtures/m1/review-qualification/records"
for run in 1 2; do
  (cd "$repo" && ADC_QUALIFY_REGISTRY="$scratch/registry.json" ADC_QUALIFY_GROUPS="$lib/fixtures/m1/config/groups.json" \
    ADC_QUALIFY_EXECUTOR="$reviewer" ADC_QUALIFY_OUT="$records" \
    go test -count=1 -tags qualify -run TestQualifyLive -timeout 30m ./internal/qualify/ >"$scratch/qualify-$run.log" 2>&1) ||
    die "qualification run $run: $(tail -c 600 "$scratch/qualify-$run.log")"
  sleep 1.1 # a record's name is its second
done
# Wrappers: the fault named in $lib/rehearsal, read by the installed adc and adcd and by --podman.
mv "$prefix/bin/adc" "$prefix/bin/adc.real"
mv "$prefix/bin/adcd" "$prefix/bin/adcd.real"
cat >"$prefix/bin/adc" <<EOF
#!/bin/sh
if [ "\$(cat '$lib/rehearsal')" = verify-log ] && [ "\$1 \$2" = "dev verify-log" ]; then echo "rehearsal: verify-log fails" >&2; exit 1; fi
exec '$prefix/bin/adc.real' "\$@"
EOF
cat >"$prefix/bin/adcd" <<EOF
#!/bin/sh
fault="\$(cat '$lib/rehearsal')" state=""
prev=""; for a in "\$@"; do [ "\$prev" = --state-dir ] && state="\$a"; prev="\$a"; done
work="\$(dirname "\$state")"
n=\$(( \$(cat "\$work/.starts" 2>/dev/null || echo 0) + 1 )); echo "\$n" >"\$work/.starts"
if [ "\$fault" = restart ] && [ "\$n" = 2 ]; then echo "rehearsal: the restart fails" >&2; exit 3; fi
if [ "\$fault" = canary-log ]; then cat "\$work/out/canary"; fi
exec '$prefix/bin/adcd.real' "\$@"
EOF
cat >"$scratch/podman" <<EOF
#!/bin/sh
case " \$* " in *" unshare bash -c "*) [ "\$(cat '$lib/rehearsal')" = scan ] && { echo "rehearsal: the scan never runs" >&2; exit 1; } ;; esac
exec $PODMAN "\$@"
EOF
chmod 0755 "$prefix/bin/adc" "$prefix/bin/adcd" "$scratch/podman"

# rehearse FAULT: the driver's live run; sets rc (its exit) and w (its kept work directory).
rehearse() {
  printf '%s' "$1" >"$lib/rehearsal"
  local out="$scratch/run-${1:-ok}.log" r unlabelled g=0
  rc=0
  (cd "$scratch" && "$lib/scripts/installed-cycle.sh" "$prefix" --routes live --project repo --issue "$issue" --until candidate \
    --bench work --registry "$scratch/registry.json" --groups "$lib/fixtures/m1/config/groups.json" \
    --acceptance "inventory/transfer_hidden_test.go=$scratch/acceptance/transfer_hidden_test.go" --canary-file "$scratch/canary" \
    --podman "$scratch/podman" --keep) >"$out" 2>&1 || rc=$?
  w="$(sed -n 's/^installed-cycle: rehearsal (stub-qualified, scratch): kept \([^ ]*\).*/\1/p' "$out")"
  r="$(sed -n 's/.* and the sandbox root \(.*\)$/\1/p' "$out" | head -n 1)"
  [[ -n "$w" ]] && kept+=("$w")
  [[ -n "$r" ]] && kept+=("$r")
  # The other lines, acquired first: grep's 1 is "every line labelled", any other status a log not read (#226 entry 203).
  unlabelled="$(grep -v '^installed-cycle: rehearsal (stub-qualified, scratch):' "$out")" || g=$?
  ((g <= 1)) || die "${1:-the run}: its output could not be read (grep exit $g)"
  if grep -q '^installed-cycle' <<<"$unlabelled"; then die "${1:-the run}: a line not labelled a rehearsal: $(grep '^installed-cycle' <<<"$unlabelled" | head -n 3)"; fi
}

say "the live run"
rehearse ""
[[ "$rc" == 0 ]] || die "the live run exited $rc: $(tail -c 1500 "$scratch/run-ok.log")"
if ! [[ -s "$w/out/published/bench.json" ]] || ! "$node" -e 'JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"))' "$w/out/published/bench.json"; then
  die "the live run published no record"
fi
grep -h '^installed-cycle: rehearsal (stub-qualified, scratch): summary: ' "$scratch/run-ok.log" || die "no summary row"

# Each failing run must fail where its fault is, with that failure's own message.
declare -A because=([verify-log]="the log does not verify" [restart]="adcd exited (exit 3)" [scan]="could not be searched for a secret (exit 3)"
  [canary-log]="the acceptance canary appears in: ")
for fault in verify-log restart scan canary-log; do
  say "a failing run: $fault"
  rehearse "$fault"
  [[ "$rc" != 0 ]] || die "$fault: the run exited 0"
  [[ -n "$w" && ! -e "$w/out/published/bench.json" ]] || die "$fault: no kept work directory, or a record was published"
  grep -qF -- "${because[$fault]}" "$scratch/run-$fault.log" ||
    die "$fault: it failed otherwise: $(grep -v ': kept \|never share it' "$scratch/run-$fault.log" | tail -n 1 | head -c 300)"
  say "$fault: exit $rc, no record: $(grep -F -- "${because[$fault]}" "$scratch/run-$fault.log" | head -n 1 | head -c 300)"
done

say "a failing run: canary-request"
kill "$stub" 2>/dev/null || true
wait "$stub" 2>/dev/null || true
"$node" -e 'const fs = require("node:fs"); const [scenario, canary, out] = process.argv.slice(1); const s = JSON.parse(fs.readFileSync(scenario, "utf8"));
  const w = s.calls["stub-builder-1"].turns[0].write; w["inventory/transfer.go"] += "\n//" + fs.readFileSync(canary, "utf8").replace(/\n$/, "") + "\n";
  s.name += "-canary"; fs.writeFileSync(out, JSON.stringify(s));' \
  "$scratch/scenario.json" "$scratch/canary" "$scratch/canary-scenario.json"
: >"$scratch/stub.out"
listen="${endpoint#http://}" listen="${listen%/v1}"
"$node" "$lib/fixtures/m1/stub-provider/server.mjs" --scenario "$scratch/canary-scenario.json" --listen "$listen" \
  --log "$scratch/stub.jsonl" >"$scratch/stub.out" 2>"$scratch/stub.err" &
stub=$!
for _ in $(seq 300); do grep -q '^listening ' "$scratch/stub.out" 2>/dev/null && break; sleep 0.1; done
again="$(sed -n 's/^listening //p' "$scratch/stub.out")"
[[ "$again" == "$endpoint" ]] || die "the stub provider did not listen again at $listen: $(head -c 300 "$scratch/stub.err")"
rehearse canary-request
[[ "$rc" != 0 && -n "$w" && ! -e "$w/out/published/bench.json" ]] || die "canary-request: exit $rc, or a record was published"
grep -q '/state/captures/' "$w/out/canary-hits" ||
  die "canary-request: the capture is not among the canary's hits: $(grep -v ': kept \|never share it' "$scratch/run-canary-request.log" | tail -n 1 | head -c 600)"
say "canary-request: exit $rc, the capture among the hits, no record"

after="$(untouched)"
[[ "$after" == "$before" ]] || die "the repository's qualification fixtures or a release prefix changed"
say "ok: the live route from its setup to its record, and five failures each without one, each where its fault is; nothing outside the scratch release changed"
