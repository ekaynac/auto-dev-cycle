// SPDX-License-Identifier: Apache-2.0
// A stub gateway for the tests: loopback or a Unix socket, never the
// network. It records every request and answers from a script, one reply
// per request in order.

import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";

export interface Seen {
  method: string;
  path: string;
  authorization: string | undefined;
  body: any;
}

export type Answer = { status?: number; body: unknown };

export interface Stub {
  base: string;
  seen: Seen[];
  close(): Promise<void>;
}

/** reply is a chat-completions reply with one message and usage. */
export function reply(message: object, usage: object | null = { prompt_tokens: 100, completion_tokens: 20, prompt_tokens_details: { cached_tokens: 40 } }): Answer {
  return { body: { id: "x", model: "m-reported", choices: [{ index: 0, message: { role: "assistant", content: null, ...message }, finish_reason: "stop" }], ...(usage ? { usage } : {}) } };
}

/** call is a tool call as the model sends it. */
export const call = (id: string, name: string, args: object) => ({ id, type: "function", function: { name, arguments: JSON.stringify(args) } });

async function body(req: IncomingMessage): Promise<any> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

/** stub serves script; listen is a socket path, or loopback when unset. */
export async function stub(script: Answer[], listen?: string): Promise<Stub> {
  const seen: Seen[] = [];
  const server = createServer(async (req, res) => {
    seen.push({ method: req.method ?? "", path: req.url ?? "", authorization: req.headers.authorization, body: await body(req) });
    const a = script[seen.length - 1] ?? { status: 500, body: { error: "the script ran out" } };
    res.writeHead(a.status ?? 200, { "content-type": "application/json" });
    res.end(JSON.stringify(a.body));
  });
  await new Promise<void>((r) => (listen ? server.listen(listen, r) : server.listen(0, "127.0.0.1", r)));
  const base = listen ? "http://gateway" : `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  return { base, seen, close: () => new Promise((r) => server.close(() => r())) };
}
