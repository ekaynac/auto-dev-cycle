// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import type { AdapterDescribeResult } from "@adc/contract-gen/types/adapter/describe-result.ts";
import type { AdapterOutcome } from "@adc/contract-gen/types/adapter/outcome.ts";
import type { AdapterPrepareParams } from "@adc/contract-gen/types/adapter/prepare-params.ts";
import type { AdapterRunParams } from "@adc/contract-gen/types/adapter/run-params.ts";
import type { AdapterRunResult } from "@adc/contract-gen/types/adapter/run-result.ts";
import {
  Adapter, type EventBody, FramingError, type RunTools, heartbeatPeriod, InternalError, InvalidParams, InvalidRequest, MethodNotFound, ParseError,
  StrictJSONError, eventMethod, frame, lines, parseStrict, serve,
} from "../src/index.ts";

const att = "att_01m3250vnfeyrv67x6w0ddqr6a";
const cal = "cal_01m3250v9xft08ssr4ygq6g2np";
const handle = (name: string) => ({ attempt: att, handle: name, digest: "sha256:" + "a".repeat(64), size: 3 });
const usage = { tokens: { in: 1, out: 1, cache_read: 0, cache_write: 0 }, turns: 1, milliseconds: 5,
  cost: { amount_micros: 0, currency: "USD", basis: "cash", estimate: true } };
const prepareParams = { attempt: att, fencing_token: 3, role: "builder", executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", model: "m",
  workspace: { path: "/workspace", writable: true }, exchange: { in: "/exchange/in", out: "/exchange/out" }, inputs: [], limits: { max_turns: 1, max_output_tokens: 10, wall_seconds: 60 },
  tool_policy: { allowed: [], mediated: [] }, gateway: null, native_login: null };
const runParams = { attempt: att, fencing_token: 3, call: cal, prompt: handle("p.md"), response_schema: null };
const ok: AdapterOutcome = { outcome: "empty", reason: "completed_clean", detail: "" };

class Toy extends Adapter {
  events: unknown[] = [{ event: "turn.start", turn: 1, sub_call: "s1", reported_at: "2026-10-05T01:00:00Z" }];
  result: unknown = { outcome: "found", reason: "completed_with_findings", detail: "a patch", result: { kind: "patch", handle: handle("x.diff") }, usage_final: usage };
  description: unknown = { harness: "toy", harness_version: "1", adapter_version: "0.1.0", contract_version: "1.0.0", capabilities: [],
    session_semantics: "fresh_process", tool_surface: [], telemetry: { usage: true, upstream: false, stream_deltas: false, per_turn_coverage: false },
    host_side: false, needs_native_login: false };
  delayMs = 0;
  canceled = false;
  async describe() { return this.description as AdapterDescribeResult; }
  async prepare(_p: AdapterPrepareParams) { return ok; }
  async *run(_p: AdapterRunParams, _signal?: AbortSignal, _tools?: RunTools): AsyncGenerator<EventBody, AdapterRunResult> {
    for (const e of this.events) yield e as EventBody;
    if (this.delayMs) await new Promise((r) => setTimeout(r, this.delayMs));
    return this.result as AdapterRunResult;
  }
  override async cancel() { this.canceled = true; return ok; }
}

async function* chunks(...parts: string[]) {
  for (const p of parts) yield Buffer.from(p);
}

type Msg = { id?: unknown; result?: any; error?: { code: number; message: string }; method?: string; params?: any };

async function talk(adapter: Adapter, requests: unknown[], heartbeatMs?: number): Promise<Msg[]> {
  const out: Msg[] = [];
  const text = requests.map((r) => (typeof r === "string" ? r : JSON.stringify(r)) + "\n").join("");
  await serve(adapter, { input: chunks(text), write: (l) => out.push(JSON.parse(l)), heartbeatMs });
  return out;
}

const req = (id: number, method: string, params?: unknown) => ({ jsonrpc: "2.0", id, method, params });
const byId = (msgs: Msg[], id: number) => msgs.find((m) => m.id === id)!;

