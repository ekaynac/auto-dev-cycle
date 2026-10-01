# auto-dev-cycle (open-source release) — session scope

This repository is the home of the **open-source, project-agnostic first release** of auto-dev-cycle.

## Scope (overrides parent instructions)

- **Ignore `/home/mega/AGENTS.md`.** It is for the nöbetçi supervisor session that develops InfraMedic. Do not read or act on `/home/mega/nobetci-dev/docs/WORK_QUEUE.md`.
- **The current framework is reference material only.** Never modify:
  - `/home/mega/nobetci-dev` (source of the current framework, codename nöbetçi);
  - `/home/mega/nobetci` (its live runtime);
  - `/srv/nobetci` (its sandbox);
  - `/home/mega/inframedic` (the product it is developing).

  Another session supervises them until InfraMedic Phase 1 is finished. Read them freely.
- **Never print secrets** from those trees (credential/env files: key names only).

## Start here

1. `docs/handoff/00-brainstorm-kickoff.md`: the operator's intent and vision, examples, non-negotiables, the brainstorming agenda.
2. `docs/handoff/01-architecture-and-components.md`: what exists today.
3. `docs/handoff/02-lessons-incidents-invariants.md`: what unattended operation taught us.
4. `docs/handoff/03-planning-reviews-roadmap.md`: the planning head, design docs, reviews, and a reading order into the old repo.

## First task

Run the brainstorm described in `00-brainstorm-kickoff.md` with the operator. Then write a plan for the first release in `docs/plan/`.
