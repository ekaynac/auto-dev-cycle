// SPDX-License-Identifier: Apache-2.0
// The contract-faithful fake adapter (docs/design/m0/09 "Fakes", 11
// "Contract-faithful fakes"): it speaks the whole JSON-RPC contract through
// the SDK and plays one scenario per process, so the core and the
// conformance kit can be tested against cumulative patches, streaming gaps,
// sub-calls, usage, provider errors, mediated tools and cancellation. An
// executor that does not complete reports could_not_run with the reason the
// exit map gives for its cause (internal/vocab/exitmap).

import { createHash } from "node:crypto";
import type { AdapterDescribeResult } from "@adc/contract-gen/types/adapter/describe-result.ts";
import type { AdapterOutcome } from "@adc/contract-gen/types/adapter/outcome.ts";
import type { AdapterPrepareParams } from "@adc/contract-gen/types/adapter/prepare-params.ts";
import type { AdapterRunParams } from "@adc/contract-gen/types/adapter/run-params.ts";
import type { AdapterRunResult } from "@adc/contract-gen/types/adapter/run-result.ts";
import { Adapter, type EventBody, type Handle, type RunTools } from "@adc/contract";

/** The scenarios, one per process. */
export const scenarios = [
  "patch", // two turns, one sub-call, a cumulative patch
  "text", // one turn, a text result
  "sub-calls", // two sub-calls in one call, usage numbered per call
  "stream-gap", // a pause between deltas; heartbeats keep the call alive
  "tool", // a controller-mediated tool round-trip
  "no-usage", // usage telemetry declared off: no usage events, a null final
  "rate-limited", // a 429 mid-call
  "credit-exhausted", // a 402 mid-call, after partial work
  "http-4xx",
  "http-5xx",
  "idle-cut",
  "degenerate",
  "hang", // streams until cancelled
  "broken-event", // yields an event the contract refuses: the kit's red-proof
] as const;
export type Scenario = (typeof scenarios)[number];

/** errorScenarios: the error event's class_hint and the run's reason. */
const errorScenarios: Partial<Record<Scenario, { hint: string; reason: string }>> = {
  "rate-limited": { hint: "rate_limited", reason: "quota_exhausted" },
  "credit-exhausted": { hint: "credit_exhausted", reason: "broker_credit_exhausted" },
  "http-4xx": { hint: "http_4xx", reason: "provider_request_refused" },
  "http-5xx": { hint: "http_5xx", reason: "provider_unavailable" },
  "idle-cut": { hint: "idle_cut", reason: "stream_idle_cut" },
  degenerate: { hint: "degenerate", reason: "degenerate_output" },
};

export interface FakeOptions {
  scenario: Scenario;
  /** The stream-gap scenario's pause, in milliseconds. */
  gapMs?: number;
  /** The mediated tool the tool scenario requests. */
  tool?: string;
}

/** handle is an attempt-scoped handle for content the fake would have
 *  written; its digest and size are the content's. */
export function handle(attempt: string, name: string, content: string): Handle {
  const bytes = Buffer.from(content);
  return { attempt, handle: name, digest: "sha256:" + createHash("sha256").update(bytes).digest("hex"), size: bytes.length };
}

const clean: AdapterOutcome = { outcome: "empty", reason: "completed_clean", detail: "" };
const cost = (micros: number) => ({ amount_micros: micros, currency: "USD", basis: "cash" as const, estimate: true });
const usage = (inTokens: number, outTokens: number, turns: number, ms: number) => ({
  tokens: { in: inTokens, out: outTokens, cache_read: 0, cache_write: 0 },
  turns,
  milliseconds: ms,
  cost: cost(inTokens + outTokens),
});

export class FakeAdapter extends Adapter {
  readonly scenario: Scenario;
  readonly gapMs: number;
  readonly tool: string;
  prepared: AdapterPrepareParams | null = null;

  constructor(opts: FakeOptions) {
    super();
    if (!scenarios.includes(opts.scenario)) throw new Error(`unknown fake scenario ${JSON.stringify(opts.scenario)}`);
    this.scenario = opts.scenario;
    this.gapMs = opts.gapMs ?? 12000;
    this.tool = opts.tool ?? "RunTest";
  }

  async describe(): Promise<AdapterDescribeResult> {
    return {
      harness: "fake",
      harness_version: "1.0.0",
      adapter_version: "0.1.0",
      contract_version: "1.0.0",
      capabilities: ["patch", "text", "tool_use"],
      session_semantics: "fresh_process",
      tool_surface: [this.tool],
      telemetry: { usage: this.scenario !== "no-usage", upstream: true, stream_deltas: true, per_turn_coverage: true },
      host_side: false,
      needs_native_login: false,
    };
  }