test("strict JSON refuses what JSON.parse lets through", () => {
  assert.deepEqual(parseStrict('{"a":[1,-2,0,"x\\u00e9\\ud83d\\ude00"],"b":{"c":null,"d":true}}'),
    { a: [1, -2, 0, "xé😀"], b: { c: null, d: true } });
  assert.deepEqual(parseStrict(String(Number.MAX_SAFE_INTEGER)), Number.MAX_SAFE_INTEGER);
  for (const bad of ['{"a":1,"a":2}', "1.0", "1e1", "-0", "01", "9007199254740992", '"\\ud800"', '"\\udc00x"', '"a\u0001"',
    "[1,]", "{}x", "[1] 2", '{"a" 1}', "tru", '"\\x"', "[".repeat(129) + "]".repeat(129), "", "NaN", '{"__proto__":1,"__proto__":2}']) {
    assert.throws(() => parseStrict(bad), StrictJSONError, bad.slice(0, 20));
  }
  assert.deepEqual(parseStrict("[".repeat(128) + "]".repeat(128)) !== undefined, true, "128 levels");
  assert.throws(() => parseStrict("[1.5]"), /not an integer spelling/);
  assert.throws(() => parseStrict("2E3"), /not an integer spelling/);
  const proto = parseStrict('{"__proto__":{"polluted":1}}') as Record<string, unknown>;
  assert.equal(({} as Record<string, unknown>).polluted, undefined);
  assert.ok(Object.hasOwn(proto, "__proto__"));
});

test("framing splits lines across chunks and bounds them", async () => {
  const got: string[] = [];
  for await (const l of lines(chunks('{"a":', '1}\n{"b"', ":2}\n"))) got.push(l);
  assert.deepEqual(got, ['{"a":1}', '{"b":2}']);
  await assert.rejects(async () => { for await (const _ of lines(chunks("x".repeat(11) + "\n"), 10)); }, FramingError);
  await assert.rejects(async () => { for await (const _ of lines(chunks("x".repeat(6), "x".repeat(6)), 10)); }, FramingError);
  await assert.rejects(async () => { for await (const _ of lines(chunks('{"a":1}'))); }, FramingError, "a truncated last message");
  await assert.rejects(async () => { for await (const _ of lines((async function* () { yield Uint8Array.from([0xff, 0x0a]); })())); }, FramingError);
  // A message without its newline is refused as soon as it is too long,
  // not buffered until the input ends.
  let pulled = 0;
  const endless = (async function* () {
    for (;;) {
      pulled++;
      yield Buffer.from("x".repeat(4));
    }
  })();
  await assert.rejects(async () => { for await (const _ of lines(endless, 10)); }, FramingError);
  assert.ok(pulled <= 4, `${pulled} chunks pulled`);
  assert.equal(frame({ a: 1 }), '{"a":1}\n');
  assert.throws(() => frame({ a: "x".repeat(20) }, 10), FramingError);
});

test("a full session: describe, prepare, run with stamped events, shutdown", async () => {
  const msgs = await talk(new Toy(), [req(1, "describe"), req(2, "prepare", prepareParams), req(3, "run", runParams), req(4, "shutdown")]);
  assert.equal(byId(msgs, 1).result.harness, "toy");
  assert.deepEqual(byId(msgs, 2).result, ok);
  const events = msgs.filter((m) => m.method === eventMethod).map((m) => m.params);
  assert.deepEqual(events, [{ event: "turn.start", turn: 1, sub_call: "s1", reported_at: "2026-10-05T01:00:00Z", attempt: att, fencing_token: 3, call: cal }]);
  assert.equal(byId(msgs, 3).result.result.kind, "patch");
  assert.deepEqual(byId(msgs, 4).result, ok);
  // The run's result comes after its events.
  assert.ok(msgs.indexOf(byId(msgs, 3)) > msgs.findIndex((m) => m.method === eventMethod));
});

