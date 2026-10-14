# M1 — invariant-enforcement inventory (D45)

Status: draft 4, 2026-10-12. It is part of `00-slice-plan.md` §3; item numbers (P#) are its §5 dependency map.
Draft 4 adds D58's summary (enforcement status, owner, activation trigger) and I19-I21 for the replan (D51, D52,
D63).
Draft 5 makes the slice plan's §1 gating table the one source of truth, from which the summary is derived; adds I22
(one admitted task); and settles P27's exact-old-SHA push.
Draft 3 added the I8 note on revoked-lease settlement and qualified I18's existing coverage.
Draft 2 applies the #222 round-1 reviews:
- I9 is reclassified as a gap;
- I2 consumption is corrected;
- I10 is widened;
- I16-I18 are new.

## How to read this

- **Scope.** D45 trusts controller-owned storage and approved controller code until M7-S. It keeps every
  deterministic fold check. Authority and applicability are checked transactionally at admission, at reuse and at
  effect intent, and at consumption where a contract requires it.
- **Rows.** Each invariant below has four rows:
  - **admission**: the builder or command check in the writer transaction;
  - **fold/replay**: the projection's check, run again on rebuild;
  - **consumption**: the check when a node, effect or settlement takes the input;
  - **effect**: the re-check before egress.
- **Columns.** **Today** names the file and function, or *none*. **Status** is one of:
  - **kept**: it exists, and M1 keeps it unchanged or only wires it;
  - **GAP**: M1 must add it;
  - **kept + GAP**: part exists and part is new;
  - **n/a**: the invariant has no such check point.
- **Existing checks stay.** A fold check added in M1 never replaces a builder, commit or intent check. The
  `owedanswers` fake (`internal/testguard/fakes/owedanswers`) stays for the M0 fixtures after P5 gives `adcd` the real
  authority. Removing a check needs its own reviewed PR.
- **Two effect lifecycles.** `graph.Operate`, `graph.RunEffect` and `graph.CompleteEffect` are the lifecycle of
  **graph effect nodes**, which in M1 is `merge` only. A **`model_call`** is not a graph node. It is intended by the
  gateway through `eventstore.Tx.IntendEffect` (P12a) and reconciled by P20. A graph citation below never stands for
  the model-call lifecycle.
- Paths are under `internal/` unless they start otherwise.

## Summary (D58)

**Enforcement status** is derived from an invariant's four rows:
- **enforced:** every applicable row is kept;
- **partial:** some rows are kept and some have a gap;
- **unenforced:** no applicable row is kept.

A required invariant that is not enforced **blocks every capability it gates**. **The gating table in
`00-slice-plan.md` §1 is the one source of truth.** The "Required by" column below is derived from it row for row,
and is never edited apart from it.
- **I15 is required by every capability.**
- **Owner:** the items that close the gaps.
- **Qualification evidence:** each item's named fixtures and mutations (D46, D58).

