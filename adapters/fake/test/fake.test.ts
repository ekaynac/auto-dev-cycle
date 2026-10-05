// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { eventMethod, InternalError, serve } from "@adc/contract";
import { FakeAdapter, handle, scenarios, type Scenario } from "../src/fake.ts";

const att = "att_01m3250vnfeyrv67x6w0ddqr6a";
const cal = "cal_01m3250v9xft08ssr4ygq6g2np";
const prepareParams = { attempt: att, fencing_token: 3, role: "builder", executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", model: "m",
  workspace: { path: "/workspace", writable: true }, inputs: [], limits: { max_turns: 4, max_output_tokens: 1000, wall_seconds: 60 },
  tool_policy: { allowed: [], mediated: ["RunTest"] }, gateway: null, native_login: null };
const runParams = { attempt: att, fencing_token: 3, call: cal, prompt: handle(att, "p.md", "do it"), response_schema: null };
const req = (id: number, method: string, params?: unknown) => ({ jsonrpc: "2.0", id, method, params });

type Msg = { id?: unknown; result?: any; error?: { code: number; message: string }; method?: string; params?: any };

/** session drives serve with requests the test may add as messages arrive;
 *  it ends at shutdown. */
async function session(adapter: FakeAdapter, react: (m: Msg, send: (r: unknown) => void) => void = () => {}): Promise<Msg[]> {
  const queue: string[] = [];
  let wake: (() => void) | null = null;
  const send = (r: unknown) => {
    queue.push(JSON.stringify(r) + "\n");
    wake?.();
    wake = null;
  };
  const input = (async function* () {
    for (;;) {
      while (queue.length) yield Buffer.from(queue.shift()!);
      await new Promise<void>((r) => (wake = r));
    }
  })();
  const out: Msg[] = [];
  send(req(1, "describe"));
  send(req(2, "prepare", prepareParams));
  send(req(3, "run", runParams));
  await serve(adapter, {
    input,
    heartbeatMs: 50,
    write: (l) => {
      const m = JSON.parse(l) as Msg;
      out.push(m);
      react(m, send);
      if (m.id === 3) send(req(9, "shutdown"));
    },
  });
  return out;
}

const events = (msgs: Msg[]) => msgs.filter((m) => m.method === eventMethod).map((m) => m.params);
const runResult = (msgs: Msg[]) => msgs.find((m) => m.id === 3)!;
const named = (msgs: Msg[], name: string) => events(msgs).filter((e) => e.event === name);

test("every scenario but the broken one completes a valid session", async () => {
  for (const s of scenarios.filter((s) => s !== "broken-event" && s !== "hang" && s !== "tool")) {
    const msgs = await session(new FakeAdapter({ scenario: s, gapMs: 120 }));
    const r = runResult(msgs);
    assert.ok(r.result, `${s}: ${JSON.stringify(r.error)}`);
    assert.ok(events(msgs).every((e) => e.attempt === att && e.fencing_token === 3 && e.call === cal), `${s}: envelope`);
  }
});

test("patch: two turns, a cumulative patch, usage numbered per call and summed in the final", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "patch" }));
  const r = runResult(msgs).result;
  assert.equal(r.outcome, "found");
  assert.equal(r.result.kind, "patch");
  const usage = named(msgs, "usage");
  assert.deepEqual(usage.map((u) => u.sequence), [0, 1]);
  assert.equal(r.usage_final.tokens.in, usage.reduce((n, u) => n + u.tokens.in, 0));
  assert.equal(r.usage_final.turns, 2);
  assert.deepEqual(named(msgs, "turn.end").map((e) => e.turn), [1, 2]);
  // Cumulative: the patch handle covers both turns' changes.
  const both = "--- a/file1.go\n+++ b/file1.go\n@@ -1 +1 @@\n-old\n+new\n--- a/file2.go\n+++ b/file2.go\n@@ -1 +1 @@\n-old\n+new\n";
  assert.deepEqual(r.result.handle, { attempt: att, handle: "patch.diff", digest: "sha256:" + createHash("sha256").update(both).digest("hex"), size: both.length });
});

