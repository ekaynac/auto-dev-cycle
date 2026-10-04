# auto-dev-cycle v1 — first open-source release plan

Status: revision 3, 2026-10-02 (knowledge layer folded in, D35–D36). Revision 2, 2026-10-01: This revision applies Astra's review (`reviews/2026-10-01-astra-plan-review.md`) and
the operator's rulings on it (D23–D27 in `00-brainstorm-decisions.md`). It builds on decisions D1–D27 and on the
know-how in `docs/handoff/01..03`. "nöbetçi" means the current framework (`/home/mega/nobetci-dev`). It is reference
material only and is never modified. Astra finding numbers are cited as `[A#]`.

---

## 1. What v1 is

auto-dev-cycle is an unattended software-development cycle. It turns requirements into merged, gated and reviewed
changes on any git repository, in any language. It does that by **harvesting model capacity from wherever it can
find it** and keeping that capacity busy:
- subscriptions;
- paid APIs;
- free tiers;
- Ollama Cloud;
- local GPUs;
- idle machines anywhere.

It is designed to be operated by a human **or by a supervisor AI** through the same command surface (D23).

What v1 must prove:

1. **One capacity layer for every model and every harness.** A role asks for a *capability* and the scheduler finds
   an executor with headroom on a resource that is actually free. No role names a model in code.
2. **A framework-native planning head.** Requirements go in. A plan made of bindable acceptance rows comes out. At
   dispatch, each approved unit is run sequentially or in parallel to fit live capacity.
3. **The proven, conservative core of nöbetçi:**
   - a patch-only isolated builder;
   - content-bound gates and receipts;
   - an independent reviewer;
   - the amended simple phase gate;
   - deterministic records;
   - four outcomes;
   - holds that always name their resume signal.

The tailoring is gone: Python/pytest, GitHub-only, one host, one product and Turkish tokens.

---

## 2. Architecture

```
                 ┌──────────────────────────── controller host (Linux) ─────────────────────────────┐
 operator ─ web ─┤  adcd (Go) — one controller API; web dashboard and `adc` CLI are both clients    │
 supervisor AI ──┤   ├─ authority: actors, grants, approval records (digest-bound, transactional)   │
   (adc CLI)     │   ├─ project registry ── profiles (policy, destinations, toolchain, forge)       │
                 │   ├─ planning head ───── plan store (versioned, approved revisions)              │
                 │   ├─ cycle engine ────── stages, four outcomes, holds+signals, attempts+fencing  │
                 │   ├─ capacity layer                                                              │
                 │   │    ├─ source registry + resource groups (shared quota / GPU / concurrency)   │
                 │   │    ├─ scheduler (atomic reserve/settle, fairness, fallback re-checks)        │
                 │   │    └─ model gateway (destination check, credential injection, telemetry)     │
                 │   ├─ merge queue (tree-bound evidence) ── forge: git | GitHub | GitLab            │
                 │   ├─ sandbox runner (prepare → untrusted build/gate → controller-managed env)    │
                 │   ├─ host-role runner (confined Claude Code / Codex for plan + review)           │
                 │   ├─ evidence + accounting (SQLite WAL, append-only event log)                   │
                 │   └─ pager (webhook) + external watchdog                                         │
                 │  executor adapters (TS, JSON-RPC over stdio):                                    │
                 │   claude-code · codex · opencode · omp · http-chat · systemone                   │
                 └──────────────────▲───────────────────────────────────▲────────────────────────────┘
                                    │ HTTPS (direct endpoints)          │ dial-out tunnel (enrolled agent)
          ┌─────────────────────────┴─────────┐          ┌─────────────┴─────────────────────────────┐
          │ APIs: OpenRouter, Anthropic,       │          │ adc-agent (Go, Linux/macOS/Windows)       │
          │ OpenAI/Codex, Ollama Cloud,        │          │  relays only to allowlisted local         │
          │ free tiers, any reachable endpoint │          │  endpoints; reports idle/window; drains   │
          └────────────────────────────────────┘          └───────────────────────────────────────────┘
```

### 2.1 Core vocabulary (schema-declared, English only)

| Term | Meaning |
|---|---|
| **Source** | Something that can do model work under one credential: an endpoint, a broker key, a subscription, a free tier, an agent-relayed local server. It declares its **destinations**: who actually receives the data (upstreams, relays, retention terms). |
| **Resource group** | A shared scarcity several executors draw from: an account quota, a broker balance, a GPU, a concurrency limit, the controller's gate CPU [A3]. |
| **Model** | A model id as served by a source, with declared and measured capabilities. |
| **Harness** | Software that drives a model through a task (Claude Code, Codex, opencode, omp, or a plain request loop). It owns its tools and session semantics. |
| **Executor** | A harness × a source × a model. The unit the scheduler places work on. |
| **Role** | A pipeline function defined by **requirements**: capability floor, data floor, cost ceiling, independence rule, plus a configured preference/fallback chain. |
| **Lease** | An atomic reservation across every resource group an executor draws from, with priority, footprint and deadline. Settled with measured usage. |
| **Attempt** | One execution of one unit under one lease, carrying a monotonic **fencing token**. Results from a superseded attempt are rejected and recorded [A4]. |
| **Actor** | Who issues a command: `operator`, `supervisor` (an AI acting under a grant) or `system`. Every action records its actor [A8, D23]. |
| **Grant** | A scoped, dated, expiring delegation of action kinds from the operator to a supervisor actor. |
| **Project** | A repository plus its profile: toolchain image(s), dependency recipe, test/gate command, integration environment, forge, plugins, data policy, allowed destinations, priority, budget, protected paths. |
| **Plan / Task / Unit** | The planning head's approved output: phases → task DAG → approved units. |
| **Candidate** | A task's branch under construction (one cumulative patch). |
| **Receipt** | Evidence that names its instrument (hash), the exact tree it is bound to, its freshness, and both outer and inner status. |
| **Outcome** | `found` / `empty` / `could_not_run` / `failed`. Every result carries one plus a **reason code**, e.g. `routing_violation` or `quota_unknown` [A15]. There is no boolean "ok". |

