# go-mini: checkpoint (a)'s stub project

go-mini is the small Go project that checkpoint (a)'s CI stub cases run on. The operator decided this on 2026-10-20 (#226 entry 94). Its gate and guard need only the Go chain.

| File | What it holds |
|---|---|
| `repo/` | The repository: module `example.com/go-mini`, with package `money` (amounts in cents, parsed, formatted and added) and its ordinary tests. A remote holds it at `/var/lib/adc/remotes/go-mini.git` (the profile's `repo.url`). |
| `../profiles/go-mini.json` | Its project profile, which includes:<ul><li>this repository's pinned prepared Go image;</li><li>the gate `go test -count=1 -json ./money/...`, read with `go_test_json`, offline;</li><li>the USD rails. The count rails go in once P17a (#295) adds them to the profile schema.</li></ul> |
| `task.json` | The `task.create` scope of the stub task: `money.Split`, with an exact requirement and its acceptance attachment, by path and digest. |
| `baseline-report.json` | The gate's measured baseline (`gate.Baseline`), described below. |
| `baseline-stream.json` | The measured run's `go test -json` stream. `gate.TestGateGoMini` judges it against the baseline. |

The baseline was measured by `gate.Measure` in the pinned image (`ghcr.io/ekaynac/adc-prepared-go@sha256:8876412b…`, image `d1a298571f62`, linux/arm64), offline, in gate mode. It is bound to the gate's command, that platform image and `repo/`'s tree. Four tests pass, with no skip.

## Hidden acceptance test and reference change: outside this repository

As for #170, the acceptance test and the reference change are not committed. `repo/` is in this repository, and a builder's checkout of it must not contain them. They are recorded here by sha256 only. The lead keeps them in its own store.

| Item | Repo path | sha256 |
|---|---|---|
| The acceptance test | `money/accept_test.go` | `f172ed70d444a4d344f46b8b94a2ac7ad838e6e7217572c63b98bae787eb468a` |
| The reference change, a patch of `money/money.go` | — | `5013f19914b9744999081d83bd70e4b9626437ce307e2a1fac53d538dfbe10cd` |

- **How they are used.** The test file is uploaded as the task's acceptance blob when the task is created, and is overlaid on the gate workspace only (§2.6).
- **Canary.** It holds an unpublished canary line for §2.3's confidentiality check.
- **Names.** Its tests are named `TestAcceptGmz…`, a prefix that `task.json` reserves.
- **Checked in the pinned image.** Both runs were in gate mode with the test overlaid:
  - On `repo/` as committed, the package does not build: the run is not green.
  - With the reference change, all nine tests pass: the four ordinary ones and the five acceptance tests.

## Re-recording

Re-record with `gate.Measure` in the pinned image whenever `repo/` or the pin changes. The gate refuses a baseline of another command, platform image or base tree, and `TestGateGoMini` fails when `repo/`'s tree is no longer the baseline's.
