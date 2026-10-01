# 01 — Architecture and components of the auto-dev-cycle ("nöbetçi") — know-how transfer

Audience: a fresh agent session that will turn this framework into an open-source,
project-agnostic first release in a **new** repo/branch. Everything here was read on
2026-10-01 from `/home/mega/nobetci-dev` (source, git HEAD `7a83825`, 1271 commits),
`/home/mega/nobetci` (installed runtime), `/srv/nobetci` (root sandbox), user/system
systemd and `/home/mega/nobetci-dash`. Nothing was modified. Secrets: only key **names**
appear here; no env/credential file was opened.

Legend: **[fact]** read directly from code/config; **[inferred]** my reading of intent or
behaviour that I did not trace end to end; **[stale]** a documented claim that no longer
matches what is on disk.

Key orientation files (read these first in the new session):

| File | Why |
|---|---|
| `/home/mega/nobetci-dev/CLAUDE.md` | Repo guide: tiers, commands, architecture summary, invariants |
| `/home/mega/nobetci-dev/docs/CANON.md` | Mission order, standing dated authorizations, "caps are rails not budgets" |
| `/home/mega/nobetci-dev/docs/WORK_QUEUE.md` | Live backlog (383 lines); InfraMedic-centric |
| `/home/mega/nobetci-dev/docs/state-vocabulary.md` | Cross-process state tokens (Turkish → English migration) |
| `/home/mega/nobetci-dev/docs/2026-09-15-project-neutrality-audit.md` | Prior neutrality audit (starting point for section 8) |
| `/home/mega/nobetci-dev/docs/2026-09-28-simple-phase-gate.md` | Current (simplified) phase-gate design |
| `/home/mega/nobetci-dev/etc/components.json` | Installable component manifest (which file belongs to which component) |

---

## 1. What it is, and the three tiers

**What it is.** nöbetçi ("the one on duty") is an unattended, multi-model software
development cycle for one target git repository. An immutable, operator-approved **batch
manifest** lists ordered tasks with budgets; a Python dispatcher (`bin/nb-batch`) admits one
task at a time and runs a bash state machine (`bin/nb-cycle`) per task: a planner model
(Claude Opus) plans, a cheap builder model (GLM flash over OpenRouter, driven by the Claude
Code CLI) edits code **inside a root-owned sandbox under a separate Unix user** and returns
only a patch, deterministic code accepts the patch (canon boundary, test-integrity guard),
runs the full test suite in isolation and compares it mechanically against a measured
baseline, a fresh Opus reviewer judges the diff (severity policy decided in code), then the
candidate is verified on the real target runtime ("edge"), merged via a GitHub PR, deployed
over SSH, recorded as a delivery, and a **deterministic** ledger row is written, gated,
merged and verified on `origin/main`. Integrated "phase audits" are verified by a second
reviewer family (Codex `gpt-6-astra`, fallback Claude Opus 5.5). Every decision is code; a
model never decides stop/continue, severity, or authority. All state is append-only JSONL /
hash-bound JSON under `var/`; every model call is correlated before egress and charged to a
per-task budget. It was built while developing exactly one product (InfraMedic, a Python
infra-diagnostics app at `/home/mega/inframedic`) and still carries that coupling (section 8).

**Three tiers [fact, CLAUDE.md + filesystem]:**

| Tier | Path | Owner / writer | Contents | Why separate |
|---|---|---|---|---|
| Source | `/home/mega/nobetci-dev` | developer (git) | `bin/` (framework), `ops/` (installers, rollouts, adapters, approvals, specs), `etc/` (templates), `t/` (tests), `docs/`, `dash/` | Editing source must never change the live cycle |
| Runtime | `/home/mega/nobetci` | `mega`, but only via staged/hash-bound installers | installed `bin/`, `etc/` (roles.conf, profiles, locks, `active-batch`, 0600 env files), all state under `var/` (immutable manifests in `var/prepared/<rev>/`, receipts in `var/maintenance/`) | A running bash script must never be edited under itself; every installed file is sha256-bound in the active manifest; installs happen at idle boundaries with receipts |
| Root sandbox | `/srv/nobetci` | `root` (rollouts in `ops/`) | `bin/nb-sandbox-unit` (only sudo-able binary), `bin/nb-run-worker`, `bin/claude` (pinned 215 MB CLI binary), `bin/nb-observe-claude`, `bin/nb-builder-key-probe`, `builder/` (root-only `provider.env`, `credentials.json`), `venv/` (read-only gate venv), `mirror.git`, `runs/` (`mega:nbxfer`, setgid), `status/` | The builder (untrusted model output, its tests, `pytest`, `conftest.py`) runs as `nbbuild`, cannot read controller keys or host git history, cannot touch `$HOME` of `mega`; only a patch crosses back |

Identities [fact]: `mega` (uid 1000, operator + controller, member of `nbxfer`),
`nbbuild` (uid 1001, home `/srv/nobetci/builder`, shell nologin), group `nbxfer` (gid 1001:
`nbbuild,mega`). sudoers: `mega` may run **only** `/srv/nobetci/bin/nb-sandbox-unit` as root,
NOPASSWD. The cycle re-execs itself under `sg nbxfer` if the session lacks the group
(`nb_require_xfer_group` in `bin/nb-env`; cron/timer sessions historically lacked it — see
memory note "Zamanlanmış giriş grup farkı").

---

## 2. The cycle pipeline (`bin/nb-cycle`, 1664 lines bash)

Body is wrapped in `main(){...}; main "$@"` because bash reads scripts incrementally (an edit
during a run corrupted a run on 2026-09-05). Exit codes: `0` done · `1` stopped by a
**decision** · `2` could not run. Invoked with no argument and an `etc/active-batch` present,
it `exec`s `nb-batch run` instead. Env switches: `NB_DRY=1` (no merge/deploy, but still
touches edge), `NB_DEV_ONLY=1` (mechanical stop before edge), `NB_RESUME_BRANCH`,
`NB_PLAN_FILE`, `NB_MAX_ROUNDS` (default 3), `NB_GATE_ISO` (default 1; 0 requires recorded
authority), `NB_GATE_SCOPE`, `NB_FIX_ADDENDUM`, `NB_CLOSING_DRAFT_BASE`.

### 2.1 Stages in order [fact, `stage=` assignments]