### 2.2 Capacity layer (the centre of v1)

**Source registry.** Each source records:
- its kind and models;
- its resource-group memberships;
- price basis;
- trust class plus explicit destinations and retention;
- availability window;
- measured health.

**Quota state is never assumed** [A3]:

| Field | States |
|---|---|
| quota | `known` (from provider headers or usage endpoint), `estimated` (learned from limits), `unknown` |
| source state | `healthy`, `degraded`, `exhausted(reset_at)`, `exhausted(reset_unknown)`, `invalid_config`, `revoked`, `retired`, `quarantined` |

Each source state maps to its own resume signal:
- reset time;
- bounded probe schedule;
- config revision;
- operator act.

Quarantined sources are re-probed on a bounded schedule with a real generation request, never `/v1/models` alone.

**Health from evidence.** Per-turn telemetry records:
- error class;
- time to first token;
- stream chunk gaps;
- cache-hit ratio;
- the reported upstream;
- degenerate-output rate.

Thresholds quarantine a source with a receipt. Health never comes from configuration.

**Scheduler** [A3]:
- `reserve(role, requirements, priority, footprint, deadline)` atomically reserves **every** resource group the executor
  draws from. It returns a lease or `wait(signal)`. Leases are settled with measured usage. Usage that cannot be
  measured stays `uncertain` until it is reconciled. Uncertain usage is never dropped and never assumed zero.
- **Placement and every fallback step** re-check all of:
  - the project's data floor and allowed destinations;
  - the capability floor;
  - the role independence rule;
  - paid-fallback authority;
  - budget.

  A fallback never widens what the initial placement could do [A3, A7]. Each chain entry is authorised for a
  **role and mode** (e.g. a learning digest ≠ a phase review), checked in the placement path (nöbetçi Δ§4).
- **Footprints are learned per executor**, not per role: on one identical review packet nöbetçi measured 9× the
  input tokens for one reviewer family against another. Priors come from the nöbetçi measurement export (§4 M2).
- **Rate windows and credit balances are separate resource groups.** A broker-balance lease needs a fresh balance
  probe. Credit exhaustion (402) is `could_not_run/broker_credit_exhausted`, a hold on balance probe / operator
  top-up, with partial work sealed, never read as "no changes" (nöbetçi Δ§14).
- **Never fail a task because one source is exhausted.** Walk the chain. Otherwise `wait` on the earliest *known*
  reset. When no reset is known, wait on a bounded probe schedule plus a page if the wait exceeds the project's
  threshold.
- **Progress and fairness:**
  - priority with aging;
  - per-role reserved shares, so builds cannot starve review: a new build is not admitted when the reviewer capacity
    for its candidate is not reservable;
  - the controller's gate/integration resources are a resource group too;
  - projects are weighted by priority and budget;
  - a parked candidate never blocks other projects.
- **Preemption.** Cycle work preempts filler work. Draining sources get no new leases (§2.5 drain/resume).

**Model gateway:**
- Every HTTP-speaking executor in the sandbox reaches models **only through the gateway**. The gateway:
  1. checks the destination against the project's allowed destinations **before egress** [A7];
  2. injects the credential, so the builder never holds a key;
  3. fsyncs the call id before egress;
  4. records every turn;
  5. enforces the lease.
- **Routing is labelled `verified` or `reported`.** It is `verified` when the gateway itself chose the final
  destination (a direct endpoint, an owned server through an allowlisted agent). It is `reported` when only the
  provider says so (broker upstreams). Projects may require `verified` for private content.

**Accounting.** One append-only per-call ledger:
- tokens, including cache;
- turns and seconds;
- `cost_basis ∈ {cash, subscription_equivalent, free, local}`;
- per-call deltas, never session totals;
- estimates calibrated against provider usage endpoints.

Cancellation is never assumed to prevent billing. Counters and charges are never reset.

**Usage profiles (D28).** Model-usage settings start from one of three built-in profiles, chosen per installation
(and overridable per project). Each profile has a recorded **intention**. Every usage setting, including every setting
added in future releases, must declare its default under each profile, so the intention stays true as the settings
grow. After a profile is chosen, any setting can be changed individually. The effective configuration shows which
values are profile defaults and which are overrides.

| Profile | Intention | Defaults (summary) |
|---|---|---|
| **`standard`** (default) | An average developer's setup: strong subscriptions, own local models, and paid/broker APIs **only through providers that neither retain nor train on the data**. | Subscription harnesses (Claude Code, Codex); own/local endpoints and agents; broker routes restricted to no-retention, no-training providers (e.g. OpenRouter data-collection deny / zero-retention endpoints); free tiers that log or train are excluded for code. |
| **`open`** | Harvest everything: use every free source and model as fully as possible. | All configured source kinds, including free tiers with logging/training; maximal filler; paid fallback still only where flagged (D11); strict ToS still applies (D12). |
| **`on-prem`** | Fully in-house development: nothing leaves the organisation. | Only own endpoints and enrolled agents with `verified` routing; no external APIs, brokers or subscriptions; the model gateway refuses all other egress. |

A project's data policy and allowed destinations (D9) are evaluated **within** the active profile. A project can
narrow the profile, and it can widen it only by an operator-only action.

### 2.3 Executor adapters

- **Contract.** A TS adapter is a process speaking JSON-RPC over stdio to the Go core. Calls:
  - `describe()`: harness, version, capabilities, session semantics, tool surface, telemetry depth;
  - `prepare(spec)`;
  - `run(spec, fencing_token) → event stream`;
  - `result()`: patch | text | typed decision, with usage and outcome;
  - `cancel()`: best effort only; the core never relies on it for checkpoints [A4].
