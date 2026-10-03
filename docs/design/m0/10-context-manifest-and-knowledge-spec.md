# 10 — ContextManifest, refs, fingerprints, shared knowledge spec

Implements `03-knowledge-layer.md` (authoritative) for M0: schemas, invariants and fixture definitions only (K12,
KA8). M0 review [M11][M14] applied. Assembly behaviour arrives in M1.

## Refs and fingerprints [M14]

```yaml
Ref:
  type: blob | git_tree | git_path | receipt | finding | approval | contract | claim | view | attempt_record
  id: string                        # digest, git id + path, or controller id
  revision: string|null             # e.g. adopted contract revision
  scope: {project: prj_…|null, framework: bool}
  lineage: [Ref.id]                 # what it was derived from (controller-recorded)
  restrictions: [restriction]       # source restrictions inherited through lineage (KA1)
  content_digest: sha256:…          # digest of the bytes
  identity_digest: sha256:…         # JCS digest of {type, id, revision, scope, lineage, restrictions, content_digest}

ViewFingerprint:
  renderer: {name, version, config_digest}
  inputs: [Ref]                     # ordered: tree/candidate snapshot, receipts, findings/dispositions, adopted contract (KA4),
                                    # and for review packets: the reviewer's demands (owed inputs and evidence demand keys)
                                    # and the approvals that answered them (04, 12; nöbetçi Δ§1, Δ§3)
  digest: sha256:…                  # over renderer + ordered input identity_digests (not content digests)
```

Invariants:
- Fingerprints use **identity digests**, so a change of revision, scope, lineage or restriction changes the fingerprint
  even when the bytes are identical.
- **Content deduplication never merges restrictions.** Two refs with identical bytes but different lineage stay two
  refs; the blob is shared, the identities are not.
- A view is served only if its recomputed fingerprint matches. Otherwise it is regenerated. A fingerprint proves that
  the view matches its inputs, nothing more. Evidence applicability is checked separately (06).

## ContextManifest: three append-only events [M11]

A manifest is not one record written before egress with fields it cannot know yet. It is three events on the same
`cmf_…` id:

1. **`manifest.intent`**, committed **before egress**, as part of the call's effect intent (03):
   ```yaml
   id: cmf_…
   call: cal_…
   consumer: {role, executor: exe_…, model_snapshot, harness_version, capability_profile}
   task: tsk_…|null
   attempt: att_…|null
   inputs: [Ref]
   policy: {usage_profile, data_policy_digest, renderer_versions}
   included: [{ref: Ref, order, bytes, purpose}]
   omitted:  [{ref: Ref, reason: budget|policy|unavailable|restricted}]
   budget: {tokens, bytes}
   rendered_digest: sha256:…
   destination: destination_id
   routing: verified|reported
   ```
2. **`manifest.delivery`**, appended per turn as the adapter streams:
   - controller-mediated tool results with the exact handles the core supplied;
   - adapter coverage declarations;
   - compaction marks.

   Every delivered ref passes the restriction check again before it is supplied (KA1).
3. **`manifest.finalized`**, on call end:
   ```yaml
   coverage:
     assembled: complete
     delivered_refs: [Ref.identity_digest]
     declared_only: [{turn, what}]            # adapter-declared, unverified
     unobservable: [{turn|null, what: harness_instructions|compaction|harness_tool|other}]
     status: complete | partial | unknown     # derived: complete only if every turn is evidenced and nothing is unobservable
   delivery: {outcome, reason}
   ```

Invariants:
- Cuts are named in `omitted`, never silent.
- `status` is derived from evidence, never from declarations alone. Unknown stays unknown (E0).
- Restricted refs are checked at intent and at every delivery (lookup, fallback), against both their inherited
  restrictions and the receiving project's policy and destination.

## Shared knowledge spec draft (`spec/knowledge/`, K10)

M0 writes:
1. `vocabulary.md`: information classes, claim kinds, lifecycle dimensions, ref types, identity vs content digests,
   restriction semantics.
2. `invariants.md`:
   - one home per assertion;
   - supersede, never delete;
   - data, never instructions;
   - acceptance does not authorise export;
   - identity fingerprints;
   - dedup never merges restrictions;
   - applicability ≠ validity;
   - coverage derived from evidence.
3. `schemas/`: Ref, ViewFingerprint, the three manifest events, AttemptRecord (shared with 05), FailureSignature, Claim
   (draft, unused until K3).
4. `fixtures/`: **definitions** (inputs + expected outcome), including:
   - a stale view after a finding changes at the same tree;
   - identical bytes with different lineage;
   - an on-prem-derived lesson offered to an open project;
   - revocation between selection and delivery;
   - a poisoned builder note offered to a reviewer;
   - a real-but-irrelevant citation;
   - a compaction turn;
   - a declared-only coverage claim;
   - a mixed-project supervisor handover;
   - a refactor under an anchored item.

   Each fixture names the milestone that implements its check (M1, M2 or M4).

The spec carries its own semver. InfraMedic vendors a pinned revision. Spec or fixture updates never change grants,
accepted claims or policy automatically.
