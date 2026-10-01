<!-- Astra (gpt-6-astra, Codex CLI 0.157.0, read-only, reasoning effort high), 2026-10-01. Brief: scratchpad astra-brief.md (intention, sources, required sections). Output follows verbatim. -->

# Independent report: what is abstractable between the two knowledge layers

**[I] My recommendation is to share a vocabulary, a small set of invariants, and executable contract examples first. Do not extract InfraMedic’s knowledge subsystem into a common library yet.** InfraMedic provides useful implementation evidence for provenance, authorization, publication, retrieval auditing, and retirement. auto-dev-cycle provides a different test of those concepts: disposable executors, concurrent code revisions, independent review, and unattended operation.

**[I] The shared object is a governed knowledge record—not an agent’s memory and not necessarily a document.** Its essential questions are: what is being asserted, from which evidence, about which scope and revision, under which decision, and what exactly did a consumer receive?

## Reading conventions and limits

**[F]** identifies a source-backed fact, including facts about what a document proposes. **[I]** identifies my interpretation or recommendation. “Built” below means present in the inspected implementation; it does not mean I independently validated the deployed service. I did not run tests or modify files.

Paths use these aliases:

| Alias | Absolute root |
|---|---|
| `IM/` | `/home/mega/inframedic/` |
| `ADC/` | `/home/mega/auto-dev-cycle/` |
| `NB/` | `/home/mega/nobetci-dev/` |
| `RT/` | `/home/mega/nobetci/` |
| `MEM/` | `/home/mega/.claude/projects/-home-mega-inframedic/memory/` |

The inspected repository heads were InfraMedic `555f2e8143d94101cac775ae636d913c73936763` and auto-dev-cycle `351851fbea963677db864b094178f5a1f15aae1d`. `MASTER/README.md` below means the unmerged master-platform contract at commit `cb4eec05f0cad605cc1b3780417fca279829e5d4`. Runtime measurements are a separate, later snapshot.

## 1. Intention restated and the meaning of “abstractable”

**[F]** InfraMedic’s stated ambition is broader than infrastructure diagnosis: it describes itself as a domain instance of a general hierarchical knowledge-cumulating framework, with human-gated foundations and separate knowledge and action evolution. auto-dev-cycle’s brief requires project-agnostic knowledge that survives dynamically replaced executors and serves different roles and model capacities. (`IM/docs/plans/2026-08-26-vision-roadmap.md:60`; `IM/docs/plans/2026-08-26-vision-roadmap.md:73`; `ADC/docs/handoff/04-knowledge-layer-brainstorm.md:39`)

**[I]** I understand the operator’s intention as two related questions:

1. Which lessons from InfraMedic should shape auto-dev-cycle’s knowledge design now?
2. Which concepts remain valid when both infrastructure-specific and development-specific meanings are removed?

**[I]** An abstraction qualifies only if both platforms can instantiate it without pretending their policies or workflows are identical. I distinguish four possible outputs:

| Interpretation | My assessment |
|---|---|
| Shared vocabulary and invariants | Adopt now. |
| Shared record and interface specification | Develop against examples from both platforms. |
| Shared runtime library | Defer until two implementations reveal genuine duplication. |
| Exchange protocol between installations or projects | Define compatibility principles now; implement only for an actual exchange use case. |

**[I]** This is a semantic kernel with policy adapters. It should standardize evidence identity, derivation, decisions, validity, publication, and consumer exposure. It should not standardize every domain’s ontology, human approval process, retrieval backend, or model gateway.

**[I]** The important distinction is between **accumulating information** and **accumulating justified, usable knowledge**. A catalog can tell us which version was retrieved. It cannot, by itself, establish whether a claim in that version is true, still applicable, or sufficient to authorize an action.

## 2. InfraMedic’s knowledge layer: built, designed, and claimed

### 2.1 Two ontologies coexist

**[F]** The Knowledge Center’s catalog ontology is:

`artifact → version → representation → placement → projection`

Its doctrine separates identity, immutable content, derived forms, publication location, and retrieval infrastructure. The schema implements these as distinct records, with payload storage and session selections alongside them. (`IM/docs/notes/2026-08-28-knowledge-center-doctrine.md:18`; `IM/inframedic/registry/store.py:1489`)

**[F]** The implemented catalog currently recognizes six artifact kinds: `canon`, `standard`, `apidoc`, `annex`, `session`, and `upload`. It recognizes five representation relations, four placement channels, and six projection states. These are closed code vocabularies, not an arbitrary domain registration mechanism. (`IM/inframedic/knowledge/catalog.py:57`; `IM/inframedic/knowledge/catalog.py:296`)

**[F]** Separately, the domain ontology recognizes subjects of kind `device`, `entity`, and `relation`; the implemented entity kinds are `cluster` and `datastore`. Facts and entities are extracted through infrastructure-specific code. (`IM/inframedic/knowledge/subjects.py:24`; `IM/inframedic/knowledge/entities.py:1`; `IM/inframedic/knowledge/facts.py:93`)

**[I]** This distinction matters to the framework ambition. InfraMedic already implements a reusable-looking **governance ontology for information artifacts**. It does not yet implement a generally extensible **ontology extraction framework for arbitrary domains**. Extracting its catalog would accomplish the former, not automatically the latter.

### 2.2 What the code supports