- **v1 adapters:**
  - `claude-code`;
  - `codex`;
  - `opencode`;
  - `omp`;
  - `http-chat`: OpenAI/Anthropic shapes; covers OpenRouter, Ollama, Ollama Cloud, vLLM, LM Studio, llama.cpp, free
    tiers;
  - `systemone`: Jev/Kev typed decisions.
- **Conformance kit**, recorded fixtures:
  - stream normalisation;
  - cancel;
  - usage;
  - error classes;
  - patch boundary;
  - stale-fencing rejection;
  - **hostile repository** cases, for host-side adapters: injected instruction files, hook and MCP config in the
    repo, scripts that try to run, requests for subagents [A1].

  A contract-faithful fake per adapter is used in core tests.
- **`docs/adapters/AUTHORING.md`** is the single central guide to writing, testing and registering an adapter. It is
  a release gate.

### 2.4 Host-side roles: Claude Code and Codex (D22, [A1])

Subscription harnesses run outside the sandbox, on the host, with their native login. They serve only the planner,
reviewer and phase reviewer roles. Running host-side is not treated as isolation. An **enforced host-role capability
profile** applies:
- **Files.** The harness gets a fresh read-only snapshot of the candidate, not the live repo. OS-level confinement
  (Landlock/bubblewrap where available) restricts reads to the snapshot plus one scratch directory. Foreign
  instruction files and memory are excluded.
- **Execution.** No arbitrary shell execution. The harness gets read-only inspection tools only. A test the reviewer
  wants to run becomes a **sandbox-mediated request**: the controller runs the named test in the sandbox and returns
  the receipt.
- **Extensions.** Repo-provided hooks, MCP servers, plugins and settings are disabled. Only controller-provided,
  confined tools are allowed.
- **Child models.** Subagents and model switching are disabled. The model reported back is checked against the
  leased model.
- **Telemetry limits.** These are declared per harness (what usage and upstream data the CLI exposes). Accounting
  marks the gaps as `uncertain`.
- **Proof.** The hostile-repository conformance cases must pass on the installed harness versions.

### 2.5 Cycle engine: an artifact graph (D40; `m0/13`)

The cycle is a **graph of nodes over content-addressed artifacts**, not a stage script:
- **Nodes:** plan, build, accept, gate, integration, evidence, review, closure, merge, deploy, record, phase gate.
- **Each node declares its inputs.** Its output artifact is keyed by its exact inputs, and nothing else can
  influence it.
- **Evaluation is demand-driven.** The controller resolves a goal ("merge task T's candidate"), reuses artifacts
  whose keys already exist, runs the producers of missing inputs first, and leases what each runnable node needs.
- **Repairs are unrolled rounds**, so the graph stays acyclic.
- **The logical order stays the same:**

  `plan → build → accept → gate → review → repair* → merge-queue (rebase, re-validate) → closure → deploy → record`

  plus a phase gate at phase boundaries. Closure comes **after** the final merge tree is known [A5].

**What the graph makes structural** (narrowed after Astra's review of the redesign, `m0/13` rev 2):
- a model's **controller-supplied** context comes only from its declared inputs (harness-internal context stays
  `partial`/`unknown`);
- an inapplicable, missing or expired input makes a node wait for its producer, never run;
- `could_not_run` is retried on its signal, never cached;
- answers and new demands create successor nodes;
- equal typed conclusions reuse a review, and changed ones re-run it;
- build outcomes combine observed completion with the observed diff, never executor claims alone;
- effects are durable operations reconciled against current state, never cache hits;
- evidence targets are leases whose settlement requires verified restore;
- the merge queue runs in **generations**, with a carry-over attestation or delta review per generation;
- tests run in a source-test workspace, exactly the hashed package bytes are deployed, and runtime verification
  runs on that package with the target's runtime facts.

**Still checked explicitly:** authority and applicability are re-validated at commit (approvals can be revoked and
receipts can expire between claim and commit), and semantic acceptance review stays with humans and reviewers.

- **Accept:**
  - the patch is applied to a temp index;
  - the file list comes from git;
  - protected paths are refused, except through the protected-document change operation (§2.11, `03-knowledge-layer.md` §4.6);
  - the integrity guard runs.

  The guard distinguishes **conclusive violations** from **heuristic warnings** [A6]. Conclusive means a test was
  deleted or skipped, or an assertion was removed, without an approved row. It blocks. Heuristic warnings go to the
  reviewer.
- **Gate:**
  - the project test command runs in the untrusted sandbox phase;
  - the verdict is a mechanical comparison against a measured baseline;
  - it is bound to tree, environment identity and command;
  - per-test results come from a declared result adapter (JUnit XML, TAP, `go test -json`, or custom).

  A runner without per-test results is allowed, but per-test evidence for it is `could_not_run`, never `empty` [A15].
- **Review:**
  - a fresh session per round plus a controller digest;
  - the reviewer must be **independent of every model family that contributed** to the candidate (builders, repairs,
    closure writer). If no independent reviewer is available, wait; never relax [A5];
  - severity policy lives in code;
  - P3 is banked as background work.
- **Repair** routing is a table in one module, with wrong-owner routing: access, environment, tooling and
  infrastructure failures go to their owner through hold and page, **never** as a builder repair [A6]. Gate output is
  classified by **ownership** (`m0/07` ownership rules): a setup error in a test file the candidate added or changed
  is a builder repair; collection errors, untouched files and unaccounted counts are runner faults. Each target
  environment's **facts** (VCS metadata, network, runtime) are published to the builder and the reviewer
  (nöbetçi Δ§7).
