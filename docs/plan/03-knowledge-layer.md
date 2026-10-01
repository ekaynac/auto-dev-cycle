# The knowledge layer: decisions, design, experiments

Status: **adopted, revision 2, 2026-10-02.** This revision applies Astra's review
(`reviews/2026-10-02-astra-knowledge-review.md`, GO with changes; all findings applied by operator ruling D36).
Findings are cited as `[KA#]`.

**This file is authoritative for knowledge semantics.** D35/D36 record adoption; plan §2.15 summarises by reference
[KA9].

Origin: a brainstorm between the operator and Claude (Opus 5.5), following
`docs/handoff/04-knowledge-layer-brainstorm.md`.

## Inputs

- The starting proposal: `docs/research/2026-10-01-memory-vs-fresh-context.md`.
- At the operator's request, a comparison with InfraMedic's knowledge layer, done as two independent reports and two
  syntheses in `docs/research/2026-10-01-knowledge-abstraction/`:
  - `01-claude-report.md`, `02-astra-report.md`;
  - `03-claude-synthesis.md`, `04-astra-synthesis.md`.

  The brainstorm started from the combination of the two syntheses.
- Predecessor evidence: nöbetçi's context modules and runtime metrics. Read-only.

Every decision below was chosen by the operator from options with a recommended default. Only **K11** differs from
the recommendation.

---

## 1. Intention

Knowledge must survive any executor being swapped, drained or replaced. It must help cheap and small builders without
breaking reviewer independence. And it must not grow into a second acceptance machine.

The evidence from all three sources points one way: the literature, nöbetçi and InfraMedic. Knowledge that is
**mechanical** has a track record: code truth, gates, receipts, deterministic views, lessons turned into mechanisms.
Curated-text layers are unmeasured or measured as no gain on every side:
- nöbetçi's dossier comparison is unmatched and inconclusive;
- InfraMedic has no knowledge-on/off ablation.

v1 therefore ships the mechanical classes and the instruments to measure the rest. It turns optional layers on only
on evidence.

---

## 2. Decisions

