# Development workflow until self-host (M0–M6)

Status: adopted 2026-10-03 (D38). This document is written to be **self-sufficient for a fresh Claude session**: a
background session must be able to pick up the work from this file, the repository and the Development log issue
alone.

Until M6 the cycle cannot build itself, and nöbetçi is reserved for InfraMedic (`CLAUDE.md` scope). So auto-dev-cycle
is built by a lead Claude session with subagents, reviewed as below, and merged through PRs.

## 1. Roles

| Role | Who | Does |
|---|---|---|
| **Operator** | the human owner | decides; approves gated PRs; answers blocking decisions |
| **Lead session** | one Claude Code session at a time (attended in the operator's terminal, or the background session `adc-dev`) | plans the next PRs, writes the core contracts itself, integrates subagent work, runs reviews, merges, reports |
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

- **Branch per PR from `main`:** `feature/<scope>/<desc>`, `fix/<scope>/<desc>`, `chore/<desc>`. Conventional commits.
  Nothing is committed to `main` directly. The shell guard blocks pushing `main`, and that is intended.
- **Size:** ≤ ~400 hand-written lines. Generated code, fixtures and vendored schemas don't count. A larger piece is
  split.
- **The PR description states, before review:**
  - acceptance criteria;
  - the smoke check (exact commands);
  - the design note(s) it implements (`docs/design/m0/…`);
  - for `fix(...)` PRs, the red-proof test (11-testing).
- **CI must be green:** GitHub-hosted runners (amd64 + arm64). Tests run inside the sandbox; on hosted runners that
  means the rootless-container fallback (11-testing).
- **Split after 3 NO-GOs** on the same PR.

## 3. Review

**Contract PRs → Astra.** A PR is a contract PR if it touches any of:
- `schemas/`, `gen/` generators (not generated output alone), `spec/knowledge/`;
- `internal/authority`, `internal/eventstore`, `internal/capacity`, `internal/attempt`, `internal/receipt`;
- the sandbox, the adapter protocol (`internal/adapterhost`, `adapters/contract`), the test sandbox, security
  configuration;
- `docs/design/`, `docs/plan/`.

**Mechanical PRs → a fresh Opus subagent:** generated output, fixtures, docs outside design/plan, tooling, CLI
cosmetics, refactors with no contract change.

**Brief format** for both: the PR diff (`git diff main...HEAD`), the design note it implements, the acceptance
criteria. Output: GO / GO with changes / NO-GO, and findings by severity. Block only on Critical/High. Reviews are saved
to `docs/reviews/<date>-pr<N>-<reviewer>.md` in the PR itself.

**When Codex credits run out:**
1. contract PRs wait, unmerged;
2. the lead continues with other work;
3. the lead retries Astra later (credits have returned within hours before);
4. if a contract PR waits more than 24 h, the lead asks the operator (§5).

## 4. Merge

The lead merges (`gh pr merge --merge`) when **CI is green and the review is GO**, after applying any requested
changes.

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
- bubblewrap is used for local test sandboxing where available; otherwise Docker (present) is the fallback.

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
