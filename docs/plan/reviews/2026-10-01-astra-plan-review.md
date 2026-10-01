# Astra review of the first-release plan

Reviewer: Astra (`gpt-6-astra`, Codex CLI 0.157.0, read-only, reasoning effort high), 2026-10-01.
Brief: everything under `docs/`, plus nöbetçi and CTIS-SIMS references.
The reviewer output follows verbatim.

---

**GO with changes** for committing this plan as the basis of M0. Resolve the High findings first; none requires reversing the operator’s fixed choices.

1. **High — Host-side role labels do not enforce isolation.**  
   **Location:** [01-first-release-plan.md §2.2, “Model gateway”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:124), §2.3.  
   **Problem:** Read-only worktrees and clean sessions do not establish that a planner/reviewer cannot execute repository code, read unrelated host files, invoke hooks/MCP tools, or launch unleased model calls. “Roles that do not execute untrusted code” is currently an assumption.  
   **Why:** nöbetçi’s routing leak came from a nested `Agent(model=sonnet)` call; its isolation lesson explicitly rejected command parsing as the trust boundary.  
   **Change:** Require an enforced host-role capability profile: bounded file access, no arbitrary execution or implicit extensions, sandbox-mediated test requests, and no unleased child model calls. Preserve native subscription login, but declare telemetry limitations explicitly. Add hostile-repository conformance checks.

2. **High — The sandbox contract does not explain how real projects build and integrate.**  
   **Location:** [§2.10 “Sandbox”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:297), §2.9, §3 T2, R9.  
   **Problem:** Gateway-only egress leaves dependency acquisition unspecified; “one container per run” does not explain the multi-service integration environment. Giving candidate-controlled Compose files the controller’s container socket would undermine the boundary.  
   **Why:** T2 depends on several databases, Redis and service networking. Discovering this incompatibility at M4 would force a security redesign.  
   **Change:** Define separate dependency preparation, untrusted build/gate execution, and controller-managed integration environments. Pin prepared inputs; constrain candidate deployment specifications and mounts; expose no controller socket or credentials. Prove a small multi-service fixture before committing to the T2 execution design.

3. **High — Executor leases do not yet model shared scarcity or guarantee progress.**  
   **Location:** [§2.1 “Source / Executor / Lease” and §2.2 “Scheduler”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:72).  
   **Problem:** Several executors can share one account quota, broker balance, GPU or concurrency limit. Weighted priority alone does not prevent starvation. “Earliest reset” is undefined for unknown quotas, revoked credentials, retired models and quarantined sources.  
   **Why:** A shared OpenRouter limit previously blocked both background work and builders. Capacity can also be consumed by builds while every completed candidate waits for review.  
   **Change:** Add shared resource/quota groups, atomic reservations and settlement, explicit unknown states, and distinct waits for exhaustion versus invalid configuration. State fairness and recovery rules, including reviewer capacity, local gate resources and bounded quarantine probes. Apply trust, paid-fallback authority and budgets on every fallback—not only initial placement.

4. **High — Drain/resume promises exceed the adapter contract.**  
   **Location:** [§2.2 “Preemption”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:117), §2.3, §2.11.  
   **Problem:** `cancel()` cannot guarantee a checkpoint when a source disappears. No fencing rule prevents a late old attempt from publishing after replacement starts. Graceful checkpoint import and reboot’s “partial work not imported” are not reconciled.  
   **Why:** The predecessor needed explicit interrupted-call accounting and errored-build continuation; a changed but unreviewed candidate once reached closure and stalled.  
   **Change:** Specify graceful drain versus abrupt loss, durable controller-owned checkpoints, attempt/lease fencing, and which validated partial patches may survive. Reserve before dispatch; retain uncertain usage until reconciled; do not assume cancellation prevents provider billing. Test disconnect, late completion and controller restart during handoff.

5. **High — Re-gating alone does not preserve reviewed delivery identity.**  
   **Location:** [§2.5 “Review / Merge queue”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:198), §2.9.  
   **Problem:** Rebase can change the reviewed candidate, but the queue requires only another gate. Closure precedes this transformation. External writers can move the base despite “only the queue moves” it. Mixed-source builds also make “the builder’s family” ambiguous.  
   **Why:** `behind_base`, sweep merges and unresolved rebase markers are documented failures. Passing tests does not establish that conflict resolution retained reviewed behavior.  
   **Change:** Bind review, closure and required target receipts to the final prospective merge tree. Define when evidence remains reusable; otherwise refresh affected review. Use expected-base checks and reconcile ambiguous remote outcomes. Require reviewer independence from every contributing builder/repair family, or wait. Park blocked candidates without blocking unrelated projects.

