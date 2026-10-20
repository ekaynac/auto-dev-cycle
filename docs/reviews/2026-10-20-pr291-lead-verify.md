## Lead verification of #291 after Astra "GO with changes" (2026-10-20)

Head 06bca23. Astra (`2026-10-20-pr291-astra.md`) gave GO with changes. It found no High and no regression against main. Its findings:
- **M1:** an unleased result that is not selected, or was invalidated, escaped the contributors;
- **M2:** the repair did not bind the findings to the patch that was reviewed;
- **L3:** the hold's diagnostic named the reviewer.

Per D64, the lead verifies the changes, with no further external round.

**Checks**
- **The diff since bfa8b1e:**
  - `graph.Contributors` scans `ResultProjection`, so any unleased build result counts as `unknown`: selected or not, `could_not_run`, or invalidated (M1).
  - `Repair` requires the review's claim to have fixed the build's currently selected result id. The redundant declared-producer check is removed, since its mutation survived (M2).
  - `ErrNotIndependent` carries a `Signal`, and the recorded hold names `contributor:<executor>` or `contributor:<build node>`. A reviewer's own registration failure keeps `registry:<reviewer>` (L3).
- **The builder's evidence:** 25 mutations, each one caught. `TestUnleasedBuildIsUnknown` covers the three cases at claim and at commit.
- **The lead's own probe** (throwaway, deleted): P1 → invalidate → P2 → a no_go review of P2 → invalidate → P3.
  - `Repair(build, review of P2)` is refused ("did not review its selected patch").
  - A new no_go review of P3 repairs.
  - The binding holds across two invalidations, not only the one the row covers.
- **`-race`:** `TestRepair`, `TestUnleased*`, `TestContributor*` and `TestReviewCommit*` in `internal/graph`, and `internal/review` and `internal/receipt`, all pass.

**Verdict:** GO.

**Residual (stated in the PR body):**
- whether a repair builder may consume acceptance-derived findings (a builder-safe view, or the repair stops) is P22a's decision under #226 entry 91;
- families come from today's registry;
- rails on rounds are P17's;
- nothing is wired until P22a.