- **Drain and resume** [A4]:
  - *Graceful drain.* The source announces the drain. The executor is asked to stop at a turn boundary. The
    controller seals a checkpoint: notes, trail, the partial worktree diff, the evidence manifest. The checkpoint
    lives on the controller, because the sandbox does.
  - *Abrupt loss* (stream dies, agent disconnects). The attempt is marked `interrupted` and its usage `uncertain`. The
    sandbox state is sealed as a checkpoint if its evidence manifest is intact.
  - A partial patch survives **only** through the normal path: accept → gate → review as part of the continuing
    candidate. A sealed but unreviewed candidate change always gets a builder continuation before closure (the
    `closure_material_missing` lesson).
  - *Controller restart.* Uses the `host_restart` disposition: charged once, interrupted, unsealed work not imported.
  - *Fencing.* A late result from a superseded attempt is rejected by its fencing token and recorded.
- **Merge queue** (per project and base branch) [A5]:
  - The candidate is rebased onto the current base.
  - If the rebased change equals the reviewed change (patch-id equality, no conflict resolution), review and closure
    evidence carry over and only the gate and target receipts are refreshed.
  - Otherwise the conflict resolution gets a **delta review** and every receipt is refreshed.
  - All evidence is bound to the **final prospective merge tree**.
  - The push uses an expected-base check (compare-and-swap through the forge API or a lease-protected push).
  - An ambiguous remote result is reconciled by a done-probe.
  - A base moved by an external writer is detected, and affected candidates re-enter the queue.
- **Holds.** Every hold reason is an enum value naming its resume signal. The watchdog refuses unknown reasons.
  Healthy is silent.
- **Parallelism.** Any number of independent tasks, bounded by capacity, resource groups and owned-path overlap.

### 2.6 Simple phase gate (ported with its amendments, [A6])

1. A claim list of ≤25 claims (≤12 blocking) per phase, with source pointers. **Requirement reconciliation:** every
   required row in the phase is covered by a claim or explicitly deferred by an approved record. The claim cap
   groups requirements; it never deletes scope.
2. Deterministic checks first:
   - the full gate on every target environment;
   - the integrity guard;
   - required receipts present, current for the final tree, and passing on their **inner predicate**.

   Evidence is controller-produced. Missing evidence never passes.
3. One independent reviewer proves claims with test id + output + code lines. It **inspects assertions and mocks**
   for each blocking claim and states which wrong implementation the evidence would catch.
4. After a repair, the reviewer re-reads **the whole repair diff** and re-checks every claim it touches.
5. Wrong-owner problems go to their owner. They are never builder repairs.

Each rule has a test in T0.

**Owed inputs.** When the reviewer needs something only an outside party can give (a ruling, lab data), it records
a typed owed input. The answering approval names it, and the answer becomes part of the next review's input
fingerprint, so it re-keys the review. Every evidence demand the reviewer makes (e.g. a walk plan) is part of that
fingerprint too. Evidence production follows the evidence-job lifecycle (`m0/12`) (nöbetçi Δ§1–3).

**Strict options** (nöbetçi Δ§10; off by default; one gate code path, never a second gate). A project may enable,
per phase:
- **Reviewer pair** [PR5-4]: two reviewers of different vendors/families on the identical packet, each in a fresh
  session.
  - **Precondition:** two complete, applicable review receipts, each satisfying the independence rule (`m0/06`
    rule 3) against every contributor. An unavailable member is **waited for** (bounded, then paged) and never
    replaced by a second session of the other family. A refutation never stands in for a missing member.
  - **Claim-level merge:**

    | Member A | Member B | Result |
    |---|---|---|
    | pass | pass | pass |
    | fail | any | block (repair) |
    | pass | missing (no conclusion on this claim) | contested: one refutation call by the missing side's member, with passing test ids or the claim's proof receipt; unrefuted → pass, refuted or no answer → repair |
    | missing | missing | repair |

  - **Receipt-only claims** (`proof_receipts`) pass only on their applicable receipt. Reviewers can block them
    but cannot pass them without it.
- **`test_ids` required:** every pass row names test ids, checked by the controller against the per-test results
  for this tree. Unknown or failed → missing; unreadable results → hold.
- **`proof_receipts`:** a claim provable only by a receipt declares it, so it can pass on that receipt.
- **Dispositions:** an operator-owned, schema-validated file of deferrals (`moved_to_later_phase` with triggers,
  `substituted`, `unperformed_pending_authority`, `transition_condition`, `recorded_only`), each with an owner
  and an approval digest. Validated at admission and re-checked at acceptance; a pass on a deferred claim is a
  hold.

With no strict options, the simple gate's packet, schema and prompt are byte-identical (tested).

### 2.7 Planning head

- **Input:** requirements documents (an SRS with ids, a roadmap table, issues) plus doctrine/canon documents.
- **Compile.** The planner role produces a **plan document** (versioned schema):
  - phases;
  - a task DAG;
  - per task: acceptance rows traced to requirement ids, a test-name convention, `remaining` human acts, canon
    anchors, dependencies, owned paths, tags (difficulty, risk, size);
  - **approved unit boundaries** where the planner proposes parallelisable units;
  - per phase: a claim list and required receipts.

  Missing requirement ids are assigned and the mapping recorded.
- **Validators** [A12]:
  - *Mechanical, blocking:* schema valid; every required row covered or deferred by record; anchors resolve;
    dependencies acyclic; units' owned paths disjoint; no task requests protected-path edits; every hold reason has a
    signal; human acts enumerated; no builder-only closure prose; **every acceptance row binds to a concrete
    producer instance** (a builder test id pattern; an evidence node spec with instrument, subject, demand and grant;
    or a human act with actor and completion artifact). The validator proves wiring and capability, not semantic
    satisfiability, which stays with contract review (D40, `m0/13` rev 2, nöbetçi Δ§9).
  - *Judgement, advisory:* size versus builder economics, whether acceptance is whole. Reported to the approver. Never
    a blocking heuristic.
- **Approval** goes through the authority layer (§2.11). Later changes are typed revisions (add, prerequisite,
  hold/release, replace, amend, carry, rows, context). A started task's contract changes only by an approved amend.
