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
| I16 Rails | partial | P17b, P22a (P17a done, inert) | `launch`: all |
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
| consumption | graph answers come from `authority.Answers` (`authority/owed.go`, P5), the real `graph.Authority`. A task's approval: `adcd`'s graph fold (`graph.Projector.Tasks` = `authority.Tasks`, P22a-3) refuses every run of a node of a task whose approval is not in force, `failed/authority_revoked`: a claim, a reuse, a re-claim by invalidation or renewal; and the lease fold refuses its attempt start (`graph.Units.Tasks`). A revocation before the first claim, or between a claim and its launch, starts no attempt. Such a task ends abandoned (`task.done` as abandoned, `authority_revoked`; #226 entry 100) | kept | — |
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
| admission | `graph.Claim` → `claimNode` (`graph/events.go`) over `graph.Resolve` → `applies` → `receipt.Applies`; `graph.ClaimReview` against the registry. `dispatch.Match` (P8b, #247) is the one entry point: before it reserves, it re-checks that the lease's unit is the claimed node's, of the card's project, and the roster against the registry (13:106; `graph.Units.Placement`). Kept in code, inert until P22a's reconcile loop | kept + GAP | P22a (P22a-5: before `launch` is active, the reconcile loop's `graph.Env` in `adcd` carries `Need`, the gate's requirements, and `Blobs`, the receipt blobs, without which every evaluation is refused; Opus #312 L5) |
| fold/replay | `claimed`, `resultRecorded`, `selected` (`graph/events.go`, `graph/lifecycle.go`) refuse a result whose claimed result inputs are no longer selected; approvals are I1 | kept | — |
| consumption | `graph.Commit` → `revalidate` (`graph/evaluate.go`); `CarryOver`, `carryInputs`, `roleInput` (`graph/queue.go`). `graph.Requirements` exist only in fixtures: there is no gate contract and no review or merge requirement for a real gate | kept + GAP | P15 (the gate requirement), P16 (the review's) |
| effect | merge only: `graph.Operate` → `revalidate`; `inputsFenced`, `subjectFenced` (`graph/fence.go`) | kept | — |

## I6. Effect intent: intended before egress, one live operation per target, resumed without re-validation

| Check | Today | Status | Item |
|---|---|---|---|
| admission | merge: `graph.Operate` → `eventstore.Tx.IntendEffect` (refuses an unregistered kind), `Tx.LiveEffect`. `adcd` registers no kind. `model_call`: `modelcall.Intend` (P12a, #251) records one effect per call (target = the call id) inside the writer transaction, after checking that the attempt is current, running and on the lease and unit; the gateway sends nothing before it is committed. Inert until P22a registers the kind | kept + GAP | P18 (`merge`); P22a-5 (register `model_call`; `MarkPendingUncertain` runs at every non-offline start since P22a-1) |
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
| admission | `capacity.Reservations.reserve` (`capacity/reserve.go`), all-or-nothing, wired; admission counts liabilities like reservations (saturating). The durable binding call → attempt → lease → groups is `model_call.intended` (P12a, #251); `Intend` refuses a non-positive reservation and a second call on a lease (P12b, #253). `adcd` registers the event kind and projects the binding (`modelcall.Project`, by call and by lease; P22a-1); the `model_call` effect kind stays unregistered until `launch`, so nothing intends a call yet | kept + GAP | P22a-5 (register the effect kind; construct the Recorder and the Reconciler with the system principal and one shared `modelcall.Clock`) |
| fold/replay | `ledger.Project` (`capacity/ledger`): deduplicated by `(call, sequence)`; refuses a report whose attempt or lease is not the call's binding. `ledger.StandingOf` never fails: mixed currencies or an overflow leave the call uncertain at a saturated figure, flagged `inconsistent_usage` (an overflow also `Saturated`, and its liability with it). Liability records replay; a zero liability is omitted, so older stores replay unchanged. Every command must be folded event by event (`ProjectEach`). The binding projections are wired in `adcd` (P22a-1), so a settlement at start keeps an uncertain call as a liability (#226 entry 70) | kept | — |
| consumption | A settlement names every bound call once (`capacity.Settlement`). A call is settled by a `final` or a `provider_reconciliation`; any other owes `max(positive reservation, deduplicated delta sum)` (Q8, approved 2026-10-14). Late usage recomputes the liability over every call of the settled lease, in any order. **P11b (#256):** the gateway counts every request it sends. Before the send it commits a floor (`gateway_floor`: request bytes ÷ 16 input, `max_tokens` or the reservation output), and it refuses the send if the floor is not recorded. The count read from the forwarded response supersedes the floor. A `final` settles only when every request has a count and the final is not below it (input+cache and output apart); otherwise the call stays uncertain at `max(positive reservation, the field-wise maximum of the reports and the gateway's counts)`, so a hostile zero `final` keeps a conservative charge (#226 entry 69). P21a's step 4 (`recovery.settle`, #259) settles a restart-cut call this way (X3(b)). Kept in code, inert until P22a | kept + GAP | P22a (start check, #226 entry 70; run recovery at start, #226 entry 81) |
| effect | `model_call` reconciliation (03: query usage, else keep the liability): `modelcall.Reconciler` (P20's `model_call` half, #258). It queries the provider only when every floor-only request is older than `FloorGrace`. It never releases a call at zero (no query, no usage found, or a failed query leave the liability), and a reconciliation never settles below the gateway's observed counts or floor inputs (flagged `inconsistent_usage`). Kept in code, inert until P22a | kept + GAP | P22a (one `Reconciler` per store; qualify each production `Query.Usage` to honour cancellation; #226 entries 60 and 78; `model_call.reconcile` is the system actor's alone since P22a-1) |

## I10. Egress, data policy and context delivery

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `registry.New` checks Q8's envelope at start, and `registry.Admits` (P7, #236) checks the data floor and the allowed destinations (an empty `allowed_destinations` is refused). `dispatch.Match` (P8b, #247) derives the placement from the registry and calls `Admits` before any reservation. Unsupported policies are refused, never approximated. Kept in code, inert until P22a | kept + GAP | P22a |
| fold/replay | the lease records its seven `placement_checks`, each naming what it decided on (the data policy's digest, the pinned executor's registry-entry digest, the image pin, the roster's digest; `internal/dispatch/dispatch.go:checks`, P8b #247), and `Launch` re-derives them before the start. Kept in code, inert until P22a | kept + GAP | P22a |
| consumption | `graph.Context` / `renderContext` (`graph/successor.go`) render only claimed inputs. `knowledge.CheckDelivery` exists for manifests, but no model node in `adcd` records a `manifest.intent` | kept + GAP | P16 (the builder's and the reviewer's `manifest.intent`, #226 entry 71) |
| effect | 01 §2.2 requires, before egress, the destination check, credential injection and the model checked against the lease: `internal/gateway` (P11, #248). P11b (#256) widens the response redaction: every JSON spelling of the credential and a tail cut at EOF are redacted, and only known media types and charsets are forwarded. Kept in code, inert: `adcd` constructs no gateway | kept + GAP | P22a (construct the gateway and serve `Gateway.Server()` on the attempt's socket, hand the adapter `Mint`'s pathless `base_url`, and `Unbind` at every attempt end; P22a-5g added the per-write deadline, the per-token in-flight cap and the cancelling `Unbind`, #226 entries 57, 58, 78) |

## I11. Restart and replay

| Check | Today | Status | Item |
|---|---|---|---|
| admission | `checkCapacityHistory`, `checkCapacityGroups`, `checkKinds` (`daemon/startup.go`); no graph history check (X1 §4, 15 §3.1) | kept + GAP | P4a |
| fold/replay | `checkProjections` (`daemon/startup.go`) compares replay for the wired projectors; graph kinds are not wired | GAP | P4a |
| consumption | at start, running attempts become `lost` before anything else runs (05): `Recovery.Run`'s step 1 (P21a, #259), run by `adcd` at every non-offline start after `MarkPendingUncertain` (P22a-1). A recovery that leaves an attempt running refuses the start; a later step's failure is noted and resumed at the next start | kept | — |
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
| admission | `graph/holds.go:retry` refuses a reason that names no hold ("the rails decide"). The count rails (P17a): optional `budget.rails.attempts_per_round`, `rounds_per_task`, `merge_operations_per_generation` (defaults 3, 5, 2; `graph.CapsOf`); `task.raise_rail` (`authority.TaskRaiseRailHandler`, operator only, above the cap in force) raises one for one task and clears the task's `budget_rail` holds on it (`graph.ClearRail`). The USD rails (P17b): `dispatch.Match`'s budget check refuses a lease that, with its reservation priced at the executor's declared price, would pass the task's `per_task_usd` or its project's `per_day_usd` on the grant's UTC day (`dispatch/usd.go`): settled calls at their priced usage, never the reported cost; unsettled ones at their liabilities (`capacity.LiabilitiesOf`), or, while their lease is live, at the larger of the reservation and the reports and the gateway's counts, so a hostile zero final counts (#226 entry 69). A saturated amount (reports the ledger cannot sum, `ledger.Standing.Saturated`; a liability clipped at the contract's limit) is unaffordable at any declared rate; a priced executor is checked whatever it reserves; the lease fold grants at most one priced lease per command, so no two placements in one command pass a rail together (Astra #302). `could_not_run/budget_rail_reached` names the `budget_rail` hold (`rail:per_task_usd`, `rail:per_day_usd`) the caller raises (`Refused.Hold`). `task.raise_rail` raises `per_task_usd` (`to_usd`); `per_day_usd` only a profile change. Handler built, deferred to P22a | kept + GAP | P22a (wire the handler, `graph.Env.Rails` and `dispatch.Dispatcher.Rails` to `authority.Rails`; raise the hold a refused match names; clear a `per_day_usd` hold once a new day or a profile change admits the lease; wire `modelcall.Project`, since an ended lease with no bound call costs nothing, #226 entry 70) |
| fold/replay | no counter is stored: each count is derived from records no invalidation rewrites (a unit's fencing token; a build's `Round`; the effects intended for each generation of a merge node's claim). The USD spend is derived from the leases, their bound calls' measurements and the settlements' liabilities, and each lease's grant time (`lease.GrantProjection`, folded from `lease.granted`). The `task.rail_raised` fold admits a raise only of a recorded task, by a live operator, with exactly the cap its rail takes, above the cap in force | kept + GAP | P22a |
| consumption | the evaluator holds a build past its round rail on `budget_rail` (`rail:rounds_per_task`) before claiming it; `dispatch.Launch` refuses the start of an attempt past its round's rail and holds the node (`rail:attempts_per_round`), counting the attempts of every node it replaces (`graph.RoundAttempts`: a superseded review's replacement keeps its round); a refused launch starts no attempt, so it counts against no rail. Only these commands check a rail: the folds do not, and `graph.Claim`, `lease.StartNext`, `lease.SupersedeAttempt`, `lease.StartAttempt` (which `adapterhost.Host.Run` calls directly) and the `hold_cleared` fold do not either. Kept in code, inert until P22a | kept + GAP | P22a (builds are claimed only through `Evaluate`, attempts start only through `dispatch.Launch`, and merges operate only through `Operate`; no other caller of `Claim`, `StartNext`, `SupersedeAttempt` or `StartAttempt` outside fixtures. The adapter host's start, `adapterhost.Host.Run` → `lease.StartAttempt`, goes through `dispatch.Launch` or takes an attempt already started and fenced, P22a-5; Astra #295 L1) |
| effect | `graph.Operate` holds a merge node whose earlier keys' operations reach its rail on `budget_rail` (`rail:merge_operations_per_generation`) instead of intending another. Kept in code, inert until P22a | kept + GAP | P22a |

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
| fold/replay | none: no `manifest.intent` of a build or review is recorded yet to check. The check exists: `intent.Check` / `intent.CheckRecorded` (22a-6L) re-walk a recorded intent's every listed ref under its consumer's role against the task's records. The role is derived from the intent's attempt's node (build → builder, review → reviewer), never the intent's own word; an intent of another task, with no attempt or stating another role is refused (Opus #321 H1). `Check` is the I18 check only: it does not compare `Included` with the node's rendered context (restricted evidence is `graph.Context`'s) | GAP | P22a (22a-6: the folds of recorded build and review intents call `intent.CheckRecorded`; I18 moved from P14, #226 entry 71) |
| consumption | `graph.Context` renders only claimed inputs. `graph/content.go:classed` filters **evidence results only**. P16a's `intent.Builder` and `intent.Reviewer` refuse an input that is, or derives at any depth from, an acceptance blob, walking authoritative ref records (`graph.Records`: identity and complete lineage, bounded, the task's own index range read (`graph_task_results`, registered in `addedSinceRecord`, so a store from before it builds it at start: #226 entry 107), each stored result decoded within its bound) against the task's own acceptance (`authority.TaskAcceptance`), and a reviewer input that is a review's result or its content (a fresh context per round); nothing calls them until P22a. Lineage stays truthful (22a-6L): `gate.Result` seals its receipt and report as derived from the acceptance blobs its overlay applied, so the builder never takes them. Declassification is granted to verified safe outputs only (Astra #321 H1): `graph.Records` marks candidates (`Sanitised`: the two refs of a gate node's gate_result holding exactly its receipt and a blob; `BuilderSafe`: a review node's view); `recordResult` refuses to record a gate result carrying anything beside its receipt and report (the write side: the replay fold does not, so a forged event of another shape replays, and no ref of it is a candidate, Opus #321 r2 L2); and `intent` honours a candidate only when its blob is read and verified: its digest, and its bytes strictly a gate receipt exactly as `gate.Result` makes one (`gate.CheckReceipt`: `records/receipt`, kind `gate`, its id the ref's, the gate's instrument, no call, model family or contributor, registered reasons, a tree subject, its free text each at most 1024 bytes, exactly one predicate, `tests`, with no payload and one `results` evidence item of the gate's media type: Mythos #348 M1, #355 M1), exactly a gate report's with its content checked (`gate.CheckReport`: a known outcome and reason, a hex tree id, its detail one of the controller's phrases (Mythos #348 L1), counts at least 0), or exactly the findings view `review.Result` publishes of the no_go decision it names (`review.CheckView`: severities and titles, gate ids redacted, the operator's decision, #226 entry 94(2)); a repair builder takes the view (#226 entry 91); each round's view is its own ref (its bytes name its decision and round). Records are `intent.Load`'s only, private and immutable (Astra #321 M4). **22a-6 obligation (Opus #321):** `Sanitised` lets a reviewer's intent list the gate report as Included, so 22a-6 never renders the report's bytes into a prompt: the reviewer gets counts and the verdict only (`review.Prompt`), never test ids, failure text or the report blob | kept + GAP | P3 (acceptance blobs classed), P16a (the builder's and reviewer's intent), 22a-6L (declassification, truthful gate lineage, the findings view), P22a (22a-6: calls them for every build and review, the findings-* input rendering the view, with consumption-path fault evidence), P15 (gate results sanitised for consumers); plus X1's stub-log canary |
| effect | n/a: the stub-log canary assertion (X1) is the check | n/a | — |

## I19. Liveness: every non-terminal task has a frontier (D63)

| Check | Today | Status | Item |
|---|---|---|---|
| admission | none: the reconcile loop (`internal/reconcile`, P22a-4) admits and evaluates, but checks no frontier | GAP | P29 |
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
| fold/replay | `authority/admit.go:admitTask`: the fold refuses, from the log alone, an admission by any actor but the system's, of a task not in Ready or without its approval in force, naming another approval, at another time or under another project, or while the `admitted` key is held. `doneTask` frees it, of the admitted task only: as a candidate with none of its current nodes claimed or held, abandoned with its approval not in force, and either way only once nothing runs any of its nodes, whatever their state: every lease on their units ended and every attempt on them sealed (Astra #316 H1; read from the task's node index and each unit's open index, never a scan, M3). A result with no attempt is refused on a node whose attempt is running, stopping or finishing (H1); a review's supersession records its replacement only for an admitted task (M2). Obligation for P22a-5: keep that check on every path that ends a task, now that attempts start. Its own result is recorded through `graph.CommitAttempt`. **Upgrade:** the indexes (`admissible/`, `graph_task_nodes`, `unit_open`) and the no-attempt rule are folds, and a start never repairs a projection a store holds. `graph_task_nodes` and `unit_open` are projections a store from before them never built: its first start builds them from the log (#226 entry 107), so a node, an unended lease or an unsealed attempt no longer refuses it. The `admissible/` keys sit inside `tasks`, which it holds and which is only compared. So such a store starts only if its replay is accepted and yields the `tasks` rows it holds: one holding an approved Ready task gains `admissible/` rows on replay, so its start finds the projections differ (`ambiguous_state`); one whose history records a result with no attempt while the node's attempt ran is refused on replay (`ErrReplayRefused`). Only such a store is replaced, by a fresh store, never rebuilt in place (Astra #333 L1). None is deployed: no wired `adcd` action writes leases or nodes before this stack. Obligation for P22a-6: a candidate's done only once the go of its latest round's review is selected (Opus #309 L2) | kept | — |
| consumption | `graph.Projector.Tasks` (`authority.Tasks`, wired in `adcd`): the fold refuses a `node.recorded` of a task that is not admitted, and a `node.claimed`, `node.reused`, `node.invalidated` or `node.renewed` of a node whose task is not admitted (`graph.ErrNotAdmitted`), on append and on replay; the lease fold refuses an attempt start the same (`graph.Units.Tasks`). The fixture `admission-held` is P22a-7's | kept | — |
| effect | n/a: admission is no external effect | n/a | — |

## Count

The counts by status are not kept in this file: run `make inventory-count`, which counts the tables above (`internal/inventory`). A figure kept here conflicted whenever two branches changed a row's status. Where the kept rows came from: P6, which also flipped P3's and P5's I1 and I2 rows (#226 entry 45); P7, P8a, P8b, P11, P11b, P20's `model_call` half, P21a and P21b, whose rows are kept in code, with the wiring left to P22a (P8a's I8 fold row is kept, since `adcd` wires `graph.Units`); P17a, whose four I16 rows are kept in code, inert until P22a. From now on, each row's own Item column records what changed it.
Every gap is closed by an item of `00-slice-plan.md` §5. None is left to a later milestone, except where 01 §4.1 or
§6's clarifications place it. The evidence rows of 12 follow #187's trigger (slice plan §4).