| Area | Status and evidence |
|---|---|
| Catalog identity and versions | **[F] Built.** Artifact identity is based on kind and origin; content hashes identify versions within an artifact. Registration reuses matching content rather than manufacturing a new content version. (`IM/inframedic/knowledge/catalog.py:276`; `IM/inframedic/registry/store.py:1500`) |
| Representations and provenance | **[F] Built.** Derived representations retain parent references, provenance, payload ownership, and content hashes. Sanitized kinds publish through a sanitized representation. (`IM/inframedic/knowledge/catalog.py:708`; `IM/inframedic/knowledge/catalog.py:80`) |
| Publication and index state | **[F] Built.** Placements and projections are separate. A projection may be `behind` and still retrievable; replacement and revocation are managed explicitly. (`IM/inframedic/knowledge/catalog.py:59`; `IM/inframedic/knowledge/catalog.py:776`) |
| Authorization | **[F] Built.** Authorization considers live placements, audience coverage, subject coverage, sensitivity, destination locality, and consent. Applicability affects selection without becoming a grant. (`IM/inframedic/knowledge/authz.py:256`) |
| Retrieval auditing | **[F] Built.** Audit records include principal, delegation, session/model correlation, generation, query digest, exact returned identities, latency, and outcome flags. (`IM/inframedic/knowledge/retrieval_audit.py:124`) |
| Session context | **[F] Built.** References are resolved under current authorization each turn; documents are included whole within a budget or explicitly omitted. This path does not summarize them. (`IM/inframedic/sandbox/context.py:1`) |
| Generation lifecycle | **[F] Built.** Index generations have eight states; activation and retirement transitions are reserved for human verbs. (`IM/inframedic/knowledge/generation.py:62`) |
| Retention | **[F] Built.** Eligible private session payloads can be purged with tombstones. Ever-shared, governance, or retired artifacts are excluded from automatic purging. (`IM/inframedic/knowledge/retention.py:91`; `IM/inframedic/knowledge/retention.py:151`) |
| Evolution records | **[F] Built.** A separate append-only event mechanism records proposals, gates, actors, correlations, channels, and loop outcomes. (`IM/inframedic/registry/evolution.py:1`; `IM/inframedic/registry/evolution.py:45`) |
| General artifact vocabulary | **[F] Broader doctrine than implementation.** The doctrine includes additional future library objects, while current catalog kinds remain the six above. (`IM/docs/notes/2026-08-28-knowledge-center-doctrine.md:309`; `IM/inframedic/knowledge/catalog.py:57`) |
| Declassification | **[F] Not built.** The gate registry explicitly says the act was not implemented; the existing seam does not lower a sensitivity floor. (`IM/inframedic/auth/gates.py:243`) |
| General cross-version exchange | **[F] Designed, not established by the current bundle implementation.** The roadmap requires producer/consumer compatibility envelopes; current bundle code implements signed pack bundles with a narrower manifest. (`IM/docs/plans/2026-08-26-vision-roadmap.md:200`; `IM/inframedic/bundle/bundle.py:87`) |
| Master and mesh | **[F] Future designs.** The master contract is a design record; mesh appears as a later direction. (`MASTER/README.md:3`; `IM/docs/plans/2026-08-26-vision-roadmap.md:648`; `MEM/archive/future-directions-topology-tenancy-mesh.md:55`) |

### 2.3 Write paths and gates

**[F]** InfraMedic does not have one undifferentiated “write knowledge” operation. Standards distillation currently converts archived structured material into drafts without supplying verification or severity. Standards refresh detects source change without rewriting accepted content. Model-produced annotations are constrained by citation and authority rules. Foundational detectors, packs, and gold have distinct promotion paths. (`IM/inframedic/standards/distill.py:1`; `IM/inframedic/standards/distill.py:37`; `IM/inframedic/standards/refresh.py:1`; `IM/docs/notes/2026-08-10-diagnosis-layers-gates-and-ground-truth.md:60`; `IM/docs/notes/2026-08-10-diagnosis-layers-gates-and-ground-truth.md:127`)

**[F]** “Human-gated” does not mean every observation waits for manual approval. The doctrine permits a bounded automatic annotation placement path, identity rules can operate under explicit operator opt-in, and chronic-state transitions include deterministic thresholds. (`IM/docs/notes/2026-08-28-knowledge-center-doctrine.md:320`; `IM/inframedic/knowledge/identity.py:1`; `IM/inframedic/chronic/promotion.py:60`)

**[I]** The reusable distinction is:

- Recording evidence may be automatic.
- Producing an interpretation may be automatic.
- Accepting an interpretation requires the appropriate policy.
- Publishing it to an audience requires an authorization decision.
- Using it to authorize an action requires a separate authority path.

**[I]** A common `promoted=true` field would erase these distinctions. “Published,” “accepted as knowledge,” “verified against evidence,” and “authorized for execution” must not become synonyms.

### 2.4 Versioning and provenance: valuable, but not universally immutable

**[F]** Content versions and derived representations are preserved, while placements, projection state, and current pointers change. Matching previously seen content can reuse a version. The implementation therefore is not an immutable database in the blanket sense. (`IM/inframedic/knowledge/catalog.py:324`; `IM/inframedic/knowledge/catalog.py:708`; `IM/inframedic/knowledge/catalog.py:776`)

**[F]** Representation deduplication currently uses version, relation, and output content hash. It does not include the derivation producer in that lookup. (`IM/inframedic/knowledge/catalog.py:718`)

**[I]** Before generalizing this interface, distinguish **content identity** from **production-event identity**. Two extractors can produce identical bytes under different policies or model snapshots. Deduplicating the bytes is reasonable; losing the second derivation event would weaken provenance. This is a framework design concern, not a reproduced runtime defect.

**[F]** The source hierarchy itself contains drift: an older durable-memory passage describes subjects as device sets, while the current subject implementation includes devices, entities, and relations. (`MEM/durable-principles.md:223`; `IM/inframedic/knowledge/subjects.py:24`)

**[I]** This is a concrete reason to preserve source precedence and supersession. A remembered architectural summary cannot outrank the current ratified contract and implementation merely because it is labeled “durable.”

### 2.5 Scoping, authorization, and retrieval

**[F]** The doctrine distinguishes:

- `audience`: who may access;
- `data_scope`: whose information is present;
- `applies_to`: where the content is relevant;
- sensitivity and egress constraints: where it may be sent.

The code requires coverage of all subjects in data scope and checks live sensitivity before cloud use. (`IM/docs/notes/2026-08-28-knowledge-center-doctrine.md:150`; `IM/docs/notes/2026-08-28-knowledge-center-doctrine.md:210`; `IM/inframedic/knowledge/authz.py:298`)

**[F]** Canon scoping uses registered identity facts and lexical matches, captured as a version-associated scope snapshot. A subsequently registered device does not automatically rescope old content. (`IM/inframedic/knowledge/scoping.py:4`; `IM/inframedic/knowledge/scoping.py:28`)

**[I]** The separation of scope dimensions is highly abstractable. The identity-scanning algorithm is not. For a general framework, declared source lineage should establish the conservative information scope; text scanning can supplement that lineage but cannot prove that all sensitive subjects were found.

**[F]** The broker constructs an authorized selection, resolves ownership and generation, uses bounded retrieval batches, and audits outcomes. The doctrine also requires authorization on dereference rather than treating an earlier selection as permanent permission. (`IM/inframedic/knowledge/broker.py:560`; `IM/inframedic/knowledge/broker.py:656`; `IM/docs/notes/2026-08-28-knowledge-center-doctrine.md:178`)

