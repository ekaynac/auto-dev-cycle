// SPDX-License-Identifier: Apache-2.0
//
// The stub provider (M1 slice plan §2.6, P24): an OpenAI-compatible chat
// server, scripted per scenario, the upstream of the real gateway and the
// real http-chat adapter in stub mode. It has no dependency beyond Node.
//
//   node server.mjs --scenario FILE [--listen HOST:PORT|SOCKET] [--log FILE] [--barriers DIR]
//
// It prints "listening <base URL>" once ready (the base URL ends in /v1). It
// listens on loopback only (127.0.0.1, ::1 or localhost) or on a Unix socket
// (a stale one is replaced).
//
// A scenario: {"name", "description"?, "usage_query"?: bool, "calls": {MODEL:
// SCRIPT}}. The request's model picks its script, one of:
//   - {"persistent": REPLY}: the reply to every call;
//   - {"replies": [REPLY, ...]}: one per call of the model, in order;
//   - {"turns": [REPLY, ...]}: one per position in the conversation (the
//     number of assistant messages the request carries), so each attempt,
//     which starts a conversation afresh, replays the same sequence.
// A call past the list is refused with a 500 that names the scenario. A
// REPLY has exactly one form:
//   - {"read": [PATH], "write": {PATH: CONTENT}, "delete": [PATH]} (any of
//     them): tool calls (read_file, write_file, delete_file) as the model
//     sends them, reads first, then writes in path order, then deletes;
//   - {"say": TEXT}: an assistant message with no tool call (a builder's
//     summary, which ends its edit loop);
//   - {"decision": OBJECT}: a reviewer's decision, its JSON as the content;
//   - {"body": ANY}: the body, as is;
// and, optionally:
//   - "usage": {"prompt_tokens", "completion_tokens"}, or null for none
//     (default 100 and 20 for a 2xx status, none otherwise);
//   - "status": the HTTP status (default 200);
//   - "stall_ms": a wait before anything is sent, headers included;
//   - "trickle_ms": the body written one byte every so many ms;
//   - "partial": the body cut after so many bytes, the connection reset;
//   - "barrier": a name: the server logs the request as held and holds the
//     reply until DIR/<name>.release exists, at most "barrier_timeout_ms"
//     (default 120 s), then answers 500.
// A scenario with any other key, or a reply without exactly one form, is
// refused at start. GET /v1/usage/{request_id} answers the usage a reply
// reported (404 for an unknown id, and for every id when "usage_query" is
// false). GET /v1/usage?call=ID answers a call's usage (#226 entry 121): the
// gateway stamps each request with its call (X-Adc-Call) and the request's
// count sequence (X-Adc-Call-Seq); the answer is {call, requests, pending,
// usage}: the sum over the call's requests whose usage was set, each once (a
// request held at a barrier counts once released), and how many are still
// being served with none yet (pending: an answer is complete only at 0). 404
// for a call with no request, and for every call when "usage_query" is
// false. The log is JSONL: each request (its whole body, and its call and
// sequence), each usage served ("delivered": "body", or "cut" when the body
// was cut before it), each barrier, and each usage query with its status.
// It holds at most maxLog bytes: past that, a request is refused with a 500
// and one "log_full" entry is written, so no body is ever truncated.

import { appendFileSync, existsSync, lstatSync, readFileSync, unlinkSync } from "node:fs";
import { createServer } from "node:http";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const maxBody = 8 << 20;
const scenarioKeys = new Set(["name", "description", "usage_query", "calls"]);
const forms = [["read", "write", "delete"], ["say"], ["decision"], ["body"]];
const replyKeys = new Set([...forms.flat(), "usage", "status", "stall_ms", "trickle_ms", "partial", "barrier", "barrier_timeout_ms"]);
const loopback = new Set(["127.0.0.1", "::1", "localhost"]);

/** check refuses a scenario with an unknown key or a malformed reply. */
export function check(scenario) {
  const fail = (what) => {
    throw new Error(`scenario ${scenario?.name ?? "?"}: ${what}`);
  };
  if (typeof scenario !== "object" || scenario === null) fail("not an object");
  for (const k of Object.keys(scenario)) if (!scenarioKeys.has(k)) fail(`unknown key ${k}`);
  if (typeof scenario.name !== "string" || scenario.name === "") fail("no name");
  if (typeof scenario.calls !== "object" || scenario.calls === null) fail("no calls");
  const count = (v) => Number.isInteger(v) && v >= 0;
  for (const [model, script] of Object.entries(scenario.calls)) {
    const kinds = Object.keys(script ?? {});
    if (kinds.length !== 1 || !["persistent", "replies", "turns"].includes(kinds[0])) fail(`${model}: a script is one of persistent, replies or turns`);
    const replies = kinds[0] === "persistent" ? [script.persistent] : script[kinds[0]];
    if (!Array.isArray(replies) || replies.length === 0) fail(`${model}: ${kinds[0]} is not a list of replies`);
    replies.forEach((r, i) => {
      const at = `${model} reply ${i}`;
      if (typeof r !== "object" || r === null) fail(`${at}: not an object`);
      for (const k of Object.keys(r)) if (!replyKeys.has(k)) fail(`${at}: unknown key ${k}`);
      if (forms.filter((f) => f.some((k) => k in r)).length !== 1) fail(`${at}: not exactly one of read/write/delete, say, decision or body`);
      if ("read" in r && !(Array.isArray(r.read) && r.read.every((p) => typeof p === "string"))) fail(`${at}: read is not a list of paths`);
      for (const k of ["stall_ms", "trickle_ms", "partial", "barrier_timeout_ms"]) if (k in r && !count(r[k])) fail(`${at}: ${k} is not a count`);
      if ("status" in r && !(Number.isInteger(r.status) && r.status >= 100 && r.status <= 599)) fail(`${at}: status is not an HTTP status`);
      if ("barrier" in r && !/^[a-z0-9-]{1,64}$/.test(r.barrier)) fail(`${at}: barrier is not a name`);
      if (r.usage != null && !(count(r.usage.prompt_tokens) && count(r.usage.completion_tokens) && Object.keys(r.usage).length === 2)) {
        fail(`${at}: usage is not {prompt_tokens, completion_tokens}`);
      }
    });
  }
}

