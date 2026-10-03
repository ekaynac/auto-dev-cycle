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
- **Fallback:** a rootless container with the same mounts and policy, for hosts without bubblewrap (macOS developers,
  CI runners without user namespaces).
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
| Fake provider | quota headers, 429 with reset, unknown quota, degenerate output, single-chunk tool arguments | `…/fakes/provider` |
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
