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
  kind: plan | build | accept | gate | runtime_verify | integration | evidence | review | carry_over | closure | merge | deploy | record | phase_gate
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
  kind: plan | patch | partial_patch | tree | package | gate_result | review | evidence | carry_over_attestation | merge_result | …
  value: [Ref]
  conclusions: {typed projection}  # for reuse decisions only (see below)
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
- **The reviewer roster is part of a review's identity** (intake B1).
  - **In the key.** A review node's `params` carry its roster: for each member, the executor id and its model family.
    The family is derived from the controller's executor registry, never asserted by a caller (06 independence). The
    roster is in the params digest, so it is in the computation key, and a result produced by one roster can never
    satisfy another roster's key.
  - **Placement is bound to it** (05). A review member's lease must place on exactly the pinned executor. A fallback
    that would change a member does not run under the old key: the replacement node is instantiated first, and the
    lease belongs to it.
  - **The required review is a graph input.** For each subject, the controller selects an immutable
    **review-requirement** result: `{subject, roster, definition_version, revision}`. A review node is made for the
    selected requirement. Base moves do not change it, so a valid carry-over survives them. Comparisons are on the
    requirement's revision (roster and definition), never on a rebased review's computation key.
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
    - **Holds.**
      - A hold whose wait the replacement still has is **transferred**: a recorded reassociation that keeps its hold
        id, concrete signal, owner and paging history (02), so escalation does not restart.
      - A hold is **retired** only if the replacement's actual dependencies no longer include its wait (e.g. waiting for
        a removed member's executor), with the reason recorded.
      - Resource and settlement obligations stay on their owning leases. Internal artifact waits stay structural.
  - **Nothing reuses a review across rosters.** The reuse rule below compares only reviews with an identical non-input
    identity. Carry-over in a merge-queue generation binds the requirement revision it was attested under, and the
    merge's commit-time re-validation ([G4]) compares it with the currently selected revision.
- **Reuse decisions use typed conclusions.** For the declared edges below, a consumer may treat a new result as
  equivalent to one it already used if their `conclusions` projections are equal **and** the subject, demand,
  instrument digest and restrictions are identical, **and** the consumer's non-input identity (kind, definition
  version, params including a review's roster, generation) is identical: only the declared input edges may differ. Equivalence decides only whether to re-run. Manifests still list
  the exact result ids.

## What is cached, and what is retried [G2]

- **Only completed results are reusable.** A result is reusable if `completed: true` and its outcome is `found`,
  `empty`, or a conclusive `failed` (e.g. a gate that ran and failed).
- **`could_not_run` is an attempt record, not a cached result.** The node retries automatically when its signal fires
  (a precondition probe passes, a balance probe passes, a reset arrives). Retries are bounded by the rails and attempt
  caps (05, 12). Retrying never resets demand counters or charges.
- **Freshness.** A result with `valid_until` (e.g. an evidence receipt) is reusable only while fresh. Expiry makes it a
  missing input, and a renewal run is a new attempt under the same counters.

## Evaluation: demand-driven

The controller evaluates **goals** (e.g. "merge the candidate of task T", "close phase P"):
1. Resolve each node's inputs to selected, reusable, applicable results (06).
2. Schedule producers of missing inputs first.
3. Run a node when all its inputs are present, by leasing what it requires (05).

- **Atomic producer claiming** [G10]. Claiming a runnable node instance is a writer-transaction step (03), so two goals
  needing the same node never run it twice. Duplicate demands join the existing claim.
  - A claim is a node instance's: a repeat demand of a claimed node joins its claim. Two instances never share one;
    build, plan and effect nodes are not reused (see "v1 scope" and [G4]).
  - For the memoizable kinds, a computation key is claimed once per task. Another instance whose key is claimed in its
    task is refused, records nothing and stays as it was. It never claims and never runs: it completes through a
    reuse binding, `node.reused`, which sets its key and result from the holder's selected result, of the same task,
    kind and key (the resolver's memoization, under applicability, 06). No claim spans tasks or projects.
  - A command claims a given node, and a given memoizable key, at most once; the evaluator deduplicates its demands
    before it claims.
  - The fold re-derives a claim: one resolution per declared input, in order, and the key from them.
- **Waiting is structural for internal inputs.** A node with a missing input is `waiting(input)`, and its signal is
  that input's result. External waits (approval, quota, human act, probe) use the hold registry (02).
- **Inputs are resolved, and mutable eligibility is re-checked at commit** [G4]. A node's inputs are fixed when it is
  claimed. But approvals and grants can be revoked, receipts can expire and policy can tighten in the meantime. So
  authority (04) and applicability (06) are **re-validated in the writer transaction that commits the node's
  result**, and for effectful nodes before the effect starts. This is the explicit exception to "inputs only".

## Successor rounds [G10]

The graph is append-only. Nothing modifies an existing node's inputs. New information creates **successor nodes**:
- **Repair:** round *n* is a new build node whose inputs include round *n−1*'s review findings and candidate tree.
- **Answers and new demands:** when a review records an owed input or an evidence demand, the answer or the demanded
  evidence is an input of the **successor review** (round *n+1*), never of the review that asked. No cycles.

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

## Evidence targets and restore [G5]

Evidence targets are `evidence_target` resource groups, capacity 1 (05). Restore is encoded in the lease contract (05
"Evidence-target leases"):
- settlement requires a persisted `lease.restore_verified`;
- `restore_failed` keeps the target excluded across restarts, with an owner and a page;
- the **controller** publishes the run's accepted draft result at settlement;
- late executor submissions are refused by fencing.

Replacement nodes on the same target wait for the lease.

## Merge-queue generations [G6]

The merge queue is serialized per project and base branch. Each attempt to merge a candidate is a **queue generation**:

| Node in the generation | Inputs | Result |
|---|---|---|
| `rebase` | candidate result, pinned base head | prospective tree (+ conflict flag) |
| `carry_over` | the reviewed candidate's review/closure results, the prospective tree, the selected review-requirement result | a **carry-over attestation** if the rebased change has the same patch identity, no conflict resolution, **and** the carried review was made under the selected requirement revision (06 applicability is satisfied by the attestation, which binds the old review to the new tree); otherwise a demand for a **delta review**, or for the replacement review after a roster change |
| `gate`, `runtime_verify`, required evidence | the prospective tree / package | refreshed receipts for the prospective tree |
| `merge` (effect) | all of the above + the expected base head | merge result, by expected-base compare-and-swap |

If the base moves before the merge commits, the CAS fails, and a **new generation** starts from the new base. The old
generation's results remain as history.

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