| # | Topic | Decision |
|---|---|---|
| K1 | Success criterion | **Correct accepted work per total cost** (model + retrieval + curation), with escaped-defect and isolation guardrails. Time to first edit, re-reads and context bytes are diagnostics only. |
| K2 | Information classes | **Four classes, each with its own retention and acceptance:** authoritative state · regenerated views · ephemeral attempt continuity · durable knowledge (§3). |
| K3 | Durable knowledge in v1 | **Ships only after M2 ablations show it helps.** Until then, durable project knowledge is what the plan already has: approval and decision records, plan revisions, project-profile conventions. |
| K4 | Recurring lessons | **Typed evidence first, then choose a destination.** Prefer a mechanism (test, gate, validator) delivered as a normal candidate when the lesson is enforceable. Otherwise keep a scoped explanation, decision or negative result. Injecting lesson text into prompts stays experimental: holdout + shadow (E3) until measured. |
| K5 | Acceptance | **Confirmation policy by claim type and purpose** (§5). Mechanical evidence is recorded automatically. Advisory claims get evidence review (default `independent_review`, settable to `human`). Changes to authoritative project rules use the normal reviewed change path. Optional knowledge never blocks work; a missing **required** contract or decision is a named hold. |
| K6 | Validity | **Declared dependencies + checks.** A dependency change makes an item `stale` or `unknown` and triggers re-validation, never deletion. A resolving anchor proves location only. Derived views are bound to their exact input revisions + renderer, so a served view always matches its inputs; evidence applicability is checked separately [KA4]. |
| K7 | Reviewer exposure | **E2 tests three arms:** fresh-only · pointer digest · staged disclosure (a fresh assessment recorded first, then prior findings revealed for reconciliation). Builder-derived lineage is tracked through summaries. **Default until measured: staged disclosure.** |
| K8 | Storage | **Constrained hybrid.** Project decisions and conventions live as protected, reviewable files in the project repo; the controller records the adopted revision, never a second editable copy. Receipts, manifests, attempt continuity and private operational data live in controller SQLite. **One owner per assertion.** |
| K9 | Views per model | **Role × measured capability.** Simple presets at start: a compact whole-injected view, and bounded `list`/`search` over an authorized manifest. Which models get lookup tools is decided by measurement, not size. No model browses the store or widens its own selection. |
| K10 | Shared spec with InfraMedic | **A semantic spec now, with one authoritative home**: vocabulary, invariants, JSON Schemas, conformance fixtures from both domains. Recommended home: this repo, open source. InfraMedic vendors it pinned to a revision and maps onto it in its own session. A runtime library only after two implementations show measured duplicate maintenance. A bundle/exchange protocol only for a concrete exchange need. |
| K11 | Cross-project knowledge | **Framework lessons are shared across projects in v1** (operator's choice; recommended was strict isolation). Guardrails in §6. |
| K12 | Experiments and milestones | **ContextManifest + use receipt in M0. Ablations on T1 in M2.** Plan in §8. |

---

## 3. The four information classes

| Class | Contents | Produced by | Consumed by | Retention / acceptance |
|---|---|---|---|---|
| **Authoritative state** | receipts, approvals, attempts, findings, plan revisions, decisions | the controller | everyone, as regenerated status | append-only event store; never edited (plan §6) |
| **Regenerated views** | dossier, code index (repo map, symbol ranges), pointer/staged digests, plan sidecar, canon excerpts | deterministic code from the tree + receipts | per-role assembly | not stored as knowledge; regenerated per call. **Fingerprint = exact input revisions (tree / candidate snapshot, receipts, findings and dispositions, adopted contract) + renderer configuration and version** [KA4] |
| **Ephemeral attempt continuity** | sealed at **every builder exit** (normal end, repair handoff, drain, interruption), reusing the checkpoint mechanism [KA6]: partial diff, failing tests, trail (incl. shell reads), untrusted builder notes | the attempt (notes) + controller (seal) | **builder continuations of the same project, task and contract revision only**; raw builder notes never reach reviewers [KA6] | bound to project, task, contract revision and attempt; labelled untrusted data; never auto-loaded elsewhere; retained under configurable post-closure TTLs with tombstones (active resumable checkpoints are kept); payload deletion is separate from immutable accounting; a fenced late attempt cannot add to it |
| **Supervisor continuity** | notes, open threads, handovers (`02-supervisor-role.md` §5) | the supervisor | the supervisor seat | untrusted operational continuity, scoped to its grant and projects, referencing authoritative records; never auto-loaded into cycle roles [KA6] |
| **Durable knowledge** | accepted decisions, conventions, verified lessons, scoped negative results (K3: after M2) | proposals from any role or code; acceptance by policy (K5) | role views by purpose | typed claims; per-purpose acceptance; validity by declared dependencies (K6); history never rewritten |

This also fixes a contradiction in the research proposal. Its builder row both included attempt records and excluded
unpromoted notes. Attempt notes are continuity, not knowledge: they never need promotion and are never shown outside
their task.

---

## 4. The design (ADC v1 profile of the shared spec)

### 4.1 Records ADC v1 adds

ADC v1 adds **at most four record types** to the planned event store (D-defaults: SQLite WAL + append-only log).
Everything else reuses existing receipts, approvals, attempts and plan revisions **by reference, never by copy**.

| Record | When | Contents |
|---|---|---|
| `ContextManifest` (with use-receipt fields) | **M0 contract**, written for every model call | consumer (role, executor, model snapshot, capability profile) · task/attempt · input revisions · policy + renderer versions · ordered included refs with their fingerprints · omitted refs with reasons (budget, policy, unavailable) · budget · rendered-content digest · destination · delivery outcome · size/latency. **Three coverage levels** [KA5]: *assembled* (what the controller built), *delivered* (initial prompt plus every controller-mediated tool result, per turn), *unobservable* (harness instructions, compaction, harness-internal tools), declared per adapter and per turn. Unknown coverage stays `unknown`. Cost is a **reference to the call ledger**, not a second accounting owner. |
| `AttemptRecord` | M1 | **the schema of the existing checkpoint** (§3), bound to project, task, contract revision, attempt fencing token and base tree [KA8] |
| `Claim` | **only after M2** (K3) | revision of a typed claim: kind · subjects · supporting/refuting evidence refs · applicability · declared dependencies + validity conditions · observed/recorded/valid times · producer (role, model snapshot, attempt) · **source restrictions inherited through lineage** [KA1] · purpose-bound decision ref |
| `FailureSignature` | M2 | **a derived, project-scoped aggregate**: normalized failure + count; no prose; crossing a threshold makes one deduplicated advisory proposal for a mechanism task (K4). No clustering, no lifecycle table [KA8] |

### 4.2 Claim kinds (one home each)

| Kind | Example | Sole home | Acceptance path |
|---|---|---|---|
| observation | "test X failed on tree T" | receipt (authoritative state) | automatic; it is evidence |
| external fact | "library L ≥2.0 drops API A" | `Claim` with a source pointer | evidence review |
| inference / lesson | "this approach fails because…" | `Claim` | evidence review; prompt injection only via E3 |
| decision / convention | ADR, naming rule | protected repo file (K8); the controller holds only the **adoption/approval record** of a revision, never the content [KA9] | the **protected-document change operation** (§4.6) |
| procedure / mechanism | test, validator, gate | code | normal candidate: merge queue + independent review |

Lower layers reference upper ones by id and never restate them. Competing interpretations are separate claims with
an explicit dispute. "One home" does not mean "one opinion".

### 4.3 Lifecycle dimensions (independent; none grants authority)

- **Decision:** `unassessed | accepted(purpose) | rejected | disputed`. Supersession is an explicit link.
- **Validity:** `current | stale | unknown | expired`, with reasons.
- **Retention:** `available | held | purged` with a tombstone.

Publication and materialization from the shared spec are not needed in v1: there is no index, and the consumer class
is a field.

### 4.4 Assembly

- A view is assembled server-side from an authorized manifest under the project's data policy, **before egress**,
  and re-checked on every fallback step (plan §2.2). This includes local→cloud fallback meeting restricted history.
- Small and large models differ in budget and tools (K9), never in what knowledge exists.
- Cuts are named in the manifest, never silent (nöbetçi's `nb_canon` rule).
- Every derived view carries its fingerprint. A mismatch means regenerate, never serve. Regeneration guarantees the
  view matches its inputs. It does **not** make an old receipt applicable: evidence applicability is checked
  separately against the consumer's tree and environment [KA4].
- At every delivery (initial, lookup, fallback) **both** are checked: the source restrictions inherited by each
  included ref, and the receiving project's policy and destination [KA1].

### 4.5 Security

- Injected knowledge is wrapped as **data, never instructions**.
- Builder-ingested repository content cannot promote itself.
- A model-produced factual claim needs **exact supporting evidence** to be accepted. A resolving citation is a
  mechanical prerequisite, **not proof of truth**: the reviewer checks that the evidence entails the claim.
- Host-side harnesses run with their native memory disabled (plan §2.4).
- Spec or fixture updates (K10) never automatically alter grants, accepted claims or policy [KA8].

### 4.6 Protected-document change operation [KA3]

Decisions and conventions live in protected files (K8). The accept stage refuses protected-path edits, so they change
only through one narrowly scoped operation under the authority layer (plan §2.11):
- An approval record binds the permitted paths, the old and new content digests, and the expected adopted revision.
- The change still goes through independent review and the merge queue.
- On merge, the controller records the new adopted revision.
- A convention can never approve its own adoption or widen authority. Anything touching data policy or authority
  stays on the human-reserved paths (D32).

---

## 5. Acceptance policy (K5)

| Kind | Default confirmation | Settable to |
|---|---|---|
| observation | none (recorded by the controller) | — |
| external fact, inference/lesson, scoped negative result | `independent_review` (a model family that did not produce it, plus the evidence) | `human`; InfraMedic's posture |
| decision / convention | the protected-document change operation (§4.6) | — |
| framework lesson (K11) | `independent_review` + framework-scope checks (§6). Acceptance does **not** authorise export; non-template text also needs a human export approval (§6) | `human` |
| anything that widens data policy or authority | not a knowledge act; existing human-reserved paths (D32) | — |

- Curation is asynchronous and never on the critical path.
- A missing **required** contract or decision blocks only the dependent work, through a hold that names its resume
  signal.
- The supervisor's J2/J3 jobs include the curation queue. Its age and depth are measured (curation overhead is part
  of K1).

---

## 6. Framework lessons across projects (K11) and their guardrails

**The operator's choice:** a shared collection of framework-level lessons (defect classes about the cycle itself)
visible to all projects in v1.

**Guardrails, so that D9 (data trust) and D18 (multi-project) still hold** (revised by [KA1]; no validator can prove
that paraphrased prose is free of project information, so enforcement rests on lineage and templates, not on text
inspection):
1. **Framework scope only.** A framework lesson is about auto-dev-cycle's behaviour, roles, gates or harnesses. It is
   never about a project's code or domain.
2. **Lineage-inherited restrictions.** Every framework lesson carries the source restrictions of every project it was
   derived from, recorded by the controller. An on-prem-derived lesson keeps on-prem restrictions wherever it goes.
3. **Two export paths:**
   - **Automatic:** only allowlisted framework telemetry (failure-signature counts, reason codes, stage timings),
     rendered through **fixed templates**.
   - **Prose:** any other project-derived text needs an **explicit, digest-bound human export approval**. Independent
     acceptance review does not authorise export.

   A mechanical check still rejects project paths, symbols, repository content and quoted prompts or tool output, as
   a first filter, not as proof.
4. **Opaque evidence pointers.** The producing project's evidence stays in that project. Pointers expose no metadata
   to other projects.
5. **Check at every delivery** (initial, lookup, fallback): source restrictions **and** the receiving project's policy
   and destination. Revocation between selection and delivery is honoured.
6. **Preferred destination is code** (K4). A framework lesson that can become a framework test or validator becomes
   one through the self-host path (M6, independent review before install). The shared collection holds only what
   cannot be enforced.
7. **Measurement:** framework lessons are an arm of E3. If they do not help, they stay stored but are not injected.

**Timing (resolved):**
- admission and exposure controls are proven with fixtures in M2, including on-prem→open leakage and revocation
  between selection and delivery;
- two-project integration is proven in M4;
- the collection is populated only after the K3 evidence. M6 need not be the first second project.

Sharing remains in v1.

---

## 7. What v1 does not build

- No memory service (Mem0/Zep/Letta/Cognee), no vector or graph DB, no embeddings.
- No self-editing agent memory, no resumed long sessions.
- No harness-native memory as a source of truth.
- None of InfraMedic's machinery:
  - placement channels and audiences;
  - index generations;
  - ingestion doors and redaction pipelines;
  - signed bundles, master or mesh.
- No universal hypothesis lifecycle, confidence score, contradiction engine or policy language.
- No durable `Claim` collection before M2 evidence (K3).

---

## 8. Experiments (K12)

**Run on T1 per capability class** (small local, cheap API, frontier). T1's 20–30 tasks **screen** effects. They do
not establish broad safety; that needs the release run and later data.

**Operational definition of K1, predeclared before any run** [KA7]:
- **Correct** means hidden acceptance tests pass **and** an independent defect adjudication (a model family that took
  no part in the run) finds no escaped defect. Acceptance by the cycle alone is not correctness.
- **Total cost** means the call ledger's cost, under predeclared rules per cost basis:
  - cash at its price;
  - subscription at its list-price equivalent, labelled;
  - local compute at a declared per-GPU-hour rate;
  - curator and reviewer time for curated layers.
- **Design:**
  - paired comparisons (same task × same model snapshot across arms);
  - ≥3 repeats per pair;
  - holdouts separated by incident;
  - continuity tested with matched interruptions;
  - E2 run on identical defective candidates.
- **Exposure, not causation.** Manifests show what was seen. Attribution comes from the ablation design.
- **Inconclusive means disabled.**

- **Guardrails:** escaped defects, recovery success, isolation failures.
- **Diagnostics:** first edit, re-reads (shell included), turns, context bytes.
- **Fair arms:** mandatory requirements, canon and safety constraints are identical across arms. "Off" means optional
  assistance off, never essential task information removed.

| # | Milestone | Arms | Question | Extra |
|---|---|---|---|---|
| E0 | M0–M1 | — | Is the ContextManifest complete and true? | Reconcile **complete sampled sessions** (incl. staged disclosure and fallback) against assembled / delivered / unobservable coverage; unknown stays unknown [KA5]. Verifies the metric itself (nöbetçi's `foreign_reads 0` claim was false) |
| E1 | M2 | off · dossier · + code index · + attempt continuity · + lookup tool | Which views help which capability class? | Exposure per item via manifests; attribution via the paired ablation |
| E1L | M2 | no lesson · **reviewed fixture lessons** (source-linked, experimental payloads, no production `Claim` store) | Does curated lesson text help at all? **This is the K3 gate** [KA2] | Admission (E4) and validity (E5) exercised on the same fixtures |
| E2 | M2 | reviewer fresh-only · pointer digest · staged disclosure | How much framing leaks; which finds more injected and critical defects? | Lineage tracking through summaries |
| E3 | M6, only if E1L is positive and K3 enables durable knowledge (live, production claims) | no lesson · source-linked lesson · mechanism (where equivalent) · framework lessons (K11) | Does curated text help, at what curation cost? | InfraMedic's GEPA discipline: holdout + human/independent gate + shadow before any text enters role instructions |
| E4 | M2 | seeded poisoning: injected instructions in repo files; a real-but-irrelevant citation; a planted framework lesson; **poisoned builder notes; a mixed-project supervisor handover** [KA6] | Does admission hold? Injection success per capability class | Includes the §6 checks with **on-prem→open leakage and revocation-between-selection-and-delivery fixtures** [KA1]. Delimiters alone are not enforcement |
| E5 | M2 | refactors under existing items; unchanged symbol with changed behaviour; expired observations; purged evidence; **changed findings at an unchanged tree; an approved amendment; a rebase** [KA4] | Does validity catch false-current items and views? | Measure false-current, not only mismatches detected |

**Rule:** a layer stays on for a role and capability class only if it improves K1 without raising harmful errors.
"Nothing enabled" is a valid outcome, as in the classifier program (D6).

---

## 9. Milestone placement (additions to plan §4)

| M | Knowledge deliverable |
|---|---|
| M0 | `ContextManifest` schema with coverage levels · exact ref/revision/fingerprint types · purpose-bound decision binding (the existing approval records) · scope and source-restriction labels on refs · the shared spec draft (K10): **schemas, invariants and fixture definitions only**; each fixture's checks are implemented in the milestone that owns the behaviour [KA8] |
| M1 | Regenerated task views with full input fingerprints · `AttemptRecord` as the checkpoint schema, sealed at every builder exit · resume from it · fenced late submissions rejected · protected-document change operation (§4.6) · **exit: E0 reconciliation passes** [KA7] |
| M2 | T1 ablations E1, **E1L**, E2, E4, E5 · `FailureSignature` · **exit: published ablation decisions, including K3** [KA7] |
| M4 | Two-project integration of the K11 exposure checks |
| M5 | Planner and supervisor views over decision records and conventions; curation queue in `adc` CLI + dashboard (parity) |
| M6 | Framework lessons become framework mechanisms through self-host (K11); live E3 only if E1L was positive |

---

## 10. Open questions

All resolved by D36, following Astra's recommendations:
1. **K11 timing:** see §6 "Timing". Fixtures in M2, two-project integration in M4, populated after K3 evidence.
2. **Spec home:** this repo (as K10 and D35 state). InfraMedic vendors a pinned revision. Reconsider a neutral
   repository only when maintenance warrants it.
3. **Retention:**
   - active resumable checkpoints are kept;
   - explicit, configurable post-closure TTLs with tombstones;
   - no evidence-based reason for a shorter on-prem default;
   - payload deletion is separate from immutable accounting and history.
4. **Capability profiles** are keyed by role × model snapshot × harness version. They start from effective context
   budget, tool and schema reliability, correctness, latency and cost (from M2 data). Authorisation is kept separate.
5. **InfraMedic follow-ups** (inputs for InfraMedic's own session; not v1 dependencies, and no InfraMedic changes are
   authorised here):
   - a knowledge-on/off ablation before further knowledge expansion;
   - a consumer manifest beside the retrieval audit;
   - derivation identity under content dedup;
   - a written design for the "fleet of small/medium local LLMs" vision.
