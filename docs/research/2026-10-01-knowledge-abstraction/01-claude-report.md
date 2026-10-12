# What is abstractable between the InfraMedic and auto-dev-cycle knowledge layers — Claude's report

Date: 2026-10-01. Author: Claude (Opus 5.5), in the auto-dev-cycle knowledge-layer brainstorm session.
An independent Astra report on the same question is `02-astra-report.md`. Each of us then writes a synthesis
(`03-claude-synthesis.md`, `04-astra-synthesis.md`).

**Sources.** Read-only, from:
- `/home/mega/inframedic` (roadmap v0.3, the knowledge-center doctrine, the 08-10/08-12/08-13 knowledge canons, the
  1.3-x build records, code sizes);
- the InfraMedic memory notes (`durable-principles.md`, `archive/framework-vision-and-roadmap.md`,
  `archive/future-directions-topology-tenancy-mesh.md`, `archive/knowledge-layer-chronic-duties.md`);
- the unmerged master-platform contract (`ekaynac/inframedic-master`, branch `docs/inframedic-facing-contract`);
- this repo's plan and research documents, and a helper's survey of nöbetçi's context modules and runtime metrics.

**Fact or inference.** **[F]** marks a fact read from a source. **[I]** marks my inference. "Not measured" means no
measurement exists.

**Limit.** I read the cross-device and model-change doctrines only in the sections cited here. They are 1,236 and
1,179 lines long.

---

## 1. The intention and what "abstractable" should mean

**The operator's words:** InfraMedic has a knowledge layer "with the intention of creating a fully agnostic knowledge
layer", and a canon and plan "to become an abstracted ontological knowledge extraction framework for networks, groups,
fleets of small-medium (preferably local) LLMs, agents". The report serves two consumers:
1. InfraMedic's planned general framework;
2. the auto-dev-cycle (ADC) knowledge-layer brainstorm.

**What exists in writing [F].** The closest written statements of the general framework are:
- roadmap §1, "a domain instance of a general hierarchical knowledge-cumulating framework";
- invariant 7, the "generalization test": judge every feature twice, for the product and for the general framework;
- the 08-10 memory note, "a general, hierarchical knowledge-cumulating and knowledge-extracting framework — a fully
  self-growing intelligence / knowledge system";
- the 08-12 vision shelf: mesh/P2P sharing between self-hosted instances is "the productization of the whole
  framework".

I found **no document** that specifies the framework itself. Its entities, interfaces and package boundary are not
written down, and nothing addresses "fleets of small-medium LLMs" as such. The vision exists as an intention plus
invariants. The concrete design exists only in InfraMedic's domain vocabulary: device, platform, fleet group, Kobra
session, governed MCP gateway.

**Three readings of "abstractable":**

| Reading | What would be shared | Fit now |
|---|---|---|
| A shared **library** | One code package used by both | Poor. Python vs Go, a mature system vs an unbuilt one. Extracting code from one consumer produces that consumer's abstraction. |
| A shared **kernel spec** | Ontology, lifecycle states, invariants, minimal interfaces, conformance scenarios. Each platform implements it natively. | **Good.** Cheap, testable, and it forces InfraMedic to name what is domain-free. |
| A shared **protocol** | A bundle and envelope format so knowledge can move between instances and platforms | Later. Nothing crosses between the two platforms today. It matters for ADC cross-project transfer and for InfraMedic's master/mesh, both post-v1. |

**I adopt the second reading:** a small knowledge-kernel specification that both implement natively, with a
library extracted only by a rule of three (a third consumer, or both implementations stable).

---

## 2. InfraMedic's knowledge layer: built, designed, claimed

### 2.1 Built [F]

1. **A ground-truth hierarchy and epistemic separation** (08-10 canon; 08-12 canon §7).
   - Truth rungs:
     1. deterministic detector output (code);
     2. curated gold (human);
     3. tool-log evidence (investigation citations).
   - "Everything a model produces is measured against one of these three rungs."
   - Knowledge is organised **by epistemic type, not topic**, with one home per claim type:
     - semantic meaning → value dictionary;
     - firing rule → detector code;
     - judgement → learned pack;
     - rationale → prose canon.
   - Rules: "reference, never copy", mechanically enforced. Precedence: vendor fact > human canon > learned pack.
