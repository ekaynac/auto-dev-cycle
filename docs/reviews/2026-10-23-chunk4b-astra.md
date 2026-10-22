## External review of chunk 4b, 22a-6d (Astra, 2026-10-23)

Verdict: NO-GO

1. **High — Damaged ancestry blobs become permanent content faults.** `internal/intent/intent.go:347`

   **Input:** Complete a normal `no_go` review whose gate used acceptance blobs, then corrupt the stored decision bytes before the repair builder starts. Leave its findings view intact.

   `viewOf` calls `readBlob`, which returns `(nil, false, nil)` for a digest mismatch. Verification therefore returns “not safe” without `ErrBlobDamaged`. The walk follows the decision’s ancestry into acceptance and returns `ErrAcceptanceInput`. `holdOn` immediately raises permanent `runner: failed`; repairing the blob does not clear it. Corrupting the gate report has the same effect through `verifier`’s report reader.

   **Fix:** Return `ErrBlobDamaged` from the shared `readBlob` digest check and propagate it through every caller. Consolidate the separate damage classifications in `readRef` and `Review.include`. Add consumption rows covering damaged decisions and reports, including recovery during the bounded retry window.

2. **High — The repeated-ID exception admits records differing in revision.** `internal/graph/records.go:126`

   **Input:** Record two sealed refs in one task, from producers of the same kind, with identical content-addressed ID, digest, type, scope and restrictions, but revisions `"1"` and `"2"` and different clean lineages.

   `RefRecord` discards `Ref.Revision`, so the comparison treats these as differing only in lineage and resolves both. The ancestry walk cannot recover or check the lost revision. Before this change, the different lineages made this input ambiguous. This is an audit identity fail-open against “any other difference stays ambiguous”; it does not require bypassing the acceptance-ancestor walk.

   **Fix:** Preserve and compare revision, including nil versus non-nil, in recorded identities and input matching. Prefer a shared comparison of the canonical ref fields with only lineage excluded. Add a row requiring this pair to remain ambiguous.

3. **Medium — A malformed forged brief now takes host retries instead of an immediate content hold. Regression.** `internal/intent/build.go:66`

   **Input:** A recorded plan ref points to correctly hashed `{}` bytes, with otherwise valid ref metadata.

   `brief.Decode` returns its schema error before `intent.Builder` performs the own-brief check. That error is returned unclassified, so `contentFault` misses it and the runner retries until exhaustion. Previously, `runner.plan` performed the I18 check first and immediately refused the forged brief as `ErrNotTheBrief`. The consequence is bounded and visible, but contradicts the promised immediate hold.

   **Fix:** Classify brief decoding failures as content faults after the digest-checked read, preserving host errors separately. Extend the forged-brief row to cover malformed content.

Reviewed `a179d11..9823b1c` and both Opus records. No tests ran; no files changed.

Reuse searches used `rg` by name and purpose for the new repair actions and predicates, roster admission, builder-safe filtering, checked-node construction, prompt contexts and rendering, claimed-result loading, round labels, view verification, fencing, and blob-damage handling. Existing equivalents included `Successor`, `RecordReview`, `registered`, `Candidate`, `CheckView`, `readBlob`, and `brief.Prompt`. The duplicate damage-classification rule is included in finding 1.

Static inspection found the repair command atomic, successor uniqueness enforced by the fold, and the rounds rail enforced before claim. Kept-bit code and rows are present; `review` and `launch` remain inactive. Round 1 still renders `brief.Prompt` unchanged, and the extracted reviewer fence preserves qualification prompt bytes.