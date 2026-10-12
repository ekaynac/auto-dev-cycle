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
| D5 | Routing layer | **Our own capacity scheduler + thin executor adapters.** freerouter's complexity tiering is at most one input. | The scheduler is the product. **Superseded by D50 (2026-10-12).** |
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
| D20 | Schedule | **No dates or durations.** Milestones are ordered and closed by their exit checks. **Superseded by D53 (2026-10-12).** |
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

### Development workflow (operator, 2026-10-03)

| # | Topic | Decision |
|---|---|---|
| D38 | Building v1 until self-host | `04-development-workflow.md`: a lead Claude session plus parallel subagents in worktrees; PRs ≤ ~400 hand-written lines with criteria up front; **Astra reviews contract PRs**, a fresh Opus subagent reviews mechanical PRs; the lead merges on green CI + GO, while decision, plan, licensing and security-posture PRs wait for the operator; a line per merge plus a daily digest on the pinned **Development log issue (#3)**, with Telegram (openclaw) only for blocking decisions; ask with options and keep going when blocked; a **background self-paced session** in tmux `adc-dev`; GitHub-hosted CI with the container sandbox fallback. **Superseded by D49 (2026-10-12)** for how work is chosen and detailed; the roles, review, merge and reporting rules stay, as amended by D46. |
| D39 | nöbetçi delta intake | The operator asked the planning session to filter the nöbetçi supervisor's 2026-10-03 reports. Accepted items A1–A14 (`docs/research/2026-10-03-nobetci-delta-intake.md`): owed inputs and re-keying; the evidence-job lifecycle (new `m0/12`); strict gate options (pair, test_ids, proof_receipts, dispositions); broker credit exhaustion; the producible-acceptance validator; environment facts and gate-output ownership; applicable inputs before review; role/mode-scoped fallback; approval lineage fields (the lexical new-identity ban was removed after review); no blocking prompts; legacy incident goldens; a measurement export and data-based `page_after`; per-executor footprints; pre-existing drift routing. Deferred and rejected items are listed there with reasons. |
| D40 | Design out, don't patch | Operator: "we are in development, so instead of carrying the lessons as rules to watch, make suitable architectural choices." Adopted all six: **the cycle as an artifact graph** (nodes over content-addressed artifacts, demand-driven, structural waiting); **only the manifest reaches a model**; **outcomes from observed facts**; **evidence targets as leased resources**; **test what you ship** (revised after review: source-test workspace, hashed package deployed byte-exact, runtime verification on the package); **typed acceptance producers**. New `m0/13-artifact-graph.md`; note 12 reduced to evidence specifics; 02/04/05/06/07/10 and plan §2.5/§2.7 simplified. nöbetçi lessons remain as fixtures proving each class cannot occur. |
| D41 | Rulings during M0 (2026-10-03) | Asked by the background lead session. **Copyright holder** in `NOTICE`: Mega Bilgisayar Tic. Ltd. Şti. (#7). **PR size:** the ~400-line limit counts hand-written non-test code; tests don't count; a cohesive package that still exceeds it may ship as one PR with the reason stated, rather than be split with temporary scaffolding (04 §2). **Test sandbox (#10):** a contract PR; the lead merges it on Astra GO, because it implements D37's boundary without changing it. |
| D42 | Test sandbox fallback deferred (2026-10-04) | Asked by the background lead session. The rootless-container fallback of the test sandbox (11-testing; split from #10 after three NO-GOs) is **deferred to M3**, when adc-agent ships for macOS and Windows and needs tests there. Until then **bubblewrap is the only test sandbox**: `make test` refuses to run where it is unavailable, never running tests unsandboxed. GitHub-hosted CI runs bubblewrap (since #75 CI runs on the self-hosted homelab runner, also with bubblewrap). The fallback, when built, is a security-posture change (identity mapping, network, mounts, environment, pinned image) and waits for the operator. |
| D43 | Review scope for contract PRs (2026-10-05) | Asked by the background lead session after #70 and #72 needed four Astra rounds each (both converged as soon as the scope was explicit). Every contract-PR brief states the threat model. **In scope**, eligible to block: honest use, and adversarial behaviour by any party without elevated privileges: malicious API clients, forged adapter messages, hostile repositories, sandbox escape attempts, processes that ignore a cooperative lock. **Out of scope**, Low at most: attacks requiring pre-existing **elevated** privileges outside this threat model, such as root or resetting a file's change time; and, separately, the operator deliberately overriding the build system (make variables such as `GOBUILD`, `LDFLAGS`, `GO`; a forged `runtime.modinfo`). Attacks that start from ordinary credentials, a limited grant or normal filesystem access stay in scope. Where a design note says a safeguard cannot prevent an in-scope act (e.g. a writer changing the store during `verify-log`), detecting it and refusing the result (`could_not_run`) satisfies the contract. D43 never relaxes an existing contract, authority or sandbox boundary, or 04 §4's operator gate. Reachability makes a finding eligible; it blocks only at Critical/High. Amended by D45 (2026-10-09). |
| D44 | What the PR size limit counts (2026-10-05) | Asked by the background lead session. The ~400-line limit (04 §2) is added plus deleted lines (`git diff --numstat main...HEAD`) of hand-written non-test code, and **project-authored schemas under `schemas/` count as code**: they are contracts. Tests, test fixtures, generated output and unchanged vendored schemas do not count, so size never pushes toward fewer tests. D41's cohesive-package exception stands. |

### Process amendment after M0 (operator, 2026-10-09)

Following Astra's process review of 2026-10-09, after M0 exited (`docs/reviews/2026-10-09-m0-exit.md`). D43 and D44
stay as recorded above; where these rows differ, these rows apply from 2026-10-09.

| # | Topic | Decision |
|---|---|---|
| D45 | Threat model, amending D43 | **The supported local deployment trusts controller-owned storage and approved controller code.** Arbitrary fabricated histories (direct store edits, events manufactured outside supported commands) are outside the current security claim. **Still in scope:** untrusted inputs (API clients, adapters, repositories, supervisors), invalid transitions the controller itself generates, crashes, stale authority and uncertain effects. Folds keep their deterministic transition and consistency checks; authority and applicability are checked transactionally at admission, at reuse and at effect intent, with consumption checks where a contract requires them. Existing safeguards stay. **Trust boundary:** the controller principal (a dedicated service uid plus enforced confinement) owns the storage, credentials, configuration and installed executable. Builders, tests, host-side model harnesses and the unattended supervisor cannot reach that authority through files, inherited descriptors, sockets or process control; they submit constrained commands or observations, never authoritative event payloads. Installing new controller code stays independently reviewed. A same-user developer installation is supported and treats that user's unrestricted processes as **controller administrators**. The hash chain detects changes to an anchored prefix; it is **not writer authentication**. D43's "normal filesystem access" and "processes that ignore a cooperative lock" stay in scope for every principal except the controller principal. **Deferred: M7-S, untrusted-history and import hardening** (authenticated writers, verified import, untrusted-history replay). M7-S must land **before** any store import, any change to remote-storage trust, or any additional store writer; remote model capacity alone is not a store writer. |
| D46 | Review and PR rules | 04 §2–§3 as amended: blocking is Critical/High inside D45's threat model, plus four bounded acceptance obligations; every other Medium/Low goes to one deduplicated backlog issue per PR; one defect class gets one repair across its entry points; after two NO-GOs the contract is settled before more code is split; PRs are typically 300–600 hand-written lines with a ceiling of about 800 (D44's counting stands, and replaces D38's and D41's ~400); mutation or equivalent fault evidence is required only for the invariant classes 04 §2 lists, and the red-proof rule (11) stays. |
| D47 | Milestones M1–M6 as vertical slices | 01 §4 as amended: M1 one real task (amended by D52, 2026-10-12: M1 progresses through three outcome checkpoints, a live reviewed candidate, a useful approved merge on this repository's GitHub and safe repetition, and minimal GitHub delivery moves forward from M4 into M1), M2 this repository, M3 unattended recovery, M4 actual delivery, M5 backlog to approved work, M6 controlled self-host (≥5 substantive changes, D15). Exits that move past M6 are listed in 01 §4.1's deferral ledger. D19 and D26 (the v1 release bar) are unchanged. |

### Routing vision (operator, 2026-10-12)

The operator's vision, verbatim except that the proprietary product it names is written here as "a governed
upstream": *"In my mind this auto-dev-cycle will be a dark factory with a resource selecting optimized gateway +
[a governed upstream's] explainability and traceability. Dynamically adapting model choosing strategy for the project.
With an additional config for model sovereignty."* The operator chose among recommended options after Astra's
vision-fit review (2026-10-12) and an integration brief on an external governed model gateway.

| # | Topic | Decision |
|---|---|---|
| D48 | Routing vision and governed upstreams (2026-10-12) | Refines D5, D6, D9, D28 and D47. **(1) Governed upstream, M2 or later.** An external governed gateway may sit as a registered destination **behind** ADC's model gateway, which keeps leases, fencing, intent-before-egress, credential handling and accounting; the upstream adds identity-bound audit and model attestation. No proprietary upstream implementation code enters ADC; product-specific adapters and setup notes are distributed separately. The upstream does not replace ADC's gateway, and its identity or policy engine does not enter ADC's authority; an export interface was not chosen. **(2) M1 unchanged.** The P7 registry schema stays open to a governed-upstream destination with an attestation tier; P11 does not preclude per-destination header or trust-secret injection at the gateway, never in the sandbox; Q8's narrowing is an M1 supported-transport limit, not a narrowing of the executor, lease or sovereignty abstractions. The gateway enforces policy and never selects or silently changes a model; selection belongs to the scheduler. **(3) Explainability.** ADC's own routing-decision record is authoritative: candidate set, exclusion reasons, strategy version, fallback cause, selected executor and destination; a governed upstream's correlation or provenance id is attached to the `model_call` observation. The record arrives **with the scheduler, not in M1** (operator's choice over a minimal M1 record). **(4) Adaptive per-project model choice** is a measured experiment after T1, ranking only among admitted executors: configured baseline, then health- and cost-aware ranking, then learned ranking. Metrics: accepted work per total cost, latency, repairs, escaped defects as a guardrail. Promotion and rollback are recorded; zero promotions is valid. D6 holds: learned policy never authorizes destinations, paid fallback, independence or stop/continue. **(5) A named routing slice, M6-R,** after M6 and before M7, with explicit exits: T1, placement across ≥3 source kinds, fairness, builds that cannot starve review, fallback optimisation, the routing-decision record and the adaptive experiment. D47's order stays; this replaces "post-M6" for those ledger rows (01 §4.1). **(6) Model sovereignty** keeps one source: project `data_policy`, trust classes, destination allowlists and the D28 usage profiles. An **`attested`** verification tier is added as a registry property (the model's identity is proven, e.g. by a governed upstream's attestation), in M2; 01 §2.2 defines it as cumulative over `verified`, with trust class, recipients, retention and allowed destinations still checked independently. No second sovereignty configuration. **(7) Naming.** ADC docs describe a generic governed-upstream and attestation interface and name no product; any product-specific adapter or setup note is distributed separately. |

### Replan (operator, 2026-10-12)

After field research on 31 working agent factories (2026-10-12) and Astra's re-evaluation of the plan, the operator
adopted the decisions below, chose the validated-pull kanban (option C), made a bounded issue in this repository the
first real target, and asked for a weekly Monday review on the Development log (#3). The wording follows Astra's
re-evaluation; the D63 liveness rule was added in the operator-facing report. Where a row below differs from an
earlier one, the row below applies from 2026-10-12.

| # | Topic | Decision |
|---|---|---|
| D49 | Development (supersedes D38's planning) | Development reconciles one current outcome goal against observed evidence after every merge or failed live run. Detail the next three to five items; preserve D45–D46 and operator-gated changes. 04 §1–§2. |
| D50 | Product (supersedes D5) | ADC's product is governed unattended delivery across heterogeneous harness, source and model capacity. Scheduling is measured by accepted work, total cost, latency, interventions and escaped defects. |
| D51 | Kanban | The authoritative board is a projection of controller records. Executor slots may request ready work; only controller-validated claims authorize execution through leases, fencing, reservations and policy checks. Matching is done in batch. 01 §2.2. |
| D52 | M1 checkpoints (amends D47) | M1 progresses through live reviewed candidate, useful approved merge and safe repetition. Each capability activates only after its reachable invariants are enforced; deferred capabilities remain inaccessible. 01 §4. |
| D53 | Schedule (supersedes D20) | Milestones close on evidence. Weekly calendar reviews assess outcome progress and revise scope or forecasts; dates never waive exit criteria. 01 §4.2, 04 §5. |
| D54 | Multi-harness | Before self-host, two distinct harnesses deliver accepted work under a published conformance matrix. Registration is configuration-driven within qualified compatibility constraints. |
| D55 | Gate ledger | Every recurring gate records cost, validated catches and false blocks. Retirement requires an approved evidence-based decision; required safety properties retain equivalent enforcement. 04 §3. |
| D56 | Review qualification | Reviewer configurations must discriminate planted defects from clean controls. Sealed-suite plurality and divergence-only opinions are measured, risk-scoped experiments. |
| D57 | Delegation | Automated policy changes may only tighten the approved envelope. Standing delegations use existing scoped grants and carry `stop_on` and `max_actions`; widening retains human confirmation. |
| D58 | Enforcement visibility | Every active invariant identifies its enforcement and qualification evidence. Required unenforced invariants block capability activation. |
| D59 | Reconciliation | A pure reconciliation decision selects typed actions from snapshots; execution rechecks authority and revisions, persists effect intent and supports periodic resynchronization. |
| D60 | Health | Health findings include evidence, owner, resume signal and a suggested authorized repair; findings never confer repair authority. |
| D61 | Routing evidence (amends D48's order) | A small mixed-capacity comparison and placement explanations precede self-host. Full T1, fairness and learned ranking remain separately qualified; zero learned-policy promotions is valid. |
| D62 | Fence assurance | Concurrent claim and recovery changes receive a bounded formal fence model and discriminating fault fixtures before activation. |
| D63 | Liveness | Every non-terminal task carries a running attempt, a requested or granted lease, a registered hold, or a claimable producer; otherwise a `stalled` hold owned by the operator is raised and paged. 01 §2.2. |

**Superseded:** D5 by D50, D20 by D53, D38 by D49 (D38's roles, review, merge and reporting rules stay, as amended by
D46). The superseded rows above are kept as the record.

**D1–D48 re-evaluated.** **KEEP is the default:** every decision not listed below stands unchanged. The verdicts
concern decisions, not the removal of existing safeguards; an AMEND is carried out by the later plan change that
applies it, not by this table.

| # | Verdict | Reason |
|---|---|---|
| D3 | AMEND | A staged, measured compatibility matrix (D54). |
| D4 | AMEND | Add useful real-project delivery. |
| D5 | SUPERSEDE | Governed delivery is the outcome (D50). |
| D6 | AMEND | Remove the implied need to deploy Jev now. |
| D8 | AMEND | A versioned outer plan; bounded approved units. |
| D14 | AMEND | Start with minimal board and confirmation surfaces. |
| D15 | AMEND | Deliver ADC changes before self-install. |
| D20 | SUPERSEDE | Calendar reviews expose stalled value (D53). |
| D21 | DEFER | The full slice follows core delivery and self-host. |
| D22 | AMEND | Conditional on installed-version confinement. |
| D24 | DEFER | No demonstrated idle-capacity need yet. |
| D33 | AMEND | Add repair packets and bounded delegations (D57, D60). |
| D34 | DEFER | Wait for measured decision volume. |
| D35 | AMEND | Portable continuity now; curation later. |
| D38 | SUPERSEDE | Goal reconciliation (D49); review and accountability stay. |
| D42 | AMEND | Tie delivery to actual platform activation. |
| D43 | AMEND | Consolidate with D45 to remove ambiguity. |
| D46 | AMEND | Track delay; prevent new invariant scope inflation. |
| D47 | AMEND | Earlier activation and useful GitHub delivery (D52). |
| D48 | AMEND | Earlier routing evidence (D61); learning and attestation later. |

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
