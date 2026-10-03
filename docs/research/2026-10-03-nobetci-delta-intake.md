# nöbetçi delta intake (2026-10-01 → 03): what we take, and why

Date: 2026-10-03. Source: the nöbetçi supervisor's reports in
`/home/mega/nobetci-dev/docs/handoff/2026-10-03/` (read-only):
- `05-delta-lessons-2026-10-01..03.md` (cited as **Δ§n**);
- `06-gap-audit.md` (**G#**);
- `07-operator-decisions-log.md` (**OD#**);
- `08-incident-test-index.md`.

They were written without knowledge of auto-dev-cycle's new structure (they do cite the m0 notes). The operator asked
the planning session to act as the filter: "we don't have to take everything it says."

**Filter criteria:**
- the lesson is general, not InfraMedic- or lab-specific;
- it closes a failure class the plan or M0 notes leave open;
- its cost is a contract line or a fixture, not new machinery;
- it does not reopen an operator decision.

## Accepted

| # | Item | Source | Where it lands | Why |
|---|---|---|---|---|
| A1 | **Owed input → answering approval → review re-key.** A reviewer's "needs input" is a typed demand with an id. An approval that answers it names the id. Demands and their answers are inputs to the review's fingerprint. Every packet builder uses one collector | Δ§1, Δ§3, G4 | m0/04 `answers`; m0/10 demand/ruling inputs; plan §2.6 | Two live incidents (a ruling never reached the reviewer; a demanded walk never re-keyed). Our contracts had no such concept |
| A2 | **Evidence-job lifecycle.** States, attempts keyed by demand plus a total cap per candidate, restore verified before the next run, environment preconditions, one deadline, remote-process hygiene, receipt content policy | Δ§2, Δ§5, Δ§6, Δ§8, G2, G10, G12 | **new m0/12-evidence-jobs.md**; m0/03 effect kind `remote_process`; m0/06 rules | The biggest missing contract: m0/05 covers units, not evidence producers. Integration (T2) and phase gates depend on it |
| A3 | **Strict gate as profile options on the one gate path:** a two-vendor reviewer pair, deterministic merge, refutation, wait at a vendor limit (never two sessions of one family), `test_ids` checked against per-test results, `proof_receipts`, and an operator-owned **dispositions** schema | Δ§10, G3, OD#71–72 | plan §2.6 "Strict options"; implemented in M5 | Proven in nöbetçi's final-phase gate. "Deferred by an approved record" needed a schema anyway. Off by default, so the simple gate is unchanged |
| A4 | **Broker credit exhaustion is its own reason:** `could_not_run/broker_credit_exhausted`, hold signal balance probe / operator top-up, partial work sealed. Probe the balance before leasing on a broker group. Distinguish rate windows from credit balance | Δ§14, Δ§4, G5 | m0/02, m0/05, m0/09 conformance (402), m0/11 fake provider | It hid an applied candidate for 4 h 22 m. Two incidents in one day |
| A5 | **Plan validator: every acceptance row's evidence is producible by its owner.** A row needing an act outside the builder's reach must name the actor's receipt | Δ§9, G13 | plan §2.7 validators | Stopped tasks three times (09-29 ×2, 10-02) |
| A6 | **Environment facts** (VCS metadata present? network? interpreter) published to builder and reviewer, plus **gate-output ownership rules**: a candidate-owned setup error is a builder repair, a runner fault is a hold | Δ§7 | m0/07 `gate.environments[].facts`; plan §2.5 repair routing | `.git`-less target incident (twice: 09-24, 10-03) |
| A7 | **A review lease needs applicable inputs.** If gate results, receipts or rulings for the exact tree are missing, schedule their producer first; never send an inapplicable input | Δ§13, G16 | m0/05 `placement_checks.inputs_applicable` | F-1-audit reviewers got no per-test results |
| A8 | **Fallback authorization is scoped by role and mode and checked in the call path.** Moving to another family re-checks independence | Δ§4 | plan §2.2 (already re-checks; made explicit) + a fixture | A learning fallback silently ran on a phase review |
| A9 | **Approval records carry `question`, `answer_verbatim`, `supersedes`**; the approval UI renders the exact bytes approved. (A lexical "no new identity in amended text" rule was taken first, then removed after Astra's PR #5 review: authority never derives from prose, and identity changes go through typed actions) | G7, G15, Δ§11, Δ§12 | m0/04 | Supersession lineage was scattered across 78 records; humans approve bytes, not intent |
| A10 | **No blocking prompts for supervisors and the background dev session.** Questions are asynchronous decision packets, non-urgent ones are batched, and the first act after any answer is a liveness check | Δ§15 | plan/02 §6; plan/04 §5 | An F-1.7 stop went unseen for ~2 h while a question blocked the supervisor's turn |
| A11 | **Incident corpus as conformance seed.** Copy `t/fixtures/` real incident artifacts, anonymised, as data-only goldens. Use the incident→test index (206 names) as the checklist for T0 behaviours | G1, 08 | m0/11; M0/M1 work items | The cheapest transfer of 3 weeks of hard-won cases |
| A12 | **Measurement export and stall profile.** A one-shot, read-only export of nöbetçi usage/build/context/terminal data (anonymised) as T1/footprint priors and classifier-eval data. `page_after` defaults come from the measured stall profile (operator-owned holds ≤ 1 h) | G6, G8, Δ§17 | m0/02 `page_after`; M2 work item | Turns priors from guesses into data; the plan already promised "sanitised datasets" without a spec |
| A13 | **Footprint is learned per executor, not per role.** On an identical packet Opus used 9× Astra's input | Δ§17 | plan §2.2 | Directly shapes the scheduler model |
| A14 | **Pre-existing drift** found by a deploy plugin is recorded and routed to its owner, never charged to the candidate | Δ§16 | plan §2.13 | Generic deploy behaviour |

## Deferred (useful later; no change now)

| Item | Source | When |
|---|---|---|
| Local inference hardware facts (DGX Spark memory, KV per token, vLLM flags) as a "local source profile" example | G11 | M2, with capacity footprints |
| A legacy ops-traps runbook (root rollouts via sudo, install ordering, lab traps) | G9 | Generic parts are already invariants. The product/lab traps feed only examples. Revisit when the T2 environment is built (M4/M5) |
| "Scarce reset credits are a resource-group asset" | Δ§16 | M2 scheduler policy |

## Not taken

| Item | Why |
|---|---|
| Lab-specific fixes (cloud-init hosts template, LDAP lab seeding, InfraMedic release-proof contract file) | Product/lab specific. The general lessons (preconditions, receipt content policy, remote-process hygiene) are taken in A2 |
| nöbetçi's per-phase claim counts, floors, model ids | Product numbers. The patterns are taken |
| Codex-specific account checks | Covered generically by A4 (rate windows vs credit balance as separate resource groups) |
