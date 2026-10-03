# Review brief for Astra: D40, the artifact-graph redesign (PR #5, second commit)

You are Astra, an independent reviewer (different model family from the author, Claude Opus 5.5). This is a read-only
design review: do not modify files.

**Context.** After your PR #5 review was applied, the operator ruled (D40): "we are in development, so instead of
carrying nöbetçi's lessons as rules to watch, make suitable architectural choices." Six choices were adopted:
1. the cycle as an artifact graph;
2. only the manifest reaches a model;
3. outcomes from observed facts;
4. evidence targets as leased resources;
5. test what you ship;
6. typed acceptance producers.

**Under review:** `git -C /home/mega/auto-dev-cycle diff f4e099e..HEAD`, which covers:
- the new `docs/design/m0/13-artifact-graph.md`;
- the reduced `12-evidence-jobs.md`;
- simplifications in `02, 04, 05, 06, 07, 10, 11`;
- plan §2.5/§2.7, D40, and the intake record's "Reframed by D40" section.

Earlier review context: `docs/reviews/2026-10-03-pr5-astra.md`, `docs/design/m0/reviews/`, `docs/plan/reviews/`.

**Check:**
1. **Soundness of the graph model:**
   - key computation, including conclusions-only inputs;
   - memoization of non-pure model nodes, and whether it can wrongly reuse a result;
   - unrolled repair rounds;
   - effectful nodes (merge, deploy) with crash recovery (03);
   - how the merge queue fits a demand-driven graph (base moves, rebase, delta review, final-tree binding);
   - parallelism.
2. **Does each "by construction" claim actually hold?** Or is a rule still needed somewhere? Name any failure class
   that is now *less* protected than in the rule-based version.
3. **Consistency** with authority (04), leases/fencing (05), receipts (06), the manifest (10) and evidence nodes (12).
   Is anything decided twice?
4. **Scope:** is the engine small enough for v1? What should be cut?
5. **Ship tree:** is the default (export without VCS metadata plus declared build steps) project-agnostic? Are there
   pitfalls for TS, Go or PHP projects?

**Output:** a one-line verdict (GO / GO with changes / NO-GO), then findings by severity, each with location, problem,
why and a concrete change. Be terse. Block only on Critical/High.