**[I]** ADC should reuse that invariant, especially when capacity fallback changes the destination from a local model to a remote provider. Authorization must cover the complete assembled context, including historical notes and derived summaries.

### 2.6 Staleness, negative knowledge, and evolution

**[F]** InfraMedic distinguishes source, sanitized-content, redactor, chunker, and embedder changes in projection freshness. Standards disputes are explicit rather than silent rewrites. Pattern rejection, negative correlation knowledge, recurrence, and detector changes have separate semantics. (`IM/inframedic/knowledge/catalog.py:1871`; `IM/inframedic/standards/models.py:124`; `IM/inframedic/patterns/store.py:1`; `IM/inframedic/patterns/registry.py:114`)

**[F]** Chronic auto-resolution requires a checkable detector; accepted-risk handling is not simply overwritten by that automation. (`IM/inframedic/chronic/promotion.py:60`; `IM/inframedic/chronic/promotion.py:128`)

**[I]** These implementations support a general principle: **absence, disagreement, invalidation, and expiry are different events**. “Unused for thirty days” does not mean false. “No retrieval result” does not mean no relevant knowledge. “Not reproduced on this revision” does not prove a historical observation never occurred.

**[F]** Evolution-event storage is implemented, but the retrieval-audit build record explicitly leaves usage-driven candidate proposals to later work. It should not be read as evidence that retrieval logs already drive autonomous knowledge improvement. (`IM/inframedic/registry/evolution.py:45`; `IM/docs/notes/2026-09-04-1.3-7-retrieval-audit.md:75`)

### 2.7 Trust and cross-installation knowledge

**[F]** InfraMedic’s trust rules use mechanical measurements rather than an LLM judge. Model-change doctrine identifies profiles by snapshots and distinguishes model coupling such as serving, tuning, and embedding. A stronger model must not widen authority. (`IM/inframedic/trust/rules.py:1`; `IM/inframedic/trust/rules.py:42`; `IM/docs/notes/2026-08-28-model-change-resilience-doctrine.md:40`; `IM/docs/plans/2026-08-26-vision-roadmap.md:123`)

**[F]** Installation identity and the uplink-enable gate have implementation records; those records explicitly exclude sender implementation. The master design makes the local manifest authoritative for admitted software, treats vendor records as support/distribution records, and requires local re-gating of incoming artifacts. (`IM/docs/notes/2026-09-19-1.4-9-installation-identity.md:17`; `MASTER/README.md:274`)

**[F]** The planned export excludes raw prompts, responses, transcripts, device identifiers, and restricted projections. Returned recommendations are signed artifacts, not remote settings changes. Wire format and transport remain open design work. (`MASTER/README.md:254`; `MASTER/README.md:274`; `MASTER/README.md:433`; `MASTER/README.md:454`)

**[I]** What transfers is the distinction between **foreign evidence** and **local acceptance**. A signature authenticates origin and integrity; it does not establish local applicability or truth. Mesh replication, reputation, and conflict resolution should remain outside the initial shared kernel.

## 3. auto-dev-cycle’s needs, proposal, and predecessor evidence

### 3.1 The knowledge problem is primarily continuity under replacement

**[F]** ADC plans controller ownership of repositories, sandboxes, gates, and project knowledge while capacity sources supply interchangeable executors. Its approved plan includes exact approval bindings, attempt fencing, sealed handover evidence, and evidence tied to the final prospective merge tree. (`ADC/docs/plan/00-brainstorm-decisions.md:26`; `ADC/docs/plan/01-first-release-plan.md:267`; `ADC/docs/plan/01-first-release-plan.md:275`; `ADC/docs/plan/01-first-release-plan.md:420`)

**[I]** ADC therefore needs at least four classes of information:

1. **Authoritative state:** task, attempt, tree, approvals, findings, and receipts.
2. **Regenerated structural context:** symbols, dependencies, relevant tests, and source locations.
3. **Ephemeral continuity:** attempted approaches, partial work, observed failures, and unresolved questions.
4. **Durable project knowledge:** accepted decisions, conventions, verified lessons, and mechanisms.

**[I]** These need different retention and acceptance policies. Treating all four as a promoted “memory” collection would make recovery unnecessarily expensive and durable knowledge unnecessarily noisy.

**[F]** The current proposal recommends controller-owned, source-linked, versioned records; fresh role-specific sessions; propose→promote; and conversion of repeated lessons into tests, validators, or invariants. It explicitly proposes no external memory service, vector database, graph database, or self-editing agent memory for v1. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:169`; `ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:205`)

**[I]** I agree with that direction. The missing design work is the semantics of individual record types, decision authority, temporal validity, and exposure to consumers—not selection of a memory product.

### 3.2 The predecessor already separates several useful surfaces

**[F]** The dossier derives source anchors, symbol ranges, tests, and gate failures mechanically from the repository. Builder notes are explicitly untrusted and can survive interrupted attempts. Build trails now include shell reads and re-resolve symbols. Phase digests are bounded transformations of prior receipts and packets. (`NB/bin/nb_task_dossier.py:1`; `NB/bin/nb_build_notes.py:1`; `NB/bin/nb_build_trail.py:1`; `NB/bin/nb_build_trail.py:176`; `NB/bin/nb_phase_digest.py:1`)

**[F]** The phase digest is not literally just neutral pointers: its implementation includes passed/open claim groupings, statuses, finding claims, coverage outcomes, and prior decisions. (`NB/bin/nb_phase_digest.py:128`)

**[I]** Consequently, “controller-generated” is not equivalent to “free of model framing.” A deterministic transformation can faithfully carry a previous model’s judgment. Provenance must retain this distinction.

### 3.3 Dossier measurements: no causal gain established

**[F—computed]** I recomputed the build-metric comparison using the recorded installation time, September 30 at 23:28, and records from September 28 through October 1 at `23:33:53.921538+03:00`. I classified each task’s first recorded call as initial and later calls as repairs. Missing measurements were excluded rather than treated as zero. The installation timestamp comes from `NB/docs/WORK_QUEUE.md:51`, read as data; source rows are listed in the measurement appendix.

| Measure | Before installation | After installation |
|---|---:|---:|
| Initial-call median time to first edit | 245 s, n=16 measured | 461 s, n=6 |
| Repair median time to first edit | 215 s, n=33 | 421 s, n=24 |
| Repair pre-edit re-read share, where recorded | 82/113 = 72.6%, n=5 calls | 390/497 = 78.5%, n=24 calls |

**[F—computed]** The initial-call “before” group had seventeen calls, one without a first-edit measurement. This is not an exact replication of the supplied helper result: my later snapshot includes twenty-four post-install repairs, and my explicit first-call classification produces thirty-three pre-install repairs. The supplied `398 s, n=23` post-install median is consistent with the earlier sample before the additional repair. Median-contributing rows include `RT/var/build-metrics/F-1.6-1.jsonl:1`, `F-1.1-validated-structured-output.jsonl:1`, `F-1.6-8.jsonl:1`, `F-1.6-6.jsonl:1`, `F-1.5-audit.jsonl:7`, `F-1.6-3.jsonl:3`, and `F-1.6-7.jsonl:2`, all under the same metrics directory.

**[I]** These are unmatched before/after samples. Task mix, task difficulty, model behavior, and repair history are not controlled. They establish neither benefit nor harm caused by the dossier. The small pre-install re-read sample further limits that comparison.

**[F]** The research proposal’s older account of a Read-tool-only trail is no longer an accurate description of the current trail implementation, which includes shell reads. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:128`; `NB/bin/nb_build_trail.py:1`)