- **Dispatch-time shaping (D8, narrowed by [A12]).** The scheduler decides only **sequential vs parallel execution of
  approved units**, from live capacity. One gate and one review run on the combined candidate. Arbitrary
  re-partitioning or merging of units is experimental. It stays off by default until T1 shows a benefit.
- **Builder hand-off** is data: plan sidecar, dossier, notes, trail. Language-specific parts sit behind toolchain
  plugins. v1 has Python, TypeScript/JavaScript, Go and PHP.

### 2.8 Classifier program (D6, [A13])

- **Port.** One `/v1/systemone`-shaped interface (Jev now; small Kev models on local/Ollama paths).
- **Shadow mode first.** A classifier's answer is logged next to the rule's decision and has no effect.
- **When promoted** by an approved record citing its eval report, a classifier may **only rank among choices the
  deterministic policy already admitted**. It can never:
  - suppress a finding;
  - authorise a fallback;
  - terminate a call;
  - establish independence;
  - feed a gate verdict, severity, authority or stop/continue.

  `null` or low confidence means the rule decides.
- **Evaluation:**
  - reviewed labels, not raw historical outcomes;
  - held-out splits separated by incident;
  - unseen providers and languages;
  - abstention rate;
  - **per-surface harmful-error measures** (false negatives that would hide a problem), not only accuracy.
- **Surfaces include the unattended supervisor's own decisions** (D34).
- **Datasets:** nöbetçi history (read-only) plus T1 decisions. Sanitised versions are published.
- **"No classifier enabled" is a successful outcome** of the program.

### 2.9 Background work (D24: framework in v1, one surface live)

- **The contract ships in v1:**
  - a work item states role requirements, data floor, a reproduce-first rule where relevant, and a receipt;
  - outputs enter the merge queue like any candidate;
  - four outcomes;
  - an independent reviewer on a different route;
  - metered even when free;
  - escalations feed the planner.
- **Filler leases:**
  - lowest priority, preempted first;
  - admitted only when the downstream review and gate demand they create is reservable, so filler cannot flood the
    pipeline (the sweeper once used ~75% of shared-key spend, and 71 of 101 receipts were escalations) [A14].
- **One live surface in v1:** banked P3 findings (the proven sweeper). Further surfaces open in v1.x, each on
  measurement.

### 2.10 Sandbox: three phases (D13, [A2])

1. **Dependency preparation.**
   - A rootless container with **no credentials**.
   - Egress only to an allowlisted package-registry proxy (npm, PyPI, Packagist, Go proxy) that records what was
     fetched.
   - Input: the lockfiles at the candidate tree. Output: a content-addressed, pinned dependency layer.
   - Install scripts run here, never in a phase that has a credential or a socket.
2. **Untrusted build/gate.**
   - A rootless container per attempt: fresh clone, prepared layer, no credentials.
   - Egress only to the model gateway (build) or none (gate).
   - Resource limits apply.
   - Output: patch, logs, results.
3. **Controller-managed integration environment.**
   - The topology (services, databases, networks, mounts) comes from the **project profile at the approved revision**,
     never from candidate-controlled compose files. Topology files are protected paths.
   - Candidate code enters only as images built in phase 2.
   - The environment runs in its own rootless runtime instance with no access to the controller's container socket,
     credentials or host mounts.
   - Its receipt is the integration evidence.

The patch boundary and identity boundary are the trust anchor. Evidence is copied out before teardown. Phase 3 is
proven on a small multi-service T0 fixture in **M1**, before anything depends on it.

### 2.11 Authority, CLI and dashboard (D14, D23, [A8])

- **One controller API.** The `adc` CLI and the web dashboard are both clients of it.
  - **Every dashboard function has a CLI equivalent.** The CLI is a superset (diagnostics, bulk operations,
    machine-readable `--json` output everywhere).
  - The CLI is the primary interface for a **supervisor AI** operating the cycle. `docs/operating/supervisor.md`
    documents it.
- **Actors and grants (D31, D32).** The supervisor AI is **external** to auto-dev-cycle; its role is defined in
  `02-supervisor-role.md`.
  - **Attended (default):** the operator token is open to everything, whether a human or the developer's own agent
    holds it. The actor type is a self-declared audit label, not an enforcement.
  - **Unattended supervisor token:** issued by the operator under a **grant** (scoped action kinds, projects, window,
    expiry). Sensitive actions are not refused; they become **proposals** with a per-action confirmation policy
    (`none | independent_review | human`):
    - self-install: `independent_review`;
    - widening data policy or destinations: `human`;
    - paid fallback: `human`;
    - agent enrolment: `human`;
    - grant creation or extension: `human`.

    Human confirmation means presence-verified: a passkey/WebAuthn assertion on the dashboard, the only channel (D37).
    The pager notifies and links. A pending
    proposal is a hold with a named signal while other work continues.
  - **Enforcement strength is stated honestly.** Supervisor-token limits hold only if the unattended agent cannot
    read operator credentials (separate OS user, container or host).
- **Approval records** bind actor, project, the exact object digest and version, scope, and the **expected current
  state**. They are applied transactionally and idempotently. Stale or replayed actions are refused.
  - *Immediate actions:* hold, revoke, quarantine, drain.
  - *Revision-bound actions:* plan approval, amend, carry.
  - *Precedence:* a revocation or narrower scope always wins over an earlier wider one. Multi-block ambiguity fails
    closed.
- **Dashboard security:** authenticated first-run bootstrap (a one-time setup token), local admin auth, CSRF
  protection, session protection. Stale-action and replay tests are part of the contract. OIDC is v1.x (D25).
- **Protected-document change operation** [KA3]: the only path for changing protected decision/convention files. An approval binds the paths, the old/new digests and the expected adopted revision. The change keeps independent review and the merge queue. It can never approve its own adoption or widen authority.
- **Model output never acquires authority** by entering a store. A plan is executable only after a matching approval
  record exists.

### 2.12 adc-agent (D10, [A7])

