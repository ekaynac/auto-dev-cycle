// SPDX-License-Identifier: Apache-2.0
//
// A case's expect block against what its run shows (M1 slice plan §2.3,
// §2.6, §2.7; P24b; #226 entries 188, 189), run by installed-cycle.sh with
// the installed Node once the run ended and adcd replayed its store:
//
//   node cycle-expect.mjs CASE_JSON OUT_DIR CASE
//
// OUT_DIR holds the run's records: show.json (adc task show --json, its
// bench read giving each node's outcome and reason), events.jsonl (adc
// events --json from seq 1 to the log's head), stub.jsonl (the stub
// provider's log), adcd-1.log, capabilities.json (adc dev capabilities
// --json) and canary. It prints one "fail: ..." line per assertion that
// does not hold and exits 1 when there is one; otherwise it prints the
// case's summary row (a Markdown table row: case, end, nodes, holds,
// tokens) and exits 0.
//
// Stub mode ends at the gate (#226 entry 189): review stays inactive, since
// no release's records qualify the stub's reviewer route. So a case that
// expects the candidate ends with its gate completed and its review node
// held review_inactive, adcd's start naming why; "closed as a candidate
// after review" is the live route's (D2).

import { readFileSync } from "node:fs";
import { join } from "node:path";

const [caseFile, out, name] = process.argv.slice(2);
const read = (f) => readFileSync(join(out, f), "utf8");
const lines = (f) => read(f).split("\n").filter((l) => l !== "").map((l) => JSON.parse(l));
const c = JSON.parse(readFileSync(caseFile, "utf8"));
const e = c.expect;
const show = JSON.parse(read("show.json"));
const events = lines("events.jsonl");
const fails0 = [];
// The stub's log is required: a missing or unreadable one is a failure, never no requests (Opus #435 M3).
let stub = [];
try {
  stub = lines("stub.jsonl");
} catch (err) {
  fails0.push(`the stub provider's log cannot be read: ${err.message}`);
}
const log = read("adcd-1.log");
const caps = JSON.parse(read("capabilities.json"));
const canary = read("canary").trim();

const fails = fails0;
const fail = (why) => fails.push(why);
const card = show.card ?? {};
const nodes = card.nodes ?? [];
const bench = show.bench?.nodes;
if (!Array.isArray(bench)) fail("the task read carries no bench nodes (the operator reads attempts and model calls)");
const holdsOf = (n) => (n.holds ?? []).map((h) => h.hold);
const reviews = nodes.filter((n) => n.kind === "review");

