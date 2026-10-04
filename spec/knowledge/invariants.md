# Invariants

Rules every implementation of the shared knowledge spec keeps. Each names the fixtures that exercise it
(see [`fixtures/`](fixtures/README.md)).

1. **One home per assertion.** Every claim kind has a single home (see the vocabulary). Other layers refer to it by
   id and never restate it.

2. **Supersede, never delete.** Records are append-only. A correction is a new record that supersedes the old one
   by an explicit link. Purging a payload leaves a tombstone; history and accounting are never purged.

3. **Data, never instructions.** Injected knowledge is wrapped as data. Content a model ingested from a repository
   cannot promote itself, and an untrusted note never reaches a role it was not written for.
   *Fixture:* `poisoned-builder-note`.

4. **Disclosure across projects is controlled (KA1).** No validator can prove that paraphrased prose is free of
   project information, so disclosure rests on lineage and templates, not on text inspection:
   - **Framework scope only.** A lesson shared across projects is about auto-dev-cycle's behaviour, roles, gates or
     harnesses, never about a project's code or domain.
   - **Two export paths.** *Automatic:* only allowlisted framework telemetry (failure-signature counts, reason
     codes, stage timings), rendered through fixed templates. *Prose:* any other project-derived text needs an
     explicit, digest-bound human export approval.
   - **Acceptance does not authorise export.** Accepting a claim (for example by independent review) never allows
     it to leave its project.
   - **Approval never overrides inherited restrictions.** An export approval adds permission to disclose the
     approved text; a delivery must still satisfy every restriction the text inherited.
   - **A mechanical filter is a first check, not proof.** Project paths, symbols, repository content and quoted
     prompts or tool output are rejected mechanically before any prose path.
   - **Opaque evidence pointers.** Evidence stays in its project; a pointer seen from another project exposes no
     metadata (no path, symbol, title, size or time of the evidence).

   *Fixtures:* `on-prem-lesson-to-open-project`, `opaque-evidence-pointer`, `automatic-versus-prose-export`.

5. **Identity fingerprints.** Fingerprints cover identity digests, so a change of revision, scope, lineage or
   restriction changes the fingerprint even when the bytes are identical. A view whose recomputed fingerprint
   differs from the recorded one is regenerated, never served.
   *Fixture:* `stale-view-after-finding-change`.

6. **Deduplication never merges restrictions.** Two refs with identical bytes and different lineage are two refs:
   the blob is shared, the identities and restrictions are not.
   *Fixture:* `same-bytes-different-lineage`.

7. **Restrictions are checked at every delivery.** At the initial delivery, at every lookup and at every fallback,
   both the restrictions each ref inherited and the receiving project's policy and destination are checked, live.
   Revocation between selection and delivery is honoured; a record already written is never rewritten, the
   delivery is refused instead.
   *Fixtures:* `on-prem-lesson-to-open-project`, `revocation-before-intent`, `revocation-after-intent`.

8. **Applicability is not validity.** A resolving citation is a mechanical prerequisite, not proof: the evidence
   must entail the claim for the claim's own subject, tree and environment.
   *Fixture:* `real-but-irrelevant-citation`.

9. **Coverage is derived from evidence.** The coverage status is computed from what the controller observed, by
   the rules in the vocabulary, never taken from an adapter's declaration. Unknown stays unknown; compaction and
   harness internals are unobservable.
   *Fixtures:* `compaction-turn`, `declared-only-coverage`.

10. **Cuts are named.** Every declared input of a manifest is either included or omitted with a reason. Nothing
    outside the declared inputs is supplied as controller context.

11. **Spec updates change no policy.** Adopting a new spec or fixture revision never alters grants, accepted claims
    or policy by itself.

12. **Continuity is scoped.** Ephemeral attempt continuity reaches only builder continuations of the same project,
    task and contract revision. Supervisor continuity is untrusted, scoped to the supervisor's grant and projects,
    and never auto-loaded into cycle roles. A record that continuity references is supplied only if the receiving
    seat is itself authorised for it.
    *Fixtures:* `poisoned-builder-note`, `mixed-project-handover`.

13. **Validity follows declared dependencies (K6).** A claim declares what it depends on. When a dependency
    changes, the claim becomes `stale` or `unknown` until it is revalidated; resolving or moving its anchor
    establishes where it points, not that it still holds.
    *Fixture:* `refactor-under-anchored-item`.
