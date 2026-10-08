# 13 — The cycle as an artifact graph

Status: revision 2, 2026-10-03.
- Revision 1 was added by operator ruling D40: "we are in development, so instead of carrying lessons as rules, make
  architectural choices that remove their failure classes".
- Revision 2 applies Astra's review of revision 1 (`docs/reviews/2026-10-03-pr5-d40-astra.md`, cited `[G#]`). That
  review found several "by construction" claims overstated; they are corrected or narrowed here.
- 2026-10-05: the reviewer roster joins a review's identity (intake B1, `docs/research/2026-10-05-nobetci-phase1-close-intake.md`).

## Why

nöbetçi's cycle was a stage script with state in scratch files. Its recurring failures shared one shape: **a decision
was made from an input that was missing, stale, for another tree, or not part of the identity.** Examples:
- a ruling never reached the reviewer;
- a demanded walk did not re-key the review;
- a review was admitted with another tree's gate results;
- a receipt was reused across demands.

The graph makes each decision a node with **declared inputs**, so these become structural properties. Where structure
alone is not enough, this note says so and names the remaining check.

## Model

```yaml
Node:                              # one instance in a task's graph
  id: nod_…
  kind: plan | build | accept | gate | runtime_verify | integration | rebase | evidence | review | review_requirement | carry_over | closure | merge | deploy | record | phase_gate
  definition_version: string
  inputs: [InputSpec]              # declared, typed
  params: {…}
  requires: {role, capability, resources}   # leased via 05
  effect: none | effect_kind       # merge, deploy, notification (03)
  round: int                       # successor index, see "Successor rounds"

ComputationKey:                    # what to compute  [G1]
  key: sha256:…                    # = H(kind, definition_version, params digest, ordered input result ids, generation)
  generation: int                  # bumped only by an explicit, recorded invalidation

Result:                            # what was computed: immutable  [G1]
  id: res_…                        # identity of THIS output (content digest + attempt), never the computation key
  computation: sha256:…            # the ComputationKey it answers
  kind: plan | patch | partial_patch | tree | package | gate_result | review | evidence | carry_over_attestation | merge_result | review_requirement
  value: [Ref]
  conclusions: {typed projection}  # for reuse decisions only (see below); gate, evidence: the receipt's rule 2a document (06); review_requirement: the requirement
  outcome, reason
  completed: bool                  # did execution observably complete? (see "Facts, not claims")
```

- **Computation keys and result identities are different things** [G1].
  - A computation key names *what* would be computed. A result is *one* immutable output.
  - Downstream inputs and manifests reference **result ids**, so every manifest stays exact.
  - Each key has one **selected result** at a time. An explicit invalidation (an operator action, or a definition or
    instrument version change) bumps the generation, so the key changes, a new result is produced, and dependants
    re-key. No artifact is ever overwritten. This holds for the memoizable kinds (see "v1 scope"), whose key is claimed
    once per task. Instances of the other kinds can share a key (two builds, deploy A again) and select per node.
  - **Generations of the memoizable kinds are the task's.** For each identity (kind, definition version, params and
    inputs, without the generation) a task holds a current generation, 1 until an invalidation moves it. A claim at any
    other generation is refused as stale, and an invalidated key stays stale: no later claim names its old holder. The
    reuse binding (G4a) reuses only a result of the current generation's key. The other kinds keep their generation
    per node.
  - **A key is invalidated through its holder.** A node bound to a key by reuse (`node.reused`) shares the holder's key
    and result, so it is never invalidated on its own (`ErrReused`). The holder's invalidation releases every node
    bound to the old key in the same command, each with its own `node.reuse_released`: the release drops the node's
    reuse and result and returns it to waiting, so it reuses or waits again at the new generation. Nothing serves an
    invalidated result through a reused node. As a second guard, a memoizable producer whose key is not of its
    identity's current generation counts as not completed: the resolver waits on it and a claim refuses it.
    Dependants that already claimed the old result keep their claims until supersession (G6b).
  - **Renewal is the one exception to "a key selects once".** It applies only to a memoizable kind whose selected
    result carries a receipt. Once that receipt has passed its `valid_until`, by the event's time, `node.renewed`
    reopens the same key: the node is claimed again with no selected result, so a new attempt records and selects a
    fresher result. The renewal is a selection boundary: the node then selects only a result recorded after it, never
    the renewed result or an older one, and a result renews once. The new result's receipt must be produced at or
    after the renewal: recording and the resolver refuse an older receipt, still valid or not. The fold checks expiry against the event's time,
    never wall time, from the receipt validity the result bound when it was recorded (read from the receipt blob).
    The generation, the claim and its command stay, so counters do not reset, and the old selection stays in the log
    as history. A reused node is renewed through its holder, whose renewal releases the nodes bound to the key, as an
    invalidation does.
  - **What the fold cannot check.** Folds read no blobs, so the fold trusts a result's bound receipt validity. The
    resolver and the renewal builder re-read the blob and refuse a binding that differs, so a result with a forged
    binding never serves a dependant and is never renewed by the controller. Nor does a fold check a receipt's
    `produced_at` against the renewal; recording and the resolver do. As for an invalidation, the fold trusts the
    builder to emit a renewal's `node.reuse_released` events in the same command: a renewal without them leaves its
    reusers completed on the renewed result, serving nothing (the resolver refuses it) until the holder's
    invalidation releases them. An invalidation never silently ends a wait. A retry wait of the old key (a hold with a reason, raised for
    a `could_not_run` result) is retired in the same command by its own event, with the reason `invalidated`. Every other
    wait (awaiting an approval, an input or a budget raise) carries over to the new generation with its id, `since` and
    pages, and the node stays held: as in supersession, a hold is retired only when the node no longer has the wait.
