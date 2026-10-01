# 02 — Lessons, incidents and invariants from ~3 weeks of unattended operation

Know-how transfer for the session that rebuilds the "auto-dev-cycle" framework (codename
**nöbetçi**, "the on-duty agent"; source `/home/mega/nobetci-dev`) as an open-source,
project-agnostic first release.

This is **not** an architecture description (see the other handoff files for that). It is
what we *learned by running it*: which rules turned out to be load-bearing, which failure
classes kept coming back, what the numbers were, what the operator ruled, and what we would
not build again.

Period covered: first supervised round 2026-09-05/08, unattended batch from ~2026-09-11,
dense operation 2026-09-14 → 2026-10-01. First (and only) target project: InfraMedic
(`/home/mega/inframedic`), a Python product deployed to one Linux "edge" host.

Written 2026-10-01 from read-only sources. Nothing under `/home/mega/nobetci-dev`,
`/home/mega/nobetci`, `/srv`, `/home/mega/inframedic` was modified to write it.

---

## 0. How to read this / sources / vocabulary

### 0.1 Primary sources (cite these when you need the full story)

| Source | What it holds |
|---|---|
| `/home/mega/nobetci-dev/CLAUDE.md` § "Invariants that have already been broken and fixed" | The short invariant list the supervisor checked new code against |
| `/home/mega/nobetci-dev/docs/CANON.md` | Mission priority, standing dated authorizations, "caps are rails" |
| `/home/mega/nobetci-dev/docs/PLANNING_CANON.md` | 16 abstract planning lessons (written 09-15/16) — the most reusable text in the repo |
| `/home/mega/nobetci-dev/docs/WORK_QUEUE.md` | Live backlog (R10–R59 open rows, "Not recommended now") |
| `/home/mega/nobetci-dev/docs/archive/2026-09-work-queue-done.md` | Rows R1–R51 verbatim with incidents and fixes |
| `/home/mega/nobetci-dev/docs/archive/2026-09-20-work-queue-history.md` | Everything up to 09-20 (2840 lines) |
| `docs/2026-09-20-cycle-time-token-analysis.md` | Where time/tokens go (O-1..O-12) |
| `docs/2026-09-28-review-gate-rethink.md` | Two independent reviews of the phase gate; 7-day stop accounting |
| `docs/2026-09-28-simple-phase-gate.md` | The gate that replaced everything (operator verbatim) |
| `docs/2026-09-30-builder-exploration-review-cost.md` | Builder exploration + reviewer cost measurements |
| `docs/2026-09-30-deterministic-closure-text.md` | Closure deadlock class and its mechanical fix |
| `docs/2026-10-01-lab-deploy-recovery-design.md` | Receipt-that-lies failure modes (F1–F10) |
| `docs/2026-09-15-project-neutrality-audit.md` | ~60 literals + 11 architectural overfits |
| `docs/2026-09-27-astra-unattended-assessment.md` | "Not suitable for unattended development in its current form" |
| `/home/mega/.claude/projects/-home-mega/memory/*.md` | Supervisor memory notes (mostly Turkish; translated below) |
| `git -C /home/mega/nobetci-dev log` | 808 commits since 09-20, 241 of them `fix(...)` |

Memory notes are referenced by filename only, e.g. `[mem:nobetci-tekrar-eden-kusurlar]`
= `/home/mega/.claude/projects/-home-mega/memory/nobetci-tekrar-eden-kusurlar.md`.

### 0.2 Vocabulary you will meet in the sources

| Term | Meaning |
|---|---|
| **operator** | The human user. Never call an agent "operator". |
| **nöbetçi** | The framework / the unattended cycle. |
| **Supervisor** | An *external* interactive agent session that develops the framework and watches the cycle (Fable = Claude session; Astra = GPT on Codex; Sol = GPT-6-sol; later Opus 5.5). Not part of the cycle. |
| **Spark** | The controller host (DGX Spark, aarch64, Py 3.12). Runs controller, gates, sandbox, local Qwen. |
| **edge** | The product's deploy target (x86_64, Py 3.14). Has no `.git`. |
| **planner / builder / reviewer** | Opus 5.5 plans; GLM-5.3-flash (OpenRouter) builds in an isolated root-owned worker; Opus 5.5 reviews. |
| **phase auditor / phase reviewer** | Integrated audit at the end of a sub-phase (Astra/Codex primary, Opus fallback). |
| **candidate** | The task branch under construction (one cumulative commit/patch). |
| **gate** | Full product test suite on Spark (and an "edge gate" on the target runtime). |
| **hold** | Batch stops itself (`held: <reason>`) — service unit exits. |
| **hold watcher** | `nb-hold-watch` timer that resumes holds on signals. |
| **boundary / idle boundary** | Point between rounds/tasks where no worker/call is live; the only time installs may land. |
| **staged install** | Hash-pinned install request applied by the watcher at the next boundary. |
| **manifest / revision** | Immutable batch definition (`var/prepared/<rev>/manifest.json`); policy changes = new revision. |
| **grant / evidence job** | Bound authority for a controller-run live probe (`ops/evidence-jobs/`); produces a **receipt**. |
| **ledger** | Product repo's markdown task ledger (`docs/notes/development-task-ledger.md`), written deterministically. |
| **sweeper** | Parallel, low-priority P3-fixing loop ("nanobots", "a fly"). |
| **blind verification** | A cheap model checking requirement→code/test citations without context (later "inspect" agentic version). Retired by the simple gate. |
| **obligation gate** | 269-row frozen obligation list + calibration + two-session confirmation. Retired. |
| Ids | `F-1.5-3` = framework-admitted product task; `R-…`/`RR-…` = review findings in the product ledger; `OB-1.5-104` = obligation row; `R10..R59` = operator-request rows in WORK_QUEUE. |
| Turkish tokens | `tuttu`=held/passed, `ölçülmedi`=unmeasured, `sapma`=deviated, `okunamadı`=unreadable, `TAVAN`=cap, `kabul`=accept, `kapı`=gate, `inceleme`=review, `devir`=handover, `teslimat`=delivery, `geçti/hata`=passed/failed. |

---

## 1. Invariants and design principles that proved essential

Each item: the rule, why it exists (the incident that taught it), and how to make it
structural in the rebuild. These are listed roughly by how much damage their absence did.

### 1.1 Four outcomes, never three: `found` / `empty` / `could_not_run` / `failed`

**Rule.** Every check, probe, gate, collector and parser returns one of four distinct
outcomes. Something that could not be measured is never "clean". A gate that cannot
compare does not pass.

**Why (repeated incidents).**
- Quota exhaustion was read as success; a `-1` summary became "0 failures"; a missing
  baseline "passed" (`[mem:nobetci-tekrar-eden-kusurlar]` class 2).
- `nb_dirty` swallowed a git error and said "clean"; `nb-review` read six fields into five
  variables so a P3-only review looked clean (`[mem:inframedic-nobetci-kurulumu]`, the
  first 100-case probe suite found four such silent breaks).
- `xargs` turns any child failure into rc 123; a guard looked for `rc=1`, so canon writes
  silently passed.
- A judge treated a message-less `assert` (`AssertionError('')`, `str(e)==''`) as success;
  a contract violation fell through to un-repaired routing and the cycle stopped twice,
  ~$152 + $163 wasted (`[mem:nobetci-kurulum-grant-kopar]`, fix `da9055f`).
- Product side, same class: the saga scheduler raised before writing any row; the 1.4 runtime
  observation read "no saga rows" as "no activity" (`docs/WORK_QUEUE.md` operator-decisions
  block). Lesson recorded: *absence of a loop's rows while its trigger fired is
  `could_not_run`, not `empty`.*
- Blind run harness wrote 819 `unavailable` items as `observed` after a 400 (`a00c8f1`).
- Lab-deploy receipt: outer status `observed` while inner `deploy.status` was `stale` or
  `could_not_run`; the simple gate read only the outer field (`docs/2026-10-01-lab-deploy-recovery-design.md` F1).

**Rebuild.** Make the outcome an enum in the core result type; forbid boolean "ok". Every
error path's text must be non-empty (`str(e) or type(e).__name__`). Every collector has a
"could not run" exit code distinct from "nothing found". Nested receipts are validated on
their inner predicate, not just the envelope.

### 1.2 Copy evidence before destroying the thing that produced it

**Rule.** Read/copy logs, replies, patches and measurements *before* tearing down the run
dir, worktree or sandbox.

**Why.**
- Gate log disappeared with the round; the third failing test could never be learned.
- Cron measurement was read after `destroy` (`[mem:nobetci-tekrar-eden-kusurlar]` class 3).
- Sweeper: `nb-iso done` destroyed the run dir *before* the reviewer's `final.txt` was read;
  every successful independent review became "could not run (empty reply)"; no `fixed`
  receipt was ever possible (Astra P1, `docs/2026-09-21-astra-review-for-fable.md` §1, fix
  `249173c`; also `fc32824` "keep the check a not-reproduced attempt wrote before its
  worktree is dropped").
- Final cleanup wrote `stage=passed` before verification ran (Astra fix `726eb11`).

**Rebuild.** Teardown APIs take an "evidence manifest" and copy first; tests use fakes that
implement the *real* teardown contract (copy-then-delete), not a no-op.

### 1.3 One decision, one place

**Rule.** Verdict, baseline comparison, authority, idle/busy, cost and task identity are
each computed by exactly one function. Append-only files with multiple blocks fail closed
(`auth_scope_ambiguous`) instead of picking first or last.

**Why.**
- The original nöbetçi lesson (PR 15, 2026-09-05): three re-review rounds each found a new
  defect, two of them in round 1's own fixes; root cause was *re-implementing verdict logic
  in a second place* (an early-return branch swallowed other verdicts) (`[mem:inframedic-nobetci-kurulumu]`).
- `etc/authorization` readers used `awk '/^scope/{...; exit}'`: the operator appended a new
  authorization, the old one kept being read, the new decision was silently ignored; worse,
  appending a *narrower* scope was invisible (`[mem:nobetci-tekrar-eden-kusurlar]` class 5).
- `nb-next` gave two answers for the same args (rc 0 with `--task`, rc 1 on the summary
  path); preflight used the summary path and wrote "no open work" while work existed — an
  unattended night would read that as "queue exhausted".
- Idle detection existed in five installers with different definitions; one counted a
  reviewer's *test* `nb-cycle` process as a live cycle (incident 09-30 17:10, fixes
  `5d86328`, `5c04055` "one idle decision for every rollout; busy() probes fail closed").
- Unmeasured-count logic duplicated between recovery and `nb-batch` (R15 follow-up).
- Normal vs resume decisions twinned in the controller (Astra priority list 09-18, `46a6a15`).
- Reviewer schema, validator and prompt derived separately from the obligation routes →
  live cycle stopped twice in one evening on "status not allowed" (Astra 09-27, P0-1 fix
  "reviewer contract from ROUTES").

