<!-- Astra (gpt-6-astra, Codex CLI 0.157.0, read-only, reasoning effort high), 2026-10-02. Synthesis brief: astra-synth-brief (inputs 01-claude-report.md and 02-astra-report.md). Output follows verbatim. -->

# Astra synthesis

**[I] Share a small semantic specification first. For auto-dev-cycle, implement revision-bound context and exposure records before building a curated-memory subsystem.** Claude strengthens the case for explicit knowledge ownership, hybrid storage, and evaluating changes to instructional text. I retain separate meanings for acceptance, publication, validity, and delivery; I reject a universal hypothesis lifecycle, a mechanism-only destination for lessons, and model size as an acceptance rule.

**Reading conventions:** **[F]** = source-backed fact, including what a report proposes. **[I]** = interpretation or recommendation. Implementation inspected; tests and deployed behavior were not independently exercised.

Paths below use:

- `IM/` = `/home/mega/inframedic/`
- `ADC/` = `/home/mega/auto-dev-cycle/`
- `NB/` = `/home/mega/nobetci-dev/`
- **C** = [Claude’s report](/home/mega/auto-dev-cycle/docs/research/2026-10-01-knowledge-abstraction/01-claude-report.md)
- **A** = [my independent report](/home/mega/auto-dev-cycle/docs/research/2026-10-01-knowledge-abstraction/02-astra-report.md)

## 1. Where the reports agree

The following are **[I] decisions ready to adopt**. The cited passages establish the consensus.

| Decision | Agreement |
|---|---|
| **Share contracts and conformance examples before runtime code.** Do not transplant InfraMedic’s subsystem into ADC. | C:44; A:23; A:454 |
| **Durable knowledge belongs to the platform; authorship remains attributable.** Executor replacement must preserve evidence and continuity without preserving an agent session. | C:172; C:247; A:150; A:542 |
| **Use exact content and source identities.** Preserve provenance, supersession, decisions, and what each consumer actually received. | C:245; C:258; A:232 |
| **Keep relevance, access, and destination permission separate.** Apply policy to assembled context, including derived material. | C:86; C:279; A:109 |
| **Writing, selecting, or retrieving text creates no execution permission.** Promotion follows platform policy, bound to exact content and state. | C:275; A:306 |
| **Start ADC with derived context and bounded continuity.** Reuse controller records; defer vectors, federation, and a separate knowledge service. | C:408; A:424 |
| **Measure optional knowledge assistance before enabling it broadly.** Include correctness and operational overhead; smaller prompts alone are insufficient. | C:428; A:442 |

**[I] One terminology decision should also be immediate:** use **view** for consumer context and **index** for retrieval materialization. Both reports identify the overloaded use of “projection.” (C:218; A:214)

## 2. Where they disagree

### 2.1 Acceptance is not a placement tier

**[F] Claude’s position:** `Placement.tier = proposed | accepted | retired`; promotion appends an accepted placement. **Mine:** epistemic acceptance and publication are independent dimensions. (C:250; C:263; A:253)

**[F] Evidence:** InfraMedic’s placements describe representation, channel, audience, expiry, and revocation. Separately, its standards layer can record that previously verified content is disputed without editing the underlying dictionary. (`IM/inframedic/registry/store.py:1536`; `IM/inframedic/standards/models.py:124`)

**[I] Verdict — hold.** A proposal may be visible to a curator before acceptance. An accepted claim may be unpublished. Published material may become disputed. Claude’s tier conflates these cases.

**[I] Settling fixture:** represent those three cases plus publication revocation without retracting the historical claim. Any schema that needs to invent unrelated “placements” to express epistemic decisions should be revised.

### 2.2 Fingerprints detect dependency change, not all invalidity

**[F] Claude’s position:** staleness is a locally computed fingerprint mismatch; accepted evidence must resolve now. **Mine:** materialization freshness, evidence availability, and claim validity require separate answers. (C:272; C:279; A:128; A:530)

