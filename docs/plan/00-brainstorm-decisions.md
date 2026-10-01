# Brainstorm decisions — first open-source release

Session: 2026-10-01, operator + Claude (Opus 5.5), following `docs/handoff/00-brainstorm-kickoff.md`.
Every decision below was chosen by the operator from a set of options or stated in the operator's own words.
Turkish statements are translated; the gist is kept, not paraphrased into stronger claims.
The plan built on these decisions is `01-first-release-plan.md`.

## The intention (operator, restated and confirmed)

> The 5090 lab was only an example; the intention is what matters. We want to harvest capacity in a
> distributed, platform-agnostic way, from every source we can find. Our abstract structure must be able to use
> a resource that comes from anywhere: wherever something is served and whatever is idle, we want to use it.

Concrete sources the operator named:
- Qwen3.8 27B is by far the best local model.
- GLM-5.3-flash is very good for API cost; the old system used it.
- DeepSeek v4.1 flash is a candidate worth trying.
- Cloud models from providers that offer free usage.
- The operator's Ollama subscription, for testing with small models.
- Planner and reviewer: cloud Opus 5.5 and the latest Astra **for now; both must be configurable**.

## Decisions

| # | Topic | Decision | Notes |
|---|---|---|---|
| D1 | Topology | One controller owns git, sandbox and gates. Remote machines contribute **model capacity** (they receive prompts, not repos). | Remote build execution is post-v1. |
| D2 | Codebase | **Clean rewrite. Go core + TypeScript adapters.** nöbetçi is reference only. | TS adapters talk to the Go core over a process contract. |
| D3 | Executors in v1 | Claude Code CLI, Codex CLI, opencode, **oh-my-pi (omp)**, OpenAI/Anthropic-compatible HTTP. | Operator: "there must be one central document on adding adapters." |
| D4 | Neutrality proof | **Fixtures only** (no InfraMedic migration in v1), but meaningful. | Operator: use standard developments as benchmarks, or rebuild half-finished repos; CTIS-SIMS is a candidate (it has an SRS and is a polyglot microservice project). |
| D5 | Routing layer | **Our own capacity scheduler + thin executor adapters.** freerouter's complexity tiering is at most one input. | The scheduler is the product. |
| D6 | Classifiers ("Jev") | **Evaluation-first.** Use Jev now; test local alternatives (e.g. Kev). Enable a surface only where a structural, clear-cut decision is needed and tests show it beats the rule. | Operator: "no need to use it unless needed; only on really meaningful surfaces, after testing first." |
| D7 | Benchmark | **Tiered:** T0 tiny Py/TS/Go fixtures (CI), T1 standard task set with known answers, T2 CTIS-SIMS greenfield from its SRS/SDD/test design. | |
| D8 | Planning & capacity | **Plan once, re-shape at dispatch.** The plan is capacity-independent; the scheduler splits/merges units to live capacity. | |
| D9 | Data trust | **Per-project data policy × per-source trust class.** Work is never placed on a source below the project's floor. | |
| D10 | Joining | **Both:** any reachable endpoint by config (URL + key ref + window), or a cross-platform **dial-out agent** for machines behind NAT/firewalls. | |
| D11 | Exhaustion | **Ordered fallback chain per role; paid fallback only where config marks it;** otherwise wait for the earliest reset while other work continues. | |
| D12 | Terms of service | **Strict:** official APIs/harnesses, one account, within terms. No account multiplication, scraping or quota evasion. | |
| D13 | Sandbox | **Rootless containers** (Podman/Docker), per-project toolchain images. | Replaces root-owned systemd + separate uid. |
| D14 | Operator surface | **Web dashboard with actions** (approve, hold, release, rule). | Implies auth in v1. Approvals still persist as immutable dated records. |
| D15 | Dogfooding | **Yes, after a self-host milestone.** From then on the project's own backlog runs through the cycle, with independent review before self-install. | |
| D16 | Licence / name | **Apache-2.0.** "auto-dev-cycle" is the working name; brand chosen before public launch. | |
| D17 | Concurrency | **Parallel tasks, serial merge queue** (rebase + re-gate before merge). | Fixes the `behind_base` and sweep-merge lesson class. |
| D18 | Projects | **Multi-project:** one controller, one shared capacity pool, per-project priority/budget/data policy. | |
| D19 | v1 done | **T0/T1 green on Py/TS/Go + a defined CTIS-SIMS slice delivered using ≥3 source kinds + self-host.** Full CTIS-SIMS is the v1.x showcase. | |

### Plan review (operator, 2026-10-01)

| # | Topic | Decision |
|---|---|---|
| D20 | Schedule | **No dates or durations.** Milestones are ordered and closed by their exit checks. |
| D21 | CTIS-SIMS v1 slice | **Polyglot, several services:** Identity + Inventory (PHP/Laravel), Audit + Notification (Node/Fastify), AI + MCP (Python/FastAPI), Nginx gateway, Redis bus, React frontend. Detail in plan §3. |
| D22 | Subscription harnesses | **Claude Code and Codex are used from outside the sandbox (host-side) in v1.** |

### Rulings on Astra's review (operator, 2026-10-01; review in `reviews/2026-10-01-astra-plan-review.md`)

