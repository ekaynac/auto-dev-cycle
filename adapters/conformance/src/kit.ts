// SPDX-License-Identifier: Apache-2.0
// The adapter conformance kit (docs/design/m0/09 "Conformance kit"): it runs
// an adapter as the core does, one process per case over stdio, validates
// everything the adapter sends against the generated validators (never
// trusting the adapter's own SDK), and checks the contract's rules a schema
// cannot: per-turn ordering, sub-calls announced before use, gapless usage
// numbering, coverage per completed turn, liveness, the mediated tool
// round-trip, cancellation and the message bound. A case returns findings;
// none means the adapter conforms on that case.

import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { frame, lines, maxMessageBytes, parseStrict } from "@adc/contract";

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

type Msg = { id?: unknown; result?: any; error?: { code: number; message: string }; method?: string; params?: any };
type Seen = { at: number; msg: Msg };

/** The fixed identities the kit drives an attempt with. */
export const attempt = "att_01m3250vnfeyrv67x6w0ddqr6a";
export const call = "cal_01m3250v9xft08ssr4ygq6g2np";
const token = 3;
const handleOf = (name: string) => ({ attempt, handle: name, digest: "sha256:" + "0".repeat(64), size: 0 });
const prepareParams = (mediated: string[]) => ({
  attempt, fencing_token: token, role: "builder", executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", model: "m",
  workspace: { path: "/workspace", writable: true }, inputs: [], limits: { max_turns: 8, max_output_tokens: 8000, wall_seconds: 600 },
  tool_policy: { allowed: [], mediated }, gateway: null, native_login: null,
});
const runParams = { attempt, fencing_token: token, call, prompt: handleOf("prompt.md"), response_schema: null };
const req = (id: number, method: string, params?: unknown) => ({ jsonrpc: "2.0", id, method, params });

/** The heartbeat bound (09: every ≤ 10 s), with slack for scheduling. */
export const livenessMs = 10_000 + 1_500;

/** session runs one adapter process: it sends describe, prepare and run,
 *  lets react answer messages as they arrive, sends shutdown once the run
 *  answers, and returns everything seen. It gives up after deadlineMs. */
async function session(a: AdapterUnderTest, opts: { mediated?: string[]; deadlineMs?: number; raw?: string; react?: (m: Msg, send: (r: unknown) => void) => void } = {}) {
  const child = spawn(a.command[0], a.command.slice(1), { env: { ...process.env, ...a.env }, stdio: ["pipe", "pipe", "pipe"] });
  const seen: Seen[] = [];
  const findings: Finding[] = [];
  const send = (r: unknown) => {
    if (!child.stdin.destroyed) child.stdin.write(typeof r === "string" ? r : frame(r));
  };
  const exited = new Promise<number | null>((r) => child.on("exit", (code) => r(code)));
  let done = false;
  const deadline = setTimeout(() => {
    findings.push({ check: "deadline", detail: `the session did not finish within ${opts.deadlineMs ?? 60_000} ms` });
    child.kill("SIGKILL");
  }, opts.deadlineMs ?? 60_000);
  if (opts.raw !== undefined) send(opts.raw);
  send(req(1, "describe"));
  send(req(2, "prepare", prepareParams(opts.mediated ?? [])));
  send(req(3, "run", runParams));
  const runSentAt = Date.now();
  try {
    for await (const line of lines(child.stdout)) {
      let msg: Msg;
      try {
        msg = parseStrict(line) as Msg;
      } catch (e) {
        findings.push({ check: "framing", detail: `a line that is not strict JSON: ${(e as Error).message}` });
        continue;
      }
      seen.push({ at: Date.now(), msg });
      opts.react?.(msg, send);
      if (msg.id === 3 && !done) {
        done = true;
        send(req(9, "shutdown"));
      }
    }
  } catch (e) {
    findings.push({ check: "framing", detail: `the output stream broke: ${(e as Error).message}` });
  }
  const code = await exited;
  clearTimeout(deadline);
  child.stdin.destroy();
  return { seen, findings, code, runSentAt };
}

const isEvent = (m: Msg) => m.method === "adapter.event";

