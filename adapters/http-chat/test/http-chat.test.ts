// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { eventMethod, serve } from "@adc/contract";
import { HttpChatAdapter, maxCallsPerTurn } from "../src/adapter.ts";
import { requestKeys } from "../src/gateway.ts";
import { call, refused, reply, stub, type Answer, type Stub } from "./stub.ts";
import { start } from "../../../fixtures/m1/stub-provider/server.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const att = "att_01m3250vnfeyrv67x6w0ddqr6a";
const cal = "cal_01m3250v9xft08ssr4ygq6g2np";
const token = "g".repeat(32);
const digest = (s: string | Buffer) => "sha256:" + createHash("sha256").update(s).digest("hex");
const req = (id: number, method: string, params?: unknown) => ({ jsonrpc: "2.0", id, method, params });
type Msg = { id?: unknown; result?: any; error?: { code: number; message: string }; method?: string; params?: any };

/** An attempt's directories: a workspace holding a small repository, an
 *  exchange with the prompt (and a response schema) in in/, and a sibling
 *  directory outside the workspace that nothing may write. */
function dirs(t: { after: (f: () => void) => void }, prompt = "add a greeting", schema?: object) {
  const root = mkdtempSync(join(tmpdir(), "adc-http-chat-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const d = { root, ws: join(root, "ws"), in: join(root, "in"), out: join(root, "out"), outside: join(root, "outside") };
  for (const p of [d.ws, d.in, d.out, d.outside, join(d.ws, "src")]) mkdirSync(p, { recursive: true });
  writeFileSync(join(d.ws, "src", "a.txt"), "one\ntwo\nthree\n");
  writeFileSync(join(d.outside, "secret.txt"), "outside\n");
  writeFileSync(join(d.in, "prompt.md"), prompt);
  if (schema) writeFileSync(join(d.in, "schema.json"), JSON.stringify(schema));
  return d;
}

type Dirs = ReturnType<typeof dirs>;
const handle = (d: Dirs, name: string) => {
  const b = readFileSync(join(d.in, name));
  return { attempt: att, handle: name, digest: digest(b), size: b.length };
};
const prepare = (d: Dirs, s: Stub, role = "builder", over: object = {}) => ({ attempt: att, fencing_token: 3, role, executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", model: "m-pinned",
  workspace: { path: d.ws, writable: role === "builder" }, exchange: { in: d.in, out: d.out }, inputs: [], limits: { max_turns: 6, max_output_tokens: 10000, wall_seconds: 60 },
  tool_policy: { allowed: ["read_file", "list_dir", "write_file", "delete_file"], mediated: [] }, gateway: { base_url: s.base, token }, native_login: null, ...over });
const runParams = (d: Dirs, over: object = {}) => ({ attempt: att, fencing_token: 3, call: cal, prompt: handle(d, "prompt.md"), response_schema: null, ...over });

/** session drives the adapter through the SDK's serve, as the core does. */
async function session(adapter: HttpChatAdapter, prep: object, run: object): Promise<Msg[]> {
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
  send(req(2, "prepare", prep));
  send(req(3, "run", run));
  await serve(adapter, { input, heartbeatMs: 50, write: (l) => {
    const m = JSON.parse(l) as Msg;
    out.push(m);
    if (m.id === 3) send(req(9, "shutdown"));
  } });
  return out;
}

const events = (msgs: Msg[], name: string) => msgs.filter((m) => m.method === eventMethod && m.params.event === name).map((m) => m.params);
const result = (msgs: Msg[]) => {
  const r = msgs.find((m) => m.id === 3)!;
  assert.ok(r.result, `the run failed: ${JSON.stringify(r.error)}`);
  return r.result;
};

async function withStub(t: { after: (f: () => void) => void }, script: Answer[], listen?: string): Promise<Stub> {
  const s = await stub(script, listen);
  t.after(() => s.close());
  return s;
}

/** The gateway's allowlist, read from its source (internal/gateway/egress.go). */
function gatewayAllowlist(): Set<string> {
  const src = readFileSync(join(here, "../../../internal/gateway/egress.go"), "utf8");
  const block = src.match(/var allowed = map\[string\]bool\{([^}]*)\}/);
  assert.ok(block, "the gateway's allowlist was not found");
  return new Set([...block[1].matchAll(/"([a-z_]+)": true/g)].map((m) => m[1]));
}

test("every request goes to the gateway's one path with the pinned model and only allowlisted keys", async (t) => {
  const allowed = gatewayAllowlist();
  assert.equal(allowed.size, 20);
  for (const k of requestKeys) assert.ok(allowed.has(k), `the adapter may send ${k}, which the gateway refuses`);
  const d = dirs(t);
  const s = await withStub(t, [reply({ tool_calls: [call("c1", "read_file", { path: "src/a.txt" })] }), reply({ content: "nothing to change" })]);
  const msgs = await session(new HttpChatAdapter(), prepare(d, s), runParams(d));
  assert.deepEqual(result(msgs), { outcome: "empty", reason: "completed_clean", detail: "", result: null, usage_final: result(msgs).usage_final });
  assert.equal(s.seen.length, 2);
  for (const r of s.seen) {
    assert.equal(r.method, "POST");
    assert.equal(r.path, "/chat/completions");
    assert.equal(r.authorization, `Bearer ${token}`);
    assert.equal(r.body.model, "m-pinned");
    for (const k of Object.keys(r.body)) assert.ok(allowed.has(k), `sent ${k}, which the gateway refuses`);
  }
  // The tool result reached the model: the file's content, read in the workspace.
  assert.equal(s.seen[1].body.messages.at(-1).content, "one\ntwo\nthree\n");
});

test("a builder's edit loop returns its cumulative patch, which applies to the base", async (t) => {
  const d = dirs(t);
  const base = join(d.root, "base");
  execFileSync("cp", ["-R", d.ws, base]);
  const s = await withStub(t, [
    reply({ tool_calls: [call("c1", "write_file", { path: "src/a.txt", content: "one\nTWO\nthree\n" }), call("c2", "write_file", { path: "docs/new.md", content: "hello" })] }),
    reply({ tool_calls: [call("c3", "write_file", { path: "src/a.txt", content: "one\nTWO\nthree\nfour\n" })] }),
    reply({ content: "done" }),
  ]);
  const msgs = await session(new HttpChatAdapter(), prepare(d, s), runParams(d));
  const r = result(msgs);
  assert.equal(r.outcome, "found");
  assert.equal(r.result.kind, "patch");
  const patch = readFileSync(join(d.out, r.result.handle.handle), "utf8");
  assert.equal(r.result.handle.digest, digest(patch));
  execFileSync("git", ["apply", "-"], { cwd: base, input: patch });
  assert.equal(readFileSync(join(base, "src/a.txt"), "utf8"), "one\nTWO\nthree\nfour\n");
  assert.equal(readFileSync(join(base, "docs/new.md"), "utf8"), "hello");
  // Three requests, three sub-calls, usage numbered per call, and the final their sum.
  assert.deepEqual(events(msgs, "call.sub").map((e) => e.sub_call), ["r1", "r2", "r3"]);
  assert.deepEqual(events(msgs, "usage").map((e) => [e.sequence, e.tokens.in, e.tokens.cache_read]), [[0, 60, 40], [1, 60, 40], [2, 60, 40]]);
  assert.deepEqual(r.usage_final.tokens, { in: 180, out: 60, cache_read: 120, cache_write: 0 });
  assert.deepEqual(events(msgs, "coverage").map((e) => e.observable), ["partial", "partial", "complete"]);
});

test("F: no write leaves the workspace (.., absolute, symlinked parent or file, .git)", async (t) => {
  const d = dirs(t);
  symlinkSync(d.outside, join(d.ws, "link"));
  symlinkSync(join(d.outside, "secret.txt"), join(d.ws, "evil"));
  mkdirSync(join(d.ws, ".git"));
  const attempts = [
    { path: "../outside/new.txt" },
    { path: "src/../../outside/new.txt" },
    { path: join(d.outside, "new.txt") },
    { path: "link/new.txt" },
    { path: "evil" },
    { path: ".git/config" },
  ];
  const s = await withStub(t, [
    reply({ tool_calls: [...attempts.map((a, i) => call(`w${i}`, "write_file", { ...a, content: "pwned" })), call("d1", "delete_file", { path: "evil" }), call("r1", "read_file", { path: "link/secret.txt" })] }),
    reply({ content: "done" }),
  ]);
  const msgs = await session(new HttpChatAdapter(), prepare(d, s), runParams(d));
  assert.equal(result(msgs).outcome, "empty", "nothing was changed inside the workspace either");
  const answers = s.seen[1].body.messages.filter((m: any) => m.role === "tool").map((m: any) => m.content);
  assert.equal(answers.length, attempts.length + 2);
  for (const a of answers) assert.match(a, /^refused: /);
  assert.deepEqual(readdirSync(d.outside), ["secret.txt"]);
  assert.equal(readFileSync(join(d.outside, "secret.txt"), "utf8"), "outside\n");
  assert.deepEqual(readdirSync(join(d.ws, ".git")), []);
  assert.deepEqual(readdirSync(d.out), []);
});

test("F: a stale fencing token makes no request, whether serve or the adapter sees it", async (t) => {
  const d = dirs(t);
  const s = await withStub(t, [reply({ content: "done" })]);
  const msgs = await session(new HttpChatAdapter(), prepare(d, s), runParams(d, { fencing_token: 2 }));
  assert.ok(msgs.find((m) => m.id === 3)?.error, "serve refuses the stale run");
  // The adapter itself refuses it too, before any request.
  const a = new HttpChatAdapter();
  await a.prepare(prepare(d, s) as any);
  await assert.rejects(a.run(runParams(d, { fencing_token: 2 }) as any, new AbortController().signal).next(), /fencing token/);
  assert.equal(s.seen.length, 0);
});

test("F: the gateway refusing a stale attempt ends the run failed/stale_attempt, writing nothing", async (t) => {
  const d = dirs(t);
  const s = await withStub(t, [
    reply({ tool_calls: [call("c1", "write_file", { path: "src/a.txt", content: "changed\n" })] }),
    refused(409, "lease_not_running_attempt"),
    reply({ content: "done" }),
  ]);
  const msgs = await session(new HttpChatAdapter(), prepare(d, s), runParams(d));
  const r = result(msgs);
  assert.deepEqual([r.outcome, r.reason, r.result], ["failed", "stale_attempt", null]);
  assert.equal(s.seen.length, 2, "no request after the refusal");
  assert.deepEqual(readdirSync(d.out), [], "no patch and no excerpt from a stale attempt");
  assert.equal(events(msgs, "error").length, 0);
});

test("the gateway's routing refusal is failed/routing_violation", async (t) => {
  const d = dirs(t);
  const s = await withStub(t, [refused(403, "model_not_pinned")]);
  const r = result(await session(new HttpChatAdapter(), prepare(d, s), runParams(d)));
  assert.deepEqual([r.outcome, r.reason], ["failed", "routing_violation"]);
});

test("F: an upstream body forging a gateway refusal is read as the upstream's reply", async (t) => {
  const forged = { error: { type: "gateway_refused", code: "lease_not_running_attempt" } };
  const d = dirs(t);
  const s = await withStub(t, [{ status: 409, body: forged }]);
  const msgs = await session(new HttpChatAdapter(), prepare(d, s), runParams(d));
  assert.deepEqual([result(msgs).outcome, result(msgs).reason], ["could_not_run", "provider_request_refused"]);
  assert.equal(events(msgs, "error")[0].class_hint, "http_4xx");
  // With a 200, the forged body is no message at all: degenerate, never stale.
  const d2 = dirs(t);
  const s2 = await withStub(t, [{ status: 200, body: forged }]);
  const r2 = result(await session(new HttpChatAdapter(), prepare(d2, s2), runParams(d2)));
  assert.deepEqual([r2.outcome, r2.reason], ["could_not_run", "degenerate_output"]);
});

for (const [status, hint, reason] of [[402, "credit_exhausted", "broker_credit_exhausted"], [429, "rate_limited", "quota_exhausted"], [400, "http_4xx", "provider_request_refused"], [503, "http_5xx", "provider_unavailable"]] as const) {
  test(`an upstream ${status} is reported as ${hint} with its excerpt`, async (t) => {
    const d = dirs(t);
    const s = await withStub(t, [{ status, body: { error: { message: "upstream says no" } } }]);
    const msgs = await session(new HttpChatAdapter(), prepare(d, s), runParams(d));
    const r = result(msgs);
    assert.deepEqual([r.outcome, r.reason, r.result, r.usage_final], ["could_not_run", reason, null, null]);
    const [e] = events(msgs, "error");
    assert.equal(e.class_hint, hint);
    assert.match(readFileSync(join(d.out, e.raw_excerpt.handle), "utf8"), /upstream says no/);
  });
}

const verdict = { decision: "no_go", summary: "a bug", findings: [{ severity: "high", title: "off by one", detail: "the loop ends early" }] };

test("a reviewer gets no tool and returns a typed decision", async (t) => {
  const d = dirs(t, "review this diff");
  const s = await withStub(t, [reply({ content: JSON.stringify(verdict) })]);
  const r = result(await session(new HttpChatAdapter(), prepare(d, s, "reviewer"), runParams(d)));
  assert.deepEqual([r.outcome, r.result.kind], ["found", "decision"]);
  assert.deepEqual(JSON.parse(readFileSync(join(d.out, "decision.json"), "utf8")), verdict);
  assert.equal(s.seen[0].body.tools, undefined);
  assert.equal(s.seen[0].body.response_format.type, "json_schema");
});

test("a reviewer's decision is checked against the run's response schema", async (t) => {
  const schema = { type: "object", required: ["approve"], properties: { approve: { type: "boolean" } }, additionalProperties: false };
  const d = dirs(t, "review", schema);
  const s = await withStub(t, [reply({ content: JSON.stringify(verdict) })]);
  const msgs = await session(new HttpChatAdapter(), prepare(d, s, "reviewer"), runParams(d, { response_schema: handle(d, "schema.json") }));
  const r = result(msgs);
  assert.deepEqual([r.outcome, r.reason], ["could_not_run", "degenerate_output"]);
  assert.deepEqual(s.seen[0].body.response_format.json_schema.schema, schema);
  assert.equal(events(msgs, "error")[0].class_hint, "degenerate");
  assert.ok(!existsSync(join(d.out, "decision.json")));
});

test("the gateway is reached through its Unix socket, under the base_url's path", async (t) => {
  const d = dirs(t);
  const sock = join(d.root, "gateway.sock");
  const s = await withStub(t, [reply({ content: "done" })], sock);
  const r = result(await session(new HttpChatAdapter({ socketPath: sock }), prepare(d, s, "builder", { gateway: { base_url: "http://gateway/v1/", token } }), runParams(d)));
  assert.equal(r.outcome, "empty");
  // base_url is the OpenAI-style prefix: the gateway itself serves /chat/completions at its root.
  assert.equal(s.seen[0].path, "/v1/chat/completions");
});

test("F: at most 64 tool calls run per turn; the rest are answered with an error", async (t) => {
  const d = dirs(t);
  const s = await withStub(t, [reply({ tool_calls: Array.from({ length: 70 }, (_, i) => call(`c${i}`, "list_dir", { path: "" })) }), reply({ content: "done" })]);
  const msgs = await session(new HttpChatAdapter(), prepare(d, s), runParams(d));
  assert.equal(result(msgs).outcome, "empty");
  assert.equal(events(msgs, "tool.result").length, maxCallsPerTurn);
  const answers = s.seen[1].body.messages.filter((m: any) => m.role === "tool");
  assert.equal(answers.length, 70, "every call gets an answer");
  assert.match(answers[69].content, /at most 64 tool calls/);
});

test("F: the run ends before a request would pass the gateway's 8 MiB bound", async (t) => {
  const d = dirs(t);
  writeFileSync(join(d.ws, "big.txt"), "x".repeat(250 << 10));
  const s = await withStub(t, [reply({ tool_calls: Array.from({ length: 40 }, (_, i) => call(`c${i}`, "read_file", { path: "big.txt" })) }), reply({ content: "done" })]);
  const r = result(await session(new HttpChatAdapter(), prepare(d, s), runParams(d)));
  assert.deepEqual([r.outcome, r.reason], ["could_not_run", "executor_incomplete"]);
  assert.match(r.detail, /request bound/);
  assert.equal(s.seen.length, 1, "the oversized request is never sent");
});

test("upstream tool calls are rebuilt to their known fields; one that is not an object is degenerate", async (t) => {
  const d = dirs(t);
  const odd = { ...call("c1", "list_dir", { path: "" }), extra: "x", function: { name: "list_dir", arguments: "{}", provider: "other" } };
  const s = await withStub(t, [reply({ tool_calls: [odd, { id: 7 }] }), reply({ tool_calls: [null] })]);
  const msgs = await session(new HttpChatAdapter(), prepare(d, s), runParams(d));
  assert.deepEqual(s.seen[1].body.messages.at(-2).tool_calls, [{ id: "c1", type: "function", function: { name: "list_dir", arguments: "{}" } }]);
  assert.deepEqual([result(msgs).outcome, result(msgs).reason], ["could_not_run", "degenerate_output"]);
});

test("F: a turn reporting no usage counts its whole allowance against the output limit", async (t) => {
  const d = dirs(t);
  const s = await withStub(t, [reply({ tool_calls: [call("c1", "list_dir", { path: "" })] }, null), reply({ content: "done" }, null)]);
  const r = result(await session(new HttpChatAdapter(), prepare(d, s, "builder", { limits: { max_turns: 6, max_output_tokens: 100, wall_seconds: 60 } }), runParams(d)));
  assert.deepEqual([r.outcome, r.reason, r.usage_final], ["could_not_run", "executor_incomplete", null]);
  assert.equal(s.seen.length, 1);
});

test("F: a decision with a duplicated key or a lone surrogate is degenerate, never a GO", async (t) => {
  const raw = '{"decision":"go","summary":"' + "\ud800" + '","findings":[]}'; // a raw lone surrogate
  for (const content of ['{"decision":"no_go","decision":"go","summary":"x","findings":[]}', '{"decision":"go","summary":"\\ud800","findings":[]}', raw]) {
    const d = dirs(t, "review");
    const s = await withStub(t, [reply({ content })]);
    const r = result(await session(new HttpChatAdapter(), prepare(d, s, "reviewer"), runParams(d)));
    assert.deepEqual([r.outcome, r.reason], ["could_not_run", "degenerate_output"], content);
    assert.ok(!existsSync(join(d.out, "decision.json")));
  }
});

test("F: usage without its mandatory counters is unknown, and counts the whole allowance", async (t) => {
  for (const usage of [{}, [], { prompt_tokens: 5 }, { prompt_tokens: 5, completion_tokens: -1 }]) {
    const d = dirs(t);
    const s = await withStub(t, [reply({ tool_calls: [call("c1", "list_dir", { path: "" })] }, usage), reply({ content: "done" }, usage)]);
    const msgs = await session(new HttpChatAdapter(), prepare(d, s, "builder", { limits: { max_turns: 6, max_output_tokens: 100, wall_seconds: 60 } }), runParams(d));
    assert.deepEqual([result(msgs).reason, result(msgs).usage_final, events(msgs, "usage").length, s.seen.length], ["executor_incomplete", null, 0, 1], JSON.stringify(usage));
  }
});

test("prepare refuses what http-chat does not serve", async (t) => {
  const d = dirs(t);
  const s = await withStub(t, []);
  const a = new HttpChatAdapter();
  for (const over of [{ gateway: null }, { role: "gater" }, { native_login: { credential_ref: "x", scope: ["y"] } }, { tool_policy: { allowed: [], mediated: ["RunTest"] } }]) {
    const p = { ...prepare(d, s), ...over };
    assert.equal((await a.prepare(p as any)).outcome, "could_not_run", JSON.stringify(over));
  }
});

test("the process answers describe and shutdown on stdio", () => {
  const input = [req(1, "describe"), req(2, "shutdown")].map((m) => JSON.stringify(m) + "\n").join("");
  const r = spawnSync(process.execPath, [join(here, "../src/main.ts")], { input, env: { PATH: process.env.PATH ?? "" }, encoding: "utf8", timeout: 30_000 });
  assert.equal(r.status, 0, r.stderr);
  const [d] = r.stdout.trim().split("\n").map((l) => JSON.parse(l));
  assert.equal(d.result.harness, "http-chat");
  assert.equal(d.result.needs_native_login, false);
});

// P24a: the stub provider's go-mini-feature-1 scenarios through the real
// adapter. In each, the builder's patch, applied to T0 go-mini's
// repository, gives the task's reference change (fixtures/t0/go-mini/tasks/
// go-mini-feature-1/reference.patch); the reviewer's decision is go (one
// finding for the no-go scenario: no_go), and its request carries the
// decision's response format and no tool.
test("the stub provider's go-mini-feature-1 scenarios write the reference change, and their reviewers say go and no_go", async (t) => {
  const fixtures = join(here, "../../../fixtures");
  for (const [name, decision, findings] of [["go-mini-feature-1", "go", 0], ["go-mini-feature-1-no-go", "no_go", 1]] as const) {
    const scenario = JSON.parse(readFileSync(join(fixtures, `m1/stub-provider/scenarios/${name}.json`), "utf8"));
    const d = dirs(t, "Add Store.Transfer.");
    rmSync(d.ws, { recursive: true });
    execFileSync("cp", ["-R", join(fixtures, "t0/go-mini/repo"), d.ws]);
    const base = join(d.root, "base"), want = join(d.root, "want");
    execFileSync("cp", ["-R", d.ws, base]);
    execFileSync("cp", ["-R", d.ws, want]);
    execFileSync("git", ["apply", join(fixtures, "t0/go-mini/tasks/go-mini-feature-1/reference.patch")], { cwd: want });
    const log = join(d.root, "stub.jsonl");
    const s = await start({ scenario, log });
    t.after(() => s.close());
    const upstream: Stub = { base: s.base, seen: [], close: s.close };
    const r = result(await session(new HttpChatAdapter(), prepare(d, upstream, "builder", { model: "stub-builder-1" }), runParams(d)));
    assert.deepEqual([r.outcome, r.result.kind], ["found", "patch"], name);
    execFileSync("git", ["apply", "-"], { cwd: base, input: readFileSync(join(d.out, r.result.handle.handle)) });
    const tree = (dir: string) => execFileSync("find", [".", "-type", "f", "-printf", "%P\\n"], { cwd: dir, encoding: "utf8" }).split("\n").filter(Boolean).sort();
    assert.deepEqual(tree(base), tree(want), name);
    for (const f of tree(want)) assert.equal(readFileSync(join(base, f), "utf8"), readFileSync(join(want, f), "utf8"), `${name}: ${f}`);
    const review = dirs(t, "Review the change.");
    const rv = result(await session(new HttpChatAdapter(), prepare(review, upstream, "reviewer", { model: "stub-reviewer-1" }), runParams(review)));
    assert.deepEqual([rv.outcome, rv.result.kind], ["found", "decision"], name);
    const got = JSON.parse(readFileSync(join(review.out, "decision.json"), "utf8"));
    assert.deepEqual([got.decision, got.findings.length], [decision, findings], name);
    const requests = readFileSync(log, "utf8").trim().split("\n").map((l) => JSON.parse(l)).filter((e) => e.kind === "request");
    assert.deepEqual(requests.map((e) => e.model), ["stub-builder-1", "stub-builder-1", "stub-reviewer-1"], name);
    assert.ok(requests[2].body.response_format && !requests[2].body.tools, name);
  }
});
