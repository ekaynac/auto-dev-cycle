// SPDX-License-Identifier: Apache-2.0
// The adapter's one way out: the controller's model gateway (internal/gateway,
// P11). It holds no credential: the attempt's gateway token is its only
// authorisation, and the gateway injects the destination's credential. It
// POSTs to base_url + "/chat/completions", the OpenAI convention: the gateway
// serves that path at its root, so the controller hands it a base_url with no
// path (#226 entry 58). In the sandbox the gateway is a Unix socket
// (internal/sandbox GatewaySocket), named by socketPath; the URL then gives
// only the path and Host. Redirects are never followed and no proxy is used
// (node:http does neither).

import { request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";

export interface Gateway {
  base_url: string;
  token: string;
  /** The gateway's Unix socket, when it is reached through one. */
  socketPath?: string;
}

/** The bytes of one gateway reply the adapter reads; more is an error. */
export const maxReplyBytes = 16 << 20;

/** The top-level request keys this adapter sends: all in the gateway's
 *  allowlist (internal/gateway/egress.go allowed), which refuses any other. */
export const requestKeys = ["model", "messages", "tools", "tool_choice", "max_tokens", "response_format", "stream"] as const;
export type ChatRequest = Partial<Record<(typeof requestKeys)[number], unknown>>;

export interface Reply {
  status: number;
  text: string;
}

/** chat sends one chat-completions request and reads the whole reply. */
export function chat(g: Gateway, body: ChatRequest, signal: AbortSignal): Promise<Reply> {
  const url = new URL(g.base_url);
  const payload = Buffer.from(JSON.stringify(body));
  const send = url.protocol === "https:" ? httpsRequest : httpRequest;
  return new Promise((resolve, reject) => {
    const req = send(
      {
        method: "POST",
        hostname: url.hostname.replace(/^\[|\]$/g, ""),
        port: url.port || undefined,
        socketPath: g.socketPath,
        path: url.pathname.replace(/\/+$/, "") + "/chat/completions",
        headers: { authorization: `Bearer ${g.token}`, "content-type": "application/json", "content-length": payload.length, host: url.host },
        agent: false,
        signal,
      },
      (res) => {
        const chunks: Buffer[] = [];
        let size = 0;
        res.on("data", (c: Buffer) => {
          size += c.length;
          if (size > maxReplyBytes) res.destroy(new Error(`the gateway's reply is over ${maxReplyBytes} bytes`));
          else chunks.push(c);
        });
        res.on("error", reject);
        res.on("end", () => resolve({ status: res.statusCode ?? 0, text: Buffer.concat(chunks).toString("utf8") }));
      },
    );
    req.on("error", reject);
    req.end(payload);
  });
}

type ClassHint = "http_4xx" | "rate_limited" | "credit_exhausted" | "http_5xx" | "idle_cut" | "degenerate" | "other";

/** Failure is what a non-2xx reply ends the run with. A hint names an
 *  error event to report (the core's exit map decides); a gateway refusal
 *  of the attempt itself reports none and writes nothing. */
export interface Failure {
  outcome: "could_not_run" | "failed";
  reason: string;
  hint: ClassHint | null;
  detail: string;
}

/** The gateway's refusals that end the attempt as stale: its token is
 *  unbound, or its lease no longer runs it. */
const stale = new Set(["unbound", "lease_not_running_attempt"]);
/** The refusals of the route or model asked for: refused before egress. */
const routing = new Set(["model_not_pinned", "destination_not_admitted", "executor_not_registered", "redirect", "credential_echoed", "credential_in_body"]);

/** classify reads a non-2xx reply: the gateway's own refusal
 *  ({"error":{"type":"gateway_refused","code":…}}), or the upstream's status. */
export function classify(r: Reply): Failure {
  let code = "";
  try {
    const e = JSON.parse(r.text)?.error;
    if (e?.type === "gateway_refused" && typeof e.code === "string") code = e.code;
  } catch {
    // not JSON: an upstream body
  }
  const s = r.status;
  if (stale.has(code)) return { outcome: "failed", reason: "stale_attempt", hint: null, detail: `the gateway refused the attempt (${code}): it is no longer current` };
  if (routing.has(code)) return { outcome: "failed", reason: "routing_violation", hint: null, detail: `the gateway refused the request before egress (${code})` };
  if (code === "upstream_unreachable") return { outcome: "could_not_run", reason: "provider_unavailable", hint: "http_5xx", detail: "the gateway could not reach the upstream" };
  if (code !== "") return { outcome: "could_not_run", reason: "executor_incomplete", hint: "other", detail: `the gateway refused the request (${code}, HTTP ${s})` };
  if (s === 402) return { outcome: "could_not_run", reason: "broker_credit_exhausted", hint: "credit_exhausted", detail: "the upstream answered HTTP 402" };
  if (s === 429) return { outcome: "could_not_run", reason: "quota_exhausted", hint: "rate_limited", detail: "the upstream answered HTTP 429" };
  if (s >= 400 && s < 500) return { outcome: "could_not_run", reason: "provider_request_refused", hint: "http_4xx", detail: `the upstream answered HTTP ${s}` };
  if (s >= 500 && s < 600) return { outcome: "could_not_run", reason: "provider_unavailable", hint: "http_5xx", detail: `the upstream answered HTTP ${s}` };
  return { outcome: "could_not_run", reason: "executor_incomplete", hint: "other", detail: `the gateway answered HTTP ${s}` };
}
