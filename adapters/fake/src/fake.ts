// SPDX-License-Identifier: Apache-2.0
// The contract-faithful fake adapter (docs/design/m0/09 "Fakes", 11
// "Contract-faithful fakes"): it speaks the whole JSON-RPC contract through
// the SDK and plays one scenario per process, so the core and the
// conformance kit can be tested against cumulative patches, streaming gaps,
// sub-calls, usage, coverage, provider errors, mediated tools and
// cancellation. An executor that does not complete reports could_not_run
// with the reason the exit map gives for its cause (internal/vocab/exitmap),
// and no final usage: an interrupted run cannot know it (run-result.json).

import { createHash } from "node:crypto";
import type { AdapterDescribeResult } from "@adc/contract-gen/types/adapter/describe-result.ts";
import type { AdapterOutcome } from "@adc/contract-gen/types/adapter/outcome.ts";
import type { AdapterPrepareParams } from "@adc/contract-gen/types/adapter/prepare-params.ts";
import type { AdapterRunParams } from "@adc/contract-gen/types/adapter/run-params.ts";
import type { AdapterRunResult } from "@adc/contract-gen/types/adapter/run-result.ts";
import { Adapter, type EventBody, type Handle, type RunTools } from "@adc/contract";

/** The scenarios, one per process. */
export const scenarios = [
  "patch", // two turns on one sub-call, a cumulative patch
  "text", // one text-only turn, a text result
  "no-change", // completed with no change: empty
  "sub-calls", // two sub-calls in one call, usage numbered per call
  "stream-gap", // a pause between deltas; heartbeats keep the call alive
  "compaction", // the harness compacts its context mid-turn
  "tool", // a controller-mediated tool round-trip
  "no-usage", // usage telemetry declared off: no usage events, a null final
  "rate-limited", // a 429 after partial work
  "credit-exhausted", // a 402 after partial work
  "http-4xx",
  "http-5xx",
  "idle-cut",
  "degenerate",
  "other-error", // an error the adapter cannot classify
  "two-errors", // a 5xx, then credit exhaustion: the exit map's precedence decides
  "hang", // streams until cancelled
  // The SDK's red-proof: an event the contract's schema refuses.
  "broken-event",
  // The conformance kit's red-proofs: schema-valid streams that break the contract's other rules.
  "bad-sequence", // usage sequence skips a number
  "unannounced-sub-call", // turns name a sub-call never announced by call.sub
  "non-cumulative", // the patch result holds only the last turn's change
] as const;
export type Scenario = (typeof scenarios)[number];

type ClassHint = Extract<EventBody, { event: "error" }>["class_hint"];
type Unobservable = Extract<EventBody, { event: "coverage" }>["unobservable"];
type Usage = Omit<Extract<EventBody, { event: "usage" }>, "event" | "sub_call" | "sequence">;

/** errorScenarios: the error events' class hints and the run's reason. */
const errorScenarios: Partial<Record<Scenario, { hints: ClassHint[]; reason: string }>> = {
  "rate-limited": { hints: ["rate_limited"], reason: "quota_exhausted" },
  "credit-exhausted": { hints: ["credit_exhausted"], reason: "broker_credit_exhausted" },
  "http-4xx": { hints: ["http_4xx"], reason: "provider_request_refused" },
  "http-5xx": { hints: ["http_5xx"], reason: "provider_unavailable" },
  "idle-cut": { hints: ["idle_cut"], reason: "stream_idle_cut" },
  degenerate: { hints: ["degenerate"], reason: "degenerate_output" },
  "other-error": { hints: ["other"], reason: "executor_incomplete" },
  "two-errors": { hints: ["http_5xx", "credit_exhausted"], reason: "broker_credit_exhausted" },
};

const twoTurns: readonly Scenario[] = ["patch", "sub-calls", "bad-sequence", "unannounced-sub-call", "non-cumulative"];
const maxGapMs = 2_147_483_647; // setTimeout's bound

