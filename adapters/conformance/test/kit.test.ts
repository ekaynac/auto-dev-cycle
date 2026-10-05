// SPDX-License-Identifier: Apache-2.0
// The kit proves its own red (09): against conforming adapters it finds
// nothing, and each broken mode makes it red with the finding for the rule
// broken, while the kit itself never crashes, hangs or floods.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
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

test("an adapter cannot crash, hang or flood the kit", async () => {
  const quick = { deadlineMs: 3_000 };
  // No reply to prepare: the deadline settles the session.
  assert.ok(checks(await roundTrip(raw("no-prepare"), quick)).includes("deadline"));
  // A closed stdin (writes get EPIPE): no crash; the missing replies are findings.
  assert.ok(checks(await roundTrip(raw("closes-stdin"), quick)).includes("describe"));
  // A descendant holding stdout open after the adapter exits: the process
  // group is killed and the session ends at the deadline, not at end of output.
  const pidfile = join(mkdtempSync(join(tmpdir(), "kit-")), "pid");
  const started = Date.now();
  assert.ok(checks(await roundTrip({ ...raw("descendant"), env: { RAW_MODE: "descendant", RAW_PIDFILE: pidfile } }, quick)).includes("deadline"));
  assert.ok(Date.now() - started < 10_000, "settled at the deadline");
  const pid = Number(readFileSync(pidfile, "utf8"));
  await new Promise((r) => setTimeout(r, 100));
  assert.throws(() => process.kill(pid, 0), "the descendant was killed with the process group");
  // A flood is cut at the output bound.
  assert.ok((await roundTrip(raw("flood"), { maxOutputBytes: 1 << 20 })).some((f) => f.check === "output-bound" && /bytes of output/.test(f.detail)));
  // A command that cannot run is a finding, not an exception.
  assert.ok(checks(await roundTrip({ command: ["/nonexistent/adapter"] }, quick)).includes("spawn"));
});
