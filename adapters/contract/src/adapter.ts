// SPDX-License-Identifier: Apache-2.0
// The adapter side of the JSON-RPC contract (docs/design/m0/09): an Adapter
// implements the methods; serve frames, parses strictly, validates every
// params and result against the generated validators, stamps each event
// with its attempt, fencing token and call, and sends heartbeats.

import { createRequire } from "node:module";
import type { AdapterDescribeResult } from "@adc/contract-gen/types/adapter/describe-result.ts";
import type { AdapterEvent } from "@adc/contract-gen/types/adapter/event.ts";
import type { AdapterFencedParams } from "@adc/contract-gen/types/adapter/fenced-params.ts";
import type { AdapterOutcome } from "@adc/contract-gen/types/adapter/outcome.ts";
import type { AdapterPrepareParams } from "@adc/contract-gen/types/adapter/prepare-params.ts";
import type { AdapterRunParams } from "@adc/contract-gen/types/adapter/run-params.ts";
import type { AdapterRunResult } from "@adc/contract-gen/types/adapter/run-result.ts";
import type { AdapterToolResponseParams } from "@adc/contract-gen/types/adapter/tool-response-params.ts";
import { frame, lines } from "./framing.ts";
import { parseStrict } from "./strict-json.ts";

type Validator = ((doc: unknown) => boolean) & { errors?: unknown };
const validators: Record<string, Validator> = createRequire(import.meta.url)("@adc/contract-gen/adapter-validators.cjs");

type Envelope = "attempt" | "fencing_token" | "call";
/** An event as an adapter yields it: serve adds the envelope. */
export type EventBody = AdapterEvent extends infer E ? (E extends AdapterEvent ? Omit<E, Envelope> : never) : never;

/** The notification method that carries events (adapter → core). */
export const eventMethod = "adapter.event";

// JSON-RPC 2.0 error codes.
export const ParseError = -32700;
export const InvalidRequest = -32600;
export const MethodNotFound = -32601;
export const InvalidParams = -32602;
export const InternalError = -32603;

const clean: AdapterOutcome = { outcome: "empty", reason: "completed_clean", detail: "" };

/** An attempt-scoped blob handle, as events and tool responses carry it. */
export type Handle = AdapterToolResponseParams["request"];

/** What a run uses besides its params and signal. */
export interface RunTools {
  /** response waits for the core's tool.response to the tool.request whose
   *  request handle this is: yield that event first. It resolves with the
   *  core's answer (its delivered handle is what the run's tool.result must
   *  report) and rejects when the run is aborted. */
  response(request: Handle): Promise<AdapterToolResponseParams>;
}

export abstract class Adapter {
  abstract describe(): Promise<AdapterDescribeResult>;
  abstract prepare(p: AdapterPrepareParams): Promise<AdapterOutcome>;
  /** run yields the call's events (without their envelope) and returns its
   *  result. signal aborts on cancel, shutdown or a broken input: stop the
   *  harness and return. */
  abstract run(p: AdapterRunParams, signal: AbortSignal, tools: RunTools): AsyncGenerator<EventBody, AdapterRunResult>;
  /** Best effort (A4); the default does nothing beyond the aborted signal. */
  async cancel(_p: AdapterFencedParams): Promise<AdapterOutcome> {
    return clean;
  }
  async shutdown(): Promise<AdapterOutcome> {
    return clean;
  }
}

export interface ServeOptions {
  input: AsyncIterable<Uint8Array>;
  write: (line: string) => void;
  /** Heartbeat period during a run (see heartbeatPeriod). */
  heartbeatMs?: number;
}

/** heartbeatPeriod: the requested period, 5 s when unset or not a positive
 *  number, and never over 9 s, so a beat lands within 09's 10 s. */
export function heartbeatPeriod(ms?: number): number {
  return ms === undefined || !Number.isFinite(ms) || ms <= 0 ? 5000 : Math.min(ms, 9000);
}

class RPCError extends Error {
  readonly code: number;
  constructor(code: number, message: string) {
    super(message);
    this.code = code;
  }
}

/** wellFormed: every string (keys included) is well-formed UTF-16. A lone
 *  surrogate would be sent as a \u escape the core's reader refuses. */
function wellFormed(v: unknown): boolean {
  if (typeof v === "string") return v.isWellFormed();
  if (Array.isArray(v)) return v.every(wellFormed);
  if (v !== null && typeof v === "object") return Object.entries(v).every(([k, e]) => k.isWellFormed() && wellFormed(e));
  return true;
}

function check(schema: string, doc: unknown, code: number, what: string): void {
  const v = validators[schema];
  if (!v(doc)) throw new RPCError(code, `${what} does not match ${schema}: ${JSON.stringify(v.errors)}`);
  if (!wellFormed(doc)) throw new RPCError(code, `${what} has a string that is not well-formed UTF-16`);
}

const sameHandle = (a: Handle, b: Handle) => a.attempt === b.attempt && a.handle === b.handle && a.digest === b.digest && a.size === b.size;

