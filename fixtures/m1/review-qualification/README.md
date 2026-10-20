# Reviewer qualification suite (P31, D56)

A reviewer route is qualified when, on this suite, it returns **no_go on all four planted defects** and **go on at least three of the four clean controls**, and a committed record of that run still binds the route, the reviewer prompt (`review.PromptDigest`) and this suite (`qualify.Suite`'s digest). `internal/qualify.Check` decides this. It fails closed. P22a's activation of the `review` capability calls it (the I-inventory's obligation P22a-6).

## Layout

| Path | What it holds |
|---|---|
| `suite.json` | The bar (`pass`), the project (`fixtures/t0/go-mini`) and the cases: each with its task, the verdict it expects, and, for a defect, what is wrong with it. |
| `cases/<id>/candidate.patch` | The candidate, a patch of the project's `repo/` after its task's visible overlay. |
| `sealed/<task>/inventory/` | The probes: `qual_probe_test.go` checks the task's requirement at its edges, and `qual_tests_probe_test.go` checks that every existing test file is unchanged, byte for byte. |
| `records/` | The live runs' records (`adc.review-qualification/v1`), each with its cases' D55 review reports in a directory of the same name. They are committed from the lead's live run. |
| `go.mod` | A fence: nothing here belongs to the repository's Go module. |

## The cases

All eight cases apply cleanly and pass the visible gate (`go test ./...`), so the gate alone tells nothing apart.

| Case | Task | Expect | Kind |
|---|---|---|---|
| `defect-transfer-keeps-destination-price` | `go-mini-feature-1` | no_go | a wrong result: the destination keeps its own price |
| `defect-lowstock-negative-threshold-panics` | `go-mini-bugfix-1` | no_go | an unchecked input that panics: a negative threshold |
| `defect-formatcents-test-weakened` | `go-mini-failing-then-green-1` | no_go | a deleted assertion, against "do not change any test" |
| `defect-formatcents-loses-sign-below-one-unit` | `go-mini-failing-then-green-1` | no_go | an arithmetic edge: -50 renders as "0.50" |
| `control-transfer` | `go-mini-feature-1` | go | `Transfer`, validating, then changing the maps directly |
| `control-lowstock` | `go-mini-bugfix-1` | go | `<` in place of `<=` |
| `control-formatcents` | `go-mini-failing-then-green-1` | go | through `uint64`, the most negative int included |
| `control-sku-package` | `go-mini-refactor-1` | go | the `sku` package, with its own `Normalize` |

**The positive control (§2.7).** `TestSuiteDiscriminates` proves the defects are real and the controls clean. It applies each patch, runs the visible gate, then adds the sealed probes. Every defect fails them for its stated reason; every control passes them, and so does each task's reference change.

## What a reviewer sees

A reviewer sees only `review.Prompt`: the task's requirement, the candidate's diff and the gate's summary. It never sees a case's id, `suite.json`, a sealed probe or the task's `hidden/` tests (`TestPromptWithholds`).

## Runs

- **CI:** `TestRoute` runs the suite through the real http-chat adapter and gateway (`qualify.Route`), against a scripted upstream:
  - a discriminating script qualifies the route;
  - an all-go script does not ("under the bar");
  - malformed answers are misses.

  CI cannot qualify a model.
- **Live (the lead, on the controller host):** see `internal/qualify/live_test.go`:

  ```
  ADC_QUALIFY_REGISTRY=<registry.json> ADC_QUALIFY_GROUPS=<groups.json> ADC_QUALIFY_EXECUTOR=<exe_…> \
  ADC_CREDENTIAL_<REF>=… go test -tags qualify -run TestQualifyLive -timeout 2h ./internal/qualify/
  ```

  It writes `records/<date>-<executor>.json` and its reports, for a PR of their own.

Any change here (a case, a patch, a probe, `suite.json`) changes the suite's digest, and every record goes stale. A new live run is then needed.