test("protocol errors", async () => {
  const msgs = await talk(new Toy(), ["{not json", '{"jsonrpc":"2.0","method":"describe"}', JSON.stringify({ jsonrpc: "1.0", id: 5, method: "describe" }),
    req(6, "frobnicate"), req(7, "prepare", { ...prepareParams, home: "/home/op" }), req(8, "run", runParams), req(9, "cancel", { attempt: att, fencing_token: 3 }),
    req(10, "prepare", prepareParams), req(11, "run", { ...runParams, fencing_token: 4 }), req(12, "cancel", { attempt: att, fencing_token: 2 }),
    req(13, "run", { ...runParams, call: "not-a-call" }), req(14, "cancel", { attempt: att, fencing_token: 3, reason: "x" })]);
  assert.equal(msgs[0].error!.code, ParseError);
  assert.equal(msgs[0].id, null);
  assert.equal(msgs[1].error!.code, InvalidRequest, "a notification is not served");
  assert.equal(byId(msgs, 5).error!.code, InvalidRequest);
  assert.equal(byId(msgs, 6).error!.code, MethodNotFound);
  assert.equal(byId(msgs, 7).error!.code, InvalidParams);
  assert.equal(byId(msgs, 8).error!.code, InvalidParams, "a run before prepare");
  assert.equal(byId(msgs, 9).error!.code, InvalidParams, "a cancel before prepare");
  assert.equal(byId(msgs, 11).error!.code, InvalidParams, "a run under another token");
  assert.equal(byId(msgs, 12).error!.code, InvalidParams, "a cancel under another token");
  assert.equal(byId(msgs, 13).error!.code, InvalidParams, "a run with an invalid call");
  assert.equal(byId(msgs, 14).error!.code, InvalidParams, "a cancel with an unknown field");
});

test("the adapter's own output is validated before it is sent", async () => {
  const cases: [string, (t: Toy) => void][] = [
    ["an invalid event", (t) => { t.events = [{ event: "turn.start", turn: 0, sub_call: null, reported_at: "x" }]; }],
    ["a result on a failed run", (t) => { t.result = { outcome: "failed", reason: "unsafe_output", detail: "x", result: { kind: "patch", handle: handle("x") }, usage_final: usage }; }],
    ["an invalid description", (t) => { t.description = { harness: "toy" }; }],
  ];
  for (const [name, spoil] of cases) {
    const t = new Toy();
    spoil(t);
    const msgs = await talk(t, [req(1, "describe"), req(2, "prepare", prepareParams), req(3, "run", runParams)]);
    const failed = name === "an invalid description" ? byId(msgs, 1) : byId(msgs, 3);
    assert.equal(failed.error?.code, InternalError, name);
    assert.ok(!msgs.some((m) => m.method === eventMethod && m.params.turn === 0), `${name}: an invalid event was sent`);
  }
});

test("heartbeats during a run, and a cancel served while it runs", async () => {
  const t = new Toy();
  t.events = [];
  t.delayMs = 120;
  const msgs = await talk(t, [req(1, "prepare", prepareParams), req(2, "run", runParams), req(3, "cancel", { attempt: att, fencing_token: 3 }), req(4, "shutdown")], 20);
  const beats = msgs.filter((m) => m.method === eventMethod && m.params.event === "heartbeat");
  assert.ok(beats.length >= 2, `${beats.length} heartbeats`);
  assert.deepEqual(beats[0].params, { event: "heartbeat", attempt: att, fencing_token: 3, call: cal });
  assert.ok(t.canceled);
  assert.ok(msgs.indexOf(byId(msgs, 3)) < msgs.indexOf(byId(msgs, 2)), "the cancel was answered while the run was going");
  assert.ok(msgs.indexOf(byId(msgs, 4)) > msgs.indexOf(byId(msgs, 2)), "shutdown waits for the run");
});

test("a failed prepare prepares nothing", async () => {
  const t = new Toy();
  t.prepare = async () => ({ outcome: "could_not_run", reason: "ambiguous_state", detail: "no workspace" });
  const msgs = await talk(t, [req(1, "prepare", prepareParams), req(2, "run", runParams)]);
  assert.equal(byId(msgs, 1).result.outcome, "could_not_run");
  assert.equal(byId(msgs, 2).error?.code, InvalidParams);
});

test("the envelope wins, and the fence names both attempt and token", async () => {
  const t = new Toy();
  t.events = [{ event: "turn.start", turn: 1, sub_call: "s1", reported_at: "2026-10-05T01:00:00Z", attempt: "att_01m3250v57ft7aehkqwq0yrq3n", fencing_token: 9 }];
  const msgs = await talk(t, [req(1, "prepare", prepareParams), req(2, "run", { ...runParams, attempt: "att_01m3250v57ft7aehkqwq0yrq3n" }), req(3, "run", runParams)]);
  assert.equal(byId(msgs, 2).error?.code, InvalidParams, "another attempt with the same token");
  const ev = msgs.find((m) => m.method === eventMethod)!.params;
  assert.equal(ev.attempt, att);
  assert.equal(ev.fencing_token, 3);
});

