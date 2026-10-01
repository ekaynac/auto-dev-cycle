**GO with changes — resolve the High findings before starting M0 implementation.**

1. **High — Validation is outside the serialization boundary.**  
   **Location:** [03-event-store.md, Rules / Idempotency](/home/mega/auto-dev-cycle/docs/design/m0/03-event-store.md:35).  
   Two commands can validate against the same state before the writer commits either. One goroutine also does not exclude a second daemon. **Change:** perform state-dependent authorization, expected-state checks, reservations, idempotency lookup, event append and projection updates inside one writer transaction. Persist the command digest/result; reject key reuse with different input. Specify process-level exclusivity. Test concurrent approvals and reservations.

2. **High — Intent-before-egress does not define crash recovery.**  
   **Location:** [03-event-store.md, Rules 1–2](/home/mega/auto-dev-cycle/docs/design/m0/03-event-store.md:35).  
   A crash after intent commit, or after an external effect but before recording its result, leaves replay behavior undefined. Retrying can duplicate billing/delivery; returning the original command result can lose unfinished work. **Change:** define durable effect identities and pending/uncertain/completed dispositions. Specify reconciliation versus retry per effect, durable blob publication before referencing commits, and crash fixtures at each boundary. Runtime recovery may remain M1.

3. **High — Sensitive authority depends on the command’s name.**  
   **Location:** [04-authority.md, Action registry](/home/mega/auto-dev-cycle/docs/design/m0/04-authority.md:43); [07-project-profile.md, Location and authority](/home/mega/auto-dev-cycle/docs/design/m0/07-project-profile.md:6).  
   Generic profile adoption gets ordinary approval semantics, although its changes can widen destinations or weaken protections. The default-`none` rule also omits KA1’s human export approval and K5’s knowledge acceptance policies. **Change:** authority must classify the effective change regardless of endpoint. Register purpose-bound knowledge acceptance/export actions, and explicitly prohibit protected-document approvals from widening authority or data policy.

