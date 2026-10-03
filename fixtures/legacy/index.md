# Incident → fixture index

Every row of the predecessor's incident→test index (handoff 2026-10-03, file `08-incident-test-index.md`, 206
verified test names) mapped to the auto-dev-cycle fixture that will reproduce its behaviour, with the owning
milestone, or to a one-line reason it does not apply. The predecessor's test files are named only as identifiers.
Its fix commit ids are left out.

**Kinds:**
- **LG**: a legacy golden in this directory.
- **N13**: a "cannot occur" fixture of `docs/design/m0/13-artifact-graph.md` (these take priority, note 11).
- **M0C**: an M0 contract fixture of `docs/design/m0/11-testing.md` (the M0 list or the intake list); a few name the
  fixture list of note 12.
- **T0F**: a T0 fault fixture of note 11 ("Later fixtures"), implemented in its owning milestone.
- **NEW**: no fixture is named in the design notes yet; a proposed fixture is given.
- **N/A**: does not apply, with the reason.

Milestones are those of `docs/plan/01-first-release-plan.md` §4. Where two are given, the first defines the fixture and
the second implements the behaviour it drives.

## A. Delta lessons 2026-10-01 → 03

| Row | Lesson | Predecessor tests (files) | auto-dev-cycle fixture | Kind | Milestone |
|---|---|---|---|---|---|
| A1 | §1 A ruling reaches the reviewer only when it answers an owed input; a new ruling re-keys | `test_simple_gate.py`, `test_phase_audit_context.py` (2) | "an undeclared ruling never enters a review's controller context"; "an answer reaches only the successor review"; also M0C "a ruling answers an owed input and re-keys the review", "a ruling for an input not owed is not delivered" | N13 | M0 |
| A2 | §2 Evidence attempts counted per demand; stalled driver relaunched at most N times; cooldown names its end | `test_phase_evidence_jobs.py` (3) | "a new demand gets new evidence attempts and re-keys"; "total-cap exhaustion across demands"; note 12 "a new demand gets new attempts; the same demand does not" | M0C | M0 |
| A3 | §3 Review identity includes the demand; a same-plan rerun does not re-key | `test_phase_identity_demand.py` (5) | `f16-walk-demand/`; also M0C "an unchanged `conclusions_digest` does not trigger re-review", N13 "equal conclusions reuse a review" | LG | M0 |
| A4 | §4 A learning fallback never reaches a phase review; inline-fallback recovery; self-disposed refusal not a duplicate | `test_supervisor_fallback.py`, `test_phase_recovery_inline_fallback.py` (7) | `f16-inline-fallback/`; also M0C "a fallback outside its role/mode scope is refused". The duplicate-disposition part is the predecessor's batch admission; attempts here are keyed and fenced (note 05) | LG | M0 / M2 |
| A5 | §6 Lab installer lock waits share one deadline; a non-lock failure is not retried; shipped file in its component | `test_lab_install_apt_lock.py`, `test_components.py` (5) | Lab-installer specific (intake "Not taken"); the general one-deadline rule is note 12 text, and component manifests are not ported (plan §5) | N/A | — |
| A6 | §7 Candidate-owned setup errors on a `.git`-less target become a builder repair; others need diagnosis | `test_edge_input_errors.py` (6) | `edge-owned-errors/`; T0F "missing `.git` at runtime"; M0C "an import error outside the changed files that passes through a changed test file stays a runner fault" | LG | M0 / M1 |
| A7 | §8 Release proof: facts not content, isolation by probe, no production writes, one deadline, remote leftovers killed, a leftover copy blocks | `test_release_proof_job.py` (10) | "an unrestored evidence run blocks"; "a crash between evidence collection and cleanup leaves no applicable receipt; a new job on an excluded target is refused; a late result from a superseded run is refused"; N13 "restore failure keeps the target excluded across restart"; note 12 "an empty isolation setting is never published", "a restricted artifact never reaches a prompt" | M0C | M0 |
| A8 | §10 Strict gate: claim union, test ids, receipt-proven claims, dispositions, pair merge, refutation, never one family twice, limit wait | `test_phase_strict_gate.py`, `test_phase_pair_invocation.py`, `test_f1_gate_config.py` (17) | "strict pair: an unavailable member holds; the contested path with and without refutation; a receipt-only claim cannot pass on reviewer text" | M0C | M0 / M5 |
| A9 | §10 A held task is released only when its gate capability is installed | `test_task_hold.py`, `test_f1_gate_config.py` (3) | The predecessor's capability-release of parked tasks is not ported; a waiting node here resumes only on its registered signal (note 02) | N/A | — |
| A10 | §11 Contract amend: quoted removals, verbatim new line, added identity refused | `test_task_add.py`, `test_f1_gate_config.py`, `test_harness_install.py` (4) | "an effective-change widening disguised as a profile edit"; "a protected-document change carrying a policy widening" (the lexical amend rule itself was dropped after the PR #5 review, intake A9) | M0C | M0 |
| A11 | §12 Approvals bound by canonical content; pinned dispositions survive relocation | `test_phase_strict_gate.py`, `test_phase_provider.py` (2) | "identical bytes with different lineage"; "an answer approval that does not bind the owed input's revision is refused" | M0C | M0 |
| A12 | §13 Stale gate at audit admission (open in the predecessor) | none (nearest `context-transmission.py`) | "a review cannot start with another tree's gate result"; also M0C "a review lease is refused while its gate results are for another tree" | N13 | M0 |
| A13 | §14 A 402 read as "no changes" (open in the predecessor) | none (nearest `test_transient.py`, `errored-build-continuation.sh`) | "a 402 before the first edit is `could_not_run`, never `empty`"; "a 402 after edits is a sealed `partial_patch`"; also M0C "a 402 mid-call with an applied candidate is sealed" | N13 | M0 |
| A14 | §16 Orphaned test loop (open in the predecessor) | none (`errored-build-continuation.sh`) | Removed structurally: every test runs in the test sandbox with `--unshare-pid --die-with-parent` (note 11); its containment red tests already exist | N/A | M0 |
| A15 | R10a Receipt predicates on the inner status; full-package digest; an interrupted install blocks | `test_receipt_rules.py`, `test_simple_gate.py`, `test_lab_deploy_job.py` (7) | "nested receipt lie"; "omitted required predicate"; "an unrestored evidence run blocks"; N13 "a gate on the source-test workspace and runtime verification on the package artifact see their declared facts" | M0C | M0 |

## B. Handoff invariants and incident classes

| Row | Invariant / incident | Predecessor tests (files) | auto-dev-cycle fixture | Kind | Milestone |
|---|---|---|---|---|---|
| B1 | 1.1 Four outcomes; `could_not_run` ≠ `empty`; non-empty error text | `test_executor.py`, `test_phase_report_revalidation.py`, `test_row_map.py`, `test_builder_key_probe.py`, `test_browser_walk.py`, `gate-excerpt.py` (7) | "a 402 before the first edit is `could_not_run`, never `empty`"; T0F "lying test output", "non-JUnit results" | N13 | M0 / M1 |
| B2 | 1.2 Copy evidence before teardown | `test_executor.py`, `test_refusal_disposition.py`, `test_host_restart_recovery.py`, `test_sweep_end_to_end.py`, `final-cleanup.py` (5) | "crash at each effect boundary"; M0C "a crash between evidence collection and cleanup leaves no applicable receipt"; the fake sandbox's artifact-before-teardown | T0F | M1 |
| B3 | 1.3 One decision in one place (idle/busy, refusal definition) | `test_boundary_busy_hardening.py`, `test_refusal_disposition.py`, `test_accounting.py` (4) | Design rule (plan §6), held by module structure; the predecessor's idle/busy boundary is not ported (one writer, note 03) | N/A | — |
| B4 | 1.3 Two live parked states are ambiguous and stop admission | `test_task_hold.py` (1) | The predecessor's batch parking states are not ported; each waiting node carries one hold with one signal (note 02) | N/A | — |
| B5 | 1.4 Test guard: no writes outside a temp home; a test proves its own setup | `test_nb_test_guard.py`, plus the real-fixture setup checks (6) | Note 11 sandbox containment red tests (sacrificial sentinels); each legacy golden's loader verifies its setup (README) | M0C | M0 |
| B6 | 1.4 Fakes must match the real contract (cumulative patch) | `test_sweep_worker_patch.py` (1) | Note 11 contract-faithful fakes: the fake adapter's cumulative patches, fixed first when a real implementation differs | M0C | M0 |
| B7 | 1.4 Behaviour, not source strings | `test_provider_pref.py`, `final-cleanup.py` (2) | Enforced by lint (note 11, red-proof rule: assertions on source text are rejected), not by a fixture | N/A | M0 |
| B8 | 1.5 Validation before any irreversible write | `test_accounting.py`, `test_harness_install.py`, `test_build_provider.py` (4) | "idempotency key reuse with a different input"; "stale expected state" | M0C | M0 |
| B9 | 1.6 Install footprint: component manifest, import closure | `test_components.py` (4) | Hand-maintained installer lists are not ported (plan §5); the controller ships as one Go binary | N/A | — |
| B10 | 1.7 Never reset counters, charges, candidates | `test_accounting.py`, `test_review_call_cost.py`, `test_host_restart_supervisor.py`, `final-cleanup.py` (4) | "stale-attempt usage still counted"; "duplicate usage deltas"; N13 "an expired receipt triggers renewal without resetting counters" | M0C | M0 |
| B11 | 1.8 Caps raised only by a bound approval; rails come from the profile | `test_harness_install.py`, `test_baseline_relock.py` (3) | "an effective-change widening disguised as a profile edit" | M0C | M0 |
| B12 | 1.11 Staged installs at an idle boundary; content-pinned; grants rebound | `test_harness_install.py`, `test_hold_watch.py` (6) | "a revoked approval between claim and commit blocks the commit"; framework self-install with independent review is M6 | N13 | M0 / M6 |
| B13 | 1.11 / 2.5 Busy counts only the installed runtime | `test_boundary_runtime_only.py` (3) | Busy detection by process inspection is not ported; tests run in the sandbox, and the maintenance boundary is controller state (`maintenance_boundary` hold) | N/A | — |
| B14 | 1.11 / 2.1g Back-to-back staged installs vs failure cooldown | `test_hold_watch.py` (3) | Fixed cooldowns are not ported (plan §5) | N/A | — |
| B15 | 1.14 Every hold has a resume signal; event-keyed dedup | `test_hold_watch.py`, `test_task_hold.py`, `test_edge_unreachable.py` (6) | "a missing answer yields `awaiting_input` and recovers on the answering approval without a manual release; a precondition failure recovers on its probe"; N13 "a `could_not_run` evidence result retries on its probe without operator invalidation" | M0C | M0 |
| B16 | 1.15 Proof keyed by content; identity hashes observations, not timestamps | `test_phase_identity.py` (3) | "equal conclusions reuse a review; a changed step outcome re-runs it"; LG `f16-walk-demand/` step 4 | N13 | M0 |
| B17 | 1.15 Evidence names its instrument; a changed candidate re-executes; tamper or expiry never executes | `test_phase_evidence_jobs.py`, `test_harness_install.py` (3) | "an expired receipt triggers renewal without resetting counters"; M0C "unrelated GO reuse", "a receipt for another demand with identical conclusions does not apply" | N13 | M0 |
| B18 | 1.13 Neutrality; refuse to run when unconfigured | `test_paths.py`, `test_executor.py` (3) | Proposed: "the controller refuses to start without a valid profile, naming what is missing" (note 07 validation; plan §6) | NEW | M0 |
| B19 | 4.2-8 English vocabulary; the old words never written or compared | `test_vocab.py` (2) | Turkish vocabulary and the compat layer are not ported (plan §5); unregistered codes fail `adc dev vocab check` (note 02) | N/A | — |
| B20 | 2.1a Closure deadlock → deterministic closure text; bounded closure call; unmappable rows shown, never invented | `test_row_map.py`, `test_closure_text.py`, `closure-text-call.sh`, `closure-row-map.sh` (4 + 2 suites) | Proposed: "an unmappable acceptance row is shown unmapped in the closure, never invented" and "the closure call stops at its admission cap" | NEW | M1 |
| B21 | 2.1a Closure vs product routing, including absolute paths | `fix-routing.py` (3) | `closure-record-finding-r4/` | LG | M1 |
| B22 | 2.1b An errored build with an unreviewed changed candidate gets a continuation | `errored-build-continuation.sh`, `partial-build-resume.sh` (2 suites) | "a 402 after edits is a sealed `partial_patch`"; T0F "idle cut"; plan §2.5 builder continuation before closure | N13 | M0 / M1 |
| B23 | 2.1c `base_broken`; runaway editless builds; degenerate no-change repair | `test_base_broken.py`, `test_build_runaway.py`, `test_transient.py` (6) | Proposed: "a test failing on the base passes as base-broken and is shown to the reviewer; one-of-two base failures is flaky"; "an editless streaming build is stopped at its threshold, evidence first" | NEW | M1 |
| B24 | 2.1d Rebase broken pipe; conflicts become builder work | `rebase-candidate-sigpipe.sh`, `test_rebase_candidate.py` (2 + 1 suite) | "external base move"; N13 "a base move mid-generation starts a new generation", "carry-over applies only with an equal patch identity" | T0F | M4 |
| B25 | 2.1f Task identities named by a task must be authorised | `test_task_add.py`, `test_task_rows.py` (3) | Proposed: "a plan or task revision naming an identity outside its grant is refused by the plan validator" | NEW | M5 |
| B26 | 2.1h Host restart: charged once, interrupted, evidence kept; empty-branch resume | `test_host_restart_recovery.py`, `empty-branch-resume.sh` (2 + 1 suite) | "a crash after effect intent resumes, never re-allocates"; M0C "a revoked attempt's late result", "stale-attempt usage still counted"; T0F "crash at each effect boundary" | N13 | M0 / M1 |
| B27 | 2.2a–c Provider transients: idle timeout, 4xx protocol refusal, 503, dropped stream; upstream recorded | `test_transient.py`, `test_build_provider.py` (6) | "provider 4xx"; "idle cut" (fake provider: degenerate output, streaming gaps) | T0F | M1 / M2 |
| B28 | 2.2b A rejected request is a disposition, never a fallback trigger | `test_refusal_disposition.py`, `test_accounting.py` (4) | "provider 4xx"; note 02 "Disposition is not an outcome" | T0F | M2 |
| B29 | 2.2d Remote 5xx: bounded retry and done-probe | `merge-remote-retry.sh`, `test_transient.py`, `test_audit_publish.py` (2 + 1 suite) | "crash at each effect boundary" with the fake forge's ambiguous remote outcomes; N13 "a crash after effect intent resumes" | T0F | M1 |
| B30 | 2.2e A key over its limit is a provider limit honouring the announced reset | `test_transient.py` (2) | "provider 4xx" with the fake provider's 429-with-reset and unknown quota (`quota_exhausted`, `quota_unknown`) | T0F | M2 |
| B31 | 2.2f Routing: the actual model must match the requested one | `test_build_provider.py` (2) | Proposed: "a response from another model or endpoint than requested is `routing_violation`" (code registered in note 02; no fixture named) | NEW | M2 |
| B32 | 2.3c Per-call cost deltas, never session totals; unknown price is unknown, never zero | `test_review_call_cost.py`, `test_claude_call_cost.py`, `test_prices.py` (5) | "duplicate usage deltas"; "stale-attempt usage still counted" | M0C | M0 |
| B33 | 2.3b Fresh review session per round plus digest | `test_phase_fresh_session.py`, `test_phase_round2_recovery.py`, `test_phase_provider.py` (3) | Proposed: "every review round opens a fresh session whose context is the manifest plus the controller digest; a resumed session is refused" | NEW | M1 |
| B34 | 2.4 Pager, durable outbox, wake unit, bounded pause | `test_pager.py` (4) | Proposed: "a page is deduplicated by hold id + signal instance; an overdue `page_after` pages once" (note 02) | NEW | M1 |
| B35 | 2.5 Lock truncated by a probe; an empty lock taken silently | `deployment-recovery.sh`, `test_boundary_busy_hardening.py` (1 + 1 suite) | "a second daemon" | M0C | M0 |
| B36 | 2.6 Simple gate: clean close; failed claim → repair → re-check; wrong owner → hold | `test_simple_gate.py` (4) | Proposed: "a failed blocking claim goes to builder repair and is re-checked; an environment fault holds `wrong_owner`". The legacy obligation, blind and package blockers in this row are not ported (plan §5) | NEW | M1 |
| B37 | 2.6 Test deletion / weakening guard | `test_test_integrity.py` (3) | "test deletion/skip" (`test_integrity_violation`) | T0F | M1 |
| B38 | 4.2-12 Over-broad classification: an assertion in the candidate's own fixture is repairable | `test_runner_trouble.py` (1) | `edge-owned-errors/` (ownership rules, note 07); M0C "an import error outside the changed files … stays a runner fault" | LG | M1 |
| B39 | 4.1-13 The background sweeper never blocks and yields at boundaries | `test_sweep_run.py` (3) | Proposed: "a filler lease yields to cycle work and never delays review" (M7 exit check) | NEW | M7 |
| B40 | 4.1-10 Session isolation: no foreign instruction files | `test_cli_isolation.py` (1) | "hostile repo" (instruction files planted in the repository and its ancestors) | T0F | M1 |
| B41 | Confined reviewer sandbox: paths outside the read roots unreachable, TCP denied | `test_scratch_sandbox.py` (2) | Proposed: "the host-role reviewer cannot read outside its roots or open a connection" (plan §2.4 host-role profile) | NEW | M1 |

## C. Gaps the predecessor recorded

| Row | Gap | auto-dev-cycle fixture | Kind | Milestone |
|---|---|---|---|---|
| C1 | §13 stale gate, §14 402 classification, R62 test-process containment | See A12, A13, A14 | (cross-reference) | M0 |
| C2 | 2.1e A held admission died silently (proven only by operation) | Proposed: "the external watchdog pages when the controller stops while a hold is open" | NEW | M1 |
| C3 | Δ§5 Environment preconditions (name resolution, lab reachability) | "a precondition failure spends no attempt"; "a precondition failure recovers on its probe" (`precondition_failed`) | M0C | M0 |

## Counts

| Kind | Rows |
|---|---|
| LG (legacy golden) | 5 (A3, A4, A6, B21, B38) |
| N13 ("cannot occur") | 9 (A1, A12, A13, B1, B12, B16, B17, B22, B26) |
| M0C (M0 contract) | 15 (A2, A7, A8, A10, A11, A15, B5, B6, B8, B10, B11, B15, B32, B35, C3) |
| T0F (T0 fault) | 8 (B2, B24, B27, B28, B29, B30, B37, B40) |
| NEW (proposed fixture) | 11 (B18, B20, B23, B25, B31, B33, B34, B36, B39, B41, C2) |
| N/A | 10 (A5, A9, A14, B3, B4, B7, B9, B13, B14, B19) |
| Cross-reference | 1 (C1) |
| **Total** | **59** |