6. **High — The simple gate’s essential amendments need explicit preservation.**  
   **Location:** [§2.5–2.6](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:180), §5 “Simple phase gate.”  
   **Problem:** The port reference carries more substance than the written acceptance criteria: requirement reconciliation, advisory claim caps, assertion inspection, whole-repair-diff review and wrong-owner routing are missing or implicit.  
   **Why:** The [September 28 amended design](/home/mega/nobetci-dev/docs/2026-09-28-simple-phase-gate.md:47) added these deliberately. Machinery caused approximately **88 hours of stops versus 27 hours of real blockers**; a test-deletion cheat was banked as P3.  
   **Change:** Restate those five rules and test them. Required behavior stays blocking unless explicitly deferred; claim caps cannot delete scope. Evidence is controller-produced; missing evidence never passes; infrastructure failures do not become builder repairs. Distinguish conclusive integrity violations from heuristic warnings so the guard does not become another noisy gate.

7. **High — Trust placement must cover the actual recipient, including agent relays.**  
   **Location:** [§2.2 “Placement filters / Routing verification”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:107), §2.4; decisions D9–D10.  
   **Problem:** `own / contracted / free-unknown` is too coarse to express allowed destinations and retention constraints. An owned relay may forward to another service. A returned model-family string cannot establish where data went. “Prompts, not repos” still permits source code and confidential tool output to leave.  
   **Why:** Post-response routing detection cannot undo disclosure.  
   **Change:** Keep the trust floor, augmented by explicit permitted destinations/upstreams. Check before egress and on fallback. Specify agent endpoint allowlists, controller/source-scoped identity, revocation behavior for active tunnels, and prompt/log retention. State plainly what agents can see; distinguish reported routing from verified routing.

8. **High — Approval records need immediate, exact authority semantics.**  
   **Location:** [§2.11 “Web dashboard”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:314), §2.6 “Approval.”  
   **Problem:** “Immutable, dated” and “bound into the next revision” do not define what an approval authorizes or when holds, revocations and rulings take effect. Concurrent dashboard actions can approve stale content.  
   **Why:** nöbetçi repeatedly misread appended authorization and ignored narrower scope. Model-generated plans must never acquire authority merely by entering the store.  
   **Change:** Bind each action to actor, project, exact object digest/version, scope and expected current state; apply it transactionally and idempotently. Define immediate versus revision-bound actions and revocation precedence. Include authenticated bootstrap, session protection and replay/stale-action tests in the minimal dashboard contract.

9. **High — Milestone dependencies are reversed.**  
   **Location:** [§4 M0–M5 and M7–M8](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:365).  
   **Problem:** M1 builds before M2’s gateway; M2 admits remote sources before M3’s data placement and M5’s dashboard enrollment; M4 approves plans before approval records exist. Recovery arrives after concurrency, while the T1 comparison suite arrives after shaping and classifier development.  
   **Change:** Move authority/outcome/lease/receipt contracts into M0; minimum gateway, accounting and recovery into M1; placement policy and authenticated enrollment before remote admission. Deliver minimal approval UI before approved-plan execution. Establish T1 before evaluating shaping/classifiers. Attach a reproducible command, fixture, expected result and prerequisite list to every exit check; run checks against installed artifacts.

10. **High — D19 can pass without proving distributed unattended operation.**  
    **Location:** [00-brainstorm-decisions.md D19](/home/mega/auto-dev-cycle/docs/plan/00-brainstorm-decisions.md:44), [plan §3 “Capacity / Metrics” and M8](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:351).  
    **Problem:** Three source kinds “in the pool,” green product tests and five self-hosted changes do not prove useful participation, dial-out operation, recovery or fairness. “Target 0” interventions is not an executable release condition. Private history and private T2 inputs also prevent a stranger reproducing the checks.  
    **Change:** Require measured accepted contributions from all three kinds, including a dial-out source; an unattended run after initial authorization; and injected quota exhaustion, disconnect, restart and base movement. Verify no unauthorized fallback, duplicate delivery or cross-project disclosure, and continued independent-project progress. Publish redistributable fixtures and distinguish expected approvals from rescue interventions. Include PHP explicitly in the final checks.

