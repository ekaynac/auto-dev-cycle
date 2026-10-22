## External review of chunk 1, 22a-5c-2b-2 to 5c-2c (Astra, 2026-10-22)

Verdict: NO-GO

1. **High — The sandbox lock does not protect a shared run directory.** `internal/daemon/sandboxlock.go:25`
   
   **Input:** two daemons with different stores and canonical sandbox roots, `/srv/sb-a` and `/srv/sb-b`, whose `runs/` symlinks point to the same dedicated filesystem directory. This configuration passes sandbox validation; linked `runs/` directories are explicitly supported.
   
   Both daemons acquire different `.adcd.lock` files. B’s `RemoveStale` sees only B’s Podman storage and can return successfully while A runs. B then deletes A’s workspace through `tearDownRuns` (`internal/daemon/sweep.go:100`) and can remove A’s exchanges through `RefuseOrphans`. These are live runs of another store.
   
   **Fix:** retain the sandbox-root lock and also acquire exclusive ownership of the actual run directory before creating or sweeping anything there. The ownership check must cover symlink and bind-mount aliases.

2. **High — The guard’s own failure bypasses the promised `could_not_run` retry.** `internal/daemon/accept.go:136`
   
   **Input:** the guard child exceeds its production ten-minute deadline while the fifteen-minute accept deadline remains live, or the child fails to start or crashes.
   
   `accept.Integrity` returns a `test_integrity_violation` block with **nil error** for these cases (`internal/accept/integrity_proc.go:283`, `:333`, `:347`). Consequently, this integration skips `couldNotRun`, commits a selected `failed` result, and completes the accept node without a retry hold. A transient `go list` sandbox failure follows the same terminal path. The timeout tests shorten the outer accept deadline, so they exercise a different branch.
   
   **Fix:** expose a typed execution-failure indication from the guard and route it through `couldNotRun` and the bounded hold. Preserve substantive integrity findings as candidate refusals.

3. **Medium — A refused final accept commit leaves the node eligible for unlimited reruns.** `internal/daemon/accept.go:149`
   
   **Input:** accept claims a sealed build result, then that producer is invalidated through `graph.Invalidate` before accept commits. `graph.Commit` refuses the stale input, but the accept node retains its claim.
   
   This final command returns its error directly, without the hold fallback used by the gate and accept’s `could_not_run` path. Subsequent reconciliation passes repeat the clone and guard against the same permanently stale input, without consuming the bounded retry count.
   
   **Fix:** handle final-command errors with the same hold fallback as `gateNode`, checking that the original claim still applies before holding it.

4. **Medium — A base compilation failure does not receive the immediate terminal hold.** `internal/daemon/gate.go:146`
   
   **Input:** the base’s sole Go package fails to compile, producing a package-level failure and no individual test results.
   
   The parsed report is red, but `len(measured.Results.Results) == 0` excludes it from the red-base branch. It instead receives transient retries, repeatedly measuring the unchanged red base.
   
   **Fix:** recognize package-level failures when classifying a red baseline; do not require an individual test result.

Static review of `dcdb809..123e990`; no tests run or files changed. Outstanding charge uses `dispatch.Cost`, and launch remains inactive.