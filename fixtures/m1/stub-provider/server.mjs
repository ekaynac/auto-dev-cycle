// SPDX-License-Identifier: Apache-2.0
//
// The stub provider (M1 slice plan §2.6, P24): an OpenAI-compatible chat
// server, scripted per scenario, the upstream of the real gateway and the
// real http-chat adapter in stub mode. It has no dependency beyond Node.
//
//   node server.mjs --scenario FILE [--listen HOST:PORT|SOCKET] [--log FILE] [--barriers DIR]
//
// It prints "listening <base URL>" once ready (the base URL ends in /v1).
//
// A scenario: {"name", "usage_query": bool, "calls": {MODEL: SCRIPT}}. The
// request's model picks its script; a script is {"persistent": REPLY}, the
// reply to every call, or {"replies": [REPLY, ...]}, one per call in order,
// a call past the list refused with a 500 that names the scenario. A REPLY
// is one of:
//   - {"write": {PATH: CONTENT}, "delete": [PATH]}: tool calls (write_file,
//     delete_file) as the model sends them, in path order;
//   - {"say": TEXT}: an assistant message with no tool call (a builder's
//     summary, which ends its edit loop);
//   - {"decision": OBJECT}: a reviewer's decision, its JSON as the content;
//   - {"body": ANY}: the body, as is;
// with, on any of them:
//   - "usage": {"prompt_tokens", "completion_tokens"} (default 100 and 20),
//     or null for a reply with no usage;
//   - "status": the HTTP status (default 200);
//   - "stall_ms": a wait before the reply;
//   - "trickle_ms": the body written one byte every so many ms;
//   - "partial": the body cut after so many bytes, the connection dropped;
//   - "barrier": a name: the server logs "barrier <request_id>" on stderr
//     and holds the reply until DIR/<name>.release exists.
// GET /v1/usage/{request_id} answers the usage a reply reported (404 for an
// unknown id, and for every id when "usage_query" is false). The log is
// JSONL: each request's body, and each served usage.

import { appendFileSync, existsSync, readFileSync } from "node:fs";
import { createServer } from "node:http";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const maxBody = 8 << 20;

/** reply is the chat-completions body of r, request id id. */
export function body(r, id) {
  if ("body" in r) return r.body;
  const message = { role: "assistant", content: null };
  if (r.write || r.delete) {
    const writes = Object.keys(r.write ?? {}).sort().map((path, i) => ({ id: `${id}-w${i}`, type: "function",
      function: { name: "write_file", arguments: JSON.stringify({ path, content: r.write[path] }) } }));
    const deletes = (r.delete ?? []).map((path, i) => ({ id: `${id}-d${i}`, type: "function", function: { name: "delete_file", arguments: JSON.stringify({ path }) } }));
    message.tool_calls = [...writes, ...deletes];
  } else if ("decision" in r) {
    message.content = JSON.stringify(r.decision);
  } else {
    message.content = r.say ?? "";
  }
  const usage = r.usage === undefined ? { prompt_tokens: 100, completion_tokens: 20 } : r.usage;
  return { id, object: "chat.completion", model: "stub", choices: [{ index: 0, message, finish_reason: message.tool_calls ? "tool_calls" : "stop" }],
    ...(usage ? { usage } : {}) };
}

async function read(req) {
  const chunks = [];
  let n = 0;
  for await (const c of req) {
    n += c.length;
    if (n > maxBody) throw new Error("the request is past the gateway's bound");
    chunks.push(c);
  }
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

/** start serves scenario (an object) and resolves to {base, close}. */
export async function start({ scenario, listen = "127.0.0.1:0", log, barriers = "." }) {
  const calls = new Map(); // model -> calls served
  const usages = new Map(); // request id -> usage
  let served = 0;
  const record = (entry) => log && appendFileSync(log, JSON.stringify({ at: new Date().toISOString(), ...entry }) + "\n");
  const send = (res, status, doc) => {
    res.writeHead(status, { "content-type": "application/json" });
    res.end(JSON.stringify(doc));
  };
  const server = createServer(async (req, res) => {
    try {
      const usage = req.method === "GET" && /^\/v1\/usage\/([^/]+)$/.exec(req.url ?? "");
      if (usage) {
        const u = scenario.usage_query === false ? undefined : usages.get(decodeURIComponent(usage[1]));
        return u ? send(res, 200, { request_id: usage[1], usage: u }) : send(res, 404, { error: { message: "no such request" } });
      }
      if (req.method !== "POST" || req.url !== "/v1/chat/completions") return send(res, 404, { error: { message: "not found" } });
      const doc = await read(req);
      const id = `req-${++served}`;
      const script = scenario.calls?.[doc.model];
      const n = calls.get(doc.model) ?? 0;
      calls.set(doc.model, n + 1);
      record({ kind: "request", request_id: id, model: doc.model, call: n, body: doc });
      const r = script?.persistent ?? script?.replies?.[n];
      if (!r) return send(res, 500, { error: { message: `scenario ${scenario.name}: no reply for call ${n} of ${doc.model}` } });
      if (r.barrier) {
        process.stderr.write(`barrier ${id}\n`);
        while (!existsSync(join(barriers, `${r.barrier}.release`))) await sleep(20);
      }
      if (r.stall_ms) await sleep(r.stall_ms);
      const b = body(r, id);
      if (b.usage) {
        usages.set(id, b.usage);
        record({ kind: "usage", request_id: id, usage: b.usage });
      }
      const bytes = Buffer.from(JSON.stringify(b));
      res.writeHead(r.status ?? 200, { "content-type": "application/json" });
      if (r.partial !== undefined) {
        res.write(bytes.subarray(0, r.partial));
        return res.destroy();
      }
      if (r.trickle_ms) {
        for (const byte of bytes) {
          res.write(Buffer.of(byte));
          await sleep(r.trickle_ms);
        }
        return res.end();
      }
      res.end(bytes);
    } catch (err) {
      if (!res.headersSent) send(res, 400, { error: { message: String(err.message ?? err) } });
    }
  });
  const port = /^(.*):(\d+)$/.exec(listen);
  await new Promise((r) => (port ? server.listen(Number(port[2]), port[1], r) : server.listen(listen, r)));
  const base = port ? `http://${port[1]}:${server.address().port}/v1` : "http://stub/v1";
  return { base, close: () => new Promise((r) => { server.closeAllConnections(); server.close(() => r()); }) };
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
  const s = await start({ scenario: JSON.parse(readFileSync(file, "utf8")), listen: flag(args, "--listen"), log: flag(args, "--log"), barriers: flag(args, "--barriers") });
  process.stdout.write(`listening ${s.base}\n`);
  for (const sig of ["SIGTERM", "SIGINT"]) process.on(sig, () => s.close().then(() => process.exit(0)));
}