11. **Medium — The CTIS-SIMS slice needs a precise benchmark contract.**  
    **Location:** [§3 “v1 T2 slice”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:333).  
    **Problem:** The service set is a meaningful, ambitious polyglot proof, but requirement labels currently overstate coverage. FReq1.1.1 includes QR output, explicitly excluded here; FReq7.6 depends on omitted FReq7.5 retries; FReq7.1 requires Turkish responses. The test-design document calls for live AI acceptance, whereas the plan specifies mocks.  
    **Change:** Retain the chosen services, but publish a small scope/deferral map and precedence rule for SRS/SDD conflicts. Bound the workflows, role matrix and nonfunctional claims. Separate mocked orchestration checks from a bounded live semantic check—or explicitly narrow the AI claim. Include audit delivery across an interruption and role-revocation propagation, not just happy paths.

12. **Medium — Dispatch shaping should be narrow and optional in v1.**  
    **Location:** [§2.6 “Validators / Dispatch-time shaping”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:227), R8.  
    **Problem:** Disjoint paths do not prove semantic independence. D8 mentions split/merge, while the plan mainly defines splitting. “Acceptance is whole” and “sizes fit economics” are not wholly deterministic predicates.  
    **Why:** Adaptive units appeared in only **2/51 plans**, with sub-builders reported as effectively unused.  
    **Change:** Preserve plan-once/shape-at-dispatch using approved unit boundaries, selecting sequential or parallel execution from live capacity. Validate row coverage, dependencies, ownership and unchanged authority. Keep arbitrary repartitioning/merging experimental until T1 demonstrates benefit. Separate mechanical validation from reviewer judgment; do not port planning doctrine “verbatim” into blocking heuristics.

13. **Medium — Advisory classifiers can still acquire indirect control.**  
    **Location:** [§2.7 “Classifier program”](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:250).  
    **Problem:** Error classification, finding routing, runaway detection and parallelisability all affect control flow if consumed operationally. “Beats the rule” on accuracy does not prevent harmful false negatives. Historical outcomes are not automatically trustworthy labels.  
    **Change:** Start in shadow mode. Permit ranking only among choices already admitted by deterministic policy; classifiers cannot suppress findings, authorize fallback, terminate calls or establish independence. Use reviewed labels, incident-separated held-out data, unseen providers/languages, abstention and per-surface harmful-error measures. Publish sanitized datasets. “No classifier enabled” must count as successful evaluation.

14. **Medium — Optional features are bundled into the release’s critical path.**  
    **Location:** [§2.8–2.11 and M7](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:272).  
    **Problem:** Generalized code-writing filler creates more reviews, gates, merges and escalations precisely when the scheduler is supposed to exploit spare capacity. Mandatory Jev/Kev comparisons and several operational plugins add independent release projects.  
    **Why:** The sweeper once consumed approximately **75% of shared-key spend**; its 101 receipts included 71 escalations.  
    **Change:** Keep one deterministic filler surface and account for its downstream review/gate demand. Move generalized filler producers, Kev quantization, optional OIDC, SSH deployment and extra pager transports to v1.x. Keep the fixed dashboard, forges, remote capacity and polyglot slice. Delete mandatory model-comparison work where no surface has demonstrated a need.

15. **Low — Neutrality and result vocabulary need small corrections.**  
    **Location:** [§1, §2.1–2.2](/home/mega/auto-dev-cycle/docs/plan/01-first-release-plan.md:68), §2.9; decisions “Defaults.”  
    **Problem:** `routing_violation` is described as an outcome despite the four-outcome contract. JUnit is an interchange option, not universal test semantics. Plain git does not itself supply protected-branch policy. English framework vocabulary must not restrict product requirements or output.  
    **Change:** Make routing violations reason codes; define unsupported/missing test-result behavior; parameterize base refs and protection capabilities. State Linux-controller support separately from cross-platform capacity agents. Include nonstandard layouts, missing `.git` at runtime, non-JUnit results and non-English product requirements in neutrality fixtures.

**Things the plan gets right that must survive**

- One capacity authority, thin adapters, ordered fallbacks and explicit paid authority.
- Patch-only builders; content-bound gates and receipts with inner predicates.
- Four outcomes, cumulative accounting, evidence before teardown.
- Fresh independent reviews, deterministic closure and one merge queue.
- Holds with executable resume signals and supervision independent of interactive sessions.
- Deletion of blind/obligation/sharded-package machinery; fixtures instead of an InfraMedic migration.

**Open decisions for the operator**

- Approve the proposed v1.x deferrals while retaining the fixed v1 choices.
- Define the unattended release bar: initial approvals allowed, rescue interventions disallowed, and minimum useful contribution per source kind.
- Decide which agent/provider destinations may receive private project content and what telemetry uncertainty is acceptable.
- Authorize redistribution or replacement of benchmark inputs, and approve the exact CTIS-SIMS deviations from its original requirements.
- Decide whether v1 shaping is limited to approved unit boundaries; broader automatic repartitioning should remain experimental.