# Writing an executor adapter

Status: draft (M0). The single guide to writing, testing and registering an adapter (plan §2.3; operator request D3).
The normative contract is `docs/design/m0/09-executor-adapter-contract.md` plus the schemas in `schemas/adapter/`.
Where this guide and the schemas disagree, the schemas win, and this guide has a bug.

## 1. What an adapter is

An adapter lets auto-dev-cycle drive one **harness** (a coding-agent CLI like Claude Code, Codex, opencode or omp, or a
plain HTTP model API) as an **executor**. The core decides *what* runs, *where* and *under which policy*. The adapter
only translates and reports.

An adapter must never:
- mint ids;
- decide policy or classify outcomes beyond hints;
- read credentials other than what `prepare` hands it;
- talk to a model except through the endpoint the core gives it (the gateway, for sandboxed roles).

## 2. Start from the SDK

```
adapters/
  contract/   @adc/contract: generated types, JSON-RPC framing, base class
  fake/       a complete, faithful example: read it first
  <yours>/
```

```ts
import { Adapter, run } from "@adc/contract";

class MyHarness extends Adapter {
  async describe() { /* harness, versions, capabilities, telemetry, host_side */ }
  async prepare(p) { /* set up workspace, tool policy, gateway endpoint */ }
  async *run(p, signal, tools) { /* spawn the harness, translate its stream into events, yield them */ }
  async cancel(p) { /* best effort */ }
}
run(new MyHarness());
```

The SDK handles framing, strict parsing, validation of everything sent and received, fencing-token stamping and
heartbeats. `run` also receives an `AbortSignal`, aborted on cancel, shutdown or a broken input: stop the harness and
return. Blob-exchange helpers are not in the SDK yet.

A **controller-mediated tool** (a test run in the sandbox) is a round-trip:

1. Yield `tool.request` with its request handle.
2. `await tools.response(handle)` for the core's `tool.response`. Its `delivered` handle is the tool's output, or the
   core's diagnostic when the tool failed or could not run.
3. Hand that content to the harness, and yield `tool.result` with `mediated_by_controller: true` and exactly that
   `delivered` handle.

The SDK answers the core with an error for a `tool.response` that is outside the fence or the active call, that
names no request this run yielded (the whole request handle must match), that answers one twice, that names another
turn or tool, or whose `delivered` handle belongs to another attempt.

Your own mistakes **fail the whole run** with an internal error, and the offending event is not sent:
- a request handle used twice, or one naming another attempt;
- a mediated `tool.result` that does not report a handle the core delivered for that turn and tool;
- reporting one delivery twice.

`tools.response` rejects in four cases:
- the handle was not yielded first;
- it is already being awaited;
- the run is aborted;
- the run ends while you still wait.

## 3. Translate the stream honestly

- Emit `turn.*`, `delta.*`, `tool.result`, `usage`, `upstream`, `compaction` and `error` as they happen. Sizes and
  timing matter more than content.
- Report **per-call usage deltas**, never session totals.
- If the harness cannot tell you something (usage, the upstream that served the turn, the tool results it injected),
  declare it in `describe().telemetry`. Never invent numbers. Unknown is a valid answer and is recorded as `uncertain`.
- `error.class_hint` is only a hint. The core's rules decide.

## 4. Files and handles

- `prepare` passes the exchange root as `exchange: {in, out}`. Inputs arrive as **attempt-scoped handles** in `in/`.
  Write each output as one regular file directly in `out/`, named exactly by its handle, and return the handle with
  its digest and size.
- The controller ingests `out/` once, after your process tree has exited, safely: no symlinks, FIFOs, devices or hard
  links; size bounds; digest checked while copying. Anything else is rejected as `unsafe_output` and marks the attempt untrustworthy.
- Never reference another attempt's handle.

## 4a. Results

- `patch`: cumulative against the attempt's base tree, written to `out/`. Only the patch crosses the boundary.
- `text` / `decision`: written to `out/`. For `decision`, validate against `response_schema_ref` before returning.
- Every result carries the attempt's fencing token. A stale or revoked attempt's output is rejected by the core.
  Its usage reports are still accepted, because a stale attempt may still have been billed.
- On `could_not_run` or `failed`, return `result: null`. Never fake an artifact.

## 5. Host-side adapters (Claude Code, Codex)

If `host_side: true`, use only the scoped `native_login` credential copy the core provides (never the operator's home directory). Run test requests as controller-mediated `tool.request`s. `prepare` must also:
- disable repo hooks, MCP servers, plugins, settings, instruction files and native memory;
- disable subagents and model switching;
- run only the tool policy the core provides, inside the confinement the core sets up.

The hostile-repository conformance cases check this.

## 6. Test and register

1. `pnpm --filter <yours> test`: unit tests run under the test guard (no real HOME, no network).
2. `pnpm conformance <yours>`: the conformance kit (round-trips, streams, errors, patch boundary, stale tokens, cancel,
   oversize, and hostile-repo for host-side adapters). All cases must pass.
3. Register in the source registry config (`schemas/config/source-registry.json`) as an executor kind. Registration
   is an approval-bound action.
4. Open a PR. Adapter PRs get an independent review like every framework change.

## 7. Versioning

Declare `contract_version`. The core refuses a different major version. When the contract gains optional fields,
adapters keep working; adopt them when you can.
