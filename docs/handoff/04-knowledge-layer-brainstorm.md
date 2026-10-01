# Brainstorm brief: the auto-dev-cycle knowledge layer

Prepared 2026-10-01 by the planning session (Claude Opus 5.5) for a **separate Claude session** that runs a detailed
brainstorm with the operator. The operator asked for this session in their own words: "your proposition can be
improved and brainstormed."

## Your job

1. Read the context files below. Start with the research document.
2. Run an interactive brainstorm with the operator on the **knowledge layer**: how auto-dev-cycle stores, curates,
   projects and retires project knowledge for agents of different roles and sizes. Ask focused questions, with options
   and a recommended default (operator style: see below). Challenge the current proposal where the evidence allows.
3. Write the outcome to **`docs/plan/03-knowledge-layer.md`**: decisions, design, experiments, open questions.
   **Write only that file** (and scratch files in your own scratchpad). The planning session owns the other files in
   `docs/plan/` and has uncommitted work in this tree. Do not edit those files, do not commit, do not switch branches.
4. When done, tell the operator. The planning session will fold the result into the plan as a decision and send it to
   Astra for independent review.

## Context files (all in this repo)

| File | Why |
|---|---|
| `docs/research/2026-10-01-memory-vs-fresh-context.md` | **The starting proposal**: "controller-owned project knowledge", the evidence behind it, per-role projections, experiments E1–E5 |
| `docs/plan/00-brainstorm-decisions.md` | All operator decisions D1–D34 (fixed unless the operator reopens them) |
| `docs/plan/01-first-release-plan.md` | v1 architecture: capacity layer, executors, cycle, planning head, classifiers, background work, sandbox, authority, milestones |
| `docs/plan/02-supervisor-role.md` | The external supervisor AI: jobs, seat handover, notes/continuity stored in the controller |
| `docs/plan/reviews/2026-10-01-astra-plan-review.md` | Astra's independent review of the plan |
| `docs/handoff/00..03-*.md` | The predecessor framework (nöbetçi): architecture, lessons, invariants, planning model |

Predecessor sources, **read-only**: `/home/mega/nobetci-dev` (source), `/home/mega/nobetci` (runtime). Never modify
these, `/srv/nobetci` or `/home/mega/inframedic`. Never open credential or env files. Useful predecessor docs:
`docs/2026-09-20-context-audit.md`, `docs/2026-09-30-builder-exploration-review-cost.md`,
`docs/2026-09-23-repetition-reduction-plan.md`, `docs/WORK_QUEUE.md` (K-1..K-10, R52, R-3), the modules
`bin/nb_task_dossier.py`, `bin/nb_build_notes.py`, `bin/nb_build_trail.py`, `bin/nb_phase_digest.py`,
`bin/nb_context.py`, `bin/nb_canon.py`.

## Project in five lines

auto-dev-cycle v1 is an open-source, project-agnostic unattended development cycle. It has a Go core and TS executor
adapters, and harvests model capacity from any source: subscriptions, APIs, free tiers, Ollama, local GPUs, idle
machines through a dial-out agent. Roles are bound to capabilities, not models. Executors are swapped, drained and
replaced dynamically, so **knowledge cannot live inside any agent**. Strong cloud models (Opus 5.5, Astra) plan and
review. Cheap or small models (GLM-5.3-flash, Qwen3.8 27B, Ollama models) build. Jev/Kev classifiers are evaluated
in shadow mode. An external supervisor AI operates the system through the `adc` CLI.

## Current proposal (summary; details in the research doc)

- The controller owns knowledge; agents own nothing that survives them.
- Records, not recollections. Live state is regenerated from receipts and records.
- Source-first items with provenance and validity fields; supersede, never delete; code-anchor staleness checks.
- Projections per role and model size: small models get compact curated views; frontier models get the core plus
  just-in-time query tools.
- Agents propose; promotion needs a validator plus independent review; injected knowledge is advisory data.
- The strongest memory is a mechanism (lesson → gate/test/invariant).
- Every layer is measured with per-call context metrics; experiments E1–E5 on T1 decide what stays on.
- Not in v1: memory services, vector or graph DBs, self-editing memory, resumed sessions, harness-native memory as a
  source of truth.

## Questions worth brainstorming (starting points, not a script)

- **Knowledge types and schema.** Decisions/ADRs, conventions, lessons, defect classes, attempt records, code facts.
  What is the record schema? What are the validity fields? How do items relate to requirement ids, files and symbols?
- **Storage.** Git-tracked files in the project repo, in a separate knowledge repo, or in the controller's SQLite
  event store with exported views? Which survives multi-project use, forks, and open-source users?
- **Promotion pipeline.** Who proposes, which deterministic validators run, who reviews (supervisor? a different
  model family?), and how promotion relates to the authority/approval layer and the classifier shadow program.
- **Projection.** How is a role- and size-specific view computed? Ranking, budgets per model size, just-in-time query
  tools for frontier models. How do we avoid "instruction pollution"?
- **Staleness and forgetting.** Code anchors, unused decay, supersession, contradiction handling.
- **Cross-project knowledge.** Can lessons transfer between projects (e.g. framework-level defect classes) without
  leaking data across trust boundaries (D9, D28 usage profiles)?
- **Code-knowledge layer.** Repo map, LSP symbols, call graphs: which in v1 for Py/TS/Go/PHP?
- **Security.** Poisoning, injection through repository content, and the data policy for knowledge leaving the
  controller.
- **Supervisor knowledge.** The relation to `02-supervisor-role.md` notes and handovers.
- **Experiments.** Refine E1–E5; acceptance thresholds; which milestone each lands in.
- **Fit with the dynamic framework.** Executor swaps mid-task, drain/resume checkpoints, parallel units, the merge
  queue, background work.

## Operator style

- The operator writes in Turkish, often briefly, and is fine with English replies and English documents.
- They decide quickly between well-argued options that come with a recommended default.
- Their examples express an **intention**. Confirm the intention before designing around an example.
- They want measurements, not adjectives, and they reject complexity creep. The predecessor's acceptance machinery
  grew until its own stops exceeded real blockers. Keep the knowledge layer as simple as the evidence allows.
