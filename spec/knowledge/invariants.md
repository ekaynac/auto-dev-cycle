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

4. **Acceptance does not authorise export.** Accepting a claim (for example by independent review) never allows it
   to leave its project. Project-derived prose needs an explicit, digest-bound human export approval.
   *Fixtures:* `on-prem-lesson-to-open-project`, `mixed-project-handover`.

5. **Identity fingerprints.** Fingerprints cover identity digests, so a change of revision, scope, lineage or
   restriction changes the fingerprint even when the bytes are identical. A view whose recomputed fingerprint
   differs from the recorded one is regenerated, never served.
   *Fixture:* `stale-view-after-finding-change`.

6. **Deduplication never merges restrictions.** Two refs with identical bytes and different lineage are two refs:
   the blob is shared, the identities and restrictions are not.
   *Fixture:* `same-bytes-different-lineage`.

7. **Restrictions are checked at every delivery.** At the initial delivery, at every lookup and at every fallback,
   both the restrictions each ref inherited and the receiving project's policy and destination are checked.
   Revocation between selection and delivery is honoured.
   *Fixtures:* `on-prem-lesson-to-open-project`, `revocation-before-delivery`.

8. **Applicability is not validity.** A resolving citation is a mechanical prerequisite, not proof: evidence must
   entail the claim, and evidence that is valid elsewhere may not apply to the consumer's tree and environment.
   *Fixtures:* `real-but-irrelevant-citation`, `refactor-under-anchored-item`.

9. **Coverage is derived from evidence.** The coverage status is computed from what the controller observed, never
   taken from an adapter's declaration. Unknown stays unknown; compaction and harness internals are unobservable.
   *Fixtures:* `compaction-turn`, `declared-only-coverage`.

10. **Cuts are named.** Every declared input of a manifest is either included or omitted with a reason. Nothing
    outside the declared inputs is supplied as controller context.

11. **Spec updates change no policy.** Adopting a new spec or fixture revision never alters grants, accepted claims
    or policy by itself.