/** ToolRequests holds a run's controller-mediated tool requests (09). Each is
 *  answered at most once by the core's tool.response, naming the same
 *  request handle, turn and tool, with handles of the run's own attempt; each
 *  delivery is reported by exactly one mediated tool.result for that turn and
 *  tool. */
class ToolRequests implements RunTools {
  private readonly pending = new Map<string, {
    turn: number;
    tool: string;
    request: Handle;
    answer?: AdapterToolResponseParams;
    wait?: { resolve: (r: AdapterToolResponseParams) => void; reject: (e: Error) => void };
  }>();
  private readonly delivered: { turn: number; tool: string; handle: Handle; reported: boolean }[] = [];
  private readonly signal: AbortSignal;
  constructor(signal: AbortSignal) {
    this.signal = signal;
  }

  /** requested records a tool.request the run yielded. */
  requested(e: { attempt: string; turn: number; tool: string; request: Handle }): void {
    if (e.request.attempt !== e.attempt) throw new RPCError(InternalError, `tool.request handle ${e.request.handle} names another attempt`);
    if (this.pending.has(e.request.handle)) throw new RPCError(InternalError, `tool.request handle ${e.request.handle} is already in use`);
    this.pending.set(e.request.handle, { turn: e.turn, tool: e.tool, request: e.request });
  }

  /** answer takes the core's tool.response. */
  answer(r: AdapterToolResponseParams): void {
    const q = this.pending.get(r.request.handle);
    if (!q || !sameHandle(q.request, r.request)) throw new RPCError(InvalidParams, `no tool.request with handle ${r.request.handle} in this run`);
    if (q.answer) throw new RPCError(InvalidParams, `tool.request ${r.request.handle} is already answered`);
    if (q.turn !== r.turn || q.tool !== r.tool) {
      throw new RPCError(InvalidParams, `tool.response names turn ${r.turn} and tool ${r.tool}; the request was turn ${q.turn}, tool ${q.tool}`);
    }
    if (r.delivered.attempt !== r.attempt) throw new RPCError(InvalidParams, `the delivered handle ${r.delivered.handle} names another attempt`);
    q.answer = r;
    this.delivered.push({ turn: r.turn, tool: r.tool, handle: r.delivered, reported: false });
    q.wait?.resolve(r);
  }

  /** reported checks a mediated tool.result the run yielded and uses up the
   *  delivery it reports. */
  reported(e: { turn: number; tool: string; delivered: Handle | null }): void {
    const d = e.delivered;
    const x = d && this.delivered.find((x) => !x.reported && x.turn === e.turn && x.tool === e.tool && sameHandle(x.handle, d));
    if (!x) throw new RPCError(InternalError, "a mediated tool.result must report a handle the core delivered for that turn and tool, once");
    x.reported = true;
  }

  response(request: Handle): Promise<AdapterToolResponseParams> {
    const q = this.pending.get(request.handle);
    if (!q || !sameHandle(q.request, request)) return Promise.reject(new Error(`no tool.request with handle ${request.handle} was yielded`));
    if (q.answer) return Promise.resolve(q.answer);
    if (q.wait) return Promise.reject(new Error(`tool.request ${request.handle} is already awaited`));
    if (this.signal.aborted) return Promise.reject(new Error("the run was aborted"));
    return new Promise((resolve, reject) => {
      const aborted = () => reject(new Error("the run was aborted"));
      this.signal.addEventListener("abort", aborted, { once: true });
      const settle = () => {
        this.signal.removeEventListener("abort", aborted);
        q.wait = undefined;
      };
      q.wait = {
        resolve: (r) => {
          settle();
          resolve(r);
        },
        reject: (e) => {
          settle();
          reject(e);
        },
      };
    });
  }

  /** close rejects every response still awaited when the run ends. */
  close(): void {
    for (const q of this.pending.values()) q.wait?.reject(new Error("the run ended"));
  }
}

/** serve runs the adapter over input until shutdown or the end of input. One
 *  run at a time; prepare only between runs. */