**[I]** Controlled effects on accepted-task throughput, escaped defects, and total cost are **not measured** by this comparison. Earlier editing is not inherently better; source re-reading can be necessary verification.

### 3.4 The stronger predecessor warning concerns process overhead

**[F]** A predecessor review recorded 24,695 package lines versus 181 product lines for one audit, and 36,769 versus 656 for another. Package byte tests caused all five recorded gate failures in the first audit. The same report carefully distinguishes historical measurements from simulated savings. (`NB/docs/2026-09-23-repetition-reduction-plan.md:10`; `NB/docs/2026-09-23-repetition-reduction-plan.md:24`)

**[F]** The ADC brief explicitly rejects complexity creep and cites acceptance machinery whose own stops exceeded real blockers. (`ADC/docs/handoff/04-knowledge-layer-brainstorm.md:80`)

**[I]** The implication is broader than “keep prompts short.” Knowledge maintenance must not become a second acceptance system that requires agents to repeatedly regenerate evidence packages proving that unchanged evidence still exists.

## 4. Side-by-side comparison

Forms and citations below are **[F]**; equivalence judgments are **[I]**.

| Concept | InfraMedic | auto-dev-cycle / predecessor | Same abstraction? |
|---|---|---|---|
| Durable custody | Installation-owned catalog and source owners. (`IM/inframedic/knowledge/catalog.py:74`) | Controller-owned records surviving executors. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:169`) | **Yes:** knowledge outlives the producer. |
| Domain subjects | Devices, entities, relations. (`IM/inframedic/knowledge/subjects.py:24`) | Projects, tasks, attempts, repository revisions. (`ADC/docs/plan/01-first-release-plan.md:267`) | **Partial:** typed references transfer; subject vocabularies do not. |
| Evidence | Observations, standards, archived sources, detector outputs. (`IM/inframedic/knowledge/facts.py:93`; `IM/inframedic/standards/models.py:73`) | Source trees, tests, receipts, diffs, findings. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:171`) | **Yes:** exact, attributable evidence references. |
| Interpretation | Annotations, patterns, judgments, disputed standards. (`IM/inframedic/patterns/store.py:1`; `IM/inframedic/standards/models.py:124`) | Lessons, planner playbooks, attempt notes. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:198`) | **Yes:** claims distinct from evidence; acceptance differs. |
| Promotion | Different human and mechanical gates by artifact and act. (`IM/docs/notes/2026-08-10-diagnosis-layers-gates-and-ground-truth.md:127`) | Proposed deterministic validation plus independent model/supervisor review. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:182`) | **Partial:** decision binding transfers; decision-maker policy does not. |
| Content version | Artifact versions and derived representations. (`IM/inframedic/registry/store.py:1500`) | Proposed versioned records and code-anchor checks. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:173`) | **Yes**, with repository-tree identity added for ADC. |
| Publication | Placement channel, audience, expiry, revocation. (`IM/inframedic/registry/store.py:1536`) | Proposed eligibility for role-specific auto-loading. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:177`) | **Partial:** publication is not identical to prompt inclusion. |
| “Projection” | Derived retrieval/index state and generation. (`IM/inframedic/knowledge/projection.py:1`) | Bounded role/model context view. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:177`) | **Different layers:** distinguish index projection from consumer view. |
| Scope and egress | Audience, all-subject data scope, applicability, live sensitivity. (`IM/inframedic/knowledge/authz.py:298`) | Project data policy × source trust; isolated executor context. (`ADC/docs/plan/00-brainstorm-decisions.md:34`; `ADC/docs/plan/01-first-release-plan.md:221`) | **Yes:** separate relevance, access, and destination permission. |
| Consumer evidence | Exact retrieval audit and pinned source references. (`IM/inframedic/knowledge/retrieval_audit.py:124`) | Context metrics, trails, proposed projection metrics. (`NB/bin/nb_build_trail.py:1`; `ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:190`) | **Yes:** record what was actually exposed, not merely available. |
| Freshness | Source/transform/index fingerprints; disputes and explicit states. (`IM/inframedic/knowledge/catalog.py:1871`) | Code anchors, supersession, final-tree evidence. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:173`; `ADC/docs/plan/01-first-release-plan.md:276`) | **Partial:** common invalidation structure, domain-specific validity tests. |
| Negative knowledge | Explicit non-relations with detector-change invalidation. (`IM/inframedic/patterns/store.py:1`; `IM/inframedic/patterns/registry.py:114`) | Failed approaches, rejected findings, failed tests. (`NB/bin/nb_build_notes.py:1`; `NB/bin/nb_phase_digest.py:128`) | **Yes:** scope negatives to conditions and evidence; avoid universal prohibitions. |
| Retention | Private expiry, publication holds, explicit purge, tombstones. (`IM/inframedic/knowledge/retention.py:91`) | Proposal says “supersede, never delete.” (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:176`) | **Currently different:** ADC should adopt a more precise retention distinction. |
| Model trust | Rule-derived measurements keyed to model context/snapshot. (`IM/inframedic/trust/rules.py:42`; `IM/docs/notes/2026-08-28-model-change-resilience-doctrine.md:40`) | Capability-based roles and evaluation before classifier enablement. (`ADC/docs/handoff/04-knowledge-layer-brainstorm.md:39`; `ADC/docs/plan/01-first-release-plan.md:333`) | **Partial:** measured suitability transfers; one universal trust score does not. |
| Knowledge to mechanism | Human-gated detectors, packs, gold, runbooks. (`IM/docs/notes/2026-08-10-diagnosis-layers-gates-and-ground-truth.md:127`) | Incident→lesson→test/validator/invariant. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:188`) | **Yes:** recurring knowledge may justify a separately reviewed mechanism. |
| Foreign knowledge | Signed imports, compatibility checks, local re-gating planned. (`MASTER/README.md:274`) | Cross-project reuse remains a brainstorm question. (`ADC/docs/handoff/04-knowledge-layer-brainstorm.md:61`) | **Potentially:** foreign evidence must not inherit local acceptance. |

