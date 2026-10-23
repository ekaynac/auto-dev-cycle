// SPDX-License-Identifier: Apache-2.0
// The http-chat adapter (docs/design/m1/00-slice-plan.md P13): an
// OpenAI-compatible chat-completions harness that talks only to the
// controller's gateway. A builder runs an edit loop over the workspace tools
// the tool policy allows and returns its cumulative patch; a reviewer gets
// no tool (its context is its prompt alone) and returns a typed decision,
// checked against the run's response schema. Each model request is one turn
// and one sub-call. It offers no subagent and no model switch: every
// request names the prepared model, and the gateway refuses any other.

import { constants } from "node:fs";
import { open } from "node:fs/promises";
import { join } from "node:path";
import { createRequire } from "node:module";
import type { AdapterDescribeResult } from "@adc/contract-gen/types/adapter/describe-result.ts";
import type { AdapterOutcome } from "@adc/contract-gen/types/adapter/outcome.ts";
import type { AdapterPrepareParams } from "@adc/contract-gen/types/adapter/prepare-params.ts";
import type { AdapterRunParams } from "@adc/contract-gen/types/adapter/run-params.ts";
import type { AdapterRunResult } from "@adc/contract-gen/types/adapter/run-result.ts";
import { Adapter, digestOf, type EventBody, type Handle, handlePattern, parseStrict, writeOutput } from "@adc/contract";
import { chat, classify, type ChatRequest, type Failure, type Gateway } from "./gateway.ts";
import { ConfinementError, Workspace } from "./workspace.ts";

type Validator = ((doc: unknown) => boolean) & { errors?: unknown };
type Ajv = { compile(schema: unknown): Validator };
const Ajv2020 = createRequire(import.meta.url)("ajv/dist/2020.js").default as new (o: object) => Ajv;

/** The workspace tools, as the model sees them. tool_policy.allowed picks. */
const toolSpecs: Record<string, { description: string; properties: Record<string, { type: string }> }> = {
  read_file: { description: "Read a UTF-8 file of the repository.", properties: { path: { type: "string" } } },
  list_dir: { description: "List a directory of the repository (\"\" is its root).", properties: { path: { type: "string" } } },
  write_file: { description: "Create or replace a file with the given content.", properties: { path: { type: "string" }, content: { type: "string" } } },
  delete_file: { description: "Delete a file.", properties: { path: { type: "string" } } },
};

/** The decision a reviewer returns when the run names no response schema. */
const finding = { type: "object", additionalProperties: false, required: ["severity", "title", "detail"],
  properties: { severity: { enum: ["critical", "high", "medium", "low"] }, title: { type: "string", maxLength: 256 }, detail: { type: "string", maxLength: 4096 } } };
export const defaultDecisionSchema = { type: "object", additionalProperties: false, required: ["decision", "summary", "findings"],
  properties: { decision: { enum: ["go", "no_go"] }, summary: { type: "string", maxLength: 4096 }, findings: { type: "array", maxItems: 100, items: finding } } };

const system: Record<string, string> = {
  builder: "You change the repository in your workspace to meet the task. Use the tools to read and edit files; every path is relative to the repository root. When the change is complete, reply with a short summary and no tool call.",
  reviewer: "You review the change described in the prompt. Reply only with a JSON object that matches the response schema.",
};

/** reviewerSystem is the reviewer's system message: the schema it is held
 *  to is in it, since a provider may ignore response_format (Ollama Cloud
 *  does: the qualification rehearsal's models guessed one); response_format
 *  stays for those that enforce it. */
export const reviewerSystem = (schema: unknown) => `${system.reviewer}\nThe response schema (JSON Schema):\n${JSON.stringify(schema)}`;

/** The bytes of an input file (prompt, response schema) the adapter reads. */
const maxInputBytes = 1 << 20;
const clean: AdapterOutcome = { outcome: "empty", reason: "completed_clean", detail: "" };

