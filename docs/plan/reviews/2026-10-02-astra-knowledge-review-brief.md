# Review brief for Astra: the knowledge layer and how it is folded into the v1 plan

You are Astra, an independent reviewer (different model family from the author, Claude Opus 5.5). This is a
read-only **design review**: do not modify any file.

## Background

- You reviewed the v1 plan on 2026-10-01 (`docs/plan/reviews/2026-10-01-astra-plan-review.md`, GO with changes). The
  plan was revised to apply all your High findings.
- A separate brainstorm with the operator then designed the **knowledge layer**: how auto-dev-cycle stores, curates,
  projects and retires knowledge for agents of different roles and sizes. You contributed an independent report and
  a synthesis comparing it with InfraMedic's knowledge layer (`docs/research/2026-10-01-knowledge-abstraction/
  02-astra-report.md`, `04-astra-synthesis.md`).
- The operator then chose decisions K1–K12 (`docs/plan/03-knowledge-layer.md`), and the planning session folded them
  into the plan as D35 and §2.15.

**Your job now:** review the brainstorm's *outcome* and its *integration*, not re-run the comparison. You took part in
the input stage, so be explicit where you are checking your own earlier recommendations.

## Files (all under /home/mega/auto-dev-cycle/docs)

Under review:
1. `plan/03-knowledge-layer.md`: decisions K1–K12, information classes, records, acceptance policy, the K11
   guardrails, experiments E0–E5, milestone placement, open questions.
2. `plan/01-first-release-plan.md`: §2.15 and the milestone rows M0/M1/M2, which carry the fold-in; open item O6.
3. `plan/00-brainstorm-decisions.md`: D35.

Context:
- `research/2026-10-01-memory-vs-fresh-context.md`: the starting proposal and evidence.
- `research/2026-10-01-knowledge-abstraction/01..04`: the two reports and two syntheses.
- `plan/02-supervisor-role.md`: supervisor notes, continuity and curation jobs.
- `plan/reviews/2026-10-01-astra-plan-review.md`: your earlier review.
- `handoff/00..03`: predecessor (nöbetçi) know-how.
- Read-only predecessor sources: `/home/mega/nobetci-dev`, `/home/mega/nobetci`. Do not open credential or env files.

## Fixed by the operator

K1–K12 as written, including **K11** (framework lessons shared across projects in v1). On K11 the operator overrode
the recommended strict isolation. Flag only concrete defects these choices cause, and propose the smallest fix that
keeps the operator's choice.

## Review focus

1. **Consistency.** Do `03-knowledge-layer.md`, §2.15, D35 and the rest of the plan agree? Pay particular attention to:
   - authority (§2.11);
   - data trust and usage profiles (D9, D28);
   - drain/resume and fencing (§2.5);
   - the merge queue;
   - host-side harnesses (§2.4);
   - the supervisor role.

   Is anything decided twice in different places ("one decision, one place")?
2. **Complexity creep.** Is v1's knowledge footprint minimal? Look at:
   - `ContextManifest` in M0;
   - `AttemptRecord`;
   - `FailureSignature`;
   - deferred `Claim`;
   - the shared spec (K10) in M0.

   Does any of it risk becoming a second acceptance machine? What can be cut or deferred?
3. **K11 guardrails.** Are the framework-scope validator and the exposure checks enforceable, or are they labels?
   Leakage paths across projects and usage profiles (on-prem)?
4. **Experiments.** Are E0–E5 fair, feasible on T1, and decisive? Is K1 measurable?
5. **Milestone fit.** Do the M0/M1/M2 additions respect the dependency order you asked for in your first review?
6. **Security.** Poisoning and injection via attempt continuity, claims, the shared spec or framework lessons.
7. **Open questions** (`03` §10, O6): give a recommendation for each where the evidence allows.

## Output format

- A one-line verdict: **GO**, **GO with changes**, or **NO-GO** for adopting the knowledge layer into the v1 plan.
- Findings ordered by severity (**Critical / High / Medium / Low**). For each: exact location (file + section), problem,
  why it matters (evidence), and a concrete change.
- Recommendations on the open questions.
- Be terse. Block only on Critical/High.
