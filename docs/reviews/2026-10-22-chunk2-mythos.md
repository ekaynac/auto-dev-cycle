## External review of chunk 2, 22a-5d (Mythos, 2026-10-22)

Verdict: GO

Nothing is Critical or High within the promise, and I found no regression against main. One Medium and five Low findings follow. I reviewed `git diff a0c5d4b..HEAD` at c189364, read-only, with no tests run, and read the three Opus records.

**What the diff delivers, checked against the promise**

- **The starter.** `runnerOf` builds `containerAdapter` and nothing else (`internal/daemon/wiring.go:109-134`). `hostAdapter`, `adapterPath`, `Jail` and `--adapter` are gone; no Go, script, Makefile or ops file still names them.
- **The image.** `Start` resolves the profile's one pin, then `CheckManifest` and `CheckAdapter` against `Want`, which `releaseAdapter` digests once at adcd's start and refuses to start without (`internal/daemon/adapter.go:96-106,181-201`). The container runs the resolved content-addressed ID with `--pull=never` (`internal/sandbox/run.go:135,158`). The rows cover each refusal, including a label with no lockfiles and a release one level off.
- **The container.** The interactive command comes from the same `args()` as the batch start: `--network=none`, `--cap-drop=all`, read-only root, and only the workspace, results, gateway socket, `in/` (read-only) and `out/` mounted (`internal/sandbox/run.go:135-158`). The podman client gets `h.env()`, nothing of the controller's. `Path` maps exactly four host paths and returns the empty string for anything else, which prepare refuses.
- **`Ready`.** The command is built only once the exchange exists; a failure discards the exchange, ends the lease, and is `errHost`, retried, never held on the profile (`internal/daemon/run.go:265-269,620-630`).
- **Failures.** A spawn failure is `ErrSpawn`, the removal still runs, and the runner records `could_not_run/precondition_failed` with the node not completed (`run.go:301-307`). `ErrLeft` alone sets `over`, which skips the diff and records `failed/unsafe_output`, torn down (`run.go:308-316,759-763`). Any other `Within` error keeps the attempt as `ambiguous_state`. `Check` runs before the deferred exchange removal and never after an unverified end (`internal/adapterhost/attempt.go:228-230`).
- **The sandbox root.** `devOf` climbs on `ENOENT` and `EACCES` only; `ENOTDIR` is pinned by a row (`internal/sandbox/sandbox.go:146`, `bounds_test.go:182-211`).
- **The image script.** Every podman call but the build goes through `pod` with `timeout --kill-after`, the native platform is podman's, Node starts only there, and the EXIT trap removes `NAME` once the build has begun. The rows cut every bounded call, one ignoring TERM, and a bad timeout value.
- **Live rows.** The end-to-end row requires `ADC_LIVE_ADAPTER_RELEASE` and checks `runs/a*`, the exchange root, the clones and the containers are empty afterwards. Both Opus L4 gaps are closed.

**On the six questions**

1. **An unpinned or unchecked image or adapter.** No. Resolution, both checks and the ID binding happen before `Ready`; the only seam is the fake sandbox in tests.
2. **Reach beyond the socket, the workspace and the exchange.** No. `/results` is also mounted, but it is the attempt's own directory, counted in the bound and torn down with it. The cid file lies outside every mount.
3. **Host fault as the candidate's, or the reverse.** Mostly no. The spawn case is closed. Two residuals are Low: an unmeasurable tree is recorded as the candidate's (L3), and a persistent host fault after launch loops (M1).
4. **A container or lease left live.** No path I can find. Every end after a start goes through the final removal before `Wait` returns; a failed removal is `ErrUnverified`, kept, with its cid as the sweep's handle. Every end before a start discards the exchange and ends the lease.
5. **The EACCES climb.** Fail-closed in practice. The climb stops at the unsearchable directory itself, whose device is measured. The one miss, a mount strictly below a root-0700 directory on the runs filesystem, is the operator's to make and is stated in the comment.
6. **Regression against main.** None found. `controllerPaths` now forbids the whole shared run directory, which is stricter than the socket alone. An existing unit file that passes `--adapter` will refuse to start, which is the intended removal; no file in the repository names the flag.

### Medium

