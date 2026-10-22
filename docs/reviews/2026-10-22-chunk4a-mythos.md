## External review of chunk 4a, 22a-6c (Mythos, 2026-10-22)

Verdict: NO-GO

One High inside the promise ("a granted review runs in the runner's attempt path"). In adcd's container path the reviewer's empty workspace fails the image's lockfile check, so no review runs under any M1 profile that lists a lockfile, and the review is held on the profile. The rest is Low.

### High

**H1 (BLOCKING). The reviewer's empty workspace fails the image manifest check; every review is held on the profile and never runs.**
- `internal/daemon/run.go:287-292` passes the profile's `dependency_recipe.lockfiles` to `adapters.Start` for a review exactly as for a build.
- `internal/daemon/adapter.go:108` runs `img.CheckManifest(run.Toolchain, run.Workspace, run.Lockfiles)`. `internal/sandbox/image.go:202-209` hashes each lockfile at the workspace through `fileSum(base, p)`.
- A review's workspace is `emptyWorkspace` (`internal/daemon/workspace.go:854`): no clone, no file. With any lockfile listed, `fileSum` fails and the start returns `ErrManifest: a lockfile at the base is unreadable`.
- `attempt` ends the lease with that cause (`run.go:293-295`). `configFault` is true for `sandbox.ErrManifest` (`run.go:856`), so `holdOn` raises `source_invalid_config` on `profile <digest>` (`run.go:889-893`). Only a profile change clears it, and the next placement raises it again.
- Concrete input: the shipped profiles. `fixtures/m1/profiles/go-mini.json:17` lists `go.mod`; `fixtures/m1/profiles/auto-dev-cycle.json:17` lists `go.mod` and `go.sum`. Any gate that completes there yields a review that is matched, refused at the adapter's start and held on the profile. Its decision is never taken, and the hold blames a configuration that is correct.
- Why the rows miss it: every reviewer row runs under `peerStarter` (`internal/daemon/run_test.go:105`), which never calls `containerAdapter.Start`. `run_test.go:440` pins only that an `ErrManifest` start holds the node on the profile.
- Fix: for a review, pass no lockfiles to `AdapterRun` (`Lockfiles: nil` when `review`), and let `CheckManifest` keep the lockfile-set equality against the image while skipping the base hash when the run has no base to hash (or add an explicit `AdapterRun.NoBase`). The image's toolchain and adapter checks stay. Add one row under `containerAdapter` with a profile listing a lockfile: a review is prepared. The build path stays unchanged.

### Medium

None.

### Low

**L1. The reviewer's workspace is bind-mounted writable; the code says contract 09 mounts it read-only.** `internal/sandbox/run.go:185` mounts `/workspace` with no `ro`; only `ExchangeIn` gets `ro=true` (`run.go:193`). `Writable: false` (`run.go:308`) is honoured by the adapter alone (`adapters/http-chat/src/adapter.ts:137`). Nothing in that directory is read back for a review, and a disk overrun abandons the attempt, so the consequence is bounded. Fix: mount the review's workspace `ro=true` through a flag on `sandbox.Run` or `AdapterRun`, or correct the comment at `workspace.go:851` and the promise's "read-only".

**L2. A second completed gate would refuse `reconcile.review` every pass with no hold.** `reconcile.Observe` offers any completed gate no review takes (`internal/reconcile/reconcile.go:1144-1150`); `reviews.Record` always records requirement revision 1 (`internal/daemon/reviews.go:111`), which the fold refuses once a revision exists. Unreachable today: `graph.Repair` has no caller in `internal/daemon` or `internal/reconcile`. Obligation on 22a-6d: record the repaired round's review before its gate completes, or derive the next revision in `Record`.

**L3. Second builders of a result literal (rubric "Reuse").** Search: `OutcomeFailed`, `ReasonUnsafeOutput` and `OutcomeCouldNotRun` in `internal/daemon/run.go`. `unreviewable` (`run.go:458-459`) repeats `complete`'s failed/unsafe_output literal (`run.go:1019-1020`); `notIndependent` (`run.go:1112-1113`) repeats `abandon`'s could_not_run literal (`run.go:1036-1037`) with a direct `CommitAttempt`. Fix: one `resultOf(key, kind, outcome, reason, detail)` helper in the package.

**L4. `readBlob` now reads a blob to its end to hash it** (`internal/intent/intent.go:974-978`). A blob replaced on disk by a large file costs its whole size in I/O before `ErrUnbound`; memory stays at `limit+1`. Opus noted this. Fix: one line in the comment, or hash at most a fixed multiple of the bound and refuse past it as `ErrUnbound`.

### Judged

1. **Tools, writes, the tree, the ids, the acceptance.** `ToolPolicy.Allowed` is empty and `Spec.Tools` stays the edit loop's, as `qualify.Route` does (`internal/qualify/route.go:169`). Any `tool.request` unserves the run (`internal/adapterhost/attempt.go:449-450`), and a tool the adapter ran outside the spec refuses it (`attempt.go:455`). The prompt is `intent.Review`'s, with counts only from `gate.SummaryOf`. The I18 walk refuses acceptance lineage. The workspace is empty. The findings view redacts `gate.ReportIDs`. Only L1's mount mode deviates.
2. **A non-independent reviewer's verdict selected.** No. `commitRecorded` runs `reviewIndependent` before `recordResult` (`internal/graph/evaluate.go:449-458`), exempting only could_not_run, which `selected` refuses (`internal/graph/lifecycle.go:259`). The unreviewable `failed` verdict passes the same check, and a refusal with no attempt holds the review and records nothing.
3. **Spin, double record, stuck without a hold.** The refused commit emits nothing before `ErrNotIndependent`, so `notIndependent`'s second `CommitAttempt` records once. A refused reviewer (tool request, degenerate decision) is bounded by the attempts rail (`internal/dispatch/launch.go:228`). H1 raises a visible hold, but on the wrong owner, and in effect it never clears.
4. **Faults crossed.** A damaged blob is `ErrUnbound`, hash before size (`intent.go:984-988`). A reviewer's disk overrun is abandoned (`run.go:378`). An oversize or non-UTF-8 diff is the candidate's `failed/unsafe_output`. H1 is a host-path fault recorded as a configuration hold on the profile.
5. **Second implementations.** `graph.HoldNotIndependent` and `graph.TakesGate` are each the one builder in this diff, and the holds table lists the former. `dispatch.rosterMiss` (`launch.go:267`) still builds its own signal, from before this chunk. L3 covers the result literals.
6. **Regression against main.** `builderIntent` now runs before the clone and the Prepare (`run.go:238-249`); its failures hold as before and nothing is left to discard. `Observe`'s placed branch keeps its kind guard in the case condition (`reconcile.go:469`). `Reviewable` with reviews off stays empty. None found.

Blocks on H1.