**Rebuild.** One module per decision with a docstring "this is the only place that decides
X"; lint/test that no other file contains the decision's literals. Schemas for model output
are *generated* from the same table that validates them.

### 1.4 A test must prove which red it catches, and must verify its own setup

**Rule.** For every green test, answer "which defect would turn this red?" and prove it by
mutation (break the code, see red, restore). A test must not measure its own harness.

**Why.**
- A test asking "is the lock free after refusal" passed on code with the `finally: close()`
  deleted — CPython closes the local handle anyway. The real question was ordering (lock
  acquired *before* write).
- A probe suite run gave 891/7, another 907/0: the 7 reds were fake. Root cause measured:
  probes seeded `in/base` from the *developer checkout's live HEAD* and bypassed the only
  code that refreshes the mirror; after a merge ~16 cases never ran
  (`[mem:nobetci-tekrar-eden-kusurlar]` class 4; `nobetci/docs/2026-09-08-takim-kararsizligi-ayna-tazeligi.md`).
  Rule: before comparing two runs, check case counts are equal.
- Probes "passed for the wrong reason" twice: in the sandbox the canon definition was
  missing so the hook blocked everything.
- Source-string tests (`assertIn('preference(route_sort())', src)`) prove code was *written*,
  not that it *runs*; Astra rewrote two of the supervisor's tests to load and execute the
  module (`96a4c30`, `t/test_provider_pref.py`) (class 6).
- E2E fake returned an *incremental* patch while the real worker returns a *cumulative* one;
  the sweeper's fix step could never land in production for days (`eb646aa`,
  `[mem:nobetci-supurucu-ilk-tam-tur]`). "Fake must do what the real one does."
- Tests ran against the real HOME: a fake `claude` wrote `{"refreshed": N}` into the real
  `~/.claude/.credentials.json` and every sub-agent went "Not logged in"; same day an
  installer test overwrote the runtime's blind provider key file with a fake
  (`[mem:red-proof-gercek-home]`; suite-wide write guard `t/nb_test_guard.py`, `571e386`).
- Hook probe was run against both the fixed and a *deliberately broken* hook: only that
  proves the probe works (`var/hook-probe.sh`).

**Rebuild.** Ship a test-guard that refuses writes outside a temp HOME/root. Provide
contract-faithful fakes for worker, provider, git host. Require a "red on old code" line in
every fix commit's test evidence (the repo already does this: e.g. `b849e7d` "red 13 on old").
Run red-proofs only under `HOME=/tmp/...`.

### 1.5 Validation before any irreversible write ("fail fast" means *before* the write)

A new `sort` setting validated with `ValueError`, but only *after* an ambiguous "paid attempt"
marker had been written over a carefully proven primary-refusal record; a typo in
`roles.conf` would have destroyed a night's evidence (`[mem:nobetci-tekrar-eden-kusurlar]`
class 7). Ask for every new parameter: "if this line raises, what has already been written?"

### 1.6 Install footprint is part of the change

Adding an import without adding the module to every installer's file list: source green,
runtime green, a *clean* install would fail on import (class 8). Recurred: 09-27 activation
stopped because three files were missing from a hand-maintained `FILES` list (R35, fixes
`8f3d355` "one versioned component manifest for installers", `7add6bb`, `eb1689a`).
**Rebuild:** one component manifest; installer verifies the import closure.

### 1.7 Never reset counters, candidates, charged attempts or authorization

**Rule.** Restarts, installs and recoveries preserve the candidate branch, every charged
attempt, every counter (calls, rounds, gate repairs), limits and task ordering. Never
"start the product over". Never hand-edit product code from the supervisor (the builder
owns builds).

**Why.** Every recovery feature (host restart disposition, interrupted-call disposition,
resume on empty branch, rebase) was designed around "the call is charged once, marked
interrupted, partial work is not imported, the task continues on its branch with counters
kept" (`[mem:nobetci-host-restart-kurtarma]`). Same-ID re-charge bugs were found and fixed
(`152a254` "retained --repair refuses a settled call (a paid repair under a charged id was
never charged)"). AGENTS.md for the supervisor: "Do not reset candidates, accounting or
authorization."

**Rebuild.** Accounting is append-only; "resume" is a first-class path with tests proving a
restart cannot repeat a deployment, lose evidence, or double-charge.

### 1.8 Caps are safety rails, not budgets (operator ruling 2026-09-14)

**Rule.** Call/round/wall caps exist to stop runaway retries and the unexpected. They are
generous and are *raised by a bound approval* when work is healthy. A cap that stops
healthy work forces a rerun that costs more than it saved. Token efficiency comes from
scoped instructions and context handed over up front, never from starving a call.

