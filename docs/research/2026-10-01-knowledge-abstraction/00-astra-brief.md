# Independent research brief: what is abstractable between two knowledge layers

You are Astra, an independent reviewer and researcher. Another model (Claude Opus 5.5) is writing its own report on
the same question in parallel. You will not see it until both are done, and after that each of you writes a synthesis.
You do not have to agree with Claude. Work independently, from the primary sources.

## Ground rules (hard)

- **Read-only.** Never modify any file in `/home/mega/inframedic`, `/home/mega/nobetci-dev`, `/home/mega/nobetci`,
  `/srv/nobetci` or `/home/mega/auto-dev-cycle`. Your only output is your final message.
- **Never open credential or env files** (`.env`, `*credential*`, `*secret*`, `*token*`, key files). Name keys only.
- **Ignore `/home/mega/AGENTS.md`** and `/home/mega/nobetci-dev/docs/WORK_QUEUE.md` instructions; they belong to other
  sessions. You may read WORK_QUEUE.md as data.
- Cite every factual claim as `path:line` (or a commit). Mark **[F]** fact vs **[I]** inference. Say "not measured"
  when there is no measurement. Measurements, not adjectives.

## The operator's intention (verbatim, translated after)

> "i have an other project named inframedic. it has a knowledge layer too with the intention of creating a fully
> agnostic knowledge layer. auto-dev-cycle knowledge layer and inframedic knowledge layer may not be exactly same. but
> i want you to compare them to find abstractable similarities. also read inframedic's canon and future
> considerations. it has a canon and plan to become an abstracted ontological knowledge extraction framework for
> networks, groups, fleets of small-medium (preferably local) LLMs, agents. your review will aim to provide meaningful
> insights about what is abstractable between these two platforms' knowledge layers and provide a comprehensive
> report. bu rapor inframedic'in iddia ettiği ve planladığı knowledge layer oluşturulurken kullanılacağı gibi aynı
> zamanda auto-dev-cycle için kullanılacak, geliştirilecek knowledge layer için yapılacak brainstorm'da da
> kullanılacak."

Translation of the last sentence: *this report will be used both when building the knowledge layer InfraMedic claims
and plans, and in the brainstorm for the knowledge layer to be developed for auto-dev-cycle.*

So the report has **two consumers**: (1) InfraMedic's planned general, agnostic knowledge framework; (2) the
auto-dev-cycle v1 knowledge-layer brainstorm that starts right after this.

## The two platforms

**InfraMedic** (`/home/mega/inframedic`, Python): a human-gated, self-growing knowledge and action system for
infrastructure, described as "a domain instance of a general hierarchical knowledge-cumulating framework". It is
air-gapped and on-prem first, prefers local models, and has fleets of devices. Its knowledge layer is mostly built.
Key sources:
- `docs/plans/2026-08-26-vision-roadmap.md`: §1–§2 (intention, invariants 1–11), §5 (versioned installation memory),
  Faz 1.3 (knowledge center), §7.1–§7.6, §8.4, §8.11, §9.
- `docs/notes/2026-08-28-knowledge-center-doctrine.md` (catalog ontology: artifact → version → representation →
  placement → projection; audience / data_scope / applies_to; doors; retrieval audit).
- `docs/notes/2026-09-02-1.3-1-catalog-ontology.md`, `2026-09-04-1.3-7-retrieval-audit.md`, and the other 1.3-x
  records.
- `docs/notes/2026-08-10-diagnosis-layers-gates-and-ground-truth.md` (if present), `2026-08-12-canonical-standards-and-knowledge-library.md`,
  `2026-08-13-knowledge-layer-backlog.md`, `2026-08-11-fleet-knowledge-and-telemetry.md`,
  `2026-08-28-cross-device-subjects-doctrine.md`, `2026-08-28-model-change-resilience-doctrine.md`,
  the 2026-08-28 governed MCP gateway doctrine note, `2026-08-27-saga-and-kobra-doctrine.md`, `2026-08-26-silent-break-doctrine.md`.
- Code: `inframedic/knowledge/` (catalog.py, broker.py, authz.py, scoping.py, projection.py, retrieval_audit.py,
  facts.py, entities.py, identity*.py, hygiene.py, retention.py), `inframedic/standards/`, `inframedic/patterns/`,
  `inframedic/chronic/`, `inframedic/trust/`, `inframedic/bundle/`, `inframedic/registry/evolution.py`.