// The review stays inactive in stub mode, and adcd's start says why.
if (!/^adcd: review stays inactive: P31's qualification on record: /m.test(log)) fail("adcd's start does not name why review is inactive");
for (const r of reviews) if (!holdsOf(r).includes("review_inactive")) fail(`review ${r.id} is ${r.state}, not held review_inactive`);

// How the task ends.
if (card.column === "done" || card.column === "done_candidate") fail(`the task closed (${card.column}): stub mode never reviews`);
if (e.until === "candidate") {
  const gates = nodes.filter((n) => n.kind === "gate");
  if (gates.length === 0 || !gates.every((g) => g.state === "completed")) fail(`the gate is not completed: ${gates.map((g) => g.state).join(",")}`);
  if (reviews.length === 0) fail("no review node: the candidate template's review was never recorded");
}
const held = [...new Set(nodes.filter((n) => n.kind !== "review").flatMap(holdsOf))].sort();
const want = [...new Set(e.holds)].sort();
if (held.join(",") !== want.join(",")) fail(`the task's holds are [${held}], not [${want}]`);
const signals = nodes.flatMap((n) => (n.holds ?? []).map((h) => h.signal_instance));
for (const s of e.hold_signals ?? []) if (!signals.includes(s)) fail(`no hold's signal is "${s}": [${signals.join("; ")}]`);

// Each listed kind's results; no claim of an absent kind. A kind whose
// results are never selected (selected: false) has no selected result on
// any node; its outcome is read where the task read shows it: the first
// attempt's own result, when the first attempt is such a node's.
for (const [kind, r] of Object.entries(e.nodes)) {
  const all = (bench ?? []).filter((n) => n.kind === kind);
  if (r.selected === false) {
    if (all.length === 0) fail(`no ${kind} node`);
    for (const n of all) if (n.outcome !== null) fail(`${kind} ${n.id} (round ${n.round}) has a selected result, ${n.outcome}/${n.reason}`);
    const first = show.first_attempt;
    if (first && all.some((n) => n.id === first.node) && (first.outcome !== r.outcome || first.reason !== r.reason)) {
      fail(`the first attempt (${kind} ${first.node}) recorded ${first.outcome}/${first.reason}, not ${r.outcome}/${r.reason}`);
    }
    continue;
  }
  const of = all.filter((n) => n.outcome !== null);
  if (of.length === 0) fail(`no ${kind} node recorded a result`);
  for (const n of of) {
    if (n.outcome !== r.outcome || n.reason !== r.reason) fail(`${kind} ${n.id} (round ${n.round}) is ${n.outcome}/${n.reason}, not ${r.outcome}/${r.reason}`);
  }
}
for (const kind of e.absent ?? []) {
  for (const n of (bench ?? []).filter((n) => n.kind === kind && n.claimed_at !== null)) fail(`${kind} ${n.id} was claimed`);
}

// Merge disabled (§2.3 assertion 4).
if (e.merge !== false) fail("checkpoint (a)'s cases expect no merge");
if (nodes.some((n) => n.kind === "merge") || (bench ?? []).some((n) => n.kind === "merge")) fail("a merge node is recorded");
const cap = (name) => (caps.capabilities ?? []).find((x) => x.name === name);
for (const name of ["merge-local", "merge-github"]) {
  if (cap(name) === undefined || cap(name).active !== false) fail(`adc dev capabilities shows ${name} not inactive`);
}

// Approval before the first claim (§2.3 assertion 1).
const seqOf = (kind) => events.find((x) => x.kind === kind)?.seq;
const approved = seqOf("approval.recorded/v1"), claimed = seqOf("node.claimed/v1");
if (approved === undefined || claimed === undefined || approved > claimed) fail(`approval.recorded (${approved}) does not precede the first node.claimed (${claimed})`);
for (let i = 1; i < events.length; i++) if (events[i].seq !== events[i - 1].seq + 1) fail(`the event stream skips from seq ${events[i - 1].seq} to ${events[i].seq}`);

// Protected acceptance and the hostile canaries (§2.3 assertion 3, §2.7):
// in no builder or reviewer request the stub logged.
const requests = stub.filter((x) => x.kind === "request");
// A full log stopped logging requests: what came after it is unchecked (Mythos #435 L2).
if (stub.some((x) => x.kind === "log_full")) fail("the stub provider's log is full: requests past it went unlogged");
const roleOf = (x) => (x.model === "stub-builder-1" ? "builder" : x.model === "stub-reviewer-1" ? "reviewer" : "other");
const sub = (s) => s.replaceAll("{canary}", canary);
for (const x of requests) {
  const body = JSON.stringify(x.body);
  for (const k of e.canaries) if (body.includes(k)) fail(`canary ${k} in a ${roleOf(x)} request (${x.request_id})`);
  if (roleOf(x) === "reviewer") for (const k of (e.reviewer_canaries ?? []).map(sub)) if (body.includes(k)) fail(`reviewer canary ${k} in a reviewer request (${x.request_id})`);
  if (roleOf(x) === "other") fail(`a request for model ${x.model}`);
  // The canary minted for this run (§2.7 hostile-gate-output's gate writes it into its results mount) reaches no request at all.
  if (body.includes(canary)) fail(`the run's minted canary in a ${roleOf(x)} request (${x.request_id})`);
}
// Nor either saved operator read (Opus #435 H1).
for (const f of ["show.json", "events.jsonl"]) if (read(f).includes(canary)) fail(`the run's minted canary in the operator's ${f}`);
// An attempt that started made at least one builder request the stub logged (Opus #435 M3).
if (events.some((x) => x.kind === "attempt.started/v1") && !requests.some((x) => roleOf(x) === "builder")) fail("an attempt started, and the stub logged no builder request");
for (const s of e.builder_sees ?? []) {
  if (!requests.some((x) => roleOf(x) === "builder" && JSON.stringify(x.body).includes(JSON.stringify(s).slice(1, -1)))) fail(`no builder request carries ${s}`);
}

// Tokens: what the stub served, and the task read's first attempt.
const served = stub.filter((x) => x.kind === "usage" && x.usage).reduce((t, x) => ({ in: t.in + x.usage.prompt_tokens, out: t.out + x.usage.completion_tokens }), { in: 0, out: 0 });
if (e.usage && (served.in !== e.usage.in || served.out !== e.usage.out)) fail(`the stub served ${served.in}/${served.out} tokens, not ${e.usage.in}/${e.usage.out}`);
const first = show.first_attempt;
if (requests.length > 0) {
  if (first === null || first === undefined) fail("the task read has no first attempt");
  else {
    // An absent key is as missing as a null one (Mythos #435 L3).
    for (const k of ["seconds_after_approval", "status", "tokens", "charge"]) if (first[k] == null) fail(`the first attempt's ${k} is null or absent`);
  }
}

// A launch refused before its start (§2.3 assertion 6): no attempt, no
// model call, and every lease granted released.
if (e.no_attempt === true) {
  if (events.some((x) => x.kind === "attempt.started/v1")) fail("an attempt started");
  if (requests.length > 0) fail(`the stub served ${requests.length} requests`);
  const granted = events.filter((x) => x.kind === "lease.granted/v1").map((x) => x.subject);
  const released = new Set(events.filter((x) => x.kind === "lease.released/v1").map((x) => x.subject));
  for (const l of granted) if (!released.has(l)) fail(`lease ${l} was granted and not released`);
}
// One admitted task, held included (I22).
if (e.second_task_admitted === false) {
  const second = JSON.parse(read("second.json"));
  if (second.card?.admitted !== false) fail(`the second task is admitted (${second.card?.admitted}) while the first is held`);
}
// §2.7 hostile-model-switch's requests from inside the attempt's container
// are not sent by this driver: a case that names them cannot pass here.
if ((e.gateway_requests ?? []).length > 0) fail("gateway_requests: this driver does not send requests on the attempt's gateway socket");

if (fails.length > 0) {
  for (const f of fails) console.log(`fail: ${f}`);
  process.exit(1);
}
const results = Object.keys(e.nodes).map((k) => `${k} ${e.nodes[k].outcome}/${e.nodes[k].reason}${e.nodes[k].selected === false ? " (none selected)" : ""}`).join("; ") || "no result";
const ended = e.until === "candidate" ? "gate completed, review held review_inactive" : `held: ${want.join(", ")}`;
const reviewers = requests.filter((x) => roleOf(x) === "reviewer").length;
console.log(`| ${name} | ${ended} | ${results} | ${served.in}/${served.out} | ${requests.length - reviewers}/${reviewers} |`);
