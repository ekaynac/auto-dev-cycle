## External review of chunk 1, round 2 (Astra, 2026-10-22)

Verdict: NO-GO

1. **High — H2: loader setup failures still become candidate refusals.** `internal/accept/integrity_loader.go:135`

   **Input:** a valid candidate needs a listing, but `NewAttempt` fails with `ENOSPC` while creating its workspace. `boxList` returns the error without `ErrGuardRun`; `service.serve` records only `listFailed`, so the guard returns a refusal with nil error. Accept completes with a selected failure instead of `could_not_run` and a retry hold. Workspace writes, output-copy filesystem errors, and teardown failures have the same gap.

   **Fix:** mark host setup, filesystem I/O, and cleanup failures as `ErrGuardRun` at their source. Keep candidate input violations and explicit resource-bound violations separately classified as refusals.

2. **High — H2: the exit-status classifier confuses Podman failures with candidate failures.** `internal/accept/integrity_loader.go:195`

   **Input:** `podman run` succeeds, but the subsequent `podman rm` exits 1. `Host.Start` joins execution and cleanup errors (`internal/sandbox/run.go:187`); `didNotRun` sees cleanup’s `ExitError(1)` and treats it as the candidate’s failure. Likewise, a host-killed Podman client produces `ExitCode() == -1`, which this branch also classifies as a candidate refusal despite receiving no workload exit status.

   **Fix:** preserve error provenance in the sandbox result: distinguish workload status, client termination, and cleanup failure. Route host failures through `ErrGuardRun`; use only an actual workload result to classify candidate failure.

3. **Medium — H2: wrapped `ErrSpec` bypasses the bounded retry hold.** `internal/accept/integrity_loader.go:156`

   **Input:** sandbox preparation returns `ErrSpec`, for example because a mount source contains a comma. The new error wraps both `ErrGuardRun` and `ErrSpec`. Although accept records `could_not_run`, `configFault` still recognizes the wrapped `ErrSpec` (`internal/daemon/run.go:597`), producing a profile hold instead of the promised bounded retry. Repairing the host path does not clear that profile hold.

   **Fix:** give `ErrGuardRun` explicit precedence over generic `ErrSpec` classification when choosing the hold.

H1’s same-directory symlink and bind-mount aliases now contend on the same lock inode before runner creation or sweeping; the sweep leaves the lock file intact. M1’s refused final commit now holds only while the original computation claim matches. M2 now recognizes package-level build failures without individual test results. I found no additional acceptance bypass in the reviewed retry paths.

Read the prior review, PR #395’s round section, and `123e990..0691b40`. Static review only; no tests run or files changed.