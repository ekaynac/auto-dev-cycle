# 11 — Testing: sandboxed tests, fakes, T0, red-proofs

Carries nöbetçi lesson 1.4 ("a test must prove which red it catches, and must verify its own setup"). M0 review
[M10][M15] applied.

## Tests run inside a sandbox [M10, operator ruling D37]

In-process guards cannot stop direct filesystem calls or child processes. So **every test process tree runs inside a
sandbox in which the real home and state do not exist**:
- **Default:** bubblewrap (`bwrap`) on Linux:
  - a fresh tmpfs as `/home` and `/tmp`;
  - the repository bind-mounted read-only, plus one writable scratch directory;
  - `--unshare-net` (no network) unless a test declares a loopback-only need;
  - `--unshare-pid --die-with-parent`;
  - `--clearenv` with an explicit allowlisted environment (no `*_TOKEN`, `*_KEY`, `ANTHROPIC_*`, `OPENAI_*`,
    `OPENROUTER_*`, `CODEX_HOME`, `CLAUDE_*`).
- **Fallback (deferred to M3, D42):** a rootless container with the same mounts and policy, for hosts without
  bubblewrap (macOS developers, CI runners without user namespaces). Until it exists, bubblewrap is the only test
  sandbox; building it is a security-posture change that waits for the operator.
- **No sandbox, no tests:** `make test` refuses to run tests directly on the host. `ADC_TEST_UNSANDBOXED=1` exists only
  for the sandbox's own bootstrap tests.
- **Red tests for the containment itself**, using **sacrificial sentinels**: the harness creates a decoy directory
  outside the sandbox (e.g. `/tmp/adc-sentinel-<rand>`, never the real home) and asserts that:
  - a direct Go `os.WriteFile`, a TS `fs.writeFileSync`, a child `sh -c 'echo > …'` and a symlink trick all fail to
    touch it;
  - no credential-like environment variable is visible;
  - a network connection fails.

  A broken sandbox fails these tests against the decoy and can never damage the real home.
- `internal/testguard` remains as a second, in-process layer (temp roots for the store and blob store, a fake clock).
  It is defence in depth, not the boundary.

## Contract-faithful fakes

| Fake | Faithful to | Lives in |
|---|---|---|
| Fake adapter | the full JSON-RPC contract: cumulative patches, streaming gaps, sub-calls, usage, errors, stale submissions, unsafe-output modes | `adapters/fake` |
| Fake forge | push with expected-base, change requests, merge, ambiguous remote outcomes, external base moves | `internal/testguard/fakes/forge` |
| Fake sandbox | three phases, artifact-before-teardown, evidence manifest, abrupt loss | `…/fakes/sandbox` |
| Fake provider | quota headers, 429 with reset, unknown quota, **402 credit exhaustion mid-call**, separate rate window and balance, degenerate output, single-chunk tool arguments | `…/fakes/provider` |
| Fake clock | deadlines, resets, TTLs, challenge expiry | `…/fakes/clock` |

When a real implementation is found to differ from its fake, the fake is fixed first, with a test that fails on the
old fake.

## M0 exercises real validators, not scripted answers [M15]

M0's fixture runs drive the **real** M0 components:
- vocabulary;
- event store and writer;
- authority with effective-change classification;
- lease, attempt and usage state machines;
- receipt derivation;
- profile validation;
- ContextManifest schemas.

Fakes stand in only for external dependencies (adapters, forge, sandbox, provider, clock). For each fault fixture the
golden file names:
- **the invariant it targets**, e.g. "receipt derivation must not yield `empty` with a missing required predicate";
- **the expected `(outcome, reason)` sequence**;
- **the mutation that must turn it red**, e.g. "make `Derive()` skip the completeness check".

## T0 fixtures (`fixtures/t0/`)

- **Repos:** `py-mini`, `ts-mini`, `go-mini`, `php-mini`, `multi-service` (two services + a database + a bus). Their
  task sets (feature, bugfix, refactor, failing-then-green) are defined now and run for real from M1.
- **M0 contract fixtures** (run in M0 against real validators):
  - nested receipt lie;
  - omitted required predicate;
  - unknown reviewer family;
  - unrelated GO reuse;
  - concurrent conflicting approvals;
  - concurrent last-unit reservation;
  - idempotency key reuse with a different input;
  - stale expected state;
  - a revoked attempt's late result;
  - stale-attempt usage still counted;
  - duplicate usage deltas;
  - an effective-change widening disguised as a profile edit;
  - a protected-document change carrying a policy widening;
  - a replayed/expired WebAuthn challenge (with a fake authenticator);
  - a stream after grant revocation;
  - identical bytes with different lineage;
  - declared-only coverage;
  - unsafe adapter outputs (symlink, FIFO, digest mismatch, cross-attempt handle);
  - a hash-chain edit and truncation;
  - a second daemon.
- **Later fixtures, defined now, implemented in their owning milestone:**
  - lying test output;
  - protected-path edit;
  - test deletion/skip;
  - provider 4xx;
  - idle cut;
  - hostile repo;
  - external base move;
  - non-JUnit results;
  - missing `.git` at runtime;
  - non-English requirements;
  - crash at each effect boundary (03);
  - the knowledge fixtures in 10.

## Legacy incident goldens (nöbetçi intake A11)

