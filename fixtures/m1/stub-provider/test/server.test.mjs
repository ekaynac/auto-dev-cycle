// SPDX-License-Identifier: Apache-2.0
// The stub provider's own tests (P24a): each script and reply form, each
// fault and its pacing, the usage query, the log and its bound, the
// scenario check and the loopback rule.
import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, lstatSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { request } from "node:http";
import { spawnSync } from "node:child_process";
import { check, start } from "../server.mjs";

async function serve(t, scenario, options = {}) {
  const dir = mkdtempSync(join(tmpdir(), "adc-stub-"));
  const s = await start({ scenario, log: join(dir, "log.jsonl"), barriers: dir, ...options });
  t.after(async () => {
    await s.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const log = join(dir, "log.jsonl");
  return { ...s, dir, log: () => (existsSync(log) ? readFileSync(log, "utf8").trim().split("\n").map((l) => JSON.parse(l)) : []) };
}

// Every client wait is bounded, so a fault that never answers fails its test
// rather than hanging the runner.
const chat = (s, model, extra = {}) => fetch(`${s.base}/chat/completions`, { method: "POST", headers: { "content-type": "application/json" },
  body: JSON.stringify({ model, messages: [{ role: "user", content: "hi" }], stream: false, ...extra }), signal: AbortSignal.timeout(10_000) });

// refused asserts start rejects options with match, closing a server it
// started anyway.
async function refused(options, match, what) {
  let s;
  try {
    s = await start(options);
  } catch (err) {
    assert.match(err.message, match, what);
    return;
  }
  await s.close();
  assert.fail(`${what}: started`);
}

// raw POSTs a chat request with node:http and reports when the headers came,
// how many data events the body took, and whether it ended or was reset.
function raw(s, model) {
  return new Promise((resolve) => {
    const begin = Date.now();
    let responded = false;
    const req = request(`${s.base}/chat/completions`, { method: "POST", headers: { "content-type": "application/json" } }, (res) => {
      responded = true;
      const got = { status: res.statusCode, headersAt: Date.now() - begin, chunks: 0, bytes: 0 };
      res.on("data", (c) => {
        got.chunks++;
        got.bytes += c.length;
      });
      res.on("error", (e) => (got.error = e.code ?? e.message));
      res.on("close", () => resolve({ ...got, complete: res.complete }));
    });
    req.on("error", (e) => responded || resolve({ error: e.code ?? e.message, complete: false }));
    req.setTimeout(10_000, () => req.destroy(new Error("timed out")));
    req.end(JSON.stringify({ model, messages: [], stream: false }));
  });
}

test("replies come in order per model, past the list a 500 that names the scenario", async (t) => {
  const s = await serve(t, { name: "order", calls: { b: { replies: [{ say: "one" }, { say: "two" }] }, r: { persistent: { say: "always" } } } });
  const said = async (model) => (await (await chat(s, model)).json()).choices[0].message.content;
  assert.equal(await said("b"), "one");
  assert.equal(await said("r"), "always");
  assert.equal(await said("b"), "two");
  assert.equal(await said("r"), "always");
  const past = await chat(s, "b");
  assert.equal(past.status, 500);
  assert.match((await past.json()).error.message, /scenario order: no reply for call 2 \(turn 0\) of b/);
  const unknown = await chat(s, "nobody");
  assert.equal(unknown.status, 500);
});

test("a turns script replays its sequence in every attempt, by the conversation's position", async (t) => {
  const s = await serve(t, { name: "turns", calls: { b: { turns: [{ write: { "a.go": "package a\n" } }, { say: "done", status: 402 }] } } });
  const attempt = async () => {
    const messages = [{ role: "user", content: "build" }];
    const first = await (await chat(s, "b", { messages })).json();
    assert.equal(first.choices[0].message.tool_calls[0].function.name, "write_file");
    messages.push(first.choices[0].message, { role: "tool", tool_call_id: first.choices[0].message.tool_calls[0].id, content: "ok" });
    const second = await chat(s, "b", { messages });
    assert.equal(second.status, 402);
    messages.push({ role: "assistant", content: "x" });
    assert.equal((await chat(s, "b", { messages })).status, 500, "past the turns");
  };
  await attempt();
  await attempt();
  assert.deepEqual(s.log().filter((e) => e.kind === "request").map((e) => e.turn), [0, 1, 2, 0, 1, 2]);
});

test("a read is the adapter's read_file calls, before its writes (P24c)", async (t) => {
  const s = await serve(t, { name: "reads", calls: { b: { replies: [{ read: ["AGENTS.md", "b.md"], write: { "a.go": "package a\n" } }] } } });
  const calls = (await (await chat(s, "b")).json()).choices[0].message.tool_calls.map((c) => [c.function.name, JSON.parse(c.function.arguments).path]);
  assert.deepEqual(calls, [["read_file", "AGENTS.md"], ["read_file", "b.md"], ["write_file", "a.go"]]);
  assert.throws(() => check({ name: "x", calls: { b: { replies: [{ read: "AGENTS.md" }] } } }), /read is not a list of paths/);
  assert.throws(() => check({ name: "x", calls: { b: { replies: [{ read: ["a"], say: "done" }] } } }), /not exactly one/);
});

test("a write is the adapter's tool calls, in path order; a decision is its JSON content", async (t) => {
  const s = await serve(t, { name: "forms", calls: {
    b: { replies: [{ write: { "z.go": "package z\n", "a/b.go": "package b\n" }, delete: ["old.go"] }] },
    r: { replies: [{ decision: { decision: "go", summary: "fine", findings: [] }, usage: null }] } } });
  const b = await (await chat(s, "b")).json();
  const calls = b.choices[0].message.tool_calls;
  assert.deepEqual(calls.map((c) => [c.type, c.function.name, JSON.parse(c.function.arguments)]), [
    ["function", "write_file", { path: "a/b.go", content: "package b\n" }],
    ["function", "write_file", { path: "z.go", content: "package z\n" }],
    ["function", "delete_file", { path: "old.go" }]]);
  assert.equal(new Set(calls.map((c) => c.id)).size, 3);
  assert.equal(b.choices[0].finish_reason, "tool_calls");
  assert.deepEqual(b.usage, { prompt_tokens: 100, completion_tokens: 20 });
  const r = await (await chat(s, "r")).json();
  assert.deepEqual(JSON.parse(r.choices[0].message.content), { decision: "go", summary: "fine", findings: [] });
  assert.equal(r.usage, undefined);
});

test("the usage query and the log: what was served, how, and each query", async (t) => {
  const s = await serve(t, { name: "usage", calls: {
    b: { persistent: { say: "x", usage: { prompt_tokens: 7, completion_tokens: 3 } } },
    refused: { persistent: { say: "no", status: 402 } },
    cut: { persistent: { say: "cut", partial: 5 } } } });
  const id = (await (await chat(s, "b", { canary: "c-1" })).json()).id;
  const u = await fetch(`${s.base}/usage/${id}`);
  assert.deepEqual(await u.json(), { request_id: id, usage: { prompt_tokens: 7, completion_tokens: 3 } });
  assert.equal((await fetch(`${s.base}/usage/req-99`)).status, 404);
  const no = await chat(s, "refused");
  assert.equal(no.status, 402);
  assert.equal((await no.json()).usage, undefined, "a reply that is not 2xx carries no usage by default");
  assert.equal((await fetch(`${s.base}/usage/req-2`)).status, 404);
  assert.ok((await raw(s, "cut")).error);
  assert.equal((await fetch(`${s.base}/usage/req-3`)).status, 200, "a cut body's usage is still queryable");
  const log = s.log();
  assert.deepEqual(log.map((e) => [e.kind, e.request_id, e.delivered ?? e.status]), [
    ["request", id, undefined], ["usage", id, "body"], ["usage_query", id, 200], ["usage_query", "req-99", 404],
    ["request", "req-2", undefined], ["usage_query", "req-2", 404],
    ["request", "req-3", undefined], ["usage", "req-3", "cut"], ["usage_query", "req-3", 200]]);
  assert.equal(log[0].body.canary, "c-1");
  const off = await serve(t, { name: "off", usage_query: false, calls: { b: { persistent: { say: "x" } } } });
  const offID = (await (await chat(off, "b")).json()).id;
  assert.equal((await fetch(`${off.base}/usage/${offID}`)).status, 404);
});

test("the usage query by call: the gateway's X-Adc-Call, a cut request counted once released (#226 entry 121)", async (t) => {
  const s = await serve(t, { name: "by-call", calls: {
    b: { persistent: { say: "x", usage: { prompt_tokens: 7, completion_tokens: 3 } } },
    held: { persistent: { say: "held", barrier: "x3", usage: { prompt_tokens: 11, completion_tokens: 5 } } } } });
  const as = (call, seq) => ({ headers: { "content-type": "application/json", "x-adc-call": call, "x-adc-call-seq": String(seq) } });
  const send = (model, call, seq, signal) => fetch(`${s.base}/chat/completions`, { method: "POST", ...as(call, seq), signal: signal ?? AbortSignal.timeout(10_000),
    body: JSON.stringify({ model, messages: [{ role: "user", content: "hi" }] }) });
  const usage = async (call) => {
    const r = await fetch(`${s.base}/usage?call=${encodeURIComponent(call)}`, { signal: AbortSignal.timeout(10_000) });
    return r.status === 200 ? r.json() : r.status;
  };
  for (const [call, seq] of [["cal_1", 0], ["cal_1", 1], ["cal_2", 2]]) assert.equal((await send("b", call, seq)).status, 200);
  assert.equal((await chat(s, "b")).status, 200, "a request with no call is in no call's usage");
  // The gateway is killed mid-request: the client goes, the stub holds the reply at its barrier.
  const cut = new AbortController();
  const held = send("held", "cal_1", 3, cut.signal).catch(() => "cut");
  for (let i = 0; i < 100 && !s.log().some((e) => e.kind === "barrier"); i++) await new Promise((r) => setTimeout(r, 20));
  cut.abort();
  assert.equal(await held, "cut");
  assert.deepEqual(await usage("cal_1"), { call: "cal_1", requests: 3, pending: 1, usage: { prompt_tokens: 14, completion_tokens: 6 } }, "held: not complete");
  writeFileSync(join(s.dir, "x3.release"), "");
  for (let i = 0; i < 100 && (await usage("cal_1")).pending > 0; i++) await new Promise((r) => setTimeout(r, 20));
  const done = { call: "cal_1", requests: 3, pending: 0, usage: { prompt_tokens: 25, completion_tokens: 11 } };
  assert.deepEqual(await usage("cal_1"), done, "released: the cut request counted");
  assert.deepEqual(await usage("cal_1"), done, "counted once");
  assert.deepEqual(await usage("cal_2"), { call: "cal_2", requests: 1, pending: 0, usage: { prompt_tokens: 7, completion_tokens: 3 } });
  assert.equal(await usage("cal_9"), 404);
  const req = s.log().filter((e) => e.kind === "request").map((e) => [e.adc_call, e.adc_seq]);
  assert.deepEqual(req, [["cal_1", "0"], ["cal_1", "1"], ["cal_2", "2"], [null, null], ["cal_1", "3"]]);
  const off = await serve(t, { name: "off", usage_query: false, calls: { b: { persistent: { say: "x" } } } });
  await fetch(`${off.base}/chat/completions`, { method: "POST", ...as("cal_1", 0), body: JSON.stringify({ model: "b", messages: [] }) });
  assert.equal((await fetch(`${off.base}/usage?call=cal_1`)).status, 404, "no usage query: no call's usage");
});

test("faults: a status, a stall before the headers, a trickle byte by byte, a partial body reset", async (t) => {
  const s = await serve(t, { name: "faults", calls: {
    status: { persistent: { body: { error: { message: "no" } }, status: 400 } },
    stall: { persistent: { say: "late", stall_ms: 150 } },
    trickle: { persistent: { say: "slow", trickle_ms: 1 } },
    partial: { persistent: { say: "cut", partial: 10 } } } });
  assert.equal((await chat(s, "status")).status, 400);
  const stall = await raw(s, "stall");
  assert.ok(stall.headersAt >= 140, `no header before the stall: headers at ${stall.headersAt} ms`);
  assert.ok(stall.complete);
  const trickle = await raw(s, "trickle");
  assert.ok(trickle.complete && trickle.chunks > 10, `a trickled body arrives in pieces: ${trickle.chunks}`);
  const partial = await raw(s, "partial");
  assert.equal(partial.complete, false, "a partial body is not ended cleanly");
  assert.equal(partial.bytes, 10);
  assert.ok(partial.error, "the connection is reset");
});

test("a barrier holds its reply until released, logs it, times out, and close() ends a held reply", async (t) => {
  const s = await serve(t, { name: "barrier", calls: {
    held: { persistent: { say: "held", barrier: "b1" } },
    never: { persistent: { say: "never", barrier: "b2", barrier_timeout_ms: 100 } } } });
  let released = false;
  const held = chat(s, "held").then(async (r) => {
    assert.ok(released, "the barrier held the reply until its release");
    return (await r.json()).choices[0].message.content;
  });
  await new Promise((r) => setTimeout(r, 100));
  released = true;
  writeFileSync(join(s.dir, "b1.release"), "");
  assert.equal(await held, "held");
  const never = await chat(s, "never");
  assert.equal(never.status, 500);
  assert.match((await never.json()).error.message, /barrier b2 was never released/);
  assert.deepEqual(s.log().filter((e) => e.kind === "barrier").map((e) => e.barrier), ["b1", "b2"]);
  const other = await serve(t, { name: "close", calls: { b: { persistent: { say: "x", barrier: "b3" } } } });
  const pending = raw(other, "b");
  await new Promise((r) => setTimeout(r, 50));
  const begin = Date.now();
  await other.close();
  assert.ok((await pending).error || !(await pending).complete);
  assert.ok(Date.now() - begin < 1000, "close() does not wait for the barrier");
  // The closed server's held handler has ended: a late release serves nothing.
  writeFileSync(join(other.dir, "b3.release"), "");
  await new Promise((r) => setTimeout(r, 100));
  assert.deepEqual(other.log().map((e) => e.kind), ["request", "barrier"]);
});

test("a malformed request is refused, not served, and so is one past the 8 MiB bound", async (t) => {
  const s = await serve(t, { name: "bad", calls: { b: { persistent: { say: "x" } } } });
  const status = await new Promise((resolve, reject) => {
    const req = request(`${s.base}/chat/completions`, { method: "POST" }, (res) => resolve(res.statusCode));
    req.on("error", reject);
    req.end("{not json");
  });
  assert.equal(status, 400);
  const big = await fetch(`${s.base}/chat/completions`, { method: "POST", body: JSON.stringify({ model: "b", pad: "x".repeat(8 << 20) }) });
  assert.equal(big.status, 413);
  assert.match((await big.json()).error.message, /past the bound/);
  assert.equal(s.log().length, 0, "neither is logged as a request");
});

test("the log is bounded: past it a request is refused and one log_full entry written, no body truncated", async (t) => {
  const s = await serve(t, { name: "full", calls: { b: { persistent: { say: "x", usage: null } } } }, { maxLog: 600 });
  assert.equal((await chat(s, "b", { pad: "y".repeat(200) })).status, 200);
  const refused = await chat(s, "b", { pad: "y".repeat(400) });
  assert.equal(refused.status, 500);
  assert.match((await refused.json()).error.message, /the log is full/);
  assert.equal((await chat(s, "b")).status, 500);
  const log = s.log();
  assert.deepEqual(log.map((e) => e.kind), ["request", "log_full"]);
  assert.equal(log[0].body.pad.length, 200);
});

test("every committed scenario passes the check, named as its file", () => {
  const dir = join(import.meta.dirname, "../scenarios");
  const files = readdirSync(dir).filter((f) => f.endsWith(".json"));
  assert.ok(files.length >= 7);
  for (const f of files) {
    const scenario = JSON.parse(readFileSync(join(dir, f), "utf8"));
    check(scenario);
    assert.equal(`${scenario.name}.json`, f);
  }
});

test("a scenario with an unknown key or a malformed reply is refused at start", async () => {
  const bad = {
    "a misspelt fault": { b: { persistent: { say: "x", stauts: 400 } } },
    "a misspelt stall": { b: { persistent: { say: "x", stall: 150 } } },
    "no form": { b: { persistent: { status: 400 } } },
    "two forms": { b: { persistent: { say: "x", decision: {} } } },
    "an unknown script": { b: { cycle: [{ say: "x" }] } },
    "two scripts": { b: { persistent: { say: "x" }, replies: [{ say: "y" }] } },
    "an empty list": { b: { replies: [] } },
    "a negative stall": { b: { persistent: { say: "x", stall_ms: -1 } } },
    "a malformed usage": { b: { persistent: { say: "x", usage: { prompt: 1 } } } },
    "a bad barrier": { b: { persistent: { say: "x", barrier: "../x" } } },
  };
  for (const [name, calls] of Object.entries(bad)) {
    await refused({ scenario: { name, calls } }, /scenario/, name);
  }
  await refused({ scenario: { name: "k", calls: {}, usage_qurey: false } }, /unknown key usage_qurey/, "a misspelt scenario key");
  await refused({ scenario: { calls: {} } }, /no name/, "no name");
});

test("it listens on loopback only, or on a socket that replaces a stale one", async (t) => {
  const s = await serve(t, { name: "lo", calls: { b: { persistent: { say: "x" } } } });
  assert.match(s.base, /^http:\/\/127\.0\.0\.1:\d+\/v1$/);
  for (const listen of ["0.0.0.0:0", ":0", "192.0.2.1:0", "nowhere"]) {
    await refused({ scenario: { name: "lo", calls: {} }, listen }, /loopback/, listen);
  }
  const dir = mkdtempSync(join(tmpdir(), "adc-stub-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const path = join(dir, "s.sock");
  // A process that dies listening leaves its socket behind.
  spawnSync(process.execPath, ["-e", "require('node:net').createServer().listen(process.argv[1], () => process.kill(process.pid, 'SIGKILL'))", path]);
  assert.ok(lstatSync(path).isSocket(), "a stale socket");
  const sock = await start({ scenario: { name: "sock", calls: { b: { persistent: { say: "over a socket" } } } }, listen: path });
  t.after(() => sock.close());
  const said = await new Promise((resolve, reject) => {
    const req = request({ socketPath: path, path: "/v1/chat/completions", method: "POST" }, (res) => {
      let text = "";
      res.on("data", (c) => (text += c));
      res.on("end", () => resolve(JSON.parse(text).choices[0].message.content));
    });
    req.on("error", reject);
    req.end(JSON.stringify({ model: "b", messages: [] }));
  });
  assert.equal(said, "over a socket");
});
