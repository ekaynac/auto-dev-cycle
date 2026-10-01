# Claude's synthesis of the two knowledge-abstraction reports

Date: 2026-10-01. Inputs:
- `01-claude-report.md`, my report with its erratum;
- `02-astra-report.md`, Astra's independent report.

Astra writes its own synthesis (`04-astra-synthesis.md`) in parallel. I do not have to agree with it.
**[F]** marks a sourced fact; **[I]** my judgement.

---

## 1. The consensus core (ready to decide)

The two reports were written independently, from the same sources, and reached these conclusions separately.

1. **Share a specification, not a library.** Shared vocabulary, invariants and conformance fixtures now. A runtime
   library only after two working implementations show measurable duplicated maintenance. An exchange protocol only
   when a real exchange need exists.
2. **InfraMedic has built a governance ontology for information artifacts, not a general ontology-extraction
   framework.** The catalog (artifact → version → representation → placement → projection) is close to domain-free.
   The subject grammar, fact extraction, scoping by identity scan, chronic and pattern rules are infrastructure
   semantics.
   - The "fleets of small/medium local LLMs" framework is an intention with no design document.
3. **"Projection" names two different things.** InfraMedic: a derived search index. ADC: a consumer-specific context
   assembly. Keep *index* for the first; call the second a **context manifest / view**.
4. **Record what a consumer actually received, including what was omitted.** InfraMedic's retrieval audit records
   retrieval. ADC's metrics record sizes and reads. Neither records per-item delivered context.
   - This **context manifest + use receipt** is the single most useful abstraction for ADC.
   - Without it, E1–E3 cannot attribute any effect to any item.
5. **Scope dimensions stay separate:** relevance (`applies_to`), access (audience), whose information it is
   (`data_scope`), and where it may be sent (sensitivity/egress).
   - Applicability never grants access.
   - Every exposure re-checks access and destination. For ADC this matters most on fallback from a local model to a
     remote one.
6. **Knowledge never carries authority.** Selection, injection, citation or a stronger model widens nothing. Both
   platforms already state this. It belongs in the kernel.
7. **The decision-maker is policy, not kernel.** InfraMedic's "a human at every pillar" and ADC's "independent
   review under a grant" are two settings of one confirmation policy per knowledge kind.
8. **Do not import InfraMedic's machinery into ADC v1:** placement channels, index generations, ingestion doors,
   redaction, the fleet registry, the bundle network, the mesh.
9. **Measure outcomes, not proxies.** Neither platform has a knowledge-on vs knowledge-off measurement. The
   predecessor's before/after dossier comparison is unmatched and establishes nothing.
   - Astra recomputed it independently: first builds 245 s → 461 s (n=16/6); repairs 215 s → 421 s (n=33/24);
     re-read share 72.6% → 78.5%.
10. **Recurring knowledge should become a separately reviewed mechanism** (test, gate, validator), not accumulate as
    prose.

---

## 2. Disagreements and my verdicts