| # | Topic | Decision |
|---|---|---|
| D23 | Astra findings + CLI parity | **Apply all High findings.** Also: every dashboard function has a CLI alternative and vice versa; the CLI may have more capabilities. Operator: "most of the time a supervisor AI will be running auto-dev-cycle; at least I prefer it like that." |
| D24 | Background work | **Framework in v1, one surface live** (P3 findings); further surfaces in v1.x on measurement. |
| D25 | Deferrals to v1.x | Kev-27B quantisation research, OIDC, SSH deploy plugin, extra pager transports (v1 ships webhook). |
| D26 | Unattended bar | **Initial approvals OK, zero rescues.** Every source kind, including a dial-out agent, must contribute measured accepted work; faults are injected during the release run. |
| D27 | Medium/Low findings | Applied as refinements (CTIS-SIMS scope contract, narrow shaping, classifier shadow mode, vocabulary fixes); the CTIS-SIMS scope items are still open as O3. |

### Open-decision rulings (operator, 2026-10-01)

| # | Topic | Decision |
|---|---|---|
| D28 | Model-usage defaults | **Three built-in usage profiles with recorded intentions:** `standard` (the operator's own preference and the average developer's: Claude, Codex, local LLM on own hardware, OpenRouter restricted to providers that neither retain nor train on data), `open` (use every free source and model as fully as possible), `on-prem` (fully in-house). Every current and future setting declares a default per profile; settings stay individually changeable after a profile is chosen. |
| D29 | CTIS-SIMS redistribution | **Publish** the CTIS-SIMS requirement documents as public T2 inputs. |
| D30 | CTIS-SIMS scope contract | **Approved:** QR generation in FReq1.1.1, FReq7.5 added, Turkish in FReq7.1, precedence SRS/SDD/test design, bounded live AI check plus mocked unit tests. |
| D31 | Supervisor model | The supervisor AI is **external** to auto-dev-cycle. Nothing can be closed to the developer's attended agent (it holds the developer's credentials). An **unattended supervisor** runs on a separate, isolated token; its restricted actions become proposals needing confirmation, not refusals. The operator also asked to define the unattended supervisor's full function, not only its guardrails: what it does, whether it is meaningful, reporting, and its relation to the attended agent. Proposal: `02-supervisor-role.md`. |
| D32 | Confirmation defaults (supervisor token) | self-install = `independent_review`; data-policy/destination widening, paid fallback, agent enrolment, grant creation/extension = `human` (presence-verified). |
| D33 | Supervisor role | `02-supervisor-role.md` **approved**: five jobs, single-seat handover, once-a-day digest. |
| D34 | Supervisor + classifiers | The unattended supervisor may use Jev-style classifiers where needed and meaningful; these uses join the classifier experiments and **start in shadow mode** like every other surface. |
| D35 | Knowledge layer | **Adopted from the knowledge-layer brainstorm (2026-10-02), decisions K1–K12 in `03-knowledge-layer.md`:** four information classes, mechanical knowledge first, durable curated knowledge only after M2 ablations, `ContextManifest` in M0, constrained hybrid storage, staged reviewer disclosure by default, a shared semantic spec with InfraMedic homed in this repo, framework lessons shared across projects with guardrails (K11, operator's choice). |
| D36 | Knowledge-layer review | Astra (2026-10-02, GO with changes): **all findings applied**: K11 keeps sharing, but with lineage-inherited source restrictions, template-only automatic export and digest-bound human export approval for prose; the M2 fixture-lesson ablation (E1L) becomes the K3 gate; a protected-document change operation; full-input view fingerprints; manifest coverage levels; continuity at every builder exit; predeclared K1 and experiment exits; a minimal spec in M0. Open questions resolved per Astra's recommendations. |
| D37 | M0 design review | Astra (2026-10-02, GO with changes) on `docs/design/m0/`: **all findings applied**. Also: human confirmation is **WebAuthn on the dashboard only** (the pager notifies and links, never confirms); **tests run inside a sandbox** (bubblewrap or a rootless container) where the real home does not exist; Q1 module path `github.com/ekaynac/auto-dev-cycle`; Q2 standard TypeID/UUIDv7; Q3 owner-only socket + explicit shared socket + verified TLS; Q4 hash chain with full verification and a trusted head. |

## Defaults taken without a question (operator may overturn)

- Forge: plain git, GitHub and GitLab all in v1 behind one interface.
- Deploy/evidence: plugins; v1 ships `none`, `script`, `ssh`, `docker-compose`.
- English-only code, state tokens and docs, with a schema-declared vocabulary.
- State: embedded SQLite (WAL) with an append-only event log; no external database needed for v1.
- The four outcomes (`found / empty / could_not_run / failed`) and every invariant in `docs/handoff/02` §1 carry over.

## Research inputs gathered during the session

- **freerouter** (MIT, TypeScript): OpenAI-compatible proxy; a 14-dimension weighted prompt classifier maps a
  request to SIMPLE/MEDIUM/COMPLEX/REASONING tiers; per-tier timeout fallback. No quotas, resets, harnesses or leases.
- **Jev** (Typesafe AI): "system one" classifier; returns typed Choice/Score with probabilities, or `null`. It does not
  generate text.
- **Kev** (`jaredpalmer/kev`, Apache-2.0): Jev-style decision models on Qwen3.5 (0.8B–9B) and Qwen3.8 27B, with a
  pointer head and a `/v1/systemone` API. Kev-27B scored 0.851 vs Jev 0.857 on new-domain tasks. It ships bf16/LoRA,
  with no GGUF.
- **oh-my-pi (omp)**: Rust terminal coding agent (fork of Pi) with hash-anchored edits, LSP/DAP, subagents and
  60+ providers.
- **CTIS-SIMS3.0** (operator's private repo): a 784-line SRS with hierarchical ids (`FReq1.1.1`, each with
  Input/Output/Function/Requirement), an SDD, and a test-pyramid design (PHPUnit, Jest, pytest, Docker-backed
  integration). The services are frontend, identity, inventory, transactions, workflow, notification, reporting,
  audit, ai and mcp.