## 5. The abstractable kernel

### 5.1 Logical entities

**[I]** The following are logical contracts, not a prescription for eight database tables or eight services. ADC can initially serialize several into existing event and receipt records.

| Entity | Minimum useful fields |
|---|---|
| **ArtifactRef** | `namespace`, `artifact_id`, `kind`, `origin`, `owner_ref` |
| **RevisionRef** | `revision_id`, `artifact_id`, `content_digest`, `schema_id`, `source_revision`, `recorded_at`, `supersedes[]` |
| **Derivation** | `derivation_id`, exact `inputs[]`, `output_revision`, producer identity, transformation/prompt/config digest, optional model snapshot, run ID, time |
| **KnowledgeItem** | `item_id`, `revision_ref`, `epistemic_kind`, typed claim/body, `subject_refs[]`, supporting/refuting `evidence_refs[]`, applicability, validity conditions |
| **Decision** | exact target and purpose, outcome, actor/delegation, policy revision, evidence references, expected prior state, time, optional expiry |
| **Publication** | exact representation/revision, audience, channel or consumer class, data-scope reference, destination policy, publication decision, expiry/revocation |
| **ContextManifest** | consumer role/profile, task/run and source revision, policy revision, ordered included references, omitted references with reasons, rendering digest and budget |
| **UseReceipt** | manifest/retrieval identity, actual returned or delivered references, typed outcome, latency/size/cost measurements where available, correlation ID |

**[I]** A `KnowledgeItem` needs to distinguish at least:

- an observation;
- an externally asserted fact;
- a model or human inference;
- a normative rule or accepted decision;
- a proposed procedure.

A test result is an observation. “This function is safe” is an interpretation. “Changes to this interface require review” is a normative rule. Storing all three as interchangeable “facts” would make promotion and contradiction handling ambiguous.

**[I]** Use typed claim schemas supplied by domains. InfraMedic might register a relation hypothesis; ADC might register a repository convention or failed-approach record. The kernel validates schema identity and provenance. It should not invent a universal vocabulary of infrastructure conditions and software defects.

### 5.2 Separate lifecycle dimensions

**[I]** Avoid one enormous state machine. Keep these dimensions independent:

| Dimension | Suggested states or representation |
|---|---|
| Epistemic decision | `proposed`, `accepted`, `rejected`, `disputed`, `superseded` |
| Validity | `current`, `stale`, `unknown`, `expired`, with machine-readable reasons |
| Publication | `private`, `published`, `revoked` |
| Materialization | `absent`, `building`, `ready`, `failed`, `behind` |
| Retention | retained payload, held payload, or purged payload with tombstone |

**[I]** Acceptance must be **acceptance for a stated purpose**. A claim accepted for planning assistance need not be accepted as an execution precondition. A disputed document may remain readable as historical evidence. A revoked publication does not retroactively erase its past delivery.

**[I]** Temporal fields also need distinct meanings:

- `observed_at`: when the underlying event was observed;
- `recorded_at`: when the controller learned it;
- `valid_from` / `valid_until`: when the claim applies;
- revision or configuration conditions: where it applies.

A `superseded_by` link alone does not express all four.

### 5.3 Minimal interfaces

**[I]** A portable interface could remain small:

```text
record(input, provenance, idempotency_key) -> RevisionRef
propose(typed_item, evidence_refs) -> KnowledgeItem
decide(target, purpose, expected_state, policy, actor, evidence) -> Decision
publish(revision, audience, scope, destination_policy, decision) -> Publication
resolve(ref, principal, consumer_context) -> Content | TypedRefusal
project(request, exact_source_state, budget) -> ContextManifest
record_use(manifest_or_query, actual_exposure, measurements) -> UseReceipt
invalidate_or_retire(target, reason, evidence, expected_state) -> Event
```

**[I]** Payload deletion should be a separately authorized retention operation. Domain-specific search can sit behind `project` or an optional retrieval interface; the kernel need not mandate vectors, full-text search, or a graph.

**[I]** Required adapter interfaces are:

- subject and source-revision resolution;
- authorization and destination policy;
- promotion policy;
- domain validation and freshness checks;
- payload access;
- context rendering and optional retrieval.

### 5.4 Invariants worth sharing

**[I]**

1. **A producer cannot manufacture authority by writing text.**
2. **A decision binds exact content, purpose, policy, and expected state.**
3. **Publication, epistemic acceptance, and execution authority remain separate.**
4. **Derivations preserve input lineage; deduplicated bytes do not erase production events.**
5. **Scope is conservative across derivations.** Summarization does not automatically declassify its inputs.
6. **Applicability never grants access.**
7. **Every exposure rechecks current access and destination constraints.**
8. **Pinned references never silently resolve to a different version.**
9. **Empty, unavailable, unauthorized, stale, and truncated are distinct outcomes.**
10. **A consumer receipt records actual exposure, including omission.**
11. **Invalidation preserves the historical observation and its original conditions.**
12. **Foreign acceptance does not automatically become local acceptance.**
13. **Knowledge survives executor replacement, while stale attempts cannot mutate current state.**
14. **Derived indexes and context views are reproducible from source records or explicitly identified as model-derived artifacts.**