2. **The catalog ontology** (knowledge-center doctrine §2–§3; 1.3-1, schema 42):
   - an artifact has immutable, content-addressed **versions**;
   - a version has immutable **representations** (original, extracted, sanitized, ocr, caption) with transform
     provenance;
   - publication is an append-only **placement** (channel `session | recent | shared | governance`, plus an
     audience);
   - searchability is a derived **projection** in the gateway, never the authority;
   - promotion appends a placement and never mutates identity;
   - retirement revokes and rewrites nothing; purge leaves a tombstone.
3. **Provenance** on every version, representation and projection: producer (human, code, or model + principal),
   model profile, `derived_from` edges, transform identities. The **projection fingerprint** covers the version SHA,
   the sanitized SHA and the extractor/redactor/chunker/embedder identities. Any change makes the projection
   `behind`, computed locally (1.3-2, 1.3-3).
4. **Orthogonal scoping axes** (doctrine §4–§5):
   - kind;
   - placement;
   - `applies_to` (relevance, any-of);
   - `data_scope` (confidential subjects, all-of);
   - audience;
   - selection;
   - index partition;
   - sensitivity floor (max of the sources);
   - egress (`restricted` never reaches a cloud model, whatever the consent).

   One policy seam, `authorize_document(...)`, is re-evaluated at every use (1.3-4).
5. **Server-side selection, references not copies.** A session pins exact representations. The model gets
   `list_sources` / `search_sources` over the pinned manifest and can **request but not add** a source. Injected
   sources are references resolved per turn. Injection is whole under a budget, reported as included / summarized /
   omitted, never silently truncated (1.3-5).
6. **Retrieval audit.** One row per read, on every exit, with the exact returned version/representation/chunk ids, the
   fingerprint, a selection manifest SHA, a query *digest* (never the text), the outcome and the latency (1.3-7).
7. **Evolution under gates.**
   - GEPA evolves only the **packs** (narration text) against benchmark gold, with a holdout, a human promotion gate,
     signing and a production shadow check.
   - Detectors change only by human PR.
   - Gold grows from human ✓/✗ verdicts.
   - Eleven recurring human duties form the knowledge layer's standing gates (memory `knowledge-layer-chronic-duties`).
8. **Accumulation without authority.**
   - Chronic memory: occurrences per fingerprint.
   - `fleet_issues` / `entity_sightings`.
   - `PossiblePattern`, an authority-free hypothesis: `accumulating → possible → corroborated → stale | disproved`,
     append-only membership, durable negative knowledge (`not_related`). It "can never open/close/merge/suppress a
     Case … or widen authority" (cross-device §5.3–5.4).
9. **Per-model trust from rules and data only**, never LLM-as-judge. `model_runs` records every run kind; trust
   profiles are computed views.
10. **Signed bundles and the EXPORT/IMPORT ledger kinds.** Ed25519 pack bundles verify vendor origin and content
    digest before anything is touched (master contract §4).

### 2.2 Designed, not built [F]

- Index generations and human cutover (1.3-8 / 1.3-9).
- Real principals (1.4-10).
- Installation identity.
- Model-coupling classes and materiality on every artifact (model-change §0.1, roadmap §7.6).
- The release bundle with suites and cassettes.
- The uplink, the master platform and the Installation Registry: master contract §9 lists ten open items, and the
  master is deferred "until there is more than one user".

### 2.3 Claimed or envisioned, not designed [F]

