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
if (mode === "descendant" || mode === "detached-descendant") {
  // Exit at once, answering nothing, while a descendant keeps stdout open;
  // the detached one starts its own session, out of the process group.
  spawn("sleep", [process.env.RAW_MARKER || "60"], { stdio: ["ignore", "inherit", "ignore"], detached: mode === "detached-descendant" }).unref();
  process.exit(0);
}
if (mode === "stays-alive") {
  // Answer everything, close stdout, then linger instead of exiting.
  process.stdin.on("end", () => {});
}

let run = null;
// The pending tool.response: resolved with its params, or with a cancel.
let answered = null;
const toolResponse = () => new Promise((r) => (answered = r));
const handle = (attempt, name, digit) => ({ attempt, handle: name, digest: "sha256:" + digit.repeat(64), size: 1 });
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
  if (mode === "compaction-complete" || mode === "compaction-none") event({ event: "compaction", turn: 1, before_tokens: 9, after_tokens: 3 });
  if (mode === "harness-tool-complete" || mode === "harness-tool-none") event({ event: "tool.result", turn: 1, tool: "Edit", mediated_by_controller: false, delivered: null, bytes: 1 });
  if (mode === "no-params") out({ jsonrpc: "2.0", method: "adapter.event" });
  if (mode.startsWith("tool") || mode.startsWith("cancel")) {
    const tool = mode === "tool-unmediated" ? "Shell" : "RunTest";
    const own = run.params.attempt;
    // A guess at the kit's first delivery, reported before anything was supplied.
    if (mode === "tool-early") event({ event: "tool.result", turn: 1, tool, mediated_by_controller: true, delivered: handle(own, "delivered-1", "0"), bytes: 1 });
    event({ event: "tool.request", turn: 1, tool, request: handle(mode === "tool-foreign" ? "att_01m3250vnfeyrv67x6w0ddqr6b" : own, "req-1", "1") });
    if (mode === "tool-dup-request") event({ event: "tool.request", turn: 1, tool, request: handle(own, "req-1", "1") });
    if (mode === "tool-unmediated" || mode === "tool-foreign") return;
    const r = await toolResponse();
    if (r === "stop") return reply(m.id, mode === "cancel-forged" ? { outcome: "empty", result: { kind: "text", handle: handle(own, "r", "2") } }
      : { outcome: "could_not_run", reason: "attempt_stopped", detail: "cancelled during the tool call", result: null, usage_final: null });
    if (r === "claim") event({ event: "tool.result", turn: 1, tool, mediated_by_controller: true, delivered: handle(own, "delivered-1", "0"), bytes: 1 });
    if (r !== "claim" && r !== "complete" && mode !== "tool-unreported") {
      const { size, digest, handle: name, attempt } = r.delivered; // the same handle, its keys in another order
      const turn = mode === "tool-wrong-turn" ? 2 : 1;
      if (turn === 2) event({ event: "turn.start", turn, sub_call: "s1", reported_at: "2026-10-05T00:00:00Z" });
      const reported = { size, digest: mode === "tool-wrong-digest" ? "sha256:" + "3".repeat(64) : digest, handle: name, attempt };
      const report = () => event({ event: "tool.result", turn, tool: mode === "tool-wrong-tool" ? "Other" : tool, mediated_by_controller: true, delivered: reported, bytes: size });
      report();
      if (mode === "tool-twice") report();
    }
  }
  if (mode === "unanswered-run") return; // never answers the run, no heartbeats
  if (mode === "fast-heartbeats") for (let i = 0; i < 40; i++) { event({ event: "heartbeat" }); await sleep(30); }
  const usage = (sub_call, sequence) => event({ event: "usage", sub_call, sequence, tokens: { in: 1, out: 1, cache_read: 0, cache_write: 0 }, turns: 1, milliseconds: 1,
    cost: { amount_micros: 0, currency: "USD", basis: "cash", estimate: true } });
  usage(mode === "null-sub-call" ? null : "s1", 0);
  if (mode === "bad-sequence") usage("s1", 2);
  const none = { "compaction-none": ["compaction"], "harness-tool-none": ["harness_tool"] }[mode];
  event({ event: "coverage", turn: 1, observable: none ? "none" : "complete", unobservable: none || [] });
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
      if (mode === "no-params-outside") out({ jsonrpc: "2.0", method: "adapter.event" });
      if (mode === "bad-jsonrpc") out({ jsonrpc: "WRONG", id: m.id, result: {} });
      if (mode === "unasked-reply") out({ jsonrpc: "2.0", id: 99, result: clean });
      reply(m.id, { harness: "raw", harness_version: "1", adapter_version: "0.1.0", contract_version: mode === "wrong-major" ? "2.0.0" : "1.0.0",
        capabilities: [], session_semantics: "fresh_process", tool_surface: [],
        telemetry: { usage: true, upstream: false, stream_deltas: false, per_turn_coverage: true }, host_side: false, needs_native_login: false });
    }
    if (m.method === "prepare" && mode !== "no-prepare") reply(m.id, clean);
    if (m.method === "run") doRun(m);
    if (m.method === "tool.response") {
      reply(m.id, mode === "tool-bad-ack" ? { ok: true } : {});
      answered?.(m.params);
    }
    if (m.method === "cancel") {
      reply(m.id, mode === "cancel-bad-reply" ? { outcome: "done" } : clean);
      // Best effort: a run may stop, or still complete; a broken one claims a delivery.
      answered?.({ "cancel-completes": "complete", "cancel-claims": "claim" }[mode] || "stop");
    }
    if (mode === "framing-flood" && m.method === "describe") {
      process.stdout.write("x".repeat((16 << 20) + 1));
      setInterval(() => {}, 1000);
    }
    if (m.method === "shutdown") {
      reply(m.id, mode === "bad-shutdown" ? { outcome: "done" } : clean);
      if (mode === "stays-alive") {
        require("node:fs").closeSync(1);
        setInterval(() => {}, 1000);
        return;
      }
      if (mode === "signal-exit") return process.kill(process.pid, "SIGTERM");
      // A broken line after the last reply, then a clean exit.
      if (mode === "trailing-junk") return process.stdout.write("{", () => process.exit(0));
      process.exit(mode === "exit-nonzero" ? 3 : 0);
    }
  }
});
