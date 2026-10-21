// SPDX-License-Identifier: Apache-2.0
// The adapter conformance kit (docs/design/m0/09 "Conformance kit"): the
// round-trip, the controller-mediated tool round-trip, a cancel during a tool
// call and an oversize message. It runs an adapter as the core does, one process per case
// over stdio, and treats its output as untrusted: every message is checked as a
// JSON-RPC envelope and against the generated validators (never trusting the
// adapter's SDK), replies are correlated with requests, and resources are
// bounded (a deadline that kills the process tree and settles the session
// without waiting for end of output, aggregate output limits, stderr
// drained). Beyond the schemas it checks per-turn lifecycles, sub-calls
// announced before use, call-wide usage numbering, one coverage per completed
// turn (partial for compaction and harness tools), the heartbeat interval and
// the contract's major version. Mediated tools are checked from the kit's own
// side of the exchange: each delivery carries a handle no adapter can guess,
// and must be reported once, for its turn and tool, after it was supplied.
// A case returns findings; none means the adapter conforms on that case.

import { spawn } from "node:child_process";
import { randomBytes, randomInt } from "node:crypto";
import { createRequire } from "node:module";
import { performance } from "node:perf_hooks";
import { classify, frame, lines, maxMessageBytes, parseStrict } from "@adc/contract";

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
/** The values the kit builds from one message: far above any contract
 *  message, far below what exhausts its heap. */
const maxValues = 100_000;
const stderrTail = 64 << 10;
/** The period of the kit's own stall watch during the run. */
const stallTickMs = 10;

type Msg = Record<string, unknown> & { id?: unknown; result?: any; error?: any; method?: unknown; params?: any };

/** show formats any value for a finding, whatever the adapter sent. */
const show = (v: unknown, n = 200) => {
  let t: string;
  try {
    t = JSON.stringify(v) ?? String(v);
  } catch {
    t = String(v);
  }
  return t.slice(0, n);
};

/** contain runs the command in a new PID namespace (bubblewrap), so killing
 *  it ends every process the adapter started, whatever sessions or groups
 *  they made. Only the PID namespace is new: files and network are the
 *  caller's. It bounds the lifetime of what the adapter starts itself; it is
 *  not a boundary against a hostile adapter, which can still ask a service
 *  outside the namespace to start work. Containing that is a separate change. */
const contain = (command: string[]) => ["bwrap", "--unshare-pid", "--die-with-parent", "--dev-bind", "/", "/", "--proc", "/proc", "--", ...command];

/** StallWatch records the kit's own event-loop stalls: a tick that comes
 *  late marks time the kit could not observe arrivals. stop() samples once
 *  more, so a stall still pending when the watch closes is kept. */
export class StallWatch {
  readonly stalls: [number, number][] = [];
  private timer: NodeJS.Timeout | null = null;
  private last = 0;

  start() {
    this.stop();
    this.last = performance.now();
    this.timer = setInterval(() => this.sample(), stallTickMs);
  }

  stop() {
    if (!this.timer) return;
    this.sample();
    clearInterval(this.timer);
    this.timer = null;
  }

  private sample() {
    const now = performance.now();
    if (now - this.last > 2 * stallTickMs) this.stalls.push([this.last + stallTickMs, now]);
    this.last = now;
  }
}

/** Session is one adapter process under the kit's control. */
class Session {
  readonly findings: Finding[] = [];
  readonly events: { at: number; e: any }[] = [];
  stderr = "";
  private queue: Msg[] = [];
  private readonly waiters = new Set<() => void>();
  /** onEvent sees each event of the run as it arrives (the tool exchange). */
  onEvent: ((e: any) => void) | null = null;
  /** inputDone: the adapter's output ended; stopped: cleanup ran. */
  private inputDone = false;
  private stopped = false;
  private frozen = false;
  private readonly pending = new Map<number, string>();
  private readonly answered = new Set<number>();
  private next = 1;
  private readonly child;
  private readonly timer;
  readonly exited: Promise<{ code: number | null; signal: string | null } | null>;
  replied = false;
  /** readDone settles when the reader has taken all of the adapter's output. */
  private readonly readDone: Promise<void>;
  /** stalls: the kit's own event-loop stalls during the run. */
  readonly stalls = new StallWatch();
  private markStopped!: () => void;
  /** whenStopped settles when stop() runs. */
  private readonly whenStopped = new Promise<"stopped">((r) => (this.markStopped = () => r("stopped")));
  /** run: from the run request to its answer; events outside it are findings. */
  runWindow: { from: number; to: number | null } | null = null;