- The general framework as a separable thing.
- Mesh/P2P knowledge sharing ("signed bundles gossiped between consenting peers; per-peer trust mirroring per-model
  trust").
- "Fleets of small-medium local LLMs" as the producer/consumer population of a knowledge-extraction network.
- **[I]** The code does not yet support the "agnostic" claim. The knowledge package derives scoping from the device
  registry's identity facts (`knowledge/scoping.py`), its audience grammar is device groups, and retrieval is bound to
  the product's governed MCP gateway. These are good domain choices. They also show that **the kernel has never been separated from
  the domain**: nothing in the repo is importable without InfraMedic's registry.

### 2.4 Cost and measured benefit

**Size [F]:**
- schema version 67;
- 93 `CREATE TABLE` statements in `registry/store.py`;
- about 160k lines of Python, of which `inframedic/knowledge/` alone is 17k;
- 82 commits to `inframedic/knowledge` since 08-26.

**Faz 1.3 alone took [F]:**
- seven sub-plans, about 20 PRs;
- a 14-PR fix round;
- six fresh-context reviews with re-reviews (roadmap §6, ruling 12).

**Use [F]:**
- Before Faz 1.3: "**60 `rag.read` ever**", all on 08-25/26 (doctrine §1).
- 1.3-7 live measure: 5 retrieval rows.
- Canon retrieval latency: 27.4 s for 563 ids, then 9.9–11.5 s warm. Two upstream asks are open (#200, #201).

**Benefit: not measured.** I found no ablation (knowledge on vs off) for narration, investigation or Kobra outcomes.

**[I] The knowledge center is careful and largely correct infrastructure whose value to outcomes has not been
measured.** This is the same pattern the ADC brief warns about: the predecessor's acceptance machinery "grew until its
own stops exceeded real blockers".

---

## 3. What auto-dev-cycle needs, and what the evidence says

From `docs/research/2026-10-01-memory-vs-fresh-context.md`, the plan, and the predecessor survey.

**Needs:**
- Executors are swapped, drained and replaced, so knowledge must be controller-owned.
- Builders are small or cheap models; planners and reviewers are strong.
- Reviewers must stay independent: no builder framing.
- The supervisor needs continuity without long-lived context.
- Multi-project operation with per-project data policy (D9, D18, D28).

**The literature [F, from the research doc]:**
- Unfiltered carried memory mostly does not help coding agents.
- A wrong or freely retrieved summary is worse than none.
- Source-first beats conclusion-first; stale conclusions fail at every model size.
- Small models write and use memory poorly but gain from curated context.
- Shared builder↔reviewer memory correlates failures.
- Agent-written auto-loaded notes are an injection surface.

**Predecessor evidence [F]:**
- The mechanical context modules are content-capped and name what they cut: dossier 8 KB, trail 6 KB, digest 24 KB,
  canon 8/24 KB.
- **The dossier shows no gain yet.**
  - First-build time to first edit went from 245 s to 461 s (n=16 vs 6).
  - Repair time to first edit went from 215 s to 398 s (n=34 vs 23).
  - The repair re-read share stayed 73% → 78%.
  - The comparison is confounded.
- **Lessons were never projected to agents.** CANON §6, the memory notes and the dispositions doc were never fed back
  ("nothing reads it back").
- **The lessons that held all became mechanisms:**
  - the test write guard;
  - a single `busy()`;
  - the repair signature that stops after the third identical failure;
  - the grant check in the installer;
  - read-before-teardown.
- **Builder notes never reached reviewers.** The per-task re-review still got the full previous findings in a resumed
  session.

**[I] The literature, nöbetçi and InfraMedic converge on one result:** the knowledge that demonstrably works is
**mechanical** (code truth, gates, receipts, deterministic projections). The curated-text layers on all three sides
are either unmeasured or measured as no gain.

---

## 4. Side-by-side

| Concept | InfraMedic | auto-dev-cycle (proposal / predecessor) | Same abstraction? |
|---|---|---|---|
| Unit of knowledge | artifact → immutable content-addressed versions | "item" with provenance and validity fields | **Yes.** ADC should adopt artifact/version |
| Derived content | **representation** (extracted, sanitized…) with transform identity | dossier, digest, trail (derived from records/code) | **Yes**, but ADC's are *assemblies* (below), not 1:1 transforms |
| Consumer-specific assembly | session selection + whole injection under budget + `list_sources`/`search_sources` | "projection per role and model size" | **Yes.** **Name clash:** InfraMedic "projection" = search index; ADC "projection" = role view |
| Search index | gateway projection, fingerprint, `behind` | none in v1 (no vector DB) | Concept yes, implementation no |
| Publication / audience | append-only placement (channel + audience) | which role may receive an item; reviewer never gets builder notes | **Yes.** Audience = principal/group (InfraMedic) vs role × project (ADC) |
| Confidentiality | `data_scope` all-of, sensitivity floor, egress, `restricted` never to cloud | project data policy × source trust class × allowed destinations (D9, D28) | **Yes.** Same rule: filter at assembly time, before egress |
| Ground truth | code > gold > tool log | gates/receipts (code) > tests > findings citing lines | **Yes** (source-first) |
| One home per claim | epistemic-type homes; reference never copy | "one decision, one place"; lesson → mechanism | **Yes**, and ADC lacks it for knowledge |
| Change | supersede, revoke, tombstone; never mutate | supersede, never delete | **Yes** |
| Staleness | fingerprint of every input → `behind`, computed locally | code-anchor check (symbol/file gone → stale) | **Yes.** The anchor check is one fingerprint input (tree/symbol SHA) |
| Promotion | `knowledge-promote`: **human approver**, bound to exact SHAs, always | validator + independent model or supervisor; unattended | **Pattern yes, gate no.** The key conflict (§7.1) |
| LLM role | "never LLM automation at a knowledge pillar"; LLMs propose, explore, narrate | agents propose; trusted paths promote | Same direction; InfraMedic is stricter |
| Authority | selection grants no authority; "a stronger model widens nothing" | model output never acquires authority by entering a store; injected knowledge is advisory | **Yes** |
| Read accounting | retrieval audit with exact ids | per-call context metrics (sizes, reads, re-reads) without item identities | **Yes.** ADC lacks per-item identity, so E1 cannot attribute effects |
| Hypothesis accumulation | `PossiblePattern`: accumulating → possible → corroborated; negative knowledge | recurring defect classes; repair signatures | **Yes**, and ADC has no lifecycle for it |
| Learned text under evaluation | GEPA packs: holdout + human gate + shadow | playbook / role instructions; E3 | **Yes.** ADC has no holdout/shadow discipline for text yet |
| Producer identity | model profile + snapshot SHA; materiality | executors swapped; capability-bound roles | **Yes.** An ADC item written by model X must name X |
| Loops | four outcomes + `stale` / `interrupted` | four outcomes + reason codes | Same inheritance |
| Cross-boundary transfer | signed bundles, version envelope, IMPORT re-gates locally; master/mesh | cross-project lessons; open-source distribution | **Yes**, later |
| Human duties | 11 named recurring gates | supervisor jobs J1–J5 | Analogous; ADC's supervisor plays InfraMedic's "central engineer" |

---

## 5. The abstractable kernel (proposed)

Five entities, two records, twelve invariants, six interfaces. Everything else is domain.

### 5.1 Entities

```
Item            item_id, kind, epistemic_type, origin, created_at, retired_at
Version         version_id, item_id, content_sha, supersedes, producer{human|code|model(profile, snapshot)},
                sources[] (evidence pointers: commit, file:line, receipt, run, tool call), valid_from
Representation  rep_id, version_id, relation (original | derived), transform{name, version}, content_sha
Placement       placement_id, rep_id, tier (proposed | accepted | retired), audience (consumer scope),
                scope{applies_to: any_of, data_scope: all_of, sensitivity, egress}, gate_record_id
View            view_id, consumer, budget, manifest[(rep_id, content_sha)], omitted[(rep_id, reason)],
                fingerprint(inputs), state (ready | behind | failed)
```

### 5.2 Records

- **GateRecord:** actor, decision, the exact SHAs, the expected current state, the confirmation policy used.
- **ReadRecord:** consumer, view or selection manifest SHA, the returned ids, outcome, latency, a query digest.

### 5.3 Lifecycles

- **Placement:** `proposed → accepted → retired`. Promotion **appends** an accepted placement. It never edits the
  version.
- **Hypotheses**, InfraMedic's PossiblePattern generalised: `accumulating → possible → corroborated → (mechanised |
  stale | disproved)`. Membership is append-only, and negative outcomes are kept.

### 5.4 Invariants

1. The catalog is authoritative. Indexes and views are derived, re-creatable and never the only copy.
2. Versions are immutable. Supersede or retire; never mutate. Purge leaves a tombstone.
3. **Source-first.** An accepted item points to evidence that resolves now. Assertion without source is not admitted.
4. **Citation gate.** A model-produced claim survives only if each claim cites a source that resolves mechanically
   (InfraMedic's investigation gate). Uncited claims are dropped, not shown.
5. No model output promotes itself. Promotion is a gate record bound to exact SHAs.
6. Knowledge never carries authority. Selection, injection or citation grants no permission, and no authority
   computation reads model rank or size.
7. One home per epistemic type. Lower layers reference upper ones by id and never restate them.
8. Staleness is a fingerprint mismatch, computed locally from inputs: source SHA, code anchors, transform versions.
9. Views are assembled server-side from an authorized manifest. A model may request a source but cannot widen its
   selection.
10. Every assembly and read is recorded with exact identities. Cuts are named, never silent.
11. Data policy is applied at assembly, before egress.
12. Loops end in four outcomes. A missing input is `could_not_run`, never `empty`.

### 5.5 Interfaces

- `propose(item, version, sources) → proposed placement`
- `gate(decision, sha, expected_state) → GateRecord`
- `assemble(consumer, budget, policy) → View{manifest, bytes, omitted}`
- `read(selection, query, k) → hits + ReadRecord`
- `staleness() → [(item, cause)]`
- `export_bundle(selection) → signed bundle`, and `import_bundle(bundle) → proposed placements`. Import never lands
  as accepted.

### 5.6 Check

**[I]** Every row in the side-by-side table maps to one of these entities, records or invariants. Nothing in the
kernel names a device, a repository, a role, an RBAC principal, an embedder or a language.

---

## 6. What must not be abstracted (yet)

**Keep domain-specific:**
- InfraMedic's subject grammar (device, entity, relation), device-group audiences, fleet notes data-scoping.
- RBAC principals, quorum and declassification.
- Kobra session pins and the recent shelf.
- The governed MCP gateway binding.
- Redaction/remask pipelines.
- Multimodal doors.
- ADC's role taxonomy, task/plan/receipt records and toolchain-specific code indexes.

**Keep out of the kernel for now:**

| Item | Why |
|---|---|
| Vector/embedding retrieval and index generations | ADC v1 has no vector store; InfraMedic's own retrieval value is unmeasured |
| The master/uplink/mesh protocol | Nothing crosses yet. Specify only the bundle shape (version envelope, signature, import = proposal) |
| GEPA-style evolution | It needs gold. ADC has gold only once T1 exists (M2), and then only for builder tasks |
| A shared code library | Rule of three |

**The over-abstraction risk is concrete [F, I].** One knowledge sub-phase of InfraMedic is about 34 PRs plus
reviews, with no measured outcome effect. ADC's operator rejected the predecessor's acceptance machinery for exactly
this shape. If ADC adopts InfraMedic's doctrine wholesale, it imports that machinery. If InfraMedic generalises its
code, it freezes infra-specific choices into the "agnostic" layer.

**The kernel should be about one page of spec.** Its acceptance test: both platforms map onto it with no new tables
on the ADC side beyond the five entities.

---

## 7. Fleets of small and medium local models

### 7.1 Who curates and who promotes: the central conflict

**InfraMedic** is unambiguous [F]: "hierarchical knowledge is hierarchically gated with human approvals … NEVER LLM
automation at the knowledge pillars". Every promotion is a human act, and there are eleven standing human duties.

**ADC** wants zero rescues during unattended runs (D26). Its proposal allows promotion by a validator plus "an
independent model or the supervisor".

**[I] These need not conflict.** The rule both can share is that **promotion is never on the critical path of work.**
Proposals queue; work continues without them. The confirmation policy per knowledge kind is a setting, the same
pattern as D32:
- InfraMedic sets every kind to `human`;
- ADC defaults curated kinds to `independent_review`, with `human` available.

**The evidence that small models write poor memories and follow injected instructions far more (Haiku 80% vs Opus
0% in one study) argues against small models promoting anything.** A small model may *produce* candidates. Only
code, a strong independent model or a human may *accept* them.

### 7.2 What small models are good for in a knowledge network

**[F, I] They can be cheap, parallel candidate producers under a strict citation gate:**
- extraction from a handed-over artifact (InfraMedic's distillation pattern: fetch the real file, the LLM drafts, a
  human gates);
- triage and classification (D6/D34 shadow surfaces);
- hypothesis accumulation;
- failure-signature clustering.

**The evidence for small models as curators without a stronger checker is negative.** Dynamic Cheatsheet, MemBench,
and the Letta over-invocation result all point this way.

**For a fleet of local models, the abstraction that matters most is therefore not storage. It is the verification
ladder:**
1. schema validation;
2. citation resolution;
3. deterministic contradiction checks;
4. optional redundancy, i.e. agreement across independent small models (**not measured** in either platform);
5. a strong-model or human gate.

### 7.3 Projection by model size

- The **knowledge is the same; the view differs.**
- Small models get compact, pre-selected views whole-injected under a budget.
- Large models get the same core plus bounded `list`/`search` over an authorized manifest (InfraMedic 1.3-5).
- No model gets free browsing of the store.
- InfraMedic already records model profile and locality on the session context. It has no size-dependent view.
  Neither platform has measured size-dependent views. That is ADC's E1.

### 7.4 Trust per producer

- InfraMedic computes trust per `(model, run kind)` from rules and data.
- ADC computes source health from evidence.
- The kernel's `producer` field makes per-producer acceptance rates computable: how many proposals from model X
  survived the gate. That is the only trust signal a knowledge network of small models needs to start.

---

## 8. Recommendations

### 8.1 For InfraMedic's general framework

1. **Write the kernel as a domain-free spec before more knowledge code.** One page, plus conformance scenarios
   adapted from doctrine §15. Map each InfraMedic table and verb to a kernel element or mark it "domain".
2. **Measure before 1.3-8/1.3-9.** Run an ablation (knowledge on vs off) on narration/investigation/Kobra outcomes
   using the retrieval audit already built. Without it, index generations add machinery to an unmeasured path.
3. **Write the "fleet of small local LLMs" vision down as its own note.** State:
   - producer vs consumer roles by model size;
   - the verification ladder (§7.2);
   - how it relates to the mesh.

   Until then it is an intention that no design can be checked against.
4. Keep "never LLM automation at a pillar" as InfraMedic's **setting** of the shared confirmation policy, not as a
   kernel axiom. That lets ADC share the kernel.

### 8.2 For the ADC v1 knowledge layer

1. **Adopt the kernel's vocabulary and invariants from M0.** Use artifact/version/representation/placement/view and
   avoid the "projection" clash: in ADC, "view" means the role/size assembly.
2. **Derived first.**
   - Dossier, attempt records, pointer digests and the code index are *views*, with fingerprints and read records.
   - Every view records its manifest and omissions, so E1 can attribute effects per item. That is the gap in
     nöbetçi's metrics, which counted bytes and reads but not item identities.
3. **Curated minimal:** decisions and conventions only, source-first, one home per epistemic type.
   - **Lessons enter as hypotheses** (`accumulating → possible → corroborated`) fed by failure signatures.
   - Their accepted end state is a **mechanism** (test, gate, validator) delivered as a normal candidate, or retirement.
   - A lesson is not prose injected into prompts unless E1/E3 show prose helps.
4. **Hybrid storage, like InfraMedic.**
   - The catalog (versions, placements, gate and read records, fingerprints) lives in the controller's SQLite.
   - The payload stays with its owner: curated text as files in the project repo under a protected path, so forks and
     clones keep it; derived views are regenerated.
5. **Promotion as a candidate** through the existing merge queue and approval records, with a confirmation policy per
   kind. No separate pipeline.
6. **No vectors, no RBAC audiences, no redaction pipeline, no bundle protocol in v1.** The data policy at the gateway
   covers egress. Bundles come with cross-project transfer in v1.x.
7. **Experiments:**
   - E1 needs read records.
   - Add an InfraMedic-style **shadow** check for any curated text promoted into role instructions (E3), on T1
     holdout tasks.

### 8.3 A shared component

- **Now:** the kernel spec, as a neutral document plus JSON Schemas and conformance scenarios. One copy in each repo
  until a shared home exists (the operator's call).
- **Later:** the bundle/envelope format, when ADC cross-project transfer or InfraMedic's master starts.
- **Never by default:** a shared runtime library. Extract only by the rule of three.
- **Sequencing:**
  1. the spec alongside ADC's M0 schema notes;
  2. the InfraMedic mapping doc (no code);
  3. ADC implements in Go during M1–M2;
  4. revisit after ADC M6 (self-host) and InfraMedic Faz 2.

---

## 9. Open questions for the operator

| # | Question | Options | Recommended |
|---|---|---|---|
| 1 | Is the shared thing a spec, a library or a protocol? | spec · library · protocol · nothing | **Spec now; protocol later; library only by the rule of three** |
| 2 | Promotion gate for curated knowledge in ADC | always human (InfraMedic posture) · confirmation policy per kind, default `independent_review` · supervisor alone | **Policy per kind, default `independent_review`; promotion never blocks work** |
| 3 | Where does ADC curated knowledge live? | repo files only · SQLite only · **hybrid: catalog in SQLite, payload in repo under a protected path** | **Hybrid** |
| 4 | Are lessons prose or mechanisms? | prose playbook injected · **hypothesis → mechanism, prose only if measured** | **Hypothesis → mechanism** |
| 5 | Retrieval/RAG in ADC v1 | vector search · bounded `list`/`search` over an authorized manifest · whole injection only | **Whole injection for small models, bounded list/search for large ones, no vectors** |
| 6 | Should InfraMedic measure knowledge value before 1.3-8? | yes, an ablation first · no, build as planned | **Yes**. This is a recommendation to the InfraMedic session; this session does not touch InfraMedic |

---

## 10. Disagreements with the ADC proposal (`2026-10-01-memory-vs-fresh-context.md`)

1. **"Projection" is overloaded.** Use *view* for the role/size assembly and keep *index* for search.
2. **It lacks per-item read records.** Its metrics (first-turn size, carried share, re-reads) cannot say *which* item
   helped or hurt. InfraMedic's retrieval audit is the missing piece.
3. **It lacks one home per epistemic type.** Without it, decisions, conventions, lessons and plan text collide. That
   is the WORK_QUEUE 206 KB rot and the stale canon contradicting later changes.
4. **The code-anchor check is a special case of fingerprint staleness.** Generalise it; do not build it twice.
5. **"Independent model or supervisor" promotion** should be a confirmation policy per kind, not a fixed rule. And
   lessons should target mechanisms, not playbook prose, until E3 shows prose helps.
6. **"Frontier models get on-demand query tools"** must mean bounded tools over an authorized manifest (InfraMedic's
   `search_sources`), not free store browsing.
7. **It does not name the hypothesis lifecycle.** Without `accumulating / possible / corroborated / disproved` and
   negative knowledge, recurring-failure memory either becomes noise or is lost.

What it gets right and InfraMedic confirms: controller ownership, records over recollections, source-first, supersede
never delete, agents propose, the strongest memory is a mechanism, and measure or it is not real.

---

## Erratum (added after reading Astra's report, 2026-10-01)

§2.2 understated what InfraMedic has built. I took the status from roadmap rows that were stale; the code and build
records are newer:
- **Index generations are built.** `inframedic/knowledge/generation.py` has eight states, and activation/retirement
  are human verbs.
- **Installation identity is built** (`docs/notes/2026-09-19-1.4-9-installation-identity.md`), without sender code.
- **Principals and seam activation are built** (`docs/notes/2026-09-19-1.4-10-seam-activation.md`).
- **Declassification is not built** (`inframedic/auth/gates.py:243`).

This makes my §2.4 point stronger, not weaker: still more machinery, still no outcome ablation. It is also an
instance of the stale-summary failure the kernel's invariant 3 targets: a planning document read as truth after the
code had moved past it.
