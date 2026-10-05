// SPDX-License-Identifier: Apache-2.0
// The adapter conformance kit (docs/design/m0/09 "Conformance kit"), part 1:
// the round-trip. It runs an adapter as the core does, one process per case
// over stdio, and treats it as untrusted: every message is checked as a
// JSON-RPC envelope and against the generated validators (never trusting the
// adapter's SDK), replies are correlated with requests, and resources are
// bounded (a deadline that kills the process tree and settles the session
// without waiting for end of output, aggregate output limits, stderr
// drained). Beyond the schemas it checks per-turn lifecycles, sub-calls
// announced before use, call-wide usage numbering, one coverage per completed
// turn (partial for compaction and harness tools), the heartbeat interval and
// the contract's major version. A case returns findings; none means the
// adapter conforms on that case.

import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { performance } from "node:perf_hooks";
import { frame, lines, parseStrict } from "@adc/contract";

type Validator = ((doc: unknown) => boolean) & { errors?: unknown };
const validators: Record<string, Validator> = createRequire(import.meta.url)("@adc/contract-gen/validators.cjs");

/** The adapter under test: the command the host would spawn. */
export interface AdapterUnderTest {
  command: string[];
  env?: Record<string, string>;
}

export interface Finding {
  check: string;
  detail: string;
}

export interface Limits {
  /** The whole session, from spawn to exit. */
  deadlineMs: number;
  /** All stdout bytes, and all messages, the kit accepts from one session. */
  maxOutputBytes: number;
  maxMessages: number;
  /** The heartbeat interval the contract requires (09: every ≤ 10 s). */
  heartbeatMs: number;
  /** Scheduling slack the kit allows on top of heartbeatMs. */
  toleranceMs: number;
}

export const defaultLimits: Limits = { deadlineMs: 120_000, maxOutputBytes: 64 << 20, maxMessages: 100_000, heartbeatMs: 10_000, toleranceMs: 1_000 };

/** The contract major this kit checks against (09 "Versioning"). */
export const contractMajor = 1;
/** The fixed identities the kit drives an attempt with. */
export const attempt = "att_01m3250vnfeyrv67x6w0ddqr6a";
export const call = "cal_01m3250v9xft08ssr4ygq6g2np";
const token = 3;
const maxFindings = 100;
const stderrTail = 64 << 10;

type Msg = Record<string, unknown> & { id?: unknown; result?: any; error?: any; method?: unknown; params?: any };

/** Session is one adapter process under the kit's control. */
class Session {
  readonly findings: Finding[] = [];
  readonly events: { at: number; e: any }[] = [];
  stderr = "";
  private queue: Msg[] = [];
  private waiting: (() => void) | null = null;
  private closed = false;
  private readonly pending = new Map<number, string>();
  private readonly answered = new Set<number>();
  private next = 1;
  private readonly child;
  private readonly timer;
  readonly exited: Promise<number | null>;
  /** run: from the run request to its answer; events outside it are findings. */
  runWindow: { from: number; to: number | null } | null = null;

  readonly limits: Limits;

  constructor(a: AdapterUnderTest, limits: Limits) {
    this.limits = limits;
    this.child = spawn(a.command[0], a.command.slice(1), { env: { ...process.env, ...a.env }, stdio: ["pipe", "pipe", "pipe"], detached: true });
    this.exited = new Promise((r) => {
      this.child.on("exit", (code) => r(code));
      this.child.on("error", (e) => {
        this.find("spawn", `the adapter could not run: ${e.message}`);
        r(null);
      });
    });
    this.child.stdin.on("error", () => {}); // a closed stdin shows as no answer
    this.child.stderr.on("data", (b: Buffer) => {
      this.stderr = (this.stderr + b.toString()).slice(-stderrTail);
    });
    this.timer = setTimeout(() => {
      this.find("deadline", `the session did not finish within ${limits.deadlineMs} ms`);
      this.stop();
    }, limits.deadlineMs);
    void this.read();
  }

  find(check: string, detail: string) {
    if (this.findings.length < maxFindings) this.findings.push({ check, detail: detail.slice(0, 500) });
  }

  /** stop kills the process tree and settles every wait. */
  stop() {
    if (this.closed) return;
    this.closed = true;
    clearTimeout(this.timer);
    try {
      if (this.child.pid) process.kill(-this.child.pid, "SIGKILL");
    } catch {
      // already gone
    }
    this.child.stdout.destroy();
    this.child.stderr.destroy();
    this.child.stdin.destroy();
    this.waiting?.();
  }

