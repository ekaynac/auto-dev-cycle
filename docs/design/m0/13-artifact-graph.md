# 13 — The cycle as an artifact graph

Status: added 2026-10-03 by operator ruling D40: "we are in development, so instead of carrying lessons as rules, make
architectural choices that remove their failure classes". This note replaces the stage-script model of the cycle
(plan §2.5) with a **graph of nodes over content-addressed artifacts**. Several rules elsewhere become consequences of
this structure. Each such place cites `[D40]`.

## Why

nöbetçi's cycle was a stage script with state in scratch files. Its recurring failures had one shape: **a decision was
made from an input that was missing, stale, for another tree, or silently not part of the identity.** Examples:
- a ruling that never reached the reviewer;
- a demanded walk that did not re-key the review;
- a review admitted with another tree's gate results;
- a receipt reused across demands.

Each was patched with a rule. In a graph where every decision is a node whose output is keyed by its exact inputs,
these become impossible by construction, not prevented by care.

## Model

```yaml
Artifact:                          # immutable, content-addressed
  key: sha256:…                    # = H(node kind, node definition version, params digest, ordered input identities)
  kind: plan | patch | tree | accept_record | gate_result | review | evidence | closure | merge_result | deploy_result | record | approval_answer | …
  value: [Ref]                     # receipts, blobs, records (06, 10)
  conclusions_digest: sha256:…     # semantic summary of the value (06 rule 2a); downstream keys use this
  outcome, reason                  # four outcomes (02)

Node:
  id: nod_…
  kind: build | accept | gate | integration | evidence | review | closure | merge | deploy | record | phase_gate | plan
  definition_version: string       # the Go implementation's version; changing it changes every key it produces
  inputs: [InputSpec]              # declared, typed; nothing else can influence the node
  params: {…}                      # e.g. the task contract revision, the role requirements
  requires: {role, capability, resources}   # what the scheduler must lease (05) to run it
  effect: none | effect_kind       # merge, deploy and notifications are effectful (03)
```

- **Inputs are the only influence.** A node implementation receives exactly its resolved inputs. It cannot read state
  outside them: the event store, files or "the latest X". Lint and a sandboxed node runner enforce this for model nodes;
  for controller nodes, the input set is the function signature.
- **Identity of an input** is the input artifact's **key**, except where the consuming node declares that it depends on
  conclusions only. Review nodes key on the `conclusions_digest` of evidence inputs, so the same conclusions mean the
  same key and no re-review, while a changed step outcome or restriction changes the key (nöbetçi Δ§3 by construction).
- **A model node is not pure, but it is memoized.** If an artifact for its key exists, it is reused and the node does
  not re-run. This is exactly "proof keyed by content" (nöbetçi PLANNING_CANON §4c). Re-running with identical inputs
  happens only by explicit invalidation:
  - an operator action;
  - a node definition or instrument version change, which changes the keys.

## Evaluation: demand-driven

The controller evaluates **goals** (e.g. "merge the candidate of task T", "close phase P"):
1. Resolve the goal node's inputs recursively.
2. Reuse an input whose artifact exists and is applicable (06).
3. Schedule the **producer** of a missing input first.
4. Run a node when all its inputs are present, by leasing (05) what it `requires`.

- **Waiting is structural.** A node with a missing input is `waiting(input)`. Its resume signal is that input's
  artifact. External waits (an approval, a quota reset, a human act) are inputs too: an `approval_answer` artifact, a
  `capacity` signal. So **every hold has a signal by construction**, and the hold registry (02) names the external
  ones.
- **No inapplicable input can reach a node.** Applicability (06) is checked when an input resolves. An inapplicable
  artifact is a missing input. This replaces the `inputs_applicable` placement check (nöbetçi Δ§13 by construction).

## Rounds without cycles

Repair loops are **unrolled**. Round *n* is a new build node whose inputs include round *n−1*'s review findings and
candidate tree. The graph stays acyclic. Round counts are rails (plan §6); the "same blocking findings three times"
rule compares artifacts across rounds.

## What reaches a model: only the manifest