**[F] Evidence:** InfraMedic has fingerprint-based index checks, but also time-based pattern staleness and human disputes against unchanged dictionary content. (`IM/inframedic/knowledge/catalog.py:1871`; `IM/inframedic/patterns/service.py:225`; `IM/inframedic/standards/models.py:124`)

**[I] Verdict — merge.** Adopt fingerprints for declared dependencies. Do not define validity exclusively through them. A matching fingerprint says the recorded inputs match; it cannot establish that the dependency list is complete or that an assertion is true.

Historical evidence may also remain important after its payload becomes unavailable. Record that loss explicitly; do not rewrite history or silently treat it as current evidence.

**[I] Settling experiment:** extend E5 with unchanged anchors whose behavior changes, expired observations, explicit counterevidence, and purged evidence. Measure false-current results as well as detected mismatches.

### 2.3 A universal citation gate is too broad—and too weak

**[F] Claude’s position:** every model-produced claim must cite mechanically resolvable evidence or be dropped. **Mine:** distinguish observations, inferences, normative decisions, and proposals; validation depends on the claim type and intended use. (C:273; A:241)

**[F] Evidence:** InfraMedic’s annotation gate checks that references identify tool-log entries returning content. It does not check whether the cited content entails the claim. Its gap proposals are parsed separately from annotations. (`IM/inframedic/investigate/annex.py:80`; `IM/inframedic/investigate/annex.py:100`; `IM/inframedic/investigate/annex.py:108`)

**[I] Verdict — merge.** Require exact supporting evidence for accepted factual assertions. Retain unsupported hypotheses and design proposals as explicitly unaccepted material. A normative decision can cite its authorized decision record; it need not masquerade as an empirical discovery.

Citation resolution is a useful mechanical prerequisite, not proof of truth.

**[I] Settling fixture:** submit a false assertion citing a real but irrelevant source, a clearly labeled hypothesis, and an authorized project convention. Verify that the system treats them differently.

### 2.4 Promotion policy cannot be reduced to “every kind human” or “never blocks work”

**[F] Claude’s position:** InfraMedic sets every kind to human confirmation; ADC defaults curated kinds to independent review; promotion never lies on the critical path. **Mine:** policy depends on the act and purpose, and some missing knowledge legitimately blocks dependent work. (C:337; A:325)

**[F] Evidence:** InfraMedic explicitly permits automatic placement of citation-gated, model-produced annexes while requiring `knowledge-promote` for other model-produced shared/default artifacts. ADC already distinguishes independently reviewed actions from human-reserved changes and binds approvals to expected state. (`IM/docs/notes/2026-08-28-knowledge-center-doctrine.md:320`; `ADC/docs/plan/01-first-release-plan.md:407`; `ADC/docs/plan/01-first-release-plan.md:420`)

**[I] Verdict — merge, with two corrections.**

- Human-gated knowledge pillars and the named promotion verb remain human-gated; that does not make every knowledge transition human.
- Optional lesson curation should be asynchronous. A missing mandatory requirement, unresolved controlling decision, or invalid required source must block the work that depends on it.

**[I] Settling fixture:** compare an unavailable optional lesson with an unavailable required contract. The former should permit a baseline run; the latter should produce a specific hold.

### 2.5 Lessons need not end exclusively as mechanisms

**[F] Claude’s position:** lessons become mechanisms or retire; prose injection waits for positive experimental evidence. Its broader conclusion says demonstrably useful knowledge is mechanical. **Mine:** mechanisms are preferred for enforceable recurring failures, but accepted explanations, decisions, and scoped negative results can remain useful knowledge. (C:206; C:416; A:438)

