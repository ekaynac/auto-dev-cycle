# Review brief for Astra: nöbetçi delta intake (contract PR)

You are Astra, an independent reviewer from a different model family than the author (Claude Opus 5.5). This is a
read-only design review: do not modify files.

**Context.** The nöbetçi supervisor wrote four reports on 2026-10-03 (read-only:
`/home/mega/nobetci-dev/docs/handoff/2026-10-03/05..08-*.md`). The operator asked the planning session to filter them
into the auto-dev-cycle plan, with the instruction "we don't have to take everything; you are the filter".

**Under review:** the branch diff, `git -C /home/mega/auto-dev-cycle diff origin/main...feature/plan/nobetci-delta-intake`:
- `docs/research/2026-10-03-nobetci-delta-intake.md`, the filter record: accepted A1–A14, deferred, not taken;
- `docs/design/m0/12-evidence-jobs.md` (new);
- edits to `docs/design/m0/00,02–07,09–11`;
- edits to `docs/plan/01-first-release-plan.md`, `02-supervisor-role.md`, `04-development-workflow.md`,
  `00-brainstorm-decisions.md` (D39).

**Authoritative context:** the merged plan, decisions D1–D38, and the M0 notes on `main`, including your previous
reviews (`docs/plan/reviews/`, `docs/design/m0/reviews/`).

**Check:**
1. **Filtering.** Was anything important in the reports wrongly deferred or rejected? Was anything taken that adds
   machinery without proportional value (complexity creep)?
2. **Faithfulness.** Do the edits represent the reports' lessons correctly?
3. **Consistency** with the existing contracts. Look especially at:
   - owed inputs vs authority and approvals (04) and fingerprints (10);
   - evidence jobs (12) vs receipts (06), effects (03), attempts (05);
   - strict options vs the simple gate (plan §2.6) and independence (06 rule 3);
   - `inputs_applicable` and `fallback_scope` placement checks;
   - the broker-balance group.

   Does anything get decided twice?
4. **Enforceability.** Fail-open paths in the new contracts.
5. **Fixtures:** are the new contract fixtures sufficient, and does each prove its red?

**Output:** a one-line verdict (GO / GO with changes / NO-GO), then findings by severity (Critical/High/Medium/Low), each
with location, problem, why and a concrete change. Be terse. Block only on Critical/High.
