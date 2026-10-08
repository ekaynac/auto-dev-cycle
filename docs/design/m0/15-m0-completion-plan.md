# M0 completion: delivery plan

Status: revision 2, 2026-10-08. Draft for the operator.
- Revision 1 was written on origin/main ca581b7, after step 9 of 04 §9 (14) merged with PR 11 (#163). The 9e reads
  are still deferred.
- Revision 2 applies the Mythos review of #165, which was GO with changes (`docs/reviews/2026-10-08-pr165-mythos.md`):
  - M1: a new §1.3 assigns 11's intake fixtures to milestones.
  - M2: G2 uses the existing in-process adapter peer.
  - M3: citations fixed.
  - M4: G1 pre-split, budgets reset, and split lines stated.
  - M5: D6 reframed as the adapter distribution decision.
  - M6: two reviews at a time.
  - M7: the estimate revised.
  - L1-L9: applied, except two L3 corrections. Checked, `matrix.go:49` and `matrix.go:42` are right as written.
- Approved by the operator with the default decisions D1-D8 (merged as 96ca5df).
- Amended by A7, 2026-10-08:
  - D1 and D2 are recorded in `internal/daemon/matrix.go`, `docs/reference/action-matrix.md` and 14;
  - §3.1 records how the G track has actually been delivered so far.
- Amended by S1, 2026-10-08, on the operator's decision (operator 2026-10-08 (Telegram): option C). `adcd` wires no
  lease or attempt action and serves no attempt API in M0 (`internal/daemon/wiring.go`), so no harness can drive an
  attempt against the installed daemon. S1 is therefore a standalone harness with its own temporary state; S3 and O4
  check result 9 from its output. Driving attempts against the installed daemon comes with M1's wired lease and
  attempt actions.

Scope: **everything M0 still owes before its exit check can pass.** 14 tracked four obligations outside step 9. This
note plans them, plus the gaps that a check against 01 §4 and the 00 exit table turned up:
- the artifact graph (13; #114);
- evidence nodes (12; #115);
- the knowledge spec and the legacy goldens (10, 11; #116);
- the `adc doctor` credential-isolation warning (04-authority:379-383; #117). 14 cites this as `:225`, which was its
  line before 04 grew;
- part 2 of the installed smoke: the fake-adapter run (14 §4 item 12, result 9; #162);
- the test write-guard and the contract-faithful fakes (01 §4 M0 row; 01-repo-layout:23; 11:30, 11:33-44).

It does not reopen step 9. Wiring the graph into `adcd` is M1 (§2, L1).

## 1. Scope: what M0 exit requires, against what is delivered

### 1.1 Exit criteria

**01 §4, M0 row (01:635), exit column:**
- CI green;
- schema docs generated from code;
- a fake-driven T0 dry run passes.

**00 "M0 exit check" (00:46-58),** run against installed artifacts:

| Row | Check | State on ca581b7 |
|---|---|---|
| CI green | `make ci` | green; it chains lint, gen-check, vocab-check, test, fixtures-contract, release-guard, test-sandbox-selftest, dist and install-check |
| Clean install | `make dist && scripts/install-check.sh` | delivered: installs `adcd`, `adc` and `adc-agent` |
| Contract fixtures | `adc dev fixtures --contract`, installed | delivered for the 20 fixtures of the M0 list (11:67-87), each with its mutation (`scripts/installed-fixtures.sh`). **The fixtures of 12 and 13 are missing** |
| Containment | `make test-sandbox-selftest` | delivered |
| Schemas documented | `make schema-docs` | delivered; it will cover the new schemas |
| Vocabulary closed | `adc dev vocab check` | delivered; it will cover the new holds and reasons |
| Log integrity | `adc dev verify-log` | delivered (also in the installed smoke) |

The fake-driven T0 dry run is #162's result 9: the fake adapter on `py-mini-feature-1` ends with
`attempt.result_submitted` applied and a `usage.measured` record (14 §4 item 12), from the harness's own state (S1).

### 1.2 The M0 row's contents (01:635), item by item

| Item | Delivered | Remaining |
|---|---|---|
| Repo layout, licence, CI | yes | none |
| Schemas: outcomes, reasons, authority, lease/attempt/fencing, receipt, event store, project profile | yes | none |
| `ContextManifest`, Ref, ViewFingerprint, coverage levels | yes (`schemas/records/manifest-*`, `ref`, `view-fingerprint`; `internal/knowledge`) | none |
| Knowledge spec draft: schemas, invariants, fixture definitions (K10, K12) | `spec/knowledge/` has `vocabulary.md`, `invariants.md`, 13 fixture definitions and golden vectors | **O5**: `FailureSignature` and `Claim` draft schemas (10:97-98); stale status lines in `spec/knowledge/README.md:34-36` and `spec/knowledge/schemas/README.md:10-15` |
| Artifact graph: schemas, key computation, resolver, evaluator with fakes (13) | nothing | **O1** |
| Evidence-node parameters (12) | instrument contracts, artifact classes in `instrument-contract.json`, lease restore events | **O2** |
| Legacy goldens in `fixtures/legacy/` | four goldens and the 62-row incident index (a3a29f9) | **O6**: no loader checks their setup or runs their M0 steps (11:102-113; the README says "a loader must first verify its own setup"); `reason_status` still says `proposed`, although every reason is registered |
| Controller API skeleton and `adc` CLI | step 9 (14), except 9e | 9e: see D2 |
| Test write-guard | the sandbox boundary (D37, 11:6-29) | **O3**: `internal/testguard` (temp roots, fake clock, fake registry; 11:30, 01-repo-layout:23) does not exist |
| Contract-faithful fakes | the fake adapter (`adapters/fake`), the in-process adapter peer (`internal/adapterhost/session/inproc`), `capacity/fakesched`, `authority/fakeauthn` | **O3**: fake forge, fake sandbox, fake provider and fake clock (11:35-41) do not exist; see D4 |
| T0 fixtures | `py-mini`, `ts-mini`, `go-mini` and `php-mini`, with tasks and `fixtures-check`; `multi-service` is M1 (T0 README) | **O4**: the installed dry run (#162) |

### 1.3 The intake fixtures (11:154-229) and their owners

11 lists "new contract fixtures from the intake" with no milestone marker. This table assigns each one to a milestone,
so that X1 can cite it. M0 entries name the PR that proves them. The others are owned by the milestone whose behaviour
they drive (01 §4).

| 11 line | Fixture | Owner |
|---|---|---|
| 155 | a ruling answers an owed input and re-keys the review | M0, G5 |
| 156 | a ruling for an input not owed is not delivered | M0, G5 |
| 157 | a new demand gets new evidence attempts and re-keys | M0, E2 |
| 158 | a precondition failure spends no attempt | M0, E2 |
| 159 | an unrestored evidence run blocks | M0, G8 |
| 160 | a 402 mid-call with an applied candidate is sealed, never "no changes" | M0, G2 |
| 161 | a fallback outside its role/mode scope is refused | M2 (fallback chains, 01:637; legacy `fallback_scope`) |
| 162 | a review lease is refused while its gate results are for another tree | M0, G4a |
| 163 | an answer approval that does not bind the owed input's revision is refused; a ruling cannot satisfy a receipt input | M1, with `input.answer` (D1) |
| 164 | a receipt for another demand with identical conclusions does not apply | M0, G4a |
| 165 | a crash between evidence collection and cleanup leaves no applicable receipt; a new job on an excluded target is refused; a late result from a superseded run is refused | M0, G8 (the late result is also the delivered `revoked-attempt-late-result`) |
| 166 | total-cap exhaustion across demands | M0, E2 |
| 167 | an unchanged `conclusions_digest` does not trigger re-review; a changed step outcome or changed restrictions do | M0, G5 |
| 168 | strict pair | M5 (strict options, 01:640) |
| 169 | broker affordability with reservations and uncertain liabilities; a stale or unknown balance waits | M2 (scheduler, 01:637) |
| 170 | an import error outside the changed files stays a runner fault | M1 (gate, 01:636) |
| 171 | a missing answer yields `awaiting_input` and recovers on the answering approval; a precondition failure recovers on its probe | the answer: M1, with `input.answer`; the probe: M0, E2 |
| 172 | an acceptance row not producible by its owner is refused | M5 (plan validator, as 11 says) |
| 173-189 | cited tests (B3) | M1 with the strict options off; M5 with them on |
| 190-212 | required configuration (B2) | M1 for adoption and the merge hold (with `profile.adopt`, D1); M4 for the cross-generation cases (merge queue, 01:639) |
| 213-222 | a call killed mid-stream on a single-call lease | M1 (recovery "without double charge", 01:636). The M0 parts (`internal/vocab/exitmap`, `internal/capacity/ledger`) already exist |
| 223-229 | after `capacity.exclude_source`, no launch path starts on the source | M2 (source registry and placement, 01:637); the action is not registered yet |

### 1.4 The remaining obligations

| # | Obligation | Design source | Issue |
|---|---|---|---|
| O1 | Artifact graph: schemas for Node, ComputationKey, Result, InputSpec and node state; key and generation computation; resolver and evaluator with atomic claiming and fake nodes; successor rounds; the build outcome table; queue generations as a template; the 19 fixtures of 13 and the M0 rows of §1.3 | 13:237-266; 01:635 | #114 |
| O2 | Evidence nodes: the parameter schema, attempt-key counting with the total cap, the precondition → hold mapping, artifact classes, the `remote_process` effect kind, five fixtures | 12:129-139 | #115 |
| O3 | `internal/testguard` with a fake clock and a fake registry; the fakes the O1 and O2 fixtures need | 11:30-44; 01-repo-layout:23; 01:635 | new; scope in D4 |
| O4 | Installed fake-adapter run: `make dist` packages a harness binary (standalone, with its own temporary state), the fake adapter with its dependencies and a Node runtime, and `fixtures/t0/py-mini`; the smoke checks result 9 from the harness's output | 14 §4 item 12; 01:635 "fake-driven T0 dry run" | #162 |
| O5 | Knowledge spec: the `FailureSignature` and `Claim` draft schemas; the spec's status corrected (`AttemptRecord` exists as `records/attempt-record.json`, and the manifest events are no longer "in review") | 10:83-113, 10:97-98 | #116 |
| O6 | Legacy goldens: a loader that verifies each golden's setup and runs the steps whose milestone is M0 through the real components; `reason_status` brought up to date | 11:102-113; `fixtures/legacy/index.md` rows A3, A4, A6 | #116 |
| O7 | `adc doctor` warns when a supervisor token is used from an OS user that can read the operator token file or the controller's state directory | 04-authority:379-383 (D31) | #117; milestone in D3 |
| O8 | Owners for the six deferred actions that have none, in `internal/daemon/matrix.go:33-44` and the generated `docs/reference/action-matrix.md` | 14 D1 | D1 |

**Follow-ups checked against M0 exit** (D7). None of them is a row of the exit check.

| Issue | What | Recommendation |
|---|---|---|
| #148 | `CommandProposed` names the effective change; 08 says the CLI prints it | M0: a response contract that M0 fixes (08 "human-policy output") |
| #158 | Index grant lineage by its new id; a stale schema description; a test case that cannot fail | M0: a record schema's description and its fold |
| #151 | Sandbox self-test robustness (L1, N1-N3) | M0: CI reliability on the constrained runner (R1) |
| #147 | Record `CommandRequest.reason` | M1: it needs a store-format migration and a decision on the idempotency digest (14 N1) |
| #149 | One never-proposable mechanism | M1: a refactor; behaviour is correct and tested |
| #156 | (a) seat grace override; (b) notes cursor; (c) defensive note rendering | (a) and (b) M1: the read is truthful today, since the daemon passes 0. (c) is delivered by #159 (`internal/cli/render.go`, `reads_test.go:53`); close it |
| #138 | Clock-health policy for forward clock jumps | M1 |

## 2. Lead decisions

- **L1. The graph runs under fixtures only in M0.** `internal/graph` holds the schemas' Go side, the key computation,
  the resolver and the evaluator, driven by fake nodes (13:239-240; 01:635 "with fakes"). `adcd` does not instantiate
  task graphs before M1. So no daemon path raises a node hold in M0 (D2).
- **L2. Every new fixture is an installed contract fixture.** Each fixture in 12, 13 and the M0 rows of §1.3 gets
  three things:
  - a golden in `fixtures/contract/m0/`;
  - a named mutation;
  - a runner in `internal/fixtures`.

  So `scripts/installed-fixtures.sh` proves it from the installed `adc` in isolation, and under its mutation. No new
  exit mechanism is needed.
- **L3. Installed fixtures use the in-process adapter peer.** `installed-fixtures.sh` mounts only the prefix: there is
  no bubblewrap, no Node and no source tree. Fixtures that need adapter behaviour use a scripted peer from
  `internal/adapterhost/session/inproc`, as the `unsafe-adapter-outputs` fixture already does
  (`internal/fixtures/adapterhost.go`). That peer runs inside `adc`. This covers the 402 before and after edits
  (13:255-256), which emit the `credit-exhausted` shape of `adapters/fake/src/fake.ts:38`. The live Node run belongs
  only to O4 (S3).
- **L4. Holds become node state.** The node-state schema (G1b) carries a hold record (02:78-86):
  - the hold id;
  - the registered hold;
  - the concrete signal instance;
  - the owner;
  - the paging history.

  13's supersession needs it to transfer a hold with its id and paging history (13:81-85). 12 needs it to map
  precondition failures and cap exhaustion to holds (12:64, 12:84). Lease holds stay where they are
  (`lease.waiting`, 05:91).
- **L5. Fakes stop where M0 stops.** A fake is built only as far as an M0 fixture consumes its interface (D4). Each
  one lives under `internal/testguard/fakes/` and is listed in the fake registry. So 11's rule "fix the fake first"
  (11:43) has one place to start. A1 adds an "M0 scope" column to 11:35-41 that records the line.
- **L6. Size and review.**
  - **Size:** the PR rules of 04 §2, about 400 hand-written non-test lines with `schemas/` counted (D44). 14's
    estimates ran 15-85% over (14:149; #154, #157, #159), so budgets here are set lower and each large PR states its
    split line.
  - **Review:** 04 §3, and every contract brief carries D43's threat model. The graph PRs touch `schemas/` and, from
    G3 on, `internal/eventstore`, so all of them are contract PRs.
  - **Reviewer:** the external reviewer is currently Mythos (`claude -p --model claude-mythos-5-1`, read-only), in
    Astra's place in 04 §3.
  - **Concurrency:** at most two external reviews at a time, and one at a time after a usage-limit reset.

## 3. PR breakdown

Brackets give hand-written non-test lines (04 §2, D44), with the classification (04 §3). Fixtures and tests are not
counted. Names are for tracking only.

### Track A: independent, starts at once

| PR | Content | Size | Class | Closes |
|---|---|---|---|---|
| **A1. testguard** | `internal/testguard`: temp roots for the store and blob store, a fake clock (deadlines, TTLs, resets, challenge expiry), the fake registry. Existing fixtures move to the fake clock where they hand-roll `now`. An "M0 scope" column in the 11:35-41 table (D4, L5) | [~200] | contract (it amends design 11) | O3 (part) |
| **A2. Effective change in proposals** | `CommandProposed` carries the derived object, project and change classes as the proposal binds them; the CLI prints them; a test with a grant override where the effective change differs from the request | [~150] | contract (schemas/api) | #148 |
| **A3. Grant-lineage claim** | Index `grant_lineage` by its new id too, and refuse a second claim; fix the `grant-replaced.json` description; make the test case discriminate under `storeShift` | [~80] | contract (internal/authority, schemas) | #158 |
| **A4. Self-test follow-ups** | #151 L1, N1, N2 and N3 in `scripts/test-sandbox-selftest.sh` | [~40] | contract: the test sandbox is contract territory (04:51). Its precedent #150 went to Opus; this PR goes to the external reviewer, a short brief for a small diff | #151 |
| **A5. Knowledge spec completion** | `FailureSignature` and `Claim` schemas, marked `draft` in their descriptions, with no golden vectors (D5); the status in `spec/knowledge/README.md:34-36` and `spec/knowledge/schemas/README.md:10-15` corrected; spec version 0.2.0-draft; `reason_status: registered` in the four goldens | [~250] | contract (spec/knowledge, schemas) | O5 |
| **A6. `adc doctor`** | Only if D3 says M0. For a supervisor token (whoami), it warns in three cases: the CLI's uid is the daemon's (peer credentials); it can open the daemon's state directory; it can read the daemon user's operator token file. It states what it cannot see | [~200] | contract (security configuration) | O7, #117 |
| **A7. Deferred-action owners** | After D1: `Owner{Milestone, Ref}` for the six actions in `matrix.go`; regenerate `action-matrix.md`; amend 14 for D2 (9e moves to M1, in 14's status line and 14:181), as 14 recorded the 7b split | [~30] | mechanical, but it amends design 14, so it goes to contract review | O8 |

### Track G: the artifact graph (#114)

Every PR in this track is a contract PR.

| PR | Content | Size | Fixtures |
|---|---|---|---|
| **G1a. Node schemas** | `schemas/records/`: Node, InputSpec, ComputationKey; generated Go and TypeScript; schema docs | [~250] | schema cases in `fixtures/contract/cases.json` |
| **G1b. Result and node-state schemas** | Result (with `completed` and `conclusions`), node state with the hold record (L4). Node state includes `runnable`. `NodeHold` carries a nullable `reason`, and each page in its paging history carries an `owner_ref` | [~250] | schema cases |
| **G2. Keys and build outcomes** | ComputationKey = H(kind, definition version, params digest, ordered input result ids, generation); result identity separate from the key; the build outcome table from the two observed facts (13:131-148), driven by a scripted `inproc` peer (L3) | [~250] | a 402 **before** the first edit is `could_not_run`, never `empty`; a 402 **after** edits is a sealed `partial_patch` (also 11:160) |
| **G3. Graph events and claiming** | Node, result and selection events in the store, with their projection and CheckHistory coverage; registry entries for the new event kinds. Their payloads carry the task, the predecessor, the selection and the producing attempt, so they are defined here and in G5 and G6b, never in G1b; atomic producer claiming in the writer transaction; a generation bump as a recorded invalidation. **Split line:** events and projection (G3-1), then claiming and invalidation (G3-2) | [~300] | duplicate demands share one claimed node |
| **G4a. Resolver** | Selected, reusable and applicable results (`internal/receipt`); only completed results are reused; `could_not_run` is an attempt record that retries on its signal; freshness by `valid_until` | [~300] | a `could_not_run` evidence result retries on its probe without invalidation; an expired receipt triggers renewal without resetting counters; a review cannot start with another tree's gate result (also 11:162); a receipt for another demand with identical conclusions does not apply (11:164) |
| **G4b. Evaluator** | Demand-driven goals; structural waits for internal inputs; node holds for external waits; authority and applicability re-validated in the committing transaction (13:117-120) | [~300] | a revoked approval between claim and commit blocks the commit |
| **G5. Successor rounds and equivalence** | Successor instantiation (repair, answers, demands); typed-conclusion equivalence only on review←gate and review←evidence (13:234); controller context rendered only from resolved inputs, listed by a `manifest.intent` (13:150-162, 10:74-77) | [~350] | an undeclared ruling never enters a review's controller context; an answer reaches only the successor review; equal conclusions reuse a review, and a changed step outcome re-runs it (also 11:155, 156, 167) |
| **G6a. Review roster identity** | The roster in a review's params; the review-requirement result `{subject, roster, definition_version, revision}`; the family derived from the executor registry (a fake registry under A1) | [~250] | equal gate or evidence conclusions never reuse a review across a roster change |
| **G6b. Supersession** | One writer transaction: `review.superseded{predecessor, replacement, transferred_holds, retired_holds}`, `attempt.stop_requested{preempt}` for running attempts, hold transfer or retirement; replay reaches the same outcome (13:67-86). **Split line:** the transaction with attempt stops and the race and restart cases (G6b-1), then hold transfer and retirement (G6b-2) | [~300] | a roster change is one recorded supersession, with every sub-case of 13:247-250 |
| **G7a. Effects as operations** | A new goal is a new effect operation, reconciled against the target; a crash after intent resumes the effect (03 effects; 13:164-172); a fake effect target | [~200] | deploy A, B, A deploys A again; a crash after effect intent resumes and never re-allocates |
| **G7b. Merge-queue generation** | The template: rebase, carry_over, gate/runtime_verify, and merge by expected-base CAS; a minimal fake forge (expected-base push, an external base move) (13:185-197). **Split line:** the fake forge, rebase and carry_over (G7b-1), then the refreshed receipts and the merge CAS (G7b-2) | [~300] | a base move mid-generation starts a new generation; carry-over applies only with an equal patch identity; a roster change after carry-over was selected makes the merge wait |
| **G8. Targets and facts** | Evidence targets over the existing lease restore events; source-test workspace versus package artifact versus runtime verification facts, with a minimal fake sandbox (13:174-183, 13:199-211) | [~250] | restore failure keeps the target excluded across a restart; a gate on the source-test workspace and runtime verification on the package see their declared facts; 11:159 and 11:165 |

Total for G: 12 PRs, about 3,300 lines. Up to three more if G3, G6b or G7b splits.

### 3.1 Track G as delivered

This records the splits made during review, on main and in open PRs, as of A7. The table above is the plan; where
the two differ, this section is what later PRs build on.

| Planned | Delivered as | Content |
|---|---|---|
| G1 | **G1a** (#168), **G1b** (#171) | as planned (the pre-split of M4) |
| G2 | **G2** (#173) | as planned |
| G3 | **G3-1** (#175) | node events, their projections and atomic producer claiming. Claiming moved here from G3-2. Claims are per node instance; a memoizable kind's key is unique per (task, key) |
| | **G3-2** (#176) | results, per-node selection, recorded invalidation, and the task-level generations |
| | **G3-3** (#177) | hold events: raise, page, clear, and retry waits. G3's row did not list them. There is no transfer event: hold transfer stays with G6b-2, as planned |
| G4a | **G4a-1** (#178, open) | `node.reused/v1`; `Resolve`; the fixtures for 11:162 and 11:164 |
| | **G4a-2** (next) | a `could_not_run` evidence result retrying on its probe; receipt renewal, the one exception that re-selects a result for an already selected key |

**Size.** The G track now has at least 15 PRs, against the 12 planned: G3 split in three and G4a in two. G6b and
G7b may still split.

**Fixtures.** G3's fixture "duplicate demands share one claimed node" was delivered in G3-2 (#176). G4a's probe-retry
and renewal fixtures move to G4a-2, with the behaviour they prove.

Changes to the plan that follow from these splits:
- **CheckHistory coverage moves to M1.** G3 planned CheckHistory coverage for the graph's projections. The graph is
  not wired into `adcd` in M0 (L1), so no startup replay reads them; the coverage lands with that wiring in M1.
- **G4b obligations added.** Within one command, the evaluator deduplicates demands and claims a given node instance
  or memoizable key at most once. It raises `awaiting_input` only against an owed input, to which the hold is tied.
- **Follow-up issues** from the track: #172 (schemagen: a `$ref` to a plain definition inside if/then) and #174 (an
  exit-map reason for an executor-reported failure, wanted before or with G4b; error excerpts for
  controller-derived statuses).

### Track E: evidence nodes (#115)

| PR | Content | Size | Fixtures (12:131-137) |
|---|---|---|---|
| **E1. Evidence-node schema** | `EvidenceNode` params (instrument, grant, target, subject input, demand key, limits, restore; the preconditions are the instrument contract's); artifact classes bound to the instrument contract; the `remote_process` effect kind with its reconciliation declaration (12:104-127) | [~250] | schema cases |
| **E2. Counting and preconditions** | Attempts counted per (subject, instrument digest, demand key), with a total cap per subject. Exhaustion is `could_not_run/evidence_cap_exhausted` with a `wrong_owner` hold. A precondition failure is `could_not_run/precondition_failed` with its hold, spends no attempt, and recovers on a bounded probe schedule | [~300] | a new demand gets new attempts, and the same demand does not; total-cap exhaustion; a precondition failure spends no attempt and recovers on its probe (also 11:157, 158, 166, 171) |
| **E3. Content classes** | An isolation probe inside the same wrapper, so an empty isolation setting is never published; a `restricted` artifact never reaches a manifest's `included` (12:95-102) | [~200] | an empty isolation setting is never published; a restricted artifact never reaches a prompt |

### Track L: legacy goldens (#116, second half)

| PR | Content | Size | Class |
|---|---|---|---|
| **L1. Legacy golden loader** | A loader that validates `golden.json` and checks each golden's setup: the shapes it reads, its provenance, and that nothing was trimmed (11:132). It runs the M0 steps through real components: `f16-walk-demand` through G5's reuse rule (`conclusions_unchanged`, `review_input_changed`), and `f16-inline-fallback` step 1 through G2 and the exit map (`broker_credit_exhausted`, an attempt record, never `empty`). The steps owned by M1 or M2 (`edge-owned-errors`, `closure-record-finding-r4`, `fallback_scope` placement) are setup-checked and listed with their milestone | [~200] | mechanical, unless it changes a reason or a schema |

### Track S: installed smoke part 2 (#162)

| PR | Content | Size | Class |
|---|---|---|---|
| **S1. Harness binary** | `adc-harness run`: a release binary that drives `internal/adapterhost` for one attempt against a spawned adapter. It is standalone, with its own temporary state: driving attempts against the installed daemon comes with M1's wired lease and attempt actions (operator 2026-10-08 (Telegram): option C). Every wait (start, handshake, result, shutdown) has a configurable bound, and a bound that passes is `could_not_run`, exit 2, its reason by the exit map with the stop requested (`attempt_stopped` unless a capacity hint outranks it) | [~400] | contract: it amends this plan (S1, S3, O4) and 01-repo-layout:104 |
| **S2. Dist packaging** | `make dist` packages the fake adapter with its workspace dependencies, the `node` binary under D6, and `fixtures/t0/py-mini`; `install-check.sh` installs them and re-verifies the Node checksum on the installed file; amends 01-repo-layout:104 (release builds are no longer Go binaries only) | [~200] | **contract, and it waits for the operator**: a third-party runtime in release artifacts is security posture (04 §4, 04:82) |
| **S3. Smoke result 9** | `scripts/installed-smoke.sh` runs `py-mini-feature-1` through the installed harness under bwrap, with no source tree, and checks result 9 from the harness's output: a submitted result (`attempt.result_submitted` applied in the harness's own state) and a usage record. It does not read the daemon's events | [~120] | mechanical |

### Track X: exit

| PR | Content | Size | Class |
|---|---|---|---|
| **X1. M0 exit record** | Runs the 00 exit table on installed artifacts on the arm64 controller host and the amd64 CI runner; records the commands and results in `docs/reviews/<date>-m0-exit.md`, with §1.3 as the intake-fixture record; closes #114-#117 and #162 with their completion evidence; posts the digest to #3 | [~120 docs] | mechanical; waits for the operator (D8) |

### Dependencies and parallelism

- **Now:** A1-A6, S1 and G1a. With two review slots, contract PRs queue for review even when they can be built in
  parallel.
- **After D1:** A7.
- **Graph backbone:** G1a → G1b → G2 → G3 → G4a → G4b → G5 → G6a → G6b → G7b.
- **Graph branches:**
  - G6a also needs A1 (the fake registry);
  - G7a after G4b;
  - G8 after G4b and A1;
  - G7b also needs A1 (the fake forge).
- **Evidence:**
  - E1 after G1b;
  - E2 after G4b (holds, and retry on signal);
  - E3 after G5 (manifest rendering).
- **Legacy:** L1 after G5 and A5.
- **Smoke:** S1 → S2 (after D6) → S3. The whole track is independent of the graph.
- **Exit:** X1 after everything else.

In practice the graph backbone holds one review slot throughout. The other slot takes Track A first, then E1-E3, L1
and S2.

**Size:** about 30 PRs. That is 27 as planned (26 if D3 moves #117 out of M0), plus the expected splits of G3, G6b
and G7b. They come to about 5,900 hand-written lines, of which the graph is about 3,300.

## 4. Expected results and acceptance evidence

| Obligation | Proven by | Expected result |
|---|---|---|
| O1 graph | `adc dev fixtures --contract` (installed, `installed-fixtures.sh`) | The 19 fixtures of 13:243-266 and the M0 rows of §1.3 yield their golden `(outcome, reason)` sequences. Each turns red under its named mutation in `make fixtures-contract` and stays green in the release build (`release-guard`) |
| O1 schemas | `make gen-check`, `make schema-docs` | Generated Go and TypeScript, and the new `docs/reference/schemas/records-*.md`, regenerate with no diff |
| O2 evidence | installed contract fixtures | The five fixtures of 12:133-137, plus E2's reasons and holds checked by `adc dev vocab check` (every hold has a resume signal) |
| O3 testguard and fakes | `make test` | Fixtures run on the fake clock; the fake registry lists every fake; a faithfulness test per fake (11:43); 11:35-41 states each fake's M0 scope |
| O4 dry run | `make install-check` → `scripts/installed-smoke.sh` | Results 1-9 of 14 §4 item 12. Result 9 is read from the installed harness's output (S1): a submitted result and a usage record, from its own temporary state, not the daemon's events. It runs from the installed prefix under bwrap, with no source tree and no system Node, and with the Node checksum re-verified |
| O5 spec | `go test ./internal/knowledge` (in `make test`) | The definitions validate; the golden vectors reproduce; the draft schemas validate their example cases; the spec READMEs list no stale status |
| O6 legacy | `make test` (loader tests) | Every golden's setup check passes. The loader refuses a mutated artifact (a trimmed terminal record, a missing field). The M0 steps of `f16-walk-demand` and `f16-inline-fallback` yield their golden sequences through the real components |
| O7 doctor | `make test` plus one installed-smoke step | A supervisor token used as the daemon uid, or with a readable state directory or operator token file, warns with exit code 0 and names the condition. A properly separated supervisor gets no warning. An operator token gets no supervisor warning |
| O8 owners | `make gen-check` | `action-matrix.md` shows no `unassigned` row; 14 records 9e in M1 |
| Exit | X1 | Every row of 00:50-58 passes on installed artifacts on arm64 and amd64; 01:635's three exit items are met; §1.3 accounts for 11:154-229 |

## 5. Open decisions for the operator

**D1. Owners for the six deferred actions.** The proposal: task.create, review.request and input.answer in M1;
project.create, profile.adopt and integration.change_topology in M2. Checked against 01 and 02:
- **task.create → M1.** Agreed. M1 is "one task" (01:636).
- **review.request → M1.** Agreed. M1 has accept, gate and review (01:636). A supervisor's `independent_review`
  proposals have no review source until then (`proposal.confirm` refuses with `authority.NoReviewSource`,
  matrix.go:49).
- **input.answer → M1.** Agreed. M1 reviews can record owed inputs (04 "Owed inputs"), and M1 ships "CLI approvals"
  (01:636). 11:163 and 11:171 need it (§1.3).
- **project.create → M1, not M2.** M1's exit delivers a T0 Python task (01:636). That task runs in a project
  registered with its first profile revision by this action (07:8-9; `actions.yaml:10`). Nothing in M2's contents
  (01:637) needs it first.
- **profile.adopt → M1, not M2.** Every profile change after creation is an adoption (07:8-13). M1's host-role
  profile, its amended gate checks and 11's B2 adoption cases (§1.3) need it. Alternative: M2, where data-policy and
  destination widenings first matter; M1 projects would then keep their first revision.
- **integration.change_topology → M1, not M2.** The topology first exists when phase 3 is proven in M1 (01:474;
  T0 README). It is pinned and changes only by this action (07:66). It also shares the protected-change path with
  `knowledge.change_protected_document`, which is already M1 (`internal/authority/decide.go:87-89`; matrix.go:42).
  Alternative: M5, with the `docker-compose` plugin (01:640). Topologies then stay frozen at their first revision until
  M5.

Nothing in 01 or 02 puts any of the six in M2, which is the capacity layer. **Recommended default: all six in M1, with
the alternatives above.** This adds six handler PRs to M1's plan. A7 records the answer.

**D2. 9e moves to M1, with project.create.** Confirmed, with one change: the hold **record** lands in M0 as part of
node state (L4), because 12 and 13 need hold identity and paging history. The read itself waits for M1:
- **Projects:** no daemon path creates a project before project.create (D1).
- **Holds:** no daemon path raises a hold in M0. The graph runs only under fixtures (L1), and the step-9 handlers
  raise none, so a holds read would always be empty and could not be tested end to end.

The M1 read covers both lease holds (`lease.waiting`) and node holds. Lease holds have no hold id or owner yet: `lease.waiting` records only the hold and its signal. So the M1 holds read must reconcile lease holds with `NodeHold` into one shape (id, owner, paging history) before it serves them. A7 amends 14.
**Recommended default: 9e in M1, in the PR that wires project.create or right after it.**

**D3. #117 (`adc doctor`) milestone.** Supervisor tokens are already issued in M0: an operator's `agent.enrol` over
the shared socket (14 H1; installed-smoke result 2). So D31's enforcement statement applies today, and the warning is
about 200 lines with no dependencies.
- **State directory:** the doctor learns it from the configuration `adcd` reads, or from `--state-dir`. The daemon
  exposes no path.
- **Operator token:** it finds the file from the daemon uid's home, since the private socket accepts only that uid
  (14 M3).
- **Limit:** it states that it cannot see a supervisor that can read an operator token held anywhere else.

**Recommended default: M0 (A6).** Alternative: M5, with the supervisor kit (02 §6).

**D4. How much of the fakes M0 builds.** 11:35-41 lists a fake forge, sandbox, provider and clock. Their real
interfaces arrive in M1 (forge, sandbox) and M2 (provider). **Recommended default:**
- M0 builds `internal/testguard`, the fake clock and the fake registry (A1).
- Each other fake is built only as far as an M0 fixture consumes it:
  - the forge: expected-base CAS and a base move (G7b);
  - the sandbox: declared facts and restore (G8);
  - the provider: its 402 behaviour, through scripted `inproc` peers (L3).
- The full surfaces land with their real interfaces.
- A1 records this M0 scope in 11.

Alternative: the full fakes in M0, about 800 more lines, built against interfaces that have no consumer yet.

**D5. `FailureSignature` and `Claim` draft schemas.** 10:97-98 says M0 writes them, with `Claim` unused until K3.
`spec/knowledge/README.md` defers them. The M2 placement in 01:637 and 03-knowledge-layer is for the derived
aggregate, not the draft schema. **Recommended default: both in M0 (A5),** with:
- `draft` in their descriptions;
- no golden vectors;
- their lines counted under D44.

Alternative: amend 10 to defer them, which is a design change.

*As delivered (A5, #181):* one deviation from 03-knowledge-layer. 03:91 lists a "purpose-bound decision ref" in
`Claim` and 03:109 writes `accepted(purpose)`. The draft `Claim` revision carries no decision: each purpose-bound
decision is its own record that references the revision, as validity is derived rather than stored, so a revision is
never edited and a claim can be accepted for one purpose and rejected for another. K3 does not re-add the field.

**D6. How auto-dev-cycle distributes adapters.** This decision is bigger than the smoke test:
- Every real adapter from M1 on is TypeScript on Node (01-repo-layout:56).
- Today's release builds are Go binaries only (01-repo-layout:104).
- Whatever S2 does for the fake adapter becomes how the product ships adapters.
- #162 requires that the installed run use no system Node.

**Recommended default:**
- `make dist` fetches the official Node 22 LTS release for the **server platforms only** (linux/amd64 and
  linux/arm64, `SERVER_PLATFORMS`).
- The pinned SHA-256s are taken from Node's **signed `SHASUMS256.txt.asc`**. The signature check against the Node
  release keys is recorded in the PR that sets or bumps the pins.
- `make dist` verifies the checksum before use and caches the download outside the workspace.
- **Only the `node` binary is packaged**: no npm and no corepack.
- `install-check.sh` **re-verifies the checksum** on the installed file.
- **A pin bump is a security-posture PR that waits for the operator** (04 §4).
- S1 amends 01-repo-layout:104 (`adc-harness` is a release binary); S2 amends it again (release builds are no longer
  Go binaries only).

The download adds a network need inside `make ci`. Today that need is only pnpm in `ts-deps` (Makefile:100-104).
`fixtures-check` runs as a separate CI job.

Alternatives:
- vendor the tarballs in the repository, about 50 MB per architecture;
- require a system Node at install time, which contradicts #162 and moves the trust decision to each installer.

**D7. Follow-ups in M0.** **Recommended default:**
- in M0: #148, #158 and #151 (A2-A4);
- in M1: #147, #149, #156 (a)(b) and #138;
- close #156 (c) as delivered by #159.

**D8. Declaring M0 exit.** **Recommended default:** X1 waits for the operator, like a plan PR (04 §4). M1 planning
starts once the operator approves it.

## 6. Risks

- **R1. CI runner capacity.** CI runs on one homelab VM: 4 cores, 6 GiB RAM, no swap, and two runners (#75, #151).
  - **Memory.** Two concurrent `make ci` jobs are already close to the memory limit. #150 fixed the self-test's
    compiler OOMs, not that margin.
  - **Time.** A run takes about 6 minutes alone and 9-15 minutes when two overlap (the 2026-10-07 runs).
  - **Volume.** About 30 PRs at roughly three runs each (push, review fixes, `main` after merge) is about 90 runs, or
    11-18 runner-hours.
  - **Growth.** S2 adds a Node runtime per server platform to `dist` and `install-check`. That means more disk,
    download and time on every run.

  Mitigations:
  - **Already in place:** `ci.yml:12-14` cancels superseded pull-request runs.
  - Keep the rule of a local arm64 `make ci` before every push (04 §2).
  - Keep at most two PRs in CI at once.
  - Cache the Node download outside the workspace.
  - Ask the operator for one of #151's homelab changes: swap, more RAM, or one runner per VM. This is the cheapest
    real fix.
- **R2. Review depth on the graph.** G1a-G8 are about 3,300 lines of new contract. Earlier contract PRs needed up to
  four rounds (#70, #72, D43). G6b (supersession) and G4b (commit-time revalidation) carry the most risk.
  - Mitigations: briefs state D43's scope and cite the 13 line they implement; the split lines are stated up front;
    any PR splits after three NO-GOs (04 §2).
- **R3. Review capacity.** At most two external reviews run at a time, and one at a time after a usage-limit reset.
  Contract PRs wait when the reviewer's credits run out (04 §3). The graph's sequential backbone means each wait
  delays exit day for day. Tracks A, S and L keep moving meanwhile.
- **R4. Fakes ahead of real interfaces.** D4's minimal fakes encode guesses about the M1 forge and sandbox interfaces.
  11:43 handles divergence: fix the fake first, with a red test. Some M0 fixtures may need re-wiring in M1.
- **R5. Supply chain in `dist`.** D6 puts a third-party runtime into release artifacts. `tools/releasecheck` checks Go
  build settings only and cannot see the Node binary. The safeguards are the D6 default:
  - pins taken from the signed SHASUMS file;
  - the checksum verified at `dist` and re-verified at install;
  - pin bumps gated on the operator.

  Any change to how Node is fetched is security posture (04 §4).
- **R6. Scope creep.** The graph invites wiring into `adcd`. L1 keeps that work in M1. A PR that wires it is outside
  this plan.
- **R7. Decision latency.** D1 gates A7, and D6 gates S2. Everything else proceeds without answers (04 §5). Whatever
  is still unanswered gates X1.

## 7. Estimate

- **Shape:** about 30 PRs: 7 in Track A, 12 in G (plus up to three splits), 3 in E, 1 in L, 3 in S and 1 in X.
- **Critical path:** the graph backbone, 10 sequential contract PRs (G1a → G7b), then X1.
- **Assumption:** each backbone PR takes 1.5-2 review rounds, with two review slots. Measured so far:
  - Astra: 2.0-2.3 rounds a PR (10-04 to 10-06), and 1.1 on 10-07;
  - Mythos: about 1.3.

  The daily digest tracks rounds per backbone PR against this assumption.
- **Rate:** at 6-10 merges a day, the parallel tracks finish within the first 3-4 days. The backbone alone takes about
  4-5 days, and L1, E3 and X1 queue behind it.
- **Expected:** M0 exits **6-8 calendar days** after this plan and D1-D8 are answered.
- **Pessimistic:** about **10 days**, if backbone PRs take the 2-plus rounds of 10-04 to 10-06, or if R1 or R3 bites.