| # | `stage=` | Command(s) | Consumes | Produces / typical terminal reasons |
|---|---|---|---|---|
| 0 | (auth) | `nb_auth_valid` (nb-env) | `etc/authorization` (scope/owner/finding/allow/expires/granted/verbatim lines) | `auth_scope_exhausted`, `auth_scope_ambiguous` (more than one live `scope` block → fail closed); escalation at most once/day |
| 0a | `devam` / `deployment_recovery` | resume logic, `nb-ledger`, `nb-task-status` | `var/deliveries/*.json`, `var/candidates/*.json`, `state.json` | resume order: delivered-but-ledger-pending → candidate on branch → new task; `resume_delivery`, `resume_ledger`, `resume_candidate`, `awaiting_operator` |
| 0b-0d | (identity/context) | `nb-task-context`, `nb-context` | task conf `etc/tasks/<id>.conf` (from the manifest revision) | `task_context_bound`, `task_conf` steps |
| 0c | (isolation) | — | `NB_GATE_ISO` + authority | `isolation_required` |
| 1 | `preflight` | `nb-rebase-candidate` (resume only), `nb-preflight` | repo, origin, tools | `candidate_rebased[_with_markers]`, `candidate_rebase_conflict`, `preflight_failed` (log kept in `var/logs/preflight-*.log`) |
| 2 | `branch` | `git fetch`, branch `fix/nobetci/<MMDD-HHMM>-<run6>` | clean tree | `dirty_tree`, `fetch_failed`, `cycle_resume`, `resume_branch_empty` |
| 2b | `phase_intake` | `nb-review` (phase tasks only) → `nb-phase-review` | phase manifest keys, evidence backlog `var/phase-sessions/<task>/evidence-backlog.json` | `awaiting_external_evidence`, `phase_intake_pending`, `phase_evidence_invalid` |
| 3 | `plan` | `nb-plan` (Opus, plan mode) — or reuse | task text + conf; contract digests (`plan_contract_sha`, `plan_semantic_sha`) | plan file in `var/planner/`; `plan_reused`, `task_contract_amended`, `task_contract_changed`, `plan_failed`, `budget_exhausted` |
| 4 loop r=1..max_rounds | `build/<r>` | `nb-build` (or `nb-adaptive-build`, ≤3 sub-builders); `nb-answer` if builder stopped on a prose question | plan, task, fix addendum (`nb-fix-input`), dossier/notes/trail | patch applied to branch; `build_changed`, `build_no_changes`, `build_failed`, `errored_build_continuation`, `partial_candidate_validation`, `resume_verify_only` |
| 4a0 | (ledger pre-commit) | `nb-ledger-row`/`nb-append` pre-commit | delivery fields | `ledger_precommitted` (ledger row committed on the product branch before review) |
| 4a1 | `package_regen/<r>` | `nb-package-regen` (worker `regen` mode) | phase package path from profile | regenerated phase-audit package; `package_regen_*` |
| 4a | `kabul/<r>` (accept) | `nb-accept` | `git diff origin/main...HEAD` as a patch | `var/accept/<id>.json` (tree, decision, test-integrity verdict); `canon_touched`, `test_integrity` (P1, unbankable) |
| 4b | `kapi/<r>` (gate) | `nb-iso run gate` + `nb-gate-iso --head` (+ `nb_gate_results.py`, `nb_base_broken.py`, `nb-gate-input`) | patch + base; `etc/baseline.lock` | gate log + report bound to HEAD+tree; `gate_iso_clean/deviation/undetermined`, `gate_repair_pending` (repair packet in `var/gate-repairs/<task>-<head>.json`), `repair_no_progress` (3rd identical signature), `gate_repair_scope_required`, `gate_diagnostic_required` |
| 4c | `inceleme/<r>` (review) | `nb-closing-draft` → `nb-review` (Opus fresh worktree; same-task re-review resumes reviewer's own session); for phase tasks + `nb-phase-review` | closing draft `var/closing-<run8>-r<r>.md` (+`.context.json`, `.rowmap.json`) | findings JSON; `review_clean`, `review_blocking`, `review_no_progress`, `review_fail_3x`, `closure_material_missing`, `review_failed`, `awaiting_external_evidence` |
| 4d | `closure_text/<r>` | `nb-closure-text` → `nb-build` with `NB_BUILD_CLOSURE_OUT` | closing draft + closure-only findings | rewritten closure prose (no file may change); `closure_no_progress` |
| 5 | `final_cleanup` | `nb-final-cleanup` (opt-in per task conf `p3_final_cleanup`) | banked P3 | one P3 cleanup ± one test repair; `final_cleanup_pending` |
| — | `devir` (handover) | `nb-append` under `NB_DEV_ONLY=1` | — | `dev_only_stop` |
| 6 | `edge` | `nb-edge` (unless `edge_verify_tree` == HEAD tree) | `etc/baseline-edge.lock`, profile `delivery.edge` | `edge_reused`, `edge_deviation`, `edge_repair_pending` (same repair packet kind=edge via `nb-edge-input`), `edge_unreachable` (hold watcher resumes later) |
| 7 | `merge` | `nb-merge origin/main` (gh PR create + rebase merge) | gate/review verdicts for exact HEAD, authorization | `merged`; `merge_refused`, `merge_failed`; `NB_DRY` ends here `dry_run` |
| 8 | `deploy` | `nb-deploy` (ssh to edge, product `deploy/bin/deploy-from-dev.sh` + `verify-deploy.sh`, optional `post_apply`) | merged sha | `deployed`, `deploy_refused`, `deploy_failed`, drift classification (`nb_drift.py`) |
| 9 | `teslimat-kaydi` (delivery record) | `nb_delivery_write` | state product fields | `var/deliveries/<task>.json` (the product proof); `delivery_record_failed` |
| 10 | `ledger` | `nb-append` → `nb-ledger-row` (no model) | delivery record + reviewed closing draft | ledger branch; `ledger_pending` |
| 11 | `ledger-kapi` | `nb-iso run gate` + `nb-gate-iso --head` on the ledger branch (`nb_ledger_retry.py` one retry) | ledger patch | `ledger_pending` |
| 12 | `ledger-inceleme` | mechanical (`nb-ledger-row --fill` receipt) or `nb_ledger_review` | | `ledger_review_mechanical` |
| 13 | `ledger-merge` | `nb-merge` again | | escalation if ledger missed main |
| 14 | `tamamlanma` (completion) | `nb-task-status <task> --require-pr <pr>` | `origin/main` ledger | `cycle_done`, `awaiting_operator` (row landed but `open`), `ledger_pending` |

After the cycle, `nb-batch` runs **learning** (`learn()` → `nb-supervise` → Codex
`nb-supervisor-call`, fallback `nb-supervisor-fallback-call`) and `nb-cycle-audit`, then
settles accounting and admits the next task. Learning only records observations
(`var/learning-decisions/`), never edits the framework.

Between rounds the cycle checks `bin/nb_maintenance.py requested()` and exits with
`could_not_run/maintenance_boundary` to let a staged install run (most common terminal: 70
of 351).

### 2.2 Records: steps vs terminals [fact, `bin/nb-env` `_nb_write`]

- `var/steps.jsonl` (`nb_step`): any number per run. 9883 lines, 128 distinct `reason`s.
- `var/loop_runs.jsonl` (`nb_record`, written by `finish()`): **exactly one** terminal per
  run, `kind=terminal`. 351 lines, 44 distinct reasons. Heartbeat/health read only terminals.
- Line shape: `{"at","run","kind":"step|terminal","outcome","reason","detail"}`;
  `outcome` ∈ **`found | empty | could_not_run | failed`** — the "four outcomes" invariant
  (`_nb_write` rejects anything else).
- `var/calls.jsonl` (`nb_correlate`): a call id is appended and fsynced **before** egress;
  if it cannot be written the call is not made (invariant #3). 759 lines.
- `var/state.json` is scratch; `var/candidates/<branch>.json` snapshots candidate fields;
  `var/task-runs/<task>.json` per-task run status.

Most frequent terminal reasons on disk (count): `maintenance_boundary` 70,
`awaiting_external_evidence` 56, `cycle_done` 55, `build_failed` 22, `phase_intake_pending`
18, `preflight_failed` 16, `review_failed` 12, `deploy_failed` 9, `ledger_pending` 9,
`review_blocking` 8, `build_no_changes` 6, `baseline_deviation` 5, `scope_violation` 5,
`closure_no_progress` 5. Full step vocabulary: `python3 - <<<'...'` over `steps.jsonl`
(examples: `budget_ok, patch_accepted, gate_iso_clean, ship_refused, task_context_bound,
context_metrics, call_trace_saved, build_changed, review_mandate, build_metrics,
build_notes_recorded, ledger_precommitted, gate_failures, deployed, delivery_recorded,
supervisor_fallback_used, verify_drift, preexisting_drift, repair_signature_repeated`).

State tokens: `held / deviated / unmeasured / unreadable / partial:* / CAP` (English since
2026-09-15; Turkish predecessors still read — `docs/state-vocabulary.md`, `bin/nb_vocab.py`).

---

## 3. Batch / manifest model

### 3.1 Manifest [fact, `bin/nb-batch` class `Batch`]

`etc/active-batch` holds one absolute path: `var/prepared/<YYYYMMDD-HHMMSS-label-rand>/manifest.json`
(290 revisions exist; it changed again to `20261001-142818-abs-product-path-…` while I was
reading). The revision directory also holds `tasks/*.conf`, `authorization`, `approvals/`,
`grants/`, `budgets/`, `reviewer/`, `obligations/`, `learning/`, `queue.md`, per-task logs,
`state.json` (batch state; `manifest_sha` must match → "batch contract changed; explicit new
grant revision required").

Validated fields (selection): `schema:1`, `status:"APPROVED"`, `starts_at/expires_at`
(authority window), `home`, `var`, `repo`, `cost_basis` ∈ {`combined_estimate_and_reserve`,
`openrouter_estimate`, `provider_key`} (live: `provider_key` — dollar figures are telemetry,
not a gate), `cost_limit`, `max_tasks`, `bindings` {abs path → sha256} (every installed
`bin/` file, profile, approval… re-hashed on `authority()`), `supervisor` {cli, model, effort,
wall_seconds, reserved_usd_per_call}, `phase_reviewer` {…, `fallback` {provider claude_code,
model, cli, effort, wall_seconds}}, `task_context_version` 0|1, `candidate_only`, `baseline`
(per-task usage carried from earlier revisions), plus revision lineage keys
(`supersedes_manifest`, `revision_reason`, `source_commit`, `harness_install_snapshot`,
`rollouts_accepted`, `operator_authorizations`, `*_revision` blocks naming spec path + sha +
approvals).

Task entry: `id` (regex `(?:R|RR|F)-…`), `kind` ∈ {`finding_repair`, `feature`,
`review_only`, `human_gate`, `seam_gate`}, `title`, `cost_limit`, `calls` {build, plan,
review, supervisor} (all four required), `depends_on` (earlier ids only), `ledger` (relative
path matching the profile ledger glob; required for `feature`, which must start `F-`),
optional `phase_review` {audit_index, strategy legacy|review_first} (requires supervisor ≥12
calls), `resume_branch`+`resume_head` (exact 40-hex), `plan_file`, `closing_base` (must be
bound), `review_turns`, `review_wall`, `limits_env` (e.g. `NB_BUILD_WALL`). Live manifest: 61
tasks (1 `finding_repair`, 60 `feature`: F-1.3-* … F-1.7-release, F-1-audit).

Policy changes land as a **new manifest revision**, never an edit; old revisions stay.

### 3.2 Task conf (`<rev>/tasks/<id>.conf`, copied to `etc/tasks/`) [fact]

Line-oriented `<key> <value>`. Keys observed across the 61 live confs: `acceptance` (175),
`remaining` (120), `p3_final_cleanup`, `max_rounds`, `max_gate_repairs`, `gate_repair_paths`,
`canon`, `roadmap_row`, `kind`, `context_reference` (57; `path#anchor` canon-by-content),
`closes` (5), `planner_resume` (1); older confs also `separate`, `answered`, `answer`,
`repair_paths`, `superseded`. `nb-cycle` hashes `canon|acceptance|superseded|repair_paths|gate_repair_paths`
into `plan_contract_sha`; `nb-context digest` gives `plan_semantic_sha`.

### 3.3 Budgets [fact]

- Per-task counters `var/budget/<task>.json`: `calls` {plan, build, review, supervisor},
  `cli_cost_usd`, `openrouter_est_usd`, `provider_est_usd`, `tokens_in/out`, `api_sec`,
  `unmeasured`, `runs`, `charged_calls`, `interrupted_calls` (append-only). Never reset.
- `nb_budget_check <role>` (nb-env) returns 0 ok / 1 exhausted / 2 undeterminable; the cycle
  maps these to `budget_exhausted` / `budget_undeterminable`.
- `etc/budget.conf` (legacy, R-1.3-2-8 era) — task-wide caps + `grant_at`/`wall_sec`.
- Caps are raised through `ops/limits/*.json` + approval (`--limits`); CANON §3: caps are
  rails, not budgets.
- Admission records `var/admissions/`, accounting owner `bin/nb_accounting.py`.

### 3.4 Staged installs (two mechanisms) [fact]

1. **Harness install** — `ops/harness-install.py` (962 lines, runs from SOURCE, never from
   `<home>/bin`): `--dry-run` prints a change-set digest → `--stage --expect-changes <digest>`
   writes `var/staged-harness-install.json` + `var/maintenance-request.json`
   (operation `staged-harness-install`, TTL) → the cycle exits at the next round boundary
   (`maintenance_boundary`) → `nb-hold-watch` runs `harness-install.py --apply-staged` when
   idle → new manifest revision + receipt dir `var/maintenance/<stamp>-<label>/`
   (`before/`, `after/`, `receipt.json`) → batch resumes. `--withdraw-staged` spends it.
   Idle = no unsettled call, no sandbox worker, batch/cycle locks free.
2. **Phase install** — `var/pending-phase-install.json` (status `ready`, `installer_sha256`)
   applied by `bin/nb-apply-staged-phase` from `bin/nb-run-active-batch` at service start
   (binds `etc/phase-audit-inframedic.json` by sha — product name hardcoded).
3. **Root / owner rollouts** — `ops/install-*.py`, `ops/rollout-observed-worker.py`, take the
   boundary through `ops/nb_boundary.py` (leases in `var/maintenance-leases`, honoured by the
   hold watcher), change bound files + manifest as one transition (`ops/nb_bound_rollout.py`),
   write a receipt under `var/maintenance/`; a later harness install accepts it with
   `--accept-rollout`. `ops/run-operator-rollouts.sh` = one sudo entrypoint for pending
   privileged steps.

`harness-install.py` flags (each binds into the new revision): `--files` (installed under
`<home>/bin` by basename; checked against `etc/components.json` + import closure),
`--evidence-job` (grants from `ops/evidence-jobs/*.json`), `--approval` (records from
`ops/approvals/`), `--task-context <spec>`, `--limits`, `--reviewer-config`, `--profile-key`
(PROFILE_KEYS = `instructions, brief, p3_cleanup, gate_command, canon_definition,
commit_guard, phase_evidence_command, delivery, browser_walk, pager, test_integrity,
build_runaway, gate_results`), `--baseline-lock` (approval must name operation
`relock_gate_floors`, spec and exact floors), `--extend-authority`, `--obligations` +
`--obligation-items` + `--obligation-amendment`, `--interrupted-disposition` (approval with
operation `dispose_interrupted_call`), `--accept-rollout`, `--service`, `--label`,
`--reason`.

### 3.5 Task-context spec kinds (`--task-context`, specs in `ops/task-context/`, 40 files) [fact]

| `kind` | Module | Effect (manifest key written) |
|---|---|---|
| `task_context` | `ops/nb_task_context.py` | add `context_reference` lines to never-admitted task confs (`task_context_revision`) |
| `task_rows` | `ops/nb_task_context.py` | append `acceptance <row id>` to never-admitted confs (`task_rows_revision`) |
| `task_add` | `ops/nb_task_add.py` | insert ONE new task behind everything started (`task_add_revision`) |
| `task_authorize` | `ops/nb_task_add.py` | authorize a queued task (`task_authorize_revision`) |
| `authority_findings` | `ops/nb_task_add.py` | extra `finding <ID>` lines into the authorization only |
| `task_closes` | `ops/nb_task_add.py` | `closes <ID>` lines into one conf |
| `task_conf_amend` | `ops/nb_task_add.py` | replace one prose line of one conf (`contract_amendments`; cycle drops the retained plan and replans on the kept candidate) |
| `finding_carry` | `ops/nb_task_add.py` | carry one P2/P3 finding of one task (`finding_carries`) |
| `task_replace` | `ops/nb_task_replace.py` | replace a never-started task by an ordered chain |
| `task_prerequisite` | `ops/nb_task_prerequisite.py` | queued task runs before a held one (`prerequisites`) |
| `task_hold` / `task_release` | `bin/nb_task_hold.py` | hold/release a queued task (`task_holds`, `task_releases`) |

Other revision-level kinds: `limits`, `reviewer_config`, `profile keys`, `baseline_lock`,
`extend_authority`, `obligations`, `interrupted_usage_dispositions`, evidence-job grants
(`phase_evidence_jobs`), `framework_corrections`, `phase_review_route_amendments`,
`phase_review_routes`.

### 3.6 Approvals [fact]

70 records in `ops/approvals/*.json`. Common keys: `schema, kind, approved_at, scope,
user_response` (operator's verbatim words), `question_asked, conditions, measured, statement,
answers, acts, operation, expires_at`. Kinds: `operator_authorization` 49, `operator_ruling`
12, `bind_phase_evidence_grants` 3, and one each of `waive_one_learning_call`,
`archive_restore_final_cleanup`, `framework_correction`, `relock_gate_floors`,
`supervisor_learning_fallback_route`, `dispose_interrupted_call`. Approvals are what makes a
revision legal; CANON §4 lists the standing ones.

---

## 4. Roles and model routing

### 4.1 Configuration [fact]

- `etc/roles.conf` (source template, Turkish comments, 127 lines) → installed
  `/home/mega/nobetci/etc/roles.conf`. Read by `bin/nb-env` `nb_role_model <VAR>`; one
  variable per role, no automatic fallback without recorded authority. Installed via
  `ops/install-role-keys.py` (owner rollout) and, for builder routing keys,
  `ops/install-build-provider.py`.
- Live role variables (no secrets live here):

| Variable | Live value | Used by |
|---|---|---|
| `NB_PLAN_MODEL` | `claude-opus-5-5` | `nb-plan` |
| `NB_BUILD_MODEL` / `NB_BUILD_BASE_URL` | `z-ai/glm-5.3-flash` / `https://openrouter.ai/api` | `nb-build` record + gate; real env from root `provider.env` |
| `NB_BUILD_SORT`, `NB_BUILD_MAX_PROMPT_PRICE`, `NB_BUILD_MAX_COMPLETION_PRICE`, `NB_BUILD_IGNORE_UPSTREAMS` | `price`, `0.15`, `0.50`, `Fireworks,BaseTen,OpenInference,Relace` | runtime-only keys (not in source template) written by `ops/install-build-provider.py`; turned into OpenRouter `provider` prefs |
| `NB_REVIEW_MODEL` | `claude-opus-5-5` | `nb-review` |
| `NB_SEAM_CLI` / `NB_SEAM_MODEL` / `NB_SEAM_EFFORT` | `codex` / `gpt-6-astra` / `high` | `nb-seam`; manifest `supervisor`/`phase_reviewer` carry the same route |
| `NB_LOCAL_MODEL` / `NB_LOCAL_URL` / `NB_LOCAL_EFFORT` | `RadixArk/Qwen3.8-27B-NVFP4` / `http://127.0.0.1:8000/v1/chat/completions` / `medium` | `nb-answer`, `ops/nb-blind-verify.py` local route, sweeper local route |
| `NB_BLIND_*` (MODEL, URL, PROVIDER, SORT, ROUTE, GROUP, TREE_CEILING_USD) | flash, OpenRouter, `price`, `inspect`, `1`, `15` | blind verifier (legacy) |
| `NB_SWEEP_MODEL` / `NB_SWEEP_REVIEW_MODEL` | local Qwen / `z-ai/glm-5.3-flash` | `nb-sweep` (must differ; refuses otherwise) |
| `NB_FORBIDDEN_MODELS` | `z-ai/glm-5.3` (full model banned by operator) | `nb-sweep` refuses |
| `NB_SUPERVISE_FALLBACK_MODEL` / `_SORT` | flash / `price` | learning second route |

- Secret key **names** (values only in 0600/root files): builder `ANTHROPIC_AUTH_TOKEN`
  + `ANTHROPIC_BASE_URL` in `/srv/nobetci/builder/provider.env` (root-only; generated from
  roles.conf by `var/setup-builder-openrouter.sh` [fact per comment]); fallback OAuth copy
  `/srv/nobetci/builder/credentials.json` (`nb-builder-cred`; unused when the token is set);
  `NB_BLIND_API_KEY` in `etc/blind-provider.env`; `NB_CONSOLE_PASS` in `etc/console.env`;
  `NB_ALERT_SINK_SECRET` in `etc/alert-sink.env`; lab: `NB_LAB_KEY`, `NB_LAB_SECRETS`,
  `NB_TARGET_SECRETS` (paths). Also referenced: `OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY`,
  `NB_BUILD_API_KEY`, `CODEX_HOME`. Claude/Codex subscriptions use the operator's own CLI
  logins (`~/.claude`, `~/.codex`).
- Price table `bin/nb_prices.py`; OpenRouter endpoint ordering `bin/nb_provider_pref.py`
  (sort, never pin).

### 4.2 Role → route today

| Role | Entrypoint | Model / CLI | Where it runs | Session policy |
|---|---|---|---|---|
| Controller | `nb-batch`, `nb-cycle` | none (code) | host as `mega` | — |
| Planner | `nb-plan` | Claude Code CLI `--model claude-opus-5-5 --permission-mode plan --disallowedTools Agent`, subscription | host, product repo, `nb_cli_isolation.py` settings strip foreign CLAUDE.md/memory | plan reused while task contract unchanged; turn-limit continuation via `nb_plan_checkpoint.py` |
| Builder / fix builder | `nb-build` → `nb-iso run build` | sandbox `/srv/nobetci/bin/claude -p --permission-mode acceptEdits --permission-prompts none --disallowedTools Agent`, `--allowedTools` = venv pytest + read-only shell (grep, rg, ls, cat, sed, find…), PreToolUse Bash policy hook (`nb_builder_bash_policy.py`), model GLM 5.3 flash via OpenRouter (Anthropic-shaped API) | `nbbuild` systemd unit `nb-run-<id>` | throwaway per call; plan passed as full text; state carried via notes file, build trail, dossier |
| Adaptive sub-builders | `nb-adaptive-build` (planner work units, `nb_work.py`) | same as builder, ≤3 (`execution-profile.adaptive_builders`) | sandbox | one parent candidate, normal final gates |
| Reviewer / re-reviewer | `nb-review` | Claude CLI `--model claude-opus-5-5 --permission-mode plan`, subscription | host, **detached git worktree**; main-tree fingerprint compared before/after | first review fresh; same-task re-review resumes reviewer's own session (ruling 2026-09-09) |
| Phase auditor | `nb-review` → `nb-phase-review` → `nb-supervise` → `nb-supervisor-call` | Codex CLI `gpt-6-astra` effort high ("Astra"); fallback `claude_code` `claude-opus-5-5` (was `claude-fable-5-1`), handover when Codex hits usage limit (`nb_audit_recovery.py`, `nobetci-audit-recovery.timer`, route records `phase_review_routes`) | host; read-only inspection via one confined MCP tool (`nb_audit_tools.py`), Landlock scratch (`nb_scratch_sandbox.py`) | fresh session per round + controller digest of previous round (`nb_phase_digest.py`, since 2026-09-30) |
| Learning supervisor | `nb-batch learn()` → `nb-supervise` | Codex primary; fallback flash via OpenRouter (`nb-supervisor-fallback-call`, approval 2026-09-18) | host | one compact packet per delivered task |
| Ledger writer | `nb-append` + `nb-ledger-row` | **none** (deterministic since 2026-09-09) | host | — |
| Tool permission | `nb-permit` (MCP stdio server, `etc/mcp.json`) | none; default-deny allowlist, every decision → `var/permits.jsonl` | host | — |
| Prose answers | `nb-answer` | rules first; local Qwen may only pick an option key the builder offered | host → localhost vLLM | single attempt |
| P3 sweeper | `nb-sweep` (timer) | reproduce/fix: local Qwen through `nb-iso --local-route` (observer swaps endpoint to 127.0.0.1); review: flash | sandbox | queues a labelled PR, never merges |
| Blind verifier (legacy, phases 1.3/1.4) | `ops/nb-blind-verify.py`, `bin/nb_blind_inspect.py` | flash over OpenRouter HTTP, tool loop over sealed snapshot | host | memory-less per item |

Historical routing [fact, `var/usage.jsonl` role/model counts]: `sweep_reproduce`
flash 317 + Qwen 71, `build` flash 288, `sweep_fix` flash 87 + Qwen 27, `review-resume` opus
66 + opus-5-5 52, `review` 51 + 31, `plan` 38 + 32, `sweep_review` full glm-5.3 31 (now
banned) + flash 14, `append` opus 3 (pre-deterministic). Phase-review usage lives in
`var/phase-reviews/` / `var/phase-sessions/`, not in `usage.jsonl` [inferred].

Blind-verification history [fact, CANON + docs]: local Qwen → paid full GLM 5.3 (2026-09-16,
1.82 USD/run, refused) → flash (`NB_BLIND_MODEL`) → `inspect` route (2026-09-25) → computed
check rule (≤10% unverified 1.3, 5% proposed 1.4) → cost root cause (175M tokens/pass,
2026-09-28) → **removed from the subphase path** by the simple phase gate (2026-09-28;
profile `blind_mode_legacy_phases: ["1.3","1.4"]`, phase `gate: simple` for 1.5/1.6).

### 4.3 Inventory of model surfaces (every place a model is called)

| # | Surface | File(s) | Role var / route | Budget role | Notes |
|---|---|---|---|---|---|
| 1 | Plan | `bin/nb-plan` | `NB_PLAN_MODEL`, Claude CLI | plan | `nb_correlate plan` before call; `nb_claims_draft.py` hook drafts claim lists for simple gate [inferred: runs inside the planner call] |
| 2 | Planner continuation | `bin/nb_plan_checkpoint.py` + `nb-plan` | same | plan | bounded continuation of a turn-limited plan; approval `planner_continuation_authorization` |
| 3 | Build | `bin/nb-build` → sandbox worker | builder route | build | via `nb-observe-claude` (stream observer, provider prefs injection via `CLAUDE_CODE_EXTRA_BODY`) |
| 4 | Errored-build continuation | `nb-cycle` `nb_errcont_*` → `nb-build` | builder | build | cap `nb_errcont_cap` |
| 5 | Gate repair build | `nb-gate-input` / `nb-edge-input` packet → `nb-build` | builder | build | `max_gate_repairs`, signature stop on 3rd identical |
| 6 | Rebase-marker repair | `nb_rebase_repair.py` brief → `nb-build` | builder | build | |
| 7 | Adaptive sub-builders | `bin/nb-adaptive-build` | builder | build | ≤3 |
| 8 | Closure-text call | `bin/nb-closure-text`, `nb_closure_text.py` → `nb-build` (`NB_BUILD_CLOSURE_OUT`) | builder | build | no file may change; `NB_CLOSURE_TEXT_CALLS` per task+HEAD |
| 9 | Final cleanup | `bin/nb_final_cleanup.py` → `nb-build` [inferred] | builder | build | one P3 + ≤1 test repair |
| 10 | Package regen | `nb-package-regen` worker `regen` mode | **no model** (runs product writer via pytest) | — | listed to avoid confusion |
| 11 | Prose answer | `bin/nb-answer` | `NB_LOCAL_*` | — | option-key only |
| 12 | Review | `bin/nb-review` | `NB_REVIEW_MODEL` | review | |
| 13 | Review output repair | `bin/nb-review` (`review-repair`, resume, `NB_REVIEW_REPAIR_TURNS` default 4) | same | review | repairs unreadable reviewer output in the same session |
| 14 | Ledger review (legacy/dev-only branches) | `nb_ledger_review` (nb-env) → `nb-review` | review | review | fill branches are mechanical |
| 15 | Phase review | `bin/nb-phase-review` → `nb-supervise` → `nb-supervisor-call` (Codex) / `nb_claude_runtime.py` (Claude fallback) | manifest `phase_reviewer` | supervisor | MCP confined tool; simple gate: reviewer fans claims out to subagents |
| 16 | Phase audit recovery | `bin/nb_audit_recovery.py`, `nb-audit-recover` | phase route | supervisor | exactly-once resume after rate limit |
| 17 | Learning step | `nb-supervise` (from `nb-batch learn()`) | manifest `supervisor` | supervisor | |
| 18 | Learning fallback | `bin/nb-supervisor-fallback-call`, `nb_supervisor_fallback.py` | `NB_SUPERVISE_FALLBACK_*` | supervisor | only on provider-level stop |
| 19 | Seam review | `bin/nb-seam` | `NB_SEAM_*` | supervisor [inferred] | historically operator-run |
| 20 | Sweeper reproduce/fix | `bin/nb-sweep`, `nb_sweep_run.py` | `NB_SWEEP_MODEL` | sweeper's own accounting [inferred] | |
| 21 | Sweeper review | `bin/nb_sweep_review.py` | `NB_SWEEP_REVIEW_MODEL` | same | |
| 22 | Blind verify / inspect (legacy) | `ops/nb-blind-verify.py`, `bin/nb_blind_inspect.py` | `NB_BLIND_*` | blind spend `var/blind-spend/` | tree rail |
| 23 | Claude preflight | `bin/nb_claude_preflight.py` | **fake local provider** | — | exercises fresh/resume + MCP confinement |
| 24 | Builder key probe | `ops/nb-builder-key-probe` | **no model**; OpenRouter `/auth/key` + `/credits` | — | writes `/srv/nobetci/status/builder-key.json` |
| 25 | Silent scan provider-key probe | `bin/nb-silent-scan` + `etc/silent-scan.json` | no model; key endpoint | — | |

---

## 5. Isolation and security model

**Sandbox call chain [fact]:** `nb-build`/`nb-cycle` → `bin/nb-iso run <mode>` →
`sudo -n /srv/nobetci/bin/nb-sandbox-unit prepare|start|stop|destroy <id>` →
`systemd-run --collect --wait --unit=nb-run-<id> --uid=nbbuild --gid=nbbuild
-p PrivateTmp -p TemporaryFileSystem=/dev/shm -p NoNewPrivileges -p ProtectSystem=strict
-p ProtectHome -p ReadWritePaths=<rundir> -p RestrictSUIDSGID -p LockPersonality
-p RestrictRealtime [-p EnvironmentFile=/srv/nobetci/builder/provider.env]` →
`/srv/nobetci/bin/nb-run-worker <id>` (fixed path; the caller cannot choose what runs; job is
a **mode**, never a command string).

- Worker modes (`nb-run-worker`, 54 KB, rev 2 + inserted fragments from `ops/worker-*.sh`):
  `boundary`, `patch` (test scenarios `code|canon|traversal|lying|malformed`), `build`,
  `gate` (scopes `smoke` — **hardcoded `tests/test_broker.py`** —, `full`, `nodes`),
  `package`, `regen`, `linger`.
- `runs/<id>/` owned by `mega` (0710), `in/` `mega:nbxfer 0750` (builder reads, cannot
  replace — parent-dir ownership lesson), `home/` per run (`HOME`), `out/` results.
  Inputs: `task`, `plan.md`, `base`, `accepted.patch`, `limits.json` (build wall, gate
  parallelism, gate_results format), `model`, `provider-route.json`, `local-route`,
  `regen.json`, node list.
- `mirror.git` (`mega`-owned bare mirror, `flock` `nobetci-update.lock`, `git remote update`)
  — the worker clones it fresh; base commit must exist; host git history beyond the mirror
  is not available, anything needed from old commits is shipped hash-bound.
- Output is a **patch** (+ build log, trace, usage). `nb-accept` treats it as data only:
  applies to a temp index, asks git for the file list (never parses patch text), refuses
  canon paths (product `deploy/bin/canon-paths` via profile `canon_definition`, evaluated
  from HEAD by a trusted copy — `nb_canon_check`), runs `nb_test_integrity.py` (mode
  `shadow` today; `block` requires a plan sidecar `var/plan-sidecars/<task>.json`).
- `nb-gate-iso` explicitly does not trust the test counts printed by builder code (pytest
  output can lie, `os._exit(0)`); verdict = mechanical comparison vs baseline, report
  written by the worker, bound to HEAD+tree. `nb-ship` copies artifacts out immutably
  (TOCTOU), never runs repo code on the controller side.
- Survivors: `nb-unit-survivors` / `no_survivors` — destroy refused while any `nbbuild`
  process lives. Runaway rail: `nb_build_recovery.py watch` stops a build that made no edit
  for `no_edit_seconds` 1800 while ≥50k chars streamed (profile `build_runaway`).
- Builder provider route is enforced by `nb-observe-claude provider_environment`: policy file
  must be controller-owned `in/provider-route.json`, model/base URL must match the CLI args,
  injects `CLAUDE_CODE_EXTRA_BODY={"provider":{sort,max_price,ignore,allow_fallbacks}}`;
  local route only `http://127.0.0.1:<port>`, paid key kept out.
- Reviewer/planner on host: `nb_cli_isolation.py` settings keep the operator's
  CLAUDE.md/memory out; reviewer in detached worktree + fingerprint check; phase reviewer
  read-only via MCP + Landlock (`nb_scratch_sandbox.py`: curated reads, one writable
  scratch, no TCP).
- `nb-permit` (951 lines): MCP permission server, default deny, explicit allowlist (git
  read-only subcommands + push etc.), `decide` error → deny ("a crashing permission server is
  worse than a denying one"). It is documented as a *policy/early-warning layer, not the trust
  anchor* — the trust anchor is the OS user + patch boundary. Also sourced by the operator's
  bash guard hook.
- Escalation triggers are code: `etc/triggers.conf` section A (unconditional stops:
  `promotion_gate`, `permanent_knowledge_write`, `production_or_thirdparty`,
  `credential_or_access`, `naming_or_org_reality`, `calendar_gate`, `new_axis`), consumed by
  `nb-answer`/`nb-deploy` (`allow corpus_index_apply` in authorization gates the product's
  `post_apply`).
- Test write guard `t/nb_test_guard.py` (imported by 191 test files): HOME/NB_HOME/CODEX_HOME
  to temp; Python write APIs and subprocesses targeting `/home/mega/nobetci`,
  `/home/mega/.claude`, `/home/mega/.codex`, `/srv` raise `PermissionError` (after two
  incidents in which tests overwrote real credentials/env). Protected roots are hardcoded.
- Root rollouts use `ops/nb_install_guard.py` (write targets must resolve under the validated
  home).

---

## 6. Evidence and accounting

| Artifact | Path | Bound to | Notes |
|---|---|---|---|
| Delivery record (product proof) | `var/deliveries/<task>.json` (56) | candidate | fields: `build_branch/head, gate_head/log/scope/verdict, review_head/findings/round/*history, edge_verify_sha/tree/verdict, merged_pr/sha/branch/at, deployed_sha/at, deploy_preexisting_drift, final_cleanup_receipt, closing_draft, plan_file, task_acceptance/canon, index_applied, run` |
| Ledger row | product repo `docs/notes/*ledger*.md` (profile `ledger_schema`) | delivery + reviewed closing draft | markdown table, ids `R-…`/`RR-{pr}-{n}`, severities BLOCKER/P1/P2/P3, final states closed/resolved/superseded |
| Gate baseline | `etc/baseline.lock` (Spark, floor 6457, tree-bound), `etc/baseline-edge.lock` | measured gate log sha | `known_failure`, `flaky_tolerated`, `passed_floor`, `margin`, `evidence`; relock only by `ops/nb_baseline_lock.py` + approval |
| Gate verdict | state `gate_head`, `gate_verdict`, `gate_env`, `gate_cmd`; `var/logs/iso-<id>/gate.log` | HEAD **and** tree; env identity (`nb_env_identity.py`) and gate command | a changed command or env forces re-gate |
| Per-test results | `<gate log>.results.json` (`nb_gate_results.py`, junit-xml) | HEAD+tree+report | offered to simple-gate reviewer as full-suite evidence |
| Base-broken | `var/base-broken/` (`nb_base_broken.py`) | base tree | failures also on base are not the candidate's |
| Accept record | `var/accept/<id>.json` | tree | canon + test-integrity decision |
| Review findings | `var/reviews/`, `review_findings` path; quarantine `var/review-quarantine/` | HEAD | policy `nb_review_policy.py` |
| Phase audit | `var/phase-reviews/<task>/<head>-<digest>/` (packet, intent, usage, receipt), `var/phase-sessions/<task>/` | head + input digest | |
| Evidence jobs | `var/evidence-jobs/`, grants in revision `grants/` | grant (task, job), freshness, restore | controller-run, reviewer judges receipts |
| Per-call telemetry | `var/usage.jsonl` (1110) | call id | keys: `at, run, call, role, model, in, out, cache_read, cache_write, turns, sec, cost_usd, cost_is_estimate, cost_scope, provider_est_usd, session_cost_usd, scope_delta_in, model_usage_in/out, measured, denials` |
| Cost scopes | `cost_scope` ∈ `cli-total`, `session-delta`, `fresh-session` (null on old rows) | | resumed-session delta rule in `nb_session_cost.py` |
| Routing | `var/routing.jsonl` (`nb_routing_record`/`nb_routing_verify`) | | returned model verified against allowed regex |
| Sessions | `var/sessions.jsonl` (`nb_session_event`) | | fresh/resume intent before call |
| Pending calls | `var/pending-calls/`, interrupted dispositions | | an unsettled call blocks installs |
| Context metrics | `var/context-metrics/`, `var/logs/call-<id>/` (prompt, context.json, input/result patch, usage) | | |
| Build metrics/notes/trail | `var/build-metrics/`, `var/build-notes/`, `var/build-trail/` | | |
| Campaign cash ledger | `bin/nb_campaign_ledger.py` (evidence-gate spec; not cut over) | | |

Dollar figures are CLI estimates unless labelled as a provider receipt; under
`cost_basis=provider_key` the provider key's own limit is the real rail (paged by
`nb-notify` from `/srv/nobetci/status/builder-key.json` and the blind key endpoint).

---

## 7. Supervision and operations

| Mechanism | Unit / file | Cadence | What |
|---|---|---|---|
| Main service | `~/.config/systemd/user/nobetci-development.service` → `bin/nb-run-active-batch` | `Restart=on-failure`, `RestartPreventExitStatus=1 2`, `KillMode=control-group`; ExecStartPre waits for DNS of api.telegram.org | applies pending phase install, then `nb-batch run` |
| Hold watcher | `nobetci-hold-watch.timer/.service` → `bin/nb-hold-watch` | 5 min | resumes a held batch when awaited thing arrived; applies staged installs (`harness-install.py --apply-staged`); honours `var/maintenance-leases`; log `var/hold-watch.jsonl`; pause = `var/hold-watch-pause.json` `{until (zoned, ≤24h), reason}` |
| Wake | `nobetci-hold-wake.timer` → `nb-hold-watch --wake` | 15 min | restarts the watcher timer unless paused; re-sends due outbox pages |
| Hold classification | `bin/nb_hold_class.py` | — | one rule: does this hold need a human? (shared by watcher + notifier) |
| Notifier / pager | `nobetci-notify.timer` → `bin/nb-notify` (+ `nb_notify_text.py`) | 1 min | Telegram via `openclaw message send --channel telegram`; outbox with retry 10 min doubling to 1 h, never dropped; pages: hold > `pager.hold_page_after_min` 60, watcher silent > 20 min, spend ≥ 0.8 of a key/cash cap; config `etc/notifications.json` |
| Escalation (inline) | `nb_escalate` in `bin/nb-env` | on event | `openclaw message send` to a hardcoded Telegram target |
| Heartbeat | `bin/nb-heartbeat` (`--show`, `--digest`) | (cron/openclaw in the past [inferred]) | stale / interrupted / hung from terminals + lock; healthy = no output |
| Health | `nobetci-health.timer` → `bin/nb-health` | 15 min | read-only JSON: status, active task, stage, worker pids, unit state, manifest lineage, outbox age |
| Silent scan | `nobetci-silent-scan.timer` → `bin/nb-silent-scan` (`etc/silent-scan.json`) | 15 min | what stopped without anyone being told; companion containers, HTTP probes, provider key |
| Audit recovery | `nobetci-audit-recovery.timer` → `nb-audit-recover` | 5 min | resumes interrupted phase audit when provider is back |
| Builder key probe | system `nobetci-builder-key-probe.timer` (root) | 30 min | key usage/limit/credits numbers |
| Alert sink | `nobetci-alert-sink.service` → `bin/nb-alert-sink` | always | signed-webhook receiver for the *product's* alerts (evidence for silent-loop checks) |
| Sweeper | `nobetci-sweeper.timer/.service` → `nb-sweep once` | continuous, yields to main cycle | see 4.2; drain between tasks `nb_sweep_boundary.py` (mode off/dry_run); main-only deploy `nb-sweep-deploy` |
| Dashboard | `nobetci-dashboard.service` → `/home/mega/nobetci-dash/nbdash.py --bind 172.16.0.192 --port 8765` | always | read-only stdlib HTTP page + `/api/status`, `/api/gate`; synthwave UI built from `dash/ui-src` (npm) |

Dashboard: source `/home/mega/nobetci-dev/dash/` (11 `nbdash*.py`, 3202 lines, 4 test
files / 65 tests, `samples/`, `ui/` built, `ui-src/` Vite-style project), installed by
`ops/install-dashboard.py` into `/home/mega/nobetci-dash`. Never calls a model, never
opens key files, escapes HTML.

Maintenance boundary protocol (summary): request → cycle exits `maintenance_boundary` at a
settled checkpoint → batch holds as `maintenance boundary` → watcher applies within one tick
→ restart. `nb_maintenance.OPERATIONS = build-turn-policy | phase-harness |
staged-harness-install`; a dead coordinator (pid/process-start check) never leaves the
product paused. Never stage a harness request while a root rollout is pending; never edit a
staged/pinned file before it lands (memory "Sahnelenen dosyaya dokunma").

Operator read-only commands: `nb-health`, `nb-task-status <id>`, `nb-heartbeat --show`,
`nb-recovery-inspect`, `nb-batch status --manifest …`, `python3 ops/nb-components.py check
--home …`.

User-unit files live only in `~/.config/systemd/user/` (17 unit files); sources in
`etc/systemd/` (10) and `ops/systemd/` (audit-recovery, builder-key-probe) and
`ops/nobetci-{development,sweeper}.service`, `ops/nobetci-sweeper.timer`. There is **no**
`/home/mega/nobetci/etc/systemd`. Wake unit install is manual (`components.json` `wake_unit`).

---

## 8. Product coupling (InfraMedic / GitHub / host) — the extraction worklist

### 8.1 Product-named code and data

| Where | What | Severity for release |
|---|---|---|
| `bin/inframedic-*` (13 files, 1607 lines) | adapters: browser login (local + LDAP), console, deployed identity, lab deploy/install, LDAP cases, quorum-2 (+ driver run on app host), phase1-prepare, phase-evidence, second-source | move to an `examples/inframedic/` adapter pack |
| `ops/inframedic-*.py` (7) | evidence-job adapters: canon references, console/doors journeys, alert sink declare, knowledge reconciliation, runtime observation, silent-loop experiment | same |
| `ops/lab/` (18 files) | Proxmox/AD DC/LDAPS/app-lab provisioning, fixtures | product lab, out of core |
| `ops/evidence-jobs/inframedic-*.json`, `ops/claims/inframedic/`, `ops/obligations/inframedic/`, `ops/approvals/*` (70), `ops/task-context/*` (40), `ops/limits/*`, `ops/reviewer/verification-inframedic.json`, `ops/baselines/` | product/operator records | drop from release; keep schemas + one synthetic example |
| `etc/phase-audit-inframedic.json` (31 KB) | roadmap path, ledgers, phases 1, 1.3–1.6, doctrines, `Faz` heading regexes, Turkish roadmap vocabulary, evidence topics | becomes `examples/`; core needs a documented schema |
| `etc/evidence-profile-inframedic.json`, `etc/tasks/R-1.3-2-8.conf`, `etc/authorization`, `etc/budget.conf`, `etc/baseline*.lock` | product instances | templates only |
| `etc/execution-profile.json` | `adapter: pytest`, `/srv/nobetci/venv/bin/pytest`, runner sha binding, `brief` about `docs/notes/phase-1.3-audit.json`, `gate_command .venv/bin/pytest -q -n 8`, `canon_definition deploy/bin/canon-paths`, `commit_guard deploy/hooks/pre-commit`, `phase_evidence_command inframedic-phase-evidence`, `delivery.edge.host gns3@192.168.20.36`, tree `/home/gns3/linux-verify/inframedic`, `apply deploy/bin/deploy-from-dev.sh`, `verify deploy/bin/verify-deploy.sh`, `post_apply` sudo `inframedic corpus index --apply`, `test_integrity.production_globs inframedic/**`, `browser_walk` (console URL 100.101.24.56:8300, lab DC 100.68.102.86, lab SSH key path, LDAP identities `reader-ankara/kayseri`, node + playwright paths under `/home/mega`) | profile mechanism is the right abstraction; values must become an example profile |
| `etc/silent-scan.json` | container `nb-embedder`, embedder URL 172.16.0.195:11434 (`bge-m3`) | example only |
| `bin/nb-apply-staged-phase:56` | `phase-audit-inframedic.json` literal | fix: profile name from manifest |
| `ops/install-phase-audit-profile.py:23` | default template `phase-audit-inframedic.json` | env default → required arg |
| `ops/install-obligation-gate.py:20` | `ops/obligations/inframedic/phase-1.5.activation.json` | same |
| `bin/nb-sweep:78` | default `--repo /home/mega/inframedic` | remove default |
| `/srv/nobetci/bin/nb-run-worker:937` | gate `smoke` target `tests/test_broker.py` | profile key |
| `bin/nb_work_map.py`, `bin/nb_blind_scope.py`, `bin/nb-batch:277`, `bin/nb-selfpack` | product names only in comments | cosmetic |
| `t/lib.sh` `t_repo` | fake repo has `inframedic/` dir, `deploy/bin/canon-paths`, `docs/notes/2026-09-04-phase-1.3-review-ledger.md` | test fixtures assume product layout |
| Task id regex `(?:R|RR|F)-…`, `feature` must start `F-` (`nb-batch`) | ledger-id grammar from the product's review doctrine | make configurable |
| Branch name `fix/nobetci/<MMDD-HHMM>-<run6>` (`nb-cycle:647`) | | configurable prefix |
| Phase-audit concepts (Faz, roadmap rows `1.4-7`, doctrines, obligation lists, blind scope) | product roadmap doc format (`docs/plans/2026-08-26-vision-roadmap.md`) | generalize or leave as optional plugin |

### 8.2 GitHub-specific

- `bin/nb-merge`: `gh pr list/create/view/merge --rebase --delete-branch`; assumes rebase-merge
  repo; push 5xx retry; `NB_GH=gh`.
- Delivery identity is the **PR number** (`merged_pr`), ledger repair ids `RR-{pr}-{n}`
  (`bin/nb_ledger.py`), `nb-task-status --require-pr`, `nb_pr_title.py`.
- Sweeper: `bin/nb_sweep_pr.py` (`gh pr create --label`, label must pre-exist),
  `nb_sweep_run.py` (`gh pr view <branch>`), `nb_sweep_boundary.py` (required GitHub check
  name or pseudo-check `receipt:full_gate`), `nb_sweep_deploy.py` (`NB_GH`).
- `nb-cycle:105` passes `--gh` to cycle audit.
- `origin/main` is a literal base throughout `nb-cycle`, `nb-merge`, `nb-rebase-candidate`,
  profile `base_ref` (`origin/main`) exists but is not used everywhere [inferred from literals].
- Tests fake `gh` via `NB_TEST_GH`.

### 8.3 Delivery / host specific

- Edge = SSH host running the suite and the deploy scripts (`nb-edge`, `nb-deploy`,
  `nb_drift.py`, `nb_transient.py`, `nb-hold-watch` edge reachability, `nb-preflight`).
  Python 3.12/aarch64 (Spark) vs 3.14/x86_64 (edge) rationale is product-specific; the
  "verify on target before merge" gate is generic and worth keeping.
- `post_apply` with escalation class `permanent_knowledge_write` is InfraMedic's corpus index.
- Tailscale installers `ops/install-tailscale-{edge,spark}.sh`.
- Hardcoded host paths: `nb-env` PATH includes `$HOME/.nvm/versions/node/v24.20.0/bin`;
  user units hardcode `/home/mega/...`, dashboard bind IP `172.16.0.192`; ~35 ops/bin files
  contain `/home/mega` or private IPs (largest: `ops/inframedic-*`, `bin/nb-notify` 4,
  `ops/install-*` 1–3 each). `/srv/nobetci` paths are literals in `nb-iso`, worker, profile,
  executors registry (`etc/executors.json`, `toolchain.root /srv/nobetci/venv`).
- `nb-env` hardcodes the escalation channel `NB_TG_CHANNEL=telegram` and a Telegram
  **target chat id** (value intentionally not reproduced; must become config + secret-free).
- `openclaw` CLI is the only message transport (`nb-env`, `nb-notify`, `nb-answer`,
  `nb-preflight`).
- Claude CLI binary pinned at `/srv/nobetci/bin/claude`; Codex at `/home/mega/.local/bin/codex`
  (manifest).
- Local vLLM at `127.0.0.1:8000` (DGX Spark specific; memory notes on MXFP4/enforce-eager).
- Model ids are vendor-specific (`claude-opus-5-5`, `gpt-6-astra`, `z-ai/glm-5.3-flash`,
  `RadixArk/Qwen3.8-27B-NVFP4`) and the approved fallback list is a code literal
  (`nb_phase_provider.APPROVED_FALLBACK_MODELS`).
- Builder env assumes OpenRouter's Anthropic-compatible endpoint + `provider` routing body
  (`nb-observe-claude`), and Claude Code CLI as the agent harness for the builder.

### 8.4 Language

- Turkish: 26 bin/ops files contain Turkish diacritics; 23 bin files emit Turkish operator
  text in `finish`/`nb_escalate`/`echo`/`die`; `nb-cycle` alone has 453 comment lines, mostly
  Turkish. Stage names are Turkish tokens (`devam`, `kabul/<r>`, `kapi/<r>`, `inceleme/<r>`,
  `devir`, `teslimat-kaydi`, `ledger-kapi`, `ledger-inceleme`, `tamamlanma`) and some terminal
  `detail` strings ("urun indi", "kapi gecemedi"). `t_done` prints `N gecti · M hata`; test
  output `ok`/`HATA`. `etc/roles.conf`, `triggers.conf`, `budget.conf`, `baseline.lock`
  headers are Turkish. Telegram wording in `nb_notify_text.py` is operator-facing Turkish
  [inferred from operator quote in its docstring].
- Most docs from 2026-09-15 on are English; earlier docs Turkish; docs are full of operator
  quotes in Turkish.

---

## 9. Size, complexity, tests

### 9.1 LOC by area [fact, `wc -l`]

| Area | Files | Lines |
|---|---|---|
| `bin/` total | 174 entries (incl. `__pycache__`) | 39,022 |
| — `bin/nb_*.py` modules | 100 | 21,002 |
| — `bin/` python entrypoints (shebang python, no `.py`) | 39 (incl. some `inframedic-*`) | 9,316 |
| — `bin/` bash entrypoints | 34 | 8,596 |
| — `bin/inframedic-*` | 13 | 1,607 |
| `ops/*.py` | 53 | 11,899 |
| `ops/*.sh` | 11 | 575 |
| `ops/lab/` | 18 | 1,572 |
| `ops/` data | approvals 70, task-context 40, evidence-jobs 34, claims 59, obligations 10, limits 4, reviewer 3, baselines 3 | — |
| `t/` | 244 entries | 34,063 |
| `dash/*.py` | 15 | 3,202 |
| `docs/` | 126 entries | 13,778 (md) |
| Largest files | `nb-cycle` 1664, `nb-env` 1057, `ops/harness-install.py` 962, `nb-permit` 951, `nb-build` 927, `nb_phase_audit.py` 918, `nb-batch` 915, `nb-notify` 859, `nb-review` 723, `nb_claims.py` 698 | |
| Root sandbox | `nb-run-worker` 54,803 B, `nb-sandbox-unit` 8,413 B, `nb-observe-claude` 23,123 B (copy of source) | |

Component manifest `etc/components.json`: 22 components (simple_gate, gate_results,
phase_review_fresh, phase_audit, obligation_gate, blind_verification, base_broken,
package_regen, builder_policy, installer_source, srv_worker, evidence_gate,
evidence_toolchain, deploy_drift, evidence_claims, pager, wake_unit, build_runaway,
build_notes, task_dossier, test_integrity, …) over 119 files; each component names its
installer and target (`home` / `source` / `srv` / `user_systemd`).

### 9.2 Test harness

- **Bash suites** `t/*.sh` (42): source `t/lib.sh` (`ok`/`bad`/`t_done`, `t_repo` = fake git
  repo with origin, fake ledger, fake `canon-paths`, `t_task_confs`); `T_S` scratch under
  `/tmp/nb-t-<name>`; `t/cycle-fakes.sh` `t_overlay` builds a symlink overlay of `bin/` with
  fake `nb-preflight`, `nb-accept`, `nb-plan`, build/iso/gate/edge/merge/deploy writing
  `calls-<name>.txt`; real `nb-cycle`, `nb-env`, `nb-review` (fake claude), closing draft,
  fix input, append, ledger-row, ledger, next, task-status. ~629 `ok`/626 `bad` call sites.
  Examples: `cycle-flow.sh`, `full-repair-loop.sh`, `preflight-evidence.sh`,
  `feature-cycle.sh`, `p3-cycle.sh`, `edge-repair.sh`, `merge-pr.sh`, `sweep-deploy.sh`,
  `review-routing.sh`, `closure-text-call.sh`, `empty-branch-resume.sh`.
- **Python** `t/test_*.py` (187 files, 1,651 `def test_`) + 12 non-prefixed `t/*.py`
  (216 tests: `batch.py` 66, `notify.py` 36, `final-cleanup.py` 33, `context-transmission.py`
  25, …). unittest; each file puts `bin/` on `sys.path` and imports `nb_test_guard` first.
  CLAUDE.md's "~7 s, 177 tests" is **[stale]**.
- `NB_TEST=1` + `NB_TEST_DIR` is the single override gate in `nb-env` (fake `NB_TEST_CLAUDE`,
  `NB_TEST_GH`, `NB_TEST_OPENCLAW`, `NB_TEST_SSH`, `NB_TEST_REPO`, `NB_TEST_AUTH`,
  `NB_TEST_BASELINE(_EDGE)`, `NB_TEST_BIN`, `NB_TEST_ROLES`, `NB_TEST_TASKS`).
- Fixtures `t/fixtures/` (closure record, evidence-profile demo, obligation golden, retro
  roadmap rows). Dashboard tests in `dash/test_*.py` (65).
- No top-level runner, no CI (memory: "CI kapalı"); docs record which suites ran per change.
  I did **not** run the suites (read-only brief; a past test overwrote real credentials).
- Worker fragments are tested by sourcing the same `ops/worker-*.sh` the rollout inserts.

### 9.3 Invariants to carry into the release (from CLAUDE.md / CANON §6)

Four outcomes distinct (`found/empty/could_not_run/failed`); a gate that cannot compare does
not pass; copy evidence before destroying its run dir; one decision in one place (append-only
multi-block files fail closed); tests must prove the red they catch and not measure their own
harness; never start a second worker while a call is pending; never reset candidates,
counters, charged attempts or authority; never edit a running bash script; false-positive
guards get removed, not tolerated; correlate every model call before egress; models never
decide stop/continue or widen authority.

---

## 10. Suggested reading order for the extraction session [inferred recommendations]

1. `bin/nb-env` (paths, test gate, records, auth, budget, state helpers) — the de-facto core library.
2. `bin/nb-cycle` stages above; `bin/nb-batch` admission/learning.
3. `bin/nb-iso` + `/srv/nobetci/bin/nb-sandbox-unit` + `nb-run-worker` (isolation contract).
4. `bin/nb-accept`, `bin/nb-gate-iso`, `ops/nb_baseline_lock.py` (gate semantics).
5. `bin/nb-review`, `bin/nb_review_policy.py` (severity policy in code).
6. `ops/harness-install.py`, `bin/nb_maintenance.py`, `bin/nb-hold-watch` (install/boundary).
7. `etc/execution-profile.json` + `bin/nb_profile.py` + `bin/nb_paths.py` (where neutrality already exists).
8. `docs/2026-09-15-project-neutrality-audit.md`, `docs/2026-09-28-simple-phase-gate.md`.

Natural seams for an agnostic release [inferred]: (a) a VCS-host adapter (GitHub `gh` today)
for PR create/merge/status and delivery identity; (b) a delivery-target adapter (edge SSH
suite + deploy scripts today); (c) a notifier adapter (openclaw/Telegram today); (d) a
test-runner adapter (pytest/junit today; `nb_executor.py` + `etc/executors.json` +
`nb_adapter_python_venv.py` already started this); (e) model-route adapters per role (Claude
CLI, Codex CLI, OpenRouter HTTP, local OpenAI-compatible); (f) the phase-audit/claims layer as
an optional plugin with the product roadmap grammar in a profile; (g) English-only state
tokens and operator text.
