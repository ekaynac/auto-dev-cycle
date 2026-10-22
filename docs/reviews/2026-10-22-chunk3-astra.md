## External review of chunk 3, 22a-6a to 6b (Astra, 2026-10-22)

Verdict: NO-GO

1. **High — A registry change can strand a claimed review without a hold.** `internal/reconcile/reconcile.go:447`; `internal/daemon/place.go:71`.

   **Input:** Claim the review on reviewer A, then restart before placement with A removed and an independent reviewer B registered. `Match` returns `graph.ErrNotIndependent`, but `Place` handles only `dispatch.Refused`. Every subsequent pass retries A; no `reviewer_unavailable` hold is raised, so `Unheld` never selects B.

   This is the unresolved Opus round-three M1, **High under the supplied severity floor**. It occurs in 6a’s placement path before review execution. The state table’s claimed branch (`internal/daemon/reviews_test.go:455`) returns without checking placement or recovery.

   **Fix:** Convert this refusal into `reviewer_unavailable`, reusing the handling already present in `dispatch.Launch`, then let `Unheld` supersede onto B. Pin the claimed-but-unplaced restart cell explicitly.

2. **Medium — A valid empty candidate cannot produce reviewer context.** `internal/intent/review.go:99`.

   **Input:** A builder completes without changes. `graph.Build` records `patch/empty` with zero refs; accept can accept the unchanged base, and its gate can complete. `Review` nevertheless requires exactly one patch ref and rejects this result. Its final requirement of three included refs also excludes this case.

   **Fix:** Handle the existing empty-patch representation explicitly, render a deterministic “no changes” indication, and derive the corresponding intent without inventing a patch ref. Update the prompt contract and qualification digest as necessary. This is a context false block, independent of the already-stated oversized-diff obligation.

3. **Medium — The new brief-to-review integration rejects schema-valid Unicode requirements.** `internal/intent/review.go:98`.

   **Input:** A task requirement containing 17,000 four-byte Unicode characters satisfies the task and brief schemas’ 20,000-character limit. Its brief fits `MaxBrief`, but `brief.Prompt` produces over 68,000 bytes, exceeding `review.Prompt`’s 65,536-byte requirement bound. The builder can consume this task; the reviewer context cannot.

   **Fix:** Align the reviewer’s byte bound with the admitted title-and-requirement domain, or enforce and document a consistent bound at intake. Preserve identical rendering and validation in the runner and intent fold.

Only finding 1 blocks this chunk. Static review of the supplied ranges and Opus records; no tests run and no files changed. Launch and review remain inactive.