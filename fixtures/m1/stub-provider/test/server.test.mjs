// SPDX-License-Identifier: Apache-2.0
// The stub provider's own tests (P24a): each reply form, each fault, the
// usage query and the log.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { request } from "node:http";
import { start } from "../server.mjs";

async function serve(t, scenario) {
  const dir = mkdtempSync(join(tmpdir(), "adc-stub-"));
  const s = await start({ scenario, log: join(dir, "log.jsonl"), barriers: dir });
  t.after(async () => {
    await s.close();
    rmSync(dir, { recursive: true, force: true });
  });
  return { ...s, dir, log: () => readFileSync(join(dir, "log.jsonl"), "utf8").trim().split("\n").map((l) => JSON.parse(l)) };
}

const chat = (s, model, extra = {}) => fetch(`${s.base}/chat/completions`, { method: "POST", headers: { "content-type": "application/json" },
  body: JSON.stringify({ model, messages: [{ role: "user", content: "hi" }], stream: false, ...extra }) });

test("replies come in order per model, past the list a 500 that names the scenario", async (t) => {
  const s = await serve(t, { name: "order", calls: { b: { replies: [{ say: "one" }, { say: "two" }] }, r: { persistent: { say: "always" } } } });
  const said = async (model) => (await (await chat(s, model)).json()).choices[0].message.content;
  assert.equal(await said("b"), "one");
  assert.equal(await said("r"), "always");
  assert.equal(await said("b"), "two");
  assert.equal(await said("r"), "always");
  const past = await chat(s, "b");
  assert.equal(past.status, 500);
  assert.match((await past.json()).error.message, /scenario order: no reply for call 2 of b/);
  const unknown = await chat(s, "nobody");
  assert.equal(unknown.status, 500);
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

test("the usage query serves each reply's usage, 404 for an unknown id or when disabled; the log holds each request and usage", async (t) => {
  const s = await serve(t, { name: "usage", calls: { b: { persistent: { say: "x", usage: { prompt_tokens: 7, completion_tokens: 3 } } } } });
  const id = (await (await chat(s, "b", { canary: "c-1" })).json()).id;
  const u = await fetch(`${s.base}/usage/${id}`);
  assert.deepEqual(await u.json(), { request_id: id, usage: { prompt_tokens: 7, completion_tokens: 3 } });
  assert.equal((await fetch(`${s.base}/usage/req-99`)).status, 404);
  const log = s.log();
  assert.deepEqual(log.map((e) => [e.kind, e.request_id]), [["request", id], ["usage", id]]);
  assert.equal(log[0].body.canary, "c-1");
  const off = await serve(t, { name: "off", usage_query: false, calls: { b: { persistent: { say: "x" } } } });
  const offID = (await (await chat(off, "b")).json()).id;
  assert.equal((await fetch(`${off.base}/usage/${offID}`)).status, 404);
});

test("faults: a status, a stall, a trickle, a partial body and a barrier", async (t) => {
  const s = await serve(t, { name: "faults", calls: {
    status: { persistent: { body: { error: { message: "no" } }, status: 400 } },
    stall: { persistent: { say: "late", stall_ms: 150 } },
    trickle: { persistent: { say: "slow", trickle_ms: 1 } },
    partial: { persistent: { say: "cut", partial: 10 } },
    barrier: { persistent: { say: "held", barrier: "b1" } } } });
  assert.equal((await chat(s, "status")).status, 400);
  let begin = Date.now();
  assert.equal((await (await chat(s, "stall")).json()).choices[0].message.content, "late");
  assert.ok(Date.now() - begin >= 150);
  begin = Date.now();
  const slow = await (await chat(s, "trickle")).json();
  assert.equal(slow.choices[0].message.content, "slow");
  assert.ok(Date.now() - begin >= 100, "a trickled body arrives slowly");
  await assert.rejects(async () => (await chat(s, "partial")).json());
  let released = false;
  const held = chat(s, "barrier").then(async (r) => {
    assert.ok(released, "the barrier held the reply until its release");
    return (await r.json()).choices[0].message.content;
  });
  await new Promise((r) => setTimeout(r, 100));
  released = true;
  writeFileSync(join(s.dir, "b1.release"), "");
  assert.equal(await held, "held");
});

test("a malformed request is refused, not served", async (t) => {
  const s = await serve(t, { name: "bad", calls: { b: { persistent: { say: "x" } } } });
  const status = await new Promise((resolve, reject) => {
    const req = request(`${s.base}/chat/completions`, { method: "POST" }, (res) => resolve(res.statusCode));
    req.on("error", reject);
    req.end("{not json");
  });
  assert.equal(status, 400);
});