**[F]** These are grounded in existing decisions about InfraMedic’s exact versions, authorization and retrieval outcomes, and ADC’s content-bound approvals and attempt fencing. They are not all already implemented identically in both systems. (`IM/docs/notes/2026-08-28-knowledge-center-doctrine.md:114`; `IM/inframedic/knowledge/authz.py:256`; `IM/inframedic/knowledge/retrieval_audit.py:124`; `ADC/docs/plan/01-first-release-plan.md:275`; `ADC/docs/plan/01-first-release-plan.md:420`)

## 6. What must not be abstracted

### 6.1 Domain semantics and authority policy

**[I]** Keep these outside the shared kernel:

- Device identity heuristics, neighbor attribution, fleet grouping, detector thresholds, and chronic-state rules.
- Repository symbol extraction, merge-tree construction, build/test interpretation, and review acceptance.
- InfraMedic’s particular human/quorum rules.
- ADC’s particular unattended supervisor grants and independent-review rules.
- Vendor recall semantics, infrastructure action ceilings, and Saga/Kobra execution policy.
- Model-routing and provider-adapter implementations.

**[F]** These distinctions already exist in the source: InfraMedic implements platform-specific facts and identity rules; ADC reserves some decisions to humans while allowing bounded unattended operation under grants. (`IM/inframedic/knowledge/facts.py:93`; `IM/inframedic/knowledge/identity.py:1`; `ADC/docs/plan/00-brainstorm-decisions.md:71`; `ADC/docs/plan/01-first-release-plan.md:403`)

**[I]** The common framework should call a policy interface, not encode “human approval everywhere” or “two models approve everything.” Either universalization would damage one consumer.

### 6.2 Infrastructure that is premature for ADC v1

**[I]** Do not bring across the complete placement/channel system, index-generation management, document ingestion doors, fleet identity registry, signed distribution network, or mesh. Their existence in InfraMedic does not establish their necessity in ADC.

**[F]** A bounded count illustrates the implementation surface: the inspected Knowledge Center migrations define seven base tables plus upload staging, index generations, generation reports, and upload dispositions—eleven named tables in that subset. This excludes much of the broader knowledge, authorization, and evolution system. (`IM/inframedic/registry/store.py:1489`; `IM/inframedic/registry/store.py:1655`; `IM/inframedic/registry/store.py:1763`; `IM/inframedic/registry/store.py:1793`; `IM/inframedic/registry/store.py:1851`)

**[F]** Other independent dimensions include six authorization actions, six projection states, and eight generation states. These are different vocabularies, not a single twenty-state lifecycle. (`IM/inframedic/knowledge/authz.py:72`; `IM/inframedic/knowledge/catalog.py:60`; `IM/inframedic/knowledge/generation.py:62`)

**[I]** Counts do not prove bad design. They show why “reuse the knowledge layer” is not a small implementation decision. The extraction cost, portability benefit, and maintenance savings are **not measured**.

### 6.3 Avoid a universal graph and a universal trust score

**[I]** Shared typed references do not require a graph database. Ontological extraction does not require the kernel to own every relation type. Likewise, model reliability on one task family should not become a universal trust score that decides authorization or claim truth.

**[I]** Avoid introducing a policy language, workflow language, or generic rule engine merely to make the first specification appear agnostic. Plain schema identifiers and explicit adapter functions are sufficient until actual extension pressure demonstrates otherwise.

## 7. Fleets of small and medium local models

### 7.1 What the evidence establishes

**[F]** InfraMedic explicitly permits model-dependent investigation to be skipped while deterministic diagnosis remains available. Its staged verification record includes model comparisons, but those runs share the knowledge substrate rather than testing knowledge-on versus knowledge-off. (`IM/docs/notes/2026-08-10-diagnosis-layers-gates-and-ground-truth.md:87`; `IM/docs/notes/2026-08-17-llm-verification-night.md:25`)

**[F]** One Knowledge Center measurement used a cloud-hosted `gpt-oss:20b` session: a six-second answer with three collection retrieval records. That is an integration measurement, not evidence for local deployment performance or a controlled memory benefit. (`IM/docs/notes/2026-09-04-1.3-7-retrieval-audit.md:60`)

**[F]** The same build record measured warm canon retrievals at 11.5 and 9.9 seconds for 564 authorized IDs, and a cold request timed out at approximately thirty seconds. These are dated measurements, not current service guarantees. (`IM/docs/notes/2026-09-04-1.3-7-retrieval-audit.md:54`)

**[I]** End-to-end benefit for a local small-model fleet is therefore **not measured** by these records. Retrieval and curation overhead must be included in the experiment; reduced prompt size alone is insufficient.

