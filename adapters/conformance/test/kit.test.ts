// SPDX-License-Identifier: Apache-2.0
// The kit proves its own red (09): against the contract-faithful fake it
// finds nothing, and each of the fake's broken modes makes it red with the
// finding for the rule broken.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { cancel, mediatedTool, oversize, roundTrip, type AdapterUnderTest, type Finding } from "../src/kit.ts";

const fakeMain = fileURLToPath(new URL("../../fake/src/main.ts", import.meta.url));
const fake = (scenario: string, env: Record<string, string> = {}): AdapterUnderTest => ({
  command: [process.execPath, fakeMain],
  env: { ADC_FAKE_SCENARIO: scenario, ...env },
});
const checks = (fs: Finding[]) => [...new Set(fs.map((f) => f.check))].sort();

test("the faithful fake conforms on every round-trip scenario", async () => {
  for (const s of ["patch", "text", "no-change", "sub-calls", "compaction", "no-usage", "rate-limited", "credit-exhausted", "http-4xx", "http-5xx", "idle-cut", "degenerate", "other-error", "two-errors"]) {
    assert.deepEqual(await roundTrip(fake(s)), [], s);
  }
});

test("heartbeats keep a long gap live", async () => {
  assert.deepEqual(await roundTrip(fake("stream-gap", { ADC_FAKE_GAP_MS: "11000" })), []);
});

test("the mediated tool round-trip, and cancel during a stream and during a tool call", async () => {
  assert.deepEqual(await mediatedTool(fake("tool"), "RunTest"), []);
  assert.deepEqual(await cancel(fake("hang")), []);
  assert.deepEqual(await cancel(fake("tool")), [], "cancel during the tool call");
});

test("an oversize message is refused, never processed", async () => {
  assert.deepEqual(await oversize(fake("patch")), []);
});

test("the kit is red on each broken mode, for the rule it breaks", async () => {
  assert.deepEqual(checks(await roundTrip(fake("bad-sequence"))), ["usage-sequence"]);
  assert.deepEqual(checks(await roundTrip(fake("unannounced-sub-call"))), ["sub-call"]);
  const broken = await roundTrip(fake("broken-event"));
  assert.deepEqual(checks(broken), ["run"]);
  assert.match(broken[0].detail, /the run gave no result/);
  // A tool the policy does not mediate is not requested: the tool case fails.
  assert.ok(checks(await mediatedTool(fake("tool"), "Other")).includes("tool"));
  // An adapter that ignores cancel: the patch scenario completes found.
  assert.deepEqual(checks(await cancel(fake("patch"))), ["cancel"]);
});

// Raw adapters, without the SDK, for rules the SDK itself keeps (so no
// SDK-based adapter can break them): the kit must still catch an adapter
// written without it. mode selects the broken rule.
const raw = (mode: string) => `
const mode = ${JSON.stringify(mode)};
const rl = require("node:readline").createInterface({ input: process.stdin });
const out = (m) => process.stdout.write(JSON.stringify(m) + "\\n");
const clean = { outcome: "empty", reason: "completed_clean", detail: "" };
let run = null;
const event = (e) => out({ jsonrpc: "2.0", method: "adapter.event", params: { ...e, attempt: run.params.attempt, fencing_token: run.params.fencing_token, call: run.params.call } });
const handle = (name, d) => ({ attempt: run.params.attempt, handle: name, digest: "sha256:" + d.repeat(64), size: 1 });
const finish = () => out({ jsonrpc: "2.0", id: run.id, result: { ...clean, result: null, usage_final: null } });
rl.on("line", (l) => {
  const m = JSON.parse(l);
  if (m.method === "describe") out({ jsonrpc: "2.0", id: m.id, result: { harness: "raw", harness_version: "1", adapter_version: "0.1.0",
    contract_version: "1.0.0", capabilities: [], session_semantics: "fresh_process", tool_surface: ["RunTest"],
    telemetry: { usage: false, upstream: false, stream_deltas: false, per_turn_coverage: false }, host_side: false, needs_native_login: false } });
  if (m.method === "prepare") out({ jsonrpc: "2.0", id: m.id, result: clean });
  if (m.method === "run") {
    run = m;
    if (mode === "silent") return setTimeout(finish, 12000);
    event({ event: "call.sub", sub_call: "s1", model_requested: "x" });
    event({ event: "turn.start", turn: 1, sub_call: "s1", reported_at: "2026-10-05T00:00:00Z" });
    if (mode === "bad-event") event({ event: "delta.text", turn: 1 });
    if (mode === "forged-tool") return event({ event: "tool.request", turn: 1, tool: "RunTest", request: handle("req.json", "1") });
    if (mode !== "no-coverage") event({ event: "coverage", turn: 1, observable: "complete", unobservable: [] });
    event({ event: "turn.end", turn: 1, sub_call: "s1", reported_at: "2026-10-05T00:00:01Z" });
    finish();
  }
  if (m.method === "tool.response") {
    out({ jsonrpc: "2.0", id: m.id, result: {} });
    event({ event: "tool.result", turn: 1, tool: "RunTest", mediated_by_controller: true, delivered: handle("forged.txt", "f"), bytes: 1 });
    event({ event: "coverage", turn: 1, observable: "complete", unobservable: [] });
    event({ event: "turn.end", turn: 1, sub_call: "s1", reported_at: "2026-10-05T00:00:01Z" });
    finish();
  }
  if (m.method === "shutdown") { out({ jsonrpc: "2.0", id: m.id, result: clean }); process.exit(0); }
});`;
const rawAdapter = (mode: string): AdapterUnderTest => ({ command: [process.execPath, "-e", raw(mode)] });

test("raw adapters conform when they keep the rules, and are red for each rule they break", async () => {
  assert.deepEqual(await roundTrip(rawAdapter("clean")), []);
  assert.deepEqual(checks(await roundTrip(rawAdapter("bad-event"))), ["event"]);
  assert.deepEqual(checks(await roundTrip(rawAdapter("no-coverage"))), ["coverage"]);
  assert.deepEqual(checks(await mediatedTool(rawAdapter("forged-tool"), "RunTest")), ["tool"]);
  // The SDK never stays silent (it beats at most every 9 s): silence is a raw adapter's.
  assert.deepEqual(checks(await roundTrip(rawAdapter("silent"))), ["liveness"]);
});
