// SPDX-License-Identifier: Apache-2.0
// The kit proves its own red (09): against conforming adapters it finds
// nothing, and each broken mode makes it red with the finding for the rule
// broken, while the kit itself never crashes, hangs or floods.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { cancelDuringTool, oversize, roundTrip, StallWatch, toolRoundTrip, type AdapterUnderTest, type Finding } from "../src/kit.ts";

const fakeMain = fileURLToPath(new URL("../../fake/src/main.ts", import.meta.url));
const rawMain = fileURLToPath(new URL("./raw-adapter.cjs", import.meta.url));
const fake = (scenario: string, env: Record<string, string> = {}): AdapterUnderTest => ({ command: [process.execPath, fakeMain], env: { ADC_FAKE_SCENARIO: scenario, ...env } });
const raw = (mode: string): AdapterUnderTest => ({ command: [process.execPath, rawMain], env: { RAW_MODE: mode } });
const checks = (fs: Finding[]) => [...new Set(fs.map((f) => f.check))].sort();

test("the faithful fake conforms on every round-trip scenario", async () => {
  for (const s of ["patch", "text", "no-change", "sub-calls", "compaction", "no-usage", "rate-limited", "credit-exhausted", "http-4xx", "http-5xx", "idle-cut", "degenerate", "other-error", "two-errors"]) {
    assert.deepEqual(await roundTrip(fake(s)), [], s);
  }
});

test("heartbeats keep a long gap live", async () => {
  assert.deepEqual(await roundTrip(fake("stream-gap", { ADC_FAKE_GAP_MS: "11000" })), []);
});

test("conforming raw adapters pass: plain, call-level usage, and heavy stderr logging", async () => {
  for (const m of ["clean", "null-sub-call", "stderr-flood"]) assert.deepEqual(await roundTrip(raw(m)), [], m);
});

test("each broken rule is red with its finding", async () => {
  for (const [adapter, want] of [
    [fake("bad-sequence"), ["usage-sequence"]],
    [fake("unannounced-sub-call"), ["sub-call"]],
    [fake("broken-event"), ["run"]],
    [raw("bad-sequence"), ["usage-sequence"]],
    [raw("unannounced"), ["sub-call"]],
    [raw("bad-event"), ["event"]],
    [raw("bad-jsonrpc"), ["envelope"]],
    [raw("unasked-reply"), ["correlation"]],
    [raw("dup-result"), ["correlation"]],
    [raw("bad-shutdown"), ["shutdown"]],
    [raw("wrong-major"), ["version"]],
    [raw("event-after-run"), ["run-boundary"]],
    [raw("dup-turn-start"), ["turn-order"]],
    [raw("dup-turn-end"), ["turn-order"]],
    [raw("delta-after-end"), ["turn-order"]],
    [raw("two-coverage"), ["coverage"]],
    [raw("compaction-complete"), ["coverage"]],
    [raw("harness-tool-complete"), ["coverage"]],
    [raw("exit-nonzero"), ["exit"]],
  ] as [AdapterUnderTest, string[]][]) {
    const fs = await roundTrip(adapter);
    assert.deepEqual(checks(fs), want, `${JSON.stringify(adapter.env)}: ${JSON.stringify(fs)}`);
  }
  // The duplicate is named as such, not only as an unasked reply.
  assert.ok((await roundTrip(raw("dup-result"))).some((f) => /a second reply for request/.test(f.detail)));
});

test("silence between heartbeats over the interval is red", async () => {
  const fs = await roundTrip(raw("no-heartbeat"), { heartbeatMs: 1_000, toleranceMs: 200 });
  assert.deepEqual(checks(fs), ["heartbeat"], JSON.stringify(fs));
});

/** alive reports whether a sleep process with the marker argument runs
 *  (matching only sleep processes, never a command line quoting the marker). */
const alive = (marker: string) => {
  const pids = spawnSync("pgrep", ["-x", "sleep"], { encoding: "utf8" }).stdout.split("\n").filter(Boolean);
  return pids.some((pid) => {
    try {
      return readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0").includes(marker);
    } catch {
      return false;
    }
  });
};

