// SPDX-License-Identifier: Apache-2.0
// A raw adapter, written without the SDK, for the kit's red controls:
// RAW_MODE selects the one rule (or resource) it breaks; "clean" breaks none.
// The SDK itself keeps many of these rules, so only an adapter written
// without it can show the kit catching them.
"use strict";
const { spawn } = require("node:child_process");
const mode = process.env.RAW_MODE || "clean";
const out = (m) => process.stdout.write(JSON.stringify(m) + "\n");
const reply = (id, result) => out({ jsonrpc: "2.0", id, result });
const clean = { outcome: "empty", reason: "completed_clean", detail: "" };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// A synchronous write blocks once the stderr pipe is full, unless the host drains it.
if (mode === "stderr-flood") require("node:fs").writeSync(2, "x".repeat(1 << 20));
if (mode === "closes-stdin") process.stdin.destroy();
if (mode === "descendant") {
  // Exit at once, answering nothing, while a descendant keeps stdout open.
  const child = spawn("sleep", ["60"], { stdio: ["ignore", "inherit", "ignore"] });
  if (process.env.RAW_PIDFILE) require("node:fs").writeFileSync(process.env.RAW_PIDFILE, String(child.pid));
  process.exit(0);
}

let run = null;
const event = (e) => out({ jsonrpc: "2.0", method: "adapter.event", params: { ...e, attempt: run.params.attempt, fencing_token: run.params.fencing_token, call: run.params.call } });

async function doRun(m) {
  run = m;
  if (mode === "flood") {
    for (let i = 0; i < 200000; i++) event({ event: "heartbeat" });
  }
  event({ event: "call.sub", sub_call: "s1", model_requested: "x" });
  const start = (turn) => event({ event: "turn.start", turn, sub_call: mode === "unannounced" ? "s9" : "s1", reported_at: "2026-10-05T00:00:00Z" });
  const end = (turn) => event({ event: "turn.end", turn, sub_call: "s1", reported_at: "2026-10-05T00:00:01Z" });
  start(1);
  if (mode === "dup-turn-start") start(1);
  event({ event: "delta.text", turn: 1, bytes: 4 });
  if (mode === "bad-event") event({ event: "delta.text", turn: 1 });
  if (mode === "no-heartbeat") for (let i = 0; i < 6; i++) { await sleep(500); event({ event: "delta.text", turn: 1, bytes: 1 }); }
  if (mode === "compaction-complete") event({ event: "compaction", turn: 1, before_tokens: 9, after_tokens: 3 });
  if (mode === "harness-tool-complete") event({ event: "tool.result", turn: 1, tool: "Edit", mediated_by_controller: false, delivered: null, bytes: 1 });
  const usage = (sub_call, sequence) => event({ event: "usage", sub_call, sequence, tokens: { in: 1, out: 1, cache_read: 0, cache_write: 0 }, turns: 1, milliseconds: 1,
    cost: { amount_micros: 0, currency: "USD", basis: "cash", estimate: true } });
  usage(mode === "null-sub-call" ? null : "s1", 0);
  if (mode === "bad-sequence") usage("s1", 2);
  event({ event: "coverage", turn: 1, observable: "complete", unobservable: [] });
  if (mode === "two-coverage") event({ event: "coverage", turn: 1, observable: "complete", unobservable: [] });
  end(1);
  if (mode === "dup-turn-end") end(1);
  if (mode === "delta-after-end") event({ event: "delta.text", turn: 1, bytes: 1 });
  const result = { ...clean, result: null, usage_final: null };
  reply(m.id, result);
  if (mode === "dup-result") reply(m.id, result);
  if (mode === "event-after-run") event({ event: "delta.text", turn: 1, bytes: 1 });
}

let buf = "";
process.stdin.on("data", (chunk) => {
  buf += chunk;
  let i;
  while ((i = buf.indexOf("\n")) >= 0) {
    const m = JSON.parse(buf.slice(0, i));
    buf = buf.slice(i + 1);
    if (m.method === "describe") {
      if (mode === "bad-jsonrpc") out({ jsonrpc: "WRONG", id: m.id, result: {} });
      if (mode === "unasked-reply") out({ jsonrpc: "2.0", id: 99, result: clean });
      reply(m.id, { harness: "raw", harness_version: "1", adapter_version: "0.1.0", contract_version: mode === "wrong-major" ? "2.0.0" : "1.0.0",
        capabilities: [], session_semantics: "fresh_process", tool_surface: [],
        telemetry: { usage: true, upstream: false, stream_deltas: false, per_turn_coverage: true }, host_side: false, needs_native_login: false });
    }
    if (m.method === "prepare" && mode !== "no-prepare") reply(m.id, clean);
    if (m.method === "run") doRun(m);
    if (m.method === "shutdown") {
      reply(m.id, mode === "bad-shutdown" ? { outcome: "done" } : clean);
      process.exit(mode === "exit-nonzero" ? 3 : 0);
    }
  }
});