test("sub-calls: each sub-call is announced, usage numbers continue across them", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "sub-calls" }));
  assert.deepEqual(named(msgs, "call.sub").map((e) => e.sub_call), ["s1", "s2"]);
  assert.deepEqual(named(msgs, "usage").map((u) => [u.sub_call, u.sequence]), [["s1", 0], ["s2", 1]]);
});

test("no-usage: telemetry says so, no usage events, a null final", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "no-usage" }));
  assert.equal(msgs.find((m) => m.id === 1)!.result.telemetry.usage, false);
  assert.equal(named(msgs, "usage").length, 0);
  assert.equal(runResult(msgs).result.usage_final, null);
});

test("stream-gap: heartbeats cover the pause between deltas", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "stream-gap", gapMs: 300 }));
  const all = events(msgs);
  const first = all.findIndex((e) => e.event === "delta.text");
  const next = all.findIndex((e, i) => i > first && e.event.startsWith("delta."));
  assert.ok(all.slice(first, next).filter((e) => e.event === "heartbeat").length >= 2, "heartbeats in the gap");
});

test("provider errors: an error event, could_not_run with the exit map's reason, no result, the usage so far", async () => {
  for (const [s, hint, reason] of [
    ["rate-limited", "rate_limited", "quota_exhausted"],
    ["credit-exhausted", "credit_exhausted", "broker_credit_exhausted"],
    ["http-4xx", "http_4xx", "provider_request_refused"],
    ["http-5xx", "http_5xx", "provider_unavailable"],
    ["idle-cut", "idle_cut", "stream_idle_cut"],
    ["degenerate", "degenerate", "degenerate_output"],
  ] as [Scenario, string, string][]) {
    const msgs = await session(new FakeAdapter({ scenario: s }));
    const r = runResult(msgs).result;
    assert.deepEqual([r.outcome, r.reason, r.result], ["could_not_run", reason, null], s);
    assert.deepEqual(named(msgs, "error").map((e) => e.class_hint), [hint], s);
    assert.equal(r.usage_final.tokens.in, 1000, `${s}: the partial work's usage`);
  }
});

test("tool: a controller-mediated round-trip reports the delivered handle", async () => {
  const delivered = handle(att, "out.txt", "FAIL TestX");
  const msgs = await session(new FakeAdapter({ scenario: "tool" }), (m, send) => {
    if (m.method === eventMethod && m.params.event === "tool.request") {
      send(req(4, "tool.response", { attempt: att, fencing_token: 3, call: cal, turn: m.params.turn, tool: m.params.tool, request: m.params.request,
        outcome: "found", reason: "gate_test_failed", detail: "TestX failed", delivered }));
    }
  });
  assert.deepEqual(msgs.find((m) => m.id === 4)!.result, {});
  const [result] = named(msgs, "tool.result");
  assert.deepEqual([result.mediated_by_controller, result.delivered], [true, delivered]);
  assert.equal(runResult(msgs).result.outcome, "found");
});

test("hang: streams until cancelled, then could_not_run/attempt_stopped", async () => {
  let cancelled = false;
  const msgs = await session(new FakeAdapter({ scenario: "hang" }), (m, send) => {
    if (!cancelled && m.method === eventMethod && m.params.event === "delta.thinking") {
      cancelled = true;
      send(req(5, "cancel", { attempt: att, fencing_token: 3 }));
    }
  });
  assert.deepEqual(msgs.find((m) => m.id === 5)!.result.outcome, "empty");
  const r = runResult(msgs);
  // The run ends after the abort: either its own stopped result, or the
  // SDK's report that the run was cut short; never a success.
  assert.ok(r.result ? r.result.reason === "attempt_stopped" && r.result.outcome === "could_not_run" : r.error, JSON.stringify(r));
});

test("broken-event: the SDK refuses the off-contract event and fails the run", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "broken-event" }));
  assert.equal(runResult(msgs).error?.code, InternalError);
  assert.match(runResult(msgs).error!.message, /an event does not match adapter\/event/);
});

test("an unknown scenario is refused", () => {
  assert.throws(() => new FakeAdapter({ scenario: "nope" as Scenario }), /unknown fake scenario/);
});