**[F] Evidence:** the supplied research already reports benefits from accurately selected summaries and curated context. I checked the primary sources: SWE-ContextBench reports benefits from accurately summarized and retrieved experience; ACON reports benefits from compressed observations and history. These are conditional results, not an ADC outcome measurement. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:43`; `ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:65`; [SWE-ContextBench](https://arxiv.org/abs/2602.08316); [ACON](https://arxiv.org/abs/2510.00615))

**[I] Verdict — hold on representation; merge on enablement.** Store justified knowledge even when it cannot sensibly become a test. Keep optional automatic injection experimental. “Why this interface exists” and “this approach failed under these conditions” need not become enforcement code.

**[I] Settling experiment:** compare no optional lesson, a source-linked lesson, and—where genuinely equivalent—a mechanism. Measure accepted correctness, escaped defects, total cost, and maintenance effort. ADC-specific benefit remains **not measured**.

### 2.6 Model size is a routing default, not an epistemic rule

**[F] Claude’s position:** small models propose; only code, a strong independent model, or a human accepts. Small models receive injected views; large models also receive retrieval tools. Producer acceptance rate is its initial trust signal. **Mine:** allocate work by measured capability, and keep authority in platform policy. (C:349; C:375; C:384; A:374; A:393)

**[F] Evidence:** InfraMedic’s citation gate evaluates references rather than model size. Its shadow evaluator compares candidate and current outputs against the same stored signals. ACON also explicitly distills compression into smaller models. (`IM/inframedic/investigate/annex.py:88`; `IM/inframedic/optimize/shadow.py:3`; [ACON](https://arxiv.org/abs/2510.00615))

**[I] Verdict — hold.** A strong independent reviewer is a reasonable initial allocation, not a truth guarantee or kernel requirement. Permit narrow extraction or retrieval by any evaluated model. Never let a model’s own output supply its authorization.

Acceptance rate measures agreement with the gate. It does not, by itself, measure correctness.

**[I] Settling experiment:** evaluate producer–reviewer pairs on supported, unsupported, stale, and poisoned claims. Measure false acceptance, false rejection, downstream correctness, and cost—not merely promotion rate.

### 2.7 Hypothesis accumulation belongs in an optional domain profile

**[F] Claude’s position:** the shared kernel includes `accumulating → possible → corroborated → mechanised/stale/disproved`, and ADC lessons enter that lifecycle. **Mine:** the kernel needs typed claims, evidence, decisions, and invalidation; a universal hypothesis workflow is premature. (C:265; C:417; A:251)

**[F] Evidence:** InfraMedic itself implements `accumulating` as a derived view over existing fleet issues, not a persisted `PossiblePattern` state. Persisted states are `possible`, `corroborated`, `stale`, and `disproved`. (`IM/inframedic/patterns/service.py:254`; `IM/inframedic/patterns/store.py:28`)

**[I] Verdict — hold.** Keep recurrence evidence and negative outcomes. Add a hypothesis profile when repeated-failure analysis requires it. A project convention, external fact, or single decisive counterexample does not naturally pass through this sequence.

**[I] Settling fixture:** model a recurring failure cluster, an ADR, and a one-run failed experiment using ordinary typed records. Introduce dedicated states only where they produce necessary behavior that the simpler representation cannot express clearly.

### 2.8 Claude’s hybrid-storage proposal improves my implementation recommendation

**[F] Claude’s position:** catalog and operational records in controller SQLite; portable curated payloads in protected repository files. My report recommended the existing controller substrate without choosing a concrete portable-payload default. (C:420; A:428)

**[F] Evidence:** InfraMedic distinguishes payload ownership—repository, package, generated, database, local—from catalog identity. ADC already plans SQLite WAL and an append-only event log. (`IM/inframedic/knowledge/catalog.py:74`; `ADC/docs/plan/00-brainstorm-decisions.md:81`)

**[I] Verdict — concede the useful specificity; adopt a constrained hybrid.** Project decisions and conventions can live in the repository. Attempt continuity, exposure receipts, and private operational material belong to controller storage. Neither becomes an independently editable duplicate of the other.

**[I] Settling fixture:** clone a project, restart the controller, and interrupt a knowledge-file update. The adopted revision must remain unambiguous; files arriving in a clone must not inherit another controller’s grants.

## 3. What each report contributed—and factual corrections

### What Claude found that I missed or underdeveloped

1. **An operational “one home per claim type” rule.**  
   **[F]** I distinguished epistemic kinds but did not make ownership enforcement equally concrete. InfraMedic’s canon assigns semantic meaning, firing rules, judgment, and rationale to different homes; tests ban specific duplicated enum semantics in grounded detectors. (A:241; C:67; `IM/docs/notes/2026-08-12-canonical-standards-and-knowledge-library.md:196`; `IM/tests/test_standards_enforcement.py:1`)  
   **[I]** Adopt one authoritative owner for each accepted assertion or rule. Permit competing interpretations as separate claims with explicit disputes. “One home” must not mean “only one opinion may be stored.”

2. **A stronger evaluation discipline for changes to instructional text.**  
   **[F]** Claude explicitly transfers holdout/shadow evaluation to ADC playbook changes. InfraMedic’s shadow implementation compares current and candidate packs on previously collected signals. (C:428; `IM/inframedic/optimize/shadow.py:1`)  
   **[I]** Adopt this for behavioral instructions and playbooks. Ordinary factual updates should not automatically trigger a full optimization pipeline.

3. **A concrete portability boundary for storage.**  
   **[I]** The constrained hybrid above is clearer than my original substrate-only recommendation.

4. **The visibility value of accumulated weak evidence.**  
   **[F]** Both reports covered negative knowledge, but Claude made the accumulation workflow more explicit. (C:110; A:128)  
   **[I]** Preserve that as an optional recurring-failure feature, without making it the lifecycle of every knowledge item.

### What I found that Claude missed or underdeveloped

1. **Content deduplication can collapse distinct production events.**  
   **[F]** `add_representation` returns an existing representation for matching version, relation, and content hash, without including producer or transformation identity in that lookup. (`IM/inframedic/knowledge/catalog.py:718`)  
   **[I]** Preserve derivation events separately from deduplicated bytes. This is a contract weakness to test, not a reproduced incident.

2. **Deterministic summaries can carry model judgments.**  
   **[F]** The predecessor’s digest includes passed/open claim groupings, finding claims, statuses, and decisions. (`NB/bin/nb_phase_digest.py:128`)  
   **[I]** Reviewer independence requires exposure lineage. Calling something a controller-generated “pointer digest” does not make it neutral.

3. **ADC’s revision and concurrency requirements are central to knowledge correctness.**  
   **[F]** The plan fences late executor results and binds evidence to the final prospective merge tree. (`ADC/docs/plan/01-first-release-plan.md:275`)  
   **[I]** Knowledge submissions need those same bindings; otherwise an obsolete executor can publish current-looking advice.

4. **Continuity, validity, and retention need explicit exceptions to blanket rules.**  
   **[F]** The predecessor deliberately carries unaccepted builder notes across interrupted attempts; InfraMedic deliberately purges eligible payloads while preserving tombstones. (`NB/bin/nb_build_notes.py:12`; `IM/inframedic/knowledge/retention.py:91`; `IM/inframedic/knowledge/retention.py:151`)  
   **[I]** Neither “only promoted material is consumable” nor “never delete” is a sufficient contract.

### Checks of Claude’s factual claims

| Claim | Check and correction |
|---|---|
| Index generations, installation identity, and principals were not built. | **[F] Wrong in the original report; corrected by its erratum.** The code/build records support the correction. Declassification remains unbuilt. (C:123; C:480; `IM/inframedic/knowledge/generation.py:62`; `IM/docs/notes/2026-09-19-1.4-9-installation-identity.md:20`; `IM/docs/notes/2026-09-19-1.4-10-seam-activation.md:26`; `IM/inframedic/auth/gates.py:243`) |
| Placements are append-only; retirement rewrites nothing. | **[F] Too broad as an implementation claim.** Promotion adds placements, but revocation updates existing placement rows. Preserve the historical-content principle without claiming the whole catalog is immutable. (C:77; `IM/inframedic/knowledge/catalog.py:474`) |
| Injection reports included/summarized/omitted. | **[F] The inspected path does not summarize.** It injects whole documents or reports omission/refusal. (C:100; `IM/inframedic/sandbox/context.py:14`) |
| `accumulating` is part of the persisted pattern lifecycle. | **[F] Conceptually present, physically derived.** It is computed from existing fleet issues; the persisted pattern enum excludes it. (C:113; `IM/inframedic/patterns/service.py:254`; `IM/inframedic/patterns/store.py:28`) |
| Nothing is importable without InfraMedic’s registry. | **[F] Literally false.** The subject parser explicitly has no registry lookups; standards distillation is stdlib-only. The integrated subsystem nevertheless has domain coupling. (C:140; `IM/inframedic/knowledge/subjects.py:17`; `IM/inframedic/standards/distill.py:6`) |
| InfraMedic’s distillation pattern demonstrates an implemented LLM drafter. | **[F] Needs qualification.** The inspected tool mechanically extracts structured enums. Its docstring describes an LLM drafting path for prose, but this tool does not implement that path. (C:356; `IM/inframedic/standards/distill.py:6`; `IM/inframedic/standards/distill.py:37`) |

**[F] The dossier numbers are not a substantive disagreement.** Claude reports the supplied earlier sample; my report identifies a later snapshot and an explicit classification rule, yielding 421 seconds across 24 post-install repair calls instead of 398 across 23. Both reports acknowledge confounding. (C:190; A:177; A:185)

**[I] Neither sample establishes benefit or harm caused by the dossier.** Controlled ADC productivity gains and local-model-fleet gains remain **not measured** in the inspected evidence. Likewise, Claude’s erratum should change “measure before 1.3-8/1.3-9” into “measure before further expansion”; those stages already exist.

## 4. My merged kernel

**[I] The smallest version I defend has seven logical record shapes. They are contracts, not seven required tables.** Typed claims live inside revision payloads; representations are derived revisions. This removes a separate `KnowledgeItem` wrapper from my original proposal and a mandatory representation table from Claude’s.

### Entities and records

| Shape | Minimum contract |
|---|---|
| **Artifact** | Namespaced identity, kind/schema family, origin, authoritative owner. |
| **Revision** | Exact content digest, schema, payload reference, source-state reference, recording time, optional supersession links. Claim payloads carry epistemic kind, subjects, supporting/refuting evidence, and validity conditions. |
| **Derivation** | Exact input revisions, output revision, producer/run identity, transformation/configuration identity, model snapshot when available. Separate occurrences survive content deduplication. |
| **Decision** | Exact target, purpose, outcome, actor/delegation, policy revision, supporting evidence, expected prior state, time. |
| **Publication** | Exact revision, permitted audience/consumer class, information scope, destination constraints, decision reference, expiry/revocation. |
| **ViewManifest** | Consumer/task/attempt, source state, policy and renderer versions, ordered included references, relevant omissions and reasons, budget, rendered-content digest. |
| **ExposureReceipt** | Manifest or retrieval reference, actual delivered revisions/chunks, delivery outcome, destination, correlation, available latency/size/cost measurements. |

**[I] A representation’s “represents” relationship must remain distinct from “supersedes.”** A sanitized rendering does not replace the source assertion. An assembled dossier derives from multiple records; its manifest preserves those inputs.

**[I] Evidence references should be attachable to individual claims within a payload.** Do not require one database row for every sentence.

### Lifecycle dimensions

| Dimension | Minimal meaning |
|---|---|
| **Epistemic decision** | Unassessed, accepted for a purpose, rejected, or disputed. Supersession is an explicit relationship. |
| **Validity** | Current, stale, unknown, or expired, with reasons and conditions. |
| **Publication** | No publication, active publication, or revoked/expired publication. |
| **Materialization** | Absent, ready, behind, or failed; optional until caching/indexing exists. |
| **Retention** | Payload available, held, or purged with an honest tombstone. |

**[I] Keep observation time, recording time, and validity interval distinct where the domain needs them.** None of these states independently grants execution authority.

### Invariants

All are **[I] proposed shared requirements**:

1. **Exact references never silently resolve to different content.**
2. **Decisions bind content, purpose, policy, and expected state.** Obsolete attempts cannot mutate current accepted state.
3. **Acceptance, publication, and execution authorization are separate.**
4. **Evidence, interpretation, and normative decisions remain distinguishable.** A resolving citation is not proof of entailment.
5. **One authoritative home per accepted assertion or rule.** Derived views reference it; disagreements remain explicit.
6. **Derivation preserves lineage and conservative information scope.** Summarization does not declassify.
7. **Every exposure rechecks access and destination policy.** Applicability alone grants nothing.
8. **Record actual delivery and relevant omissions.** Exposure is not proof that a model attended to or used the material.
9. **Unavailable, unauthorized, empty, stale, and budget-omitted are distinct outcomes.**
10. **Preserve decision history without requiring permanent payload retention.** Foreign acceptance does not silently become local acceptance.

### Interfaces

**[I] These are conceptual operations; a native implementation may combine them transactionally.**

```text
record(artifact, payload, source_state, derivation?, attempt?) -> Revision
decide(target, purpose, evidence, actor, policy, expected_state) -> Decision
publish(revision, audience, information_scope, destination_policy, decision) -> Publication
assess(revision, consumer_context) -> ValidityResult + reasons
assemble(request, source_state, consumer_profile, budget) -> ViewManifest
expose(manifest_or_ref, principal, destination) -> Content | Refusal, ExposureReceipt
retire(target, retirement_kind, reason, expected_state) -> Event
```

Domain adapters supply source resolution, claim validation, freshness checks, authorization, and rendering. Search is an optional implementation behind selection/resolution.

**[I] Explicit cuts for complexity:**

- **Cut:** mandatory hypothesis lifecycle, universal confidence score, and generic contradiction engine.
- **Cut:** separate representation storage when derivation records suffice.
- **Cut:** mandatory index generations, vector/graph stores, ingestion doors, mesh, and bundle operations.
- **Cut:** a policy language and generic workflow engine.
- **Cut:** a five-table or one-page acceptance criterion. Bound behavior and implementation burden, not document length.
- **Cut:** universal mechanical-versus-human knowledge ranking. Domain policy must identify what each source can establish.

## 5. Recommendations and sequencing

### (a) InfraMedic’s general framework

**[I] First, document the extension boundaries against existing behavior.** Map domain subjects and claim schemas separately from artifact governance and platform policy. Keep InfraMedic’s human-gated pillars as its policy, including its explicit bounded automatic paths.

**[I] Second, validate the proposed abstraction using ADC-shaped fixtures before refactoring.** Prioritize:

- identical bytes from different derivations;
- a readable but unaccepted proposal;
- an unchanged artifact becoming disputed;
- a still-resolving source anchor whose applicability changed;
- access revoked between selection and delivery;
- a stale executor submitting a late claim.

**[I] Third, measure consumer value and maintenance cost.** Run controlled comparisons for optional retrieval/context assistance and record correctness, latency, model cost, human curation time, and backlog growth. Preserve essential authorization and provenance in every arm.

**[I] Fourth, extract only demonstrated seams.** A catalog adapter, provenance validator, or fixture runner may become reusable. Package-wide generalization should follow evidence of duplication, not precede the second implementation.

### (b) auto-dev-cycle v1

**[F]** The approved sequence already places contracts in M0, recovery in M1, and the T1 baseline in M2. (`ADC/docs/plan/01-first-release-plan.md:533`)

**[I] Extend that sequence narrowly:**

| Stage | Knowledge deliverable |
|---|---|
| **M0** | Exact revision/evidence references, purpose-bound decisions, scope labels, and a manifest/receipt contract. Define ownership for each record type. |
| **M1** | Regenerated task context; sealed attempt continuity; restart recovery; fencing of late knowledge submissions. Keep attempt notes explicitly unaccepted and scoped to continuation. |
| **M2** | Baseline and controlled ablations for dossier, code index, continuity, retrieval, and optional curated knowledge. |
| **After demonstrated benefit** | A small accepted collection, initially project decisions/conventions and carefully scoped lessons or negative results. |
| **Later, on demand** | Recurrence clustering, richer retrieval, cross-project transfer, or extraction of shared implementation. |

**[I] Use constrained hybrid storage.** Repository-owned decisions and conventions should have one protected, reviewable source. Controller-owned receipts and continuity remain in controller storage. Record adopted repository revisions rather than maintaining a second editable copy.

**[I] Reuse existing approval and candidate paths.** A change to an authoritative project rule follows the project’s normal reviewed change path. Advisory acceptance uses a purpose-specific decision. Do not route every observation through the merge queue.

**[I] Start reviewers with independent evidence.** Test a two-stage review: record an initial assessment, then expose prior findings for reconciliation. Track builder-derived lineage through summaries.

**[I] Primary evaluation measures:** accepted correct tasks per time/cost, escaped defects, recovery success, and curation overhead. Use first-edit latency, rereads, and context bytes diagnostically. Keep mandatory requirements and authority constraints identical between experimental arms.

### (c) Shared spec/component

**[I] Now:** write one versioned semantic spec plus small fixtures from both domains. Select one authoritative home; vendored copies should identify the exact source revision.

**[I] Next:** implement native mappings in InfraMedic and ADC. Require the same fixtures to preserve the same meanings without requiring matching schemas or workflows.

**[I] Then:** compare duplicated implementation and maintenance effort. Extract schema validation or fixture tooling if useful.

**[I] Only after a concrete exchange use case:** specify transport envelopes, compatibility, signatures, and local re-gating. Do not make protocol design a prerequisite for ADC v1.

## 6. Recommended ADC brainstorm agenda

All defaults below are **[I] recommendations**, ordered by how strongly they constrain the design.

| Order | Question | Options | Recommended default |
|---|---|---|---|
| **1** | **What must survive an executor replacement?** | Current controller state only; state plus attempt continuity; an immediately comprehensive project-memory store. | **State plus bounded continuity**, with separate accepted project knowledge. Distinguish these classes before choosing storage. |
| **2** | **What does acceptance permit, and who decides?** | Human for everything; universal independent-model review; policy by type and purpose. | **Policy by type and purpose.** Mechanical evidence is recorded automatically; advisory claims receive evidence review; authority changes retain existing approval rules. |
| **3** | **What makes an item valid for this task and tree?** | Resolvable anchor; whole-tree equality; declared dependencies plus domain checks. | **Declared dependencies plus checks**, with conservative revision bounds and an explicit unknown state. Anchor resolution proves location only. |
| **4** | **What may each consumer see?** | Shared project context; fixed role/model-size templates; role and measured-capability views with lineage. | **Role and capability views.** Preserve reviewer independence, recheck destination policy, and give continuation notes only where their provenance and scope permit. |
| **5** | **Where is each authoritative source kept?** | Repository only; controller database only; hybrid. | **Constrained hybrid.** Portable project rules in protected repository files; operational records in controller storage; one owner per assertion. |
| **6** | **What happens to a recurring lesson?** | Inject prose; require a mechanism; retain typed evidence and choose a destination. | **Typed evidence, then choose.** Prefer a mechanism when enforceable; otherwise retain a scoped explanation, decision, or negative result. Add hypothesis workflow only when needed. |
| **7** | **What evidence enables a context layer?** | Prompt-size/read-count improvement; whole-run outcome comparison; operator judgment alone. | **Controlled outcome comparison by role and capability**, with exposure receipts and total cost. Optional layers may remain off; provenance and authorization stay. |
| **8** | **What do the two projects share now?** | Runtime library; complete ontology/protocol; vocabulary, invariants, and fixtures. | **Vocabulary, invariants, and fixtures.** Revisit runtime extraction and exchange only after native implementations expose a concrete need. |