- A cross-platform Go binary: Linux, macOS, Windows; runs as a system service.
- It dials out to the controller over an authenticated tunnel. No inbound ports are needed.
- **Enrolment** is operator-only, via CLI or dashboard: a one-time token, then a controller-scoped agent identity.
- **Endpoint allowlist.** The agent relays only to local endpoints declared at enrolment. It reports them as
  destinations. Relaying elsewhere is a config change that needs re-approval.
- **What the agent sees:** the prompts and tool outputs of the units placed on it, which may include source code.
  Placement is limited by project policy. **It never sees repositories, credentials or other sources.**
- **Revocation** tears down live tunnels immediately. In-flight attempts are fenced and treated as abrupt loss.
- **Retention.** The agent keeps no prompt logs by default. The controller's own logs follow the project's retention
  setting.
- **Availability:** declared window and idle policy (interactive session, GPU utilisation), plus drain requests.

### 2.13 Forge and plugins

- **Forge interface:**
  - push with expected-base check;
  - change request (PR/MR or none);
  - status/checks;
  - merge;
  - comments;
  - base ref and protection capabilities as **declared parameters**, because plain git has no protected-branch
    policy [A15];
  - delivery identity as an opaque id.

  v1 has `git`, `github` and `gitlab`.
- **Deploy/evidence plugins:** evidence jobs (`m0/12`) producing receipts with inner predicates. v1 has `none`,
  `script` and `docker-compose`. `ssh` is v1.x (D25). Drift found before apply is recorded and routed to its owner,
  never charged to the candidate.
- **Pager:** webhook in v1. Telegram and email in v1.x (D25). Durable outbox. Pages only on real blockers.

### 2.14 Operations

- `adcd` runs as a user service on a Linux controller. Capacity agents are cross-platform; the controller is not
  (stated support matrix) [A15].
- A separate watchdog unit pages when `adcd` is silent.
- Self-update is staged and content-pinned. It lands at an idle boundary, one at a time, with receipts.

### 2.15 Knowledge layer (D35, D36)

**`03-knowledge-layer.md` is authoritative for knowledge semantics; this section only summarises it.**
- Mechanical knowledge comes first: authoritative state, regenerated views fingerprinted by their exact inputs, and
  attempt continuity sealed at every builder exit.
- Durable curated knowledge ships only if the M2 fixture-lesson ablation (E1L) shows it helps (K3).
- Every model call writes a `ContextManifest` with assembled / delivered / unobservable coverage.
- Decisions and conventions are protected repo files, changed only through the protected-document change operation
  (§2.11).
- Framework lessons are shared across projects (K11) with lineage-inherited source restrictions, template-only
  automatic export, and human export approval for prose.
- No memory service, vector/graph DB, embeddings, self-editing memory, resumed sessions or harness-native memory as a
  source of truth.

---

## 3. Benchmarks (D7)

| Tier | Content | Purpose | Runs |
|---|---|---|---|
| **T0** | Tiny repos in Python, TypeScript, Go and PHP; one **multi-service fixture** (two services + a database + a bus) for the integration environment. Fault fixtures: lying test output, protected-path edit, test deletion, provider 4xx, idle cut, hostile repo for host roles, stale attempt, base moved externally, non-JUnit results, missing `.git` at runtime, non-English requirements. | Correctness of cycle, invariants, gate rules | Every PR (fakes); nightly with one real cheap source |
| **T1** | ~20–30 standard tasks with known-good answers and hidden acceptance tests, across Py/TS/Go/PHP | Baseline for routes and costs; **established in M2, before shaping or classifier work** [A9]; source of reviewed classifier labels | Nightly / on demand |
| **T2** | CTIS-SIMS greenfield from its SRS/SDD/test design, v1 slice below | Release showcase | Milestone runs and the release run |

### 3.1 CTIS-SIMS v1 slice (D21) with its scope contract [A11]

| Service | Stack | Requirements in scope |
|---|---|---|
| Identity | PHP 8.4 / Laravel, `mysql-identity` | FReq8.1, FReq8.2, FReq8.3, FReq6.2.1–6.2.3 |
| Inventory | PHP 8.4 / Laravel, `mysql-inventory` | FReq1.1.1 (including QR generation), FReq1.1.2, FReq1.1.3, FReq1.2, FReq5.1.1–5.1.3; publishes domain events |
| Audit | Node.js / Fastify, `mysql-audit` (append-only) | The change history of FReq1.1.2 and the archive actor of FReq1.1.3; read-only API |
| Notification | Node.js / Fastify, `mysql-notification` | FReq3.4 (WebSocket alerts plus the scheduled check) |
| AI + MCP | Python / FastAPI (LangGraph), MCP tool gateway | FReq7.1 (**including Turkish**), FReq7.3, FReq7.5 (retry, needed by 7.6), FReq7.6, FReq7.7. Read-only inventory queries; write intents are v1.x |
| Gateway + bus | Nginx with JWT validation; Redis pub/sub with the SDD event contract | Cross-cutting |
| Frontend | React + TypeScript | Login, inventory CRUD/search, audit view, low-stock toasts, chat panel |

- **Precedence.** The SRS decides behaviour. The SDD decides stack and architecture. The test design decides test
  strategy. A conflict between them is resolved by a recorded scope decision.
- **AI checks.** Orchestration logic is tested with a mocked LLM. In addition, a **bounded live semantic check** runs
  a fixed set of Turkish and English questions against a configured model and produces a receipt.
- **Beyond happy paths:** audit delivery across a service interruption, role revocation propagating to MCP queries,
  RBAC denial paths.
- **Out of the slice (v1.x):** transactions, workflow, reporting, MinIO, backup, QR scanning (FReq5.2).
- **Scope contract approved** by the operator (D30).
- **Redistribution:** the CTIS-SIMS3.0 requirement documents (SRS, SDD, test design) are **published as public T2
  benchmark inputs** (D29).