export async function serve(adapter: Adapter, opts: ServeOptions): Promise<void> {
  const heartbeatMs = heartbeatPeriod(opts.heartbeatMs);
  const send = (msg: unknown) => opts.write(frame(msg));
  let prepared: { attempt: string; fencing_token: number } | null = null;
  let active: { controller: AbortController; done: Promise<void>; call: string; tools: ToolRequests } | null = null;
  const respond = async (id: unknown, work: () => Promise<unknown>) => {
    try {
      send({ jsonrpc: "2.0", id, result: await work() });
    } catch (e) {
      const err = e instanceof RPCError ? e : new RPCError(InternalError, `adapter error: ${(e as Error)?.message ?? String(e)}`);
      try {
        send({ jsonrpc: "2.0", id, error: { code: err.code, message: err.message } });
      } catch (sendErr) {
        process.stderr.write(`adc adapter: could not send an error reply: ${(sendErr as Error).message}\n`);
      }
    }
  };
  const fenced = (p: { attempt: string; fencing_token: number }) => {
    if (!prepared || p.attempt !== prepared.attempt || p.fencing_token !== prepared.fencing_token) {
      throw new RPCError(InvalidParams, "not the prepared attempt and fencing token");
    }
  };
  const stop = async () => {
    const a = active;
    if (a) {
      a.controller.abort();
      await a.done;
    }
  };
  const run = async (p: AdapterRunParams, controller: AbortController, tools: ToolRequests): Promise<AdapterRunResult> => {
    check("adapter/run-params", p, InvalidParams, "params");
    fenced(p);
    const envelope = { attempt: p.attempt, fencing_token: p.fencing_token, call: p.call };
    const beat = setInterval(() => send({ jsonrpc: "2.0", method: eventMethod, params: { event: "heartbeat", ...envelope } }), heartbeatMs);
    const it = adapter.run(p, controller.signal, tools);
    let finished = false;
    try {
      for (;;) {
        const step = await it.next();
        if (step.done) {
          finished = true;
          check("adapter/run-result", step.value, InternalError, "the adapter's result");
          return step.value;
        }
        const event = { ...step.value, ...envelope }; // the envelope wins
        check("adapter/event", event, InternalError, "an event");
        // Recorded before it is sent: the core answers only what it read.
        if (event.event === "tool.request") tools.requested(event);
        if (event.event === "tool.result" && event.mediated_by_controller) tools.reported(event);
        send({ jsonrpc: "2.0", method: eventMethod, params: event });
      }
    } finally {
      clearInterval(beat);
      tools.close();
      if (!finished) {
        // Let the adapter's own cleanup (its finally) run: stop the harness.
        controller.abort();
        await it.return?.(undefined as never).catch(() => undefined);
      }
    }
  };
  try {
    for await (const line of lines(opts.input)) {
      let msg: { jsonrpc?: unknown; id?: unknown; method?: unknown; params?: unknown };
      try {
        msg = parseStrict(line) as typeof msg;
      } catch (e) {
        send({ jsonrpc: "2.0", id: null, error: { code: ParseError, message: (e as Error).message } });
        continue;
      }
      const id = msg?.id;
      const validId = typeof id === "string" || (typeof id === "number" && Number.isInteger(id));
      if (typeof msg !== "object" || msg === null || Array.isArray(msg) || msg.jsonrpc !== "2.0" || typeof msg.method !== "string" || !validId) {
        send({ jsonrpc: "2.0", id: validId ? id : null, error: { code: InvalidRequest, message: "not a JSON-RPC 2.0 request with an id" } });
        continue;
      }
      const p = msg.params;
      switch (msg.method) {
        case "describe":
          await respond(id, async () => {
            const d = await adapter.describe();
            check("adapter/describe-result", d, InternalError, "the adapter's description");
            return d;
          });
          break;
        case "prepare":
          await respond(id, async () => {
            check("adapter/prepare-params", p, InvalidParams, "params");
            if (active) throw new RPCError(InvalidParams, "a run is in progress");
            const pp = p as AdapterPrepareParams;
            prepared = null; // a failed prepare leaves nothing prepared
            const r = await adapter.prepare(pp);
            check("adapter/outcome", r, InternalError, "the adapter's outcome");
            if (r.outcome === "empty" || r.outcome === "found") prepared = { attempt: pp.attempt, fencing_token: pp.fencing_token };
            return r;
          });
          break;
        case "run": {
          if (active) {
            send({ jsonrpc: "2.0", id, error: { code: InvalidParams, message: "a run is in progress" } });
            break;
          }
          // A run streams while later requests (cancel) are served.
          const controller = new AbortController();
          const tools = new ToolRequests(controller.signal);
          const done = respond(id, () => run(p as AdapterRunParams, controller, tools)).finally(() => {
            active = null;
          });
          active = { controller, done, call: String((p as AdapterRunParams | undefined)?.call), tools };
          break;
        }
        case "tool.response":
          // The core's answer to a tool.request of the active run (09).
          await respond(id, async () => {
            check("adapter/tool-response-params", p, InvalidParams, "params");
            const r = p as AdapterToolResponseParams;
            fenced(r);
            if (!active || active.call !== r.call) throw new RPCError(InvalidParams, `no run for call ${r.call}`);
            active.tools.answer(r);
            return {};
          });
          break;
        case "cancel":
          await respond(id, async () => {
            check("adapter/fenced-params", p, InvalidParams, "params");
            fenced(p as AdapterFencedParams);
            active?.controller.abort();
            const r = await adapter.cancel(p as AdapterFencedParams);
            check("adapter/outcome", r, InternalError, "the adapter's outcome");
            return r;
          });
          break;
        case "shutdown":
          await stop();
          await respond(id, async () => {
            const r = await adapter.shutdown();
            check("adapter/outcome", r, InternalError, "the adapter's outcome");
            return r;
          });
          return;
        default:
          send({ jsonrpc: "2.0", id, error: { code: MethodNotFound, message: `no method ${JSON.stringify(msg.method)}` } });
      }
    }
  } catch (e) {
    await stop(); // a broken input ends the session: stop the run first
    throw e;
  }
  await active?.done;
}

/** run serves the adapter on stdin and stdout; stderr stays the adapter's log. */
export function run(adapter: Adapter): Promise<void> {
  return serve(adapter, { input: process.stdin, write: (line) => process.stdout.write(line) });
}