**M1. A persistent spawn failure is re-placed with no hold, backoff or cap.** `internal/daemon/run.go:296,301-307`
- **Input:** `--podman /usr/bin/podman` where the binary is missing or not executable on the controller's host, for example after a package upgrade.
- **Effect:** each pass clones the workspace, opens a gateway, prepares the exchange, fails the spawn, records `could_not_run/precondition_failed`, revokes the lease, and the next pass places the build again. The retry counter is deleted at launch, so nothing bounds it. Before the launch the same class of fault gets `holdOn`'s bounded retry, five tries with backoff. Nothing is unsafe; it is noise and a clone per pass until the operator notices.
- **Fix:** keep `r.tries[node]` across the launch for this cause, and raise the runner's retry hold after `abandon` on `ErrSpawn`, so the fifth failure ends "failed" like a pre-launch host fault. Add a row with the podman path missing on two consecutive passes, expecting a `retry 2 until` hold.

### Low

**L1. `Start`'s image inspect is unbounded.** `internal/daemon/adapter.go:96`, `internal/sandbox/sandbox.go:169-179`
- **Input:** a podman client that hangs on `image inspect` (a stale storage lock under `<root>/storage`).
- **Effect:** the attempt goroutine and the granted lease hang for as long as the client does. `Remove`, `gone` and `RemoveStale` are bounded by `removeTimeout`; this call is not. The placer's `Observe` has the same class on main, so this is not a regression.
- **Fix:** wrap `ctx` with a timeout in `Start` before `Resolve`, `removeTimeout` or similar, and map the deadline to `errHost`.

**L2. An exchange past the cleanup's budget stays until the next start.** `internal/adapterhost/exchange.go:101`, `internal/sandbox/run.go:333`
- **Input:** an `out/` with more than `MaxOutEntries` entries, or one at exactly the limit plus `in/`'s entries and the root's three.
- **Effect:** `Within` fails it as `ErrLeft` (correct), but the deferred removal visits at most `MaxWork` entries over the whole exchange, so the remainder stays under the exchange root until recovery's sweep at adcd's next start. Only the release's own adapter writes `out/`, so a candidate cannot reach this.
- **Fix:** set the `out/` bound to `MaxWork` less `in/`'s entries and the root's, or sweep the exchange root at runtime after a breach.

**L3. "Cannot be measured" is the candidate's.** `internal/sandbox/run.go:232-237`, `internal/daemon/run.go:312-313`
- **Input:** an `EIO` or `ENOMEM` while walking the attempt directory after the container is gone.
- **Effect:** the unit completes as `failed/unsafe_output`. The detail says "or what cannot be measured", so the record is honest, and a candidate-made `EACCES` (a 000 directory in `/workspace`) is rightly the candidate's. This is decision A's fail-closed reading; I note it because question 3 asks.
- **Fix:** optional: split `ErrLeft` into a breach and an unmeasurable tree, and map the latter to `ambiguous_state` with the directory kept.

**L4. The script's message when the build itself fails.** `scripts/prepare-go-image.sh:250-262,271`
- **Input:** `podman build` fails before it creates the list.
- **Effect:** `built` is already set, so `cleanup` runs `pod manifest rm NAME`, which fails on a name that does not exist, and prints "could not be removed: do not push it; run podman manifest rm". Nothing is pushed, but the lead is told to remove what is not there.
- **Fix:** in `cleanup`, run `pod manifest exists "$name"` first and print "nothing to remove" when it is absent.

**L5. An `Open` failure that is not a spawn keeps the attempt.** `internal/adapterhost/session/container.go:46,53`, `internal/daemon/run.go:317-319`
- **Input:** `os.Pipe` fails (the descriptor limit reached) in `StartContainer`.
- **Effect:** no client ran and no container exists, but `Check` runs, `Within` reports "may still be running" because `Ready` marked the attempt unsafe, and the runner keeps the directory as `ambiguous_state`. That is the promise's "any other doubt keeps the attempt", and the start sweep clears it; it is only an attempt directory held until a restart.
- **Fix:** run `c.Remove` on the pipe failure too, as the spawn path does, and return it wrapped in `ErrSpawn`.

**Nothing to change for the mechanical pin commit.** The schema's pattern still allows a registry-less `name@sha256:…`; `Resolve` refuses it as `ErrUnqualified`, held on the profile, so the pin must carry `ghcr.io/…`.