Operator: "hard limit koyarsak zaten yeniden koşmamız gerekir" ("if we put hard limits we
have to rerun anyway") — `docs/CANON.md` §3, `[mem:nobetci-gelistirme-kanonu]`.

Corollary measured on the blind verifier: a rail is sized to the job
(`max($3, 2.5 × estimate)`, hard ceiling $15); a normal run never sees it; touching it is a
`budget_anomaly` alarm, not a stop of healthy work (operator 09-25 ~19:00: "true positives
must never hit the limit") (`[mem:nobetci-2026-09-25-operator-kararlari]` item 14).

Second corollary (operator 09-16): the round cap is for product code, not for evidence
documents — a P2 on the audit package stays blocking past ten rounds while a computed check
is not pass (`ops/approvals/2026-09-16-audit-package-p2-blocking.json`).

### 1.9 Information up front instead of read caps (operator ruling 2026-09-15)

Operator: "her yere saçma sapan sınırlar koymuşuz, bırakalım da okusun LLM ... token usage'ı
limitler koyarak optimize edemeyiz" ("we put silly limits everywhere; let the LLM read; we
cannot optimize token usage by limits"). Build 17 read a 3475-line test file 26 times; the
cure was a symbol index with line ranges (`hints.symbols`), not a cap
(`[mem:nobetci-model-bilgi-sinirlari]`). All information caps were removed (`e18e586`).

The one legitimate exception: a **decode-speed rail** on the local GPU model (Qwen blind
brief 32–60k chars took 41 s/request vs 4 s; 819 items would take hours) — that is a
hardware rail, measure end-to-end before widening.

Context program that followed (09-20, `[mem:nobetci-context-programi]`): canon delivered
*as content* (`context_reference path#anchor`, `bin/nb_canon.py`), plan as a re-readable
sidecar file, work map, builder notes, trail, later a mechanical task dossier
(`bin/nb_task_dossier.py`, ≤8 kB). Before: 0/37 task confs used canon references; 10 of 12
reviews never opened the canon; planner found doctrine with `ls | grep`.

### 1.10 Fail closed — but a crashing guard is worse than a denying one

- Permission server: a missing import crashed the whole MCP server; every error inside
  `decide` now returns `deny` (`[mem:inframedic-nobetci-kurulumu]`).
- Tool permission is an **allowlist** (a denylist's misses become permissions). Prose
  questions use a *stop-list* first (a miss there means "stop", which is safe).
- Canon definition unreadable → every consumer stops `canon_undeterminable`.
- Unknown flags: installers ran on `--help` instead of previewing (R20 follow-up) — installers
  must refuse unknown flags and preview by default.
- **But:** guards that raise false positives get *removed*, not tolerated (a guard that cries
  wolf trains everyone to bypass it; `CLAUDE.md`).
- The boundary is identity, not parsing: twenty rounds of shell-command parsing could not
  close the hole once an interpreter (`pytest`) was allowed. Builder runs as a separate uid
  (`nbbuild`) in its own cgroup on a fresh clone; what crosses the boundary is a **patch**,
  applied by the controller to a temp index, file list asked of git
  (`[mem:nobetci-izolasyon-mimarisi]`).
- Hook error messages must not name the bypass (`--no-verify` was advertised by the hook's
  own message).

### 1.11 Staged installs only, at idle boundaries, one at a time

**Rule.** Never hand-copy into the runtime. Installs are hash-pinned requests that land at
an idle boundary; never two back-to-back; never while a root rollout is pending; never edit
a staged file before it lands; never merge to the framework's main while a `--wait` rollout
is pending.

**Why (each of these cost hours).**
- Editing a script while bash runs it: bash reads incrementally → `t_run: command not found`.
  Every long-running bash entrypoint is `main(){...}; main "$@"`.
- Editing a staged file: install refused at the boundary ("staged file content changed
  since staging"); the cycle waited at the maintenance boundary for **8 h** (09-16 02:33,
  `[mem:nobetci-sahnelenmis-dosya]`, `[mem:guard-bash-heredoc-git]`).
- Uncommitted edit while a profile-only rollout waited → refused; boundary held with no new
  revision to resume it (fix `6fd87cd`, `nb_boundary.ask_resume`).
- Merging #18/#19 to main while `install-phase-audit-profile --wait` waited → "not the
  previewed set", profile written but manifest not rebound, service `failed` ~20 min
  (09-30 13:35, `[mem:nobetci-profil-kurulumu-main-sabit]`).
- Two back-to-back staged installs → hold watcher applied a 900 s failure cooldown → service
  failed ~7 min (09-30 11:50, fix `1cf7c39`: orderly staged boundary resumes after a 60 s floor).
- Root rollout landed while the worker was idle but the controller active → bound
  execution-profile hash changed → "bound configuration changed" → review failed + generic
  hold (`bc5d50f`; installer `--accept-rollout <receipt>`).
- Installing a new version of an evidence script silently orphaned the grant bound to the
  old sha → job "unbound", never ran → **12 h lost** (09-22, `[mem:nobetci-kurulum-grant-kopar]`;
  fix `18b2b16` "reject executable upgrades that leave evidence grants stale").
- A test process was mistaken for runtime (see §2.5).

**Rebuild.** Installer is a state machine: preview → stage (content-pinned) → land at
boundary (watcher) → receipt under `var/maintenance/<date>-<name>/`. Every hash binding
ships with its change path (PLANNING_CANON §15). One staged request at a time; the
framework repo's main is frozen while one is pending.

### 1.12 Independent reviewer GO before install

Every framework change in the last two weeks went: own worktree → independent reviewer
(Astra/Sol on Codex, Opus 5.5 when Codex credits ran out) → GO → staged install → installed
bytes `cmp`-verified against main. NO-GO rounds were common and caught real defects
(undercharge path `4e9a9f0`, out-of-contract ledger rows `2511d88`, injection in notes
`40b9b7c`, sandbox bash policy mediums `15f728f`). Supervisor's own working rule after the
09-26 latency research: framework branches ≤ ~400 lines, acceptance criteria + smoke stated
up front, block only on critical/high, split after 3 NO-GOs, live smoke before GO
(`[mem:nobetci-2026-09-25-operator-kararlari]` item 16).

Counter-lesson: review rounds can also be a sink — the blind "inspect" branch took 11 review
rounds (`5e0dc57`, R20) and then the whole mechanism was retired a few days later.

### 1.13 Project neutrality

Operator, repeatedly (09-15, 09-16, 09-22, 09-28): the framework is a separate product for
future projects; do not overfit to InfraMedic, Python/pytest, a phase, or the dev machines
(`[mem:nobetci-cerceve-tarafsizligi]`, `[mem:inframedic-urun-tarafsizligi]`). `bin/` must not
learn the product; product specifics live in `etc/` profiles, `ops/` adapters and
`ops/claims/<project>/`. The 09-15 audit found ~60 literals and 11 architectural overfits
(§4.2 below). The framework ran "*as InfraMedic*" when unconfigured instead of refusing —
fix: `nb_paths` with a named error and `/nb-repo-unconfigured` sentinel.

### 1.14 No hold without a signal; no stop without a page

Every way the cycle can stop must name what restarts it (receipt, retry time, revision,
human act) — PLANNING_CANON §16. A hold whose only exit is a person noticing is a stall.
Test each hold reason against the resumer's signal list. Dedup keys must name the *event*,
not the *channel*: the watcher deduplicated experiment signals by name, so every receipt
after the first was ignored and a hold sat 40 min "no new signal" (`e7a8b49`).

### 1.15 Other rules that earned their place

- **Evidence names its instrument** (receipt carries the producer's hash; another
  producer's receipt is stale) — otherwise a corrected verifier cannot supersede its own
  earlier evidence (`dba6275`, PLANNING_CANON §4a).
- **Machine-produced checks are decided by a rule**, computed by the controller; the
  reviewer overturns a pass only with a contradiction (`bin/nb_phase_rules.py`,
  `ops/approvals/2026-09-16-computed-blind-check.json`). Ten review rounds were spent
  "auditing the instrument" before this.
- **Proof keyed by content, not round**: carry verdicts while the hashed input is unchanged;
  re-ask only `unavailable`/model errors (`4d05e74`). Identity must hash observations, not
  timestamps or live counters — receipts with timestamps changed the package identity and
  bought a useless review round (`9231866`, `c4304dd`, `15900b2`).
- **Completeness ≠ all-pass**: "complete = every id computed", the checks decide pass. The
  1.3 audit sat on "complete = hepsi pass" while 726/819 rows were by design not-pass
  (`8664b6d`).
- **Healthy = silent on every stream** for cron/heartbeat: the cron delivers stderr too.
- **Interactive works, scheduled fails → measure both contexts** (`/proc/<pid>/status`
  Groups): the scheduler daemon lacked a supplementary group added after it started
  (`[mem:nobetci-zamanlanmis-giris-grup]`; fix: `sg` re-exec once, otherwise refuse to start).
- **Write times from `date`, never from feel** — the supervisor wrote future times four
  times in one day (`[mem:saat-tahmini-yazma]`).
- **Long-lived background work must be a systemd unit** (`systemd-run --user --unit=…
  --collect`), never a shell job of an interactive session (`[mem:nobetci-oturumdan-bagimsiz-is]`).
- **Model health checks must be a real request** (vLLM `/v1/models` returns 200 while the
  engine is dead) (`[mem:vllm-motor-olumu-sessiz]`).
- **Policy refusal is not a stop**: if the repo progressed it is `found` with a refusal count.
  Counting every denial as a stop would stop the agent every night.

---

## 2. Incident catalogue (by class)

Format per class: examples (date, symptom, root cause, fix commit/record), then the
operator's verdict where there was one.

### 2.1 Deadlocks and silent stalls inside the cycle

**a) `closure_no_progress` — closure findings nobody can fix.**
- *09-29 F-1.5-regression-tests r4–r6.* Gate green, BLOCKER/P1 0; only open item a closure
  P2: "the closure artifact and ledger row carry no 21-row `OB-1.5-<nnn>: <node id>` map and
  no `code fixed` lines". Closure findings never go to the builder (operator ruling
  09-09); the closure-only round regenerates the closing draft mechanically and cannot
  produce that text → same closure key → `closure_no_progress` 15:21. Carried by operator
  (`ops/approvals/2026-09-29-regression-tests-carry-closure-p2.json`); supervisor wrote the
  map by hand.
- *09-30 F-1.2-6e-trigger-guard r2/r3*: closure P2 about the closure *prose* — same stop,
  carried (`ops/approvals/2026-09-30-trigger-guard-carry-closure-p2.json`).
- **Root cause:** the task draft made builder-only text an acceptance condition, and the
  only party allowed to fix closure text was a mechanical generator.
- **Fix:** operator option A — closure text is deterministic: `bin/nb_row_map.py` builds the
  row→test map from the contract + AST of changed tests; plus **one bounded "closure-text"
  builder call** that must return a patch whose tree equals HEAD (any product change is
  refused, `closure_text_patch_refused`), capped 2 per task/HEAD, transient failures return
  their admission (`docs/2026-09-30-deterministic-closure-text.md`, `10aabf8`, `2511d88`,
  closure-call installed 09-30 20:22). Then routing: a closing-record remedy goes to the
  closure-text path unless it names a product path (`4621143`, `479907d`, absolute-path gap
  `d06f539`, `c1684f9` — R58, merged and installed 2026-10-01 14:28).

**b) `closure_material_missing` after an errored build.** *10-01 F-1.6-5*: first build cut by
"Upstream idle timeout" (70 turns, 1 file), continuation cut again (160 turns, 6 files, no
tests) → candidate changed but never reviewed → cycle went to gate+closure with nothing to
close → held 06:33–07:42. **Fix** `2eac5d3`/`20488b2`: an errored build with a changed,
never-reviewed candidate gets one builder continuation (resume entry
`error_continuation`); gate repairs excluded. Operator approved extra continuations via a
one-shot env var, unset right after (`NB_ERROR_CONTINUATIONS=2`, then 3).

**c) `build_no_changes` / `repair_no_changes` routing.**
- *09-26 F-1.5-audit*: round-6 build fixed a P2 by editing a test but did not regenerate the
  builder-written audit package (3 bound sha256 fields) → gate failed on
  `test_committed_package_equals_the_rebuilt_one`; gate repair scope limited to `tests/*` →
  GLM 475 turns, **no change** → held 05:49, missed by the supervisor for ~6 h (R26).
  Operator "2 olur": framework regenerates the package itself.
- *09-26 test-clock*: gate-repair build `01afe966` — **595 turns, 64 permission denials,
  0 edits**: the builder never ran a test (only `/srv/nobetci/venv/bin/pytest` was allowed;
  it kept writing `python -m pytest`, `git -C`, `python3 -c`) and never found the time bomb
  (`_AT` fixed at 2026-09-19 with a 7-day horizon vs the real clock) (R30).
- *10-01 F-1.6-3 r4*: `build_no_changes` because a closure finding whose fix targets the
  closing record was routed to the builder (fix `479907d`).
- **Fixes:** `base_broken` (re-run failing nodes on base; same failures → no repair, alarm)
  `1214153`; runaway watchdog — zero edits, ≥50k chars, 30 min → self-evidenced
  `controller_runaway` disposition, but never stop a call that has already edited (`7d86536`,
  `3790ad2`); degenerate no-change repair retries as `provider_transient` (`5149deb`); honest
  tool list in the builder brief (pipelines allowed; repo is cwd, no `git -C`).

**d) Rebase died mid-rebase on SIGPIPE.** *09-29 16:50/17:24*: F-1.5 resume left the product
repo mid-rebase (`preflight_failed`) because `bin/nb-rebase-candidate` ran
`git log | head -10` under `pipefail` with a candidate 13 commits behind → SIGPIPE.
**Fix** `b849e7d`: no pipes, ERR/EXIT guard aborts and names rc/line, cycle stops
`candidate_rebase_left_in_progress`; test red 13 on old. Operator had to authorize the
`rebase --abort`. Follow-up incident: the marker rebase left 12 unresolved conflict markers
in the ledger, caught by the phase reviewer r8 (R44).

**e) Held admission → silent death.** *09-18*: F-1.4-4 delivered 23:58; at 01:00 the batch
held on an accounting guard ("unpriced usage must match reserved supervisor calls"),
`nb-batch` exited 1, the unit had no `Restart=` → **dead 9 h**. The fix was already
committed but not installed (`[mem:nobetci-tutulan-admission-sessiz-olum]`, `8e1957a`).
Decision: *do not* add auto-restart (re-trying a held admission corrupts accounting); what
was missing is supervision. Post-delivery accounting is the most frequent hold site
because the cycle rarely exercises that path.

**f) Holds nothing resumes.**
- *09-25* merging a sweep PR into product main while a candidate was open → `nb-merge`
  refused `behind_base` (correct) → terminal `merge_failed` → watcher: "not an evidence
  wait" → never resumed. A plain service start rebased and continued
  (`[mem:nobetci-main-merge-behind-base]`). Lesson: merge to product main between tasks.
- *09-28* watcher stopped deployment probing once the hold reason text changed; cycle
  restarted by hand (R37). Astra counted the watcher saying "not an evidence wait" 97
  times on 09-28 while doing nothing (`docs/2026-09-28-review-gate-rethink.md` §3).
- *09-14* the original: a held batch stopped the service and nobody resumed it → the hold
  watcher (`bin/nb-hold-watch` + 5-min timer) was born.
- *09-21 03:48* a task the supervisor added was not in the authorization list →
  `scope_violation` → stopped **4 h 12 min** until the operator asked "what's the status"
  (`[mem:nobetci-birincil-gorev]`, fix `e910a7c`). Rule since: every R-/RR-/F- id a task's
  text names must be in the authorization; rehearse queue changes by running the first
  step for real.
- *09-29 05:53* task draft told the builder to edit `docs/deployment.md` (canon); two builds
  (03:01, 05:02; 520 turns, 12 files) finished the work and both were refused
  `canon_touched` → held for operator (`[mem:nobetci-gorev-taslagi-canon-yasagi]`; fixed by
  operator-approved `task_conf_amend`).

**g) Cooldowns and floors that strand the cycle.** 900 s failure cooldown after an orderly
staged install (09-30, `1cf7c39`); boundary resume of a rollout-opened boundary needed the
explicit request + 60 s floor (`39c13f1`, `1eb93a6`); `launch cooldown until <ts>` became an
explicit hold reason the watcher resumes on (`f973854`).

