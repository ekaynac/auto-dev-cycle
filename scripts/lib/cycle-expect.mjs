// SPDX-License-Identifier: Apache-2.0
//
// A case's expect block against what its run shows (M1 slice plan §2.3,
// §2.6, §2.7; P24b; #226 entries 188, 189), run by installed-cycle.sh with
// the installed Node once the run ended and adcd replayed its store:
//
//   node cycle-expect.mjs CASE_JSON OUT_DIR CASE [stub|live]
//   node cycle-expect.mjs --canary-forms OUT_DIR
//
// --canary-forms writes OUT_DIR/canary-forms, one per line, every form
// OUT_DIR/canary may take where it would leak (Opus #440 H2): as is, and
// trimmed of its surrounding whitespace; each as JSON escapes it once (a
// request body), twice (a JSON document inside a string), and as Go's
// encoding/json escapes it (the task read and the event stream: <, >, &,
// U+2028 and U+2029 as \u escapes). The driver's grep reads them as its
// patterns, never as arguments; this checker uses the same forms.
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
// after review" is the live route's (D2): in live mode (§2.3's live route,
// its expect block the driver's own) review is active and the task closes
// a candidate (done_candidate), and the canaries are the driver's to check
// in the gateway's egress capture (there is no stub log).

import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const canaryForms = (canary) => {
  const esc = (s) => JSON.stringify(s).slice(1, -1);
  const goEsc = (s) => esc(s).replace(/[<>&\u2028\u2029]/g, (ch) => `\\u${ch.charCodeAt(0).toString(16).padStart(4, "0")}`);
  const forms = [];
  for (const s of [canary, canary.trim()]) forms.push(s, esc(s), esc(esc(s)), goEsc(s), goEsc(goEsc(s)));
  return [...new Set(forms)].filter((s) => s !== "");
};
const canaryOf = (dir) => readFileSync(join(dir, "canary"), "utf8").replace(/\n$/, "");
if (process.argv[2] === "--canary-forms") {
  writeFileSync(join(process.argv[3], "canary-forms"), canaryForms(canaryOf(process.argv[3])).join("\n") + "\n", { mode: 0o600 });
  process.exit(0);
}

const [caseFile, out, name, mode = "stub"] = process.argv.slice(2);
const live = mode === "live";
const read = (f) => readFileSync(join(out, f), "utf8");
const lines = (f) => read(f).split("\n").filter((l) => l !== "").map((l) => JSON.parse(l));
const c = JSON.parse(readFileSync(caseFile, "utf8"));
const e = c.expect;
const show = JSON.parse(read("show.json"));
const events = lines("events.jsonl");
const fails0 = [];
// Stub mode: the stub's log is required, a missing or unreadable one a failure, never no requests (Opus #435 M3).
// Live mode has no stub log: the driver checks the gateway's egress capture.
let stub = [];
if (!live) {
  try {
    stub = lines("stub.jsonl");
  } catch (err) {
    fails0.push(`the stub provider's log cannot be read: ${err.message}`);
  }
}
const log = read("adcd-1.log");
const caps = JSON.parse(read("capabilities.json"));
const canary = canaryOf(out);
// Any form of it (raw, trimmed, JSON-escaped; Opus #440 H2) counts as it.
const forms = canaryForms(canary);
const holdsCanary = (text) => forms.some((f) => text.includes(f));

const fails = fails0;
const fail = (why) => fails.push(why);
const card = show.card ?? {};
const nodes = card.nodes ?? [];
const bench = show.bench?.nodes;
if (!Array.isArray(bench)) fail("the task read carries no bench nodes (the operator reads attempts and model calls)");
const holdsOf = (n) => (n.holds ?? []).map((h) => h.hold);
const reviews = nodes.filter((n) => n.kind === "review");