  private async read() {
    let bytes = 0;
    const limits = this.limits;
    const counted = (async function* (src: AsyncIterable<Buffer>, self: Session) {
      for await (const chunk of src) {
        bytes += chunk.length;
        if (bytes > limits.maxOutputBytes) {
          self.find("output-bound", `more than ${limits.maxOutputBytes} bytes of output`);
          self.stop();
          return;
        }
        yield chunk;
      }
    })(this.child.stdout, this);
    let count = 0;
    try {
      for await (const line of lines(counted)) {
        if (++count > limits.maxMessages) {
          this.find("output-bound", `more than ${limits.maxMessages} messages`);
          this.stop();
          break;
        }
        this.accept(line);
      }
    } catch (e) {
      if (!this.closed) this.find("framing", `the output stream broke: ${(e as Error).message}`);
    }
    this.closed = true;
    this.waiting?.();
  }

  /** accept checks one line's envelope and routes it. */
  private accept(line: string) {
    let m: unknown;
    try {
      m = parseStrict(line);
    } catch (e) {
      return this.find("framing", `a line that is not strict JSON: ${(e as Error).message}`);
    }
    if (typeof m !== "object" || m === null || Array.isArray(m) || (m as Msg).jsonrpc !== "2.0") {
      return this.find("envelope", `not a JSON-RPC 2.0 message: ${line.slice(0, 200)}`);
    }
    const msg = m as Msg;
    if (msg.method === "adapter.event" && !("id" in msg)) {
      const now = performance.now();
      if (!this.runWindow || this.runWindow.to !== null) return this.find("run-boundary", `an event outside the run: ${JSON.stringify(msg.params).slice(0, 200)}`);
      this.events.push({ at: now, e: msg.params });
      return;
    }
    const isResponse = typeof msg.id === "number" && !("method" in msg) && ("result" in msg) !== ("error" in msg);
    if (!isResponse) return this.find("envelope", `neither a response nor an adapter.event notification: ${line.slice(0, 200)}`);
    const id = msg.id as number;
    if (this.answered.has(id)) return this.find("correlation", `a second reply for request ${id}`);
    if (!this.pending.has(id)) return this.find("correlation", `a reply for request ${id}, which was not asked`);
    this.pending.delete(id);
    this.answered.add(id);
    this.queue.push(msg);
    this.waiting?.();
  }

  /** request sends one request and waits for its reply (null if the session
   *  ended first). */
  async request(method: string, params?: unknown): Promise<Msg | null> {
    if (this.closed) return null;
    const id = this.next++;
    this.pending.set(id, method);
    if (method === "run") this.runWindow = { from: performance.now(), to: null };
    this.child.stdin.write(frame({ jsonrpc: "2.0", id, method, params }));
    for (;;) {
      const i = this.queue.findIndex((m) => m.id === id);
      if (i >= 0) {
        const [m] = this.queue.splice(i, 1);
        if (method === "run" && this.runWindow) this.runWindow.to = performance.now();
        return m;
      }
      if (this.closed) return null;
      await new Promise<void>((r) => (this.waiting = r));
    }
  }

  /** end waits for the process to exit, up to graceMs, then stops it. */
  async end(graceMs: number): Promise<number | null> {
    const code = await Promise.race([this.exited, new Promise<"late">((r) => setTimeout(() => r("late"), graceMs))]);
    this.stop();
    return code === "late" ? (this.find("exit", `the process did not exit within ${graceMs} ms of shutdown`), null) : code;
  }
}

const handleOf = (name: string) => ({ attempt, handle: name, digest: "sha256:" + "0".repeat(64), size: 0 });
const prepareParams = { attempt, fencing_token: token, role: "builder", executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", model: "m",
  workspace: { path: "/workspace", writable: true }, inputs: [], limits: { max_turns: 8, max_output_tokens: 8000, wall_seconds: 600 },
  tool_policy: { allowed: [], mediated: [] }, gateway: null, native_login: null };
const runParams = { attempt, fencing_token: token, call, prompt: handleOf("prompt.md"), response_schema: null };

/** result checks a reply: a result (not an error) valid against schema. */
function result(s: Session, method: string, reply: Msg | null, schema: string): any {
  if (!reply) return s.find(method, `no reply to ${method}`), null;
  if ("error" in reply) return s.find(method, `${method} failed: ${JSON.stringify(reply.error).slice(0, 300)}`), null;
  if (!validators[schema](reply.result)) return s.find(method, `the ${method} result does not match ${schema}: ${JSON.stringify(validators[schema].errors).slice(0, 300)}`), null;
  return reply.result;
}