type Usage = Omit<Extract<EventBody, { event: "usage" }>, "event" | "sub_call" | "sequence">;
type Message = { role: string; content: string | null; tool_calls?: ToolCall[]; tool_call_id?: string };
type ToolCall = { id: string; type: "function"; function: { name: string; arguments: string } };

/** The tool calls handled per turn; the rest are answered with an error. */
export const maxCallsPerTurn = 64;
/** The gateway's bound on one request body (internal/gateway maxBody). */
export const maxRequestBytes = 8 << 20;

const count = (v: unknown) => (Number.isSafeInteger(v) && (v as number) >= 0 ? (v as number) : 0);
const cap = (v: unknown, n: number) => (typeof v === "string" ? v.slice(0, n) : null);

/** toolCalls keeps a reply's well-formed tool calls, rebuilt with capped
 *  strings and nothing else; null when one is not an object. */
function toolCalls(raw: unknown): ToolCall[] | null {
  if (raw === undefined || raw === null) return [];
  if (!Array.isArray(raw) || raw.some((c) => typeof c !== "object" || c === null)) return null;
  return raw.flatMap((c) => {
    const [id, name, args] = [cap(c.id, 256), cap(c.function?.name, 128), cap(c.function?.arguments, maxRequestBytes)];
    return id !== null && name !== null && args !== null ? [{ id, type: "function" as const, function: { name, arguments: args } }] : [];
  });
}

/** usageOf is a response's usage report, or null when it has none. Its cost
 *  is the upstream's when reported (OpenRouter's usage.cost, in USD), else
 *  zero marked as an estimate: the controller prices the tokens. */
function usageOf(u: any, ms: number): Usage | null {
  const valid = (v: unknown) => Number.isSafeInteger(v) && (v as number) >= 0;
  // Without both mandatory counters the report is unknown: the caller counts the whole allowance.
  if (typeof u !== "object" || u === null || !valid(u.prompt_tokens) || !valid(u.completion_tokens)) return null;
  const cached = Math.min(count(u.prompt_tokens_details?.cached_tokens), count(u.prompt_tokens));
  const cost = typeof u.cost === "number" && Number.isFinite(u.cost) && u.cost >= 0 ? Math.round(u.cost * 1e6) : null;
  return {
    tokens: { in: count(u.prompt_tokens) - cached, out: count(u.completion_tokens), cache_read: cached, cache_write: 0 },
    turns: 1,
    milliseconds: ms,
    cost: { amount_micros: cost ?? 0, currency: "USD", basis: "cash", estimate: cost === null },
  };
}

/** sum is the field-wise sum of usage reports. */
function sum(us: Usage[]): Usage {
  const t = (f: (u: Usage) => number) => us.reduce((n, u) => n + f(u), 0);
  return {
    tokens: { in: t((u) => u.tokens.in), out: t((u) => u.tokens.out), cache_read: t((u) => u.tokens.cache_read), cache_write: t((u) => u.tokens.cache_write) },
    turns: t((u) => u.turns),
    milliseconds: t((u) => u.milliseconds),
    cost: { amount_micros: t((u) => u.cost.amount_micros), currency: "USD", basis: "cash", estimate: us.some((u) => u.cost.estimate) },
  };
}

export class HttpChatAdapter extends Adapter {
  private readonly socketPath?: string;
  private prepared: { p: AdapterPrepareParams; gateway: Gateway; workspace: Workspace; tools: string[] } | null = null;

  /** socketPath: the gateway's Unix socket inside the sandbox (ADC_GATEWAY_SOCKET). */
  constructor(opts: { socketPath?: string } = {}) {
    super();
    this.socketPath = opts.socketPath;
  }

  async describe(): Promise<AdapterDescribeResult> {
    return {
      harness: "http-chat",
      harness_version: "0.1.0",
      adapter_version: "0.1.0",
      contract_version: "1.0.0",
      capabilities: ["patch", "decision", "openai_chat"],
      session_semantics: "fresh_process",
      tool_surface: Object.keys(toolSpecs),
      telemetry: { usage: true, upstream: true, stream_deltas: true, per_turn_coverage: true },
      host_side: false,
      needs_native_login: false,
    };
  }

