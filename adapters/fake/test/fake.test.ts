// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import { eventMethod, InternalError, serve } from "@adc/contract";
import { FakeAdapter, handle, scenarios, type Scenario } from "../src/fake.ts";

const att = "att_01m3250vnfeyrv67x6w0ddqr6a";
const cal = "cal_01m3250v9xft08ssr4ygq6g2np";
const prepare = (mediated: string[] = ["RunTest"]) => ({ attempt: att, fencing_token: 3, role: "builder", executor: "exe_01m3250vnfeyrv67x6w0ddqr6a",
  model: "m", workspace: { path: "/workspace", writable: true }, inputs: [], limits: { max_turns: 4, max_output_tokens: 1000, wall_seconds: 60 },
  tool_policy: { allowed: [], mediated }, gateway: null, native_login: null });
const runParams = { attempt: att, fencing_token: 3, call: cal, prompt: handle(att, "p.md", "do it"), response_schema: null };
const req = (id: number, method: string, params?: unknown) => ({ jsonrpc: "2.0", id, method, params });

type Msg = { id?: unknown; result?: any; error?: { code: number; message: string }; method?: string; params?: any };

/** session drives serve; react may send more requests as messages arrive.
 *  It ends at shutdown, sent once the run answers. */
async function session(adapter: FakeAdapter, react: (m: Msg, send: (r: unknown) => void) => void = () => {}, mediated?: string[]): Promise<Msg[]> {
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
  send(req(2, "prepare", prepare(mediated)));
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
const named = (msgs: Msg[], name: string) => events(msgs).filter((e) => e.event === name);
const result = (msgs: Msg[]) => {
  const r = msgs.find((m) => m.id === 3)!;
  assert.ok(r.result, `the run failed: ${JSON.stringify(r.error)}`);
  return r.result;
};
/** total is the field-wise sum of the usage events, computed here, apart from the fake's own sum. */
const total = (msgs: Msg[]) => {
  const us = named(msgs, "usage");
  const add = (f: (u: any) => number) => us.reduce((n, u) => n + f(u), 0);
  return {
    tokens: { in: add((u) => u.tokens.in), out: add((u) => u.tokens.out), cache_read: add((u) => u.tokens.cache_read), cache_write: add((u) => u.tokens.cache_write) },
    turns: add((u) => u.turns),
    milliseconds: add((u) => u.milliseconds),
    cost: { amount_micros: add((u) => u.cost.amount_micros), currency: "USD", basis: "cash", estimate: true },
  };
};
const change = (n: number) => `--- a/file${n}.go\n+++ b/file${n}.go\n@@ -1 +1 @@\n-old\n+new\n`;
const digest = (s: string) => "sha256:" + createHash("sha256").update(s).digest("hex");

test("every scenario a host can complete runs a valid session, the envelope stamped", async () => {
  for (const s of scenarios.filter((s) => !["broken-event", "hang", "tool"].includes(s))) {
    const msgs = await session(new FakeAdapter({ scenario: s, gapMs: 100 }));
    result(msgs);
    assert.ok(events(msgs).every((e) => e.attempt === att && e.fencing_token === 3 && e.call === cal), `${s}: envelope`);
  }
});

test("patch: two turns, a cumulative patch, usage numbered per call and summed field by field in the final", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "patch" }));
  const r = result(msgs);
  assert.deepEqual([r.outcome, r.result.kind], ["found", "patch"]);
  assert.deepEqual(named(msgs, "usage").map((u) => u.sequence), [0, 1]);
  assert.deepEqual(r.usage_final, total(msgs));
  const both = change(1) + change(2);
  assert.deepEqual(r.result.handle, { attempt: att, handle: "patch.diff", digest: digest(both), size: both.length });
  // Every turn: started, upstream reported, edited by the harness, covered (partial), ended.
  assert.deepEqual(named(msgs, "turn.end").map((e) => e.turn), [1, 2]);
  assert.deepEqual(named(msgs, "upstream").map((e) => e.turn), [1, 2]);
  assert.deepEqual(named(msgs, "coverage").map((c) => [c.turn, c.observable, c.unobservable]), [[1, "partial", ["harness_tool"]], [2, "partial", ["harness_tool"]]]);
  assert.deepEqual(named(msgs, "tool.result").map((t) => [t.tool, t.mediated_by_controller, t.delivered]), [["Edit", false, null], ["Edit", false, null]]);
});

