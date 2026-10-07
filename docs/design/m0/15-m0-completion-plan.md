# M0 completion: delivery plan

Status: revision 1, 2026-10-08. Draft for the operator. Not reviewed yet. Written on origin/main ca581b7, after step 9
of 04 §9 (14) merged with PR 11 (#163). The 9e reads are still deferred.

Scope: **everything M0 still owes before its exit check can pass.** 14 tracked four obligations outside step 9. This
note plans them, plus the gaps that a check against 01 §4 and the 00 exit table turned up:
- the artifact graph (13; #114);
- evidence nodes (12; #115);
- the knowledge spec and the legacy goldens (10, 11; #116);
- the `adc doctor` credential-isolation warning (04-authority:379-383; #117). 14 cites this as `:225`, which was its
  line before 04 grew;
- part 2 of the installed smoke: the fake-adapter run (14 §4 item 12, result 9; #162);
- the test write-guard and the contract-faithful fakes (01 §4 M0 row; 01-repo-layout:23; 11:30, 11:33-44).

It does not reopen step 9. Wiring the graph into `adcd` is M1 (§3, L1).

## 1. Scope: what M0 exit requires, against what is delivered

### 1.1 Exit criteria

**01 §4, M0 row (01:635), exit column:**
- CI green;
- schema docs generated from code;
- a fake-driven T0 dry run passes.

**00 "M0 exit check" (00:46-58),** run against installed artifacts:

| Row | Check | State on ca581b7 |
|---|---|---|
| CI green | `make ci` | green; it already chains lint, gen-check, vocab-check, test, fixtures-contract, release-guard, test-sandbox-selftest, dist and install-check |
| Clean install | `make dist && scripts/install-check.sh` | delivered: installs `adcd`, `adc` and `adc-agent` |
| Contract fixtures | `adc dev fixtures --contract`, installed | delivered for the 20 fixtures in the M0 list (11:67-87), each with its mutation (`scripts/installed-fixtures.sh`). **The fixtures of 12 and 13 are missing** |
| Containment | `make test-sandbox-selftest` | delivered |
| Schemas documented | `make schema-docs` | delivered; it will cover the new schemas |
| Vocabulary closed | `adc dev vocab check` | delivered; it will cover the new holds and reasons |
| Log integrity | `adc dev verify-log` | delivered (also in the installed smoke) |

The fake-driven T0 dry run is #162's result 9: the fake adapter on `py-mini-feature-1` ends with
`attempt.result_submitted` applied and a `usage.measured` record (14 §4 item 12).

### 1.2 The M0 row's contents (01:635), item by item

| Item | Delivered | Remaining |
|---|---|---|
| Repo layout, licence, CI | yes | none |
| Schemas: outcomes, reasons, authority, lease/attempt/fencing, receipt, event store, project profile | yes | none |
| `ContextManifest`, Ref, ViewFingerprint, coverage levels | yes (`schemas/records/manifest-*`, `ref`, `view-fingerprint`; `internal/knowledge`) | none |
| Knowledge spec draft: schemas, invariants, fixture definitions (K10, K12) | `spec/knowledge/` has `vocabulary.md`, `invariants.md`, 13 fixture definitions and golden vectors | **O5**: `FailureSignature` and `Claim` draft schemas (10:178); stale status lines in the spec |
| Artifact graph: schemas, key computation, resolver, evaluator with fakes (13) | nothing | **O1** |
| Evidence-node parameters (12) | instrument contracts, artifact classes in `instrument-contract.json`, lease restore events | **O2** |
| Legacy goldens in `fixtures/legacy/` | four goldens and the 62-row incident index (a3a29f9) | **O6**: no loader checks their setup or runs their M0 steps (11:102-113, README "a loader must first verify its own setup"); `reason_status` still says `proposed` although every reason is now registered |
| Controller API skeleton and `adc` CLI | step 9 (14), except 9e | 9e: see D2 |
| Test write-guard | the sandbox boundary (D37, 11:6-29) | **O3**: `internal/testguard` (temp roots, fake clock, fake registry; 11:30, 01-repo-layout:23) does not exist |
| Contract-faithful fakes | the fake adapter (`adapters/fake`), `capacity/fakesched`, `authority/fakeauthn` | **O3**: fake forge, fake sandbox, fake provider and fake clock (11:35-41) do not exist; see D4 |
| T0 fixtures | `py-mini`, `ts-mini`, `go-mini` and `php-mini`, with tasks and `fixtures-check`; `multi-service` is M1 (T0 README) | **O4**: the installed dry run (#162) |

### 1.3 The remaining obligations

| # | Obligation | Design source | Issue |
|---|---|---|---|
| O1 | Artifact graph: schemas for Node, ComputationKey, Result, InputSpec and node state; key and generation computation; resolver and evaluator with atomic claiming and fake nodes; successor rounds; the build outcome table; queue generations as a template; the 19 fixtures | 13:237-266; 01:635 | #114 |
| O2 | Evidence nodes: the parameter schema, attempt-key counting with the total cap, the precondition → hold mapping, artifact classes, the `remote_process` effect kind, five fixtures | 12:71-81 | #115 |
| O3 | `internal/testguard` with a fake clock and a fake registry; the fakes the O1 and O2 fixtures need | 11:30-44; 01-repo-layout:23; 01:635 | new; scope in D4 |
| O4 | Installed fake-adapter run: `make dist` packages a harness binary, the fake adapter with its dependencies and a Node runtime, and `fixtures/t0/py-mini`; the smoke checks result 9 | 14 §4 item 12; 01:635 "fake-driven T0 dry run" | #162 |
| O5 | Knowledge spec: the `FailureSignature` and `Claim` draft schemas; the spec's status corrected (`AttemptRecord` exists as `records/attempt-record.json`, and the manifest events are no longer "in review") | 10:164-194 | #116 |
| O6 | Legacy goldens: a loader that verifies each golden's setup and runs the steps whose milestone is M0 through the real components; `reason_status` brought up to date | 11:102-113; `fixtures/legacy/index.md` rows A3, A4, A6 | #116 |
| O7 | `adc doctor` warns when a supervisor token is used from an OS user that can read the operator token file or the controller's state directory | 04-authority:379-383 (D31) | #117; milestone in D3 |
| O8 | Owners for the six deferred actions that have none, in `internal/daemon/matrix.go` and the generated `docs/reference/action-matrix.md` | 14 D1; matrix.go:29-44 | D1 |

**Follow-ups checked against M0 exit** (D7). None of them is a row of the exit check:

| Issue | What | Recommendation |
|---|---|---|
| #148 | `CommandProposed` names the effective change; 08 says the CLI prints it | M0: a response contract M0 fixes (08 "human-policy output") |
| #158 | Index grant lineage by its new id; a stale schema description; a test case that cannot fail | M0: a record schema's description and its fold |
| #151 | Sandbox self-test robustness (L1, N1-N3) | M0, mechanical: CI reliability on the constrained runner (R1) |
| #147 | Record `CommandRequest.reason` | M1: needs a store-format migration and a decision on the idempotency digest (14 N1) |
| #149 | One never-proposable mechanism | M1: a refactor; behaviour is correct and tested |
| #156 | (a) seat grace override; (b) notes cursor; (c) defensive note rendering | (a) and (b) M1: the read is truthful today, since the daemon passes 0. (c) is delivered by #159 (`internal/cli/render.go`, `reads_test.go:53`); close it |
| #138 | Clock-health policy for forward clock jumps | M1 |

## 2. Lead decisions

- **L1. The graph runs under fixtures only in M0.** `internal/graph` holds the schemas' Go side, the key computation,
  the resolver and the evaluator, driven by fake nodes (13:239-240; 01:635 "with fakes"). `adcd` does not instantiate
  task graphs before M1. So no daemon path raises a node hold in M0 (D2).
- **L2. Every new fixture is an installed contract fixture.** Each fixture in 12 and 13 gets a golden in
  `fixtures/contract/m0/` and a named mutation. It also gets a runner in `internal/fixtures`. So
  `scripts/installed-fixtures.sh` proves it from the installed `adc` in isolation, and under its mutation. No new exit
  mechanism is needed.
- **L3. Installed fixtures are pure Go.** `installed-fixtures.sh` mounts only the prefix: there is no Node and no
  source tree. Fixtures that need adapter behaviour (402 before or after edits, 13:255-256) drive the build outcome
  table from observed facts: an `adapterhost` exchange recorded from a named fake-adapter scenario
  (`credit-exhausted`, `adapters/fake/src/fake.ts:38`), replayed through `internal/vocab/exitmap`. This follows 11's
  rule on captured shapes: provenance, and a loader that checks it (11:114-133). The live Node run belongs to O4 only.
- **L4. Holds become node state.** The node-state schema (G1) carries a hold record: hold id, registered hold, the
  concrete signal instance, owner and paging history (02:78-86). 13's supersession needs this to transfer a hold with
  its id and paging history (13:81-85). So does 12's mapping of precondition failures and cap exhaustion to holds
  (12:44, 12:51). Lease holds stay where they are (`lease.waiting`, 05:91).
- **L5. Fakes stop where M0 stops.** A fake is built only as far as an M0 fixture consumes its interface (D4). Each
  one lives under `internal/testguard/fakes/` and is listed in the fake registry, so 11's rule "fix the fake first"
  (11:43) has one place to start.
- **L6. Size and review.** The PR rules are 04 §2: about 400 hand-written non-test lines, with `schemas/` counted
  (D44). Review follows 04 §3, and every contract brief carries D43's threat model. The graph PRs touch `schemas/` and,
  from G3 on, `internal/eventstore`, so all of them are contract PRs.

## 3. PR breakdown

Brackets give hand-written non-test lines (04 §2, D44), with the classification (04 §3). Fixtures and tests are not
counted. Names are for tracking only.

### Track A: independent, starts at once, all in parallel

| PR | Content | Size | Class | Closes |
|---|---|---|---|---|
| **A1. testguard** | `internal/testguard`: temp roots for the store and blob store, a fake clock (deadlines, TTLs, resets, challenge expiry), the fake registry. Existing fixtures move to the fake clock where they hand-roll `now` | [~200] | mechanical | O3 (part) |
| **A2. Effective change in proposals** | `CommandProposed` gets the derived object, project and change classes as the proposal binds them; the CLI prints them; a test with a grant override where the effective change differs from the request | [~150] | contract (schemas/api) | #148 |
| **A3. Grant-lineage claim** | Index `grant_lineage` by its new id too, and refuse a second claim; fix the `grant-replaced.json` description; make the test case discriminate under `storeShift` | [~80] | contract (internal/authority, schemas) | #158 |
| **A4. Self-test follow-ups** | #151 L1, N1, N2 and N3 in `scripts/test-sandbox-selftest.sh` | [~40] | mechanical | #151 |
| **A5. Knowledge spec completion** | `FailureSignature` and `Claim` draft schemas (D5); status in `spec/knowledge/README.md` and `schemas/README.md` corrected; spec version 0.2.0-draft; `reason_status: registered` in the four goldens | [~250] | contract (spec/knowledge, schemas) | O5 |
| **A6. `adc doctor`** | Only if D3 says M0. A warning when the CLI's uid is the daemon's uid, read from peer credentials. A warning when it can open the daemon's state directory. A warning when it can read the daemon user's operator token file. All three only for a supervisor token (whoami). A stated limit for what it cannot see | [~200] | contract (security configuration) | O7, #117 |
| **A7. Deferred-action owners** | After D1: `Owner{Milestone, Ref}` for the six actions in `matrix.go`; regenerate `action-matrix.md` | [~20] | mechanical | O8 |

### Track G: the artifact graph (#114), sequential backbone

| PR | Content | Size | Fixtures (13:243-266) |
|---|---|---|---|
| **G1. Graph schemas** | `schemas/records/`: Node, InputSpec, ComputationKey, Result (with `completed`, `conclusions`), node state with the hold record (L4); generated Go and TypeScript; schema docs; registry entries for the new event kinds | [~380] | schema cases in `fixtures/contract/cases.json` |
| **G2. Keys and build outcomes** | ComputationKey = H(kind, definition version, params digest, ordered input result ids, generation); result identity separate from the key; the build outcome table from the two observed facts (13:131-148) | [~250] | a 402 **before** the first edit is `could_not_run`, never `empty`; a 402 **after** edits is a sealed `partial_patch` |
| **G3. Graph events and claiming** | Node, result and selection events in the store, with their projection and CheckHistory coverage; atomic producer claiming in the writer transaction; a generation bump as a recorded invalidation | [~350] | duplicate demands share one claimed node |
| **G4a. Resolver** | Selected, reusable and applicable results (`internal/receipt`); only completed results are reused; `could_not_run` is an attempt record that retries on its signal; freshness by `valid_until` | [~300] | a `could_not_run` evidence result retries on its probe without invalidation; an expired receipt triggers renewal without resetting counters; a review cannot start with another tree's gate result |
| **G4b. Evaluator** | Demand-driven goals; structural waits for internal inputs; node holds for external waits; authority and applicability re-validated in the committing transaction (13:117-120) | [~300] | a revoked approval between claim and commit blocks the commit |
| **G5. Successor rounds and equivalence** | Successor instantiation (repair, answers, demands); typed-conclusion equivalence only on review←gate and review←evidence (13:234); controller context rendered only from resolved inputs, listed by a `manifest.intent` (13:150-162, 10:155-158) | [~350] | an undeclared ruling never enters a review's controller context; an answer reaches only the successor review; equal conclusions reuse a review, and a changed step outcome re-runs it |
| **G6a. Review roster identity** | The roster in a review's params; the review-requirement result `{subject, roster, definition_version, revision}`; the family derived from the executor registry (a fake registry under A1) | [~250] | equal gate or evidence conclusions never reuse a review across a roster change |
| **G6b. Supersession** | One writer transaction: `review.superseded{predecessor, replacement, transferred_holds, retired_holds}`, `attempt.stop_requested{preempt}` for running attempts, hold transfer or retirement; replay reaches the same outcome (13:67-86) | [~350] | a roster change is one recorded supersession, with every sub-case of 13:247-250 |
| **G7a. Effects as operations** | A new goal is a new effect operation reconciled against the target; a crash after intent resumes the effect (03 effects; 13:164-172); a fake effect target | [~200] | deploy A, B, A deploys A again; a crash after effect intent resumes and never re-allocates |
| **G7b. Merge-queue generation** | The template: rebase, carry_over, gate/runtime_verify, merge by expected-base CAS; a minimal fake forge (expected-base push, an external base move) (13:185-197) | [~350] | a base move mid-generation starts a new generation; carry-over applies only with an equal patch identity; a roster change after carry-over was selected makes the merge wait |
| **G8. Targets and facts** | Evidence targets over the existing lease restore events; source-test workspace versus package artifact versus runtime verification facts, with a minimal fake sandbox (13:174-183, 13:199-211) | [~250] | restore failure keeps the target excluded across a restart; a gate on the source-test workspace and runtime verification on the package see their declared facts |

Total for G: 11 PRs, about 3,300 lines.

### Track E: evidence nodes (#115)

| PR | Content | Size | Fixtures (12:73-79) |
|---|---|---|---|
| **E1. Evidence-node schema** | `EvidenceNode` params (instrument, grant, target, demand, preconditions, limits, restore); artifact classes bound to the instrument contract; the `remote_process` effect kind with its reconciliation declaration (12:64-69) | [~250] | schema cases |
| **E2. Counting and preconditions** | Attempts counted per (subject, instrument digest, demand key), with a total cap per subject; exhaustion is `could_not_run/evidence_cap_exhausted` with a `wrong_owner` hold; a precondition failure is `could_not_run/precondition_failed` with its hold, spends no attempt, and recovers on a bounded probe schedule | [~300] | a new demand gets new attempts and the same demand does not; total-cap exhaustion; a precondition failure spends no attempt and recovers on its probe |
| **E3. Content classes** | An isolation probe inside the same wrapper, so an empty isolation setting is never published; a `restricted` artifact never reaches a manifest's `included` (12:55-62) | [~200] | an empty isolation setting is never published; a restricted artifact never reaches a prompt |

### Track L: legacy goldens (#116, second half)

| PR | Content | Size | Class |
|---|---|---|---|
| **L1. Legacy golden loader** | A loader that validates `golden.json` and checks each golden's setup: the shapes it reads, its provenance and nothing trimmed (11:132). It runs the M0 steps through real components: `f16-walk-demand` through G5's reuse rule (`conclusions_unchanged`, `review_input_changed`), and `f16-inline-fallback` step 1 through G2 and the exit map (`broker_credit_exhausted`, an attempt record, never `empty`). Steps owned by M1 or M2 (`edge-owned-errors`, `closure-record-finding-r4`, `fallback_scope` placement) are setup-checked and listed with their milestone | [~200] | mechanical, unless it changes a reason or a schema |

### Track S: installed smoke part 2 (#162)

| PR | Content | Size | Class |
|---|---|---|---|
| **S1. Harness binary** | A release binary that drives `internal/adapterhost` for one attempt against the installed daemon and an adapter; covered by `tools/releasecheck`; every wait bounded | [~250] | mechanical while the adapter contract is unchanged (14 item 12), contract otherwise |
| **S2. Dist packaging** | `make dist` packages the fake adapter with its workspace dependencies, a Node runtime (D6) and `fixtures/t0/py-mini`; `install-check.sh` installs and verifies them | [~200] | mechanical; D6 is a supply-chain choice for the operator |
| **S3. Smoke result 9** | `scripts/installed-smoke.sh` runs `py-mini-feature-1` through the installed harness under bwrap, with no source tree. It checks that `attempt.result_submitted` is applied and that a `usage.measured` record exists | [~120] | mechanical |

### Track X: exit

| PR | Content | Size | Class |
|---|---|---|---|
| **X1. M0 exit record** | Runs the 00 exit table on installed artifacts on both the arm64 controller host and the amd64 CI runner; records the commands and results in `docs/reviews/<date>-m0-exit.md`; closes #114-#117 and #162 with their completion evidence; posts the digest to #3 | [~120 docs] | mechanical; waits for the operator (D8) |

### Dependencies and parallelism

- **Now, in parallel:** A1-A6, S1, and G1.
- **After D1:** A7.
- **Graph backbone:** G1 → G2 → G3 → G4a → G4b → G5 → G6a → G6b → G7b.
- **Graph branches:**
  - G7a after G4b;
  - G8 after G4b and A1;
  - G7b also needs A1 (fake forge).
- **Evidence:**
  - E1 after G1;
  - E2 after G4b (holds and retry on signal);
  - E3 after G5 (manifest rendering).
- **Legacy:** L1 after G5 and A5.
- **Smoke:** S1 → S2 (after D6) → S3. The whole track is independent of the graph.
- **Exit:** X1 after everything else.

At most two or three contract PRs are in review at a time (R2). In practice the graph backbone takes one slot
throughout, and the second slot takes Track A first, then E1-E3.

**Size:** 26 PRs (25 if D3 moves #117 out of M0), about 5,900 hand-written lines, of which the graph is about 3,300.

## 4. Expected results and acceptance evidence

| Obligation | Proven by | Expected result |
|---|---|---|
| O1 graph | `adc dev fixtures --contract` (installed, `installed-fixtures.sh`) | The 19 fixtures of 13:243-266 each yield their golden `(outcome, reason)` sequence. Each one turns red under its named mutation in `make fixtures-contract`, and stays green in the release build (`release-guard`) |
| O1 schemas | `make gen-check`, `make schema-docs` | Generated Go and TypeScript, and `docs/reference/schemas/records-node*.md` and its siblings, regenerate with no diff |
| O2 evidence | installed contract fixtures | The five fixtures of 12:75-79, plus E2's reasons and holds checked by `adc dev vocab check` (every hold has a resume signal) |
| O3 testguard and fakes | `make test` | Fixtures run on the fake clock; the fake registry lists every fake; a fake-faithfulness test per fake (11:43) |
| O4 dry run | `make install-check` → `scripts/installed-smoke.sh` | Results 1-9 of 14 §4 item 12; result 9: `attempt.result_submitted` applied and a `usage.measured` record, from the installed prefix under bwrap, with no source tree and no system Node |
| O5 spec | `go test ./internal/knowledge` (in `make test`) | The definitions validate; the golden vectors reproduce; the new draft schemas validate their example cases; the spec README lists no stale status |
| O6 legacy | `make test` (loader tests) | Every golden's setup check passes. A mutated artifact (a trimmed terminal record, a missing field) is refused by the loader. The M0 steps of `f16-walk-demand` and `f16-inline-fallback` yield their golden sequence through the real components |
| O7 doctor | `make test` plus one installed-smoke step | A supervisor token used as the daemon uid, or with a readable state directory or operator token file, warns with exit code 0 and names the condition. A properly separated supervisor gets no warning. An operator token gets no supervisor warning |
| O8 owners | `make gen-check` | `action-matrix.md` shows no `unassigned` row |
| Exit | X1 | Every row of 00:50-58 passes on installed artifacts on arm64 and amd64; 01:635's three exit items are met |

## 5. Open decisions for the operator

**D1. Owners for the six deferred actions.** The proposal: task.create, review.request and input.answer in M1;
project.create, profile.adopt and integration.change_topology in M2. Checked against 01 and 02:
- **task.create → M1.** Agreed. M1 is "one task" (01:636).
- **review.request → M1.** Agreed. M1 has accept, gate and review (01:636). A supervisor's `independent_review`
  proposals have no review source until then (`proposal.confirm` refuses with `authority.NoReviewSource`, matrix.go:49).
- **input.answer → M1.** Agreed. M1 reviews can record owed inputs (04 "Owed inputs"), and M1 ships "CLI approvals"
  (01:636). 11:171's `awaiting_input` recovery needs it.
- **project.create → M1, not M2.** M1's exit delivers a T0 Python task (01:636). That task runs in a project registered
  with its first profile revision by this action (07:8-9; `actions.yaml:10`). Nothing in M2's contents (01:637) needs
  it first.
- **profile.adopt → M1, not M2.** Every profile change after creation is an adoption (07:8-13). M1's host-role profile
  and amended gate checks will need at least one change. Alternative: M2, where data-policy and destination widenings
  (04 change classes) first matter. Then M1 projects keep their first revision.
- **integration.change_topology → M1, not M2.** The topology first exists when phase 3 is proven in M1 (01:474). It is
  pinned and changes only by this action (07:66), and it shares the protected-change path with
  `knowledge.change_protected_document`, which is already M1 (decide.go:89; matrix.go:42). Alternative: M5, with the
  `docker-compose` plugin (01:640). Topologies then stay frozen at their first revision until M5.

Nothing in 01 or 02 puts any of the six in M2; M2 is the capacity layer. **Recommended default: all six in M1, with
the alternatives above.** A7 records the answer.

**D2. 9e moves to M1, with project.create.** Confirmed, with one change: the hold **record** lands in M0 as part of
node state (L4), because 12 and 13 need hold identity and paging history. The read waits for M1 for two reasons:
- **Projects:** no daemon path creates a project before project.create (D1).
- **Holds:** no daemon path raises a hold in M0. The graph runs only under fixtures (L1), and the step-9 handlers raise
  none. A holds read would always be empty, with nothing to test end to end.

The M1 read must cover both lease holds (`lease.waiting`) and node holds. **Recommended default: 9e in M1, in the
PR that wires project.create, or right after it.**

**D3. #117 (`adc doctor`) milestone.** Supervisor tokens are already issued in M0: an operator's `agent.enrol` over
the shared socket (14 H1, installed-smoke result 2). So D31's enforcement statement applies today, and the warning is
about 200 lines with no dependencies.

The doctor learns the state directory from the configuration `adcd` reads, or from `--state-dir`. The daemon exposes
no path. It finds the operator token file from the daemon uid's home (the private socket accepts only that uid, 14 M3).
It states that it cannot see a supervisor that can read an operator token held anywhere else.

**Recommended default: M0 (A6).** Alternative: M5, with the supervisor kit (02 §6).

**D4. How much of the fakes M0 builds.** 11:35-41 lists a fake forge, sandbox, provider and clock. Their real
interfaces arrive in M1 (forge, sandbox) and M2 (provider).

**Recommended default:**
- M0 builds `internal/testguard`, the fake clock and the fake registry (A1);
- each other fake only as far as an M0 fixture consumes it: the forge's expected-base CAS and base move (G7b), the
  sandbox's declared facts and restore (G8), and provider behaviour via the fake adapter's recorded scenarios (L3);
- the full surfaces land with their real interfaces.

Alternative: the full fakes in M0, about 800 more lines, built against interfaces that have no consumer yet.

**D5. `FailureSignature` and `Claim` draft schemas.** 10:178 says M0 writes them, and `Claim` stays unused until K3.
`spec/knowledge/README.md` defers them to M2 and after K3. **Recommended default: write both as draft schemas in M0
(A5), marked unused**, as 10 says. Alternative: amend 10 to defer them. That is a design change.

**D6. The Node runtime in `make dist`** (#162 requires no system Node). **Recommended default:** `make dist` fetches
the official Node 22 release for each target platform, pinned by version and SHA-256 in the repository. It verifies
the checksum before use and caches the result. The download adds a network need to `dist`, which `fixtures-check`
already has. Alternatives:
- vendor the tarballs in the repository: about 50 MB per architecture;
- require a system Node at install time, which contradicts #162.

**D7. Follow-ups in M0.** **Recommended default:** #148, #158 and #151 in M0 (A2-A4). #147, #149, #156 (a)(b) and
#138 in M1. Close #156 (c) as delivered by #159.

**D8. Declaring M0 exit.** **Recommended default:** X1 waits for the operator, like a plan PR (04 §4). M1 planning
starts after the operator approves it.

## 6. Risks

- **R1. CI runner capacity.** CI runs on one homelab VM: 4 cores, 6 GiB RAM, no swap, two runners (#75, #151).
  - **Memory.** Two concurrent `make ci` jobs are already close to the memory limit. #150 fixed the self-test's
    compiler OOMs, but not that margin.
  - **Time.** A run takes about 6 minutes alone and 9-15 minutes when two overlap (the 2026-10-07 runs).
  - **Load.** At 26 PRs and about three runs each (push, review fixes, `main` after merge), that is about 80 runs, or
    10-16 runner-hours.
  - **Growth.** S2 adds a Node runtime per platform to `dist` and `install-check`. That means more disk, download and
    time on every run.

  Mitigations:
  - cancel superseded runs per branch;
  - keep the rule of a local arm64 `make ci` before every push (04 §2);
  - at most two or three PRs in CI at once;
  - cache the Node download outside the workspace;
  - ask the operator for one of #151's homelab changes (swap, more RAM, or one runner per VM). This is the cheapest
    real fix.
- **R2. Review depth on the graph.** G1-G8 are about 3,300 lines of new contract. Earlier contract PRs needed up to
  four rounds (#70, #72, D43). G6b (supersession) and G4b (commit-time revalidation) carry the most risk.
  - Mitigations: briefs state D43's scope and cite the 13 line they implement; any PR is split after three NO-GOs
    (04 §2); review capacity is two or three contract PRs at a time.
- **R3. Review credits.** Contract PRs wait when Codex credits run out (04 §3). The graph's sequential backbone means
  each wait delays exit day for day. Track A, S and L keep moving meanwhile.
- **R4. Fakes ahead of real interfaces.** D4's minimal fakes encode guesses about the M1 forge and sandbox interfaces.
  11:43 handles divergence: fix the fake first, with a red test. Some M0 fixtures may need re-wiring in M1.
- **R5. Installed fixtures have no Node.** L3 replays a recorded exchange for the 402 fixtures. If the recording
  drifts from the fake adapter's real behaviour, the fixture proves the recording rather than the adapter. The
  capture's provenance and loader (11:125-133), and S3's live run, bound this risk.
- **R6. Supply chain in `dist`.** D6 brings a third-party runtime into release artifacts. The pinned checksums and
  `releasecheck` limit it. Any later change to how it is fetched is security posture (04 §4).
- **R7. Scope creep.** The graph invites wiring into `adcd`. L1 keeps that work in M1. A PR that wires it is out of
  this plan.
- **R8. Decision latency.** D1 gates A7, and D6 gates S2. Everything else proceeds without answers (04 §5). Whatever
  is still unanswered gates X1.

## 7. Estimate

- **Shape:** 26 PRs: 7 in Track A, 11 in Track G, 3 in E, 1 in L, 3 in S and 1 in X.
- **Critical path:** the graph backbone, 9 sequential contract PRs, then X1.
- **Pace:** at 6-10 merges a day, the parallel tracks finish within the first 2-3 days. The backbone moves at about
  3-4 contract merges a day, which is the rate of review rounds.
- **Expected:** M0 exit in **4-5 calendar days** after this plan and D1-D8 are answered.
- **Pessimistic:** about **7 days**, if the graph PRs see the four-round reviews of #70 and #72, or if R1 or R3 bites.
