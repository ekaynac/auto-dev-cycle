# 06 — Receipts and evidence

Every piece of evidence a gate, a review or an approval relies on is a receipt. `internal/receipt` is the only place
that decides whether a receipt is valid, complete and applicable. Implements M0 review [M8].

## Instrument contracts

An instrument (gate runner, integration plugin, deploy plugin, reviewer role, probe) is registered with a contract
before it can produce receipts:

```yaml
InstrumentContract:
  name: string
  version: string
  digest: sha256:…                  # hash of the code/config that produces receipts
  subject_type: tree | proposal | object_revision | environment | call
  required_predicates:              # the coverage a complete receipt must have
    - name: string
      evidence_required: [media_type]   # e.g. application/junit+xml
  optional_predicates: [name]
  preconditions: [name]             # environment checks before each run (12); failure = precondition_failed hold, not a spent attempt
  payload_schema: sha256:…|null     # blob digest of the JSON Schema for typed predicate fields (rule 7)
  artifact_classes: {name: diagnostic|restricted}
```

## Envelope

```yaml
Receipt:
  id: rcp_…
  kind: gate | integration | deploy | review | phase_gate | probe | dependency_prep | custom
  instrument: {name, version, digest}
  subject:                          # typed, discriminated by subject_type [M8]
    type: tree | proposal | object_revision | environment | call
    # tree:            {project, tree: git:…, head: git:…|null, environment: {identity, description}, contract_revision}
    # proposal:        {proposal: prp_…, binding_digest: sha256:…}
    # object_revision: {object: {type, id, version, digest}}
    # environment:     {identity: sha256:…, description}
    # call:            {call: cal_…}
    demand: {key: sha256:…, run: run_…}|null   # controller-bound evidence demand (12) and the run that produced it [PR5-2]
  provenance:                       # recorded by the controller, never by the producer [M8]
    command: cmd_…                  # the accepted command (03 `commands`)
    attempt: att_…|null
    calls: [cal_…]                  # model calls that contributed
    model_families: [family]        # derived from the calls' leased executors and verified routing
    contributors: [act_…]
  produced_at: time
  valid_until: time|null
  predicates:
    - name: string
      outcome: found|empty|could_not_run|failed
      reason: reason_code
      evidence: [{blob: sha256:…, media_type, artifact}]   # artifact: a name in the instrument's artifact_classes (rule 7)
      payload: {…}                  # typed facts, validated against the instrument's payload_schema (rule 7)
  outcome: found|empty|could_not_run|failed     # derived (below), never accepted from the producer
  reason: reason_code
  detail: string
  verdict: go|no_go|null            # review receipts only; derived (below)
  conclusions_digest: sha256:…      # derived; rule 2a
  supersedes: rcp_…|null
```

## Derivation (one function)

1. **Completeness first.** Every `required_predicate` of the instrument contract must be present and carry its
   required evidence media types, and every evidence blob must exist in the blob store. If not, the outcome is
   `could_not_run/receipt_incomplete`. An empty or partial predicate list can never derive `empty`.
2. **Then aggregation:** any `failed` → `failed`; else any `could_not_run` → `could_not_run`; else any `found` →
   `found`; else `empty`.
3. A producer's own outer status is kept as the predicate `reported_status`, never trusted alone (nöbetçi lab-deploy
   F1).
4. **Review verdict:** `go` only if the receipt's outcome is `empty` (no blocking finding) and every blocking claim
   predicate is `empty`. Otherwise `no_go`. The verdict is computed, never read from model text.

## Rules

1. **Evidence names its instrument.** A receipt from a different instrument digest does not satisfy a requirement that
   names an instrument. An instrument upgrade turns dependent requirements into `could_not_run/instrument_changed`,
   to be re-run, never silently orphaned.
2. **Applicability is separate from validity** (KA4). A valid receipt applies only to a decision about the same typed
   subject **and the same demand**. For tree subjects, tree, environment identity and contract revision must all
   match. For merge, the subject that counts is the merge queue's final tree (A5). When a requirement names a demand
   key, a receipt for another demand never applies, even with identical conclusions [PR5-2]. `internal/receipt` is the
   **only** applicability checker. The graph calls it whenever an input resolves (13) [D40].
2a. **Evidence equivalence is not identity** [PR5-5]. Fingerprints (10) stay exact: a new receipt is a new input. Whether
   a review must run again is a separate, semantic decision. Each receipt carries a `conclusions_digest` over its typed
   conclusions:
   - the demand key;
   - each predicate's outcome and reason;
   - per-step outcomes where the instrument declares steps;
   - the restrictions.

   A new receipt whose `conclusions_digest` equals the one the reviewer already saw does not by itself require a new
   review. A changed step outcome or changed restrictions do.
3. **Independence is mechanical.** An independent-review requirement is satisfied only if the receipt's
   `provenance.model_families` is disjoint from the families of the proposer and of every contributor to the subject.
   **Unknown family means not independent:** it blocks.
4. **Binding.** A confirmation receipt applies only to the proposal whose `binding_digest` it names. An unrelated GO
   never applies.
5. **Copy before teardown.** Artifacts are written to the blob store before the run directory is destroyed. Teardown
   refuses while any artifact named in the receipt is missing.
6. **Proof keyed by content.** Requirements reference receipts by typed subject + instrument digest. Identity never
   includes timestamps or live counters.
7. **Typed predicates; declared artifact classes** (nöbetçi Δ§8, refined by [PR5-6]).
   - Predicate fields are typed facts (exit codes, digests, byte counts, versions, counts), validated by a payload
     schema the instrument declares.
   - Artifacts carry a class declared by the instrument:
     - `diagnostic` (test output, tracebacks, build logs) may be delivered to builders and reviewers as usual;
     - `restricted` (output from live systems, production data, remote stderr) is never delivered into prompts or the
       journal and is kept only under its retention scope.

   Delivery restrictions are enforced at assembly (10) by artifact class.
8. **Missing evidence never passes.** A required receipt that is absent, stale, incomplete, inapplicable or
   `could_not_run` blocks the dependent decision.

## What M0 delivers

Instrument-contract and receipt schemas, the derivation function, applicability, independence and binding checks, and
the artifact-before-teardown interface (with a fake). Fixtures:
- an omitted required predicate;
- missing evidence media;
- a nested outer-ok/inner-stale lie;
- an instrument upgrade;
- a tree mismatch;
- an unknown reviewer family;
- a GO reused for an unrelated proposal.