test("a failed re-prepare clears the fence; a prepare during a run and a second run are refused", async () => {
  let n = 0;
  const t = new Toy();
  t.prepare = async () => (n++ === 0 ? ok : { outcome: "failed", reason: "unsafe_output", detail: "x" });
  let msgs = await talk(t, [req(1, "prepare", prepareParams), req(2, "prepare", prepareParams), req(3, "run", runParams)]);
  assert.equal(byId(msgs, 3).error?.code, InvalidParams);
  const slow = new Toy();
  slow.events = [];
  slow.delayMs = 60;
  msgs = await talk(slow, [req(1, "prepare", prepareParams), req(2, "run", runParams), req(3, "run", runParams), req(4, "prepare", prepareParams)]);
  assert.equal(byId(msgs, 3).error?.code, InvalidParams, "a second run");
  assert.equal(byId(msgs, 4).error?.code, InvalidParams, "a prepare during a run");
  assert.equal(byId(msgs, 2).result?.outcome, "found");
});

test("adapter faults and off-contract outcomes are internal errors", async () => {
  const thrower = new Toy();
  thrower.describe = async () => { throw new Error("boom"); };
  let msgs = await talk(thrower, [req(1, "describe")]);
  assert.equal(byId(msgs, 1).error?.code, InternalError);
  const bad = { outcome: "failed", reason: "unsafe_output", detail: "" }; // failed needs a detail
  const t = new Toy();
  let prepares = 0;
  t.prepare = async () => (prepares++ === 0 ? ok : (bad as AdapterOutcome));
  t.cancel = async () => bad as AdapterOutcome;
  t.shutdown = async () => bad as AdapterOutcome;
  msgs = await talk(t, [req(1, "prepare", prepareParams), req(2, "cancel", { attempt: att, fencing_token: 3 }), req(3, "prepare", prepareParams), req(4, "shutdown")]);
  for (const id of [2, 3, 4]) assert.equal(byId(msgs, id).error?.code, InternalError, `request ${id}`);
  // A lone surrogate (a string cut in the middle of a pair) never goes out.
  const cut = new Toy();
  cut.result = { outcome: "found", reason: "completed_with_findings", detail: "😀".slice(0, 1), result: { kind: "text", handle: handle("t") }, usage_final: usage };
  msgs = await talk(cut, [req(1, "prepare", prepareParams), req(2, "run", runParams)]);
  assert.equal(byId(msgs, 2).error?.code, InternalError);
});

test("the heartbeat period is bounded", () => {
  assert.equal(heartbeatPeriod(), 5000);
  assert.equal(heartbeatPeriod(20), 20);
  assert.equal(heartbeatPeriod(60000), 9000);
  assert.equal(heartbeatPeriod(0), 5000);
  assert.equal(heartbeatPeriod(Number.NaN), 5000);
});

class Watched extends Toy {
  cleaned = false;
  aborted = false;
  waitForAbort = false;
  override async *run(_p: AdapterRunParams, signal: AbortSignal): AsyncGenerator<EventBody, AdapterRunResult> {
    try {
      for (const e of this.events) yield e as EventBody;
      if (this.waitForAbort) {
        await new Promise<void>((r) => signal.addEventListener("abort", () => r(), { once: true }));
        this.aborted = true;
      }
      return this.result as AdapterRunResult;
    } finally {
      this.cleaned = true;
    }
  }
}