test("text and no-change: text-only turns are fully observable; no change is empty, never found", async () => {
  const text = await session(new FakeAdapter({ scenario: "text" }));
  assert.equal(result(text).result.kind, "text");
  assert.deepEqual(named(text, "coverage").map((c) => [c.observable, c.unobservable]), [["complete", []]]);
  const none = await session(new FakeAdapter({ scenario: "no-change" }));
  const r = result(none);
  assert.deepEqual([r.outcome, r.reason, r.result], ["empty", "completed_clean", null]);
  assert.deepEqual(r.usage_final, total(none));
});

test("sub-calls: each is announced before use, and every turn, upstream and usage names its own", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "sub-calls" }));
  assert.deepEqual(named(msgs, "call.sub").map((e) => e.sub_call), ["s1", "s2"]);
  for (const kind of ["turn.start", "upstream", "turn.end"]) assert.deepEqual(named(msgs, kind).map((e) => e.sub_call), ["s1", "s2"], kind);
  assert.deepEqual(named(msgs, "usage").map((u) => [u.sub_call, u.sequence]), [["s1", 0], ["s2", 1]]);
  const all = events(msgs);
  assert.ok(all.findIndex((e) => e.event === "call.sub" && e.sub_call === "s2") < all.findIndex((e) => e.event === "turn.start" && e.sub_call === "s2"));
});

test("no-usage: telemetry says so, no usage events, a null final", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "no-usage" }));
  assert.equal(msgs.find((m) => m.id === 1)!.result.telemetry.usage, false);
  assert.equal(named(msgs, "usage").length, 0);
  assert.equal(result(msgs).usage_final, null);
});

test("stream-gap: heartbeats cover the pause between deltas", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "stream-gap", gapMs: 600 }));
  const all = events(msgs);
  const first = all.findIndex((e) => e.event === "delta.text");
  const next = all.findIndex((e, i) => i > first && e.event.startsWith("delta."));
  assert.ok(first >= 0 && next > first, "two deltas");
  assert.ok(all.slice(first, next).filter((e) => e.event === "heartbeat").length >= 2, "heartbeats in the gap");
});

test("compaction: the event, and the turn declared partial for it", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "compaction" }));
  assert.equal(named(msgs, "compaction").length, 1);
  assert.deepEqual(named(msgs, "coverage")[0].unobservable, ["compaction", "harness_tool"]);
});

test("provider errors: partial work, then the error; could_not_run with the exit map's reason, no result, no invented final", async () => {
  for (const [s, hints, reason] of [
    ["rate-limited", ["rate_limited"], "quota_exhausted"],
    ["credit-exhausted", ["credit_exhausted"], "broker_credit_exhausted"],
    ["http-4xx", ["http_4xx"], "provider_request_refused"],
    ["http-5xx", ["http_5xx"], "provider_unavailable"],
    ["idle-cut", ["idle_cut"], "stream_idle_cut"],
    ["degenerate", ["degenerate"], "degenerate_output"],
    ["other-error", ["other"], "executor_incomplete"],
    ["two-errors", ["http_5xx", "credit_exhausted"], "broker_credit_exhausted"],
  ] as [Scenario, string[], string][]) {
    const msgs = await session(new FakeAdapter({ scenario: s }));
    const r = result(msgs);
    assert.deepEqual([r.outcome, r.reason, r.result, r.usage_final], ["could_not_run", reason, null, null], s);
    assert.deepEqual(named(msgs, "error").map((e) => e.class_hint), hints, s);
    const all = events(msgs);
    const err = all.findIndex((e) => e.event === "error");
    assert.ok(all.slice(0, err).some((e) => e.event === "delta.tool_call") && all.slice(0, err).some((e) => e.event === "usage"), `${s}: partial work before the error`);
  }
});

const answerTool = (m: Msg, send: (r: unknown) => void, delivered: unknown) => {
  if (m.method === eventMethod && m.params.event === "tool.request") {
    send(req(4, "tool.response", { attempt: att, fencing_token: 3, call: cal, turn: m.params.turn, tool: m.params.tool, request: m.params.request,
      outcome: "found", reason: "gate_test_failed", detail: "TestX failed", delivered }));
  }
};