For every model node, the controller renders its prompt **only** from its resolved inputs, and writes the
`ContextManifest` (10) listing exactly those refs. Consequences:
- **A review sees what its inputs are, nothing else.** An operator ruling that answers a reviewer's owed input is an
  `approval_answer` artifact. It is an input of the next review node because the review declared that owed input. A
  ruling for an input the review did not declare cannot reach it (nöbetçi Δ§1 by construction; no separate collector).
- **A new demand re-keys the review** because the demanded evidence is a declared input whose conclusions changed.

## Facts, not claims

Node outcomes are computed by the controller from **observed facts**, never from what an executor reports:
- **Build:** the controller computes the diff between the sandbox's output tree and the base. An empty diff is `empty`;
  a non-empty diff is a `patch` artifact (sealed as a checkpoint if the attempt ended early). The adapter's exit
  status only explains *why* a run ended (02 exit map, now a fallback for the reason field). A 402 mid-run with edits is
  a sealed patch plus `broker_credit_exhausted`, never "no changes" (nöbetçi Δ§14 by construction).
- **Gate:** the verdict comes from result files the controller collects and compares to the baseline (06), never from
  printed counts.
- **Evidence:** from predicates the controller evaluates against the instrument's payload schema (06).
- **Usage:** from the gateway's counts where it sees the traffic; adapter reports where it cannot, marked as such (05).

## Evidence targets are leased resources

An evidence node's external target (an environment, a host, a deployment slot) is a **resource group of kind
`evidence_target` with capacity 1** (05). Its release rule is `on_restore_verified`:
- while a run holds or has not released the target, no other evidence node, for any job, demand or instrument, can
  lease it;
- the run's output artifact is **published at lease settlement**, which requires verified restore/cleanup. A crash
  between collection and cleanup leaves the lease unsettled and nothing published;
- late results are refused by the attempt's fencing token (05).

This is the existing lease/attempt/fencing machinery. Note 12 now only adds the evidence-specific parts (preconditions,
attempt keys by demand, content classes, remote processes).

## Test what you ship

The gate node's input tree is the **ship tree**: what the project's package recipe produces for deployment (07
`package`). By default that is the candidate exported without VCS metadata, plus the declared build steps. Tests that
need VCS metadata, network or other environment features must declare them as facts. The gate then runs with the same
facts as the target environment, so target-only failures surface in the gate (nöbetçi Δ§7 class). The gate-output
ownership rules (07) remain for classifying genuine faults, with far fewer cases reaching them.

## Typed acceptance producers

Each acceptance row in the plan schema declares its **evidence producer**: `builder_test` | `evidence:<instrument>` |
`human_act:<actor>`. The planning head's validator checks that a builder task's rows are producible by the builder or
name the producing evidence node; the graph wires those evidence nodes in as inputs of the phase gate (nöbetçi Δ§9 by
construction).

## Engine scope (kept small)

- Node kinds are Go code, not a user-defined workflow language. The per-task graph is instantiated from a fixed cycle
  template plus the approved plan.
- Storage: nodes, artifacts and node state are events and projections in the event store (03).
- Scheduling: the capacity scheduler (05) leases resources for runnable nodes. Independent nodes run in parallel,
  bounded by leases and owned-path overlap.
- No speculative execution or remote caching in v1.

## What M0 delivers

Schemas for Node, Artifact, InputSpec and node state; key computation (including conclusions-only inputs); the
resolver and evaluator with a fake node set; the waiting → signal mapping; unrolled rounds; ship-tree and
acceptance-producer schema fields (07, plan).

Contract fixtures that prove each nöbetçi class **cannot occur**:
- an undeclared ruling never reaches a review;
- a declared answer re-keys the review;
- identical conclusions reuse the review;
- a changed step outcome re-runs it;
- a review cannot start with another tree's gate result;
- a 402 mid-build with edits yields a sealed patch;
- two evidence nodes cannot hold one target;
- nothing is published before restore;
- a builder-unproducible row is rejected at plan validation;
- a gate on the ship tree sees no `.git`.