| # | Topic | Claude (report) | Astra (report) | Evidence | Verdict | What settles it |
|---|---|---|---|---|---|---|
| 1 | Lifecycle | One placement tier `proposed → accepted → retired`, plus a hypothesis lifecycle | Five independent dimensions: decision, validity, publication, materialization, retention | InfraMedic itself keeps them separate: placement state, projection state, generation state, retention, disputes (Astra §2.6, §6.2) | **Concede.** One state machine conflates "accepted", "current" and "published". **Cut for ADC v1:** materialization (no index) and a separate publication (consumer class is a field) | — |
| 2 | Retention | "Supersede, never delete" (inherited from the ADC proposal) | Policy retention with tombstones; never silently rewrite accepted history | InfraMedic purges private payloads with tombstones (`retention.py:91`) | **Concede.** ADC holds attempt payloads that may contain project data under a retention setting | — |
| 3 | Attempt notes | Proposal: builders never get "unpromoted agent notes" (I did not catch the contradiction) | Separate **ephemeral attempt continuity** (same-task, untrusted, revision-bound) from **durable knowledge** | The proposal's own builder row lists attempt records *and* forbids unpromoted notes (`memory-vs-fresh-context.md:198`); nöbetçi keeps untrusted notes across interrupted attempts | **Concede; important.** Drain/resume (plan §2.5) needs it | E1 arm "attempt records" |
| 4 | Staleness | Fingerprint over inputs; the code anchor is one input | Deterministic ≠ current; "anchor resolves" means locatable, not valid | Symbols survive behaviour changes (Astra §10.7) | **Merge.** Fingerprint = exact tree + extractor version, so derived views are never stale by construction. Curated items carry validity conditions; an anchor change triggers re-validation, not truth | E5 with semantic-change fixtures (Astra's case: unchanged symbol name, changed behaviour) |
| 5 | Model-size views | Same knowledge, different view; small = whole injection, large = bounded list/search | Size is one input; use measured capability profiles; small models may use lookup tools | Neither platform measured size-dependent views | **Concede the principle, keep the default.** Start with simple presets, make eligibility empirical | E1 per size class with a lookup-tool arm |
| 6 | Reviewer exposure | Pointer digest | Pointers can carry judgement (grouping, statuses); test **staged disclosure**: fresh assessment first, then prior findings | `nb_phase_digest.py:128` carries claim groupings and statuses | **Concede.** Add staged disclosure as an E2 arm | E2: fresh-only vs pointer vs staged |
| 7 | Success criterion | Resolve rate, first edit, reads, cost | **Correct accepted work per total cost**, with defect-escape and isolation guardrails; first edit is diagnostic | The dossier's "first edit got worse" is uninterpretable without outcomes | **Concede** | — |
| 8 | Curated knowledge in v1 | Minimal curated tier (decisions, conventions); lessons as hypotheses → mechanisms | Contracts in M0–M1, ablations in M2, a minimal accepted-knowledge collection **only after demonstrated need** | No curated-text layer has a measured gain on any side | **Merge, closer to Astra.** Details below |
| 9 | Kernel size | 5 entities, 2 records, 12 invariants, 6 interfaces | 8 entities, 5 dimensions, 14 invariants, 8 interfaces + 6 adapters | Both are larger than ADC v1 needs; the operator rejects complexity creep | **Neither as written.** A two-level kernel: the full spec for InfraMedic and future consumers, and an ADC v1 profile of four records (§4) | Mapping both platforms onto the spec without new ADC tables beyond the profile |
| 10 | Hypothesis lifecycle | `accumulating → possible → corroborated → mechanised / stale / disproved` for recurring failures | Negative knowledge scoped to conditions; a rejected hypothesis can be reconsidered when conditions change | PossiblePattern (cross-device §5.3–5.4) | **Hold, in narrowed form.** In ADC it is a derived *failure-signature* count, not a curated item. Crossing a threshold proposes a mechanism task to the planner. No separate lifecycle table | — |
| 11 | Storage of curated payloads | Hybrid: catalog in SQLite, curated text as repo files under a protected path | SQLite/event substrate; reference receipts, never copy | Both agree on "reference, don't copy" | **Open: for the operator.** Repo files suit forks and open-source users; SQLite suits validity tracking | Operator decision (agenda Q5) |
| 12 | Derivation identity | Not addressed | Content dedup must not erase the production event (`catalog.py:718`) | A real provenance gap in InfraMedic | **Concede for InfraMedic; minor for ADC** (views are records, not deduplicated) | — |

---

## 3. What each report missed

**What Astra found that I missed:**
1. The stale status of InfraMedic's roadmap rows. Generations, installation identity and principals are built. I
   corrected this in my erratum.
2. Declassification is not built (`auth/gates.py:243`).
3. Claim kinds: observation, external fact, inference, normative rule, procedure. Each needs a different promotion.
4. Acceptance is "for a stated purpose".
5. The four distinct times: observed, recorded, valid-from/until, conditions.
6. The internal contradiction in the ADC proposal's builder row.
7. The predecessor trail already includes shell reads. The research doc's claim is outdated (`nb_build_trail.py:1`).
8. Evaluation guardrails: escaped defects, curator time, recovery after replacement, destination-policy failures.
9. Ten concrete conformance fixtures (Astra §8.3), including the fenced late executor and the local→cloud fallback
   meeting restricted history.

**What I found that Astra under-weighted:**
1. **The cost side.** About 34 PRs, plus reviews, for one InfraMedic knowledge sub-phase. 60 `rag.read` ever before
   Faz 1.3. No outcome ablation. Astra counted tables and states but did not set them against use.
2. **The citation gate** (InfraMedic's investigation rule: an uncited claim is dropped) as the main admission
   mechanism for model-produced knowledge, especially from small models.
3. **One home per epistemic type**, with reference-never-copy mechanically enforced (08-12 canon §7). It is the cure
   for the predecessor's 206 KB WORK_QUEUE and its stale-canon contradictions. Astra's claim kinds are the *types*;
   the single home per type is the *rule*.
4. **GEPA's discipline as the template for any learned text** in ADC: holdout, human gate, shadow. If ADC ever
   promotes playbook text into role instructions, it must pass the same three.
5. **Lessons → mechanisms are the only knowledge with a track record.** That is the only fully positive result on
   either side (nöbetçi's write guard, single `busy()`, repair signature; InfraMedic's detectors).

**Fact check of Astra [F]:** I checked the dossier figures, the generation and identity records, and the retention
behaviour; they hold. Its post-install repair median (421 s, n=24) differs from my helper's (398 s, n=23) only
because its snapshot was later and it classified "first call" differently. Both say the same thing: unmatched,
inconclusive. I found no factual error in Astra's report.

---

## 4. The merged kernel

### 4.1 Level 1: the shared specification

For InfraMedic, ADC and future consumers. Language-neutral, with JSON Schemas and conformance fixtures.

| Element | Content | Source |
|---|---|---|
| `Ref` | artifact id, kind, origin, owner; exact revision (content digest, source revision, supersedes) | both |
| `Derivation` | inputs, transform identity + version, optional model snapshot, run id | Astra |
| `Claim` | revision ref, **claim kind** (observation · external fact · inference · rule/decision · procedure), typed body, subjects, supporting/refuting evidence refs, applicability, validity conditions, times (observed, recorded, valid from/until) | Astra + Claude |
| `Decision` | target SHA, **purpose**, outcome, actor, policy revision, expected prior state, evidence | both |
| `Exposure` | access/destination scope (audience, data scope, sensitivity, egress) and publication/revocation | InfraMedic |
| `ContextManifest` + `UseReceipt` | consumer profile, source revision, policy revision, included refs, omitted refs + reasons, render digest, budget; actual delivery, outcome, measurements | both |

**Lifecycle dimensions, kept independent:**
- decision: `proposed | accepted(purpose) | rejected | disputed | superseded`;
- validity: `current | stale | unknown | expired` + reason;
- materialization: `absent | building | ready | failed | behind`;
- retention: `retained | held | purged + tombstone`.

**Invariants (10), merged from both lists:**
1. Writing text never creates authority. Knowledge carries no permission.
2. A decision binds exact content, purpose, policy and expected state.
3. Acceptance, publication, validity and authority are separate.
4. Source-first: a model-produced claim is admitted only if every claim cites evidence that resolves now (the
   **citation gate**).
5. One home per claim kind. Reference, never copy.
6. Scope is conservative across derivations, and applicability never grants access.
7. Every exposure re-checks access and destination, and pinned refs never silently change version.
8. Empty, unavailable, unauthorized, stale and truncated are distinct outcomes. Omissions are named.
9. History is never silently rewritten. Deletion is a policy act that leaves a tombstone.
10. Foreign or imported acceptance is never local acceptance. Late or fenced producers cannot mutate current state.

**Interfaces:** `record`, `propose`, `decide`, `resolve`, `assemble → ContextManifest`, `record_use`,
`invalidate_or_retire`. Plus adapters for subjects, authorization/destination, promotion policy, validity checks and
payload access.

**Cut from both lists for complexity:**
- a separate `Publication` entity (folded into `Exposure`);
- a separate `KnowledgeItem` vs `Claim` (one entity);
- a hypothesis lifecycle (a derived count, §2 row 10);
- `export_bundle` / `import_bundle` (protocol, later);
- invariants about derivation dedup (an InfraMedic implementation note).

### 4.2 Level 2: the ADC v1 profile (what ADC actually builds)

ADC v1 adds **at most four record types** to the event store it already plans. Everything else reuses records the
plan already has: receipts, approvals, attempts, plan revisions.

| ADC v1 record | Is the kernel's | Notes |
|---|---|---|
| `ContextManifest` (with use receipt fields) | ContextManifest + UseReceipt | **M0 contract.** Written for every model call. Feeds E1–E3. Removes the predecessor's blind spot |
| `AttemptRecord` | Ref + Derivation (ephemeral continuity class) | Sealed at drain or interruption: diff, failing tests, untrusted notes, the trail. Same task only, revision-bound. Never auto-loaded elsewhere |
| `Claim` (durable) | Claim + Decision | **Only after M2 shows need.** Kinds: decision · convention · verified lesson · scoped negative result. Citation gate on admission. Confirmation policy per kind |
| `FailureSignature` | derived Ref | A count of identical normalized failures across tasks. Threshold → proposes a mechanism task. No prose |

Derived views (dossier, code index, pointer digests) are not records. They are regenerated from the tree and the
receipts. Each carries a fingerprint (tree SHA + extractor version) inside its manifest entry.

---

## 5. Recommendations

### 5.1 InfraMedic's general framework

1. State precisely what is built: a governed artifact catalog plus infrastructure-specific knowledge systems.
2. Write the Level 1 spec as a domain-free note. Map every knowledge table and verb to it or mark it "domain".
3. Add the consumer manifest beside the retrieval audit: what was delivered after budgeting, not only what was
   retrieved.
4. Run a knowledge-on/off ablation on narration, investigation and Kobra before the next knowledge investment.
5. Write the "fleet of small/medium local LLMs" vision as its own note: producer/consumer roles by measured
   capability, and the verification ladder (schema → citation → deterministic contradiction → optional redundancy →
   strong-model or human gate).
6. Fix derivation identity under dedup.
7. Move scope propagation toward lineage, keeping identity scanning as supplementary evidence.

This report does not authorise any change to InfraMedic. These are inputs for the InfraMedic session.

### 5.2 auto-dev-cycle v1

- **M0:** the `ContextManifest` schema, exact refs and revisions, decision binding (the existing approval records),
  scope labels.
- **M1:** `AttemptRecord` sealed on drain and interruption; resume from it; fenced late producers rejected.
- **M2:**
  - T1 ablations: memory-off · dossier · + code index · + attempt records · + small curated lessons. Measured as
    correct accepted work per total cost, with defect and isolation guardrails.
  - E2 with a staged-disclosure arm.
  - E5 with semantic-change fixtures.
- **After M2, only on demonstrated need:** the durable `Claim` collection. Until then, durable project knowledge is
  what already exists: approval and decision records, plan revisions and the project profile's conventions.
- **Mechanisms over prose:** `FailureSignature` thresholds propose mechanism tasks. No lesson text is injected into
  role prompts unless E3 with holdout and shadow shows a gain.

### 5.3 A shared spec

**Now:**
- the Level 1 spec + JSON Schemas + Astra's ten conformance fixtures;
- one copy per repo until the operator picks a home.

**Later:** the bundle/exchange format, when ADC cross-project transfer or InfraMedic's master starts.

**A library:** only after two working adapters show measured duplicate maintenance. The first candidate is
schema-validation and fixture tooling, not storage.

---

## 6. The brainstorm agenda I recommend (in order of design impact)

| # | Question | Options | My default |
|---|---|---|---|
| 1 | What is the knowledge layer's success criterion? | less context · faster first edit · **correct accepted work per total cost, with defect/isolation guardrails** | **The last** |
| 2 | Which information classes exist, and which ship in v1? | one "memory" store · **four classes: authoritative state, regenerated views, ephemeral attempt continuity, durable knowledge** | **Four classes; v1 ships the first three plus the manifest** |
| 3 | Durable curated knowledge in v1? | full playbook · minimal typed claims from the start · **only after M2 ablations show need** | **After M2**; until then use existing decision records and profile conventions |
| 4 | Who accepts durable claims? | always human · always an independent model · **confirmation policy per claim kind** | **Policy per kind; promotion never blocks work** |
| 5 | Where do durable claims live (when they exist)? | repo files under a protected path · controller SQLite · hybrid (catalog in SQLite, text in repo) | **Hybrid**. Astra leans SQLite; operator's call |
| 6 | How does a reviewer see prior rounds? | fresh-only · pointer digest · **staged disclosure** | **Test all three in E2**; ship pointer + staged as candidates |
| 7 | Do lessons become prose or mechanisms? | injected playbook prose · **failure signature → mechanism task; prose only via holdout + shadow** | **Mechanisms** |
| 8 | Shared spec with InfraMedic: where and when? | now in both repos · a neutral repo · later | **Now, one copy per repo**, revisited after ADC M6 |