test("tool: the tool call streams, then the round-trip reports exactly the delivered handle and its size", async () => {
  const delivered = handle(att, "out.txt", "FAIL TestX");
  const msgs = await session(new FakeAdapter({ scenario: "tool" }), (m, send) => answerTool(m, send, delivered));
  assert.deepEqual(msgs.find((m) => m.id === 4)!.result, {});
  const all = events(msgs);
  const request = all.findIndex((e) => e.event === "tool.request");
  assert.equal(all[request - 1].event, "delta.tool_call", "the tool call's delta comes first");
  const mediated = named(msgs, "tool.result").find((t) => t.mediated_by_controller);
  assert.deepEqual([mediated.delivered, mediated.bytes], [delivered, delivered.size]);
  assert.equal(result(msgs).outcome, "found");
});

test("tool: a policy that does not mediate the tool is not used", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "tool" }), () => {}, []);
  assert.equal(named(msgs, "tool.request").length, 0);
  assert.deepEqual([result(msgs).outcome, result(msgs).reason], ["could_not_run", "executor_incomplete"]);
});

test("cancel during a tool call, and during a stream, ends in could_not_run/attempt_stopped", async () => {
  for (const [s, at] of [["tool", "tool.request"], ["hang", "delta.thinking"]] as [Scenario, string][]) {
    let cancelled = false;
    const msgs = await session(new FakeAdapter({ scenario: s }), (m, send) => {
      if (!cancelled && m.method === eventMethod && m.params.event === at) {
        cancelled = true;
        send(req(5, "cancel", { attempt: att, fencing_token: 3 }));
      }
    });
    assert.equal(msgs.find((m) => m.id === 5)!.result.outcome, "empty", s);
    const r = result(msgs);
    assert.deepEqual([r.outcome, r.reason, r.result, r.usage_final], ["could_not_run", "attempt_stopped", null, null], s);
  }
});

test("broken-event, the SDK's red-proof: the SDK refuses the event and fails the run", async () => {
  const msgs = await session(new FakeAdapter({ scenario: "broken-event" }));
  const r = msgs.find((m) => m.id === 3)!;
  assert.equal(r.error?.code, InternalError);
  assert.match(r.error!.message, /an event does not match adapter\/event/);
});

test("the kit's red-proofs pass the schema but break the contract's other rules", async () => {
  const seq = await session(new FakeAdapter({ scenario: "bad-sequence" }));
  assert.deepEqual(named(seq, "usage").map((u) => u.sequence), [0, 2]);
  const sub = await session(new FakeAdapter({ scenario: "unannounced-sub-call" }));
  assert.deepEqual(named(sub, "call.sub").map((e) => e.sub_call), ["s1"]);
  assert.ok(named(sub, "turn.start").every((e) => e.sub_call === "s9"));
  const nc = await session(new FakeAdapter({ scenario: "non-cumulative" }));
  assert.equal(result(nc).result.handle.digest, digest(change(2)));
});

test("bad options are refused: an unknown scenario, a gap that is not a millisecond count", () => {
  assert.throws(() => new FakeAdapter({ scenario: "nope" as Scenario }), /unknown fake scenario/);
  for (const gapMs of [-5, 1.5, Number.NaN, 2 ** 31]) assert.throws(() => new FakeAdapter({ scenario: "stream-gap", gapMs }), /stream gap/, String(gapMs));
});

test("the process refuses a bad environment with exit 2 before reading input", () => {
  const main = fileURLToPath(new URL("../src/main.ts", import.meta.url));
  for (const env of [{ ADC_FAKE_SCENARIO: "nope" }, { ADC_FAKE_GAP_MS: "abc" }, { ADC_FAKE_GAP_MS: "" }, { ADC_FAKE_GAP_MS: "-5" }, { ADC_FAKE_GAP_MS: "99999999999" }]) {
    const r = spawnSync(process.execPath, [main], { env: { ...process.env, ...env }, input: "", encoding: "utf8" });
    assert.equal(r.status, 2, JSON.stringify(env));
    assert.match(r.stderr, /adc fake adapter:/);
  }
});