test("the adapter's cleanup runs, and its signal aborts, on every abnormal path", async () => {
  const invalid = new Watched();
  invalid.events = [{ event: "turn.start", turn: 0, sub_call: "s1", reported_at: "x" }, { event: "turn.end", turn: 1, sub_call: "s1", reported_at: "2026-10-05T01:00:00Z" }];
  await talk(invalid, [req(1, "prepare", prepareParams), req(2, "run", runParams)]);
  assert.ok(invalid.cleaned, "an invalid event: the generator was closed");
  const canceled = new Watched();
  canceled.events = [];
  canceled.waitForAbort = true;
  const msgs = await talk(canceled, [req(1, "prepare", prepareParams), req(2, "run", runParams), req(3, "cancel", { attempt: att, fencing_token: 3 })]);
  assert.ok(canceled.aborted, "cancel aborts the run's signal");
  assert.equal(byId(msgs, 2).result?.outcome, "found");
  // A broken input ends the session: the run is stopped before serve rejects.
  const broken = new Watched();
  broken.events = [];
  broken.waitForAbort = true;
  const out: string[] = [];
  const input = (async function* () {
    yield Buffer.from(JSON.stringify(req(1, "prepare", prepareParams)) + "\n" + JSON.stringify(req(2, "run", runParams)) + "\n");
    await new Promise((r) => setTimeout(r, 20));
    yield Buffer.from("x".repeat(17 * 1024 * 1024));
  })();
  await assert.rejects(serve(broken, { input, write: (l) => out.push(l) }), FramingError);
  assert.ok(broken.aborted && broken.cleaned, "the run was aborted and cleaned up");
});

// --- controller-mediated tools: tool.request → tool.response → tool.result (09) ---

/** channel is an input the test feeds while serve runs. */
function channel() {
  const queue: string[] = [];
  let wake: (() => void) | null = null;
  return {
    push(line: string) {
      queue.push(line);
      wake?.();
      wake = null;
    },
    async *[Symbol.asyncIterator]() {
      for (;;) {
        while (queue.length) yield Buffer.from(queue.shift()! + "\n");
        await new Promise<void>((r) => (wake = r));
      }
    },
  };
}

/** session drives serve interactively: react sees each message and may send
 *  more requests. It ends when serve does (after shutdown). */
async function session(adapter: Adapter, first: unknown[], react: (m: Msg, send: (r: unknown) => void) => void): Promise<Msg[]> {
  const ch = channel();
  const out: Msg[] = [];
  const send = (r: unknown) => ch.push(JSON.stringify(r));
  for (const r of first) send(r);
  await serve(adapter, {
    input: ch,
    write: (l) => {
      const m = JSON.parse(l) as Msg;
      out.push(m);
      react(m, send);
    },
  });
  return out;
}

const tool = "RunTest.v2/x+y";
const toolResponse = (over: Record<string, unknown> = {}) => ({ attempt: att, fencing_token: 3, call: cal, turn: 1, tool, request: handle("req-1.json"),
  outcome: "failed", reason: "unsafe_output", detail: "the tool's output was unsafe", delivered: handle("out.txt"), ...over });

class Mediator extends Toy {
  got: unknown = null;
  report: (delivered: unknown) => unknown = (d) => d;
  resultTurn = 1;
  resultTool = tool;
  requestAttempt = att;
  repeatRequest = false;
  reportTwice = false;
  /** Before awaiting the response: a pause, or until the run is aborted. */
  waitMs = 0;
  awaitAbort = false;
  doubleWait = "";
  /** Return without awaiting the response; abandoned is that promise. */
  abandon = false;
  abandoned: Promise<unknown> | null = null;
  aborted = "";
  override async *run(_p: AdapterRunParams, signal: AbortSignal, tools: RunTools): AsyncGenerator<EventBody, AdapterRunResult> {
    const request = { ...handle("req-1.json"), attempt: this.requestAttempt };
    yield { event: "tool.request", turn: 1, tool, request } as EventBody;
    if (this.repeatRequest) yield { event: "tool.request", turn: 1, tool, request } as EventBody;
    if (this.abandon) {
      this.abandoned = tools.response(request);
      this.abandoned.catch(() => undefined);
      return this.result as AdapterRunResult;
    }
    if (this.waitMs) await new Promise((r) => setTimeout(r, this.waitMs));
    if (this.awaitAbort) await new Promise((r) => signal.addEventListener("abort", r, { once: true }));
    try {
      const first = tools.response(request);
      if (this.waitMs === 0 && !this.awaitAbort) {
        await tools.response(request).catch((e: Error) => (this.doubleWait = e.message));
      }
      this.got = await first;
    } catch (e) {
      this.aborted = (e as Error).message;
      throw e;
    }
    const delivered = this.report((this.got as { delivered: unknown }).delivered);
    yield { event: "tool.result", turn: this.resultTurn, tool: this.resultTool, mediated_by_controller: true, delivered, bytes: 3 } as EventBody;
    if (this.reportTwice) yield { event: "tool.result", turn: this.resultTurn, tool: this.resultTool, mediated_by_controller: true, delivered, bytes: 3 } as EventBody;
    return this.result as AdapterRunResult;
  }
}