### 3.2 Release run (D19 as tightened by D26, [A10])

The v1 release is declared on one **release run**:
- T0 and T1 green on Python, TypeScript, Go and PHP.
- The T2 slice delivered **unattended after initial approvals**. Plan and revision approvals are allowed. **Any rescue
  fails the run:** a manual restart, a hand edit, a state fix.
- **Measured accepted contributions** (accepted patches or decisions) from each of:
  - a subscription harness;
  - a paid or broker API;
  - a local or Ollama source;
  - a **dial-out agent** source.
- **Injected faults during the run:**
  - quota exhaustion;
  - agent disconnect;
  - controller restart;
  - an external base move.

  The run verifies afterwards: no unauthorised fallback, no duplicate delivery, no cross-project disclosure, and an
  unrelated second project kept making progress.
- Self-host running (M6).
- All exit checks reproducible by a stranger from published fixtures.

---

## 4. Milestones

No dates or durations (D20). Each exit check is written as **command + fixture + expected result + prerequisites**.
It runs against installed artifacts, not the source tree [A9].

| M | Name | Contents | Exit check |
|---|---|---|---|
| **M0** | Contracts & foundations | Repo layout (Go module + `adapters/` workspace), Apache-2.0, CI. Schemas: outcome + reason codes, authority/approval/grant, lease/attempt/fencing, receipt, event store, project profile, **`ContextManifest` with coverage levels + ref/fingerprint/source-restriction types; shared knowledge spec draft: schemas, invariants and fixture definitions only (K10, K12)**. **Artifact graph: Node/Artifact schemas, key computation, resolver and evaluator with fakes (`m0/13`); evidence node parameters (`m0/12`). Anonymised nöbetçi incident goldens in `fixtures/legacy/`.** Controller API skeleton + `adc` CLI. Test write-guard. Contract-faithful fakes. T0 fixtures | CI green; schema docs generated from code; fake-driven T0 dry run passes |
| **M1** | One task, one source, safely | Three-phase sandbox (phase 3 proven on the multi-service fixture). Minimum gateway + accounting. Recovery (restart, interrupted, fencing). Accept/gate/review with the host-role profile; amended simple-gate checks; closure; `git` forge; CLI approvals; **task views fingerprinted by exact inputs, `AttemptRecord` as checkpoint schema sealed at every builder exit, protected-document change operation** | T0 Python delivered on GLM-5.3-flash or Qwen3.8 27B; fault fixtures yield the right outcome + reason; controller killed mid-build recovers without double charge; hostile-repo cases pass; **E0 manifest reconciliation passes** |
| **M2** | Capacity layer (direct sources) | Source registry, resource groups, quota states, health, scheduler with atomic reserve/settle and fairness, fallback chains with re-checks, data placement + destinations, all v1 adapters + conformance kit + `AUTHORING.md`. **T1 suite established with baseline numbers**; **knowledge ablations E1, E1L (fixture lessons, the K3 gate), E2, E4, E5; `FailureSignature` as a derived aggregate**; **one-shot anonymised nöbetçi measurement export (usage, builds, context, terminals) as T1/footprint priors and classifier-eval data** | T0 task placed across ≥3 direct source kinds; shared-quota exhaustion waits on the right signal; builds cannot starve review; T1 baseline published; **ablation decisions published, including K3** |
| **M3** | Remote capacity | Operator-only enrolment (CLI + minimal dashboard view), adc-agent on Linux then Windows/macOS, endpoint allowlist, live-tunnel revocation, graceful drain and abrupt-loss handling; the test sandbox for macOS and Windows hosts (the rootless-container fallback, D42) | An agent-relayed local model contributes accepted work; disconnect mid-attempt resumes elsewhere; a late stale result is rejected; revocation is immediate |
| **M4** | Many tasks, projects, forges | Merge queue with tree-bound evidence and delta review, parallel tasks, multi-project fairness, `github` + `gitlab`, TS/Go/PHP toolchains | T0 in all four languages on two concurrent projects across three forges; external base move handled; parked candidate does not block the other project; **K11 exposure checks hold across the two projects** |
| **M5** | Planning head, phase gate, dashboard | Plan schema, mechanical validators, approval through CLI and dashboard (full parity), shaping within approved units, amended simple phase gate with **strict options** (pair, `test_ids`, `proof_receipts`, dispositions), `docker-compose` integration plugin | A small SRS fixture compiles to a plan approved by a supervisor actor under a grant and executes; stale approval refused; phase gate closes on receipts; every dashboard action reproduced by CLI in a parity test; **planner/supervisor knowledge views and the curation queue (CLI + dashboard)** |
| **M6** | Self-host | The project's own backlog runs through the cycle; independent review before self-install | ≥5 auto-dev-cycle changes delivered by the cycle (D15); **framework lessons turned into mechanisms; live E3 only if E1L was positive** |
| **M7** | Background work + classifier shadow | Background-work contract, filler leases with downstream reservation, P3 surface live. systemone port, eval harness, sanitised datasets, Jev and small Kev in shadow mode | Filler uses idle capacity without delaying cycle work or flooding review; eval report per candidate surface (zero promotions is a valid result) |
| **M8** | T2 + release | CTIS-SIMS slice, docs (quickstart, concepts, adapters, profiles, security model, supervisor operation), release run (§3.2), final name | Release run passes |

M7 may run in parallel with M5–M6. It depends on T1 (M2) and read-only nöbetçi history.

---

## 5. What is ported from nöbetçi (reference only, re-implemented in Go/TS)

