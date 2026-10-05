// SPDX-License-Identifier: Apache-2.0
// The JSON-RPC envelope of what an adapter sends (docs/design/m0/09): a
// response or an adapter.event notification. The Go host classifies the same
// way (internal/adapterhost/wire); fixtures/contract/wire-cases.json holds
// the corpus both run.

export type Reply = Record<string, unknown> & { id: number; result?: unknown; error?: { code: number; message: string; data?: unknown } };

export type Classified =
  | { kind: "event"; params: Record<string, unknown> }
  | { kind: "response"; reply: Reply }
  | { kind: "invalid"; why: string };

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

/** classify takes one strictly parsed message (parseStrict). A response has
 *  an integer id, no method, and exactly one of result and error, where
 *  error is an object with an integer code and a string message; an event
 *  has method adapter.event, no id, and object params. Unknown members are
 *  allowed. */
export function classify(m: unknown): Classified {
  if (!isObject(m) || m.jsonrpc !== "2.0") return { kind: "invalid", why: "not a JSON-RPC 2.0 message" };
  if (m.method === "adapter.event" && !("id" in m)) {
    return isObject(m.params) ? { kind: "event", params: m.params } : { kind: "invalid", why: "an adapter.event without object params" };
  }
  if (!Number.isInteger(m.id) || "method" in m || ("result" in m) === ("error" in m)) {
    return { kind: "invalid", why: "neither a response nor an adapter.event notification" };
  }
  const err = m.error;
  if ("error" in m && (!isObject(err) || !Number.isInteger(err.code) || typeof err.message !== "string")) {
    return { kind: "invalid", why: "an error that is not a JSON-RPC error object" };
  }
  return { kind: "response", reply: m as Reply };
}