- **The reviewer roster is part of a review's identity** (intake B1).
  - **In the key.** A review node's `params` carry its subject and its roster: for each member, the executor id and its
    model family.
    The family is derived from the controller's executor registry, never asserted by a caller (06 independence). The
    roster is in the params digest, so it is in the computation key, and a result produced by one roster can never
    satisfy another roster's key.
    - A review (or review-requirement) node is recorded and claimed only against the registry: each time, the
      controller refuses a roster whose families are not the ones the registry now derives. An executor the registry
      does not hold has an unknown family, and is never a member (06 rule 3: unknown is not independent). The claim
      refuses with `reviewer_not_independent`; the evaluator then holds that node on `reviewer_unavailable` (signal:
      the registry's probe) and goes on with its other goals.
    - The roster is sorted by executor, names each executor once, and has at least one member. The graph fold reads no
      registry: it checks only this shape and that no family is `unknown`. **Independence is enforced by the claim
      command** (ClaimReview), so a node recorded by a raw event, or with a family the registry no longer gives, is
      never claimed through it. A raw `node.claimed` event is outside this guarantee, as the fold reads no registry:
      M1's lease and dispatch step re-checks the registry before an attempt starts.
    - A successor carries its predecessor's params, roster included, and is not re-checked when recorded: it relies
      on the claim-time check.
  - **Placement is bound to it** (05). A review member's lease must place on exactly the pinned executor. A fallback
    that would change a member does not run under the old key: the replacement node is instantiated first, and the
    lease belongs to it.
  - **The required review is a graph input.** For each subject, the controller selects an immutable
    **review-requirement** result: `{subject, roster, definition_version, revision}`. A review node is made for the
    selected requirement. Base moves do not change it, so a valid carry-over survives them. Comparisons are on the
    requirement's revision (roster and definition), never on a rebased review's computation key.
    - The requirement is recorded by a `review_requirement` node: a controller node with no inputs, whose params are
      the requirement. Its result is completed and found, and its `conclusions` are exactly those params. The subject
      is a name the template gives (e.g. `candidate`), not a tree.
    - A review declares at most one `review_requirement` input. Its claim is refused unless the requirement's
      subject, roster and definition version are the review's own. In G6a the input is optional. Only a review is
      bound so: `carry_over` and `merge` consume the requirement under their own rules (G7b-2).
    - **G7b-2 obligations**, with the cycle template:
      - *mandatory requirement:* every review declares exactly one `review_requirement` input, enforced by the fold;
      - *subject binding:* the requirement is the one selected for the review's subject;
      - *commit-time re-validation:* the merge's commit re-validates against the subject's currently selected
        requirement revision.
    - G6b keeps **one selected revision per subject**: a roster change selects the next revision and supersedes the
      review of the old one.
  - **Replacement is a recorded supersession.** A roster change, decided by placement (05) or by an authority action
    (04), is **one writer transaction** (03). It selects the new requirement revision and records
    `review.superseded{predecessor, replacement, transferred_holds, retired_holds}`, together with the attempt
    transitions it needs:
    - **Attempts.** The predecessor's `running` attempts get `attempt.stop_requested{preempt}` (05). Attempts already
      `finishing`, `stopping`, `lost` or `sealed` keep their state and settle normally with their accounting. No
      predecessor result is ever selected for the replacement, not even one submitted just before. A submission that
      races the supersession is ordered by the writer transaction, and replay after a restart reaches the same
      outcome, never a second supersession. This graph supersession is distinct from 05's sealed-attempt
      supersession and its fencing-token allocation.
    - **Inputs and obligations.** The replacement keeps the predecessor's round and its declared inputs, unchanged, with
      the new roster. The lineage of owed inputs and demands persists, and cumulative rails carry over; nothing resets.
      Answers and evidence that become available later enter a declared successor review at round *n+1* (as for any
      review), never the replacement.
    - **Moved questions (G6b obligation).** When a question moved to a different owed input, the replacement contract
      must explicitly allow replacing or removing the superseded answer dependency. Transferring holds and preserving
      ancestry alone do not resolve it.
    - **Holds.**
      - A hold whose wait the replacement still has is **transferred**: a recorded reassociation that keeps its hold
        id, concrete signal, owner and paging history (02), so escalation does not restart.
      - A hold is **retired** only if the replacement's actual dependencies no longer include its wait (e.g. waiting for
        a removed member's executor), with the reason recorded.
      - Resource and settlement obligations stay on their owning leases. Internal artifact waits stay structural.
  - **Nothing reuses a review across rosters.** A review need not run because a prior review ran on a result that the
    same producer's new result is equivalent to only if both declare the same inputs; every input of the review
    resolves now as its own claim would (selected in its project, applicable and unexpired, at a writer
    transaction's time), the input under test to that new result; and the review's key, with that input at the
    prior's result, is the prior's claimed key at its generation: the same kind, definition version, params (so the
    same roster) and every other input. Carry-over in a merge-queue generation binds the
    requirement revision it was attested under, and the merge's commit-time re-validation (G7b-2) compares it with
    the currently selected revision.
- **Reuse decisions use typed conclusions.** For the declared edges below, a consumer may treat a new result as
  equivalent to one it already used if their `conclusions` projections are equal **and** the subject, demand,
  instrument digest and restrictions are identical, **and** the consumer's non-input identity (kind, definition
  version, params including a review's roster, generation) is identical: only the declared input edges may differ. Equivalence decides only whether to re-run. Manifests still list
  the exact result ids. The decision is the review's own: the review stands completed, at its identity's current
  generation, on a claim whose input resolved to the result it used, and the fresh result is its declared producer's
  selected result now. Both are completed and conclusive, and their whole receipts must have equal conclusions digests
  (rule 2a: the demand and each step's name, outcome and reason), subjects and instruments, and their receipt Refs equal
  project scopes and restrictions. Ref lineage is not compared: it records derivation, not what was concluded. Any
  other edge is an error, never "equivalent".

## What is cached, and what is retried [G2]

- **Only completed results are reusable.** A result is reusable if `completed: true` and its outcome is `found`,
  `empty`, or a conclusive `failed` (e.g. a gate that ran and failed).
- **`could_not_run` is an attempt record, not a cached result.** The node retries automatically when its signal fires
  (a precondition probe passes, a balance probe passes, a reset arrives). Retries are bounded by the rails and attempt
  caps (05, 12). Retrying never resets demand counters or charges.
- **Freshness.** A result with `valid_until` (e.g. an evidence receipt) is reusable only while fresh. Expiry makes it a
  missing input, and a renewal run is a new attempt under the same counters, on the same key ([G1], renewal).
- **A retry is not an invalidation.** A `could_not_run` result raises a retry wait on its signal (G3-3); the signal
  clears it, and the node, still claimed on the same key and generation, records its next attempt.

## Evaluation: demand-driven

The controller evaluates **goals** (e.g. "merge the candidate of task T", "close phase P"):
1. Resolve each node's inputs to selected, reusable, applicable results (06).
   - An input resolves to its producer's selected result; a producer not yet completed, or completed at a stale
     generation, is a structural wait. A producer of another project, or a result of another kind, is missing.
   - An answer input resolves to an approval at the owed input's declared revision, or is missing. The commit checks
     the approval again ([G4]).
   - Applicability is checked against a receipt: a gate or evidence result's `value` holds a `receipt` Ref whose content
     digest names the receipt blob, and the resolver checks it with 06's rules (`internal/receipt`). A receipt that does
     not apply, or has expired, makes the input missing, with the refusal's reason. The result holds exactly one
     receipt Ref, and the receipt carries the Ref's id and the result's conclusions: they are the receipt's rule 2a
     document (06), whose digest is its `conclusions_digest`. Otherwise the input is missing as `receipt_incomplete`.
   - What an input requires of its receipt (kind, instrument, subject, demand) is code of the consuming kind, per kind and
     `definition_version`, so the definition version keys it. Schemas carry no requirement. A gate or evidence input
     always has one; none is an error, never a skipped check.
2. Schedule producers of missing inputs first.
3. Run a node when all its inputs are present, by leasing what it requires (05).

- **Atomic producer claiming** [G10]. Claiming a runnable node instance is a writer-transaction step (03), so two goals
  needing the same node never run it twice. Duplicate demands join the existing claim.
  - A claim is a node instance's: a repeat demand of a claimed node joins its claim. Two instances never share one;
    build, plan and effect nodes are not reused (see "v1 scope" and [G4]).
  - For the memoizable kinds, a computation key is claimed once per task. Another instance whose key is claimed in its
    task is refused, records nothing and stays as it was. It never claims and never runs: it completes through a
    reuse binding, `node.reused`, which sets its key and result from the holder's selected result, of the same task,
    kind and key (the resolver's memoization, under applicability, 06). The holder must be completed. The task keeps
    the nodes bound to each key, and the holder's invalidation releases them ([G1]). No claim spans tasks or projects.
  - A command claims a given node, and a given memoizable key, at most once; the evaluator deduplicates its demands
    before it claims.
  - The fold re-derives a claim: one resolution per declared input, in order, and the key from them.
- **The controller context is the claimed inputs.** A node's controller context is rendered only from the inputs its
  claim fixed, in declared order: a result input gives its result's refs, an answer input the authority's Ref to the
  exact answer the claim fixed (an approval answers one owed input at one revision, and its artifact is immutable). An
  answer to an owed input the node did not declare never reaches it; the call's `manifest.intent` lists exactly these
  refs as its inputs (10).
- **Waiting is structural for internal inputs.** A node with a missing input is `waiting(input)`, and its signal is
  that input's result. External waits (approval, quota, human act, probe) use the hold registry (02).
  - The evaluator walks from its goals through each node's producers first, once per node, and claims a node once all
    its inputs resolve. A missing internal input records nothing. One evaluation claims a memoizable key once: another
    instance of a key it claimed waits, and every instance of a key whose holder is completed reuses its result. A
    demanded completed node whose selected receipt has passed the validity it bound is renewed through the holder of
    its key, and its dependants wait for the fresher result.
  - A missing answer raises `awaiting_input` only against the owed input the node declares: the hold's signal instance
    is `<owed input>@<revision>`, and its owner is the owed input's grant holder. Once the authority answers, the
    evaluation clears the hold on that signal and claims the node in the same command. A receipt owed input is not
    an answer: the node declares the evidence node's result, and the owed input names that evidence node (E1).
- **Inputs are resolved, and mutable eligibility is re-checked at commit** [G4]. A node's inputs are fixed when it is
  claimed. But approvals and grants can be revoked, receipts can expire and policy can tighten in the meantime. So
  authority (04) and applicability (06) are **re-validated in the writer transaction that commits the node's
  result**, and for effectful nodes before the effect starts. This is the explicit exception to "inputs only".
  - The commit resolves the node's inputs again at the transaction's time and requires each to resolve to what the
    claim fixed. An answer whose approval no longer answers its owed input refuses the commit as `authority_revoked`;
    a receipt that no longer applies, with the resolver's reason; a result no longer selected, as
    `stale_expected_state`. Nothing is recorded then.
  - The commit checks the claimed inputs in declared order, and writes its events itself in the committing transaction
    (`Tx.Emit`), handing none back; it is the only exported way to record a result. The result and selection folds
    refuse, from the log alone, a result whose claim's result inputs are no longer their producers' selected results at
    their current keys.
  - Until M1's `input.answer` records owed inputs and their answers (15 D1), the evaluator takes the authority's
    answers from its caller, which reads the committing transaction and alone decides who may answer (04); the
    owed-answer fake stands in (11). So no fold checks an approval yet: M1 must make the claim and commit folds check
    the recorded approvals.

## Successor rounds [G10]

The graph is append-only. Nothing modifies an existing node's inputs. New information creates **successor nodes**:
- **Repair:** round *n* is a new build node whose inputs include round *n−1*'s review findings and candidate tree.
- **Answers and new demands:** when a review records an owed input or an evidence demand, the answer or the demanded
  evidence is an input of the **successor review** (round *n+1*), never of the review that asked. No cycles.
- A node has at most one successor. It keeps its predecessor's kind, definition version, params, requirements and
  effect, and declares its predecessor's inputs followed by the new ones, so the new inputs re-key it; an inherited
  answer may move to a newer revision of the same owed input. The `node.recorded` fold checks this and refuses an answer
  input on a first round.
- An answer is accepted only for an owed input that the node's lineage (an ancestor, its `requested_by`) asked for, at
  that owed input's current revision: the successor refuses another, and the evaluator, the commit and the controller
  context refuse any answer input, inherited, recorded directly or claimed directly, whose owed input the lineage did
  not ask for. So a ruling for an input not owed is never delivered.
- An answer input at a revision its owed input has moved past is unanswered for its node. The evaluator leaves the
  node waiting, a structural wait that raises no hold and fails nothing else in the command; the commit refuses it as
  `authority_revoked` ([G4]), and so does its controller context. The lineage moves on through a successor that answers
  the current revision.
- Holds do not move to a successor until G6b transfers them. Until then a successor is refused while its predecessor
  holds anything but an `awaiting_input` wait on an answer input the successor inherits. In the same command, before
  the successor's `node.recorded`, a wait whose answer the authority has given is cleared on its signal, and a wait on
  a revision the successor moves to a newer one is retired as `revised`. The `node.recorded` fold refuses a successor
  of a node that holds anything. An inherited answer not given yet refuses the round, so no second hold restarts its
  paging.
- *As delivered (G5, #204), what G6b inherits:*
  - A replacement (a roster change, "Replacement is a recorded supersession" above) is not a successor. It needs its
    own event, and must maintain `graph_successors`: the one-successor rule and the hold check are keyed on the
    predecessor, so a replacement takes over its predecessor's successor slot rather than opening a second lineage.
  - The answer lineage is computed from `requested_by` through `predecessor` links. Either the lineage follows
    supersession (a replacement's ancestors include what it replaced), or the authority moves `requested_by` to the
    replacement in the same transaction; otherwise the replacement's successors cannot answer what the replaced
    review asked.
  - A question superseded by a *different* owed input (not a newer revision of the same one) is a replacement's: a
    successor cannot drop or rename an inherited answer, so a lineage whose inherited answer will never be given stays
    waiting until G6b replaces it.
  - The fold cannot check that a wait retired as `revised` was revised, nor that a cleared wait's answer was given,
    until M1 records owed inputs and approvals.

Rails (round caps, "same blocking findings three times") are cumulative across rounds.

## Facts, not claims [G3]

Results are computed by the controller from observed facts:
- **Build:** two observed facts, combined.
  - *Changes:* the controller's diff of the sandbox output tree against the base.
  - *Completion:* whether the executor reached a terminal result the controller accepted (05 attempt states) and
    reported it `found` or `empty`. A reported `could_not_run` or `failed` is not completion, nor is a refused or
    missing result or a lost attempt.

  | Completed | Diff | Result |
  |---|---|---|
  | yes | non-empty | `patch`, outcome `found` |
  | yes | empty | `empty` |
  | no | non-empty | `partial_patch` checkpoint (05 AttemptRecord), outcome `could_not_run`, reason from the exit map (e.g. `broker_credit_exhausted`) |
  | no | empty | `could_not_run`, reason from the exit map. **Never `empty`** |

  The adapter's exit status only fills the reason (02).
- **Gate:** the verdict comes from result files the controller collects and compares to the baseline (06).
- **Evidence:** from predicates evaluated against the instrument's payload schema (06, 12).
- **Usage:** gateway counts where possible; adapter reports otherwise, marked (05).

## What reaches a model [G7]

- **Guarantee:** the **controller-supplied context** of a model node (its prompt, and every tool result the controller
  mediates) is rendered **only** from the node's resolved inputs, and the ContextManifest (10) lists exactly those
  results.
- **Not guaranteed by structure:** harness-internal instructions, harness tools and compaction. Their coverage stays
  `partial` or `unknown` per adapter (09, 10). Host-side roles reduce them by confinement (plan §2.4).
- **Controller nodes:** purity (no reads outside declared inputs) is enforced by tests and review of node
  implementations, not by a function signature.

Consequences that hold:
- a ruling that answers an owed input reaches only the successor review that declared it;
- a ruling for an undeclared input cannot be in a review's controller-supplied context.

## Effects are operations, not cached state [G4]

- Effectful nodes (merge, deploy, notifications) run under 03's **durable effect identities**:
  - intent before egress;
  - reconciliation on restart;
  - a crash before result publication **resumes the existing effect**, never allocates another.
- **A cached effect result is history, not current state.** A new goal ("deploy A") always creates a new operation,
  which reconciles against the target's current state. Deploying A, then B, then requesting A again deploys A again.
  It does not hit the old result.
- **The keying rule** (G7a). An effect node's goal is one effect operation, a durable effect (03) whose request is
  the node instance, its claim's computation key and inputs, the target its params name, and the **want**: what the
  target is to hold. The want is content identity, the content digest of the one ref its one result input holds (a
  deploy's package bytes), never a digest of the controller's own records. A real target reports what it runs by
  the same digest. The request's digest identifies the operation.
  - The node instance is in the request, so the computation key alone never finds an operation. Deploy A and deploy
    A again share a computation key (effect nodes are not memoized, see "v1 scope"). Each goal is its own node
    instance, so its operation is new. An invalidation's next generation is a new key, so it is also a new
    operation.
  - Re-deriving the request after a crash finds the intended effect, and the operation resumes it: nothing intends a
    second effect for one request. Two recorded effects for one request are `could_not_run/ambiguous_state`.
  - **The request is kept** (03 "Kept requests"). Deploy reconciles from its request, so it declares `RequestBlob`:
    the caller publishes the request's RFC 8785 form before the operation is intended, and a reconciler works from
    the effect's `request_digest` alone.
  - **The node links to its effect.** The intent is recorded with `node.operated{node, key, effect}`, once per node
    and key. An effect node is not invalidated while that effect is pending or uncertain: the invalidation waits for
    it to settle, so no live effect is orphaned by a new key. An operation runs only its node's current request.
- **One live operation per target.** While an effect of a kind on a target is pending or uncertain, no other
  operation of that kind is intended there: the new goal waits, structurally, recording nothing, until the live one
  completes or is confirmed failed. So a stale operation, resumed after a newer goal was demanded, never lands over
  the newer one: the newer one is intended only after the stale one settles.
- **Re-validation before egress.** A new operation re-resolves its claim's inputs in the transaction that intends
  it, exactly as a commit does ([G4]), and is refused as the commit would be. A resume of an intended effect does
  not re-validate: its reconciliation must finish.
- **Reconciliation comes before egress.** A pending effect whose target already holds the want is observed without
  egress. Otherwise it is applied under its effect id by **compare-and-set**: the apply lands only while the target
  still holds the value just observed (the shape of G7b's expected-base merge). A target that moved in between, or
  an apply that fails, leaves the effect uncertain.
  - An uncertain effect (pending at a restart) completes if the target holds the want. Otherwise an idempotent kind
    is retried under the same id. Any other kind stays uncertain, recorded as `still_uncertain` once per change in
    what was observed. Settling such an effect (to `completed` or `failed_confirmed`) is M1's reconciler, with the
    effect runtime. Until then an uncertain non-idempotent effect pins its target (no other operation is intended
    there) and its node (which is not invalidated).
  - A `failed_confirmed` effect stays its node's outcome. A retry is a new operation, by an invalidation of the node.
  - One effect is run by one runner at a time. Concurrent runners of one pending effect are M1's, with the effect
    runtime.
- **Effect-node completion** (G7b). Once its operation's effect is `completed`, an effect node records and selects its
  result, and only through completion (a commit refuses an effect node): the one result kind its node kind produces
  (a merge's `merge_result`), `empty/completed_clean`, holding exactly the input ref whose content the effect applied
  (its request's want). Its inputs were re-validated when the operation was intended, so they are not re-validated
  again: the result folds check instead that the node's operation at its current key, for its current request, has
  a completed effect, and that the result holds what that effect applied (effect states are monotone, so a replay
  reads the same). A node whose effect completed is not invalidated before its result is recorded. A deploy has no
  result kind: it records no result, and no node takes a deploy's result (recording one that does is refused, in
  either order).

## Evidence targets and restore [G5]

Evidence targets are `evidence_target` resource groups, capacity 1 (05). Restore is encoded in the lease contract (05
"Evidence-target leases"):
- settlement requires a persisted `lease.restore_verified`;
- `restore_failed` keeps the target excluded across restarts, with an owner and a page;
- the **controller** publishes the run's accepted draft result at settlement;
- late executor submissions are refused by fencing.

Replacement nodes on the same target wait for the lease. The restore transitions are wired over capacity (05 "Wiring"):
a restore failure keeps the target excluded across a restart, and the run publishes nothing until a restore is
verified (contract fixture `restore-failure-keeps-target-excluded`).

## Merge-queue generations [G6]

The merge queue is serialized per project and base branch. Each attempt to merge a candidate result onto a base head is a **queue generation**:

| Node in the generation | Inputs | Result |
|---|---|---|
| `rebase` | candidate result, pinned base head | prospective tree (+ conflict flag) |
| `carry_over` | the reviewed candidate's review/closure results, the prospective tree, the selected review-requirement result | a **carry-over attestation** if the rebased change has the same patch identity, no conflict resolution, **and** the carried review was made under the selected requirement revision (06 applicability is satisfied by the attestation, which binds the old review to the new tree); otherwise a demand for a **delta review**, or for the replacement review after a roster change |
| `gate`, `runtime_verify`, required evidence | the prospective tree / package | refreshed receipts for the prospective tree |
| `merge` (effect) | all of the above + the expected base head | merge result, by expected-base compare-and-swap |

If the base moves before the merge commits, the CAS fails, and a **new generation** starts from the new base. The old
generation's results remain as history.

As delivered (G7b-1; the refreshed receipts and the merge are G7b-2):
- **Each attempt to merge a candidate result onto a base head is a generation.** Generations are numbered per task
  and branch, from 1. The `rebase` node pins its generation in its params `{branch, base, version, candidate,
  generation}`: the base head, the forge's version of the branch when that head was observed (it rises with every
  move of the head), and the candidate's selected result. The graph admits a rebase node only as the next
  generation, only for the candidate's currently selected result, only on an observation no older than the current
  generation's, and only for another base head or another candidate result: a base move, or a rebuilt candidate on
  the same base, starts a generation, and nothing else does. An observation older than the current generation's is
  refused, so a delayed caller cannot start a generation on a head the forge has already left; a genuine return to
  an earlier head is a newer observation, and starts one. The template records the next generation, its `rebase`
  and `carry_over` nodes, whenever the observation calls for it, mid-generation included.
- **The generation's candidate is pinned** through execution, not only when it is recorded: the claim of its `rebase`
  must resolve the candidate to `params.candidate`, the rebase's result is accepted only for that result, and its
  `carry_over` takes only that candidate. A candidate reselected after the generation was recorded is refused there
  (the claim, or the carry-over, fails), and its attempt is the next generation's.
- **The latest observation is kept** per task and branch, by `queue.observed/v1`, even when it starts no generation.
  An older version, or another head at the latest version, is refused as `failed/stale_expected_state`, so a
  delayed observation never moves the queue back.
  - **Where a version comes from.** A real forge adapter takes it from a monotonic, per-branch source the forge
    offers: a push-event sequence or a ref-log index. Where the forge offers none, the adapter keeps its own counter,
    persisted with it and raised whenever it observes a new head; that counter cannot see a move it did not observe
    (an unobserved A→B→A reads as A, unmoved), which is safe here, since the queue's merge is a compare-and-set on
    the head.
  - **Versions are trusted controller input.** The graph checks that versions rise; it does not authenticate them.
    A fabricated higher version is accepted, so a version is only as good as the adapter that reports it. The fake
    forge (`internal/testguard/fakes/forge`) keeps its counters in memory: they reset when it is recreated, so its
    monotonicity does not survive a recreation, which reads to the queue as the wedge below.
  - **A wedged queue.** If the source's versions go back (a forge restart that resets them, or a bogus far-future
    version), every later observation is refused, visibly, with that reason; nothing moves silently. Recovery is
    an operator's: re-basing the queue's observations is an M1 operator action, and in M0 a new task re-queues the
    candidate.
- **A rebase result** is a `tree` whose conflict status is its outcome, never the absence of a ref: a clean rebase
  (`empty/completed_clean`) holds the prospective tree (a git tree) and the rebased patch (of the candidate patch's
  ref type); one that found conflicts (`found`) holds their resolution too. Any other shape is refused when the
  result is recorded. **A patch's identity** is the content digest of its one ref (a build's `patch` holds one: its
  diff), so the rebased patch has the candidate's identity exactly when rebasing did not change the change.
- **The carry_over** is a controller node: it attests (`empty/completed_clean`, holding the prospective tree) only
  when the carried review's claim took the candidate's patch (the same content), the rebase is clean and the rebased
  patch has the candidate's identity. Otherwise it records `found/review_input_changed`: the demand for a delta
  review, which a successor review takes (G5). The decision is one function of what the claim fixed, applied by its
  builder and again by the result fold, which refuses any other outcome, reason or value; a commit refuses a
  carry_over. A carry_over is recorded only on its current generation's rebase, a rebase of its own candidate. Its
  inputs are declared by whoever records it and selection checks only the declared kind, so the fold also holds each
  input to its role: the candidate is a `patch` result, the rebase the required rebase node's `tree`, the review a
  review node's `review`; another result in a role is refused, whatever it took. The
  review's verdict, and the selected review-requirement with its roster, are consumed by G7b-2's merge, not here.
- **Trust.** The rebase result's prospective tree and rebased patch are trusted from the committer of the rebase
  result (the controller's own rebase, run where it leases its sandbox): the graph checks their shape and types and
  compares their identities, but does not recompute the rebase. Recomputing it, or attesting it by a receipt, is
  M1's with the sandbox runtime.

## Source tests, package, runtime verification [G8]

One tree cannot serve every purpose, so three are distinguished (07):
- **Source-test workspace:** the candidate with its development dependencies, where the project's test suite runs (the
  gate). Build-time facts apply. VCS metadata is present only if the profile declares tests need it.
- **Package artifact:** what the project's package recipe produces. **Its bytes are hashed, and exactly those bytes are
  deployed.**
- **Runtime verification:** declared runtime-facing checks run against the package artifact, in an environment with
  the target's **runtime facts** (no VCS metadata, network policy, interpreter version). This is where target-only
  failures (the nöbetçi `.git` class) surface before merge.

Declarations narrow the gap between environments; they do not guarantee parity. The ownership rules (07) still
classify genuine faults.

`graph.DeclaredFacts` derives a node's facts from the profile, never from the executor:
- **gate:** the source-test workspace, with development dependencies and VCS metadata as `source_tests` declares, and
  no network;
- **runtime_verify:** exactly one `package` input and no source input. It gets the package with the VCS metadata,
  network and runtime of the gate environment `runtime_verification.facts_from` names.

The fake sandbox (11) prepares exactly those facts (contract fixture `gate-and-runtime-see-their-facts`).

## Acceptance producers [G9]

- Each acceptance row binds to a **concrete producer instance**:
  - a builder test (by test id pattern);
  - an evidence node specification (instrument, subject, demand, grant);
  - a human act (actor + the completion artifact, an approval answering an owed input).
- The validator proves the wiring:
  - the producer exists;
  - it is reachable by the graph;
  - the role or instrument has the capability;
  - the human act names an actor with authority.

  It does **not** prove that a row's text is semantically satisfiable. A live action mislabelled as a builder test
  passes the type check. Semantic acceptance review stays with the planner contract checks and the reviewer.

## v1 scope [G10]

- A **fixed cycle template** instantiated per task from the approved plan. Node kinds are Go code. No workflow
  language.
- Memoization **only** for gate, runtime_verify, evidence, review and carry_over results. Build, plan and effect nodes
  are not reused across goals.
- Conclusions-only equivalence **only** on review←evidence and review←gate edges.
- The existing scheduler, lease, attempt and effect machines. No speculative execution, no remote cache.

## What M0 delivers

Schemas for Node, ComputationKey, Result, InputSpec and node state; key and generation computation; the resolver and
evaluator with atomic claiming and fake nodes; successor-round instantiation; the build outcome table; queue
generations as a template.

Fixtures, each tied to the class it addresses:
- an undeclared ruling never enters a review's controller context;
- an answer reaches only the successor review;
- equal conclusions reuse a review; a changed step outcome re-runs it;
- a roster change is one recorded supersession: the old roster's result never satisfies the replacement; a running
  attempt is stopped, and a finishing or sealed one settles without its result being selected; a submission racing
  the supersession and a restart each reach one outcome; an owed input still outstanding carries to the replacement
  with its hold (same id and paging history), while an answer arriving later enters the round *n+1* successor;
- equal gate or evidence conclusions never reuse a review across a roster change: the replacement runs;
- a roster change after carry-over was selected, but before the merge commits, makes the merge wait for the
  replacement review;
- a review cannot start with another tree's gate result;
- a 402 **before** the first edit is `could_not_run`, never `empty`;
- a 402 **after** edits is a sealed `partial_patch`;
- a `could_not_run` evidence result retries on its probe without operator invalidation;
- an expired receipt triggers renewal without resetting counters;
- deploy A, B, A deploys A again;
- a crash after effect intent resumes, never re-allocates;
- a revoked approval between claim and commit blocks the commit;
- restore failure keeps the target excluded across restart;
- a base move mid-generation starts a new generation;
- carry-over applies only with an equal patch identity;
- duplicate demands share one claimed node;
- a gate on the source-test workspace and runtime verification on the package artifact see their declared facts.
