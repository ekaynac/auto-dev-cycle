# 03 — Planning head, task/canon model, reviews of the system, and future plans

Know-how transfer for a new session designing an **open-source, project-agnostic release** of the
auto-dev-cycle framework (codename **nöbetçi**). Written 2026-10-01 from a read-only pass over
`/home/mega/nobetci-dev` (source, git), `/home/mega/nobetci` (installed runtime, `var/`), and the first target
product `/home/mega/inframedic` (product repo, read-only). Nothing was modified. No credentials were read.

Conventions used below:
- `SRC` = `/home/mega/nobetci-dev`, `RT` = `/home/mega/nobetci`, `PRODUCT` = `/home/mega/inframedic`.
- "Operator" = the human owner. "Fable" = the Claude supervisor session that develops the framework.
  "Astra" = independent Codex reviewer (`gpt-6-astra`), and also the primary phase auditor. "Sol" = Codex
  `gpt-5.6-sol`, used for some design reviews. "Opus" = `claude-opus-5-5`, the planner and per-task reviewer.
  It is also the fallback phase auditor.
- Much older prose and code comments are Turkish. Newer docs and code are English.

---

## 0. TL;DR for the designer

1. **Today there is no framework-native planning head.** Planning is split across four places:
   - a product roadmap table that the operator and humans write (`PRODUCT/docs/plans/2026-08-26-vision-roadmap.md`);
   - a one-off, product-specific **task compiler** (`SRC/bin/inframedic-phase1-prepare`) that turned roadmap rows
     into task confs once;
   - a stream of **operator-approved queue-revision specs** (`SRC/ops/task-context/*.json`, 13 kinds, applied by
     `ops/harness-install.py`);
   - a **per-task planner call**: `bin/nb-plan`, run by Opus in Claude Code plan mode. It produces a free-form plan
     plus an optional `nobetci-work-plan` JSON block.

   A framework-native planning head would replace the first three and wrap the fourth.
2. **The task contract is a line-oriented `.conf` file.** Its keys are `kind`, `roadmap_row`, `canon`, `acceptance`,
   `remaining`, `separate`, `closes`, `context_reference`, rails and more. Semantic keys are hashed into a
   **contract digest**. Plans, builds, reviews and receipts are all bound to that digest.
   - The contract of a started task changes only through an operator-approved `task_conf_amend`.
3. **Canon is delivered as content, not as paths.** `context_reference path#anchor` is resolved mechanically
   (`bin/nb_canon.py`) and rendered identically into the plan, build and review prompts (`bin/nb_context.py`).
   This was the biggest measured context win (K-1..K-8, 2026-09-20).
4. **`docs/PLANNING_CANON.md` is the abstract planning doctrine.** It has 16 tool-free principles, distilled from
   painful audits. It is the closest thing to a spec for the future planning head. **Read it first.**
5. **The independent reviews of the framework itself (Astra, Sol, Opus, adversarial Claude subagents) show a
   consistent pattern.**
   - Recurring failures: fail-open validation; edge-case whack-a-mole in identity/carry logic; self-referential,
     builder-authored evidence; non-idempotent recovery; hand-maintained lists drifting apart; verifier failure
     misattributed to the product; and loops with no converging exit.
   - Astra on 09-27: "not suitable for unattended development in its current form".
   - The 09-28 rethink replaced the heavy phase-audit machinery with a **simple subphase gate**. That gate is
     current and closed F-1.5.
6. **The P3 sweeper is the precedent for "use idle/free capacity on low-risk surfaces".**
   - It runs a local Qwen model, and a different model reviews its work.
   - It must reproduce a finding before it fixes it, and every attempt ends in one of four outcomes.
   - It is a "fly" that never blocks the main cycle and opens queue-only PRs. The cycle owns the boundary drain.
   - Results: 101 receipts (29 fixed, 71 escalated, 1 not reproduced). Merge automation is only partly done (slice 1
     plus a dry-run drain).
7. **Open future items**:
   - R53, a stricter F-1 gate (design before F-1-audit);
   - R10 / R10a, lab-deploy recovery (design done, approvals committed);
   - R52, #14 step 6 progress rail;
   - R59, parked sweeper slices, verdict annexes and saga pin;
   - R18, the 6e deferral disposition;
   - R11, silent-loop authority redesign;
   - adaptive builders (exist, almost never used);
   - framework extraction and neutrality (R-11 backlog);
   - a discovery/retro pipeline;
   - Kobra/MCP (product side).

---

## 1. How planning works today (what a planning head must replace)

### 1.1 Layers, from strategy down to one call

| Layer | Artifact | Who writes | Machine-readable? |
|---|---|---|---|
| Vision / canon | `PRODUCT/docs/plans/2026-08-26-vision-roadmap.md` plus ~7 doctrine notes in `PRODUCT/docs/notes/*doctrine*.md` | operator and humans, through product PRs | partly: phase sections, step tables with stable row ids |
| Phase scope | `SRC/etc/phase-audit-inframedic.json` → `phases.<p>.required_steps`, `canon`, `gate` | Fable, through approved installs | yes (JSON) |
| Claim list (simple gate) | `SRC/ops/claims/inframedic/phase-<p>/claims/*.json` plus `generate.py` | Fable/planner drafts; operator decisions | yes |
| Queue (batch manifest) | `RT/var/prepared/<rev>/manifest.json` plus `tasks/<id>.conf` plus `budgets/<id>.conf` | compiler once, then `ops/harness-install.py` revisions | yes; immutable per revision |
| Queue changes | `SRC/ops/task-context/<date>-*.json` plus `SRC/ops/approvals/<date>-*.json` | Fable drafts, operator approves | yes |
| Per-task plan | `nb-plan` output (`~/.claude/plans/*.md`, copied to `RT/var/logs/call-<id>/plan-produced.md`) | Opus planner | prose, plus an optional JSON work-plan block |
| Builder's view of the plan | `.nobetci/plan.md` sidecar, work map, task dossier | controller (mechanical) | — |
| Live backlog for the framework itself | `SRC/docs/WORK_QUEUE.md` | supervisors (Fable/Astra) | prose table |

### 1.2 The planner call — `SRC/bin/nb-plan` (184 lines, bash)

- **Called from** `SRC/bin/nb-cycle` around lines 733–758, in the `plan` stage after `phase_intake`.
- **Plan reuse comes first.**
  - If the state already holds a plan for the same task (sha of the task id or text), the plan is reused without a
    model call (operator ruling 2026-09-09).
  - `NB_PLAN_FILE` forces reuse.
  - Phase-audit-only tasks skip planning (`phase_plan_not_needed`). The phase reviewer supplies scoped repairs.
- **The runtime call** is `claude -p --permission-mode plan --permission-prompts none --disallowedTools Agent
  --model claude-opus-5-5`.
  - It runs under `nb-observe-claude --trace` with wall `NB_PLAN_WALL` (default 1800 s).
  - Plan mode writes the plan to `~/.claude/plans/<slug>.md`. `nb-plan` diffs that directory before and after the
    run to find *this* run's plan, never "the newest".
  - If the working tree is dirty afterwards, the step fails with `plan_wrote`: plan mode must not write.
- **Text the harness appends to every task** (contract rules for the planner):
  - The plan describes only product change, tests and acceptance evidence.
  - It contains no review steps and no model names, and must not ask the builder to run subagents. A GLM builder
    once executed "re-review with Sonnet" as `Agent(model=sonnet)` and leaked a Claude call through OpenRouter.
  - It never writes ledger or review documents.
  - **Default to one builder.** It may append one fenced `nobetci-work-plan` JSON block with 2–3 units (spec in
    `SRC/docs/adaptive-work-plan.md`).