**[F]** The external literature supports testing compression and accurate historical context, not assuming a universal memory gain. ACON reports reduced peak context and improved smaller-model performance on its evaluated agent tasks. SWE-ContextBench reports that accurate summaries and retrieval help while misleading context can harm. VibeMemBench reports that eleven of twelve tested system–solver pairings failed to improve over their memory-disabled baselines, despite constructing tasks selected for potential benefit from experience. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:34`; `ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:65`; primary papers: [ACON](https://arxiv.org/abs/2510.00615), [SWE-ContextBench](https://arxiv.org/abs/2602.08316), [VibeMemBench](https://arxiv.org/abs/2609.23570))

**[I]** These results do not establish that small models should only consume knowledge while strong models produce it. They justify separating tasks and measuring suitability.

### 7.2 Producer roles

**[I]** A useful division is:

| Work | Default producer |
|---|---|
| Source identity, hashes, symbol anchors, test outcomes, scope inheritance | Deterministic code |
| Bounded extraction into a schema | Any model that passes task-specific evaluation |
| Candidate relations, explanations, lessons | Models or humans, recorded as proposals |
| Contradiction resolution and broad consolidation | A separately evaluated curator, with evidence access |
| Publication/acceptance | Platform policy, never the producer’s self-assessment |
| New tests, detectors, or validators | Normal reviewed development path |

**[I]** “Strong model curates” is a starting allocation, not a truth guarantee. A larger model can produce an unsupported generalization; a smaller model can perform a narrow extraction correctly. Evaluate extraction accuracy, unsupported-claim rate, evidence completeness, and downstream effects.

**[F]** InfraMedic’s chronic-duties notes enumerate recurring human landing points for automatically gathered material. ADC instead proposes independent-model or supervisor review for ordinary knowledge promotion, within its existing human-reserved boundaries. (`MEM/archive/knowledge-layer-chronic-duties.md:20`; `MEM/archive/knowledge-layer-chronic-duties.md:99`; `ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:182`; `ADC/docs/plan/00-brainstorm-decisions.md:72`)

**[I]** Human curation minutes, promotion backlog age, and unresolved-conflict growth should be measured. Sustainable curation capacity is **not measured** in the inspected evidence.

### 7.3 Consumer views

**[I]** Define consumer profiles by measured capability, context budget, tool reliability, latency, and destination policy—not parameter count alone.

Recommended starting views:

- **Builder:** exact task constraints, current source anchors, focused evidence, and same-task attempt continuity.
- **Reviewer:** requirements, exact candidate tree, independent test evidence, and a carefully controlled prior-finding view.
- **Planner:** accepted decisions and conventions, with evidence and applicability.
- **Supervisor:** regenerated controller state plus a bounded decision history.
- **Extractor/classifier:** the minimum input required by its schema.

**[I]** Permit smaller models to use on-demand retrieval if they can use it reliably. Do not reserve retrieval tools for frontier models by definition.

**[I]** For independent review, consider two phases: record a fresh assessment first, then reveal prior findings and dispositions to reconcile omissions. Track exposure lineage across summaries; a summary of builder reasoning is still builder-derived reasoning.

## 8. Recommendations and sequencing

### 8.1 For InfraMedic’s general framework

**[I]**

1. **Name the current accomplishment precisely:** a governed artifact catalog integrated with infrastructure-specific knowledge systems.
2. **Document two extension boundaries:** domain claim/subject schemas, and platform authorization/promotion policy.
3. **Add explicit claim-to-evidence semantics where needed.** Do not force every claim to become a new document.
4. **Preserve derivation occurrences independently of content deduplication.**
5. **Expose consumer manifests alongside retrieval audits.** Record both retrieved candidates and content actually delivered after budgeting.
6. **Treat scope propagation as lineage-based.** Keep registry text scanning as supplementary evidence.
7. **Validate generality using ADC-shaped fixtures before refactoring packages.** Example: a claim valid on one repository tree but invalid after a semantic change despite an unchanged symbol name.
8. **Keep master and mesh work behind actual exchange requirements.**

**[I]** A useful milestone is not “all modules renamed generically.” It is demonstrating that the same contracts represent one infrastructure observation and one development lesson without importing device rules into ADC or review rules into InfraMedic.

### 8.2 For auto-dev-cycle v1

**[F]** ADC already plans SQLite WAL and an append-only event log; its milestones place foundational contracts first, a complete task cycle next, and the T1 baseline in M2. (`ADC/docs/plan/00-brainstorm-decisions.md:81`; `ADC/docs/plan/01-first-release-plan.md:533`)

**[I]** Use that substrate rather than adding a knowledge service.

| Sequence | Deliverable |
|---|---|
| M0 contracts | Exact revision/evidence references; decision bindings; scope labels; context-manifest schema. |
| M1 task cycle | Controller-generated task context and sealed attempt records; regeneration after restart; stale-attempt fencing. |
| M2 evaluation | Baseline and ablations for dossier, code index, attempt continuity, and small curated lessons. |
| After demonstrated need | A minimal accepted-knowledge collection with narrow promotion and invalidation rules. |
| Later | Cross-project knowledge exchange, semantic retrieval, or shared runtime extraction. |

**[I]** The first durable knowledge types should be few: accepted project decisions, verified task-derived lessons, and explicitly scoped negative results. Keep raw attempt notes in a separate continuity class.

**[I]** Reuse existing receipts and events by reference. Do not copy their full contents into a parallel knowledge database and then build another mechanism to keep the copies consistent.

**[I]** Evaluate with:

- accepted correct tasks per unit time and cost;
- escaped and critical defects;
- total model, retrieval, and curation cost;
- recovery success after executor replacement;
- cross-project leakage and unauthorized destination exposure;
- stale knowledge consumed;
- secondary measures such as first-edit latency, rereads, and injected bytes.

**[I]** Compare experimental arms with equal mandatory requirements, canon, and safety constraints. “Memory off” should mean optional historical assistance off, not essential task information removed.

### 8.3 For a shared component or specification

**[I]** Start with a small specification and language-neutral fixtures. Implement the adapters independently in Python and Go.

Initial conformance cases should include:

1. Same bytes produced by two derivations.
2. A source revision changes while a pointer still resolves.
3. A proposal is published privately but not accepted for auto-loading.
4. Access is revoked after selection but before dereference.
5. A local-to-cloud fallback encounters restricted historical context.
6. A rejected hypothesis becomes eligible for reconsideration after its conditions change.
7. A context budget omits a relevant source.
8. An imported accepted item remains locally unaccepted.
9. A late executor submits knowledge after its attempt is fenced.
10. Payload deletion preserves an honest tombstone without pretending the evidence remains readable.

**[I]** Extract a library only after both implementations use these contracts and the duplicated work is measurable. The earliest plausible shared implementation may be schema validation and fixture tooling, not storage, retrieval, or orchestration.

## 9. Open questions for the operator

All defaults below are **[I] recommendations**, not blockers to starting the ADC brainstorm.

| Question | Options | Recommended default |
|---|---|---|
| What should “general framework” mean first? | Artifact governance; semantic extraction engine; complete multi-agent platform | **Artifact governance plus typed claim/evidence contracts.** Add extraction as domain adapters. |
| Who accepts ADC lessons? | Human always; independent model always; policy by record type | **Policy by type.** Mechanical evidence needs no model endorsement; advisory lessons require evidence review; authority changes follow existing reserved paths. |
| Can replacement builders see unpromoted notes? | Never; same-task only; broad reuse | **Same-task only**, explicitly untrusted, revision-bound, bounded, and excluded from durable auto-loading. |
| How much InfraMedic machinery should ADC reuse? | Python subsystem; new shared service; contracts only | **Contracts only for v1.** |
| Should knowledge cross projects automatically? | Automatic similarity matching; explicit import; prohibited | **Explicit import with provenance and local acceptance.** Default isolation. |
| How are consumer profiles selected? | Parameter-count tiers; provider/model names; measured capability profiles | **Measured capability profiles**, with simple initial presets. |
| What survives deletion? | All payloads forever; delete everything; policy retention plus tombstones | **Policy retention plus tombstones**, with publication/evidence holds. |
| How independent should reviews be? | No prior findings; prior pointers immediately; staged disclosure | **Test staged disclosure** against fresh-only and immediate-pointer arms. |
| When should a lesson become a mechanism? | Every recurrence; fixed count; expected benefit and verified failure mode | **Evidence of a recurring failure with a testable prevention mechanism.** Avoid automatic rule accumulation. |
| What warrants a shared library? | Architectural similarity; two working adapters; measured duplicate maintenance | **Two working adapters plus measured duplicate maintenance.** |
| What is the primary success criterion? | Less context; earlier first edit; correct accepted work per total cost | **Correct accepted work per total cost**, with defect and isolation guardrails. |
| When should master/mesh work start? | During abstraction; after a second installation; after any remote executor | **After a concrete second-installation exchange need.** Remote execution alone does not imply knowledge federation. |

## 10. Disagreements with the existing ADC proposal

### 10.1 “Supersede, never delete” is too broad

**[F]** The proposal uses that rule universally; InfraMedic already distinguishes retained history from purgeable private payloads and preserves tombstones. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:176`; `IM/inframedic/knowledge/retention.py:91`)