/** body is the chat-completions body of r, request id id. */
export function body(r, id) {
  if ("body" in r) return r.body;
  const message = { role: "assistant", content: null };
  if (r.read || r.write || r.delete) {
    const reads = (r.read ?? []).map((path, i) => ({ id: `${id}-r${i}`, type: "function", function: { name: "read_file", arguments: JSON.stringify({ path }) } }));
    const writes = Object.keys(r.write ?? {}).sort().map((path, i) => ({ id: `${id}-w${i}`, type: "function",
      function: { name: "write_file", arguments: JSON.stringify({ path, content: r.write[path] }) } }));
    const deletes = (r.delete ?? []).map((path, i) => ({ id: `${id}-d${i}`, type: "function", function: { name: "delete_file", arguments: JSON.stringify({ path }) } }));
    message.tool_calls = [...reads, ...writes, ...deletes];
  } else if ("decision" in r) {
    message.content = JSON.stringify(r.decision);
  } else {
    message.content = r.say;
  }
  const ok = (r.status ?? 200) >= 200 && (r.status ?? 200) < 300;
  const usage = r.usage === undefined ? (ok ? { prompt_tokens: 100, completion_tokens: 20 } : null) : r.usage;
  return { id, object: "chat.completion", model: "stub", choices: [{ index: 0, message, finish_reason: message.tool_calls ? "tool_calls" : "stop" }],
    ...(usage ? { usage } : {}) };
}