test("an adapter cannot crash, hang or flood the kit", async () => {
  const quick = { deadlineMs: 3_000 };
  // No reply to prepare: the deadline settles the session.
  assert.ok(checks(await roundTrip(raw("no-prepare"), quick)).includes("deadline"));
  // A closed stdin (writes get EPIPE): no crash; the missing replies are findings.
  assert.ok(checks(await roundTrip(raw("closes-stdin"), quick)).includes("describe"));
  // Descendants holding stdout after the adapter exits, in its process group
  // or in a session of their own: the PID namespace ends them all, promptly.
  for (const mode of ["descendant", "detached-descendant"]) {
    const marker = `${60 + Math.floor(Math.random() * 1000) / 1000}`;
    const started = Date.now();
    const fs = await roundTrip({ command: [process.execPath, rawMain], env: { RAW_MODE: mode, RAW_MARKER: marker } }, quick);
    assert.ok(checks(fs).includes("describe"), `${mode}: ${JSON.stringify(fs)}`);
    assert.ok(Date.now() - started < 10_000, `${mode}: settled`);
    await new Promise((r) => setTimeout(r, 200));
    assert.equal(alive(marker), false, `${mode}: no descendant survives`);
  }
  // An adapter that answers, closes stdout and lingers is ended within the deadline.
  const lingering = Date.now();
  assert.ok(checks(await roundTrip(raw("stays-alive"), { deadlineMs: 2_000 })).includes("deadline"));
  assert.ok(Date.now() - lingering < 8_000, "a lingering adapter is ended");
  // 16 MiB + 1 byte without a newline: a framing finding, the adapter ended.
  const framing = Date.now();
  assert.ok(checks(await roundTrip(raw("framing-flood"), quick)).includes("framing"));
  assert.ok(Date.now() - framing < 2_000, "a framing failure ends the session at once, not at the deadline");
  // A flood is cut at the output bound.
  assert.ok((await roundTrip(raw("flood"), { maxOutputBytes: 1 << 20 })).some((f) => f.check === "output-bound" && /bytes of output/.test(f.detail)));
  // A command that cannot run is a finding, not an exception.
  assert.ok(checks(await roundTrip({ command: ["/nonexistent/adapter"] }, quick)).includes("spawn"));
});

test("the findings a case returns never change afterwards", async () => {
  const fs = await roundTrip(raw("clean"), { deadlineMs: 2_000 });
  const copy = JSON.stringify(fs);
  await new Promise((r) => setTimeout(r, 2_500));
  assert.equal(JSON.stringify(fs), copy);
});

test("missing params, a signal exit, and coverage none where partial is due are red; an unanswered run's silence is checked", async () => {
  for (const [mode, want] of [
    ["no-params", "envelope"],
    ["no-params-outside", "envelope"],
    ["signal-exit", "exit"],
    ["compaction-none", "coverage"],
    ["harness-tool-none", "coverage"],
  ]) {
    const fs = await roundTrip(raw(mode));
    assert.ok(checks(fs).includes(want), `${mode}: ${JSON.stringify(fs)}`);
  }
  const silent = await roundTrip(raw("unanswered-run"), { deadlineMs: 3_000, heartbeatMs: 1_000, toleranceMs: 200 });
  assert.ok(checks(silent).includes("heartbeat"), JSON.stringify(silent));
});

test("a stall in the kit itself makes heartbeat gaps inconclusive, not the adapter's fault", async () => {
  // Block the kit's event loop for 400 ms while the adapter beats every 30 ms.
  setTimeout(() => {
    const until = Date.now() + 400;
    while (Date.now() < until);
  }, 300);
  const fs = await roundTrip(raw("fast-heartbeats"), { heartbeatMs: 100, toleranceMs: 50 });
  assert.deepEqual(checks(fs), ["inconclusive"], JSON.stringify(fs));
});

test("a short stall in the kit does not excuse a longer silence of the adapter", async () => {
  // The adapter is silent for seconds; the kit stalls 150 ms of it.
  setTimeout(() => {
    const until = Date.now() + 150;
    while (Date.now() < until);
  }, 300);
  const fs = await roundTrip(raw("no-heartbeat"), { heartbeatMs: 100, toleranceMs: 50 });
  assert.deepEqual(checks(fs), ["heartbeat"], JSON.stringify(fs));
});