**h) Host restart mid-build.** *09-20 14:08* unexplained reboot → half build → `held: worker
intent unsettled`; supervisor session also died. Operator: a machine-cut build documents
itself like a wall-clock cut — disposition `host_restart`, proven mechanically (call start <
kernel boot time, no terminal in trace, no live worker), charged once, interrupted, partial
work not imported (`[mem:nobetci-host-restart-kurtarma]`). Second defect: resume on the
task's own empty branch refused `resume_branch_empty` (`11e20c5`). Extended to supervisor
calls (`756a223`). Reboots on 09-26 16:28/16:44 and 09-27 18:01 hit again. Also: a companion
container (embedder) did not come back after reboot (`--restart unless-stopped` does not
retry a failed boot start); the symptom surfaced *days later* on the first deploy that
changed the corpus (`[mem:nb-embedder-reboot-yarisi]`).

**i) Reconcile matched itself.** The process scan for a live call matched the supervisor's
own shell command line containing the call id — run such tools from a script file (R34).

### 2.2 Provider / model failures

**a) Degenerate output.** *09-28 16:35* off-topic output; *17:27–19:38* word salad from one
OpenRouter upstream (OpenInference). Fix: operator blacklisted it (sudo
`install-build-provider`; `ops/approvals/2026-09-28-ignore-openinference.json`), observer
records `upstream_provider` per response; degenerate no-change repair = `provider_transient`
(`5149deb`). Lesson: you cannot judge a route without knowing *which upstream* answered —
the observer originally recorded only the routing policy.

**b) 4xx protocol errors.** Provider 4xx protocol refusals → retried as `provider_transient`
(`7d86536`). But: a structured `400 invalid_request_error` means "stopped before work" and
is only a disposition, *never a fallback trigger* (`9941be0`, `e9ba2f4`); a "usage limit"
answer is a refusal only with proof of no model work (`ccad9e8`, `c23a2f0`). GLM 5.3 on
OpenRouter cannot disable `reasoning` (400) (`a00c8f1`). OpenAI strict structured-output
dialect: no `oneOf`, no lookaround, all fields required/nullable, Python `\s` ≠ ECMA (`57958bf`).
Lesson recorded: when a schema changes, fire a live probe before installing.

**c) Upstream idle timeout (Relace, single-chunk tool arguments).** 94 builder calls since
09-25: all 6 "Upstream idle timeout exceeded" cuts had **Relace** serving the last turn.
Relace (and DeepInfra) deliver a tool call's arguments as one chunk at the end; a large
`Write` produces >120 s of stream silence and the router cuts it. InferenceNet streams 18
chars/chunk: 201 large tool calls, 0 cuts. Relace excluded 10-01 10:06
(`ops/approvals/2026-10-01-ignore-relace.json`; `NB_BUILD_IGNORE_UPSTREAMS=Fireworks,BaseTen,OpenInference,Relace`,
price caps 0.15/0.50, sort price). DeepInfra on watch (R55). Earlier: idle timeout made
`provider_transient` (`7015c2b`). The *first* analysis (R50) correctly refused to blame Relace
yet because Relace served every chain — causality was only accepted with the chunking
mechanism + 0/201 control.

**d) GitHub 5xx.** *09-30 07:10* push `[remote rejected] (Internal Server Error)` →
`nb-merge` rc 2 → batch held, service failed; status page green; manual restart → merged in
16 s. Fix `8dcf4de`: bounded retry on remote 5xx for push / `gh pr create` / `gh pr merge`,
done-probe after the last attempt, `network_transient` on exhaustion. Recovered two more
5xx in production the same afternoon (R47).

**e) Key / credit / quota exhaustion.**
- OpenRouter key hit its $30 total limit mid blind run (78 calls 403) — and the builder
  shares the key, so the builder was blocked too (09-16). Key-limit = `provider_limit`, not
  a failure.
- Codex workspace credits ran out repeatedly (09-25 13:07, ~17:2x, ~20:10) → reviews fell
  back to Opus 5.5; operator: "no credit refill, it returns at 16:16" (R17). Rule R13/R54:
  check Codex limits before a phase audit admits.
- `nb_transient.py` (09-15): stage-based provider/network signatures, bounded timed retry,
  honors the Claude reset time; hold writes the retry plan; watcher resumes after it.

**f) Model routing leak.** Planner (Opus) wrote a "re-review round 1 — Sonnet" step; the GLM
builder executed it as `Agent(model=sonnet)` inside the sandbox; the sandbox's only
credential was OpenRouter → Claude models billed on OpenRouter
(`[mem:nobetci-yonlendirme-siniri]`). Fix: `--disallowedTools Agent`, aliases pinned to GLM,
`nb_routing_verify` compares returned `modelUsage` to the requested family →
`routing_violation`; host roles refuse to run if any `ANTHROPIC_*` is set (subscription
only; if the subscription is exhausted work *stops*, never falls to paid API).

**g) Retired/unsupported models.** Ollama Cloud retired a pinned discovery model (HTTP 410)
→ daily product timer failed → every deploy verify showed DRIFT → 7.5 h stall (R37).
Measured: Ollama Cloud enforces no JSON schema at all; 240 earlier "passes" were
prompt-following false positives since a canary probe was added (R4).

**h) Local model quirks.** Local vLLM `/v1/messages` works for the sandbox CLI only with
`--effort medium|low|xhigh` (`high` → 400); one-sided rollout of the effort field needed a
compat fix (`8668068`).

### 2.3 Cost blowups

**a) Blind inspect resent full history — 175 M tokens per pass.** 09-27 run on tree
`951fb5c7`: 212 items asked, **6,926 calls, 175.2 M prompt / 3.1 M completion tokens, $10.69,
140 min**; median 31 turns and 658 k prompt tokens per obligation for a ~200-token brief;
161 `unknown` (60%). Root cause: `bin/nb_blind_inspect.py session()` resent full `msgs` each
turn (~95% repeated context), and price-sorted routing across ~29 providers defeated
caching (`[mem:nobetci-kor-dogrulama-maliyet-kok-nedeni]`). The first live inspect run
before that (09-25 20:22) produced **0 verdicts**: grouped sessions never answered in group
format, 60k-char tool results re-sent each turn (1.44 MB prompts), reservations 230× actual
cost → `budget_anomaly` stop (R23). Operator verdict 09-28: **"$40 review için kabul
edilemez"** ("$40 for a review is unacceptable") → gate rethink → simple gate.

**b) Phase reviewer resumed session — 31 M tokens in one round.** F-1.5 simple-gate round 7
(09-30 01:07): one Claude session resumed since 09-25 carried **474 k tokens** of history
into its first turn and re-sent it on each of 59 turns → 31.3 M input (30.7 M cached), CLI-est
$11.44; 90–99% of every phase-review round's input was carried history; first-turn context
grew 138 k → 877 k over the session. It also launched the full suite with `nohup` (one
inspection capped at 45 s) and polled it with `sleep; tail` — each poll re-sending ~535 k.
Fix (operator-approved `ops/approvals/2026-09-30-builder-exploration-review-cost.json`): fresh
phase-review session per round + controller digest (step 4, `1793f8a`), controller gate
per-test results handed to the reviewer (step 2). Estimate 50.3 M → 7.3–9.0 M for the same
three rounds.

**c) Double-counted review cost.** Claude CLI `total_cost_usd` is the **session** total, not
the call's. Summing it over resumed calls inflated per-task review cost: $54.66 recorded vs
$27.56 per-call (09-28..30); e.g. five review calls recorded 1.56+1.96+4.24+4.85+5.21 =
$17.82 where deltas sum to $5.21. Phase review had the same bug earlier (9.6× inflation;
`64fb6e3` "records the call cost, not the CLI running session total"). Also: the CLI prices
GLM as Claude — ~20–50× the real bill (CLI said $792 for 62 builds ≈ $23 real; $192.54 for
10 builds vs $6.45 real) → `bin/nb_prices.py` list-price estimate calibrated against the
key's own usage (`54fc38e`). Lesson (Fable 09-17): "an estimate not audited against the
invoice is the same defect with a nicer name."

**d) The sweeper was most of the spend.** 09-21: sweeper $16.8 (flash 7.0 + **full
`z-ai/glm-5.3` review 9.7**, 0 fixed, 0 PRs) vs cycle builder $5.5. The supervisor had told
the operator the sweeper ran on the local model — it did not; it used the same OpenRouter
key (`[mem:nobetci-harcama-dagilimi]`). When the supervisor's session died on 09-21, a
pending role-key rollout died with it and the sweeper burned ~$14 more on full GLM 5.3.
Operator verdict 09-21 16:45: **"never use full GLM 5.3 again, always flash; sweeper stays
on local Qwen; do not sort by throughput, use cheap and healthy APIs"**
(`[mem:nobetci-model-ve-rota-kararlari]`).

**e) Blind verifier on full GLM.** 09-16: operator first approved full GLM 5.3 for blind
("builder flash must not judge its own code"), then within hours: "6 $ yakmış, flash kullan"
("it burned $6, use flash"). Flash blind: 11 min / $0.28 vs 17 min / $1.82.

**f) Audit far more expensive than the build.** F-1.5 build (7 tasks, 12.8 h) cost $6.89
cash; its audit had already cost ~5.6× the cash and ~5.8× the time and was still open
(`docs/2026-09-28-review-gate-rethink.md` §2).

### 2.4 Supervision misses (multi-hour stalls nobody saw)

| When | Stall | Cause | Who noticed |
|---|---|---|---|
| 09-16 02:33 | ~8 h at maintenance boundary | supervisor edited a staged file | morning check |
| 09-18 01:00 | 9 h dead unit | held admission, unit exit 1, fix not installed | operator |
| 09-20 14:08 | 22 min + session death | host reboot | supervisor |
| 09-21 03:48 | 4 h 12 min | `scope_violation` (task id not authorized) | operator "ne durumda" |
| 09-21 18:24 | 4.5 h | supervisor session ended; watchers + `--wait` rollout died; `gate_diagnostic_required` (rule written too broad; `73900b5`) | supervisor on return |
| 09-22 20:36 | ~12 h | grant orphaned by script reinstall | morning |
| 09-26 05:49 | ~6 h | `repair_no_changes` on the audit package | supervisor (R26) |
| 09-28 02:10 | 7.5 h | `deploy_failed` (retired model → DRIFT); stop-watch fired but nothing wakes an idle session | supervisor (R37) |
| 09-20/21 | 10 h | embedder container down after reboot | first corpus deploy |

Operator verdicts, repeatedly (`[mem:nobetci-birincil-gorev]`): *"your main goal is to keep
watching the auto dev cycle, apply the needed fixes and make sure it is running … why do I
have to tell you every time"* (09-17); *"are you watching the cycle? how many times do I
have to say that monitor must never close"* (09-18). The operator saying "the cycle
stopped" is a **defect report** against both the cycle and its watcher.