/** checkSession applies the rules every completed session must keep. */
function checkSession(seen: Seen[], runSentAt: number): Finding[] {
  const f: Finding[] = [];
  const answer = (id: number) => seen.find((s) => s.msg.id === id)?.msg;
  const describe = answer(1);
  if (!describe?.result || !validators["adapter/describe-result"](describe.result)) f.push({ check: "describe", detail: `describe gave no valid result: ${JSON.stringify(describe)}` });
  const run = answer(3);
  if (!run?.result) f.push({ check: "run", detail: `the run gave no result: ${JSON.stringify(run?.error ?? run)}` });
  else if (!validators["adapter/run-result"](run.result)) f.push({ check: "run", detail: `the run result does not match adapter/run-result: ${JSON.stringify(validators["adapter/run-result"].errors)}` });
  const events = seen.filter((s) => isEvent(s.msg)).map((s) => s.msg.params);
  const announced = new Set<string>();
  const open = new Map<number, { covered: number }>();
  let sequence = 0;
  for (const e of events) {
    if (!validators["adapter/event"](e)) {
      f.push({ check: "event", detail: `an event that does not match adapter/event: ${JSON.stringify(e)}` });
      continue;
    }
    if (e.attempt !== attempt || e.fencing_token !== token || e.call !== call) f.push({ check: "envelope", detail: `an event not stamped for this call: ${e.event}` });
    if (e.event === "call.sub") announced.add(e.sub_call);
    if ("sub_call" in e && e.event !== "call.sub" && !announced.has(e.sub_call)) f.push({ check: "sub-call", detail: `${e.event} names sub-call ${e.sub_call} before call.sub announced it` });
    if (e.event === "usage") {
      if (e.sequence !== sequence) f.push({ check: "usage-sequence", detail: `usage sequence ${e.sequence}, want ${sequence} (numbered per call from 0)` });
      sequence = e.sequence + 1;
    }
    if (e.event === "turn.start") open.set(e.turn, { covered: 0 });
    else if ("turn" in e && e.event !== "turn.start" && !open.has(e.turn)) f.push({ check: "turn-order", detail: `${e.event} for turn ${e.turn} before its turn.start` });
    if (e.event === "coverage" && open.has(e.turn)) open.get(e.turn)!.covered++;
    if (e.event === "turn.end" && open.get(e.turn)?.covered !== 1) f.push({ check: "coverage", detail: `turn ${e.turn} ended with ${open.get(e.turn)?.covered ?? 0} coverage declarations, want 1` });
  }
  // Liveness: from the run request to its answer, no silence over the bound.
  const marks = [runSentAt, ...seen.filter((s) => s.msg.id === 3 || isEvent(s.msg)).map((s) => s.at)];
  for (let i = 1; i < marks.length; i++) {
    const gap = marks[i] - marks[i - 1];
    if (gap > livenessMs) f.push({ check: "liveness", detail: `${gap} ms without a message during the run (heartbeat at least every 10 s)` });
  }
  return f;
}

/** roundTrip: describe, prepare, run, shutdown; the session's rules; the
 *  process ends after shutdown. */
export async function roundTrip(a: AdapterUnderTest): Promise<Finding[]> {
  const { seen, findings, code, runSentAt } = await session(a);
  const f = [...findings, ...checkSession(seen, runSentAt)];
  if (!seen.find((s) => s.msg.id === 9)?.msg.result) f.push({ check: "shutdown", detail: "shutdown gave no result" });
  if (code !== 0) f.push({ check: "exit", detail: `the process exited ${code} after shutdown` });
  return f;
}

/** mediatedTool: the kit answers the adapter's tool.request; its
 *  tool.result must report exactly the delivered handle. */
export async function mediatedTool(a: AdapterUnderTest, tool: string): Promise<Finding[]> {
  const delivered = handleOf("tool-output.txt");
  const { seen, findings, runSentAt } = await session(a, {
    mediated: [tool],
    react: (m, send) => {
      if (isEvent(m) && m.params.event === "tool.request") {
        send(req(4, "tool.response", { attempt, fencing_token: token, call, turn: m.params.turn, tool: m.params.tool, request: m.params.request,
          outcome: "found", reason: "gate_test_failed", detail: "the kit's tool failed", delivered }));
      }
    },
  });
  const f = [...findings, ...checkSession(seen, runSentAt)];
  const events = seen.filter((s) => isEvent(s.msg)).map((s) => s.msg.params);
  if (!events.some((e) => e.event === "tool.request")) f.push({ check: "tool", detail: `no tool.request for ${tool}` });
  const result = events.find((e) => e.event === "tool.result" && e.mediated_by_controller);
  if (!result || JSON.stringify(result.delivered) !== JSON.stringify(delivered)) f.push({ check: "tool", detail: `the mediated tool.result does not report the delivered handle: ${JSON.stringify(result)}` });
  return f;
}

/** cancel: a cancel after the first event ends the run with could_not_run
 *  within the deadline; nothing is reported as done. */
export async function cancel(a: AdapterUnderTest): Promise<Finding[]> {
  let sent = false;
  const { seen, findings } = await session(a, {
    deadlineMs: 20_000,
    react: (m, send) => {
      if (!sent && isEvent(m) && m.params.event !== "heartbeat") {
        sent = true;
        send(req(5, "cancel", { attempt, fencing_token: token }));
      }
    },
  });
  const f = [...findings];
  const run = seen.find((s) => s.msg.id === 3)?.msg;
  if (run?.result?.outcome !== "could_not_run") f.push({ check: "cancel", detail: `a cancelled run must end could_not_run; got ${JSON.stringify(run?.result ?? run?.error)}` });
  return f;
}

/** oversize: a message over the bound is refused, never processed: the
 *  adapter answers the line with an error or ends; it never runs from it. */
export async function oversize(a: AdapterUnderTest): Promise<Finding[]> {
  const big = JSON.stringify(req(7, "describe", { pad: "x".repeat(maxMessageBytes) })) + "\n";
  const { seen, findings } = await session(a, { raw: big, deadlineMs: 20_000 });
  const f = findings.filter((x) => x.check !== "framing");
  if (seen.some((s) => s.msg.id === 7 && s.msg.result)) f.push({ check: "oversize", detail: "the adapter answered an oversize message" });
  return f;
}