test("a stall still pending when the watch closes is kept", () => {
  // The run's reply can be handled before the overdue tick: closing samples it.
  const w = new StallWatch();
  w.start();
  const until = performance.now() + 100;
  while (performance.now() < until);
  w.stop();
  assert.equal(w.stalls.length, 1, JSON.stringify(w.stalls));
  assert.ok(w.stalls[0][1] - w.stalls[0][0] >= 80, JSON.stringify(w.stalls));
});

test("output after the last reply is still checked", async () => {
  for (let i = 0; i < 10; i++) {
    const fs = await roundTrip(raw("trailing-junk"));
    assert.deepEqual(checks(fs), ["framing"], `run ${i}: ${JSON.stringify(fs)}`);
  }
});

test("the mediated tool round-trip: a faithful exchange conforms", async () => {
  assert.deepEqual(await toolRoundTrip(fake("tool"), "RunTest"), []);
  // The raw adapter echoes the delivered handle with its keys in another order.
  assert.deepEqual(await toolRoundTrip(raw("tool-clean"), "RunTest"), []);
});

test("each broken tool exchange is red with its finding", async () => {
  for (const [mode, want] of [
    ["tool-wrong-tool", ["tool"]],
    ["tool-wrong-turn", ["tool"]],
    ["tool-wrong-digest", ["tool"]],
    ["tool-early", ["tool"]],
    ["tool-twice", ["tool"]],
    ["tool-unreported", ["tool"]],
    ["tool-bad-ack", ["tool"]],
    ["tool-no-ack", ["tool"]],
    ["tool-stops", ["tool"]],
  ] as [string, string[]][]) {
    const fs = await toolRoundTrip(raw(mode), "RunTest");
    assert.deepEqual(checks(fs), want, `${mode}: ${JSON.stringify(fs)}`);
  }
  assert.ok((await toolRoundTrip(raw("tool-dup-request"), "RunTest")).some((f) => /a second tool.request/.test(f.detail)));
  // A completed run with an unreported delivery, a missing acknowledgement, and a
  // request with no completed exchange, are each named.
  assert.ok((await toolRoundTrip(raw("tool-unreported"), "RunTest")).some((f) => /was never reported/.test(f.detail)));
  assert.ok((await toolRoundTrip(raw("tool-no-ack"), "RunTest")).some((f) => /no acknowledgement of the tool.response/.test(f.detail)));
  assert.ok((await toolRoundTrip(raw("tool-stops"), "RunTest")).some((f) => /no tool exchange completed/.test(f.detail)));
  // An exchange still pending when the kit ends the session settles into a finding.
  assert.ok((await toolRoundTrip(raw("tool-no-ack-stays"), "RunTest", { deadlineMs: 2_000 })).some((f) => /no acknowledgement of the tool.response/.test(f.detail)));
  // A request outside the policy, or naming another attempt, ends the case.
  assert.ok(checks(await toolRoundTrip(raw("tool-unmediated"), "RunTest", { deadlineMs: 3_000 })).includes("tool-policy"));
  assert.ok((await toolRoundTrip(raw("tool-foreign"), "RunTest", { deadlineMs: 3_000 })).some((f) => /handle of another attempt/.test(f.detail)));
  assert.ok(checks(await roundTrip(raw("tool-clean"))).includes("tool-policy"), "the plain case mediates nothing");
  // A run that never requests the tool has not exercised the case.
  assert.deepEqual(checks(await toolRoundTrip(raw("clean"), "RunTest")), ["tool"]);
});

test("cancel during a tool call: best effort, but exercised and valid", async () => {
  // Stopping, and completing despite the cancel, both conform (09: best effort).
  assert.deepEqual(await cancelDuringTool(fake("tool"), "RunTest"), []);
  assert.deepEqual(await cancelDuringTool(raw("cancel-stops"), "RunTest"), []);
  assert.deepEqual(await cancelDuringTool(raw("cancel-completes"), "RunTest"), []);
  for (const [mode, want] of [
    ["cancel-bad-reply", ["cancel"]],
    ["cancel-forged", ["run"]],
    ["cancel-claims", ["tool"]],
  ] as [string, string[]][]) {
    const fs = await cancelDuringTool(raw(mode), "RunTest");
    assert.deepEqual(checks(fs), want, `${mode}: ${JSON.stringify(fs)}`);
  }
  assert.deepEqual(checks(await cancelDuringTool(fake("patch"), "RunTest")), ["tool"], "no tool request: not exercised");
});