What fixed it, in order: a persistent Monitor re-armed every expiry (≤30 min); a health
watcher that exits and wakes the session on two consecutive `attention` states; a framework
stop-watch unit; then the real fix — **framework-owned pager + wake unit**: hold > threshold
pages with an actionable reason, durable notifier outbox retried until acknowledged
(`6c561f5`, `f854933`, `808ad74`, `20ba075`; `nobetci-hold-wake.timer` restarts the watcher
within 15 min; pausing is a dated file ≤24 h ahead, not stopping a timer — `CLAUDE.md`).
Also: the Telegram text can state the wrong cause (09-21 said "external evidence missing");
always verify from the journal. A `tail | grep` monitor missed a step once → offset poller
(R45).

Root lesson: **an unattended system cannot depend on an interactive session for liveness.**
Everything that must survive a session (watchers, rollouts) is a systemd unit; paging is
part of the framework, not of the supervisor.

### 2.5 Install hazards

- **Staging broken by merges** (09-30 13:35, ~20 min down) — §1.11.
- **Back-to-back installs** → failure cooldown (09-30 11:50, ~7 min) — `1cf7c39`.
- **Test processes mistaken for runtime**: `install-phase-audit-profile --wait` took a
  boundary then gave up because a reviewer's *test* `nb-cycle` looked active (09-30 17:10,
  service failed ~3 min). Fix: `nb_boundary.busy` counts only the installed runtime
  (runtime `nb-cycle` pids, `nbbuild` worker, `batch.lock`, `batch_held`); `settled()`
  re-checks up to 15 min; `give_up()` asks resume (`5d86328`); every installer uses the one
  `busy()`, probes fail closed, a completed batch is a settled boundary (`5c04055`, `a96d130`).
  Earlier the same class in tests: "two tests measured the host's processes; a sandbox test
  measured the product checkout mid-round" (09-17).
- **Lock truncation**: `nb-deploy --probe-existing` opened `cycle.lock` with truncation/
  creation; every cycle start logged "dead lock: pid ?" `interrupted_previous`. Fix: probe
  never creates/truncates, stands down under a live cycle; `nb_lock_acquire` takes over an
  *empty* lock silently (`bcaa2c2`, `f60597a`). Earlier: lock was "exists? then write"
  (TOCTOU) → `set -o noclobber`, 40 concurrent racers, no double winner.
- **Grant orphaning** on script reinstall — §1.11 (`18b2b16`).
- **Rollout semantics**: `--wait` waited for "no controller" which never happens; now it
  writes a maintenance request and stages accept itself (`47d8744`). Boundary lease: watcher
  must not resume while a root rollout holds a live lease, and sweeps dead leases (`3d0de89`).
- **Root rollouts need the operator's sudo**; the supervisor prepared scripts
  (`/home/mega/nb-step2.sh`, `nb-relace.sh`) and the operator ran them as
  `sudo systemd-run …` units.
- **E2BIG**: a 139 KB prompt on argv → rc 126 read as "no change"; prompts now go via stdin,
  plus a prompt-size gate and `controller_dispatch_failure` disposition (09-15).
- **Sandbox env**: the sandbox unit did not pass env to the worker, so a wall-clock setting
  never reached it (build 13 cut at 5400 s) → `runs/<id>/in/limits.json` (`bd36dfa`).
- **Controller-launched drivers died with the service cgroup** → `systemd-run --user` (`56bb0b3`).

### 2.6 Review-gate over-engineering (the biggest lesson)

**Chronology.**
1. *09-13..16 Phase 1.3 audit*: builder-written acceptance package (JSON + a product test
   that rebuilds it and pins bytes) + phase auditor + blind verifier (local Qwen, then GLM).
   86–88 h wall, 27 review rounds, 10 builds. Accepted 09-16 14:40 — but with all four
   §6.5 checks `unavailable` in the package.
2. *09-16..24*: package-pin loop: every ledger row changed bytes that the previous phase's
   package pinned → ordinary tasks broke the 1.3 package test → 16 of 17 F-1.4 gate failures,
   9 of 12 tasks, ~650 min, 44 M tokens (`docs/2026-09-20-cycle-time-token-analysis.md` §3.3;
   fixes `a92036e`, `3c21c4e`, then 09-23 "sharded audit" redesign and ~10 Astra ledger/pin
   review rounds on 09-24). Package repair was 66% (1.3) / 94% (1.4) of audit build cost.
   1.4 blind verification never checked the canon's 15 criteria at all (package used
   `criteria`, reader expected `scenarios`) (`[mem:nobetci-shard-denetim-mimarisi]`).
3. *09-25*: live blind "inspect" route (agentic, per-item sessions), 11 review rounds to
   merge; first run 0 verdicts; fix rounds; 96.8% unavailable on the old tool-less route.
4. *09-26*: blind v2 design (NCC classification, record_prose, scope rulings) → "hard gate
   over frozen atomic obligations" (269 obligations, rev 6 frozen, sha recorded) → 15+ slices
   (O1..O6) each reviewed → calibration campaign with labelled pairs ($3.80/$6; pair 3 found a
   real false pass) → rulings A–D (two-session pass confirmation, etc.).
5. *09-27 18:41* obligation gate active; stopped twice within the hour (missing installer
   files; reviewer emitted a forbidden status). Astra: **"not suitable for unattended
   development in its current form"** (`docs/2026-09-27-astra-unattended-assessment.md`).
   P0 list of 7 fixes, cycle paused, installed 21:05.
6. *09-28*: measured 7 days: **running 50.9 h; gate machinery's own stops ~88 h; real
   blockers ~27 h**; 62–64% of 55 audit findings were about the builder-authored package;
   only 8 real product defects + 5 cross-task defects found by the phase audit. A test-
   deletion cheat (F-1.3-011) slipped through as a banked P3. Spec for an 8-step "evidence
   gate" (coverage per test, mutation checks, residual judge, generated customer bundle).
7. *09-28 17:00*: operator rejected the 8-step evidence gate too.

