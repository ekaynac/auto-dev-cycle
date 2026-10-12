# Development workflow until self-host (M0–M6)

Status: adopted 2026-10-03 (D38); §2–§3 amended 2026-10-09 (D45, D46); §1–§3 and §5 amended 2026-10-12 (D49, D53,
D55, D64: goal reconciliation instead of a fixed PR sequence, the gate cost ledger, the weekly review, review routing). This document is written to be **self-sufficient for a fresh Claude session**: a
background session must be able to pick up the work from this file, the repository and the Development log issue
alone.

Until M6 the cycle cannot build itself, and nöbetçi is reserved for InfraMedic (`CLAUDE.md` scope). So auto-dev-cycle
is built by a lead Claude session with subagents, reviewed as below, and merged through PRs.

## 1. Roles and the reconcile loop

**Goal plus backlog (D49).** Work is chosen by reconciling **one current outcome goal** against observed evidence, not
by working down a fixed PR sequence. The goal is an outcome with an exit check, normally the next milestone checkpoint
(01 §4, e.g. M1's "useful approved merge"). The backlog is an ordered list of outcomes, not of mandatory PRs:
- **When:** after every merge and after every failed live run, the lead reconciles: what did the evidence show, does
  the goal still hold, and what is now the shortest safe path to it.
- **Detail:** only the next **three to five** items are detailed (acceptance, owned paths, smoke check); the rest stay
  one-line outcomes until they come within reach. Measured failures of live runs become backlog items.
- **Record:** the goal and the next items are posted on the Development log (#3) with each reconcile that changes them,
  and in the weekly review (§5).
- **Limits:** a reconcile never changes a decision, the plan or an exit criterion by itself; those still wait for the
  operator (§4). D45's threat model and D46's review and PR rules (§2–§3) apply unchanged.

| Role | Who | Does |
|---|---|---|
| **Operator** | the human owner | decides; approves gated PRs; answers blocking decisions |
| **Lead session** | one Claude Code session at a time (attended in the operator's terminal, or the background session `adc-dev`) | reconciles the goal and details the next items (D49), writes the core contracts itself, integrates subagent work, runs reviews, merges, reports |
| **Subagents** | spawned by the lead with `isolation: "worktree"` | build independent pieces (a package, a fixture set, an adapter) in their own worktree; return a branch with tests passing |
| **Astra** | `codex exec -m gpt-6-astra -c model_reasoning_effort="high" --sandbox read-only` | reviews **contract PRs** |
| **Fresh Opus reviewer** | a new subagent with no build context | reviews **mechanical PRs** |

**One lead at a time.** When the operator starts an attended session, it takes over from the background session:
1. the background session finishes or parks its current step;
2. it writes a handover comment on the Development log;
3. it stops.

The attended session writes a handover comment when it hands back. This is the supervisor seat rule
(`02-supervisor-role.md` §5) applied to development.

## 2. PR rules

- **A PR serves a detailed backlog item (D49).** Its description names the current goal and the item it advances.
  Work outside the next three to five detailed items waits for the next reconcile.
- **Branch per PR from `main`:** `feature/<scope>/<desc>`, `fix/<scope>/<desc>`, `chore/<desc>`. Conventional commits.
  Nothing is committed to `main` directly. The shell guard blocks pushing `main`, and that is intended.
- **Size (D46):** typically 300–600 added plus deleted lines (`git diff --numstat main...HEAD`) of hand-written
  non-test code, with a ceiling of about 800 for **one reviewable behaviour** with a clear acceptance story. Changes
  that interact with authority, replay, fencing or merge state stay smaller. Project-authored schemas under `schemas/`
  count (D44). Tests, test fixtures, generated code and unchanged vendored schemas don't count, but their review burden
  is stated in the description. "One feature, one PR" means one reviewable behaviour, never a whole subsystem. A larger
  piece is split, unless it is one cohesive package that splitting would only fill with temporary scaffolding; then it
  ships as one PR with the reason stated in the description (D41). That exception may exceed the usual 300–600 range
  but not the ~800 ceiling, unless the description justifies it and the lead accepts before review. The justification
  names the one behaviour and its acceptance story, the scaffolding a split would need, the line count per part, and
  why no part touching authority, replay, fencing or merge state can land separately.
- **Fault evidence (D46).** Targeted mutation, or equivalent discriminating fault evidence (a test that fails under a
  named, realistic fault), is required for changes to:
  - authority, grants, containment and egress;
  - receipt completeness, applicability and review independence;
  - fencing, uncertain effects, duplicate delivery and accounting;
  - publication before restore, and restart/replay invariants.

  Routine plumbing needs ordinary behavioural tests. Existing contract fixtures keep their named mutations. The
  red-proof rule for `fix(...)` PRs (11-testing) is separate and unchanged.
- **The PR description states, before review:**
  - acceptance criteria;
  - the smoke check (exact commands);
  - the design note(s) it implements (`docs/design/m0/…`);
  - for `fix(...)` PRs, the red-proof test (11-testing).
- **CI must be green:** `make ci` and the T0 fixtures check run on the self-hosted homelab runner (amd64, #75). The
  workflow has no arm64 job: the lead must run `make ci` locally on the controller host (arm64) before every push. Tests run inside the sandbox, which is bubblewrap (11-testing; the
  rootless-container fallback is deferred to M3, D42).
- **One defect class, one repair.** A defect class is fixed once, across all its entry points (for an eligibility rule:
  every completion and consumption path together), followed by one review, not a run of small follow-up PRs. A
  prerequisite repair may land first as its own reviewed PR; no PR may activate a known blocking defect.
- **After two NO-GOs** on the same PR, the lead first settles the invariant or contract (in a design note, or a
  question to the operator when it is a decision) and only then revises or splits the code. Splitting does not resolve
  an unclear contract.

## 3. Review

**Contract PRs → Astra.** A PR is a contract PR if it touches any of:
- `schemas/`, `gen/` generators (not generated output alone), `spec/knowledge/`;
- `internal/authority`, `internal/eventstore`, `internal/capacity`, `internal/attempt`, `internal/receipt`;
- the sandbox, the adapter protocol (`internal/adapterhost`, `adapters/contract`), the test sandbox, security
  configuration;
- `docs/design/`, `docs/plan/`.

**Mechanical PRs → a fresh Opus subagent:** generated output, fixtures, docs outside design/plan, tooling, CLI
cosmetics, refactors with no contract change.

**Review routing (operator, 2026-10-12; D64).** Where this differs from the two paragraphs above, it applies:
- **Plan and docs PRs:** one external review round. On GO with changes, the lead verifies the fixes and merges with no
  second round (D46; plan PRs still wait for the operator, §4).
- **Code and contract PRs:** a fresh Opus pre-review, then one external round. On NO-GO, the lead settles the contract
  first and only then repairs the PR (D46).
- **Mechanical PRs and small Low fixes:** the fresh Opus reviewer only.
- **Default external reviewer:** Mythos, from 2026-10-13. Astra is reserved for high-risk contract PRs because its
  quota is limited. When one external reviewer is unavailable, the other is used.
- **Measurement:** each review's cost and validated catches go into the gate cost ledger (below, D55). A review round
  that does not pay for itself is retired by the D55 rule.

**Brief format** for both: the PR diff (`git diff main...HEAD`), the design note it implements, the acceptance
criteria. A contract-PR brief also states the threat model (D43 as amended by D45):
- **in scope:** honest use and adversarial behaviour by any party without elevated privileges or controller authority:
  malicious API clients, forged adapter messages, hostile repositories, sandbox escape attempts, supervisors,
  processes ignoring a cooperative lock, attacks starting from ordinary credentials or a limited grant; also invalid
  transitions the controller itself generates, crashes, stale authority and uncertain effects;
- **out of scope, Low at most:** attacks requiring pre-existing elevated privileges; the operator deliberately
  overriding the build system; arbitrary fabricated histories (direct store edits, events manufactured outside
  supported commands) by the controller principal, which is trusted until M7-S (D45).

D43 and D45 never relax an existing contract or §4's operator gate. Output: GO / GO with changes / NO-GO, and findings
by severity. Reviews are saved to `docs/reviews/<date>-pr<N>-<reviewer>.md` in the PR itself.

**What blocks (D46):**
- **Critical/High findings inside the threat model.** A synthetic raw event alone does not justify High. The reviewer
  names one of:
  1. a reachable input path;
  2. a realistic controller or recovery defect;
  3. an explicitly promised invariant (a design note or contract line).

  Authority bypass, silent accounting loss and unauthorised effects are High, never a Medium.
- **Bounded acceptance obligations**, blocking at any severity:
  1. the advertised feature fails on its ordinary supported path;
  2. a restart or migration leaves valid state unreadable or work permanently stranded;
  3. the PR's evidence cannot establish its acceptance criteria;
  4. a deferred feature is reachable without its guard.

**Everything else** (other Medium and Low findings, cosmetics, recoverable inconvenience, defence against excluded
capabilities) goes into **one deduplicated backlog issue per PR**. Each entry names an owner, the affected invariant
and its trigger (the milestone, activation or release that makes it due). These entries are not same-day follow-up
PRs; they are picked up when their trigger arrives.

**When Codex credits run out:**
1. contract PRs wait, unmerged;
2. the lead continues with other work;
3. the lead retries Astra later (credits have returned within hours before);
4. if a contract PR waits more than 24 h, the lead asks the operator (§5).

**Gate cost ledger (D55).** Every recurring gate keeps a ledger: in this workflow, CI, Astra review, fresh-Opus review
and the operator gates (§4); in the cycle, from M1's first live attempt, every gate and review node (01 §2.6, recorded
with the M1 benchmark). Each entry records:
- elapsed time, and the actual or estimated charge (unknown cost is recorded as unknown, never as zero);
- validated catches: findings that led to a change, unique to this gate;
- false blocks: blocks later shown wrong;
- repairs and review rounds caused, and escapes: defects found later that the gate should have caught.

The ledger is reviewed after every 20 tasks or monthly, and summarised in the weekly review (§5). Retiring or
weakening a gate is an **operator decision based on the ledger**; a gate that enforces a required safety property is
retired only when an equivalent enforced replacement exists. A cheap ledger never justifies removing a safeguard by
itself, and D46's blocking rules are unchanged.

## 4. Merge

The lead merges (`gh pr merge --merge`) when **CI is green and the review is GO**, after applying any requested
changes. **GO with changes** permits the merge once no Critical/High finding or bounded acceptance obligation (§3)
remains open and the nonblocking findings are recorded in the PR's backlog issue (D46); it needs no second review
round. The CI gate and the operator gates below are unchanged.

These PRs **wait for the operator**:
- a PR that changes a decision (D#);
- a PR that changes the plan (`docs/plan/`);
- licensing;
- security posture (authority defaults, data policy, sandbox boundaries, confirmation channels).

## 5. Reporting and blocking decisions

- **Development log:** the pinned GitHub issue #3 (`ekaynac/auto-dev-cycle`).
  - **Per merge:** one comment: PR number and title, review verdict and reviewer, tests/CI result, what is next.
  - **Daily digest:** one comment, in the supervisor digest format (`02-supervisor-role.md` §4): done, open, decisions
    waiting for the operator, Codex/Claude usage, anomalies. Numbers, not adjectives.
  - **Weekly review (D53), every Monday:** one comment that measures outcome progress against the week targets in
    01 §4.2:
    - days to the first useful merge (after it: days since the last useful merge, and useful merges this week);
    - cost: total charge for the week and per useful merge, unknown cost shown separately;
    - interventions: operator rescues and unplanned operator actions, each with its cause;
    - the gate cost ledger (§3): per gate, time, charge, validated catches, false blocks and escapes;
    - the current goal and the next three to five items (§1), and any proposed change to scope or targets.

    A proposed change to the plan or a decision is asked as a blocking decision (below); the review itself changes
    nothing.
- **Blocking decisions:**
  - ask the operator with options and a recommended default;
  - attended: in the session;
  - background: a Development log comment **and** a Telegram message;
  - continue with independent work. Nothing gated by the decision merges until it is answered.
- **Never block on a question** (nöbetçi Δ§15). The background session does not use synchronous prompts. It posts
  the question and continues. Non-urgent questions are batched into the daily digest. After an answer arrives,
  the first act is a status check (CI, open PRs, background jobs).
- **Telegram:** only for blocking decisions and an unattended stop.
  - Command: `openclaw message send --channel "$ADC_DEV_TG_CHANNEL" --target "$ADC_DEV_TG_TARGET" -m "<text>"`.
  - The values live in `~/.config/adc-dev/notify.env` (0600). Source it in a subshell and **never print it**.
  - Messages start with `[auto-dev-cycle dev]` so they are distinguishable from nöbetçi's pages.

## 6. Background session

- Runs in tmux session **`adc-dev`** on the controller host, in `/home/mega/auto-dev-cycle`, as a Claude Code session
  started with this document as its brief and a **self-paced loop** (`/loop` dynamic mode). Each wake:
  1. check the Development log and open PRs;
  2. continue the next step of the current milestone;
  3. report;
  4. schedule the next wake.
- **Waiting is not polling.** Background jobs (Astra reviews, CI runs) re-invoke the session on completion. The loop's
  fallback wake is long (≥ 20 min).
- **Stop conditions** (send a Telegram message and stop the loop):
  - repeated failure of the same step (3×);
  - Claude usage limit;
  - a merge-gated PR plus no other independent work;
  - anything that would touch the predecessor trees.
- **The operator** attaches with `tmux attach -t adc-dev` and detaches with `Ctrl-b d`. Interrupting the session pauses
  the loop.

## 7. Boundaries (from `CLAUDE.md`, unchanged)

- Never modify `/home/mega/nobetci-dev`, `/home/mega/nobetci`, `/srv/nobetci` or `/home/mega/inframedic`. Read them
  freely. Ignore `/home/mega/AGENTS.md`.
- Never print secrets. Credential and env files: key names only.
- The shell guard hook is the operator's policy. Work with it (`git fetch`, `gh` for merges, files named explicitly in
  `git add`, scripts written to the scratchpad instead of heredocs). Never try to bypass it.

## 8. Toolchain bootstrap (first M0 PR)

- Go is installed per user from the official tarball into `~/.local/go`, with the version pinned by `go.mod`'s
  `toolchain` directive.
- Node 22 and pnpm are present.
- bubblewrap is the test sandbox; where it is unavailable `make test` refuses to run (the rootless-container
  fallback is deferred to M3, D42).

## 9. Order of work (M0)

Follow `docs/design/m0/` and the M0 exit check in `00-overview.md`. A suggested PR sequence:
1. repo skeleton, licence, Makefile, CI, test sandbox and its self-test;
2. code-generator spike on three schemas (receipt, approval, adapter event) and the generator choice;
3. vocabulary registries and the vocab check;
4. event store (writer, commands, effects, blobs, hash chain);
5. authority;
6. leases/attempts/usage;
7. receipts;
8. project profile;
9. API/CLI skeleton and parity harness;
10. adapter contract SDK, fake adapter, conformance kit;
11. ContextManifest schemas and the knowledge spec draft;
12. contract fixtures and the installed-artifact exit check.

Independent items (e.g. 6, 7, 8) can go to parallel subagents once 3–4 exist.