test("a harness-internal tool outside the policy is red in every case", async () => {
  const shell = (mode: string, tool = "Shell"): AdapterUnderTest => ({ command: [process.execPath, rawMain], env: { RAW_MODE: mode, RAW_HARNESS_TOOL: tool } });
  assert.deepEqual(checks(await roundTrip(shell("clean"))), ["tool-policy"]);
  assert.deepEqual(checks(await toolRoundTrip(shell("tool-clean"), "RunTest")), ["tool-policy"]);
  assert.deepEqual(checks(await cancelDuringTool(shell("cancel-stops"), "RunTest")), ["tool-policy"]);
  // A mediated tool is never also the harness's own.
  assert.deepEqual(checks(await toolRoundTrip(shell("tool-clean", "RunTest"), "RunTest")), ["tool-policy"]);
  // Mediating the kit's harness tool keeps the policy valid: the SDK validates prepare.
  const edit = await toolRoundTrip(fake("patch"), "Edit");
  assert.ok(!checks(edit).includes("prepare") && checks(edit).includes("tool-policy"), JSON.stringify(edit));
});

test("oversize: served at the bound, rejected one byte over", async () => {
  // The SDK's fake ends on an oversize line; a raw adapter may instead answer it with an error.
  for (const adapter of [fake("patch"), raw("size-exits"), raw("size-errors")]) {
    assert.deepEqual(await oversize(adapter), [], JSON.stringify(adapter.env));
  }
  // Size-blind (serves it), ignoring it, and rejecting at the bound itself are red.
  assert.deepEqual(checks(await oversize(raw("clean"))), ["oversize"]);
  assert.deepEqual(checks(await oversize(raw("size-ignores"), {}, 1_000)), ["oversize"]);
  // An error value that is not a JSON-RPC error object is no rejection.
  for (const mode of ["size-error-null", "size-error-false", "size-error-empty", "size-error-types"]) {
    assert.deepEqual(checks(await oversize(raw(mode))), ["envelope", "oversize"], mode);
  }
  assert.ok(checks(await oversize(raw("size-off-by-one"))).includes("describe"));
  // Ending its output is not ending: the process must exit.
  assert.deepEqual(checks(await oversize(raw("size-closes"), {}, 1_000)), ["oversize"]);
  // A slow host (#226 entry 102): each line over 1 MiB costs the adapter 2 s, at the
  // bound and past it alike, past a 1 s floor. The grace scales with the at-bound
  // request, so a rejection is still seen, and a malformed one still is none.
  const slow = (mode: string): AdapterUnderTest => ({ ...raw(mode), env: { RAW_MODE: mode, RAW_SLOW_MS: "2000" } });
  assert.deepEqual(await oversize(slow("size-errors"), {}, 1_000), [], "a slow rejection");
  assert.deepEqual(checks(await oversize(slow("size-error-null"), {}, 1_000)), ["envelope", "oversize"], "a slow malformed rejection");
});

test("large invalid events do not exhaust a bounded heap", () => {
  // Four 4 MiB strings under a 96 MiB heap, plain, escaped or alternating,
  // or millions of nested arrays: findings, not an out-of-memory abort.
  const kit = new URL("../src/kit.ts", import.meta.url).href;
  for (const [pad, want] of [["plain", "event"], ["escapes", "event"], ["alternating", "event"], ["dense", "framing"]]) {
    const code = `const k = await import(${JSON.stringify(kit)});
      const fs = await k.roundTrip({ command: [process.execPath, ${JSON.stringify(rawMain)}], env: { RAW_MODE: "big-strings", RAW_PAD: "${pad}" } });
      console.log(JSON.stringify([...new Set(fs.map((f) => f.check))]));`;
    const r = spawnSync(process.execPath, ["--max-old-space-size=96", "--input-type=module", "-e", code], { encoding: "utf8", timeout: 60_000 });
    assert.equal(r.status, 0, `${pad}: ${r.stderr.slice(-2000)}`);
    assert.deepEqual(JSON.parse(r.stdout.trim().split("\n").pop()!), [want], pad);
  }
});