- Real incident artifacts from nöbetçi's `t/fixtures/` are copied read-only, **anonymised** (hosts, ids, personal
  names, product names replaced), into `fixtures/legacy/` as data-only goldens. Each gets a one-line expected
  `(outcome, reason)`. Examples:
  - the inline-fallback-after-limit shape;
  - a reviewer's new walk demand with an unchanged observation;
  - candidate-owned setup errors on a `.git`-less target;
  - the closure-routing record.
- The nöbetçi incident→test index (`/home/mega/nobetci-dev/docs/handoff/2026-10-03/08-incident-test-index.md`, 206
  verified test names) is the checklist of behaviours T0 must reproduce. Each row maps to a T0 fixture, or to a
  documented reason it does not apply.
- **Tests use the shape of the live record** (intake B5, `docs/research/2026-10-05-nobetci-phase1-close-intake.md`;
  nöbetçi L2). A fixture for code that reads a recorded thing is built from a captured instance of that record, never
  from the shape the last incident happened to have. That covers a usage report, a call's terminal record, an attempt
  disposition and a receipt.
  - **Capture.** An instance is captured at a stated boundary:
    - the adapter's stream, for adapter messages;
    - the controller's event log, for controller-owned records such as dispositions and receipts;
    - the ledger, for liabilities.

    The source is a real run, anonymised into `fixtures/legacy/` (whose README already records legacy sources), or a
    run of the fake adapter (`adapters/fake`) under a named scenario that reproduces it.
  - **Provenance.** Each capture carries metadata: the producer and its version, the source run or fake scenario, the
    capture boundary, and every transformation applied (anonymisation, trimming). Transformations preserve missing
    versus null fields, types, the relationships between records, and their order.
  - **Partial forms.** Each record type keeps corpus cases for its partial forms beside its complete one. The canonical
    case is a call killed mid-stream. Its stream is captured with the terminal result genuinely absent; `completed:
    false` is the controller's derivation, not a field the adapter sent. A terminal result carrying `usage_final:
    null` is a different case.
    - **Reason.** It is `could_not_run` with the reason the exit map derives from everything observed
      (`internal/vocab/exitmap`). A capacity cause from the call's error events or the controller's own observed
      statuses comes first: credit (`broker_credit_exhausted`), then quota (`quota_exhausted`). Only without one is it
      `attempt_stopped`, when the controller recorded `attempt.stop_requested` (a request, not confirmed termination),
      or otherwise `adapter_protocol_error`.
    - **Liability.** Until reconciliation it stays an **uncertain liability** (05) of its best estimate: the
      deduplicated sum of its usage deltas when there are any, otherwise the call's reservation. That amount is
      subtracted from available capacity. The call is never read as finished, and never as free.
- **"Cannot occur" fixtures for the artifact graph** are listed in `13-artifact-graph.md` (D40). They take
  priority: each proves a nöbetçi failure class is structurally impossible.
- **New contract fixtures from the intake:**
  - a ruling answers an owed input and re-keys the review;
  - a ruling for an input not owed is not delivered;
  - a new demand gets new evidence attempts and re-keys;
  - a precondition failure spends no attempt;
  - an unrestored evidence run blocks;
  - a 402 mid-call with an applied candidate is sealed, never "no changes";
  - a fallback outside its role/mode scope is refused;
  - a review lease is refused while its gate results are for another tree;
  - an answer approval that does not bind the owed input's revision is refused; a ruling cannot satisfy a receipt input;
  - a receipt for another demand with identical conclusions does not apply;
  - a crash between evidence collection and cleanup leaves no applicable receipt; a new job on an excluded target is refused; a late result from a superseded run is refused;
  - total-cap exhaustion across demands;
  - an unchanged `conclusions_digest` does not trigger re-review; a changed step outcome or changed restrictions do;
  - strict pair: an unavailable member holds (no refutation substitute); the contested path with and without refutation; a receipt-only claim cannot pass on reviewer text;
  - broker affordability with outstanding reservations and uncertain liabilities; a stale or unknown balance waits;
  - an import error outside the changed files that passes through a changed test file stays a runner fault;
  - a missing answer yields `awaiting_input` and recovers on the answering approval without a manual release; a precondition failure recovers on its probe;
  - an acceptance row not producible by its owner is refused by the plan validator (M5).
  - a call killed mid-stream (its stream captured with no terminal result; `completed: false` derived by the
    controller): without a capacity cause it is `attempt_stopped` when a stop was requested, otherwise
    `adapter_protocol_error`; with a `credit_exhausted` hint or an observed 402 it is `broker_credit_exhausted`, and
    with a `rate_limited` hint or an observed 429 `quota_exhausted`, with or without a stop request. Its liability is
    the deduplicated delta sum, or its positive reservation when no delta was reported, and available capacity drops
    by that amount (B5).

## Red-proof rule [M15]

- A `fix(...)` change must include a test that fails on the old code. CI checks it **mechanically**: it reverts the
  non-test part of the change in a scratch worktree, runs the named tests inside the sandbox, and requires them to fail
  there, then pass on the change. A trailer alone is not proof.
- Before comparing two test runs, case counts must be equal.
- Tests execute behaviour. Assertions on source text are rejected by lint.

## Live-provider probes are not in M0 [M15]

Structured-output dialect probes against real providers need credentials and network. They run in M1/M2 as a separate,
explicitly authorised job, never in `make test` or `make ci`.

## Installed-artifact exit check [M15]

`make dist` builds the release artifacts. The M0 exit check (00) installs them into a clean sandboxed prefix and runs
the checks against the **installed binaries**, never the source tree.