- **Context assembly** happens through `nb_context_prepare plan` → `SRC/bin/nb_context.py:assemble()`. The prompt
  gets, in order:
  1. the authoritative contract and its digest;
  2. the rendered canon excerpts;
  3. role execution text;
  4. plan-specific instructions:
     - anchor every step as `path`, `path:line` or `path::symbol`, or "new: path";
     - name the focused tests (`tests/file.py::test_name`);
     - end with `## Units`.
  5. an "Observed handoff" JSON (data, not instructions);
  6. the step body.
- **Isolation.** `bin/nb_cli_isolation.py` produces `--settings` with `claudeMdExcludes` for every ancestor and
  user-level memory file, and `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` is set.
  - This was measured as needed: a planner had read the supervisor's `WORK_QUEUE.md` via `/home/mega/AGENTS.md`.
  - `--bare` was rejected because it does not read the OAuth subscription login.
- **Continuation.** `bin/nb_plan_checkpoint.py` allows **one** bounded resume of a planner that hit the turn limit.
  - The checkpoint is bound to the task id, contract digest and HEAD.
  - It requires the conf key `planner_resume 1` and settled accounting.
  - An approved `task_conf_amend` sets the old checkpoint aside: it is renamed, never deleted.
- **Accounting and verification.** The call goes through `nb_budget_check`/`nb_budget_charge`, records a correlation
  id before the call ("an unrecordable call is not made"), and is checked with `nb_routing_verify plan … anthropic`.
  Context metrics go to `RT/var/context-metrics/<task>.jsonl`.
- **Measured behaviour** (2026-09-20, `SRC/docs/2026-09-20-context-audit.md`):
  - Plans ran 66–104 turns and opened about 5 documents on their own.
  - After K-2/K-3c (canon as content, cited row first), roadmap reads dropped from 2 to 1.
  - `NB_PLAN_TURNS` is defined but dead (K-9 open).

### 1.3 From plan to builder (mechanical, no model)

- **`SRC/bin/nb_plan_sidecar.py`**: the plan becomes `.nobetci/plan.md` inside the builder's input patch and is
  excluded from the returned patch. First and continuation builds get it. Repairs do not (operator ruling
  2026-09-09). This exists because builds grepped their own session transcript, up to 26 times, to re-read plan
  steps.
- **`SRC/bin/nb_work_map.py`** ("plan anchors / work map"):
  - It parses `path`, `path:12-40` and `file.py::test` references in plan or findings text.
  - It resolves them against the candidate tree to the nearest symbols, and ranks focused tests (own-module tests
    first).
  - Rails: MAX_FILES 12, MAX_TESTS 12.
  - Basis (`SRC/docs/2026-09-16-build-step-analysis.md`): a build whose findings named files reached its first edit
    in 4–5 minutes; one that had to discover the files itself took 41 minutes and 146 shell calls.
- **`SRC/bin/nb_task_dossier.py`** (step 5 / B2, installed 2026-09-30 23:28):
  - It writes `.nobetci/dossier.md` with anchored symbols and their line ranges, test nodes referencing them, the
    candidate diff stat, and failing gate nodes.
  - It never carries plan prose.
