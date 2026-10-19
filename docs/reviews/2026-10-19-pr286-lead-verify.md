## Lead verification of #286 after Astra r2 "GO with changes" (2026-10-19)

Head c575896. Astra round 2 (`2026-10-19-pr286-astra-r2.md`) gave GO with changes. It found no High and no regression against main. It had two findings:
- **M1:** the verdict kept the caller's baseline pointer, so a baseline changed between `Run` and `Result` changed the receipt's binding;
- **L2:** #170's row checked only the reference prefix.

Per D64, the lead verifies the changes, with no further external round.

**Checks**
- **The diff since a091762:**
  - `run` now stores `in.Baseline.snapshot()`, a deep copy of `Command`, `Results` and `Packages`. `results.Result` holds only strings and enums, so copying its elements by value is deep.
  - The comparison and the receipt's demand use only that copy.
  - The #170 row compares the full pin with `fixtures/m1/profiles/auto-dev-cycle.json`'s `toolchain.images.go`.
  - Nothing else changed in production code.
- **The builder's rows and mutations:**
  - `TestGateBaselineHeld` mutates an element in place and replaces the whole object;
  - mutations P1 (the caller's pointer), P2 (a shallow copy) and P3 (another digest) are all caught;
  - the earlier 28 mutations were re-run, and all are caught.
- **The lead's own probe** (throwaway, deleted): between `Run` and `Result`, mutate the caller's baseline IN PLACE in two other ways that the row does not cover. Every `Packages` value was set to `Fail`, a package was added, and `Command[0]` was rewritten.
  - **Result:** the receipt applies to B1 as judged, and it does not apply to the tampered baseline. The verdict was `empty`.
- **`-race`:** `go test -count=1 -race ./internal/gate/... ./internal/sandbox/... ./internal/results/...` passes.

**Verdict:** GO.

**Residual (stated in the PR body):**
- short candidate test and package ids stay verbatim, by design; the reviewer-side handling is P16's;
- workspace disk overruns with valid bounded results;
- forged but well-formed result events (the decision-A residual);
- the cwd contract (#275) moved to P15b;
- the daemon and graph wiring is P22a's.
