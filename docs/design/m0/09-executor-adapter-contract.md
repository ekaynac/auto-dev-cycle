# 09 — Executor adapter contract

The process contract between the Go core (`internal/adapterhost`) and an executor adapter. Implements plan §2.3–2.4 and
M0 review [M9][M11][M13]. `docs/adapters/AUTHORING.md` is the author-facing guide.

## Process model

- One adapter process per attempt. Sessions are always fresh per attempt, even when an adapter declares
  `reusable_process`.
- **JSON-RPC 2.0, newline-delimited JSON over stdio**: one message per line, UTF-8, max 16 MiB. stderr is logs,
  captured to a blob with secrets redacted by type.
- The adapter never mints authoritative ids, never decides policy, and reports only what happened.

## Blob exchange: the ingestion boundary [M9]

- Per attempt, the core creates an exchange root owned by the controller:
  - `in/`: files the core wrote, read-only to the adapter;
  - `out/`: a directory the adapter may write.

  For sandboxed roles these live inside the sandbox mount. For host-side roles they sit inside the confinement (§2.4).
- Payloads are passed as **attempt-scoped handles** `{attempt, handle, digest, size}`, never as raw paths. Handles are
  valid only for their attempt. A handle naming another attempt is refused.
- **Ingestion of `out/` by the controller:**
  1. resolve the path with `openat2(RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS)`, or an equivalent
     component-by-component walk on systems without it;
  2. accept regular files only (no symlinks, devices, FIFOs, sockets, hard links to outside files: link count 1);
  3. enforce size bounds per media type;
  4. stream-copy into the controller's blob store while hashing;
  5. compare with the declared digest;
  6. publish (03).

  The controller never reads an adapter file in place twice. It reads it once, copying, so a concurrent replacement
  cannot change what was verified.
- Any violation rejects the output with `failed/unsafe_output` and records the attempt as untrustworthy.

## Methods (core → adapter)

| Method | Params | Result (discriminated by `outcome`) [M13] |
|---|---|---|
| `describe` | — | `{harness, harness_version, adapter_version, contract_version, capabilities, session_semantics, tool_surface, telemetry: {usage, upstream, stream_deltas, per_turn_coverage: bool}, host_side: bool, needs_native_login: bool}` |
| `prepare` | `{attempt, fencing_token, role, executor, model, workspace, inputs: [handle], limits, tool_policy, gateway: {base_url, token}|null, native_login: {credential_ref, scope}|null}` | `{outcome, reason, detail}` |
| `run` | `{attempt, fencing_token, call: cal_…, prompt: handle, response_schema: handle|null}` | streams notifications, then `{outcome, reason, detail, result: {kind: patch|text|decision, handle}|null, usage_final: measurement}`. `result` is **null** whenever `outcome` is `could_not_run` or `failed` |
| `cancel` | `{attempt, fencing_token}` | `{outcome, reason}`; best effort, never relied on (A4) |
| `shutdown` | — | `{outcome, reason}` |

- **`native_login`** [M11]: host-side harnesses that use the operator's subscription login get a narrowly scoped
  reference. That is a copy of only the credential files the harness needs, placed inside the confinement for the
  attempt's duration and removed at teardown. Never the operator's whole home directory.
- **`call`** is the call-ledger id. The core records the call's intent (03 effects) before sending `run`. A harness that
  makes several model requests in one run reports each as a sub-call (`call.sub`, below), so usage and coverage attach
  to identified calls.

## Notifications (adapter → core)

All carry `attempt`, `fencing_token`, `call`. They are normalised in `schemas/adapter/event.json`:

| Event | Fields | Feeds |
|---|---|---|
| `call.sub` | `{sub_call, model_requested}` | call ledger (sub-call identities) |
| `turn.start` / `turn.end` | `{turn, sub_call, reported_at}` | timing |
| `delta.text` / `delta.thinking` / `delta.tool_call` | `{turn, bytes}`; content to the transcript handle | stream gaps (Relace lesson) |
| `tool.request` | `{turn, tool, request: handle}` for a **controller-mediated** tool (e.g. "run test X in the sandbox") | the core executes it and answers with a `tool.response` request to the adapter |
| `tool.result` | `{turn, tool, mediated_by_controller, delivered: handle|null, bytes}` | ContextManifest *delivered* coverage: a controller-mediated result references the exact content handle the core supplied |
| `coverage` | `{turn, observable: complete|partial|none, unobservable: [harness_instructions|compaction|harness_tool|other]}` | ContextManifest coverage declaration |
| `usage` | a usage report: `{sub_call, sequence, tokens, turns, milliseconds, cost}`, numbered per call from 0. The controller builds the `UsageMeasurement` (05): it mints the id, takes the lease and attempt from the fenced envelope, records kind `delta` and sets `source_of_truth` | ledger |
| `upstream` | `{turn, sub_call, provider, model_reported}` | routing: reported vs verified |
| `compaction` | `{turn, before_tokens, after_tokens}` | coverage (turn becomes partial) |
| `error` | `{turn, class_hint, raw_excerpt: handle}` | the core's rules classify |
| `heartbeat` | every ≤10 s | liveness |

**Coverage is derived from evidence** [M11]:
- *assembled* is what the controller built;
- *delivered* is the initial prompt plus controller-mediated tool results whose handles the core itself supplied;
- everything the adapter only declares stays *declared*, and it cannot make coverage `complete` by declaration alone.

A turn with harness-internal tools or compaction is `partial`. Usage notifications are accepted even from a stale
attempt (accounting is not fenced; 05). Outputs and checkpoint contributions are fenced.

## Host-side harness rules (plan §2.4, A1)

With `host_side: true`, `prepare` must:
- operate on the read-only snapshot under the core's OS confinement;
- disable repo-provided hooks, MCP servers, plugins, settings, instruction files and native memory;
- disable subagents and model switching;
- expose only the passed tool policy, with test runs as controller-mediated tools;
- use only the scoped `native_login`.

The hostile-repository conformance cases verify each rule on the installed harness version.

## Versioning

`contract_version` is semver. The core refuses a different major version.

## Conformance kit (`adapters/conformance`)

Every adapter must pass these before registration:
- round-trips;
- stream normalisation;
- sub-call identity;
- error classes (4xx, 429, **402 / credit exhaustion mid-call** with partial work, 5xx, idle cut, degenerate);
- patch boundary (cumulative patch; declared paths only);
- stale-token output rejection with usage still accepted;
- cancel during a tool call;
- oversize message;
- **unsafe output:** a symlink escape, a FIFO, a digest mismatch, a file replaced after write, a handle from another
  attempt;
- controller-mediated tool round-trip;
- **hostile repository** (host-side only).

The kit proves its own red: a deliberately broken fake adapter must fail it.

## Fakes

`adapters/fake` implements the whole contract faithfully: cumulative patches, streaming gaps, sub-calls, usage, errors,
stale submissions, unsafe-output modes for kit testing.