  readonly limits: Limits;

  constructor(a: AdapterUnderTest, limits: Limits) {
    this.limits = limits;
    const [cmd, ...args] = contain(a.command);
    this.child = spawn(cmd, args, { env: { ...process.env, ...a.env }, stdio: ["pipe", "pipe", "pipe"], detached: true });
    this.exited = new Promise((r) => {
      this.child.on("exit", (code, signal) => {
        if (!this.replied && !this.stopped) this.find("spawn", `the adapter ended before answering anything (${signal ?? `exit ${code}`}): ${this.stderr.slice(-300)}`);
        r({ code, signal });
      });
      this.child.on("error", (e) => {
        this.find("spawn", `the adapter could not run under its PID namespace (bubblewrap): ${e.message}`);
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
    this.readDone = this.read();
  }


  find(check: string, detail: string) {
    if (!this.frozen && this.findings.length < maxFindings) this.findings.push({ check, detail: detail.slice(0, 500) });
  }

  /** stop is the one cleanup path, idempotent: it ends the run window,
   *  cancels the deadline, kills the PID namespace (and the process group),
   *  destroys the streams and settles every wait. */
  stop() {
    if (this.stopped) return;
    this.stopped = true;
    if (this.runWindow && this.runWindow.to === null) this.runWindow.to = performance.now();
    this.stalls.stop();
    clearTimeout(this.timer);
    for (const target of [this.child.pid, this.child.pid && -this.child.pid]) {
      try {
        if (target) process.kill(target, "SIGKILL");
      } catch {
        // already gone
      }
    }
    this.child.stdout.destroy();
    this.child.stderr.destroy();
    this.child.stdin.destroy();
    this.wake();
    this.markStopped();
  }

  /** finish stops the session and returns its findings, fixed: nothing
   *  added after the case returns. */
  finish(): Finding[] {
    this.stop();
    this.frozen = true;
    return [...this.findings];
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
      if (!this.stopped) {
        this.find("framing", `the output stream broke: ${(e as Error).message}`);
        this.stop();
      }
    }
    this.inputDone = true;
    this.wake();
  }

  /** accept checks one line's envelope and routes it. */
  private accept(line: string) {
    let m: unknown;
    try {
      m = parseStrict(line, maxValues);
    } catch (e) {
      return this.find("framing", `a line that is not strict JSON: ${(e as Error).message}`);
    }
    const c = classify(m);
    if (c.kind === "invalid") {
      const what = c.why === "an adapter.event without object params" ? show(m) : line.slice(0, 200);
      return this.find("envelope", `${c.why}: ${what}`);
    }
    if (c.kind === "event") {
      const now = performance.now();
      if (!this.runWindow || this.runWindow.to !== null) return this.find("run-boundary", `an event outside the run: ${show(c.params)}`);
      // Only a valid event is kept: an invalid one is a finding, never retained.
      if (!validators["adapter/event"](c.params)) return this.find("event", `an event that does not match adapter/event: ${show(c.params)}`);
      this.events.push({ at: now, e: c.params });
      this.onEvent?.(c.params);
      return;
    }
    const msg = c.reply as Msg;
    const id = msg.id as number;
    if (this.answered.has(id)) return this.find("correlation", `a second reply for request ${id}`);
    if (!this.pending.has(id)) return this.find("correlation", `a reply for request ${id}, which was not asked`);
    this.pending.delete(id);
    this.answered.add(id);
    this.replied = true;
    this.queue.push(msg);
    this.wake();
  }

  /** wake settles every request waiting for a reply or the session's end. */
  private wake() {
    for (const w of this.waiters) w();
    this.waiters.clear();
  }

  /** request sends one request and waits for its reply (null if the session
   *  ended first). padTo pads the line with whitespace inside the JSON to
   *  exactly that many bytes, so its size is its only difference. */
  async request(method: string, params?: unknown, padTo?: number): Promise<Msg | null> {
    if (this.stopped || this.inputDone) return null;
    const id = this.next++;
    this.pending.set(id, method);
    if (method === "run") {
      this.runWindow = { from: performance.now(), to: null };
      this.stalls.start();
    }
    const msg = { jsonrpc: "2.0", id, method, params };
    const body = padTo === undefined ? "" : JSON.stringify(msg);
    this.child.stdin.write(padTo === undefined ? frame(msg) : `${body.slice(0, -1)}${" ".repeat(padTo - Buffer.byteLength(body))}}\n`);
    for (;;) {
      const i = this.queue.findIndex((m) => m.id === id);
      if (i >= 0) {
        const [m] = this.queue.splice(i, 1);
        if (method === "run" && this.runWindow) {
          this.runWindow.to = performance.now();
          this.stalls.stop();
        }
        return m;
      }
      if (this.stopped || this.inputDone) return null;
      await new Promise<void>((r) => this.waiters.add(r));
    }
  }

  /** end waits for the process to exit and the reader to take all of its
   *  output after shutdown, within graceMs and the session's deadline, so
   *  output after the last reply is still checked; only a normal exit 0
   *  passes. */
  async end(graceMs: number) {
    if (this.stopped) return; // the kit ended it: its exit is no verdict
    let timer: NodeJS.Timeout | undefined;
    const late = new Promise<"late">((r) => (timer = setTimeout(() => r("late"), graceMs)));
    let exited = false;
    void this.exited.then(() => (exited = true));
    const done = Promise.all([this.exited, this.readDone]).then(([e]) => e);
    const exit = await Promise.race([done, late, this.whenStopped]);
    clearTimeout(timer);
    if (exit === "late") this.find("exit", exited ? `the process exited, but its output did not end within ${graceMs} ms of shutdown` : `the process did not exit within ${graceMs} ms of shutdown`);
    else if (exit !== "stopped" && exit !== null && !this.stopped && (exit.code !== 0 || exit.signal !== null)) {
      this.find("exit", `the process ended ${exit.signal ? `by signal ${exit.signal}` : `with code ${exit.code}`} after shutdown, want exit 0`);
    }
  }
}

const handleOf = (name: string) => ({ attempt, handle: name, digest: "sha256:" + "0".repeat(64), size: 0 });
/** The harness-internal tools the kit's policy allows (tool_policy.allowed),
 *  less any the case mediates: a tool is the harness's own or mediated, never both. */
const harnessTools = ["Edit"];
const prepareParams = (allowed: string[], mediated: string[]) => ({ attempt, fencing_token: token, role: "builder", executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", model: "m",
  workspace: { path: "/workspace", writable: true }, exchange: { in: "/exchange/in", out: "/exchange/out" }, inputs: [], limits: { max_turns: 8, max_output_tokens: 8000, wall_seconds: 600 },
  tool_policy: { allowed, mediated }, gateway: null, native_login: null });
const runParams = { attempt, fencing_token: token, call, prompt: handleOf("prompt.md"), response_schema: null };

/** result checks a reply: a result (not an error) valid against schema. */
function result(s: Session, method: string, reply: Msg | null, schema: string): any {
  if (!reply) return s.find(method, `no reply to ${method}`), null;
  if ("error" in reply) return s.find(method, `${method} failed: ${show(reply.error, 300)}`), null;
  if (!validators[schema](reply.result)) return s.find(method, `the ${method} result does not match ${schema}: ${show(validators[schema].errors, 300)}`), null;
  return reply.result;
}

/** checkEvents applies the rules a schema cannot to the run's events. */
function checkEvents(s: Session) {
  const announced = new Set<string>();
  const turns = new Map<number, { ended: boolean; coverage: { observable: string; unobservable: string[] }[]; compaction: boolean; harnessTool: boolean }>();
  let sequence = 0;
  for (const { e } of s.events) {
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
      if (seen && (c.observable !== "partial" || !c.unobservable.includes(cause))) s.find("coverage", `turn ${n} had ${cause} but declared ${c.observable} ${JSON.stringify(c.unobservable)}`);
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
  // Arrival times include the kit's own scheduling: the part of a gap the
  // kit itself was stalled is no verdict on the adapter, the rest is.
  const stalled = (a: number, b: number) => s.stalls.stalls.reduce((t, [f, u]) => t + Math.max(0, Math.min(b, u) - Math.max(a, f)), 0);
  for (let i = 1; i < marks.length; i++) {
    const gap = Math.round(marks[i] - marks[i - 1]);
    if (gap <= bound) continue;
    const own = Math.round(stalled(marks[i - 1], marks[i]));
    if (gap - own > bound) s.find("heartbeat", `${gap} ms between heartbeats during the run, ${own} ms of it the kit's own stall (09: every ≤ ${s.limits.heartbeatMs} ms)`);
    else s.find("inconclusive", `a heartbeat gap of ${gap} ms, ${own} ms of it the kit's own stall`);
  }
}

type Handle = { attempt: string; handle: string; digest: string; size: number };
/** sameHandle compares handles field by field, whatever their key order. */
const sameHandle = (a: Handle, b: Handle | null) => !!b && a.attempt === b.attempt && a.handle === b.handle && a.digest === b.digest && a.size === b.size;

/** Tools is the kit's side of the controller-mediated tool exchange (09). A
 *  tool.request outside the policy, or with a handle of another attempt, is a
 *  finding that ends the case. In "answer" mode each request gets a delivery
 *  under a fresh, unguessable handle, which the adapter must acknowledge; in
 *  "cancel" mode the first request is answered by a cancel instead, and its
 *  response stays pending. Every exchange settles before the case returns. */
class Tools {
  readonly deliveries: { turn: number; tool: string; handle: Handle; at: number; acked: boolean; reported: boolean }[] = [];
  private readonly seen = new Set<string>();
  private readonly exchanges: Promise<void>[] = [];
  cancelled = false;
  private readonly s: Session;
  private readonly mediated: string[];
  /** allowed: the harness-internal tools of the policy the kit sends. */
  readonly allowed: string[];
  private readonly mode: "answer" | "cancel";

  constructor(s: Session, mediated: string[], mode: "answer" | "cancel") {
    this.s = s;
    this.mediated = mediated;
    this.allowed = harnessTools.filter((t) => !mediated.includes(t));
    this.mode = mode;
    s.onEvent = (e) => this.requested(e);
  }

  get requests() {
    return this.seen.size;
  }

  private requested(e: any) {
    if (e?.event !== "tool.request" || !validators["adapter/event"](e)) return; // checkEvents reports an invalid one
    const s = this.s;
    if (!this.mediated.includes(e.tool)) return s.find("tool-policy", `a tool.request for ${e.tool}, which the tool policy does not mediate`), s.stop();
    if (e.request.attempt !== attempt) return s.find("tool", `a tool.request handle of another attempt: ${show(e.request)}`), s.stop();
    if (this.seen.has(e.request.handle)) return s.find("tool", `a second tool.request with handle ${e.request.handle}`);
    this.seen.add(e.request.handle);
    if (this.mode === "answer") this.exchanges.push(this.answer(e));
    else if (!this.cancelled) this.exchanges.push(this.cancel());
  }

  /** settle waits for every exchange the kit started; run it after stop. */
  async settle() {
    await Promise.all(this.exchanges);
  }

  /** exercised: a tool case needs its request, and in "answer" mode one
   *  complete exchange, a delivery acknowledged and reported, whatever the
   *  run's outcome. */
  exercised() {
    if (!this.mediated.length) return;
    if (!this.requests) return this.s.find("tool", `the run never requested ${this.mediated.join(", ")}, so the case was not exercised`);
    if (this.mode === "answer" && !this.deliveries.some((d) => d.acked && d.reported)) this.s.find("tool", "no tool exchange completed: no delivery was both acknowledged and reported");
  }

  private async answer(e: any) {
    const handle = { attempt, handle: `delivered-${this.deliveries.length + 1}`, digest: "sha256:" + randomBytes(32).toString("hex"), size: 1 + randomInt(1 << 20) };
    const d = { turn: e.turn, tool: e.tool, handle, at: performance.now(), acked: false, reported: false };
    this.deliveries.push(d);
    const reply = await this.s.request("tool.response", { attempt, fencing_token: token, call, turn: e.turn, tool: e.tool, request: e.request,
      outcome: "empty", reason: "completed_clean", detail: "", delivered: handle });
    const empty = reply && "result" in reply && typeof reply.result === "object" && reply.result !== null && !Array.isArray(reply.result) && !Object.keys(reply.result).length;
    if (!reply) this.s.find("tool", `no acknowledgement of the tool.response to ${e.request.handle}`);
    else if (!empty) this.s.find("tool", `tool.response answered ${show(reply)}, want an empty result`);
    else d.acked = true;
  }

  private async cancel() {
    this.cancelled = true;
    result(this.s, "cancel", await this.s.request("cancel", { attempt, fencing_token: token }), "adapter/outcome");
  }
}

/** checkTools: a harness-internal tool.result names a tool the policy
 *  allows; each mediated one reports one delivery the kit supplied, for its
 *  turn and tool, once, after it was supplied; a completed run has reported
 *  every delivery. */
function checkTools(s: Session, t: Tools, completed: boolean) {
  for (const { at, e } of s.events) {
    if (e?.event !== "tool.result") continue;
    if (e.mediated_by_controller === false && typeof e.tool === "string" && !t.allowed.includes(e.tool)) {
      s.find("tool-policy", `a harness tool.result for ${e.tool}, which the tool policy does not allow`);
    }
    if (e.mediated_by_controller !== true) continue;
    const d = t.deliveries.find((x) => !x.reported && x.turn === e.turn && x.tool === e.tool && x.at < at && sameHandle(x.handle, e.delivered));
    if (d) d.reported = true;
    else s.find("tool", `a mediated tool.result that reports no delivery the kit supplied for turn ${e.turn} and ${e.tool} (or reports one twice, or before it was supplied): ${show(e.delivered)}`);
  }
  if (!completed) return;
  for (const d of t.deliveries) if (!d.reported) s.find("tool", `the delivery ${d.handle.handle} for turn ${d.turn} and ${d.tool} was never reported by a mediated tool.result`);
}

interface Case {
  /** The tools the policy mediates; a case with a tool requires its request. */
  mediated: string[];
  onToolRequest: "answer" | "cancel";
}

/** runCase: describe, prepare, run and shutdown; every reply correlated and
 *  valid; the run's events kept to the contract; the process exits 0 after
 *  shutdown. Tool responses and a cancel are sent while the run is open. */
async function runCase(a: AdapterUnderTest, limits: Partial<Limits>, c: Case): Promise<Finding[]> {
  const s = new Session(a, { ...defaultLimits, ...limits });
  const tools = new Tools(s, c.mediated, c.onToolRequest);
  try {
    const d = result(s, "describe", await s.request("describe"), "adapter/describe-result");
    if (d && Number(String(d.contract_version).split(".")[0]) !== contractMajor) s.find("version", `contract version ${d.contract_version}, this kit checks major ${contractMajor}`);
    const p = result(s, "prepare", await s.request("prepare", prepareParams(tools.allowed, c.mediated)), "adapter/outcome");
    if (p && p.outcome !== "empty" && p.outcome !== "found") s.find("prepare", `prepare refused the kit's ordinary attempt: ${p.outcome}/${p.reason}`);
    let completed = false;
    if (d && p) {
      const reply = await s.request("run", runParams);
      const r = result(s, "run", reply, "adapter/run-result");
      completed = r?.outcome === "empty" || r?.outcome === "found";
      if (!reply) s.stop(); // no answer: the run window closes at termination
    }
    if (s.runWindow) {
      checkEvents(s);
      checkHeartbeats(s);
      checkTools(s, tools, completed);
    }
    result(s, "shutdown", await s.request("shutdown"), "adapter/outcome");
    await s.end(5_000);
  } finally {
    s.stop();
    await tools.settle();
  }
  if (s.runWindow) tools.exercised();
  return s.finish();
}

/** roundTrip: the plain case, with no tool mediated. */
export const roundTrip = (a: AdapterUnderTest, limits: Partial<Limits> = {}) => runCase(a, limits, { mediated: [], onToolRequest: "answer" });

/** toolRoundTrip: the run requests the mediated tool, the kit delivers, and
 *  the run reports exactly that delivery (09 "controller-mediated tool
 *  round-trip"). */
export const toolRoundTrip = (a: AdapterUnderTest, tool: string, limits: Partial<Limits> = {}) => runCase(a, limits, { mediated: [tool], onToolRequest: "answer" });

/** cancelDuringTool: the kit cancels while the tool's response is pending.
 *  Cancel is best effort (09, A4): the run may still complete or stop, but
 *  its reply, the cancel's reply and the whole session must be valid, and no
 *  mediated result may claim a delivery that never came. */
export const cancelDuringTool = (a: AdapterUnderTest, tool: string, limits: Partial<Limits> = {}) => runCase(a, limits, { mediated: [tool], onToolRequest: "cancel" });

/** within resolves with the promise's value, or "late" after ms. */
async function within<T>(p: Promise<T>, ms: number): Promise<T | "late"> {
  let timer: NodeJS.Timeout | undefined;
  const late = new Promise<"late">((r) => (timer = setTimeout(() => r("late"), ms)));
  try {
    return await Promise.race([p, late]);
  } finally {
    clearTimeout(timer);
  }
}

/** overGrace is how long the one-byte-over request may take to be rejected,
 *  in times the at-bound request's round trip: the same bytes, timed in the
 *  same session, so a loaded host slows both alike (#226 entry 102). */
const overGrace = 4;

/** oversize: a request of exactly the framing bound (09: 16 MiB) is served;
 *  the same request one byte over is rejected observably, by an error reply
 *  to it or by the process ending, never served and never ignored. The two
 *  differ only in whitespace inside the JSON. The rejection is awaited for
 *  overGrace times the at-bound request's time, and at least graceMs. */
export async function oversize(a: AdapterUnderTest, limits: Partial<Limits> = {}, graceMs = 5_000): Promise<Finding[]> {
  const s = new Session(a, { ...defaultLimits, ...limits });
  try {
    result(s, "describe", await s.request("describe"), "adapter/describe-result");
    const start = performance.now();
    result(s, "describe", await s.request("describe", undefined, maxMessageBytes), "adapter/describe-result");
    const atBound = performance.now() - start, grace = Math.max(graceMs, overGrace * atBound);
    const reply = await within(s.request("describe", undefined, maxMessageBytes + 1), grace);
    if (reply === "late") {
      return s.find("oversize", `no observable rejection within ${Math.round(grace)} ms of a ${maxMessageBytes + 1}-byte message (the ${maxMessageBytes}-byte one took ${Math.round(atBound)} ms): neither an error reply nor the process ending`), s.finish();
    }
    if (reply && "result" in reply) return s.find("oversize", `a ${maxMessageBytes + 1}-byte message was served`), s.finish();
    // No reply: the output ended, which happens only once bubblewrap (which
    // holds it too) has exited with everything in its PID namespace, so the
    // process ended, whatever its code. An adapter that merely closes its
    // output is still running, and shows as no rejection.
    if (!reply) return s.finish();
    result(s, "shutdown", await s.request("shutdown"), "adapter/outcome");
    await s.end(graceMs);
  } finally {
    s.stop();
  }
  return s.finish();
}