4. **High — Confirmation lacks an executable binding contract.**  
   **Location:** [04-authority.md, Proposals and confirmation](/home/mega/auto-dev-cycle/docs/design/m0/04-authority.md:94).  
   “Proposal digest” is undefined; only object movement invalidates confirmation. Grant revocation, policy tightening and replay remain unspecified. A pager code alone does not establish an authorized operator. **Change:** bind a fresh, expiring, single-use challenge to the immutable action, object, scope, expected state and confirming actor. Revalidate current authority when applying. Define isolated pager delivery, authenticated reply handling and attempt limits; require WebAuthn origin/RP/challenge/signature/presence verification. [WebAuthn verification requirements](https://www.w3.org/TR/webauthn-3/#sctn-verifying-assertion).

5. **High — Supervisor isolation does not cover reads and bootstrap secrets.**  
   **Location:** [04-authority.md, Actors and tokens](/home/mega/auto-dev-cycle/docs/design/m0/04-authority.md:18); [08-controller-api-and-cli.md, Resources/events](/home/mega/auto-dev-cycle/docs/design/m0/08-controller-api-and-cli.md:15).  
   Bootstrap prints an operator-creation credential into logs. Read APIs and SSE have filters but no explicit grant/project authorization, including revocation during a stream. **Change:** keep bootstrap secrets in an owner-only channel, persist one-shot consumption across restarts, and redact credentials/confirmation codes. Enforce server-side read scope on resources, blobs, notes and events; recheck long-lived streams after revocation.

6. **High — Fencing does not invalidate revoked or interrupted work.**  
   **Location:** [05-leases-attempts-fencing.md, Attempt and fencing](/home/mega/auto-dev-cycle/docs/design/m0/05-leases-attempts-fencing.md:55).  
   Rejecting only *lower* tokens leaves the current token usable after revocation, expiry or interruption until replacement starts. Future tokens are also unspecified. **Change:** accept only the exact controller-issued attempt/token pair in an admissible state; invalidate publication authority atomically on stop/revoke/restart. Make supersession and replacement allocation atomic. Complete the attempt transition table, including checkpoint sealing and immutable terminal states.

7. **High — Settlement and adapter telemetry contradict each other.**  
   **Location:** [05-leases-attempts-fencing.md, Lease / Usage](/home/mega/auto-dev-cycle/docs/design/m0/05-leases-attempts-fencing.md:40); [09-executor-adapter-contract.md, Notifications](/home/mega/auto-dev-cycle/docs/design/m0/09-executor-adapter-contract.md:34).  
   Note 09 drops stale notifications; note 05 requires their usage to settle. Streaming usage and `usage_final` lack call identities and deduplication semantics. “Every lease settles” does not explain indefinitely uncertain billing or reservation release. **Change:** fence outputs separately from accounting. Define call/measurement IDs, delta-versus-final reconciliation, retained uncertain liabilities, resource-specific release rules, and all expiry/waiting transitions. Test late and duplicate usage without double charging.

8. **High — Receipts cannot yet establish complete, independent proof.**  
   **Location:** [06-receipts.md, Envelope / Rules / Review receipts](/home/mega/auto-dev-cycle/docs/design/m0/06-receipts.md:8).  
   Empty or incomplete predicate lists derive `empty`; no instrument contract declares required predicates. Review-family provenance is supposedly in `produced_by`, but that field is merely an actor ID. A tree-only subject also cannot bind confirmation to an exact proposal. **Change:** require instrument-defined predicate coverage and evidence completeness before aggregation. Add typed subjects and controller-recorded call/attempt/model-family provenance; derive GO mechanically and bind it to the reviewed action/object. Unknown independence must block. Test omitted predicates and unrelated GO reuse.

9. **High — Blob references have no safe ingestion boundary.**  
   **Location:** [09-executor-adapter-contract.md, Process model](/home/mega/auto-dev-cycle/docs/design/m0/09-executor-adapter-contract.md:6).  
   Naming directories “read-only” and “write-only” does not define safe controller reads of adapter-created files. Symlinks, special files, replacement races and cross-attempt references are unspecified. **Change:** define attempt-scoped handles, confinement, bounded regular-file ingestion, safe path resolution, digest verification and copying into controller-owned immutable storage. Reject digest mismatch, symlink escape and concurrent replacement in conformance fixtures.

10. **High — The test guard does not provide its claimed containment.**  
    **Location:** [11-testing.md, Test write-guard](/home/mega/auto-dev-cycle/docs/design/m0/11-testing.md:5).  
    Constructor/spawner wrappers do not intercept direct filesystem calls or descendants writing absolute paths. Changing `HOME` does not make credentials unreachable. **Change:** enforce containment for the complete test process tree, with sanitized credentials/network and explicit read/write boundaries. Exercise direct Go/TS writes, subprocesses and symlinks. Use sacrificial outside-boundary sentinels for red tests; a broken guard must not damage the real home directory.

11. **Medium — Adapter messages cannot substantiate ContextManifest coverage.**  
    **Location:** [09-executor-adapter-contract.md, Methods / Notifications](/home/mega/auto-dev-cycle/docs/design/m0/09-executor-adapter-contract.md:15); [10-context-manifest-and-knowledge-spec.md, ContextManifest](/home/mega/auto-dev-cycle/docs/design/m0/10-context-manifest-and-knowledge-spec.md:29); [AUTHORING.md §1](/home/mega/auto-dev-cycle/docs/adapters/AUTHORING.md:13).  
    Tool-result byte counts cannot populate delivered-content references; per-turn coverage declarations and controller-mediated tool requests have no messages. Pre-egress manifests already require a delivery outcome. Native-login access is also absent from `prepare`. **Change:** define call IDs, tool request/response and delivery-reference messages, append-only intent/delivery/finalization events, and narrowly scoped native-login access. Derive coverage from evidence; declarations alone cannot establish completeness.

12. **Medium — Integration topology is neither pinned nor consistently editable.**  
    **Location:** [07-project-profile.md, Schema](/home/mega/auto-dev-cycle/docs/design/m0/07-project-profile.md:40); [04-authority.md, Protected-document operation](/home/mega/auto-dev-cycle/docs/design/m0/04-authority.md:118).  
    `topology_ref: path` omits the adopted content identity. All protected paths supposedly use the document operation, but that operation accepts only the decision/convention subset. **Change:** pin topology to an approved revision/digest and define its reviewed update/adoption path without widening the knowledge-document exception.

13. **Medium — Four outcomes and exact approval semantics drift at the API boundary.**  
    **Location:** [08-controller-api-and-cli.md, Command style / CLI rules](/home/mega/auto-dev-cycle/docs/design/m0/08-controller-api-and-cli.md:19); [09-executor-adapter-contract.md, Methods](/home/mega/auto-dev-cycle/docs/design/m0/09-executor-adapter-contract.md:17).  
    Applied/proposed responses and cancellation omit outcome/reason; mandatory `run.result` cannot represent failure without an artifact. CLI exit-code rules overlap, and auto-fetching `--expect` can approve content the caller never inspected. **Change:** separate command disposition from the four-outcome result, use discriminated result shapes, define one exit-code table, and require explicit reviewed identity for approvals in machine mode.

14. **Medium — Knowledge identity and retention lose authoritative detail.**  
    **Location:** [10-context-manifest-and-knowledge-spec.md, Refs and fingerprints](/home/mega/auto-dev-cycle/docs/design/m0/10-context-manifest-and-knowledge-spec.md:6); [07-project-profile.md, Retention](/home/mega/auto-dev-cycle/docs/design/m0/07-project-profile.md:57).  
    Hashing only input content fingerprints omits revision, scope and lineage changes with identical bytes. A generic continuity TTL omits KA6’s protection for active resumable checkpoints. **Change:** fingerprint complete dependency identities and policy-relevant lineage; retain source restrictions across content deduplication. Define post-closure TTLs, active-checkpoint pins and grant/project-scoped supervisor continuity.

15. **Medium — M0’s exits can pass without proving the contracts.**  
    **Location:** [00-overview.md, M0 exit check](/home/mega/auto-dev-cycle/docs/design/m0/00-overview.md:41); [11-testing.md, T0 / Red-proof](/home/mega/auto-dev-cycle/docs/design/m0/11-testing.md:28); [01-repo-layout-and-toolchain.md, Code generation](/home/mega/auto-dev-cycle/docs/design/m0/01-repo-layout-and-toolchain.md:62).  
    Golden sequences through a “fake pipeline” may test scripted answers. A commit trailer proves no mutation was caught. Installed-artifact setup is missing, while mandatory live dialect probes introduce credentials/network into M0. **Change:** publish a clean install/run recipe; exercise real M0 validators with fake external dependencies; name each fault’s disabled invariant and expected red. Define later cycle/knowledge fixtures now, implement them in their owning milestones, and defer live-provider probes.

Recommendations for Q1–Q4:

- **Q1:** Use `github.com/ekaynac/auto-dev-cycle`. A later rename is manageable, but entails import and distribution migration.
- **Q2:** Use standard **TypeID/UUIDv7** consistently; the notes currently describe ULIDs. Keep event `seq` authoritative for ordering. [TypeID specification](https://github.com/jetify-com/typeid).
- **Q3:** **Yes.** Default local access to an owner-only socket; make shared-group access explicit and keep it separate from private state storage. Require verified TLS trust for remote clients.
- **Q4:** **Yes.** Specify canonical hash encoding and genesis, verify the full chain before replay, and retain a trusted head in backups. Tail-only verification misses older corruption; a local chain alone cannot detect a complete rewrite or rollback.

No files changed.