// chain: in live mode, the closing review's round (bench.closure): its
// build, accept, gate and review are the candidate's chain, and only they
// are checked against the expect block; a candidate reached by a repair is
// a reviewed candidate (#440 decision, Opus #440 M3).
let chain = null;
if (live) {
  // Review is active, so the release's records qualify every reviewer route, and the task closes a candidate after review.
  if (/^adcd: review stays inactive: /m.test(log)) fail("adcd's start says review stays inactive: the reviewer route is not qualified");
  if (card.column !== "done_candidate") fail(`the task is ${card.column}, not closed as a candidate (done_candidate)`);
  const closure = show.bench?.closure;
  const closing = (bench ?? []).find((n) => n.id === closure?.node && n.kind === "review");
  if (!closure || !closing) fail(`the task read names no closing review (bench.closure: ${JSON.stringify(closure ?? null)})`);
  else {
    if (closure.outcome !== "empty" || closure.reason !== "completed_clean") fail(`the closing review ${closure.node} is ${closure.outcome}/${closure.reason}, not a go (empty/completed_clean)`);
    chain = closing.round;
    // The rounds before it: each a build, closed by its review's repair verdict (no_go, or the unreviewable candidate).
    for (let r = 1; r <= chain; r++) if (!bench.some((n) => n.kind === "build" && n.round === r)) fail(`no build of round ${r}`);
    for (let r = 1; r < chain; r++) {
      if (!bench.some((n) => n.kind === "review" && n.round === r && (n.outcome === "found" || (n.outcome === "failed" && n.reason === "unsafe_output")))) {
        fail(`round ${r} before the closing round ${chain} was not closed by a repair verdict (its review no_go, or unreviewable)`);
      }
    }
  }
  // Every reviewer call's body was captured (Opus #440 L3): the canary's absence from reviewer requests rests on it.
  const reviewerCalls = (show.bench?.leases ?? []).filter((l) => l.role === "reviewer").flatMap((l) => (l.calls ?? []).map((x) => x.id));
  const kept = new Set(events.filter((x) => x.kind === "blob.capture_kept/v1").map((x) => x.payload?.call));
  if (reviewerCalls.length === 0) fail("the task read's bench shows no reviewer call");
  for (const id of reviewerCalls) if (!kept.has(id)) fail(`reviewer call ${id} has no captured body (blob.capture_kept)`);
} else {
  // The review stays inactive in stub mode, and adcd's start says why.
  if (!/^adcd: review stays inactive: P31's qualification on record: /m.test(log)) fail("adcd's start does not name why review is inactive");
  for (const r of reviews) if (!holdsOf(r).includes("review_inactive")) fail(`review ${r.id} is ${r.state}, not held review_inactive`);
  if (card.column === "done" || card.column === "done_candidate") fail(`the task closed (${card.column}): stub mode never reviews`);
}
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
  const all = (bench ?? []).filter((n) => n.kind === kind && (chain === null || n.round === chain));
  if (r.selected === false) {
    if (all.length === 0) fail(`no ${kind} node`);
    for (const n of all) {
      if (n.outcome !== null) fail(`${kind} ${n.id} (round ${n.round}) has a selected result, ${n.outcome}/${n.reason}`);
      // Every result recorded on the node, selected or not (#436; Opus #436 answer 5): an absent list is never "none".
      if (!Array.isArray(n.results) || n.results.length === 0) {
        fail(`${kind} ${n.id} shows no recorded results`);
        continue;
      }
      const attempts = (n.attempts ?? []).map((a) => a.id);
      for (const x of n.results) {
        if (x.outcome !== r.outcome || x.reason !== r.reason) fail(`${kind} ${n.id}'s result ${x.id} is ${x.outcome}/${x.reason}, not ${r.outcome}/${r.reason}`);
        if (x.selected !== false) fail(`${kind} ${n.id}'s result ${x.id} is selected (${x.selected})`);
        if (r.attempt === "none" && x.attempt !== null) fail(`${kind} ${n.id}'s result ${x.id} names attempt ${x.attempt}, where the controller records it with none`);
        if (r.attempt === "own" && !attempts.includes(x.attempt)) fail(`${kind} ${n.id}'s result ${x.id} names attempt ${x.attempt}, not one of the node's (${attempts.join(", ")})`);
      }
      if (r.attempt === "own") {
        const per = n.results.map((x) => x.attempt).filter((a) => attempts.includes(a));
        if (new Set(per).size !== per.length || per.length !== attempts.length) fail(`${kind} ${n.id} records ${n.results.length} results for its ${attempts.length} attempts, not one per attempt`);
      }
    }
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
  if (holdsCanary(body)) fail(`the run's minted canary in a ${roleOf(x)} request (${x.request_id})`);
}
// Nor either saved operator read (Opus #435 H1), in any of its forms; in live mode, out/canary is the acceptance's canary.
for (const f of ["show.json", "events.jsonl"]) if (holdsCanary(read(f))) fail(`the run's minted canary in the operator's ${f}`);
// An attempt that started made at least one builder request the stub logged (Opus #435 M3).
if (!live && events.some((x) => x.kind === "attempt.started/v1") && !requests.some((x) => roleOf(x) === "builder")) fail("an attempt started, and the stub logged no builder request");
for (const s of e.builder_sees ?? []) {
  if (!requests.some((x) => roleOf(x) === "builder" && JSON.stringify(x.body).includes(JSON.stringify(s).slice(1, -1)))) fail(`no builder request carries ${s}`);
}

// Tokens: what the stub served, and the task read's first attempt.
const served = stub.filter((x) => x.kind === "usage" && x.usage).reduce((t, x) => ({ in: t.in + x.usage.prompt_tokens, out: t.out + x.usage.completion_tokens }), { in: 0, out: 0 });
if (e.usage && (served.in !== e.usage.in || served.out !== e.usage.out)) fail(`the stub served ${served.in}/${served.out} tokens, not ${e.usage.in}/${e.usage.out}`);
const first = show.first_attempt;
if (live || requests.length > 0) {
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
const ended = live ? "closed a candidate after review" : e.until === "candidate" ? "gate completed, review held review_inactive" : `held: ${want.join(", ")}`;
const reviewers = requests.filter((x) => roleOf(x) === "reviewer").length;
const tokens = live ? `${first?.tokens?.in ?? "?"}/${first?.tokens?.out ?? "?"} (first attempt)` : `${served.in}/${served.out}`;
console.log(`| ${name} | ${ended} | ${results} | ${tokens} | ${requests.length - reviewers}/${reviewers} |`);