const isToolRequest = (m: Msg) => m.method === eventMethod && m.params?.event === "tool.request";

test("a controller-mediated tool round-trip, for a failed tool too", async () => {
  const a = new Mediator();
  const msgs = await session(a, [req(1, "prepare", prepareParams), req(2, "run", runParams)], (m, send) => {
    if (isToolRequest(m)) send(req(3, "tool.response", toolResponse()));
    if (m.id === 2) send(req(4, "shutdown"));
  });
  assert.deepEqual(byId(msgs, 3).result, {});
  assert.deepEqual(a.got, toolResponse());
  assert.equal(a.doubleWait, "tool.request req-1.json is already awaited");
  const result = msgs.find((m) => m.method === eventMethod && m.params?.event === "tool.result")!;
  assert.deepEqual(result.params.delivered, handle("out.txt"));
  assert.equal(byId(msgs, 2).result.outcome, "found");
  // The answer comes after the request, the result after the answer.
  assert.ok(msgs.findIndex(isToolRequest) < msgs.indexOf(byId(msgs, 3)));
  assert.ok(msgs.indexOf(byId(msgs, 3)) < msgs.indexOf(result));
});

test("tool.response is refused unless it answers this run's open request", async () => {
  const msgs = await session(new Mediator(), [req(1, "tool.response", toolResponse()), req(2, "prepare", prepareParams), req(3, "run", runParams)], (m, send) => {
    if (!isToolRequest(m)) {
      if (m.id === 3) send(req(19, "shutdown"));
      return;
    }
    send(req(10, "tool.response", toolResponse({ call: "cal_01m3250vnfeyrv67x6w0ddqr6a" })));
    send(req(11, "tool.response", toolResponse({ fencing_token: 4 })));
    send(req(12, "tool.response", toolResponse({ request: handle("req-9.json") })));
    send(req(13, "tool.response", toolResponse({ tool: "Other" })));
    send(req(14, "tool.response", toolResponse({ turn: 2 })));
    send(req(15, "tool.response", toolResponse({ delivered: null })));
    send(req(20, "tool.response", toolResponse({ request: { ...handle("req-1.json"), size: 99 } })));
    send(req(21, "tool.response", toolResponse({ delivered: { ...handle("out.txt"), attempt: "att_01m3250v57fhm9v1z9xd1wk8gw" } })));
    send(req(16, "tool.response", toolResponse()));
    send(req(17, "tool.response", toolResponse()));
  });
  const refused = (id: number, why: RegExp) => {
    assert.equal(byId(msgs, id).error?.code, InvalidParams, `request ${id}`);
    assert.match(byId(msgs, id).error!.message, why, `request ${id}`);
  };
  refused(1, /not the prepared attempt|no run/);
  refused(10, /no run for call/);
  refused(11, /not the prepared attempt and fencing token/);
  refused(12, /no tool.request with handle req-9.json/);
  refused(13, /the request was turn 1, tool RunTest/);
  refused(14, /the request was turn 1/);
  refused(15, /does not match adapter\/tool-response-params/);
  refused(20, /no tool.request with handle req-1.json/);
  refused(21, /names another attempt/);
  assert.deepEqual(byId(msgs, 16).result, {});
  refused(17, /already answered/);
  assert.equal(byId(msgs, 3).result.outcome, "found");
});