export interface FakeOptions {
  scenario: Scenario;
  /** The stream-gap scenario's pause: an integer number of milliseconds. */
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

/** usageOf is a usage report: tokens, one turn, time, and cost at one micro per token. */
function usageOf(inTokens: number, outTokens: number, ms: number): Usage {
  return {
    tokens: { in: inTokens, out: outTokens, cache_read: 0, cache_write: 0 },
    turns: 1,
    milliseconds: ms,
    cost: { amount_micros: inTokens + outTokens, currency: "USD", basis: "cash", estimate: true },
  };
}

/** sum is the field-wise sum of usage reports: the final a completed call reports. */
export function sum(us: Usage[]): Usage {
  const t = (k: keyof Usage["tokens"]) => us.reduce((n, u) => n + u.tokens[k], 0);
  return {
    tokens: { in: t("in"), out: t("out"), cache_read: t("cache_read"), cache_write: t("cache_write") },
    turns: us.reduce((n, u) => n + u.turns, 0),
    milliseconds: us.reduce((n, u) => n + u.milliseconds, 0),
    cost: { amount_micros: us.reduce((n, u) => n + u.cost.amount_micros, 0), currency: "USD", basis: "cash", estimate: true },
  };
}

const clean: AdapterOutcome = { outcome: "empty", reason: "completed_clean", detail: "" };
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export class FakeAdapter extends Adapter {
  readonly scenario: Scenario;
  readonly gapMs: number;
  readonly tool: string;
  prepared: AdapterPrepareParams | null = null;

  constructor(opts: FakeOptions) {
    super();
    if (!scenarios.includes(opts.scenario)) throw new Error(`unknown fake scenario ${JSON.stringify(opts.scenario)}`);
    if (opts.gapMs !== undefined && !(Number.isInteger(opts.gapMs) && opts.gapMs >= 0 && opts.gapMs <= maxGapMs)) {
      throw new Error(`the stream gap must be an integer number of milliseconds from 0 to ${maxGapMs}`);
    }
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
      tool_surface: ["Edit", this.tool],
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
    const s = this.scenario;
    const at = (n: number) => new Date(Date.UTC(2026, 9, 5, 0, 0, n)).toISOString();
    const h = (name: string, content: string) => handle(p.attempt, name, content);
    const stopped: AdapterRunResult = { outcome: "could_not_run", reason: "attempt_stopped", detail: "cancelled before completing", result: null, usage_final: null };
    const plan = twoTurns.includes(s) ? [{ turn: 1, sub: "s1" }, { turn: 2, sub: s === "sub-calls" ? "s2" : "s1" }] : [{ turn: 1, sub: "s1" }];
    const reportsUsage = s !== "no-usage";
    const announced = new Set<string>();
    const deltas: Usage[] = [];
    let sequence = 0;
    let patch = "";
    let lastChange = "";
    for (const { turn, sub } of plan) {
      if (!announced.has(sub)) {
        yield { event: "call.sub", sub_call: sub, model_requested: "fake-model" };
        announced.add(sub);
      }
      const named = s === "unannounced-sub-call" ? "s9" : sub;
      yield { event: "turn.start", turn, sub_call: named, reported_at: at(2 * turn) };
      yield { event: "upstream", turn, sub_call: named, provider: "fake", model_reported: "fake-model-1" };
      yield { event: "delta.text", turn, bytes: 40 };
      if (s === "stream-gap") await sleep(this.gapMs);
      // The one deliberate cast: an event the contract refuses.
      if (s === "broken-event") yield { event: "delta.text", turn } as unknown as EventBody;
      if (s === "hang") {
        while (!signal.aborted) {
          yield { event: "delta.thinking", turn, bytes: 8 };
          await sleep(20);
        }
        return stopped;
      }
      const unobservable: Unobservable = [];
      if (s === "compaction") {
        yield { event: "compaction", turn, before_tokens: 9000, after_tokens: 3000 };
        unobservable.push("compaction");
      }
      if (s !== "text" && s !== "no-change") {
        // A harness edit: its tool call streams, then the harness's own
        // (not mediated) result; the turn's coverage is then partial (09).
        yield { event: "delta.tool_call", turn, bytes: 120 };
        yield { event: "tool.result", turn, tool: "Edit", mediated_by_controller: false, delivered: null, bytes: 64 };
        unobservable.push("harness_tool");
        lastChange = `--- a/file${turn}.go\n+++ b/file${turn}.go\n@@ -1 +1 @@\n-old\n+new\n`;
        patch += lastChange;
      }
      if (s === "tool") {
        if (!this.prepared?.tool_policy.mediated.includes(this.tool)) {
          return { outcome: "could_not_run", reason: "executor_incomplete", detail: `the tool policy does not mediate ${this.tool}`, result: null, usage_final: null };
        }
        yield { event: "delta.tool_call", turn, bytes: 60 };
        const request = h("req-1.json", JSON.stringify({ tool: this.tool, args: ["-run", "TestX"] }));
        yield { event: "tool.request", turn, tool: this.tool, request };
        let answer;
        try {
          answer = await tools.response(request);
        } catch (e) {
          if (signal.aborted) return stopped; // cancelled during the tool call (09)
          throw e;
        }
        yield { event: "tool.result", turn, tool: this.tool, mediated_by_controller: true, delivered: answer.delivered, bytes: answer.delivered.size };
      }
      if (reportsUsage) {
        const u = usageOf(1000 * turn, 200 * turn, 1500 * turn);
        yield { event: "usage", sub_call: sub, sequence: s === "bad-sequence" && sequence > 0 ? sequence + 1 : sequence, ...u };
        sequence++;
        deltas.push(u);
      }
      const err = errorScenarios[s];
      if (err) {
        for (const hint of err.hints) yield { event: "error", turn, class_hint: hint, raw_excerpt: h(`error-${hint}.txt`, `${hint} from the fake provider`) };
        return { outcome: "could_not_run", reason: err.reason, detail: `the fake provider: ${err.hints.join(", ")}`, result: null, usage_final: null };
      }
      yield { event: "coverage", turn, observable: unobservable.length ? "partial" : "complete", unobservable };
      yield { event: "turn.end", turn, sub_call: named, reported_at: at(2 * turn + 1) };
    }
    const final = reportsUsage ? sum(deltas) : null;
    if (s === "no-change") return { outcome: "empty", reason: "completed_clean", detail: "", result: null, usage_final: final };
    if (s === "text") return { outcome: "found", reason: "completed_with_findings", detail: "the fake's text", result: { kind: "text", handle: h("answer.md", "the answer\n") }, usage_final: final };
    const content = s === "non-cumulative" ? lastChange : patch;
    return { outcome: "found", reason: "completed_with_findings", detail: "the fake's patch", result: { kind: "patch", handle: h("patch.diff", content) }, usage_final: final };
  }
}
