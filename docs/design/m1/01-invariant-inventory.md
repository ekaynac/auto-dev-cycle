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
| I5 Consumption | partial | P15, P16, P22a (P8b done, inert) | `launch`: admission; `review`: all |
| I6 Effect intent | partial | P18, P20 (merge half), P22a, P27 (P12a, P12b and P20's `model_call` half done) | `launch`: the `model_call` rows; `merge-local`, `merge-github`: the merge rows, including P20's GitHub settlement for `merge-github` |
| I7 Review independence | partial | P22a (P7, P8b, P16b done, inert) | `launch`: consumption; `review`: all |
| I8 Fencing and attempt binding | partial | P22a (P8a done; P21a, P21b done, inert) | `launch`: all |
| I9 Accounting | partial | P22a (P11b, P12a, P12b, P20's `model_call` half and P21a done, inert) | `launch`: all |
| I10 Egress, data policy, context | partial | P16, P22a (P7, P8b, P11, P11b done, inert) | `launch`: all |
| I11 Restart and replay | partial | P4a, P22a (P21a done, inert) | `launch`: all |
| I12 Build facts and accept | partial | P14b-out, P14c-2a, P14c-2a-allow, P14c-2a2, P14c-2a3, P14c-2b, P14d, P22a (P14a, P14b, P14c-1 done, inert) | `launch`: all |
| I13 Gate verdict and receipts | partial | P15 | `review`: all |
| I14 Single merge delivery | partial | P18, P19, P27 | `merge-local`: all but the GitHub effect check; `merge-github`: all |
| I15 Deferred surfaces | enforced | P22a | `intake`: admission (the guard); every other capability: all |
| I16 Rails | unenforced | P17 | `launch`: all |
| I17 Containment | unenforced | P9, P14a, P15 (P11 done, inert) | `launch`: all |
| I18 Acceptance confidentiality | partial | P15, P16 (the builder's I18, moved from P14), P22a | `intake`: admission; `review`: all |
| I19 Liveness (D63) | unenforced | P29 | `repeat`: all |
| I20 Merge approval, fresh review, required configuration | partial | P19, P28, P22b | `merge-local`: consumption, effect; `merge-github`: all |
| I21 Launch readiness (D51) | unenforced | P17, P22a (P8b, P11 done, inert) | `launch`: all |
| I22 One admitted task, held included (D51) | enforced | P22a (P22a-3) | `admission`, `launch`, `repeat`: all |

**Lead decision (#245, M1 review of P6).** `answers` is activated by P6 with I15's admission row still a P22a gap: no template is reachable through `answers` (adcd wires no producer of graph events or owed inputs before P16). P22a must re-check this before activating any capability that reaches a template. **Re-checked by P22a-2:** `adcd`'s graph fold refuses every node kind outside M1(a)'s (I15 admission), so `answers` now holds every row it needs.

**In code (P22a-2).** `internal/capability.Table` is §1's table, with one bit per required row, set by the PR that keeps the row. `adcd` wires a capability's actions and registers its effect and node kinds only when every bit is set and nothing is pending; `adc dev capabilities --json` prints the table compiled into `adc`. `TestKeptIsTheInventory` ties each bit to this file's status (kept or n/a), and `TestRowsAreTheInventory` each capability's rows to the "Required by" column above.

**Effect implementations land inert.** The `model_call` kind (P12a) and the `git` and GitHub targets (P18, P27) are
built and tested in their own items. Only P22a registers `model_call`, and only P22b registers `merge` and constructs
the targets. So I6's and I14's effect rows gate their capabilities through the activation item, never through the
implementation item.

## I1. Recorded approvals (the task's, and every answering approval)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `authority.Authorizer.Authorize` (`authority/derive.go`) decides each command against actor and grants. A task's approval is recorded with `task.create` (`authority/task.go`, P3; deferred until P22a); an answer's with `input.answer` (`authority/answer.go:InputAnswer`, `answerable`, P5; wired by P6) | kept | — |
| fold/replay | `recordApproval` (`authority/proposals.go`) hands a task's approval to `recordTaskApproval` (`authority/task.go`, P3) and an answer to `recordAnswer` (`authority/answer.go`, P5); the graph folds check the answers a claim, reuse or result takes (`graph/eligible.go:eligible`, P6) | kept | — |
| consumption | graph answers come from `authority.Answers` (`authority/owed.go`, P5), the real `graph.Authority`. A task's approval: `adcd`'s graph fold (`graph.Projector.Tasks` = `authority.Tasks`, P22a-3) refuses a claim or reuse of a node of a task whose approval is not in force, `failed/authority_revoked`, so a revocation before the first claim starts no attempt | kept | — |
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
| admission | `graph.Claim` → `claimNode` (`graph/events.go`) over `graph.Resolve` → `applies` → `receipt.Applies`; `graph.ClaimReview` against the registry. `dispatch.Match` (P8b, #247) is the one entry point: before it reserves, it re-checks that the lease's unit is the claimed node's, of the card's project, and the roster against the registry (13:106; `graph.Units.Placement`). Kept in code, inert until P22a's reconcile loop | kept + GAP | P22a |
| fold/replay | `claimed`, `resultRecorded`, `selected` (`graph/events.go`, `graph/lifecycle.go`) refuse a result whose claimed result inputs are no longer selected; approvals are I1 | kept | — |
| consumption | `graph.Commit` → `revalidate` (`graph/evaluate.go`); `CarryOver`, `carryInputs`, `roleInput` (`graph/queue.go`). `graph.Requirements` exist only in fixtures: there is no gate contract and no review or merge requirement for a real gate | kept + GAP | P15 (the gate requirement), P16 (the review's) |
| effect | merge only: `graph.Operate` → `revalidate`; `inputsFenced`, `subjectFenced` (`graph/fence.go`) | kept | — |

## I6. Effect intent: intended before egress, one live operation per target, resumed without re-validation

| Check | Today | Status | Item |
|---|---|---|---|
| admission | merge: `graph.Operate` → `eventstore.Tx.IntendEffect` (refuses an unregistered kind), `Tx.LiveEffect`. `adcd` registers no kind. `model_call`: `modelcall.Intend` (P12a, #251) records one effect per call (target = the call id) inside the writer transaction, after checking that the attempt is current, running and on the lease and unit; the gateway sends nothing before it is committed. Inert until P22a registers the kind | kept + GAP | P18 (`merge`); P22a (register `model_call`; run `MarkPendingUncertain` on every start, #226 entry 64) |
| fold/replay | `operated`, `settledOperation` (`graph/effects.go`); the store's transitions (`eventstore/effects.go:next`), which cover both kinds | kept | — |
| consumption | merge: `graph.CompleteEffect` → `operationCompleted` (`graph/queue.go`). `model_call`: a settlement names every bound call once and takes each call's figure or liability (I9; P12b, #253) | kept | — |
| effect | merge: `graph.RunEffect` reconciles before egress and applies by compare-and-set; `unapplied` with `Landed`. At start, `MarkPendingUncertain` (`daemon/daemon.go:start`) for both kinds. Settling `uncertain` and one runner per effect are M1's (13:445-450). `model_call`: `modelcall.Reconciler` (P20's `model_call` half, #258) runs one settlement per effect at a time and settles `uncertain` (I9's effect row); kept in code, inert until P22a constructs it | kept + GAP | P22a (one `Reconciler` per store, #258); P20's merge half (one runner per merge effect; for a GitHub push, settlement only by a received response or by P20's three history rules; an unchanged ref keeps `uncertain` and the fence, with `effect_uncertain` after `PT10M`; nothing re-fires the push) |

## I7. Review independence

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `graph.RecordReview`, `ClaimReview` → `registered` (`graph/roster.go`) against `graph.Families`. The registry (`internal/registry`, P7 #236; `adcd run --registry`) refuses equal builder and reviewer families at start (`distinct`) and implements `graph.Families` (`Registry.Family`). Contributor families (P16b): `graph.Contributors` reads the families of every lease on the task's build nodes, through today's registry (the operator never re-declares an executor id under another family), and counts `unknown` for a build with any recorded result (selected or not, invalidated or not) and no lease; `ClaimReview` and the commit of a review's result (but a `could_not_run` one) refuse a roster member of a contributing family, or any member once a contributor's family is unknown, and a review receipt the result holds is checked by `receipt.Independent` against them. A refusal's hold names the contributor (`contributor:<executor or node>`), not the reviewer's registry entry. Kept in code, inert until P22a | kept + GAP | P22a |
| fold/replay | `checkRoster`, `checkReview` (`graph/roster.go`): shape, and no `unknown` family | kept | — |
| consumption | `bindsRequirement` (`graph/roster.go`), `currentRevision` (`graph/supersede.go`); `dispatch.Match` (P8b, #247) places a rostered node only on its pinned roster member, a reviewer, and any other node only on a builder. Kept in code, inert until P22a | kept + GAP | P22a |
| effect | `serves` (`graph/requirement.go`): the merge's requirement is served at the subject's selected revision | kept | — |

## I8. Fencing, with attempts bound to nodes

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `lease.StartAttempt`, `SupersedeAttempt` (`lease/wiring.go`): a first attempt is token 1 (`lease.Start`); only a `sealed` attempt is superseded, with the next token and in one event (`lease.Supersede`); `lease.Replaceable` refuses a replacement while the old lease is settling or restoring, and `dispatch.placed` refuses a match while the unit's last lease has not settled (P21a, #259). An active old lease is still admitted (#226 entry 83). An attempt's unit is its node's (`graph.UnitOf`, P8a #244) | kept + GAP | P22a (refuse every old lease that has not ended, #226 entry 83) |
| fold/replay | `lease.Projector.apply`, `start`, `changeAttempt`; `lease.ApplyAttempt`. `adcd`'s projector passes `Units: graph.Units{}` (`daemon/wiring.go:projector`, P8a #244): an attempt starts or is superseded only on a claimed node's unit (`Startable`), on replay as at admission, and a seal binds its node's project and task (`lease/wiring.go:checkpoint`) | kept | — |
| consumption | `lease.FenceOutput`, `SubmitResult`; `graph.Supersede` derives its attempt list from the predecessor's unit (P8a, #244, 13:173), and the `result.recorded` fold refuses an attempt never started or started on another node's unit (`attemptBound`) | kept + GAP | P22a (the completion path for a `finishing` attempt, #226 entry 83) |
| effect | `recovery.Recovery.Run` (P21a, #259): X3's steps 1-4, each its own command (`lease.RecoverAfterRestart`, then revoke, seal and settle); `recovery.Next` converges every attempt a crash can cut. `adcd` runs it at every start, after `MarkPendingUncertain` and before any other command (P22a-1; `daemon/startup.go:recoverAtStart`). `Recovery.RefuseOrphans` (P21b, #261) records `attempt.output_refused{stale_attempt}` for output a restart-cut attempt left in its exchange root, bound through store records and never read; kept in code, inert: it needs the adapter host's exchange root, which launch constructs | kept + GAP | P22a-5 (`RefuseOrphans` before any launch, after killing orphaned adapter trees; the completion path for a `finishing` attempt; #226 entries 81 and 83) |

**Note (I8).** A revoked lease never settles before its attempt's publication is closed. The existing check is in
`lease/wiring.go:changeLease`: a lease leaving `active` while its attempt's publication is open is refused, and
`lease.completed` is refused unless the attempt is completed. So after a restart, the old lease goes
`lease.revoked{controller_restart}` → `settling` → `settled` once the attempt is `lost` (publication closed), never
through `lease.completed`. P21a keeps this check.

## I9. Accounting: every call charged once, uncertain usage never dropped

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `capacity.Reservations.reserve` (`capacity/reserve.go`), all-or-nothing, wired; admission counts liabilities like reservations (saturating). The durable binding call → attempt → lease → groups is `model_call.intended` (P12a, #251); `Intend` refuses a non-positive reservation and a second call on a lease (P12b, #253). `adcd` registers the event kind and projects the binding (`modelcall.Project`, by call and by lease; P22a-1); the `model_call` effect kind stays unregistered until `launch`, so nothing intends a call yet | kept + GAP | P22a-5 (register the effect kind, construct the Recorder and the Reconciler) |
| fold/replay | `ledger.Project` (`capacity/ledger`): deduplicated by `(call, sequence)`; refuses a report whose attempt or lease is not the call's binding. `ledger.StandingOf` never fails: mixed currencies or an overflow leave the call uncertain at a saturated figure, flagged `inconsistent_usage`. Liability records replay; a zero liability is omitted, so older stores replay unchanged. Every command must be folded event by event (`ProjectEach`). The binding projections are wired in `adcd` (P22a-1), so a settlement at start keeps an uncertain call as a liability (#226 entry 70) | kept | — |
| consumption | A settlement names every bound call once (`capacity.Settlement`). A call is settled by a `final` or a `provider_reconciliation`; any other owes `max(positive reservation, deduplicated delta sum)` (Q8, approved 2026-10-14). Late usage recomputes the liability over every call of the settled lease, in any order. **P11b (#256):** the gateway counts every request it sends. Before the send it commits a floor (`gateway_floor`: request bytes ÷ 16 input, `max_tokens` or the reservation output), and it refuses the send if the floor is not recorded. The count read from the forwarded response supersedes the floor. A `final` settles only when every request has a count and the final is not below it (input+cache and output apart); otherwise the call stays uncertain at `max(positive reservation, the field-wise maximum of the reports and the gateway's counts)`, so a hostile zero `final` keeps a conservative charge (#226 entry 69). P21a's step 4 (`recovery.settle`, #259) settles a restart-cut call this way (X3(b)). Kept in code, inert until P22a | kept + GAP | P22a (start check, #226 entry 70; run recovery at start, #226 entry 81) |
| effect | `model_call` reconciliation (03: query usage, else keep the liability): `modelcall.Reconciler` (P20's `model_call` half, #258). It queries the provider only when every floor-only request is older than `FloorGrace`. It never releases a call at zero (no query, no usage found, or a failed query leave the liability), and a reconciliation never settles below the gateway's observed counts or floor inputs (flagged `inconsistent_usage`). Kept in code, inert until P22a | kept + GAP | P22a (one `Reconciler` per store; authorise `model_call.reconcile` for the system actor; qualify each production `Query.Usage` to honour cancellation; #226 entries 60 and 78) |

## I10. Egress, data policy and context delivery

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `registry.New` checks Q8's envelope at start, and `registry.Admits` (P7, #236) checks the data floor and the allowed destinations (an empty `allowed_destinations` is refused). `dispatch.Match` (P8b, #247) derives the placement from the registry and calls `Admits` before any reservation. Unsupported policies are refused, never approximated. Kept in code, inert until P22a | kept + GAP | P22a |
| fold/replay | the lease records its seven `placement_checks`, each naming what it decided on (the data policy's digest, the pinned executor's registry-entry digest, the image pin, the roster's digest; `internal/dispatch/dispatch.go:checks`, P8b #247), and `Launch` re-derives them before the start. Kept in code, inert until P22a | kept + GAP | P22a |
| consumption | `graph.Context` / `renderContext` (`graph/successor.go`) render only claimed inputs. `knowledge.CheckDelivery` exists for manifests, but no model node in `adcd` records a `manifest.intent` | kept + GAP | P16 (the builder's and the reviewer's `manifest.intent`, #226 entry 71) |
| effect | 01 §2.2 requires, before egress, the destination check, credential injection and the model checked against the lease: `internal/gateway` (P11, #248). P11b (#256) widens the response redaction: every JSON spelling of the credential and a tail cut at EOF are redacted, and only known media types and charsets are forwarded. Kept in code, inert: `adcd` constructs no gateway | kept + GAP | P22a (construct the gateway; its socket server's write timeout, #226 entry 78) |

## I11. Restart and replay

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `checkCapacityHistory`, `checkCapacityGroups`, `checkKinds` (`daemon/startup.go`); no graph history check (X1 §4, 15 §3.1) | kept + GAP | P4a |
| fold/replay | `checkProjections` (`daemon/startup.go`) compares replay for the wired projectors; graph kinds are not wired | GAP | P4a |
| consumption | at start, running attempts become `lost` before anything else runs (05): `Recovery.Run`'s step 1 (P21a, #259), run by `adcd` at every start after `MarkPendingUncertain` (P22a-1). A recovery that leaves an attempt running refuses the start; a later step's failure is noted and resumed at the next start | kept | — |
| effect | `markPending` → `MarkPendingUncertain` (`daemon/startup.go`, `daemon/daemon.go:start`) | kept | — |

## I12. Build outcome from facts, and the accept boundary

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `graph.Build` (`graph/build.go`) from `BuildFacts`; ingestion via `attemptRun.admit` (`adapterhost/submit.go`); protected paths and unsafe output refused at accept (`internal/accept`, P14a, #252). P14b-init's Go integrity guard (#255) and P14c-1's Python tokenizer and encoding gate (#260) are kept in code, inert until P22a; P14b's fail-closed Python rule holds until P14c-2b | kept + GAP | P14b-out (Go, outside init), P14c-2a, P14c-2a-allow, P14c-2a2, P14c-2a3 and P14c-2b (Python), P14d (Go table rows, subtests, helper skips); P22a (wires `accept.Accept` into the build path) |
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
| admission | `checkEvidence` (`graph/events.go`); the deferred-surface guard (P22a-2, an allowlist: `graph.M1Kinds`, and `merge` only while a merge capability is active). `adcd`'s graph fold refuses a `node.recorded` of any other kind, a template's or not, on append and on replay (`graph.Projector.Kinds`, `daemon/wiring.go:nodeKinds`); `graph.CheckSurface` refuses the same of a template, and an `Env` with instrument contracts (gate contracts live apart, P15) | kept | — |
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
| effect | the gateway socket is the only egress of a build (no network for a gate). The gateway and its socket exist (P11, #248), inert until P22a; the sandbox's confinement is P9's | kept + GAP | P9 (P11 done, inert) |

## I18. Acceptance confidentiality: `hidden/` never reaches a builder or a reviewer

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `task.create` (P3) records a task's acceptance as its spec's attachments, each classed `restricted` and withheld from builder and reviewer (`authority/task.go:acceptanceOf`); the fold refuses any other classing (`classed`). Recorded as kept by P22a-2: P3 closed it, and this row was not updated | kept | — |
| fold/replay | none: no `manifest.intent` of a build or review is recorded yet to check | GAP | P22a (the folds refuse an acceptance ref in a recorded build's or review's intent, once intents are recorded; I18 moved from P14, #226 entry 71) |
| consumption | `graph.Context` renders only claimed inputs. `graph/content.go:classed` filters **evidence results only**. P16a's `intent.Builder` and `intent.Reviewer` refuse an input that is, or derives at any depth from, an acceptance blob, walking authoritative ref records (`graph.Records`: identity and complete lineage, bounded) against the task's own acceptance (`authority.TaskAcceptance`), and a reviewer input that is a review's result or its content (a fresh context per round); nothing calls them until P22a. Lineage stays truthful: a gate result that ran the acceptance overlay derives from it, so the builder never takes its report, and the reviewer takes the sanitised gate report only through an explicit controller declassification on the record (P22a, #226 entry 91) | kept + GAP | P3 (acceptance blobs classed), P16a (the builder's and reviewer's intent), P22a (calls them for every build and review, with consumption-path fault evidence; the reviewer's declassification of the sanitised gate report), P15 (gate results sanitised for consumers); plus X1's stub-log canary |
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
| admission | launch readiness at `attempt.started` (`internal/dispatch/launch.go`, P8b #247): `Observe` and `Launch` check that the pinned image is present at its digest, re-check the roster, executor and destination, and ask the gateway whether it holds the destination's credential (`gateway.Holds`, P11 #248). Kept in code, inert until P22a | kept + GAP | P22a |
| fold/replay | none: rail counters do not exist yet, so nothing says a refused launch spends none | GAP | P17 (a refused launch counts against no rail) |
| consumption | n/a: a refused launch consumes nothing | n/a | — |
| effect | `lease.start_refused` → `releasing` (`lease/lease.go` table). On a miss, `dispatch.Launch` (P8b, #247) refuses the start, releases the lease and raises a registered node hold naming the missing item: `source_invalid_config` for a credential, `wrong_owner` for an image. Kept in code, inert until P22a | kept + GAP | P22a |


## I22. One admitted task through M1, held tasks included (D51)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `authority.AdmitTask` (P22a-3), in the controller's admitting command: `task.admitted/v1` only of a task in Ready (rechecked there: a task cancelled since is refused) with its approval in force, while no admitted task is non-terminal (the tasks projection's `admitted` key, a held task included; only `task.done/v1` frees it), then the candidate template under the guard. Creation only puts a task in Ready, at most five per project (P3) | kept | — |
| fold/replay | `authority/admit.go:admitTask`: the fold refuses, from the log alone, an admission by any actor but the system's, of a task not in Ready or without its approval in force, naming another approval, or while the `admitted` key is held; `doneTask` frees it, of the admitted task only | kept | — |
| consumption | `graph.Projector.Tasks` (`authority.Tasks`, wired in `adcd`): the fold refuses a `node.claimed` or `node.reused` of a node whose task is not admitted (`graph.ErrNotAdmitted`), on append and on replay. The fixture `admission-held` is P22a-7's | kept | — |
| effect | n/a: admission is no external effect | n/a | — |

## Count

88 rows across 22 invariants. **44 rows have a gap**: 15 are GAP and 29 are kept + GAP. 38 are kept and 6 are n/a (after P6, which also flipped P3's and P5's I1 and I2 rows, #226 entry 45; and after P7, P8a, P8b, P11, P11b, P20's `model_call` half, P21a and P21b, whose rows are kept in code, with the wiring left to P22a; P8a's I8 fold row is kept, since `adcd` wires `graph.Units`; P22a-1 kept the I9 fold row and the I11 consumption row, wiring the model calls' bindings and restart recovery at start; P22a-2 kept I15's admission row, with the guard, and I18's, which P3 had closed; P22a-3 kept I22's admission, fold and consumption rows and I1's consumption row, activating `admission`). A recount on 2026-10-14 corrected the earlier figures (29 GAP, 28 kept).
Every gap is closed by an item of `00-slice-plan.md` §5. None is left to a later milestone, except where 01 §4.1 or
§6's clarifications place it. The evidence rows of 12 follow #187's trigger (slice plan §4).