test("a mediated tool.result must report the handle the core delivered, for that turn and tool", async () => {
  for (const [name, set, why] of [
    ["another handle", (a: Mediator) => (a.report = () => handle("forged.txt")), /must report a handle the core delivered/],
    ["the same name, another digest", (a: Mediator) => (a.report = (d) => ({ ...(d as object), digest: "sha256:" + "b".repeat(64) })), /must report a handle/],
    ["the same name, another size", (a: Mediator) => (a.report = (d) => ({ ...(d as object), size: 4 })), /must report a handle/],
    ["the same name, another attempt", (a: Mediator) => (a.report = (d) => ({ ...(d as object), attempt: "att_01m3250v57fhm9v1z9xd1wk8gw" })), /must report a handle/],
    ["the delivered handle on another turn", (a: Mediator) => (a.resultTurn = 2), /must report a handle/],
    ["the delivered handle for another tool", (a: Mediator) => (a.resultTool = "Other"), /must report a handle/],
    ["a request handle used twice", (a: Mediator) => (a.repeatRequest = true), /already in use/],
    ["a request handle of another attempt", (a: Mediator) => (a.requestAttempt = "att_01m3250v57fhm9v1z9xd1wk8gw"), /names another attempt/],
  ] as const) {
    const a = new Mediator();
    set(a);
    const msgs = await session(a, [req(1, "prepare", prepareParams), req(2, "run", runParams)], (m, send) => {
      if (isToolRequest(m) && !a.repeatRequest && a.requestAttempt === att) send(req(3, "tool.response", toolResponse()));
      if (m.id === 2) send(req(4, "shutdown"));
    });
    assert.equal(byId(msgs, 2).error?.code, InternalError, name);
    assert.match(byId(msgs, 2).error!.message, why, name);
    assert.equal(msgs.filter((m) => m.method === eventMethod && m.params?.event === "tool.result").length, 0, `${name}: the result was not sent`);
  }
});

test("one delivery is reported once", async () => {
  const a = new Mediator();
  a.reportTwice = true;
  const msgs = await session(a, [req(1, "prepare", prepareParams), req(2, "run", runParams)], (m, send) => {
    if (isToolRequest(m)) send(req(3, "tool.response", toolResponse()));
    if (m.id === 2) send(req(4, "shutdown"));
  });
  assert.equal(byId(msgs, 2).error?.code, InternalError);
  assert.match(byId(msgs, 2).error!.message, /once/);
  assert.equal(msgs.filter((m) => m.method === eventMethod && m.params?.event === "tool.result").length, 1, "only the first report was sent");
});

test("an answer that arrives before the run waits for it is kept", async () => {
  const a = new Mediator();
  a.waitMs = 50;
  const msgs = await session(a, [req(1, "prepare", prepareParams), req(2, "run", runParams)], (m, send) => {
    if (isToolRequest(m)) send(req(3, "tool.response", toolResponse()));
    if (m.id === 2) send(req(4, "shutdown"));
  });
  assert.deepEqual(a.got, toolResponse());
  assert.equal(byId(msgs, 2).result.outcome, "found");
});

test("waiting after an abort rejects, and a wait the run abandons is released when it ends", async () => {
  const late = new Mediator();
  late.awaitAbort = true;
  await session(late, [req(1, "prepare", prepareParams), req(2, "run", runParams)], (m, send) => {
    if (isToolRequest(m)) send(req(3, "cancel", { attempt: att, fencing_token: 3 }));
    if (m.id === 2) send(req(4, "shutdown"));
  });
  assert.equal(late.aborted, "the run was aborted");
  const gone = new Mediator();
  gone.abandon = true;
  const msgs = await session(gone, [req(1, "prepare", prepareParams), req(2, "run", runParams)], (m, send) => {
    if (m.id === 2) send(req(4, "shutdown"));
  });
  assert.equal(byId(msgs, 2).result.outcome, "found");
  await assert.rejects(gone.abandoned!, /the run ended/);
});

test("a run waiting for a tool response is released by cancel", async () => {
  const a = new Mediator();
  const msgs = await session(a, [req(1, "prepare", prepareParams), req(2, "run", runParams)], (m, send) => {
    if (isToolRequest(m)) send(req(3, "cancel", { attempt: att, fencing_token: 3 }));
    if (m.id === 2) send(req(4, "shutdown"));
  });
  assert.equal(a.aborted, "the run was aborted");
  assert.deepEqual(byId(msgs, 3).result, ok);
  assert.equal(byId(msgs, 2).error?.code, InternalError);
});
