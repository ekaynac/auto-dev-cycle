## External review of chunk 1, round 3 (Astra, 2026-10-22)

Verdict: NO-GO

1. **High — Copy-out ENOSPC becomes a candidate refusal through a panic.** `internal/accept/integrity_loader.go:184`

   **Input:** the listing succeeds, but `CopyOut` cannot create its destination directory because of ENOSPC. The subsequent `WalkDir` invokes the callback with a nil entry and an error; `d.Info()` panics. `service.serve` recovers that panic as a candidate block, bypassing the deferred error default. Accept can commit a selected failure.

   **Fix:** handle the walk’s error and nil entry, preserve copy-out/accounting errors as `ErrGuardRun`, and route unexpected service panics through the same execution-failure path.

2. **High — Measurement failures masquerade as explicit bound breaches.** `internal/sandbox/run.go:264`

   **Input:** a successful, under-bound listing is followed by EIO while `Attempt.within` reads a directory or stats an entry; cleanup succeeds. `within` attaches `ErrLeft` to every measurement error. `workloadFailed` consequently treats this host fault as positive evidence against the candidate, although no bound was observed broken.

   **Fix:** attach `ErrLeft` only to measured byte, entry, or depth breaches. Return measurement failures separately so the guard’s default produces `ErrGuardRun`.

3. **High — The reported workload status includes infrastructure helpers, not just go list.** `internal/accept/integrity_loader.go:57`

   **Input:** `go list` succeeds, but `mkdir "$r/x"` fails with ENOSPC while the attempt remains below its configured bounds. With `set -e`, the shell exits nonzero. `Host.Start` labels that shell status `WorkloadExit`, and `boxList` refuses the candidate. Export copying and the script’s final cleanup have the same provenance problem.

   **Fix:** preserve go list’s status separately from wrapper setup, copying, and cleanup. Wrapper failures must yield `ErrGuardRun`; a shell’s aggregate status is insufficient evidence of go list’s own failure.

4. **High — Malformed listing records still become candidate findings.** `internal/accept/integrity_loader.go:267`

   **Input:** successful output contains a duplicate `fmt` record, with valid archives and no bound exceeded. `readListing` synthesizes `Listed.Error("listed twice")` and returns nil error. The importer turns this into a candidate block, so the deferred default never applies. The updated test explicitly retains this refusal at `internal/accept/integrity_loader_test.go:632`.

   **Fix:** return an error for structural listing inconsistencies, including duplicate records and missing referenced archives, allowing `boxList` to mark them `ErrGuardRun`. Keep these distinct from substantive package diagnostics.

5. **High — Host errors reading copied export data bypass the default.** `internal/accept/integrity_types.go:501`

   **Input:** listing and archive validation succeed, but the child’s subsequent `os.Stat` or `os.Open` of an export archive fails with EIO. The error flows through the importer into a type-check finding. The child returns blocks rather than an execution error, so `Integrity`’s new error wrapper cannot intervene.

   **Fix:** latch export-data filesystem failures separately from type-check diagnostics and propagate them through the child result as `ErrGuardRun`. Preserve explicit export-size breaches as candidate refusals.

The new sandbox wrapper separates Podman cleanup errors, statuses 125–127, and a killed client from `WorkloadExit`. `ErrGuardRun` also correctly precedes wrapped `ErrSpec` in hold selection. I found no new acceptance bypass in the changed paths; the blockers remain host faults being selected as candidate refusals.

Read the round 2 record, PR #395’s round section and outcome table, and `0691b40..030140c`. Static review only; no tests run or files changed.