| Invariant | Enforcement status | Owner (items) | Required by (capability: rows) |
|---|---|---|---|
| I1 Recorded approvals | partial | P22a, P22b | `intake`: admission, fold (with P5's recorded answers); `merge-local`, `merge-github`: effect |
| I2 Answer binding and delivery | enforced | P5, P6 | `answers`: all |
| I3 Moved questions | enforced | P6 | `answers`: all |
| I4 Reuse | enforced | P6 | `answers`: all |
| I5 Consumption | partial | P8b, P15, P16 | `launch`: admission; `review`: all |
| I6 Effect intent | partial | P18, P20, P22a, P27 (P12a, P12b done) | `launch`: the `model_call` rows; `merge-local`, `merge-github`: the merge rows, including P20's GitHub settlement for `merge-github` |
| I7 Review independence | partial | P7, P8b, P16 | `launch`: consumption; `review`: all |
| I8 Fencing and attempt binding | partial | P8a, P21 | `launch`: all |
| I9 Accounting | partial | P11b, P20, P22a (P12a, P12b done) | `launch`: all |
| I10 Egress, data policy, context | partial | P7, P8b, P11, P11b, P16 | `launch`: all |
| I11 Restart and replay | partial | P4a, P21 | `launch`: all |
| I12 Build facts and accept | partial | P14a, P14b, P14c, P14d, P22a | `launch`: all |
| I13 Gate verdict and receipts | partial | P15 | `review`: all |
| I14 Single merge delivery | partial | P18, P19, P27 | `merge-local`: all but the GitHub effect check; `merge-github`: all |
| I15 Deferred surfaces | partial | P22a | `intake`: admission (the guard); every other capability: all |
| I16 Rails | unenforced | P17 | `launch`: all |
| I17 Containment | unenforced | P9, P11, P14a, P15 | `launch`: all |
| I18 Acceptance confidentiality | partial | P3, P15, P16 (the builder's I18, moved from P14), P22a | `intake`: admission; `review`: all |
| I19 Liveness (D63) | unenforced | P29 | `repeat`: all |
| I20 Merge approval, fresh review, required configuration | partial | P19, P28, P22b | `merge-local`: consumption, effect; `merge-github`: all |
| I21 Launch readiness (D51) | unenforced | P8b, P11, P17 | `launch`: all |
| I22 One admitted task, held included (D51) | unenforced | P22a | `admission`, `launch`, `repeat`: all |

**Lead decision (#245, M1 review of P6).** `answers` is activated by P6 with I15's admission row still a P22a gap: no template is reachable through `answers` (adcd wires no producer of graph events or owed inputs before P16). P22a must re-check this before activating any capability that reaches a template.

**Effect implementations land inert.** The `model_call` kind (P12a) and the `git` and GitHub targets (P18, P27) are
built and tested in their own items. Only P22a registers `model_call`, and only P22b registers `merge` and constructs
the targets. So I6's and I14's effect rows gate their capabilities through the activation item, never through the
implementation item.

## I1. Recorded approvals (the task's, and every answering approval)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `authority.Authorizer.Authorize` (`authority/derive.go`) decides each command against actor and grants. A task's approval is recorded with `task.create` (`authority/task.go`, P3; deferred until P22a); an answer's with `input.answer` (`authority/answer.go:InputAnswer`, `answerable`, P5; wired by P6) | kept | — |
| fold/replay | `recordApproval` (`authority/proposals.go`) hands a task's approval to `recordTaskApproval` (`authority/task.go`, P3) and an answer to `recordAnswer` (`authority/answer.go`, P5); the graph folds check the answers a claim, reuse or result takes (`graph/eligible.go:eligible`, P6) | kept | — |
| consumption | graph answers come from `authority.Answers` (`authority/owed.go`, P5), the real `graph.Authority`; nothing consumes a task's approval | kept + GAP | P22a (the first build claim requires the task approval in force) |
| effect | `graph.Operate` → `revalidate` re-checks a merge's answer inputs before its intent; the task's approval is not re-checked; the fold does not check a raw `node.operated` (P6 checks claims, reuses and results) | kept + GAP | P22b (the merge intent re-checks the task approval; a fold check of `node.operated` by `inForce`, so a raw intent does not replay once merge is registered) |

## I2. An answer binds its owed input's revision and reaches only the node that declared it

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `authority.InputAnswerHandler.Derive`, `answerable` (`authority/answer.go`, P5): the responder may answer, the owed input is open, at that revision; wired in `adcd` (P6) | kept | — |
| fold/replay | `recorded`, `succeeds` (`graph/events.go`) refuse an answer input on a first round and check a successor's shape; `recordAnswer` (`authority/answer.go`) binds an answer to its owed input's revision; `eligible` (`graph/eligible.go`, P6): a successor's new answer is to an owed input its lineage asks at its current revision (`successorOwed`), and a claim, reuse or result takes only answers in force (`inForce`) | kept | — |
| consumption | `graph/evaluate.go:answers` with `lineageOwed`, `owedIn` (`graph/successor.go`); `graph.Context` renders only claimed inputs (`renderContext`); `authority.Answers` is `adcd`'s `graph.Authority` (P5) | kept | — |
| effect | n/a: an answer is no effect; the merge intent's re-validation is I1's effect row | n/a | — |

## I3. Moved questions

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `graph.SupersedeMoving` → `moved` (`graph/supersede.go`) → `movedQuestion` (`graph/eligible.go`): through the authority, the lineage no longer asks the old owed input; a move asks the new one at its current revision, and a removal (13:206, P6) drops the answer input | kept | — |
| fold/replay | `supersededFold` (`graph/supersede.go`) checks the shape and re-derives the hold lists; `eligible` (`graph/eligible.go`, P6) re-checks each replaced or removed answer, so each `question_moved` retirement, against the owed inputs (`movesHold`), and a `revised` retirement or a signalled clear of an `awaiting_input` wait against them (`answerWaitEnds`). M0's fold (`graph.Project`) still refuses a removal | kept | — |
| consumption | `displaced` (`graph/supersede.go`) via `answers`: the evaluator waits and Commit refuses `authority_revoked`; the claim fold refuses a raw claim the same way (`inForce`, P6) | kept | — |
| effect | `revalidate` in `graph.Operate` refuses a displaced question | kept | — |

## I4. Reuse

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `graph.Reuse` → `bindsRequirement` (`graph/roster.go`), `answeredNow` (`graph/reuse.go`), the holder at its current generation (`KeyProjection`) | kept | — |
| fold/replay | `reused` (`graph/reuse.go`) re-derives the inputs and the key, requires a completed holder and binds the requirement; `eligible` (`graph/eligible.go`, P6) refuses a raw `node.reused` whose answers are not in force (`inForce`, shared with the `Reuse` builder) | kept | — |
| consumption | `selectedOf` (`graph/events.go`); `holderServes` (`graph/requirement.go`); `graph.ReviewReusable` (`graph/roster.go`), `graph.Equivalent` (`graph/equivalence.go`) | kept | — |
| effect | `verdict` (`graph/requirement.go`) through `selectedOf` at the merge intent; `mergeHolds` (`graph/queue.go`) before the apply | kept | — |

## I5. Consumption: a node consumes exactly what its claim fixed, while it still applies

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `graph.Claim` → `claimNode` (`graph/events.go`) over `graph.Resolve` → `applies` → `receipt.Applies`; `graph.ClaimReview` against the registry. No dispatcher, so a raw `node.claimed` is never re-checked before an attempt (13:106) | kept + GAP | P8b |
| fold/replay | `claimed`, `resultRecorded`, `selected` (`graph/events.go`, `graph/lifecycle.go`) refuse a result whose claimed result inputs are no longer selected; approvals are I1 | kept | — |
| consumption | `graph.Commit` → `revalidate` (`graph/evaluate.go`); `CarryOver`, `carryInputs`, `roleInput` (`graph/queue.go`). `graph.Requirements` exist only in fixtures: there is no gate contract and no review or merge requirement for a real gate | kept + GAP | P15 (the gate requirement), P16 (the review's) |
| effect | merge only: `graph.Operate` → `revalidate`; `inputsFenced`, `subjectFenced` (`graph/fence.go`) | kept | — |

## I6. Effect intent: intended before egress, one live operation per target, resumed without re-validation

| Check | Today | Status | Item |
|---|---|---|---|
| admission | merge: `graph.Operate` → `eventstore.Tx.IntendEffect` (refuses an unregistered kind), `Tx.LiveEffect`. `adcd` registers no kind. `model_call`: `modelcall.Intend` (P12a, #251) records one effect per call (target = the call id) inside the writer transaction, after checking that the attempt is current, running and on the lease and unit; the gateway sends nothing before it is committed. Inert until P22a registers the kind | kept + GAP | P18 (`merge`); P22a (register `model_call`; run `MarkPendingUncertain` on every start, #226 entry 64) |
| fold/replay | `operated`, `settledOperation` (`graph/effects.go`); the store's transitions (`eventstore/effects.go:next`), which cover both kinds | kept | — |
| consumption | merge: `graph.CompleteEffect` → `operationCompleted` (`graph/queue.go`). `model_call`: a settlement names every bound call once and takes each call's figure or liability (I9; P12b, #253) | kept | — |
| effect | merge: `graph.RunEffect` reconciles before egress and applies by compare-and-set; `unapplied` with `Landed`. At start, `MarkPendingUncertain` (`daemon/daemon.go:start`) for both kinds. Settling `uncertain` and one runner per effect are M1's (13:445-450) | kept + GAP | P20 (one runner per effect; for a GitHub push, settlement only by a received response or by P20's three history rules; an unchanged ref keeps `uncertain` and the fence, with `effect_uncertain` after `PT10M`; nothing re-fires the push) |

## I7. Review independence

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `graph.RecordReview`, `ClaimReview` → `registered` (`graph/roster.go`) against `graph.Families`, implemented only by the fake (`testguard/fakes/executors`); contributor families are not tracked | GAP | P7 (the registry, with distinct families checked at start), P16 (contributor families) |
| fold/replay | `checkRoster`, `checkReview` (`graph/roster.go`): shape, and no `unknown` family | kept | — |
| consumption | `bindsRequirement` (`graph/roster.go`), `currentRevision` (`graph/supersede.go`); no lease places a member on its pinned executor | kept + GAP | P8b |
| effect | `serves` (`graph/requirement.go`): the merge's requirement is served at the subject's selected revision | kept | — |

## I8. Fencing, with attempts bound to nodes

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `lease.StartAttempt`, `SupersedeAttempt` (`lease/wiring.go`): a first attempt is token 1 (`lease.Start`); only a `sealed` attempt is superseded, with the next token and in one event (`lease.Supersede`). No attempt names its node | kept + GAP | P8a |
| fold/replay | `lease.Projector.apply`, `start`, `changeAttempt`; `lease.ApplyAttempt`. `adcd`'s projector passes no `Units`, so every seal is refused (`daemon/wiring.go:projector`, `lease/wiring.go:checkpoint`) | kept + GAP | P8a |
| consumption | `lease.FenceOutput`, `SubmitResult`; `graph.Supersede` takes a caller-named attempt list (13:173) | kept + GAP | P8a |
| effect | `lease.RecoverAfterRestart` exists but `adcd` never runs it; orphaned exchange output of a lost attempt is never refused; nothing revokes and settles a lost attempt's lease | GAP | P21 (X3's six-step sequence) |

**Note (I8).** A revoked lease never settles before its attempt's publication is closed. The existing check is in
`lease/wiring.go:changeLease`: a lease leaving `active` while its attempt's publication is open is refused, and
`lease.completed` is refused unless the attempt is completed. So after a restart, the old lease goes
`lease.revoked{controller_restart}` → `settling` → `settled` once the attempt is `lost` (publication closed), never
through `lease.completed`. P21 keeps this check.

## I9. Accounting: every call charged once, uncertain usage never dropped

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `capacity.Reservations.reserve` (`capacity/reserve.go`), all-or-nothing, wired; admission counts liabilities like reservations (saturating). The durable binding call → attempt → lease → groups is `model_call.intended` (P12a, #251); `Intend` refuses a non-positive reservation and a second call on a lease (P12b, #253) | kept + GAP | P22a (wire `modelcall.Project` and its lease projection, with the start check of #226 entry 70; until then the binding is inert in `adcd`) |
| fold/replay | `ledger.Project` (`capacity/ledger`): deduplicated by `(call, sequence)`; refuses a report whose attempt or lease is not the call's binding. `ledger.StandingOf` never fails: mixed currencies or an overflow leave the call uncertain at a saturated figure, flagged `inconsistent_usage`. Liability records replay; a zero liability is omitted, so older stores replay unchanged. Every command must be folded event by event (`ProjectEach`) | kept + GAP | P22a (as above: `ledger.Binding` finds nothing until the binding projections are wired) |
| consumption | A settlement names every bound call once (`capacity.Settlement`). A call is settled by a `final` or a `provider_reconciliation`; any other owes `max(positive reservation, deduplicated delta sum)` (Q8, approved 2026-10-14). Late usage recomputes the liability over every call of the settled lease, in any order. Token figures are still the adapter's report: a hostile zero `final` settles at zero until P11b (#226 entry 69), which gates `launch` | kept + GAP | P11b (gateway token count or conservative bound); P22a (start check, #226 entry 70) |
| effect | `model_call` reconciliation (03: query usage, else keep the liability) | GAP | P20 (with P12b's ledger) |

## I10. Egress, data policy and context delivery

| Check | Today | Status | Item |
|---|---|---|---|
| admission | none. No registry ties routes to data floor, routing class or usage profile, and `Lease.placement_checks` exists only in the schema. Unsupported policies must be refused, never approximated | GAP | P7 (data floor and usage-profile compatibility, and Q8's envelope, at start), P8b (destination at the launch boundary) |
| fold/replay | none: no lease records the registry or profile revisions its placement decided on | GAP | P8b |
| consumption | `graph.Context` / `renderContext` (`graph/successor.go`) render only claimed inputs. `knowledge.CheckDelivery` exists for manifests, but no model node in `adcd` records a `manifest.intent` | kept + GAP | P16 (the builder's and the reviewer's `manifest.intent`, #226 entry 71) |
| effect | none: there is no gateway. 01 §2.2 requires, before egress, the destination check, credential injection and the model checked against the lease | GAP | P11 |

## I11. Restart and replay

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `checkCapacityHistory`, `checkCapacityGroups`, `checkKinds` (`daemon/startup.go`); no graph history check (X1 §4, 15 §3.1) | kept + GAP | P4a |
| fold/replay | `checkProjections` (`daemon/startup.go`) compares replay for the wired projectors; graph kinds are not wired | GAP | P4a |
| consumption | at start, running attempts become `lost` before anything else runs (05): not wired | GAP | P21 |
| effect | `markPending` → `MarkPendingUncertain` (`daemon/startup.go`, `daemon/daemon.go:start`) | kept | — |

## I12. Build outcome from facts, and the accept boundary

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `graph.Build` (`graph/build.go`) from `BuildFacts`; ingestion via `attemptRun.admit` (`adapterhost/submit.go`); protected paths and unsafe output refused at accept (`internal/accept`, P14a, #252); no integrity guard yet | kept + GAP | P14b (Go), P14c (Python), P14d (Go table rows, subtests, helper skips); P22a (wires `accept.Accept` into the build path) |
| fold/replay | `graph.Check`, `CheckRead` (`graph/build.go`) | kept | — |
| consumption | the gate must take only an accepted tree; no template wires that | GAP | P22a (P14a provides the accepted tree) |
| effect | n/a | n/a | — |

## I13. Gate verdict from collected results, and a receipt bound to its tree

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `receipt.Check`, `Derive` (`receipt/receipt.go`); no gate contract or result adapter exists | kept + GAP | P15 |
| fold/replay | `refreshed` (`graph/resolve.go`), `resultSemantics` (`graph/queue.go`) | kept | — |
| consumption | `graph.Resolve` → `receipt.Applies` (subject, tree, environment, demand) | kept | — |
| effect | `mergeInputs` (`graph/queue.go`): the merge takes exactly its generation's refreshed checks on the prospective tree | kept | — |

## I14. Single merge delivery

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `graph.Generate`, `CarryOver` (`graph/queue.go`); the rebase result is trusted from its committer (13:536-538) | kept + GAP | P19 |
| fold/replay | `generationRecorded`, `carryRecorded`, `mergeRecorded`, `queueObserved` (`graph/queue.go`) | kept | — |
| consumption | `verdict` (`graph/requirement.go`), `mergeHolds` (`graph/queue.go`) | kept | — |
| effect | `graph.RunEffect` with the generation's `expected` base, and `Landed`; only the fake forge implements it, and its versions reset with it | kept + GAP | P18 (the `git` target and a persisted counter). P27 (the GitHub target) pushes with a **server-enforced exact old SHA**, `git push --force-with-lease=refs/heads/main:<expected-base-sha>`, and a single-parent prospective commit. States: `before_send` (a local refusal before egress, recorded explicitly) → `uncertain` from the first byte the pusher may write → settled only by a received response or by P20's history rules. The pusher's `PR_SET_PDEATHSIG` is process hygiene and settles nothing. Fixtures: `github-rewind-scratch` (rejection received, `failed_confirmed`), `github-delayed-apply` (`uncertain` across a restart, then `completed`), `crash-unknown-github` (`uncertain`, then rule 2). P22b activates both targets |

## I15. Deferred surfaces stay unreachable (D47)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `checkEvidence` (`graph/events.go`); no M1 template exists, so nothing refuses a deferred kind in it | kept + GAP | P22a (the guard refuses an `evidence` node and asserts that `Env.Contracts` is empty; gate contracts live apart, P15) |
| fold/replay | the evidence-node folds (`graph/evidence.go`) | kept | — |
| consumption | `graph.ClaimEvidence` → `contractOf` (`graph/evidence.go`) fails closed with no contracts; `adcd` passes none | kept | — |
| effect | `eventstore.Tx.IntendEffect` refuses an unregistered kind; `remote_process` (`evidence.RemoteProcessKind`) is not registered in `adcd` | kept | — |

## I16. Retries bounded by rails

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `graph/holds.go:retry` refuses a reason that names no hold ("the rails decide"); the profile has USD rails only (`budget.rails`) | GAP | P17 (attempt, round and merge-operation caps; `budget_rail`; `task.raise_rail`) |
| fold/replay | none: no fold counts attempts per round or rounds per task | GAP | P17 |
| consumption | none: a claim or attempt start beyond a cap is not refused | GAP | P17 |
| effect | a `failed_confirmed` merge stays the node's outcome until an invalidation (13); there is no bounded re-operation | GAP | P17 |

## I17. Containment: builders, tests and adapters cannot reach controller authority (D45)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | the test sandbox only (`scripts/test-sandbox.sh`, `make test-sandbox-selftest`), which stays. There is no product sandbox | GAP | P9 (phase 2 on the pinned prepared image; P10 when the registry proxy activates) |
| fold/replay | n/a: containment is not a projection | n/a | — |
| consumption | results come from a mount the controller owns; the controller never runs `git` on a workspace's `.git` | GAP | P9, P14a, P15 |
| effect | the gateway socket is the only egress of a build (no network for a gate) | GAP | P9, P11 |

## I18. Acceptance confidentiality: `hidden/` never reaches a builder or a reviewer

| Check | Today | Status | Item |
|---|---|---|---|
| admission | none: there are no task acceptance blobs | GAP | P3 (classed restricted from builder and reviewer) |
| fold/replay | none: no `manifest.intent` of a build or review exists to check | GAP | P16 (the builder's and reviewer's intent; I18 moved from P14, #226 entry 71; the folds refuse an acceptance ref in a build's or review's intent) |
| consumption | `graph.Context` renders only claimed inputs. `graph/content.go:classed` filters **evidence results only**; every other result kind is rendered whole, so it establishes nothing for acceptance blobs or gate results | kept + GAP | P3 (acceptance blobs classed), P22a (acceptance refs refused as build or review inputs, with consumption-path fault evidence), P15 (gate results sanitised for consumers: failure text kept as a diagnostic blob), P16 (manifests and renderers take ids and statuses only); plus X1's stub-log canary |
| effect | n/a: the stub-log canary assertion (X1) is the check | n/a | — |

## I19. Liveness: every non-terminal task has a frontier (D63)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | none: there is no reconcile driver | GAP | P29 |
| fold/replay | none: no projection derives a task's frontier (a running attempt, a requested or granted lease, a registered hold, a claimable producer) | GAP | P29 (the frontier, derived from graph, lease and hold projections; the board reuses it) |
| consumption | none: nothing raises `stalled` | GAP | P29 (every reconcile pass checks; a frontierless task gets `stalled`, naming the frontier, owned by the operator) |
| effect | `holds.yaml` has no `stalled` entry; paging exists for registered holds only (`graph.Page`) | GAP | P29 (the entry, with signal `revision`, `page_after: PT15M`, `escalate_to: null`) |

## I20. Merge approval, fresh review after a base move, and required configuration (D52)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | none: no merge-approval owed input, and no `required_config` field in `config/project-profile` (07 "not in the schema yet") | GAP | P28 |
| fold/replay | the answer folds once P6 lands (I2); generation pins (`generationRecorded`, `graph/queue.go`) | kept + GAP | P6, P28 (an approval binds its generation) |
| consumption | `carryDecision` (`graph/queue.go`) lets a carried review attest across a base move; M1 requires a fresh review instead | kept + GAP | P19, P28 (the M1 template demands a successor review in every new generation) |
| effect | `graph.Operate` → `revalidate` re-checks a merge's answer inputs before its intent | kept + GAP | P28 (the operator's approval of this generation, and the required-configuration answer, are answer inputs of the merge), P22b |

## I21. Launch readiness: no launch without the destination's credential and the pinned image (D51)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | none: `attempt.started` checks only the lease and the unit (`lease.Projector.start`) | GAP | P8b (image digest present), P11 (the gateway holds the destination's credential) |
| fold/replay | none: rail counters do not exist yet, so nothing says a refused launch spends none | GAP | P17 (a refused launch counts against no rail) |
| consumption | n/a: a refused launch consumes nothing | n/a | — |
| effect | `lease.start_refused` → `releasing` exists (`lease/lease.go` table); no hold names the missing item | kept + GAP | P8b (`source_invalid_config` for a credential, `wrong_owner` for an image) |


## I22. One admitted task through M1, held tasks included (D51)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | none: there is no admission, and `task.create` is deferred | GAP | P22a (`task.admitted` only when no other admitted task is non-terminal, a held one included; creation only puts a task in Ready, at most five per project, P3) |
| fold/replay | none | GAP | P22a (the fold refuses a second `task.admitted` while an admitted task is non-terminal) |
| consumption | none: a claim does not check its task's admission | GAP | P22a (a node of an unadmitted task is never claimed; fixture `admission-held`) |
| effect | n/a: admission is no external effect | n/a | — |

## Count

88 rows across 22 invariants. **54 rows have a gap**: 29 are GAP and 25 are kept + GAP. 28 are kept and 6 are n/a (after P6, which also flipped P3's and P5's I1 and I2 rows, #226 entry 45).
Every gap is closed by an item of `00-slice-plan.md` §5. None is left to a later milestone, except where 01 §4.1 or
§6's clarifications place it. The evidence rows of 12 follow #187's trigger (slice plan §4).