/** read reads the whole request (draining past the bound) and parses it. */
async function read(req) {
  const chunks = [];
  let n = 0;
  for await (const c of req) {
    n += c.length;
    if (n <= maxBody) chunks.push(c);
  }
  if (n > maxBody) throw Object.assign(new Error(`the request is ${n} bytes, past the bound of ${maxBody}`), { status: 413 });
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

/** start checks and serves scenario (an object) and resolves to {base, close}. */
export async function start({ scenario, listen = "127.0.0.1:0", log, barriers = ".", maxLog = 256 << 20 }) {
  check(scenario);
  const calls = new Map(); // model -> calls served
  const usages = new Map(); // request id -> usage
  const byCall = new Map(); // X-Adc-Call -> its request ids
  const inflight = new Set(); // request ids of a call still being served (a barrier's included)
  let served = 0, logged = 0, full = false, closed = false;
  const record = (entry) => {
    if (!log) return true;
    const line = JSON.stringify({ at: new Date().toISOString(), ...entry }) + "\n";
    if (full || logged + Buffer.byteLength(line) > maxLog) {
      if (!full) appendFileSync(log, JSON.stringify({ at: new Date().toISOString(), kind: "log_full", logged }) + "\n");
      full = true;
      return false;
    }
    appendFileSync(log, line);
    logged += Buffer.byteLength(line);
    return true;
  };
  const send = (res, status, doc) => {
    res.writeHead(status, { "content-type": "application/json" });
    res.end(JSON.stringify(doc));
  };
  const server = createServer(async (req, res) => {
    let serving = null; // the chat request being served: pending in its call's usage until it ends
    try {
      const query = req.method === "GET" && /^\/v1\/usage\/([^/]+)$/.exec(req.url ?? "");
      if (query) {
        const id = decodeURIComponent(query[1]);
        const u = scenario.usage_query === false ? undefined : usages.get(id);
        record({ kind: "usage_query", request_id: id, status: u ? 200 : 404 });
        return u ? send(res, 200, { request_id: id, usage: u }) : send(res, 404, { error: { message: "no such request" } });
      }
      const byCallQuery = req.method === "GET" && /^\/v1\/usage\?call=([^&]+)$/.exec(req.url ?? "");
      if (byCallQuery) {
        const call = decodeURIComponent(byCallQuery[1]);
        const ids = scenario.usage_query === false ? undefined : byCall.get(call);
        record({ kind: "usage_query", call, status: ids ? 200 : 404 });
        if (!ids) return send(res, 404, { error: { message: "no such call" } });
        const usage = { prompt_tokens: 0, completion_tokens: 0 };
        let pending = 0;
        for (const id of ids) {
          const u = usages.get(id);
          if (u) for (const k of Object.keys(usage)) usage[k] += u[k];
          else if (inflight.has(id)) pending++;
        }
        return send(res, 200, { call, requests: ids.length, pending, usage });
      }
      if (req.method !== "POST" || req.url !== "/v1/chat/completions") return send(res, 404, { error: { message: "not found" } });
      const doc = await read(req);
      const id = `req-${++served}`;
      const script = scenario.calls[doc.model];
      const n = calls.get(doc.model) ?? 0;
      calls.set(doc.model, n + 1);
      const turn = (doc.messages ?? []).filter((m) => m?.role === "assistant").length;
      const adc = { call: req.headers["x-adc-call"], seq: req.headers["x-adc-call-seq"] };
      if (!record({ kind: "request", request_id: id, model: doc.model, call: n, turn, adc_call: adc.call ?? null, adc_seq: adc.seq ?? null, body: doc })) {
        return send(res, 500, { error: { message: `scenario ${scenario.name}: the log is full` } });
      }
      if (typeof adc.call === "string") {
        byCall.set(adc.call, [...(byCall.get(adc.call) ?? []), id]);
        inflight.add((serving = id));
      }
      const r = script?.persistent ?? script?.replies?.[n] ?? script?.turns?.[turn];
      if (!r) return send(res, 500, { error: { message: `scenario ${scenario.name}: no reply for call ${n} (turn ${turn}) of ${doc.model}` } });
      if (r.barrier) {
        record({ kind: "barrier", request_id: id, barrier: r.barrier });
        process.stderr.write(`barrier ${id}\n`);
        const deadline = Date.now() + (r.barrier_timeout_ms ?? 120_000);
        for (;;) {
          if (closed) return res.destroy();
          if (existsSync(join(barriers, `${r.barrier}.release`))) break;
          if (Date.now() > deadline) return send(res, 500, { error: { message: `scenario ${scenario.name}: barrier ${r.barrier} was never released` } });
          await sleep(20);
        }
      }
      if (r.stall_ms) await sleep(r.stall_ms);
      const b = body(r, id);
      const bytes = Buffer.from(JSON.stringify(b));
      const cut = r.partial !== undefined;
      if (b.usage) usages.set(id, b.usage);
      res.writeHead(r.status ?? 200, { "content-type": "application/json" });
      if (cut) {
        // The cut bytes reach the client before the connection is dropped.
        await new Promise((done) => res.write(bytes.subarray(0, r.partial), done));
        // The usage was not delivered in the body: only the query has it.
        if (b.usage) record({ kind: "usage", request_id: id, usage: b.usage, delivered: "cut" });
        return res.destroy();
      }
      if (r.trickle_ms) {
        for (const byte of bytes) {
          res.write(Buffer.of(byte));
          await sleep(r.trickle_ms);
        }
        res.end();
      } else {
        res.end(bytes);
      }
      if (b.usage) record({ kind: "usage", request_id: id, usage: b.usage, delivered: "body" });
    } catch (err) {
      if (!res.headersSent) send(res, err.status ?? 400, { error: { message: String(err.message ?? err) } });
    } finally {
      inflight.delete(serving);
    }
  });
  const port = /^(.*):(\d+)$/.exec(listen);
  if (port) {
    const host = port[1].replace(/^\[(.*)\]$/, "$1");
    if (!loopback.has(host)) throw new Error(`--listen ${listen}: the stub provider listens on loopback only (127.0.0.1, ::1 or localhost)`);
    await new Promise((r, j) => server.once("error", j).listen(Number(port[2]), host, r));
  } else {
    if (!listen.includes("/")) throw new Error(`--listen ${listen}: neither a loopback HOST:PORT nor a socket path`);
    if (existsSync(listen) && lstatSync(listen).isSocket()) unlinkSync(listen);
    await new Promise((r, j) => server.once("error", j).listen(listen, r));
  }
  const host = port && (port[1].includes(":") && !port[1].startsWith("[") ? `[${port[1]}]` : port[1]);
  const base = port ? `http://${host}:${server.address().port}/v1` : "http://stub/v1";
  return { base, close: () => new Promise((r) => { closed = true; server.closeAllConnections(); server.close(() => r()); }) };
}

function flag(args, name) {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const file = flag(args, "--scenario");
  if (!file) {
    process.stderr.write("usage: server.mjs --scenario FILE [--listen HOST:PORT|SOCKET] [--log FILE] [--barriers DIR]\n");
    process.exit(2);
  }
  let s;
  try {
    s = await start({ scenario: JSON.parse(readFileSync(file, "utf8")), listen: flag(args, "--listen"), log: flag(args, "--log"), barriers: flag(args, "--barriers") });
  } catch (err) {
    process.stderr.write(`stub provider: ${err.message ?? err}\n`);
    process.exit(2);
  }
  process.stdout.write(`listening ${s.base}\n`);
  for (const sig of ["SIGTERM", "SIGINT"]) process.on(sig, () => s.close().then(() => process.exit(0)));
}