  async prepare(p: AdapterPrepareParams): Promise<AdapterOutcome> {
    this.prepared = p;
    return clean;
  }

  async *run(p: AdapterRunParams, signal: AbortSignal, tools: RunTools): AsyncGenerator<EventBody, AdapterRunResult> {
    const at = (n: number) => new Date(Date.UTC(2026, 9, 5, 0, 0, n)).toISOString();
    const h = (name: string, content: string) => handle(p.attempt, name, content);
    const subCalls = this.scenario === "sub-calls" ? ["s1", "s2"] : ["s1"];
    const turns = this.scenario === "patch" || this.scenario === "sub-calls" ? 2 : 1;
    const reportsUsage = this.scenario !== "no-usage";
    let sequence = 0;
    let total = usage(0, 0, 0, 0);
    const add = (u: ReturnType<typeof usage>) => {
      total = usage(total.tokens.in + u.tokens.in, total.tokens.out + u.tokens.out, total.turns + u.turns, total.milliseconds + u.milliseconds);
    };
    let patch = "";
    for (const [i, sub] of subCalls.entries()) {
      yield { event: "call.sub", sub_call: sub, model_requested: "fake-model" } as EventBody;
      const turn = i + 1;
      yield { event: "turn.start", turn, sub_call: sub, reported_at: at(2 * turn) } as EventBody;
      yield { event: "upstream", turn, sub_call: sub, provider: "fake", model_reported: "fake-model-1" } as EventBody;
      yield { event: "delta.text", turn, bytes: 40 } as EventBody;
      if (this.scenario === "stream-gap") await new Promise((r) => setTimeout(r, this.gapMs));
      if (this.scenario === "broken-event") yield { event: "delta.text", turn } as unknown as EventBody;
      if (this.scenario === "hang") {
        while (!signal.aborted) {
          yield { event: "delta.thinking", turn, bytes: 8 } as EventBody;
          await new Promise((r) => setTimeout(r, 20));
        }
        return { outcome: "could_not_run", reason: "attempt_stopped", detail: "cancelled while streaming", result: null, usage_final: null };
      }
      if (this.scenario === "tool") {
        const request = h("req-1.json", JSON.stringify({ tool: this.tool, args: ["-run", "TestX"] }));
        yield { event: "tool.request", turn, tool: this.tool, request } as EventBody;
        const answer = await tools.response(request);
        yield { event: "tool.result", turn, tool: this.tool, mediated_by_controller: true, delivered: answer.delivered, bytes: answer.delivered.size } as EventBody;
      }
      yield { event: "delta.tool_call", turn, bytes: 120 } as EventBody;
      // The patch is cumulative: each turn's is the whole change so far.
      patch += `--- a/file${turn}.go\n+++ b/file${turn}.go\n@@ -1 +1 @@\n-old\n+new\n`;
      const u = usage(1000 * turn, 200 * turn, 1, 1500);
      if (reportsUsage) {
        yield { event: "usage", sub_call: sub, sequence: sequence++, ...u } as EventBody;
        add(u);
      }
      const err = errorScenarios[this.scenario];
      if (err) {
        yield { event: "error", turn, class_hint: err.hint, raw_excerpt: h("error.txt", `${err.hint} from the fake provider`) } as EventBody;
        return { outcome: "could_not_run", reason: err.reason, detail: `the fake provider: ${err.hint}`, result: null, usage_final: total };
      }
      yield { event: "coverage", turn, observable: "complete", unobservable: [] } as EventBody;
      yield { event: "turn.end", turn, sub_call: sub, reported_at: at(2 * turn + 1) } as EventBody;
      if (subCalls.length === 1 && turn < turns) {
        // A second turn on the same sub-call.
        const next = turn + 1;
        yield { event: "turn.start", turn: next, sub_call: sub, reported_at: at(2 * next) } as EventBody;
        yield { event: "delta.text", turn: next, bytes: 30 } as EventBody;
        patch += `--- a/file${next}.go\n+++ b/file${next}.go\n@@ -1 +1 @@\n-old\n+new\n`;
        const u2 = usage(800, 150, 1, 1200);
        yield { event: "usage", sub_call: sub, sequence: sequence++, ...u2 } as EventBody;
        add(u2);
        yield { event: "coverage", turn: next, observable: "complete", unobservable: [] } as EventBody;
        yield { event: "turn.end", turn: next, sub_call: sub, reported_at: at(2 * next + 1) } as EventBody;
      }
    }
    const result = this.scenario === "text" ? { kind: "text" as const, handle: h("answer.md", "the answer\n") } : { kind: "patch" as const, handle: h("patch.diff", patch) };
    return { outcome: "found", reason: "completed_with_findings", detail: `the fake's ${result.kind}`, result, usage_final: reportsUsage ? total : null };
  }
}