**Operator verdict (verbatim, 09-28, `docs/2026-09-28-simple-phase-gate.md`):**
> "abartmadan yalnızca sağlıklı bir dev-cycle'ı garantileyecek şekilde, her türlü development
> taskine ve subphase'e uyum sağlayabilecek ... en basit yapıda ... şu ana kadarki
> geliştirmelerimiz çoğu subphase kapısına zarar verdi. kolaylaştıralım. detaylandırdıkça
> zorlaşıyor. subphase gateleri anlamlı ama kolayca geçilebilir hale getireceksin."
>
> ("Without exaggerating, in the simplest structure that only guarantees a healthy dev cycle
> and fits any development task and subphase … most of our developments so far have harmed
> the subphase gate. Let's simplify. The more we detail it, the harder it gets. Make subphase
> gates meaningful but easy to pass.")

Also 09-28 intent answers: gate must prove (a) the subphase is really done, (b) builder
errors/test cheating/fake evidence do not pass, (c) it runs in the real environment, (d)
evidence presentable to an enterprise customer; **$5–15 per subphase audit including all
repair rounds**; page on every *real* blocker, not on noise; **deterministic evidence is
enough**, the model looks only at the residue.

**The simple gate (installed 09-28 19:39, profile 1.5.gate=simple 22:10;
`ops/approvals/2026-09-28-simple-phase-gate.json`):**
1. ≤25 short testable claims (≤12 blocking) from the roadmap rows, each with a source
   pointer; operator/reviewer may edit; no freeze machinery.
2. Deterministic checks the cycle already runs: full suite green on Spark and edge;
   test-deletion guard (in shadow, findings to the reviewer); the real-environment receipt
   the profile names exists, is successful and current for this candidate.
3. One reviewer (phase auditor, subscription) proves claims (optionally via sub-agents):
   test id + output + code lines; writes one table claim → evidence → pass|missing|fail; for
   each blocking claim says which wrong implementation the evidence would catch.
4. All blocking claims pass → close; the table is the customer report. Otherwise repair;
   after a repair the reviewer reads the whole repair diff and re-checks every claim it
   touches. Wrong-owner problems (access, env, tooling) go to their owner via hold+pager,
   never as a builder repair.

**Result:** F-1.5-audit — stuck for ~5 days under the old machinery — closed 09-30 02:33 on
the simple gate (PR #646), gate 7180 passed each round; afterwards 12 tasks delivered on
09-30 alone. R8/R12 (standing grants for blind, assembler activation) closed permanently by
the operator: "Close both permanently" (`ops/approvals/2026-10-01-close-r8-r12.json`).

**But the simple gate's own lesson (10-01):** with the computed checks gone, receipt
*content* is no longer checked mechanically — a lab-deploy receipt with outer `observed`
and inner `stale` passes; the digest covered `*.py` only; `.source-ref` written before
install (`docs/2026-10-01-lab-deploy-recovery-design.md` F1–F3). Fix D-a: a small receipt
predicate in the gate profile (`ops/approvals/2026-10-01-lab-deploy-receipt-rule.json`).
Simple ≠ trusting envelopes.

---

## 3. Measured numbers worth keeping

All from the cited documents; "CLI-est" = Claude CLI `total_cost_usd` (API-equivalent for
Opus on subscription — not cash; Claude-priced and ~20–50× too high for GLM). "prov-est" =
list-price estimate calibrated to the key's usage (within ~3%).

### 3.1 Where cycle time goes (F-1.4-1..11, 65.3 h inside runs; per-task median 5.4 h)

| Stage | Share of wall | Median/call | Calls per task (median/max) |
|---|---|---|---|
| build (isolated) | 69.6% | 31 min | 5 / 11 |
| product gate (full suite) | 16.2% | 9.9 min | 5 / 15 |
| review (Opus) | 6.0% | 5.7 min | 3 / 8 |
| plan (Opus) | 2.7% | 9.8 min | 1 |
| ledger gate | 2.4% | 10.7 min | 1 |
| deploy/edge/merge | 2.2% | — | — |
| controller overhead | 0.4% | — | — |

- 92% of build wall is model generation/waiting; tools 8% (pytest 9,565 s of 133,716 s).
  Setup (clone + mirror + patch export) ≈ 2 s/call — never the problem.
- Later split (9 observed builds): generation 63%, waiting for next response to start 28%,
  tools 9%; thinking = 67% of generated characters (~42% of build wall). Waits: median 1.5 s,
  p90 6.4 s, p99 34 s. The "17.9% silent stall" of the morning analysis was the *old
  observer* not seeing streamed thinking — not a stall. Lesson: instrument before you
  optimize.
- Spark gate median 436 s (09-13) → 680 s (09-20) while tests grew +14%; edge runs the same
  suite in 128–182 s. Parallel gate later 136 s vs ~700 s serial (O-7).
- Full-suite pytest inside the builder: 6 runs ended at 598–601 s (the Bash tool's 10-min
  cap) — the suite needs ~690 s; always wasted → brief says the controller runs the gate.
- Tokens by role (09-16..20, 1217 M input): build 62%, sweeper 16%, review 15%, plan 6%.
- Phase audit wall: F-1.3 86–88 h, F-1.4 ~70 h, F-1.5 >74 h under old gate; 09-26 latency
  research over 184 h: 30% ownerless hold, 23% evidence wait, 20% framework block, 18%
  productive; reviewer model time only 2–6%.
- Unattended 7 days to 09-28: running 50.9 h / gate-machinery stops ~88 h / real blockers ~27 h.

### 3.2 Builder (GLM-5.3-flash via OpenRouter)

| Metric | Value | Source |
|---|---|---|
| Turns per first build, median / max | 145 / 575 | 09-30 doc (11 first builds) |
| Turns per repair, median / max | 35 / 645 | 09-30 doc (19 repairs) |
| Record turn count | 1,036-turn build (no turn rail then) | 09-20 doc §3.8 |
| First-build time to first edit, median | 232 s (30 pre-edit read actions) | 09-30 |
| Repair time to first edit, median | 550 s (09-20) → 199 s (09-30); one repair 99 s after context program | 09-20, 09-30, context program |
| Extreme first-edit | build 17: 125th minute (151 min, 356 turns); build 18: 25th minute after hints | `[mem:nobetci-fable-devir]` |
| Repair pre-edit reads hitting an earlier round's file | 72% (09-20) → 80% (09-30, trail blind to Bash reads) | both docs |
| Share of repair wall before first edit | 40% (7,252 / 17,919 s) | 09-30 |
| Builder cash | 62 builds ≈ $23 prov-est (CLI said $792); 30 calls 09-28..30 $10.10 (CLI $210); ~$1–3 per task | 09-20, 09-30 |
| Builder bill composition | cached re-reads 66% / uncached 24% / output 10% (09-21); cached 91% / uncached 5.5% / output 3.2% (09-28..30) | spend note, 09-30 |
| Context per turn | ~78 k re-sent per turn; context grows 25 k → 130 k+; cost ∝ turns² | 09-20, spend note |
| Fixed overhead (system prompt + tool schemas + our prompt) | 24–35 k tokens per turn = 36% of builder tokens | 09-30 |
| ≥200-turn calls | 5 calls = 68% of tokens, 54% of builder wall | 09-30 |
| Auto-compaction | 6/30 calls compacted; 3 before first edit | 09-30 |
| Cache-miss responses | 7% of responses read full context uncached (65% of uncached input); per-upstream cache-hit 93–98% (Z.AI/Together), 75–86% (CoreWeave), 19–33% (Fireworks) | spend note |
| Permit denials | 72 in sample (`git -C` 14, `python -c` 13, `ls && …` 8); TaskCreate/Update bookkeeping 296 turns | 09-20 |
| Unproductive builds | 16 (errors/interrupted/no_changes) = 538 min, 14% of build time | 09-20 |

### 3.3 Reviewers / planners (Opus 5.5 on subscription; CLI-est = API-equivalent)

| Metric | Value |
|---|---|
| Per-task first review | 7–23 tool calls, 0.5–2.8 M tokens; fresh session 33–48 k first turn |
| Re-review (resumed) | 1–10 tool calls, 0.27–2.3 M tokens, 84–285 k carried |
| Plan | 14–81 turns, 0.6–10.7 M tokens |
| 09-28..30 per-call CLI-est | plan $25.13, first review $12.60, re-review $27.56 (budget recorded $54.66 — double count) |
| Review rounds per task | median 3; 32% of findings first appeared in round ≥3; 64% of F-1.4 repair rounds were P2-only |
| Phase reviewer session (F-1.5) | CLI-est $56.30 since 09-25; round 7 31.3 M tokens / $11.44; first-turn context 138 k → 877 k |
| Opus plan+review over 09-17..20 | ≈ $260 CLI-equivalent (larger money line than builder, but subscription, not cash) |

### 3.4 Verification machinery

| Run | Numbers |
|---|---|
| First paid blind (full GLM) | 17 min, $1.82; key's $30 limit hit mid-run |
| Flash blind | 11 min, $0.28 (154 pass / 3 fail / 26 unavailable) |
| Local Qwen blind (bound evidence) | 38 min; attribution accuracy ~99%; Qwen good on bound evidence, bad on "absence" (6 of 10 fails were absence) |
| Blind inspect pass 09-27 | 6,926 calls, 175 M prompt tokens, $10.69, 140 min; 161/212 unknown (31 cut by the $15 rail) |
| Per-item blind cost (measured) | ~$0.004–0.0066/item; grouped was more expensive and fragile |
| Retro 1.1/1.2 blind by Opus | 16 calls $3.30 / 18 calls $3.89 |
| Obligation calibration | $3.80 of $6 campaign |
| F-1.5 cash opening balance (09-28) | $38.39 (builder 5.04 + blind 33.35) |
| Phase audit value | 55 finding ids over 1.3–1.5: 8 real blocking product defects, 5 cross-task defects only a phase audit could see, 34–35 (62–64%) about the package/docs/citations/ledger |
| Per-test coverage overhead | 425 s vs 402 s plain on 6768 tests |

### 3.5 Delivery throughput

Task deliveries per day (count of `var/deliveries/F-*.json` by their `at` field, read-only;
55 task deliveries + 1 sweep-deploy total):

| Date | Deliveries | Date | Deliveries |
|---|---|---|---|
| 09-10 | 1 | 09-21 | 3 |
| 09-11 | 1 | 09-22 | 1 |
| 09-12 | 2 | 09-24 | 5 |
| 09-16 | 2 | 09-25 | 6 |
| 09-17 | 3 | 09-28 | 3 |
| 09-18 | 2 | 09-29 | 3 |
| 09-19 | 4 | **09-30** | **12** (first full day on the simple gate + fixes) |
| 09-20 | 5 | 10-01 (to ~13:00) | 3 |

- F-1.4-14 delivered in ~75 min vs 5.4 h median after the context program (09-20).
- Product test floor grew from 5,073 (Spark) / 5,085 (edge) on 09-05 to 7,180 (F-1.5) and
  7,334 (F-1.6) — every finding brings a regression test, so the floor is a floor, not an
  equality (Spark and edge collect different counts on the same tree: separate floors).
- Framework: 808 commits since 09-20, 241 `fix(...)`; ~175 files in `bin/` (~39 k lines). In
  the 09-22..28 week, 163 of 575 commits were blind/obligations/audit/ledger/gate machinery —
  most of it later switched off.
- Framework test suites: unittest grew 177 → 1,560 OK (09-29); ~900–950 bash probe cases.

---

## 4. What worked well, and what was a mistake

### 4.1 Keep (proven)

1. **The simple subphase gate** (§2.6). Closed a 5-day-stuck audit in hours; 12 deliveries
   the next day. Claims ≤25, deterministic checks first, one reviewer with a table, repairs
   re-check touched claims, wrong-owner problems paged not built.
2. **Deterministic ledger** (`nb-append` + `nb-ledger-row --pre/--fill`, no model): full text
   reviewed before deploy, only mechanical fields filled after. Product proof is
   `var/deliveries/<task>.json`; `state.json` is scratch (`[mem:nobetci-ortak-devam-yolu]`).
   Same for closure text since 09-30 (`nb_row_map.py`). "The ledger stays task-conf +
   mechanical; no builder prose."
3. **Isolation by identity, patch-only boundary.** Builder in a separate uid/cgroup/clone;
   controller applies the patch to a temp index; canon decided by a definition materialized
   from HEAD; acceptance recorded where the builder cannot write. Reviewer is unprivileged
   (read-only worktree, no network, no credentials); live proof comes from controller-run,
   grant-bound evidence jobs with receipts. Accepted risks recorded, not re-debated
   (builder can read/exfiltrate its OAuth credential; gate counts come from builder code =
   signal not proof; edge `NOPASSWD`).
4. **Closure-text call** (bounded, tree must equal HEAD, cap 2/head, transient retries return
   admission) — converted a recurring operator-carry deadlock into an automatic path.
5. **Dossier / notes / trail** for builders: plan sidecar `.nobetci/plan.md`, progressive
   notes `.nobetci/notes.md` copied even when a call dies, trail that includes Bash reads,
   mechanical task dossier (gate failures, plan anchors, symbol ranges, test nodes, diff
   stat, ≤8 kB). First production dossier 4,302 bytes (09-30 23:28). (Effect still being
   measured: R52.)
6. **Transient classification + bounded timed retries** (`bin/nb_transient.py`): provider
   limit / provider transient / network transient per role, honoring reset times; idle
   timeout, 4xx protocol, degenerate output, remote 5xx all moved from "batch hold" to
   "retry later".
7. **Hold watcher** (`bin/nb-hold-watch`, 5-min timer) + wake timer + pager with durable
   outbox; resumes on signals (new revision, receipt, retry time, resume-request file);
   pause is a dated file. Removed the manual-restart routine for evidence holds (09-14).
8. **Immutable manifests + dated approval records** (`ops/approvals/*.json`, kind
   `operator_ruling`): every operator decision is data with scope and date, bound into the
   revision and visible to the reviewer packet (PLANNING_CANON §11). Task-context kinds
   (`task_add`, `task_closes`, `task_conf_amend`, `finding_carry`, `task_rows`,
   `authority_findings`, `task_hold/release`, `task_prerequisite`) let the queue change
   without editing state.
9. **Reviewer severity policy**: P1/BLOCKER block; P3 advisory and banked to the sweeper;
   ordinary P2 may carry after ten repair/review pairs; escalate `review_fail_3x` only when
   the *same concrete blocking findings* repeat across three calls (direction/severity
   trend, not round count — "BLOCKER>BLOCKER>BLOCKER" fired correctly on 09-05).
10. **Session isolation for planner/reviewer**: `--settings '{"claudeMdExcludes":[…]}'` +
    `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` → `foreign_reads 0`. `--bare` was rejected because
    the installed CLI's own help says bare mode never reads OAuth (the docs summary said the
    opposite): check the installed binary, not documentation. (`/home/mega/AGENTS.md` now
    begins with a scope note so cycle roles ignore it — a planner had read the supervisor's
    work queue on its second tool call.)
11. **Context as content + context metrics** (`var/context-metrics/<task>.jsonl`: canon
    reads, foreign reads, tool calls before first edit) — every change measured before/after.
12. **Rebase handling**: mechanical candidate rebase when main moves under it; on conflict
    abort cleanly and let the builder resolve with markers; rollback ref `<branch>.pre-rebase-<date>`.
13. **Sweeper as a "fly"** (operator 09-17): parallel, never blocks anything (only delivery
    steps and a pending boundary stop it), interruptible, worktree disposable, no receipt =
    no work. 15 PRs merged on 09-22. Its PRs need an edge-gate check (`[mem:sweep-pr-edge-kisiti]`:
    a sweep test used `git ls-files`; edge has no `.git`).
14. **Bounded-retry + done-probe for external side effects** (push/PR create/merge): after
    the last attempt, check whether the effect happened anyway before declaring failure.
15. **Persistent single backlog file** (WORK_QUEUE.md) with archive discipline (≤400 lines;
    finished items moved verbatim with proof) — after it grew to 206 KB / ~81 k tokens per
    supervisor session (O-12).

### 4.2 Mistakes (do not repeat)

1. **Complexity creep in the acceptance machinery.** Builder-authored package + byte pins +
   ledger pins + blind v1→v4 + inspect + v2 NCC + obligation list + freeze + calibration +
   two-session confirmation + wording amendments + assembler. Each step was locally
   justified, independently reviewed, and the sum made the cycle stop more than the product
   did. The 8-step "evidence gate" redesign was the same mistake again, caught by the
   operator. *Heuristic: if the gate's own stops exceed real blockers, delete machinery.*
2. **Model-written evidence documents.** As long as the builder wrote the package, every
   evidence change was a product build and the byte chain kept breaking (66–94% of audit
   build cost; 62–64% of findings). Evidence must be generated by the controller from
   receipts — or not exist.
3. **Agentic loops per verification item** (resend full history; price-sorted routing that
   defeats caching). Cost must be proportional to what the model decides: one toolless call
   on a prepared ≤6 k dossier, not 31 turns.
4. **Resuming one reviewer session across days.**
5. **Supervisor as the liveness mechanism** (§2.4).
6. **Unmeasured claims in reports.** "No generator exists" (it did); "anchor by content,
   weakens nothing" (it broke 16 of 182 pins); "the sweeper is local" (it was not); "it costs
   pennies" (full GLM: $9.7/day). "A sentence written without measuring becomes someone
   else's decision the moment it enters a report." (`[mem:nobetci-tekrar-eden-kusurlar]`)
7. **Acting on the operator's workload by own interpretation.** The supervisor tried to stop
   the operator-enabled sweeper timer after an operator *question* about spend; the
   permission layer refused, correctly (`[mem:nobetci-operator-is-yukune-dokunma]`). Give
   findings + one-line command + rollback; the decision is the operator's. Don't announce
   "I'm doing X" before doing it; don't retry a refused action with another command.
8. **Turkish/English mix as an interface.** State tokens (`tuttu`, `ölçülmedi`, `sapma`,
   `TAVAN`, `kısmi:`), stage names (`kabul/kapi/inceleme/devir/teslimat-kaydi`), parsed
   markers (`## Kapanis taslagi:`, `## Kapanis metni`), test summaries ("N gecti · M hata"),
   review template in Turkish, task lines in ASCII-Turkish, ledger columns. Reader-side
   compat cost a release cycle (`bin/nb_vocab.py`, `27a5738`, `b9f2d7b`). **Release: English
   only, schema-declared vocabulary (`docs/state-vocabulary.md` is the start).**
9. **InfraMedic overfit spots** (from `docs/2026-09-15-project-neutrality-audit.md` and later):
   markdown pipe-table ledger machine (3 implementations); phase membership by dotted id
   substring; audit package = a product test module; Python/pytest-only symbol index, gate
   parsing, permit grammar, blind binding (`tests/x.py::name`); canon = an executable at a
   fixed path in the product repo; delivery = SSH+sudo to one edge with product scripts;
   GitHub `gh` only; single repo; one-machine ops scripts with hardcoded paths; model names
   in persisted field names (`openrouter_est_usd`, `.opus.json`); provider inferred from `/`
   in the model id; wall clocks (5400/1800/3600/900), `-n 8`, `MAX_BUILDERS 3`,
   `COOLDOWN_S 900` without rationale; a page-by-page browser-walk script tied to 1.3 routes
   (`inframedic-console-journey.py`) — operator: the reviewer plans and performs browser
   walks with a *generic* browser capability; phase-specific grants hand-written with
   `task_id: F-1.4-audit` (should be derived from the manifest + profile).
10. **Hand-maintained installer file lists** (§1.6).
11. **Unused complexity**: adaptive parallel sub-builders (`adaptive_builders=3`) never ran;
    `NB_PLAN_TURNS` defined but unreachable (K-9).
12. **Over-broad classification rules written by the supervisor**: "runner trouble" rule
    classified the builder's own fixture AssertionError as infrastructure →
    `gate_diagnostic_required` stop (`73900b5`).
13. **Product time bombs pass gates until they don't**: tests with a fixed instant and a
    7-day horizon checked against the real clock expired 09-26 and failed on main too (R29).
    Framework lesson: `base_broken` detection (`1214153`) so a red base doesn't buy repairs.

---

## 5. Operator preferences and standing decisions

Each with its record. Where the record is a memory note it is the supervisor's paraphrase of
the operator's words (quoted when available).

| # | Decision | Date | Record |
|---|---|---|---|
| 1 | **Priority:** target project's healthy development first; unattended operation second; project neutrality third (extraction after Phase 1) | 09-14 | `docs/CANON.md` §1; `[mem:nobetci-gelistirme-kanonu]` |
| 2 | **Keeping the cycle running outranks every other task**; watch without being reminded | 09-17/18/21 | `[mem:nobetci-birincil-gorev]` |
| 3 | **Caps are rails, not budgets**; raise them with a bound approval | 09-14 | `docs/CANON.md` §3; `ops/approvals/2026-09-14-limits-extension.json` |
| 4 | **No model diet caps**; cure repeated reading with information given up front | 09-15 | `[mem:nobetci-model-bilgi-sinirlari]` |
| 5 | **Never full GLM 5.3; always GLM-5.3-flash** in every role (incl. sweeper review, blind) | 09-21 16:45 | `[mem:nobetci-model-ve-rota-kararlari]` |
| 6 | **Routing: cheap + healthy, not throughput** (`NB_BUILD_SORT throughput` removed; price sort with caps, exclude bad upstreams) | 09-21; 10-01 | same; `ops/approvals/2026-09-28-ignore-openinference.json`, `2026-10-01-ignore-relace.json` |
| 7 | **Sweeper on local Qwen**, effort xhigh; never blocks the cycle ("like a fly") | 09-17, 09-21, 09-22 | `[mem:nobetci-supurucu-sinek]`, `[mem:inframedic-urun-tarafsizligi]` |
| 8 | **Give the builder the permissions it needs** (worker allowlist widened) | 09-21 | `[mem:nobetci-model-ve-rota-kararlari]` |
| 9 | **Roles:** Opus 5.5 plans/reviews (exact id `claude-opus-5-5`, not the `opus` alias, which drifts with the CLI); GLM flash builds; phase reviewer = Astra on Codex, Opus 5.5 when Codex limit hits; subscription-only for Claude roles — if exhausted, stop, never paid API | 09-09, 09-23, 10-01 | `[mem:nobetci-yonlendirme-siniri]`; `ops/approvals/2026-09-23-phase-reviewer-opus-5-5.json`; WORK_QUEUE R13 |
| 10 | **Simple, meaningful subphase gates, $5–15 per subphase incl. repairs**; deterministic evidence is enough, model only for residue | 09-28 | `docs/2026-09-28-review-gate-rethink.md`; `ops/approvals/2026-09-28-simple-phase-gate.json` |
| 11 | **"$40 for a review is unacceptable"**; rethink from outside, architecture changes allowed | 09-28 | `[mem:nobetci-kor-dogrulama-maliyet-kok-nedeni]`; `ops/approvals/2026-09-28-evidence-gate-rethink.json` |
| 12 | **Page only on real blockers**, not noise (routine maintenance boundary < 45 min not paged; one digest per task ≤ 1/30 min; Turkish page text for this operator) | 09-28 | rethink doc; R38 |
| 13 | **Framework neutrality**: auto-dev-cycle is a separate product; no overfit to InfraMedic, pytest, a phase; neutrality checklist on every PR | 09-15, 09-16, 09-28 | `[mem:nobetci-cerceve-tarafsizligi]`; `ops/approvals/2026-09-28-evidence-gate-rethink.json` (conditions) |
| 14 | **Product neutrality** too: InfraMedic is an enterprise product; environments/IPs/task ids/UI journeys go to adapters/config | 09-22 | `[mem:inframedic-urun-tarafsizligi]` |
| 15 | **Browser walk is the reviewer's** job with a generic tool; supervisor does not write product-specific walk scripts | 09-21 | `[mem:nobetci-tarayici-yuruyusu-reviewerin]`; `ops/approvals/2026-09-21-1.4-browser-walk-by-reviewer.json` |
| 16 | **External inputs a reviewer asks for are harness capabilities, not operator questions** (adapter + grant) | 09-14 | `[mem:nobetci-fable-devir]` |
| 17 | **Live experiments authorized** for all sub-phase audits with tested restore, bounded window, public receipt; cutover/withdrawal/gateway/device/identity acts still need own authority | 09-14 | `docs/CANON.md` §4; `ops/approvals/2026-09-14-live-experiments.json` |
| 18 | **Dev console account usable by agents**; secret only in a controller-only 0600 file, never in repo/log/receipt/prompt | 09-14 | `ops/approvals/2026-09-14-dev-credentials.json` |
| 19 | **P3 → sweeper, no cleanup build**; P2 carries after 10 pairs; audit-package P2 never ages out while a computed check fails; an operator carry never overrides that rule | 09-11, 09-16, 09-20, 09-30 | `ops/approvals/2026-09-20-p3-to-sweeper.json`; `2026-09-16-audit-package-p2-blocking.json`; `53c5a72` |
| 20 | **Closure text deterministic (option A)**; one bounded closure-text call | 09-29/30 | `docs/2026-09-30-deterministic-closure-text.md`; `ops/approvals/2026-09-30-trigger-guard-carry-closure-p2.json` |
| 21 | **Fresh phase-review session per round + digest; gate results reused as suite evidence; progress rail from data** | 09-30 | `ops/approvals/2026-09-30-builder-exploration-review-cost.json` |
| 22 | **Task drafts never ask the builder to edit canon**; every id a task text names must be authorized | 09-29 | `[mem:nobetci-gorev-taslagi-canon-yasagi]` |
| 23 | **One standing approval per phase-audit set**, grants derived automatically (later superseded with the blind machinery) | 09-25 | `[mem:nobetci-2026-09-25-operator-kararlari]` item 8; closed by `ops/approvals/2026-10-01-close-r8-r12.json` |
| 24 | **Target is fully unattended**; criterion = sub-phase audits pass without outside intervention; ask "what would 1.5 need by hand?" for every new mechanism | 09-23 | `[mem:nobetci-unattended-hedef-15]` |
| 25 | **Don't touch the operator's workloads** on your own interpretation; give data + command | 09-21 | `[mem:nobetci-operator-is-yukune-dokunma]` |
| 26 | **Learning recommendations accumulate**; do not remind unprompted | — | `docs/WORK_QUEUE.md` "Operator decisions pending" |
| 27 | **Overnight standing supervisor authority** can be granted for a bounded window | 09-29 | `ops/approvals/2026-09-29-overnight-supervisor-authority.json` |
| 28 | **Stricter gate for the final phase audit (F-1)**, designed before it admits; does not inherit blind/obligation machinery | 09-30, 10-01 | WORK_QUEUE R53 |

Operator style notes useful to a rebuild session: answers terse approvals ("önerdiğin şekilde
yap", "hepsine onay", "i approve as you suggest") to well-argued options with a recommended
default; wants measurements, not adjectives; repeatedly asks "is this normal?" about cost —
have the numbers ready; rejects anything that feels like spaghetti (09-17: "the code has become
spaghetti" → Astra refactor handover).

---

## 6. Rebuild checklist distilled from the above

Use as acceptance criteria for the open-source first release.

**Core semantics**
- [ ] Outcome enum `found|empty|could_not_run|failed` in every result type; non-empty error text.
- [ ] One module per decision (verdict, baseline, authority, idle/busy, cost, task identity);
      model-output schemas generated from the validating table.
- [ ] Multi-block append files fail closed on ambiguity.
- [ ] Accounting append-only; per-call cost deltas (never session totals); provider-priced
      estimates calibrated to the provider's own usage endpoint; CLI dollar figures never
      shown as cash.
- [ ] Every hold has a named resume signal; watcher + wake unit + pager with durable outbox
      are framework components; healthy = silent.

**Builder economics**
- [ ] Information up front: canon excerpts as content, plan sidecar, task dossier (symbol
      ranges, focused tests, gate failures, previous-round trail incl. shell reads),
      progressive notes copied on any exit.
- [ ] Honest tool list matching the sandbox allowlist; controller runs the full gate; tell
      the builder not to run suites that exceed tool caps.
- [ ] Observer records stream-delta kinds and the *upstream* that served each response.
- [ ] Routing: price-sorted with caps, health-filtered, upstream exclusion list in config
      (streaming granularity matters for idle timeouts).
- [ ] Progress rail from data (no edit for K turns + ≥2 compactions → resumable classified
      outcome), never a token diet.

**Gate**
- [ ] Simple gate as the default: claims list from roadmap rows → deterministic checks
      (suite on every target runtime, test-deletion guard, receipts with inner predicates)
      → one reviewer table → targeted repair → re-check touched claims.
- [ ] Evidence/acceptance documents generated by the controller from receipts; never authored
      by the builder; no byte pins across tasks.
- [ ] Receipts carry instrument hash, candidate head+tree, freshness; outer and inner status
      both checked.
- [ ] Phase reviewer: fresh session per round + digest; gets the controller's per-test results.
- [ ] Closure text and ledger rows deterministic; one bounded closure-text call that cannot
      change the tree.

**Install / ops**
- [ ] Component manifest; installer verifies import closure; preview by default; refuse
      unknown flags.
- [ ] Staged, content-pinned installs at idle boundaries; one at a time; source repo frozen
      while pending; grant re-binding on executable changes; every binding has a change path.
- [ ] `busy()` counts only the installed runtime; locks never created/truncated by probes.
- [ ] All long-lived work as service units, not session jobs; reboot reconciliation
      (`host_restart` disposition, companion services health-checked by real requests).
- [ ] Test guard: no writes outside a temp HOME/root; contract-faithful fakes; red-on-old
      evidence for every fix.

**Neutrality**
- [ ] No project literals in core; project profile supplies test command/adapter, layout,
      ledger schema, canon definition, delivery adapter (not just SSH+sudo), VCS host adapter
      (not just `gh`), receipt predicates, notification text/language.
- [ ] English-only interface vocabulary with a declared schema.
- [ ] Refuse to run when unconfigured instead of defaulting to the first project.

---

## Appendix A — commit index for the incidents above (nobetci-dev)

| Class | Commits |
|---|---|
| Closure deadlocks | `10aabf8` `2511d88` (row map), `4621143` `479907d` (closure routing), `d06f539` `c1684f9` (absolute paths, R58), `2eac5d3` `20488b2` (errored-build continuation) |
| Rebase | `b849e7d` (SIGPIPE), `82136b9` `0a5e1d5` `87ff091` (mechanical rebase, stale origin, conflicts to builder) |
| Holds / watcher | `39c13f1` `1eb93a6` `1cf7c39` (floors/cooldowns), `e7a8b49` (event-keyed dedup), `6fd87cd` (rollout that installs nothing), `a0a7c42` (maintenance vs alarm), `e910a7c` (added task id authorized) |
| Host restart | `11e20c5`, `756a223`, `a130153` |
| Provider | `7015c2b` (idle timeout), `7d86536` `3790ad2` (4xx + runaway), `5149deb` (degenerate), `8dcf4de` (remote 5xx), `9941be0` `e9ba2f4` `ccad9e8` `c23a2f0` (refusal accounting) |
| Cost accounting | `64fb6e3` (phase review call cost), `54fc38e` (provider prices), `4e9a9f0` (per-call review delta), `1fcef83` (key limit vs remaining room) |
| Install hazards | `18b2b16` (stale grants), `8f3d355` `7add6bb` `eb1689a` (component manifest), `5d86328` `5c04055` `a96d130` (busy), `bcaa2c2` `f60597a` (lock truncation), `6f45766` (interrupted disposition) |
| Evidence destroyed before read | `249173c`, `fc32824`, `726eb11` |
| Test hygiene | `571e386` (suite write guard), `96a4c30` (behaviour not source string), `eb646aa` (fake = real contract), `0de2702` (resolve gate before spending) |
| Gate classification | `73900b5` (fixture assertion is repairable), `1214153` (base_broken), `09c7768` `9312103` `1d57874` (preexisting deploy drift) |
| Phase reviewer cost | `1793f8a` (fresh sessions from round 2), step 2 gate results (`d7ba546`), dossier `44ce832` |

## Appendix B — memory notes translated (one line each)

| Note | Lesson |
|---|---|
| `nobetci-tekrar-eden-kusurlar` | Eight recurring defect classes: inference vs measurement; could_not_run→empty; data deleted before read; case measures its own setup; same decision twice / first-match on append files; source-string tests; validation after the evidence it should protect; missing install footprint. Checklist (a)–(g). |
| `nobetci-birincil-gorev` | Keeping the cycle running is the first job; permanent Monitor; health watcher first thing each session; rehearse queue changes by running the first step. |
| `nobetci-tutulan-admission-sessiz-olum` | Held admission → unit exit 1, no restart → 9 h dead; supervise, don't auto-restart. |
| `nobetci-oturumdan-bagimsiz-is` | Session death kills background jobs; use `systemd-run --user`; 4.5 h stall 09-21. |
| `nobetci-sahnelenmis-dosya` | Don't edit staged files or the repo while a rollout waits. |
| `nobetci-profil-kurulumu-main-sabit` | Don't merge to main while a profile `--wait` install is pending. |
| `nobetci-kurulum-grant-kopar` | Reinstalling an evidence script orphans its grant; empty error text reads as success. |
| `nobetci-host-restart-kurtarma` | Machine-cut build self-documents as `host_restart`; /tmp and watchers vanish on reboot. |
| `nb-embedder-reboot-yarisi` | Companion container may not return after reboot; symptom appears days later. |
| `nobetci-main-merge-behind-base` | Merging to product main under an open candidate → `behind_base` hold nobody resumes. |
| `inframedic-paralel-ajan` | Operator commits to main in parallel; conflicts always ledger + acceptance package. |
| `nobetci-harcama-dagilimi` | Sweeper was ~75% of spend on the shared key; builder bill ~90% resent context. |
| `nobetci-model-ve-rota-kararlari` | Never full GLM 5.3; flash always; sweeper local; cheap+healthy routing; builder permissions. |
| `nobetci-kor-dogrulama-maliyet-kok-nedeni` | Blind inspect 175 M tokens/pass from resent history; "$40 unacceptable"; dossier design. |
| `nobetci-kor-dogrulama-kapsami` | Blind only in sub-phase audit reviews; a pass is evidence for the reviewer, never acceptance. |
| `nobetci-shard-denetim-mimarisi` | Model-written package breaks byte chains; 1.4 blind never checked the 15 criteria; session cost ≠ call cost. |
| `nobetci-model-bilgi-sinirlari` | No information caps; decode-speed rail on local GPU is the exception. |
| `nobetci-gelistirme-kanonu` | Caps are rails; live experiments authorized; dev console account; capability landing path. |
| `nobetci-cerceve-tarafsizligi` / `inframedic-urun-tarafsizligi` | Framework and product neutrality. |
| `nobetci-tarayici-yuruyusu-reviewerin` | Reviewer plans/does browser walks with a generic tool. |
| `nobetci-operator-is-yukune-dokunma` | Don't stop operator-enabled workloads on own interpretation. |
| `nobetci-gorev-taslagi-canon-yasagi` | Task drafts must not require canon edits or builder-only closure text; all named ids authorized. |
| `nobetci-supurucu-sinek` / `nobetci-supurucu-ilk-tam-tur` | Sweeper never blocks; first real run exposed cumulative-patch and missing-venv defects. |
| `nobetci-izolasyon-mimarisi` | Boundary is identity, not parsing; patch crosses; accepted risks on record. |
| `nobetci-yonlendirme-siniri` | Sub-agent calls inside the sandbox leaked Claude models to OpenRouter; verify `modelUsage`. |
| `nobetci-zamanlanmis-giris-grup` | Scheduled context lacked a supplementary group; measure both contexts. |
| `red-proof-gercek-home` | Red-proofs against old code touched the real HOME/credentials; run under temp HOME. |
| `saat-tahmini-yazma` | Always `date` before writing a time into a persistent record. |
| `guard-bash-heredoc-git` | Supervisor's own shell guard: write patch scripts to files, `git add` by name, `commit -F`. |
| `vllm-motor-olumu-sessiz` | Liveness = a real generation request, not `/v1/models`. |
| `nobetci-context-programi` | Canon as content, context metrics, plan sidecar; `--bare` loses OAuth; non-`test_*.py` suites are outside discovery. |
| `nobetci-sure-token-analizi` | Build is 92% model time; queue kept ≤400 lines. |
| `nobetci-2026-09-25-operator-kararlari` | The long 09-25..27 ruling list (blind rails sized to the job, NCC to reviewer, obligation gate, P0) — mostly superseded by the simple gate. |