  async prepare(p: AdapterPrepareParams): Promise<AdapterOutcome> {
    this.prepared = null;
    const refuse = (detail: string): AdapterOutcome => ({ outcome: "could_not_run", reason: "executor_incomplete", detail });
    if (p.role !== "builder" && p.role !== "reviewer") return refuse(`http-chat serves the builder and reviewer roles, not ${p.role}`);
    if (p.gateway === null) return refuse("http-chat needs the controller's gateway; it never calls a provider directly");
    if (p.native_login !== null) return refuse("http-chat takes no native login");
    if (p.tool_policy.mediated.length > 0) return refuse("http-chat serves no controller-mediated tool yet");
    // A reviewer gets no tool: its context is exactly its prompt (P16).
    const tools = p.role === "builder" ? Object.keys(toolSpecs).filter((t) => p.tool_policy.allowed.includes(t)) : [];
    const writable = p.role === "builder" && p.workspace.writable;
    this.prepared = { p, gateway: { ...p.gateway, socketPath: this.socketPath }, workspace: new Workspace(p.workspace.path, writable), tools: writable ? tools : tools.filter((t) => t === "read_file" || t === "list_dir") };
    return clean;
  }

  /** input reads an attempt-scoped handle from exchange.in, checking its
   *  attempt, name, size and digest. */
  private async input(h: Handle): Promise<string> {
    const { p } = this.prepared!;
    if (h.attempt !== p.attempt || !handlePattern.test(h.handle) || h.size > maxInputBytes) throw new Error(`the input ${h.handle} is not this attempt's, or is over ${maxInputBytes} bytes`);
    const f = await open(join(p.exchange.in, h.handle), constants.O_RDONLY | constants.O_NOFOLLOW);
    try {
      const bytes = await f.readFile();
      if (bytes.length !== h.size || digestOf(bytes) !== h.digest) throw new Error(`the input ${h.handle} does not match its handle`);
      return bytes.toString("utf8");
    } finally {
      await f.close();
    }
  }

  /** tool runs one workspace tool call; its result is the model's to read. */
  private async tool(c: ToolCall): Promise<string> {
    const { workspace: w, tools } = this.prepared!;
    const name = c.function.name;
    if (!tools.includes(name)) return `error: there is no tool ${JSON.stringify(name)}`;
    let a: any;
    try {
      a = JSON.parse(c.function.arguments);
    } catch {
      return "error: the arguments are not JSON";
    }
    try {
      if (name === "read_file") return await w.read(a?.path);
      if (name === "list_dir") return await w.list(a?.path ?? "");
      if (name === "write_file") await w.write(a?.path, a?.content);
      else await w.remove(a?.path);
      return "ok";
    } catch (e) {
      if (e instanceof ConfinementError) return `refused: ${e.message}`;
      return `error: ${(e as NodeJS.ErrnoException).code ?? "the file operation failed"}`;
    }
  }