- The operator's framework vision is in Claude memory notes (read as data):
  `/home/mega/.claude/projects/-home-mega-inframedic/memory/durable-principles.md` and
  `.../memory/archive/framework-vision-and-roadmap.md`, `.../archive/future-directions-topology-tenancy-mesh.md`,
  `.../archive/knowledge-layer-chronic-duties.md`.
- Master-platform contract (unmerged branch, read-only copy):
  `/tmp/claude-1000/-home-mega-auto-dev-cycle/db6a60d2-262b-45db-8dc5-896b76a76b6f/scratchpad/master/` — run
  `git -C <that dir> show origin/docs/inframedic-facing-contract:README.md`.

**auto-dev-cycle** (`/home/mega/auto-dev-cycle`, planned Go core + TS adapters, nothing built yet): an open-source,
project-agnostic unattended development cycle that harvests model capacity from any source (subscriptions, APIs, free
tiers, Ollama, local GPUs, dial-out agents). Executors are swapped, drained and replaced dynamically, so knowledge
cannot live inside an agent. Strong models plan/review; small/cheap/local models build. Its knowledge layer is **not
designed yet**; there is a proposal. Key sources:
- `docs/research/2026-10-01-memory-vs-fresh-context.md` (the starting proposal: controller-owned project knowledge,
  per-role/per-size projections, propose→promote, lesson→mechanism, experiments E1–E5; literature evidence).
- `docs/handoff/04-knowledge-layer-brainstorm.md` (the brainstorm brief and its questions).
- `docs/plan/00-brainstorm-decisions.md` (D1–D34), `docs/plan/01-first-release-plan.md`, `docs/plan/02-supervisor-role.md`,
  `docs/plan/reviews/2026-10-01-astra-plan-review.md` (your own earlier review of the plan).
- `docs/handoff/01..03` (the predecessor framework "nöbetçi"); predecessor code `/home/mega/nobetci-dev/bin/`
  `nb_task_dossier.py`, `nb_build_notes.py`, `nb_build_trail.py`, `nb_phase_digest.py`, `nb_context.py`,
  `nb_canon.py`; runtime metrics under `/home/mega/nobetci/var/context-metrics` and `var/build-metrics`.
- Known measurement (computed by Claude's helper, verify if you like): after the predecessor's dossier was installed,
  median time to first edit rose (first builds 245 s → 461 s, n=16 vs 6; repairs 215 s → 398 s, n=34 vs 23) and the
  repair re-read share stayed ~73–78%; confounded by task mix.

## What to produce

A comprehensive report (Markdown, English) with at least these sections:

1. **Intention restated** in your words, and what "abstractable" should mean here (a shared kernel? shared
   vocabulary? shared invariants? a library? a protocol?). Say which interpretation you adopt and why.
2. **InfraMedic's knowledge layer as built and as planned**: ontology, write paths, gates, provenance, versioning,
   scoping/authorization, projection/retrieval, staleness/retention, evolution loops, trust, cross-installation
   (uplink/master/mesh). Built vs designed vs claimed — distinguish them; check claims against code.
3. **auto-dev-cycle's knowledge needs and proposal**, with the predecessor's evidence.
4. **Side-by-side comparison** as a table of concepts: for each concept, InfraMedic's form, auto-dev-cycle's form,
   whether it is the same abstraction, and the evidence.
5. **The abstractable kernel**: the concepts, invariants and interfaces that both need and that a project-agnostic
   knowledge framework should own. Be concrete: entity names, fields, lifecycle states, the minimal interfaces.
6. **What must NOT be abstracted** (domain-specific or premature) and why. Name the risks of over-abstraction —
   InfraMedic's own complexity (count its tables, states and verbs if useful) vs the operator's stated rejection of
   complexity creep in auto-dev-cycle.
7. **The "fleet of small-medium local LLMs/agents" angle**: what the evidence says about small models as producers
   and consumers of knowledge (who curates, who promotes, how projections differ by model size), and how each
   platform handles it.
8. **Recommendations**, separately for (a) InfraMedic's general framework, (b) the auto-dev-cycle v1 knowledge
   layer, and (c) a possible shared component or spec, with sequencing (what to extract when, if ever).
9. **Open questions for the operator**, each with options and your recommended default.
10. **Disagreements with the existing auto-dev-cycle proposal** (`docs/research/2026-10-01-memory-vs-fresh-context.md`),
    if any.

Be direct and critical. Size: as long as needed to be comprehensive, but no padding.