**[I]** Replace it with: **Do not silently rewrite accepted history. Retain or delete payloads under explicit policy, preserving sufficient decision and deletion records.**

### 10.2 Deterministic code knowledge is not automatically current

**[F]** The proposal says deterministic code-knowledge layers “are always current, because the code is the source of truth.” (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:120`)

**[I]** Deterministic generation does not establish freshness. Bind every map, symbol index, or extracted relation to an exact tree and extractor version. Regenerate or refuse a mismatched result.

### 10.3 Model size is an insufficient projection policy

**[F]** The proposal assigns compact projections to small models and on-demand query tools to frontier models. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:177`)

**[I]** Keep compact defaults, but make retrieval eligibility empirical. A small model may benefit from a narrow lookup tool; a frontier model may waste time with broad retrieval. Size is one input, not the policy.

### 10.4 The prohibition on unpromoted notes conflicts with recovery needs

**[F]** The builder table both includes prior attempt records with notes and excludes unpromoted agent notes. The predecessor deliberately preserves untrusted builder notes, including interrupted-attempt output. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:198`; `NB/bin/nb_build_notes.py:1`)

**[I]** Resolve this by separating **ephemeral attempt continuity** from **durable accepted knowledge**. The former can be consumed cautiously without pretending it has been promoted.

### 10.5 Independent model review is not sufficient evidence of truth

**[F]** Promotion is proposed as deterministic validation plus independent model or supervisor review. ADC’s approved plan separately says model output gains no authority merely by being stored. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:182`; `ADC/docs/plan/01-first-release-plan.md:428`)

**[I]** Specify what the reviewer must verify and what acceptance permits. Mechanical observations, explanatory hypotheses, repository conventions, and authority-bearing rules require different evidence. A second model’s agreement should not collapse those distinctions.

### 10.6 “Pointer digest” does not guarantee independence

**[F]** The proposed reviewer input emphasizes pointers rather than verdicts, but the predecessor digest carries claim groupings, statuses, and decisions. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:199`; `NB/bin/nb_phase_digest.py:128`)

**[I]** Audit semantic exposure, not just formatting. A pointer’s label, selection, grouping, or accompanying status can carry prior judgment. E2 should test that explicitly.

### 10.7 Anchor checks are necessary but insufficient

**[F]** The proposal mechanically flags an item stale when its anchored symbol or file disappears. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:173`)

**[I]** A symbol can remain while its behavior, callers, configuration, or external contract changes. Validity should reference dependencies and revision conditions. “Anchor resolves” should mean locatable, not verified current.

### 10.8 The experiments need outcome and overhead accounting

**[F]** E1–E5 cover builder context, reviewer framing, playbooks, poisoning, and anchor staleness. E1 includes resolve rate alongside first-edit latency, reads, turns, and cost. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:219`)

**[I]** Keep these experiments, but add accepted correctness, defect escape, curator time, retrieval overhead, recovery, and destination-policy failures. Treat time to first edit and reread counts as diagnostic measures. Keep essential provenance and authorization even if optional memory assistance fails to improve productivity.

### 10.9 “Agents own nothing” should mean custody, not erased authorship

**[F]** Controller ownership is the proposal’s first principle, and source provenance includes the author model. (`ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:169`; `ADC/docs/research/2026-10-01-memory-vs-fresh-context.md:173`)

**[I]** Preserve that distinction explicitly: agents possess no irreplaceable state, but every contribution retains its producer, model/configuration identity, source exposure, and attempt. Executor independence requires durable attribution.

---

## Measurement appendix

**[F—computed]** The dossier comparison used the following JSONL source rows under `RT/var/build-metrics/`. A range below identifies input rows, not an assertion that every row belongs to the same experimental group; grouping follows the timestamp and first-call method in §3.3.

| Source | Input rows |
|---|---:|
| `F-1.1-discovery-unreachable.jsonl` | 1–3 |
| `F-1.1-model-route-unavailable.jsonl` | 1–4 |
| `F-1.1-retro-gaps.jsonl` | 1 |
| `F-1.1-validated-structured-output.jsonl` | 1–4 |
| `F-1.2-4-collector-contract.jsonl` | 1–4 |
| `F-1.2-6e-trigger-guard.jsonl` | 1–2 |
| `F-1.4-approval-ttl-7d.jsonl` | 1 |
| `F-1.4-local-accounts-multi.jsonl` | 1–5 |
| `F-1.4-modelrelease-test-clock.jsonl` | 2–4 |
| `F-1.5-audit.jsonl` | 7–8 |
| `F-1.5-banner-utc.jsonl` | 1 |
| `F-1.5-kobra-1.jsonl` | 1–2 |
| `F-1.5-kobra-2.jsonl` | 1–2 |
| `F-1.5-kobra-3.jsonl` | 1–3 |
| `F-1.5-kobra-4.jsonl` | 1–3 |
| `F-1.5-regression-tests.jsonl` | 1–3 |
| `F-1.6-1.jsonl` | 1–3 |
| `F-1.6-2.jsonl` | 1–3 |
| `F-1.6-3.jsonl` | 1–7 |
| `F-1.6-4.jsonl` | 1–4 |
| `F-1.6-5.jsonl` | 1–5 |
| `F-1.6-6.jsonl` | 1–6 |
| `F-1.6-7.jsonl` | 1–3 |
| `F-1.6-8.jsonl` | 1–3 |
| `F-1.6-9.jsonl` | 1–3 |