/** checkEvents applies the rules a schema cannot to the run's events. */
function checkEvents(s: Session) {
  const announced = new Set<string>();
  const turns = new Map<number, { ended: boolean; coverage: { observable: string; unobservable: string[] }[]; compaction: boolean; harnessTool: boolean }>();
  let sequence = 0;
  for (const { e } of s.events) {
    if (!validators["adapter/event"](e)) {
      s.find("event", `an event that does not match adapter/event: ${JSON.stringify(e).slice(0, 200)}`);
      continue;
    }
    if (e.attempt !== attempt || e.fencing_token !== token || e.call !== call) s.find("envelope", `a ${e.event} event not stamped for this call`);
    if (e.event === "call.sub") announced.add(e.sub_call);
    else if (typeof e.sub_call === "string" && !announced.has(e.sub_call)) s.find("sub-call", `${e.event} names sub-call ${e.sub_call} before call.sub announced it`);
    if (e.event === "usage") {
      if (e.sequence !== sequence) s.find("usage-sequence", `usage sequence ${e.sequence}, want ${sequence} (numbered per call from 0)`);
      sequence = e.sequence + 1;
    }
    if (typeof e.turn !== "number") continue;
    const t = turns.get(e.turn);
    if (e.event === "turn.start") {
      if (t) s.find("turn-order", `a second turn.start for turn ${e.turn}`);
      else turns.set(e.turn, { ended: false, coverage: [], compaction: false, harnessTool: false });
      continue;
    }
    if (!t) {
      s.find("turn-order", `${e.event} for turn ${e.turn} before its turn.start`);
      continue;
    }
    if (e.event === "coverage") {
      t.coverage.push(e);
      continue;
    }
    if (t.ended) {
      s.find("turn-order", `${e.event} for turn ${e.turn} after its turn.end`);
      continue;
    }
    if (e.event === "turn.end") t.ended = true;
    if (e.event === "compaction") t.compaction = true;
    if (e.event === "tool.result" && !e.mediated_by_controller) t.harnessTool = true;
  }
  for (const [n, t] of turns) {
    if (!t.ended) continue;
    if (t.coverage.length !== 1) {
      s.find("coverage", `turn ${n} has ${t.coverage.length} coverage declarations, want 1`);
      continue;
    }
    const c = t.coverage[0];
    for (const [seen, cause] of [[t.compaction, "compaction"], [t.harnessTool, "harness_tool"]] as [boolean, string][]) {
      if (seen && (c.observable === "complete" || !c.unobservable.includes(cause))) s.find("coverage", `turn ${n} had ${cause} but declared ${c.observable} ${JSON.stringify(c.unobservable)}`);
    }
  }
}

/** checkHeartbeats: during the run, no gap between heartbeats (counting from
 *  the run request to its answer) is over the interval plus tolerance. */
function checkHeartbeats(s: Session) {
  const w = s.runWindow;
  if (!w || w.to === null) return;
  const marks = [w.from, ...s.events.filter((x) => x.e?.event === "heartbeat").map((x) => x.at), w.to];
  const bound = s.limits.heartbeatMs + s.limits.toleranceMs;
  for (let i = 1; i < marks.length; i++) {
    const gap = Math.round(marks[i] - marks[i - 1]);
    if (gap > bound) s.find("heartbeat", `${gap} ms between heartbeats during the run (09: every ≤ ${s.limits.heartbeatMs} ms)`);
  }
}

/** roundTrip: describe, prepare, run and shutdown, one after another; every
 *  reply correlated and valid; the run's events kept to the contract; the
 *  process exits 0 after shutdown. */
export async function roundTrip(a: AdapterUnderTest, limits: Partial<Limits> = {}): Promise<Finding[]> {
  const s = new Session(a, { ...defaultLimits, ...limits });
  try {
    const d = result(s, "describe", await s.request("describe"), "adapter/describe-result");
    if (d && Number(String(d.contract_version).split(".")[0]) !== contractMajor) s.find("version", `contract version ${d.contract_version}, this kit checks major ${contractMajor}`);
    const p = result(s, "prepare", await s.request("prepare", prepareParams), "adapter/outcome");
    if (p && p.outcome !== "empty" && p.outcome !== "found") s.find("prepare", `prepare refused the kit's ordinary attempt: ${p.outcome}/${p.reason}`);
    if (d && p) {
      result(s, "run", await s.request("run", runParams), "adapter/run-result");
      checkEvents(s);
      checkHeartbeats(s);
    }
    result(s, "shutdown", await s.request("shutdown"), "adapter/outcome");
    const code = await s.end(5_000);
    if (code !== null && code !== 0) s.find("exit", `the process exited ${code} after shutdown`);
  } finally {
    s.stop();
  }
  return s.findings;
}