| Concept | Source in nöbetçi | Note |
|---|---|---|
| Stage model, records, four outcomes | `bin/nb-cycle`, `bin/nb-env` `_nb_write` | English names; outcome + reason code |
| Patch-only boundary, accept, canon check | `bin/nb-accept`, `nb_canon_check` | Container phases instead of a uid |
| Gate bound to tree, baseline compare, base-broken | `bin/nb-gate-iso`, `nb_gate_results.py`, `nb_base_broken.py`, `ops/nb_baseline_lock.py` | Result adapters, not only JUnit |
| Review policy, fresh session + digest | `bin/nb-review`, `nb_review_policy.py`, `nb_phase_digest.py` | Independence from every contributing family |
| Simple phase gate **with its amendments** | `docs/2026-09-28-simple-phase-gate.md` (incl. §47ff amendments), `docs/2026-10-01-lab-deploy-recovery-design.md` | §2.6 lists the rules explicitly |
| Deterministic closure and records | `nb_row_map.py`, `nb_closure_text.py`, `nb-ledger-row` | Closure after the final merge tree |
| Dossier, sidecar, notes, trail, work map | `nb_task_dossier.py`, `nb_plan_sidecar.py`, `nb_build_notes.py`, `nb_build_trail.py`, `nb_work_map.py` | Toolchain plugins |
| Errored-build continuation, host restart, interrupted calls | `nb_errcont_*`, `docs/2026-09-12-interrupted-call-recovery.md` | Generalised into drain/resume with fencing |
| Transient classification + timed retry | `bin/nb_transient.py` | Rule floor under the error-class shadow surface |
| Hold signals, pager outbox | `bin/nb-hold-watch`, `nb_hold_class.py`, `bin/nb-notify` | In `adcd` + external watchdog |
| Typed revisions + approvals; supervisor authority windows | `ops/nb_task_add.py`, `ops/approvals/`, `2026-09-29-overnight-supervisor-authority.json` | Authority layer with grants |
| Confined phase reviewer | `nb_audit_tools.py`, `nb_scratch_sandbox.py` (Landlock) | Basis of the host-role profile |
| Planning doctrine | `docs/PLANNING_CANON.md` | Split into mechanical validators and advisory judgement, not ported verbatim as blocking rules |
| Sweeper contract and capacity-lease idea | `docs/2026-09-16-p3-sweeper-design.md`, Sol architecture review §B | Background-work framework |

**Not ported:**
- blind/inspect;
- obligation gate;
- sharded audit and assembler;
- builder-authored packages and byte pins;
- fixed cooldowns;
- hand-maintained installer lists;
- product adapters;
- Turkish tokens and the compat layer.

---

## 6. Non-negotiables (acceptance criteria for every milestone)

- Four outcomes plus reason codes; non-empty error text; nested receipts validated on their inner predicate.
- Copy evidence before teardown.
- One decision in one place; model-output schemas generated from the validating table.
- Tests prove the red they catch, run under a temp HOME, and use contract-faithful fakes.
- Validation before any irreversible write.
- Never reset counters, candidates, charges or authority; uncertain usage is reconciled, never dropped.
- Caps are rails, not budgets; information up front, not read caps.
- Every hold names its resume signal; healthy is silent; liveness never depends on an interactive session.
- Models and classifiers never decide stop/continue, severity, authority or independence.
- Data never reaches a destination outside the project's allowed set, including on fallback.
- Independent review before any framework change is installed, including self-install after M6.
- Refuse to run when unconfigured; no project literals in core; CLI parity for every dashboard action.

---

## 7. Out of scope for v1 (v1.x)

- Remote nodes executing builds or gates (D1).
- Subscription harnesses inside the sandbox (R1).
- InfraMedic migration (D4).
- OIDC, the SSH deploy plugin, Telegram/email pagers, Kev-27B quantisation (D25).
- More background-work surfaces beyond P3 (D24).
- Arbitrary unit re-partitioning at dispatch (until T1 shows a benefit).
- Classifier write-intent features and training.
- CTIS-SIMS beyond the v1 slice.

---

## 8. Risks, research and open decisions

| # | Item | When |
|---|---|---|
| R1 | Subscription harnesses inside a sandbox (API-key mode via gateway, per-run tokens) | v1.x |
| R3 | Quota learning per provider: which expose remaining quota and reset | M2 |
| R4 | Free-tier inventory under strict ToS: usage terms, data terms, rate limits | M2 |
| R5 | Idle detection across OSes without invasive permissions | M3 |
| R6 | Telemetry depth of opencode and omp (usage, upstream, stream deltas) | M2 |
| R7 | Merge-queue cost with many parallel candidates (re-gates, delta reviews) | M4 |
| R8 | Whether parallel execution of approved units beats sequential on T1/T2 | M5–M8 |
| R9 | Rootless multi-service integration runtime (MySQL, Redis, PHP, Node, Python) without a host socket | M1 |
| R10 | Name and brand availability | M8 |
| R11 | Host-role confinement depth per OS (Landlock/bubblewrap availability) | M1 |

**Open operator decisions:**
- ~~O1~~ Resolved by usage profiles (D28).
- ~~O2~~ Publish the CTIS requirement documents (D29).
- ~~O3~~ CTIS-SIMS scope contract approved (D30).
- ~~O4~~ Replaced by the attended/unattended model and confirmation policies (D31, D32).
- ~~O5~~ Supervisor role approved (D33); supervisor classifier uses join the shadow program (D34).
- ~~O6~~ Knowledge-layer open questions resolved (D36; `03-knowledge-layer.md` §10).

---

## 9. Division of labour and next steps

- The nöbetçi supervisor session keeps developing InfraMedic Phase 1 on the current framework. This repo only reads
  nöbetçi.
- Next steps:
  1. Operator decides O1–O4.
  2. Commit the plan on a feature branch and open a PR.
  3. Write the M0 design notes:
     - repo layout;
     - outcome/reason vocabulary;
     - authority/grant/approval schema;
     - lease/attempt/fencing schema;
     - receipt schema;
     - event store;
     - controller API + CLI surface;
     - executor adapter contract (draft `AUTHORING.md`);
     - project profile schema.
  4. Independent review of the M0 notes (Astra), then M0 implementation via PRs.