- **Shell-read trail plus builder notes** (`bin/nb_build_trail.py`, `bin/nb_build_notes.py`; #14 step 3): later
  rounds of the same task get what earlier rounds read. Notes survive failed calls.
- **Adaptive split.**
  - Components: `SRC/bin/nb_work.py` (parse, MAX_BUILDERS=3, waves) and `SRC/bin/nb-adaptive-build`.
  - Rules: one parent candidate; a normal full gate and independent review on the combined candidate;
    `owned_paths` must not overlap, otherwise units run in sequence.
  - **Barely used.** Only 2 of 51 retained `plan-produced.md` files carry a work-plan block, and WORK_QUEUE R-10
    recorded that "adaptive sub-builders never ran".
  - Design: `SRC/docs/archive/2026-09-12-adaptive-subplans-proposal.md` (approved, max 3).

### 1.4 Task contract format (`tasks/<id>.conf`)

- **Format:** one key per line, `key value`. Keys may repeat. `#` starts a comment.
- **Parser:** `nb_context.fields()`.
- **Semantic keys**, hashed into `contract_digest` (`SRC/bin/nb_context.py:14`):
  `objective, dependencies, artifacts, excluded_paths, expected_outputs, kind, roadmap_row, canon, acceptance,
  remaining, separate, superseded, repair_paths, gate_repair_paths, context_reference`.
- **Rails and flags (not semantic):** `max_rounds`, `max_gate_repairs`, `p3_final_cleanup`, `planner_resume`.

| Key | Meaning | Example (shape only) |
|---|---|---|
| `kind` | `feature` (also `finding_repair` in the manifest) | `kind feature` |
| `roadmap_row` | the product roadmap row id this task delivers | `roadmap_row 1.6-6` |
| `canon` | prose pointer(s) to canon. `<roadmap> row <id>` and section mentions are auto-resolved by `nb_canon.cited_rows()` and `cited_sections()` | `canon docs/plans/…roadmap.md row 1.6-6` |
| `acceptance` | one clause per line. Newer contracts open each row with a stable id (`OB-1.6-007 …`) and name the proving test pattern (`test_ob_1_6_007_<what>`) | — |
| `remaining` | what is explicitly NOT done or needs outside authority, carried verbatim into the closing record | live acts, operator-only actions |
| `separate <sev> <path:line> :: <claim>` | a finding split off as its own RR row (legacy, R-1.3-x era) | `SRC/etc/tasks/R-1.3-2-8.conf` |
| `closes <id>` | ledger/finding ids this task closes | `closes RR-555-5` |
| `answered <file>` / `answer <text>` | controller/operator answers to a reviewer's findings file (legacy) | — |
| `context_reference` | `path`, `path#anchor` or `path:first-last`. Its CONTENT is rendered for every role | `…llmdap-mcp-doctrine.md#Q5` |
| `gate_repair_paths` | where gate repairs may write | `tests/*` |

- **Real examples.** Read the files themselves; they are not reproduced here.
  - `RT/var/prepared/20261001-114607-cycle-lock-no-truncate-jr64jngr/tasks/F-1.6-6.conf` is a feature with OB-rows
    and context references.
  - `…/F-1.6-audit.conf` is a phase audit on the simple gate.
  - `…/F-1.5-kobra-1.conf` is an operator-inserted refactor row.
  - `SRC/etc/tasks/R-1.3-2-8.conf` is the old style with `separate`/`answered`.
- **Manifest task entry** (`manifest.json → tasks[]`):
  - fields: `id, kind, title, ledger, cost_limit, calls{build,plan,review,supervisor}, depends_on, review_turns,
    review_wall, phase_review{phase,audit_index}, limits_env`;
  - optional `resume_branch/resume_head/plan_file/closing_base`.
  - The active manifest has 61 tasks (60 feature, 1 finding_repair) and about 60 top-level keys, most of them
    revision provenance: `task_add_revision`, `contract_amendments`, `finding_carries`, `task_holds`,
    `prerequisites`, `obligation_lists`, `operator_authorizations`, …
- **Contract-change rule.**
  - A started task's contract may change only via `task_conf_amend`. Amendable keys: `roadmap_row, canon,
    acceptance, remaining`.
  - The new conf's sha must be recorded under `manifest.contract_amendments[tid]` with an approval id.
  - Otherwise `nb-cycle` finishes with `task_contract_changed`.

### 1.5 How tasks enter the queue

1. **Initial compile (product-specific, one-off):** `SRC/bin/inframedic-phase1-prepare`.
   - It regex-matches `| 1.x-n |` roadmap table rows.
   - It writes `kind/roadmap_row/canon/acceptance/remaining` lines with generic acceptance text ("Implement and test
     roadmap row X … exactly against its cited canon. Scope text: <row>").
   - It copies limits and writes a new immutable `RT/var/prepared/<rev>/` manifest.
   - It requires an `approval-context.json` and refuses an existing `manifest.json`.
   - **This is the piece a planning head must generalise.**
2. **Queue revisions:** a JSON spec in `SRC/ops/task-context/`, an approval record in `SRC/ops/approvals/`, then
   `ops/harness-install.py --task-context <spec> --approval <rec> [--dry-run|--stage]`.
   - The hold watcher applies staged requests at the next settled boundary, producing a new manifest revision.
     Revisions are never edited in place.
   - Kinds, with counts among the 41 specs:

| kind | count | effect | module |
|---|---|---|---|
| `task_add` | 18 | insert one never-admitted task after `after`; limits copied from `limits_like`; writes manifest entry, conf, budget, authorization identity; ceiling +1 | `SRC/ops/nb_task_add.py` |
| `task_authorize` / `authority_findings` | 1 / 2 | add identities to the batch authorization list. The scope guard reads every `F-/R-/RR-` id in the text as an identity, so prose mentions need authority | `nb_task_add.py` |
| `task_closes` | 1 | add `closes` lines | `nb_task_add.py` |
| `task_conf_amend` | 3 | amend prose contract keys of a started task (approved) | `nb_task_add.py` |
| `finding_carry` | 2 | carry a named review finding (surface + quoted fragments + max severity) instead of repairing it | `nb_task_add.py` |
| `task_context` | 1 | append `context_reference` lines to never-admitted tasks | `SRC/ops/nb_task_context.py` |
| `task_rows` | 1 | append acceptance rows (OB-ids) to never-admitted tasks (`skip_admitted`) | (09-30, `7d86e47`) |
| `task_prerequisite` | 5 | `run_first` X before `held` Y | `SRC/ops/nb_task_prerequisite.py` |
| `task_hold` / `task_release` | 2 / 2 | park a task until a capability exists, or release it | `SRC/bin/nb_task_hold.py` |
| `task_replace` | 2 | replace a task (e.g. F-1.5-build → six tasks) | `SRC/ops/nb_task_replace.py` |

3. **Hard-won rules for drafting tasks** (memory notes plus incidents):
   - **Never ask the builder to edit canon paths.** `nb-accept` rejects with `canon_touched`, and the 09-29
     F-1.5-regression-tests lost two builds that way.
   - **Every R-/RR-/F- id in the text must be in the authority list.** On 09-21 a missing one caused a 4-hour stall
     with `scope_violation`.
   - **Do not make builder-only closure prose an acceptance condition.** It causes `closure_no_progress`. This was
     fixed by deterministic closure text, operator "option A" (`SRC/docs/2026-09-30-deterministic-closure-text.md`,
     `SRC/bin/nb_row_map.py`, profile `closure_row_map`).
   - **Never touch a started task's contract except by an approved amend.**

### 1.6 Canon / context_reference model

- **`SRC/bin/nb_canon.py`** (139 lines) resolves references mechanically.
  - `path#anchor` resolves to:
    - the section under a heading that starts with the anchor (handles `§`, emphasis, `## 8. D6 — …`); else
    - every table row, list item or paragraph whose first cell or bold label equals the anchor (`Q5`, `M4`).
  - `path:a-b` resolves to a line range.
  - Rails: 8 KB per reference and 24 KB total. Anything cut is **named, never silently dropped**.
- **`cited_rows()` / `cited_sections()`** derive references from the `canon` prose, e.g. `<roadmap> row 1.6-6`. The
  cited row is rendered first, with the table's column names, without changing the digest.
- **Failure modes are deliberately asymmetric.**
  - A missing FILE refuses the call.
  - An anchor that no longer resolves is rendered as unresolved and recorded. Moving a heading must not stop
    development; references are verified at install time by `ops/nb_task_context.py`.
- **Provenance.** Hashes stay in `context.json` (`references[]`, `sections` byte sizes, `prompt_sha256`). The model
  sees content.
- **Project tool that generated references from roadmap canon columns:** `SRC/ops/inframedic-canon-references.py`
  (doctrine short name → `docs/notes/*<name>*doctrine*.md`).
- **The `brief` profile key** (`etc/execution-profile.json`) carries project conventions every role needs. It is
  rendered by `nb_context.execution_text()` (K-7b).
- **Rejected (WORK_QUEUE "Not recommended"):** inlining the whole roadmap phase table into every prompt (+6.9 KB per
  call, while planners read it as one ~10 s tool call).

### 1.7 Phase structure (phase-audit profile)

**File:** `SRC/etc/phase-audit-inframedic.json`. Top-level keys:
- `project, intent, foundations` (roadmap, review doctrine, corpus canon), `ledgers`;
- `closure_row_map` (OB-id → test-name convention), `history_globs`, `checks` (`browser, evolution, knowledge,
  blind`);
- `phases`, `layout` (search paths, test dirs, language=python, evidence topics), `ledger_schema` (id pattern,
  severities, closed states, header cells);
- `check_rules` (blind ratio), `blind_mode_legacy_phases`, `blind_scope`.

**`phases.<p>`** has:
- `required_steps`: roadmap step ids. For example 1.3 has 9, 1.4 has 17, 1.5 has `1.5-design` plus 1.5-1..6, 1.6 has
  1.6-1..9, and `1` (the final F-1 audit) covers all 1.1-a…1.7 steps;
- `canon`: a document list;
- optional `check_rules`, `package_regen`;
- `gate`: `{mode: "simple", claims: <glob>, roadmap_rows, receipts: [lab-deploy, browser-walk], environments:
  [spark, edge], retire_tests}`.

The simple gate was set for 1.5 and 1.6 (2026-09-28/30). It is meant for every later subphase. F-1 is to get a
**stricter** gate (R53, not designed yet).

**Phase audit task** (`F-<p>-audit`): its manifest entry has `phase_review{phase, audit_index}`. It is run by
`bin/nb-phase-review` with the `nb_phase_*.py` family. Since 09-30 it uses a fresh session per round plus a
controller digest (`50a20ba`), and is handed the gate's per-test results (`d7ba546`).

### 1.8 The product roadmap and ledger the framework follows (structure only)

**`PRODUCT/docs/plans/2026-08-26-vision-roadmap.md` (v0.3, 1236 lines):**
- §1 Intention (one paragraph).
- §2 Foundational invariants (numbered; "every item judged against these").
- §3 Product surfaces; §4 Deployment topologies; §5 Versioned installation memory.
- **§6 Phases:**
  - Faz 0 … Faz 3. Faz 1 is split into sub-phases 1.1–1.7 plus an inserted "1.5-K".
  - Each sub-phase has a headline paragraph naming its canon, then a **step table**:
    `| <phase>-<n> | **title** — description | canon refs | Console column |`.
  - Sub-plan ids are `<phase item>-<step>` (`1.2-1b`). A "schema numbering rule" covers migrations.
  - §6.5 is the phase-gate protocol (four audits: E2E browser, evolution, cumulative knowledge, blind verification).
  - §6.5b is the layered review protocol; §6.6 the build-session protocol ("each step opened in plan mode").
- §7 Design intents; §8 Brainstorm series (each DECIDED with a PR and a doctrine note); §9 Operator rulings
  (verbatim); §10 Topic register (`| # | Topic | Phase | State | Steps | Canon |`).
- **Doctrines:** `PRODUCT/docs/notes/2026-08-2x-*-doctrine.md` (silent-break, rbac, saga-and-kobra,
  knowledge-center, cross-device-subjects, llmdap-mcp, model-change-resilience). They contain labelled rulings
  (`Q5`, `M4`, `§0.2`) that `context_reference` anchors target.
- **Review doctrine:** `PRODUCT/docs/notes/2026-09-03-development-review-doctrine.md` (layered review; Sol seam
  reviewer; §6.5 four audits).

**Ledgers:**
- `PRODUCT/docs/notes/development-task-ledger.md` is a pipe table:
  `id | sev | row | earliest responsible | claim | coordinator re-verification | disposition | boundary | owner | fix
  range | re-review | final state`.
  - It has one FEATURE row per delivered task, written deterministically by `nb-append`/`nb-ledger-row`.
  - Phase review ledgers (`*phase-1.3-review-ledger.md`) carry `R-…`/`RR-…` finding rows with severities.
- Phase audit packages `phase-1.x-audit.json` (+ `.inventory`, `.pin`, `.snapshot.tar.xz`, `.verdict.json`) are
  **legacy**; the simple gate does not produce them.

**What a framework-native planning head must replace or absorb:**
- the regex row compiler;
- hand-written task_add specs;
- canon-reference generation;
- claim-list drafting (today Fable plus `generate.py`; a planner contract exists in
  `SRC/etc/claims-draft-contract.md` with admission in `bin/nb_claims_draft.py`/`nb_claims.py`, built for the
  abandoned evidence-gate spec);
- the phase `required_steps` binding;
- the PLANNING_CANON checks (acceptance whole before build, instruments before audit, human acts enumerated up front,
  every binding with a change path, every hold with a signal).

### 1.9 PLANNING_CANON.md — the 16 principles (abstract; reuse as-is)

`SRC/docs/PLANNING_CANON.md` was written 2026-09-15 from F-1.3-audit (3 days, 27 review rounds, 10 builds).

| # | Principle |
|---|---|
| 1 | Done is a named artifact a stranger can verify |
| 2 | Write the acceptance contract whole, before the first build |
| 3 | Build the instruments before the audit |
| 4 | Findings enter repair with a reproduction |
| 4a | Evidence names its instrument |
| 4b | A machine-produced check is decided by a rule |
| 4c | Proof is keyed by content (hash), not by round |
| 5 | Shape work to the builder's economics |
| 6 | Requirements are bindable rows in one table |
| 6a | Coverage and verdict are separate completenesses |
| 7 | Records carry a schema |
| 8 | Enumerate human acts up front |
| 9 | Boundaries are executable |
| 10 | Budget rails per phase |
| 11 | Decisions are data with scope and date |
| 12 | Do not audit on a moving floor |
| 13 | Start with audit zero |
| 14 | Measure the cycle, then change it |
| 15 | Every hard binding ships with its change path |
| 16 | No hold without a signal |

Companion docs:
- `SRC/docs/CANON.md` (81 lines): mission priority (target project health > unattended > neutrality), roles and
  truth, "caps are safety rails, not budgets", the dated standing-authorization table, the capability landing path,
  recurring defect classes.
- `SRC/docs/state-vocabulary.md`: state tokens.

---

## 2. Every design/decision doc under `SRC/docs/`, grouped

Status legend:
- **CUR** = current / authoritative;
- **HIST** = historical record, still accurate as history;
- **SUP** = superseded (named by what);
- **PROD** = InfraMedic product decision (not framework).

The `docs/archive/` notes (2026-09-10..13, 09-20..25 queue snapshots) were closed on 2026-09-15 and later. Their
`README.md` gives a per-note closure reason. They are not listed individually except where relevant.

### 2.1 Undated / standing

| Path | Gist | Status |
|---|---|---|
| `docs/CANON.md` | mission priority, roles, caps-as-rails, standing authorizations, landing path | CUR |
| `docs/PLANNING_CANON.md` | 16 abstract planning principles | CUR (core for the open-source design) |
| `docs/WORK_QUEUE.md` | the single live backlog; operator requests R10…R59; context program K-1..K-10; O-items; R-n retained | CUR |
| `docs/adaptive-work-plan.md` | `nobetci-work-plan` JSON schema, ≤3 units | CUR (rarely used) |
| `docs/external-supervision.md` | how an outside supervisor watches (`nb-health`, what is healthy, what to investigate) | CUR (partly stale model names) |
| `docs/state-vocabulary.md` | state tokens | CUR |
| `docs/telegram-notifications.md` | pager/outbox design | CUR |
| `docs/lab/SAMBABOX_LAB_CONTRACT.md` | project-neutral identity-lab contract | CUR |
| `docs/plans/2026-09-22-phase-1.5-task-decomposition.json` | F-1.5-build → six tasks (planning-only decomposition record) | HIST (applied via task_replace) |
| `docs/archive/README.md` + `2026-09-work-queue-done.md` | closed items with proofs | HIST |

### 2.2 Planning / task / context

| Date | Path | Gist | Status |
|---|---|---|---|
| 09-15 | `2026-09-15-audit-duration-analysis.md` | why F-1.3-audit took 3 days; source of PLANNING_CANON | CUR (analysis) |
| 09-16 | `2026-09-16-build-step-analysis.md` | plans and findings that name files → fast first edit; basis for plan anchors and the work map | CUR |
| 09-20 | `2026-09-20-context-audit.md` | what each role is given; K-1..K-9 (canon as content, plan sidecar, isolation) | CUR (mostly installed) |
| 09-20 | `2026-09-20-r1-anchor-tool-task-proposal.md` | first operator-inserted product task (task_add precedent) | HIST |
| 09-22 | `2026-09-22-inframedic-roadmap-health-review.md` | Astra: 1.5 entries were coarse units; plan "partially specified"; design doc not pinned as audit source | HIST (led to decomposition) |
| 09-22 | `2026-09-22-phase-1.5-decomposition-handover.md` | Astra: split 1.5 into design + six slices | HIST (installed) |
| 09-30 | `2026-09-30-deterministic-closure-text.md` | option A: row→test map and "code fixed" lines generated, plus one bounded closure-prose call | CUR |
| 09-30 | `2026-09-30-f16-simple-gate-readiness.md` | F-1.6 claim list (22 claims, 12 blocking), profile `1.6.gate`, checklist, operator decisions | CUR |

### 2.3 Review / gate (phase audit, evidence)

| Date | Path | Gist | Status |
|---|---|---|---|
| 09-14 | `2026-09-14-live-evidence-recipes.md` | phase evidence recipes; SSH observer NOT deployed | HIST/partly CUR |
| 09-18 | `2026-09-18-audit-anchor-review.md` | audit package anchor drift (16/17 gate failures) → R-1 anchor tool | HIST (tool delivered) |
| 09-21 | `2026-09-21-phase-1.4-audit-plan.md` | integrated intake → same-session delta → closure reconciliation | SUP (simple gate; session policy now fresh per round) |
| 09-22 | `2026-09-22-reviewer-browser-walk-design.md`, `…-browser-walk-identities.md` | the reviewer plans the browser walk; the controller executes with named identities | CUR (browser-walk receipt used by simple gate) |
| 09-23 | `2026-09-23-sharded-phase-audit-design.md`, `…-sharded-audit-stage2-design.md` | shard the phase audit into parallel verifiers | SUP (stage-1 modules dormant) |
| 09-23 | `2026-09-23-audit-identity-and-commit-spec.md` | two identities (subject vs artifact) to break the evidence-identity cycle | SUP/HIST (idea still valid) |
| 09-23 | `2026-09-23-repetition-reduction-plan.md` | measured re-review repetition; review identity, carried P3s, byte-bound package | HIST (installed after 6 Astra rounds) |
| 09-25 | `2026-09-25-assembler-activation-plan-sol.md` | 5-step plan to activate the audit assembler | SUP: R12 CLOSED 10-01 (`ops/approvals/2026-10-01-close-r8-r12.json`) |
| 09-25 | `2026-09-25-evidence-run-identity-design-sol.md` | collector-minted run UUIDs plus terminal records | CUR design (partly built; R10 builds on it) |
| 09-25 | `2026-09-25-three-designs-sol.md` | D1 sweeper containment; D2 phase-closure verdict annex; D3 retro blind for 1.1/1.2 | D1 built; D2/D3 parked (R59) |
| 09-26 | `2026-09-26-obligation-gate-design.md` (+3 Astra reviews) | obligation-based blind hard gate (269 obligations) | SUP (simple gate) |
| 09-26 | `2026-09-26-astra-blind-hard-gate*.md`, `…-f15-obligation-list-review.md` | blind as a hard gate; F-1.5 list NOT-FREEZABLE | SUP |
| 09-26 | `2026-09-26-astra-two-day-audit-review*.md` | the audit architecture is unsound for unattended operation | CUR as critique |
| 09-27 | `2026-09-27-astra-unattended-assessment.md` (+brief) | "not suitable for unattended development in its current form" | CUR as critique |
| 09-28 | `2026-09-28-review-gate-rethink.md` + `docs/evidence/2026-09-28-review-gate/` | Astra and Opus independently answer 7 questions; evidence-graph proposal | CUR (reasoning); proposal SUP |
| 09-28 | `2026-09-28-evidence-gate-spec.md` | 8-step evidence gate (claims, oracles, mutation, residue judge, bundle) | SUP by simple gate; claims schema modules and test-integrity guard survive |
| 09-28 | `2026-09-28-simple-phase-gate.md` | **the current subphase gate** | **CUR** |
| 10-01 | `2026-10-01-lab-deploy-recovery-design.md` | R10: receipt predicate, install-complete marker, lease/journal, forward recovery | CUR (R10a approvals committed) |

### 2.4 Builder / exploration / cost

| Date | Path | Gist | Status |
|---|---|---|---|
| 09-09 | `2026-09-09-build14-cagri-incelemesi.md` | anatomy of one long fix call | HIST |
| 09-09 | `2026-09-09-guvenilir-kapanis-ve-token.md` | reliable task closure and token waste (applied) | HIST |
| 09-09 | `2026-09-09-harcama-tavani-onerisi.md` | per-task spend cap proposal (NOT applied) | SUP by CANON §3 |
| 09-15 | `2026-09-15-limits-register.md` | every rail, value, location | CUR-ish (values moved since) |
| 09-17 | `2026-09-17-build-optimisation-effect.md` | measured effect of build-step changes | HIST |
| 09-19 | `2026-09-19-build-continuity-review.md` | build continuity/throughput | HIST |
| 09-20 | `2026-09-20-cycle-time-token-analysis.md` | O-1..O-12: generation 63%, wait 28%; thinking 67% of output | CUR (data) |
| 09-30 | `2026-09-30-builder-exploration-review-cost.md` | B1–B7/P1–P4; 7-step plan (#14); steps 1–5 installed, step 6 = R52 | **CUR** |

### 2.5 Isolation / security

| Date | Path | Gist | Status |
|---|---|---|---|
| 09-07 | `2026-09-07-izolasyon-siniri.md` | isolation boundary design and dry-run plan (root sandbox, mirror.git, nbxfer) | HIST (implemented) |
| 09-08 | `2026-09-08-takim-kararsizligi-ayna-tazeligi.md` | flaky probe suite caused by mirror freshness (harness) | HIST |
| 09-12 | `2026-09-12-interrupted-call-recovery.md` | the only written spec of the recovery engine | CUR |
| 09-15 | `2026-09-15-project-neutrality-audit.md` | ~60 literals, 11 shape overfits, ordered neutrality backlog | **CUR, key for open-source** |

### 2.6 Ops / supervision / handovers

| Date | Path | Gist | Status |
|---|---|---|---|
| 09-07 | `2026-09-07-review-donemi-gozlemleri.md` | first supervised period observation log | HIST |
| 09-08 | `2026-09-08-merge-sonrasi-rapor.md` | first real dev round report | HIST |
| 09-09 | `2026-09-09-durum-dogrulama.md` | read-only status verification | HIST |
| 09-11 | `2026-09-11-cycle-closure-learning.md` | first full cycle closure plus learning (PR titles) | HIST (kept) |
| 09-14 | `2026-09-14-fable-supervisor-handover.md` (+`metrics.json`) | Fable takes over supervision | HIST |
| 09-14 | `2026-09-14-learning-dispositions.md` | standing process rule for learning reports | CUR |
| 09-15 | `2026-09-15-unattended-readiness.md` | stops needing a human, from code; ranked weak points | HIST (basis) |
| 09-17/18/19 | `2026-09-1x-astra-handover.md` | nightly reports Fable → Astra (e.g. 8 h dead service) | HIST |
| 09-19 | `2026-09-19-product-reliability-tasks.md` | product reliability intake (discovery unreachable, LDAP send) | HIST (became tasks) |
| 09-21 | `2026-09-21-astra-review-for-fable.md` | 1 P1 + 4 P2 framework review | HIST (fixed) |
| 09-22 | ~15 Astra/Fable reports (browser, LDAP lab, quorum-2, recovery, edge return, state verification, enterprise-intent handover) | lab and evidence program, mostly InfraMedic adapters | HIST |
| 09-23/24 | Astra correction, final, lab-decision reviews; ledger-pin series | see §3 | HIST |

### 2.7 Routing / providers

| Date | Path | Gist | Status |
|---|---|---|---|
| 09-08 | `2026-09-08-rol-model-atamasi.md` | role→model assignment record | SUP by `etc/roles.conf` and the CLAUDE.md role table |

**Current routing**, from `SRC/etc/roles.conf`:

| Role | Model |
|---|---|
| plan | `claude-opus-5-5` |
| build | `z-ai/glm-5.3-flash` via OpenRouter |
| review | `claude-opus-5-5` |
| seam | `gpt-6-astra` |
| local | `RadixArk/Qwen3.8-27B-NVFP4` @ 127.0.0.1:8000 |
| blind | glm-5.3-flash, inspect route |
| sweep | local Qwen |
| sweep review | glm-5.3-flash |
| forbidden | `z-ai/glm-5.3` |
| supervisor fallback | glm-5.3-flash |

Routing decisions recorded elsewhere:
- Memory notes (not in the repo): "never full GLM 5.3", "sort by price + health, not throughput", "the sweeper stays
  on local Qwen".
- `ops/approvals/2026-10-01-ignore-relace.json` excludes the Relace upstream; R55 watches DeepInfra.
- Phase reviewer: Astra/Codex primary, Opus 5.5 when the Codex limit is hit (R13).

### 2.8 Sweeper

| Date | Path | Gist | Status |
|---|---|---|---|
| 09-16 | `2026-09-16-p3-sweeper-design.md` | the "nanobots" role design | CUR (core) |
| 09-25 | `2026-09-25-sweeper-review-and-architecture-sol.md` | 3 Critical/6 High findings; target architecture; 5-stage plan | CUR (stages 2–5 parked) |
| 09-25 | `2026-09-25-sweep-edge-gate-design-sol.md` | option C: second edge tree for sweeps | CUR design, not built |
| 09-25 | `2026-09-25-three-designs-sol.md` §D1 | containment slice (queue-only PR plus cycle-owned drain) | built |

### 2.9 Dashboard

- No dated design doc. See `SRC/dash/README.md`.
- It is a read-only stdlib page: batch state, holds, queue, tests, review findings, calls/tokens, sweeper page,
  gate/blind/holds tabs (09-28).
- WORK_QUEUE archive row R38: Telegram notifications and the obligation-gate dashboard, 91 dash tests.

### 2.10 Blind / obligation (legacy, superseded by the simple gate)

`2026-09-26-obligation-gate-design.md`, `2026-09-26-astra-*` (blind hard gate, obligation reviews 1–3, F-1.5 list),
`2026-09-28-evidence-gate-spec.md`, and the sharded audit docs. The code stays but is disabled by profile
(`blind_mode_legacy_phases: [1.3, 1.4]`; 1.5/1.6 `gate.mode: simple`). Operator on 10-01: "the old machinery is not
coming back".

### 2.11 Product (InfraMedic) decisions recorded in the framework repo

- `2026-09-08-rr-28-disposition-onerisi.md`, `…-R-TRUST-1-onerisi.md`, `…-trust-rotation-onerisi.md`, `…-v3-ek.md`,
  `2026-09-09-r1328-*.md` (4 docs): R-1.3-x finding dispositions. PROD/HIST.
- `2026-09-24-inframedic-family-brainstorm.md` covers:
  - the observer audit;
  - the root cause of why sagas never ran;
  - model placement;
  - Kobra option B;
  - MCP/export principles.

  PROD, CUR for product direction.

---

## 3. Reviews about the system itself

### 3.1 Verdicts by series (paths under `SRC/docs/`)

- **Early:**
  - `2026-09-20-fable-cycle-review.md`: P1s in host-restart recovery and charged calls without a disposition.
  - `2026-09-21-astra-review-for-fable.md`: 1 P1 (the sweeper destroyed its output directory before reading the
    reply) and 4 P2.
- **Sharded audit / stage 1 (09-23):**
  - `astra-sharded-audit-review`: GO with 14 amendments. Key insight: "the package partly defines what its verifier
    examines" and "writing evidence into the candidate changes the identity that evidence must verify".
  - `astra-stage1-review`: NO-GO.
  - `independent-stage1-review`: GO only for a narrow live path; NO-GO for wiring the assembler into acceptance.
  - `astra-final-review`: GO for a controlled live-path install, NO-GO for acceptance use of
    registry/verdict/assembler.
  - `astra-correction-packet-review`: GO.
  - `astra-review-and-lab-decision`: lab GO, main deploy NO-GO (sweep-deploy P1s).
  - Evidence: `docs/evidence/2026-09-23-{stage1,repetition,audit-history,lab-candidate}/`.
- **Repetition series (09-23, 6 rounds):** NO-GO ×5, then GO for `d0fabbc` under an "advisory-P3 bounded staleness"
  policy. Theme: carry-scope completeness cannot be proven from free text. Astra: closure reconciliation is "a
  reviewer obligation, not a mechanical completeness proof".
- **Ledger-pin series (09-24, 9 rounds):** NO-GO ×8, then GO for `111c94c`. Every round was a new **fail-open** path:
  - an unchecked `.pin.json` exemption;
  - invalid pins treated as a successful no-op;
  - missing or unreadable files treated as "absent";
  - Git failures treated as `pins-ok`;
  - a NUL byte in a path escaping as status 1;
  - writer failures treated as "unchanged".
- **Ledger-fill seam:** Astra NO-GO; then an Opus substitute review (Codex quota exhausted) gave a conditional GO.
- **Sweeper (Sol, 09-25):** 3 Critical findings: a sweep merge moves main under an admitted candidate; there is no
  target-runtime verdict before merge; merge automation is absent. See §4.
- **Two-day audit review (Astra 09-26):** "recovery useful; architecture not sound for unattended operation". Ranked
  Highs:
  - the blind gate measures verifier success, not requirement satisfaction;
  - evidence is coupled to builder-owned packages;
  - `base_broken` overstates causality;
  - recovery is role-specific;
  - repairs have no feasibility contract.
- **Blind as a hard gate (Astra 09-26):** yes, it can be one, but the current gate mixes requirement, product and
  verifier failures (about 29/39 non-passes were verifier-caused).
- **Obligation gate design (Astra, 3 rounds):** NO-GO ×3 (6 High, then 4, then 1 crash-refund defect).
- **Unattended assessment (Astra 09-27):** "not suitable for unattended development in its current form".
  - Reviewer contract drift stopped the cycle twice.
  - Duplicate disputes become operator waits.
  - The installer uses a manual FILES list.
  - `CAPS['repair']` has no caller.
  - Fix: generate the schema, validator and prompts from one route table.
- **Review-gate rethink (09-28)** (`2026-09-28-review-gate-rethink.md`; raw `docs/evidence/2026-09-28-review-gate/
  astra.md`, `opus.md`, `questions.md`):
  - Of 55 phase finding ids across F-1.3/1.4/1.5, only **8 were real blocking product defects**. About 62–64% were
    about the package, citations, ledger or wording.
  - 5 cross-task defects were caught only by the phase audit, so the integrated audit has unique value.
  - The F-1.5 audit cost about 5.6× the build in cash and 5.8× in time.
  - The blind pass: 6,926 calls, $10.69, 60% unknown, because it resent full history every turn.
  - Last 7 days (Opus): 50.9 h running vs about 88 h of gate-machinery stops.
  - The spec reviews (`astra-spec-review*.md`) were GO-with-changes to implement and NO-GO for cutover. They found
    the design Python/Spark-shaped and demanded a **real TS/Go fixture** before claiming neutrality.
  - `astra-simple-gate-review.md`: GO with ≤5 changes (reconcile scope against the roadmap; inspect assertions and
    mocks; require a current receipt; re-read the whole repair diff; route non-builder gaps to their owner).
- **Cost (09-30, `2026-09-30-builder-exploration-review-cost.md`):**
  - The expensive line was the phase reviewer's *resumed* session (474–877 k tokens of carried history per round).
  - In builder repairs, 80% of pre-edit reads re-read files earlier rounds had read.

### 3.2 Recurring NO-GO / finding patterns (09-17 → 10-01)

Counts: 963 commits since 09-17. `RT/var/reviews/` holds 402 per-task review files with P3 299, P2 162, P1 49,
BLOCKER 19.

1. **Fail-open validation.** Errors or missing inputs read as "absent/unchanged/ok": the ledger-pin chain, blind
   retro NO-GOs `1da23ae`/`9a0da32`/`8a1ad77`. This is the CANON's "could_not_run collapsing to empty".
2. **Edge-case whack-a-mole in identity/carry logic.** Each fix closes the reproductions and the next round finds a
   sibling: 6 repetition rounds.
3. **Self-referential evidence.** The builder-authored audit package and inventory produced BLOCKERs repeated across
   rounds (e.g. `F-1.4-audit-001` plus `controller:audit-inventory` in `var/reviews/fix_nobetci_0921-…-r1..r6`).
4. **Provenance not bound.** Declarations are checked rather than recomputed and bound to admitted inputs and charged
   calls (stage-1 P1, ledger-pin `accepted_commit`).
5. **Non-idempotent, role-specific crash recovery** (09-20 P1s, the sweeper, obligation-gate attempt refunds, R-12,
   lab-deploy R10).
6. **Contract drift between hand-maintained lists** (reviewer schema vs validator vs routes; installer FILES list;
   dormant vs live prompts).
7. **Verifier/tooling failure misattributed to the product or builder.** Infeasible repairs are assigned (wrong
   allowed paths, denied commands, canon paths).
8. **Non-converging loops.** Forced re-inspection until a favourable verdict; disputes with no exit; re-freeze on
   `text_issue`; the same blocking finding repeated (`review_fail_3x` exists in `bin/nb-review` for this).
9. **Unattended merge/deploy safety** (sweep merge under a candidate; no edge verdict; review diff truncated at 60 k
   characters).
10. **Per-task product reviews:** recent BLOCKERs are "acceptance (b)/(c) not implemented" and "contracted test file
    missing", persisting across rounds.
11. **Reviewer availability is itself a blocker** (Codex usage limits → Opus takeover; R13/R54).

### 3.3 Architectural opinions (consensus)

**Right:**
- isolated execution (root sandbox, patch in/out);
- authenticated receipts and accounting;
- read-backed citations;
- conservative crash recovery;
- the per-task Opus reviewer (it caught 8 behaviour bugs in 1.5 tasks);
- the integrated phase audit for cross-task defects;
- deterministic generation of anything mechanical (ledger rows, closure maps, packages).

**Wrong / to avoid in the open-source design:**
- builder-authored evidence packages;
- per-item agentic verification loops that resend history (cost must be proportional to what the model decides);
- blind ratio as a hard gate;
- execution-level obligation granularity (269 obligations → group into about 30–60 claims, as the simple gate does
  with ≤25);
- resumed long-lived reviewer sessions (now fresh per round plus a digest);
- the same decision encoded in several hand-maintained places;
- incident-driven rounds instead of integration tests;
- Opus reviewing Opus code has correlated blind spots;
- holds dominate wall-clock time because alarms exist but ownership and recovery do not;
- the core assumes Python/pytest/Markdown/Spark.

---

## 4. The sweeper — precedent for using idle/free capacity on low-risk surfaces

**Design:** `SRC/docs/2026-09-16-p3-sweeper-design.md`, operator 2026-09-16/17.

- **Purpose.** Advisory P3 findings are banked at acceptance (operator ruling 2026-09-20: P3 never blocks and gets no
  cleanup build). A cheap model repairs them asynchronously and must not hide larger problems behind false positives.
- **The role.**
  - It is configured in `roles.conf`: `NB_SWEEP_MODEL` is the local Qwen 3.8 27B NVFP4 on the Spark's vLLM, and the
    reviewer route **must differ** (`NB_SWEEP_REVIEW_MODEL glm-5.3-flash`).
  - History: it was once on paid GLM flash plus full GLM 5.3 review, which cost about $16.8 per day, about 75% of the
    key's spend. The operator then ordered "local Qwen; never full GLM 5.3" (memory: harcama-dagilimi,
    model-ve-rota-kararlari).
- **Input.** A deterministic queue rebuilt from receipts and ledger (`bin/nb_sweeper.py` → `var/sweeper/queue.json`).
  The sweeper never invents work and skips findings whose surface overlaps an open task.
- **The false-positive rail: four outcomes**, each with a receipt in `var/sweeper/receipts/<finding>.json`:
  - `fixed`: a red→green test in the same run, full gate held, diff inside the surface;
  - `not_reproduced`;
  - `escalated`: needs more than a P3 change, goes to planner intake;
  - `out_of_scope`.

  **Reproduce-first:** if it cannot make the defect visible, it must not change code.
- **Bounds.** One finding, one worktree, branch `sweep/<id>`; size rail of 200 lines; turn and wall rails; full gate;
  same sandbox isolation as the builder; fresh session per finding.
- **"Fly" semantics** (operator 2026-09-17: "it may run in parallel; I only forbid it from blocking anything").
  - It yields only to delivery steps in flight (deploy, edge verify, merge, ledger landing, handover) and to live
    boundary leases.
  - It does not yield to a running builder, gate, review or merely pending staged install. Dead writers are ghosts.
  - Code: `blocking_now()`, `wanted_by_others()`.
  - It may be killed, half-done or restarted at any time; lost work is small.
- **Delivery.**
  - Originally: open a `sweep` PR and let humans merge. On 09-22 the operator delegated approvals to Fable;
    15–19 PRs were merged.
  - Incidents:
    - **#604**: a test used `git ls-files`; the edge has no `.git`. A repair build was bought for an unrelated
      failure.
    - **#615**: a merge moved main under F-1.5-3, causing `behind_base`.
- **Sol review and architecture** (`2026-09-25-sweeper-review-and-architecture-sol.md`):
  - Only the cycle's **between-tasks boundary** may merge.
  - Per-finding journal: `queued → reproducing → red_verified → … → merged → delivered → ledger_closed`.
  - A second edge tree for prospective-merge gating.
  - A host **capacity manager** with leases and preemption: `reserve(role, capability, priority, footprint,
    deadline) → lease`. Cycle demand preempts sweep leases, and local-only placement never silently falls back to
    cloud.
- **Status (live `RT/var/sweeper/receipts/`):**
  - 101 receipts: 29 fixed, 71 escalated, 1 not_reproduced.
  - Of 63 escalations analysed on 09-25: 25 exceeded the surface, 17 hit the reproduction wall, 16 failed the gate.
  - Slice 1 + 1b + clean-gate were INSTALLED on 09-25 15:13. The drain hook is in `nb-batch` and ENABLED in dry-run
    (manifest `20260925-153501-sweep-drain`, `required_check receipt:full_gate`).
  - `nb-sweep-deploy` binding is installed: a sweep commit counts only if its reviewed, GitHub-merged PR landed it;
    non-forward deploys are refused.
  - **Parked (R59/R2):** real merge, re-gate of stale heads, edge tree, capacity scheduler.
- **Generalisable lessons for "idle capacity" work:**
  - Low-risk surface only.
  - Deterministic work source.
  - Reproduce-first.
  - An independent reviewer on a different route.
  - A four-outcome vocabulary.
  - Never merge or deploy itself; the cycle owns the boundary.
  - A target-runtime verdict before merge.
  - Durable per-item journal.
  - A capacity lease, not "GPU looks idle".
  - Metering even on free models.
  - Escalation is a first-class result that feeds the planner.

---

## 5. Existing future plans and ideas

| Item | Where | State |
|---|---|---|
| **Assembler activation** (R12) | `docs/2026-09-25-assembler-activation-plan-sol.md` | **CLOSED** 10-01 as superseded (`ops/approvals/2026-10-01-close-r8-r12.json`); R8 standing blind grants closed too |
| **Stricter F-1 phase gate** (R53, operator 2026-09-30) | WORK_QUEUE R53 | OPEN: design plus operator approval before F-1-audit admits; does not inherit R8/R12. The roadmap §6.5 four audits (browser, evolution, knowledge, blind) are the product-side expectation |
| **R10 lab-deploy interrupted-install recovery** (P1) | `docs/2026-10-01-lab-deploy-recovery-design.md` | Design done. R10a (receipt predicate + full-package digest + grant rebind) approvals committed 10-01 (`c442459`, `7a83825`). D-b..D-e (marker, lease/journal, forward recovery, typed `needs_operator`) about 2.5–3 days; Q1–Q6 open |
| **R11 silent-loop authority redesign** | WORK_QUEUE | OPEN "later": live_experiment + restore authority |
| **#14 step 6 progress rail** (R52) | `docs/2026-09-30-builder-exploration-review-cost.md` §5 B6, §6 | OPEN after metrics: "no edit for K turns and ≥2 compactions" settles as a resumable classified outcome; K from data. Steps 1–5 installed (metrics, gate results to reviewer, Bash-read trail + notes, fresh phase-review session + digest, task dossier). Step 7 (B5 step-split for big tasks) conditional |
| **Closure text option A** | `docs/2026-09-30-deterministic-closure-text.md`, `bin/nb_row_map.py`, `bin/nb_closure_text.py` | INSTALLED: deterministic row→test map and "code fixed" lines, plus one bounded closure-prose builder call; R58 absolute-path routing merged, install unverified |
| **Adaptive builders (≤3)** | `docs/adaptive-work-plan.md`, `bin/nb_work.py`, `bin/nb-adaptive-build` | Exists; almost never triggered (2/51 plans). B5 proposes the planner splitting big tasks |
| **Discovery** | task `F-1.1-discovery-unreachable` (spec `ops/task-context/2026-09-25-add-discovery-unreachable-task.json`) | DELIVERED (product). Framework lesson: an absent loop row while its trigger fired is `could_not_run`, not `empty` |
| **Saga** | `docs/2026-09-24-inframedic-family-brainstorm.md` §2; WORK_QUEUE "Operator decisions pending" | Root cause found (scheduler actor not canonical, swallowed); `F-1.2-saga-actor` delivered; saga posture observe_only; `model.saga_proposer` pin parked (R59/R4: Ollama Cloud enforces no schema) |
| **Kobra** | brainstorm §4–5d; operator option **B** 09-24 | 4 rows F-1.5-kobra-1..4 delivered (internal boundary now, physical split after 1.7). MCP export bound to Faz 2, air-gapped, abstract layers by default |
| **R18 roadmap 1.2-6e deferral** | WORK_QUEUE | At F-1-audit, the 1→2 transition must carry an explicit "moved to Faz 2" disposition with named triggers |
| **R59 parked** | WORK_QUEUE | R2 sweeper slices; R3 verdict-annex backfill for 1.3/1.4 (D2); R4 post-delivery re-probe + saga_proposer pin; plus D3 retro blind for 1.1/1.2 ($5 cap design, `three-designs-sol.md`) |
| **Model routing ideas** | WORK_QUEUE O-2, R13/R54/R55; memory notes | Price + health sorting (not throughput); record the upstream per response; compare waits per provider; exclude bad upstreams by approval; never full GLM 5.3; no thinking caps; Codex credits check before phase audits; Opus fallback for the phase reviewer |
| **Capacity scheduler / local model leases** | Sol sweeper doc §B | Proposed: model registry, leases, preemption, measured footprints (Spark is one GPU with unified memory, shared with vLLM) |
| **Context program leftovers** | WORK_QUEUE K-4 remainder (0.9 KB `remaining` boilerplate), K-9 dead rails, K-10 language (measure only), O-10/O-11 deferred | OPEN, low |
| **R-3 repair memory, R-4 coherent interruption/state ownership, R-12 settle-through-normal-path** | WORK_QUEUE §C | OPEN |
| **Dashboard** | `SRC/dash/README.md`, archive R38 | Installed read-only page plus synthwave UI; calibration view needs `--framework-src` in ExecStart; plans: sweeper card, daily Telegram summary |
| **Open-source / neutrality** | `docs/2026-09-15-project-neutrality-audit.md`; CANON §1.3; WORK_QUEUE R-11 ("new-project adapter; off-box backup; framework extraction"); memory "çerçeve tarafsızlığı" (09-28) | Ordered backlog (below); extraction "after InfraMedic Phase 1" (authorization window ends about 2026-10-11) |

**Neutrality backlog** (from the 09-15 audit, still the best map), in order:
1. `nb_paths` with required repo/home, so the framework no longer "runs as InfraMedic when unconfigured".
2. `ledger_schema` + `ledger_glob` with one parser.
3. A delivery adapter (`deploy{}`/`edge{}` in the profile).
4. A test adapter honoured everywhere (permit grammar, sandbox allowlist, gate report).
5. English vocabulary with a compat layer and a schema doc (Turkish tokens like `tuttu`/`olculmedi` are a
   cross-process ABI).
6. Rename the misnamed product adapters.

Shape overfits to remember:
- the markdown-table ledger machine;
- dotted phase ids as substrings;
- the audit package as a test module;
- the Python-only symbol index and gate;
- `gh` as the only delivery channel;
- single repo, ledger and base branch;
- one-machine install scripts.

Astra adds: prove neutrality with a real TS/Go fixture.

---

## 6. Recommended reading order (new session)

| # | Path | Why |
|---|---|---|
| 1 | `SRC/CLAUDE.md` | three tiers (source/runtime/sandbox), cycle stages, roles, invariants already broken and fixed |
| 2 | `SRC/docs/CANON.md` | priorities, caps-as-rails, standing authorizations, capability landing path |
| 3 | `SRC/docs/PLANNING_CANON.md` | the abstract spec for a planning head; tool-free, reusable verbatim |
| 4 | `SRC/docs/2026-09-15-audit-duration-analysis.md` | the data PLANNING_CANON came from: how "done" was discovered one clause per round |
| 5 | `SRC/bin/nb-plan` + `SRC/bin/nb_context.py` (`assemble`) + `SRC/bin/nb_canon.py` | the actual planner call and how contract, canon and handoff become a prompt |
| 6 | `RT/var/prepared/20261001-114607-cycle-lock-no-truncate-jr64jngr/tasks/F-1.6-6.conf`, `F-1.6-audit.conf` and its `manifest.json` (keys only) | real contract and manifest shape |
| 7 | `SRC/ops/nb_task_add.py` (docstring) + 3–4 specs in `SRC/ops/task-context/` | how work enters the queue today; the operator-approval pattern |
| 8 | `SRC/bin/inframedic-phase1-prepare` | the one-off roadmap→task compiler a planning head replaces |
| 9 | `PRODUCT/docs/plans/2026-08-26-vision-roadmap.md` §6 headings + one sub-phase table (e.g. Faz 1.6) | what the input roadmap looks like (structure only) |
| 10 | `SRC/etc/phase-audit-inframedic.json` (`phases`, `layout`, `ledger_schema`, `closure_row_map`) | phase scope as data; what is project profile vs core |
| 11 | `SRC/docs/2026-09-20-context-audit.md` | measured context failures and why canon-as-content, sidecars and isolation exist |
| 12 | `SRC/docs/2026-09-28-review-gate-rethink.md` + `docs/evidence/2026-09-28-review-gate/{astra,opus}.md` | the strongest independent critique of the gate; numbers on waste |
| 13 | `SRC/docs/2026-09-28-simple-phase-gate.md` + `SRC/docs/2026-09-30-f16-simple-gate-readiness.md` | the current gate and how a claim list is drafted from roadmap rows (planning-head output) |
| 14 | `SRC/docs/2026-09-27-astra-unattended-assessment.md` + `2026-09-26-astra-two-day-audit-review.md` | why "unattended" failed; ownership and recovery gaps |
| 15 | `SRC/docs/2026-09-30-builder-exploration-review-cost.md` | the latest measured cost model; plan anchors, dossier, B5 task sizing (a planning-head lever) |
| 16 | `SRC/docs/2026-09-16-p3-sweeper-design.md` + `2026-09-25-sweeper-review-and-architecture-sol.md` | idle-capacity role precedent and its safety architecture |
| 17 | `SRC/docs/2026-09-15-project-neutrality-audit.md` | the extraction map: literals, shape overfits, order |
| 18 | `SRC/docs/WORK_QUEUE.md` (top table + "Not recommended now") | live open items and what not to re-propose |
| 19 | `SRC/docs/2026-09-24-inframedic-family-brainstorm.md` §4–5b (optional) | an example of an operator decision turning into roadmap rows and tasks (Kobra B) |
| 20 | `SRC/docs/2026-10-01-lab-deploy-recovery-design.md` (optional) | the current style of a small, neutral, receipt-predicate design |

---

## 7. Design implications for the open-source planning head (synthesis)

1. **Input: a requirements table, not prose.** Stable row ids, an acceptance sentence and an observable per row
   (PLANNING_CANON §6). Prose sections are doctrine: cited, not verified.
   - Today the regex compiler plus Fable's hand-written specs do this; make it a first-class "compile" step with an
     approval record.
2. **Output per task: a contract with a digest.** It holds acceptance rows with ids, the test-name convention,
   `remaining` (human acts named), canon references as resolvable anchors, rails copied from a class, and
   dependencies and prerequisites.
   - Validate at compile time: anchors resolve, ids are authorized, no canon-path edits are requested, no
     builder-only closure prose, size within the builder's economics.
3. **Per-phase output: a claim list** of ≤25 short testable claims, ≤12 blocking, with source quotes hashed at a
   product ref (`ops/claims/<project>/phase-<p>/generate.py` pattern) and the receipts each claim needs. Also an
   "audit zero" run and the instruments built before the phase opens.
4. **Change management as typed revisions:** add, prerequisite, hold/release, replace, amend, carry, rows, context.
   Each has an approval. Never edit a started contract without an amend. Every binding has a change path; every hold
   names its release signal.
5. **Hand the builder data, not search tasks:** anchors, touch map (B4), dossier, focused tests. Decide task size
   (B5, adaptive units) from measured economics.
6. **Keep the planner unprivileged and isolated:** plan mode, no subagents, no foreign memory, no model names in
   plans. Planner text is data, never a grant.
7. **Avoid the reviewed failure modes:**
   - fail-closed everywhere (four outcomes);
   - one source for every schema and route;
   - deterministic generation for anything mechanical;
   - cost proportional to decisions;
   - fresh sessions plus digests instead of resumed history;
   - verifier failures routed to their owner, not to the builder.
8. **Neutrality from day one:** profile- or adapter-driven paths, ledger schema, test runner, delivery channel and
   vocabulary. Prove it on a non-Python fixture.
