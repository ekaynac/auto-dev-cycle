**GO with changes** — resolve the three High findings before adoption; retain K1–K12, including cross-project framework lessons.

Read-only review; no files changed. This checks my earlier recommendations on manifests, continuity, staged disclosure, hybrid storage and the shared spec. Those recommendations are inputs, not independent evidence that the resulting design works.

1. **High — K11 does not yet enforce source-side disclosure restrictions.**  
   **Location:** [03-knowledge-layer.md §6, guardrails 2–4](/home/mega/auto-dev-cycle/docs/plan/03-knowledge-layer.md:153); [plan §2.15](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:496).  
   A mechanical validator cannot establish that arbitrary paraphrased prose contains no project information. Checking the receiving project’s policy also cannot preserve the producer’s restrictions: an on-prem lesson can pass admission confirmation and subsequently reach an open project’s cloud executor. Restricted evidence pointers do not protect the description or pointer metadata.  
   **Change:** Inherit source restrictions through controller-recorded lineage. Automatically share only allowlisted framework telemetry rendered through fixed templates; require explicit, digest-bound human export approval for other project-derived prose. Keep evidence pointers opaque. Check source restrictions **and** receiver/destination policy at every delivery, including lookup and fallback. Acceptance review must not implicitly authorize export. Add on-prem→open and revocation-between-selection-and-delivery fixtures.

2. **High — K3’s evidence gate is circular.**  
   **Location:** [03 §8, E1–E5](/home/mega/auto-dev-cycle/docs/plan/03-knowledge-layer.md:195), §9 M2/M6; [plan §4 M2](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:567).  
   M2 must decide whether durable knowledge helps, but E1 contains no curated-lesson arm. E3 tests that benefit only after K3 enables the collection. E4/E5 also assume claims or lessons that do not yet exist. Mechanical-view gains cannot justify curated knowledge.  
   **Change:** Run a bounded, fixture-only lesson comparison during M2, using reviewed experimental payloads without a production `Claim` store. Exercise admission and validity against those fixtures. Keep production claims and live E3 conditional on positive results. This restores the experimental-before-production sequence I previously recommended.

3. **High — Protected knowledge files have no executable update path.**  
   **Location:** [03 §4.2, decision/convention](/home/mega/auto-dev-cycle/docs/plan/03-knowledge-layer.md:94); [plan §2.5 Accept](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:241), §2.7 validators.  
   K8 puts decisions and conventions in protected files; K5 requires candidate review and approval. The existing accept stage refuses protected-path edits, and the planner rejects tasks requesting them. Approved knowledge changes therefore cannot follow their specified path. This exposes an omission in my earlier hybrid-storage recommendation.  
   **Change:** Define one narrowly scoped protected-document change operation under §2.11, binding permitted paths, old/new digests and expected adopted revision. It must retain independent review and the merge queue. The resulting convention must not approve its own adoption or widen authority.

4. **Medium — Tree plus extractor version is an incomplete view identity.**  
   **Location:** [03 §3, regenerated views](/home/mega/auto-dev-cycle/docs/plan/03-knowledge-layer.md:63), §4.4; plan §2.15 validity.  
   Dossiers and digests also depend on receipts, findings, approved contracts and dispositions. Those can change without a tree change. Regenerating a view does not make an old receipt applicable to the current merge tree.  
   **Change:** Fingerprint exact input revisions, candidate snapshot, adopted contract and renderer configuration. Separately check evidence applicability against the consumer’s tree/environment. Test changed findings at an unchanged tree, approved amendments, and rebase. Replace “never stale by construction” with the narrower guarantee actually proved.

5. **Medium — Manifest completeness exceeds the host-adapter contract.**  
   **Location:** [03 §4.1 `ContextManifest`](/home/mega/auto-dev-cycle/docs/plan/03-knowledge-layer.md:82), E0; [plan §2.4](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:216), §2.15 records.  
   “Every byte a model saw” includes later tool results, harness instructions and compaction—not merely the controller’s initial assembly. Current adapters declare telemetry gaps without defining exposure coverage. My manifest recommendation needs this qualification.  
   **Change:** Distinguish assembled, delivered and unobservable context. Require per-turn coverage declarations and record controller-mediated tool deliveries. E0 should reconcile complete sampled sessions, including staged disclosure and fallback; unknown coverage must remain unknown. Reference the existing call ledger for cost rather than create another accounting owner.

