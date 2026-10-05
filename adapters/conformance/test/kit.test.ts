// SPDX-License-Identifier: Apache-2.0
// The kit proves its own red (09): against conforming adapters it finds
// nothing, and each broken mode makes it red with the finding for the rule
// broken, while the kit itself never crashes, hangs or floods.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { roundTrip, type AdapterUnderTest, type Finding } from "../src/kit.ts";

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

test("output after the last reply is still checked", async () => {
  for (let i = 0; i < 10; i++) {
    const fs = await roundTrip(raw("trailing-junk"));
    assert.deepEqual(checks(fs), ["framing"], `run ${i}: ${JSON.stringify(fs)}`);
  }
});