  async *run(r: AdapterRunParams, signal: AbortSignal): AsyncGenerator<EventBody, AdapterRunResult> {
    const prep = this.prepared;
    // serve fences every run; the adapter refuses a stale token too, before any request.
    if (!prep || r.attempt !== prep.p.attempt || r.fencing_token !== prep.p.fencing_token) throw new Error("not the prepared attempt and fencing token");
    const { p, gateway, workspace, tools } = prep;
    const role = p.role as "builder" | "reviewer";
    const ended = (reason: string, detail: string, outcome: "could_not_run" | "failed" = "could_not_run"): AdapterRunResult => ({ outcome, reason, detail, result: null, usage_final: null });
    let schema: unknown = defaultDecisionSchema;
    let check: Validator;
    const messages: Message[] = [];
    let size = 0; // the bytes of messages, kept as they grow
    const push = (m: Message) => void (messages.push(m), (size += Buffer.byteLength(JSON.stringify(m)) + 1));
    try {
      const prompt = await this.input(r.prompt);
      if (role === "reviewer" && r.response_schema) schema = JSON.parse(await this.input(r.response_schema));
      check = new Ajv2020({ allErrors: false, strict: false }).compile(schema);
      push({ role: "system", content: role === "reviewer" ? reviewerSystem(schema) : system[role] });
      push({ role: "user", content: prompt });
    } catch (e) {
      return ended("executor_incomplete", `http-chat could not read its inputs: ${(e as Error).message}`);
    }
    const deadline = AbortSignal.any([signal, AbortSignal.timeout(p.limits.wall_seconds * 1000)]);
    const usages: Usage[] = [];
    let complete = true;
    let outTokens = 0;
    for (let turn = 1; turn <= p.limits.max_turns; turn++) {
      const sub = `r${turn}`;
      yield { event: "call.sub", sub_call: sub, model_requested: p.model };
      yield { event: "turn.start", turn, sub_call: sub, reported_at: new Date().toISOString() };
      const body: ChatRequest = { model: p.model, messages, max_tokens: Math.max(1, p.limits.max_output_tokens - outTokens), stream: false };
      if (tools.length) Object.assign(body, { tools: tools.map((t) => ({ type: "function", function: { name: t, description: toolSpecs[t].description, parameters: { type: "object", properties: toolSpecs[t].properties, required: Object.keys(toolSpecs[t].properties) } } })), tool_choice: "auto" });
      if (role === "reviewer") body.response_format = { type: "json_schema", json_schema: { name: "decision", schema } };
      if (Buffer.byteLength(JSON.stringify({ ...body, messages: [] })) + size > maxRequestBytes) return ended("executor_incomplete", `the conversation would pass the gateway's ${maxRequestBytes}-byte request bound`);
      const began = Date.now();
      let reply;
      try {
        reply = await chat(gateway, body, deadline);
      } catch (e) {
        if (signal.aborted) return ended("attempt_stopped", "cancelled during a model request");
        return ended("executor_incomplete", `the gateway request failed: ${(e as Error).message}`);
      }
      if (reply.status < 200 || reply.status > 299) return yield* this.failed(classify(reply), reply.text, turn);
      let res: any;
      try {
        res = JSON.parse(reply.text);
      } catch {
        res = null;
      }
      const msg = res?.choices?.[0]?.message;
      if (typeof msg !== "object" || msg === null) return yield* this.failed({ outcome: "could_not_run", reason: "degenerate_output", hint: "degenerate", detail: "the reply holds no message" }, reply.text, turn);
      const content = typeof msg.content === "string" ? msg.content : null;
      const calls = toolCalls(msg.tool_calls);
      const finish: unknown = res.choices[0].finish_reason;
      if (calls === null) return yield* this.failed({ outcome: "could_not_run", reason: "degenerate_output", hint: "degenerate", detail: "a tool call is not an object" }, reply.text, turn);
      const model = typeof res.model === "string" && res.model.length > 0 ? res.model.slice(0, 256) : "unreported";
      const provider = typeof res.provider === "string" && res.provider.length > 0 ? res.provider.slice(0, 256) : "openai-compatible";
      yield { event: "upstream", turn, sub_call: sub, provider, model_reported: model };
      if (content) yield { event: "delta.text", turn, bytes: Buffer.byteLength(content) };
      if (calls.length) yield { event: "delta.tool_call", turn, bytes: Buffer.byteLength(JSON.stringify(calls)) };
      const u = usageOf(res.usage, Date.now() - began);
      if (u) {
        yield { event: "usage", sub_call: sub, sequence: usages.length, ...u };
        usages.push(u);
        outTokens += u.tokens.out;
      } else {
        // Unreported usage counts as the whole allowance asked for: the limit still ends the loop.
        complete = false;
        outTokens += body.max_tokens as number;
      }
      push({ role: "assistant", content, ...(calls.length ? { tool_calls: calls } : {}) });
      for (const [i, c] of calls.entries()) {
        if (i >= maxCallsPerTurn) {
          push({ role: "tool", tool_call_id: c.id, content: `error: at most ${maxCallsPerTurn} tool calls are run per turn` });
          continue;
        }
        const out = await this.tool(c);
        push({ role: "tool", tool_call_id: c.id, content: out });
        yield { event: "tool.result", turn, tool: tools.includes(c.function.name) ? c.function.name : "unknown", mediated_by_controller: false, delivered: null, bytes: Buffer.byteLength(out) };
      }
      yield { event: "coverage", turn, observable: calls.length ? "partial" : "complete", unobservable: calls.length ? ["harness_tool"] : [] };
      yield { event: "turn.end", turn, sub_call: sub, reported_at: new Date().toISOString() };
      if (signal.aborted) return ended("attempt_stopped", "cancelled");
      const final = complete ? sum(usages) : null;
      if (calls.length && outTokens < p.limits.max_output_tokens) continue;
      if (calls.length) return ended("executor_incomplete", "the output-token limit ended the edit loop");
      if (role === "reviewer") {
        let decision: unknown;
        try {
          // Strict: a duplicated key (two decisions), a lone surrogate or a non-integer number is refused.
          decision = content?.isWellFormed() ? parseStrict(content, 100_000) : undefined;
        } catch {
          decision = undefined;
        }
        if (decision === undefined || !check(decision)) return yield* this.failed({ outcome: "could_not_run", reason: "degenerate_output", hint: "degenerate", detail: "the decision does not match its response schema" }, content ?? "", turn);
        const h = await writeOutput(p.exchange.out, p.attempt, "decision.json", JSON.stringify(decision));
        return { outcome: "found", reason: "completed_with_findings", detail: "the reviewer's decision", result: { kind: "decision", handle: h }, usage_final: final };
      }
      // A builder's last turn (no tool call) ends complete only when the
      // provider says it stopped: one cut short (length, content_filter,
      // none reported) or one reporting tool calls it delivered none of
      // (tool_calls; Mythos #417 L1) is the executor's incomplete run, never
      // a clean end or a partial patch reported as found.
      if (finish !== "stop") return ended("executor_incomplete", `the model's last turn ended on finish_reason ${JSON.stringify(finish ?? null).slice(0, 64)}, not stop`);
      let patch: string;
      try {
        patch = await workspace.patch();
      } catch (e) {
        if (e instanceof ConfinementError) return ended("executor_incomplete", `no admissible patch: ${e.message}`);
        throw e;
      }
      if (patch === "") return { outcome: "empty", reason: "completed_clean", detail: "", result: null, usage_final: final };
      const h = await writeOutput(p.exchange.out, p.attempt, "patch.diff", patch);
      return { outcome: "found", reason: "completed_with_findings", detail: "the builder's patch", result: { kind: "patch", handle: h }, usage_final: final };
    }
    return ended("executor_incomplete", `the turn limit (${p.limits.max_turns}) ended the edit loop`);
  }

  /** failed ends the run on f. A hinted failure reports an error event with
   *  the reply's first 4 KiB as its excerpt; a gateway refusal of the
   *  attempt (stale, routing) writes nothing. */
  private async *failed(f: Failure, text: string, turn: number): AsyncGenerator<EventBody, AdapterRunResult> {
    const { p } = this.prepared!;
    if (f.hint) {
      const excerpt = await writeOutput(p.exchange.out, p.attempt, `error-${turn}.txt`, Buffer.from(text).subarray(0, 4096));
      yield { event: "error", turn, class_hint: f.hint, raw_excerpt: excerpt };
    }
    return { outcome: f.outcome, reason: f.reason, detail: f.detail, result: null, usage_final: null };
  }
}