6. **Medium — Continuity omits ordinary repair handoffs and supervisor notes.**  
   **Location:** [03 §3 and §4.1 `AttemptRecord`](/home/mega/auto-dev-cycle/docs/plan/03-knowledge-layer.md:64); [02-supervisor-role.md §5](/home/mega/auto-dev-cycle/docs/plan/02-supervisor-role.md:116).  
   Sealing only at drain/interruption misses normal builder→review→repair transitions. “Same task” does not specify which roles may consume notes. Supervisor handovers are outside this task-bound class altogether. The predecessor explicitly preserves builder notes on every exit.  
   **Change:** Reuse the existing checkpoint mechanism for every builder exit; bind continuity to project, task, contract revision and attempt. Limit raw builder notes to builder continuations. Define supervisor notes as untrusted, grant/project-scoped operational continuity referencing authoritative records. Add poisoned-note and mixed-project-handover cases to E4; delimiters alone are not enforcement.

7. **Medium — K1 and the experiment exits are not operationally defined.**  
   **Location:** [03 §8](/home/mega/auto-dev-cycle/docs/plan/03-knowledge-layer.md:186); [plan §3 T1](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:505), §4 M0–M2.  
   T1’s 20–30 tasks can screen effects, but cannot establish broad safety across all arms and capability classes. Acceptance alone is not correctness; manifests establish exposure, not causal attribution. Costs lack a common treatment of subscriptions, local compute and curator time.  
   **Change:** Predeclare correctness from hidden tests plus independent defect adjudication, cost assumptions, paired task/model comparisons, repeats and incident-separated holdouts. Test continuity with matched interruptions and E2 on identical defective candidates. Treat inconclusive results as disabled. Put E0 reconciliation and published ablation decisions into milestone exits; carry M5 curation and conditional M6 E3 into the main milestone table.

8. **Medium — Keep the shared spec from becoming a second implementation project.**  
   **Location:** [03 §4.1, §9](/home/mega/auto-dev-cycle/docs/plan/03-knowledge-layer.md:75).  
   My ten fixtures cover semantics extending beyond ADC’s initial runtime. Implementing every fixture’s machinery in M0 would undermine the intended dependency order. `FailureSignature` also risks creating another maintained workflow.  
   **Change:** Keep M0 to schemas, invariants and fixture definitions; implement relevant checks alongside their owning milestone. Make `AttemptRecord` the existing checkpoint’s schema and `FailureSignature` a derived, project-scoped aggregate with deduplicated advisory proposals. Defer clustering, extra lifecycle tables and shared runtime code. Spec or fixture updates must never automatically alter grants, accepted claims or policy.

9. **Low — Decision ownership has already drifted.**  
   **Location:** [D35](/home/mega/auto-dev-cycle/docs/plan/00-brainstorm-decisions.md:75), plan §2.15/O6; [03 K10 and §10.2](/home/mega/auto-dev-cycle/docs/plan/03-knowledge-layer.md:225).  
   D35 and §2.15 settle the spec’s home in this repo; §10/O6 still ask where it belongs. The knowledge design also calls decisions both controller-produced state and repository-owned payloads.  
   **Change:** Make `03` authoritative for knowledge semantics, with D35 recording adoption and §2.15 summarizing by reference. Distinguish repository decision content from controller adoption/approval records. Close the resolved home question and refresh the plan’s status metadata.

Recommendations for **§10 / O6**:

- **K11 timing:** Prove admission/exposure controls with fixtures in M2 and two-project integration in M4. Populate the collection only after K3 evidence; M6 need not be the first second project. Sharing remains in v1.
- **Spec home:** This repo, as D35 already states. InfraMedic vendors a pinned revision; reconsider a neutral repository only when maintenance warrants it.
- **Retention:** Preserve active resumable checkpoints. Use explicit, configurable post-closure TTLs and tombstones; shorter retention for on-prem is not established by the evidence. Keep payload deletion separate from immutable accounting/history.
- **Capability profiles:** Key measurements by role, model snapshot and harness/version. Start with effective context budget, tool/schema reliability, correctness, latency and cost; keep authorization separate.
- **InfraMedic follow-ups:** Retain all four as inputs to its own session, not ADC release dependencies. No InfraMedic changes are authorized by this review.