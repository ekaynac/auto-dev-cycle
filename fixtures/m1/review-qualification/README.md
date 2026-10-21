# Reviewer qualification suite (P31, D56)

A reviewer route is qualified when, on this suite, it returns **no_go on all four planted defects** and **go on at least three of the four clean controls** in each of **its latest two committed runs, which must have different seeds** (the lead, #226 entry 110). Each of those records (by `ran_at`) must still bind the route, the reviewer prompt (`review.PromptDigest`, and each case's rendered prompt), the http-chat adapter (`qualify.AdapterDigest`) and this suite (`qualify.Suite`'s digest). Every case's evidence must bind its report to its run, and every review report must agree with its case. `internal/qualify.Check` decides this. It fails closed. P22a's activation of the `review` capability calls it (the I-inventory's obligation P22a-6).

## Layout

| Path | What it holds |
|---|---|
| `suite.json` | The bar (`pass`), the project (`fixtures/t0/go-mini`) and the cases. Each case has its task, the verdict it expects, and the digest of its rendered prompt (`prompt_digest`). A defect also has what is wrong with it and the one sealed probe it fails (`probe`). |
| `cases/<id>/candidate.patch` | The candidate, a patch of the project's `repo/` after its task's visible overlay. |
| `sealed/<task>/inventory/` | The probes: `qual_probe_test.go` checks the task's requirement at its edges, and `qual_tests_probe_test.go` checks that every existing test file is unchanged, byte for byte. |
| `records/` | The live runs' records (`adc.review-qualification/v1`), each with its cases' D55 review reports in a directory of the same name. They are committed from the lead's live run. Each case in a record carries the evidence of its execution, which `qualify.Route.Review` makes: the run's id, the case's place in the run's order, the digest of the prompt sent, the registry route, the adapter and suite digests, and the model call. Check reads records and reports as regular files only (no link, no special file), within byte and count bounds. |
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

**The positive control (§2.7).** `TestSuiteDiscriminates` proves the defects are real and the controls clean. It applies each patch, runs the visible gate, then adds the sealed probes. Every defect fails exactly its named probe. Every control passes them, and so does each task's reference change.

## What a reviewer sees

A reviewer sees only `review.Prompt`: the task's requirement, the candidate's diff and the gate's summary. It never sees a case's id, `suite.json`, a sealed probe or the task's `hidden/` tests (`TestPromptWithholds`).

## Runs

- **CI:** `TestRoute` runs the suite through the real http-chat adapter and gateway (`qualify.Route`), against a scripted upstream, in each run's seeded order:
  - a discriminating script's one run does not qualify the route, and its second run, under another seed, does;
  - an all-go script and an all-no_go script do not ("under the bar");
  - the provider is sent exactly each case's prompt, in the seed's order;
  - each outcome's evidence is its run's, its prompt's and its route's;
  - the all-no_go run's defect reports and the all-go run's control reports, together, cannot be recorded as one run;
  - malformed answers are misses.

  CI cannot qualify a model.
- **Live (the lead, on the controller host):** see `internal/qualify/live_test.go`:

  ```
  ADC_QUALIFY_REGISTRY=<registry.json> ADC_QUALIFY_GROUPS=<groups.json> ADC_QUALIFY_EXECUTOR=<exe_…> \
  ADC_CREDENTIAL_<REF>=… go test -tags qualify -run TestQualifyLive -timeout 2h ./internal/qualify/
  ```

  It writes `records/<date>-<executor>-<run id prefix>.json` and its reports, for a PR of their own. The cases run in the order the run's random seed decides; the record keeps the seed. A route needs two passing live runs, each with its own seed, so the run is made twice; it logs `Check`'s answer, and fails only when its own run is under the bar (that record is committed all the same).

Any change here (a case, a patch, a probe, `suite.json`) changes the suite's digest. So does any change to a case's rendered prompt (its task's requirement or base, the template), which fails `TestPromptWithholds` until `suite.json` pins the new digest. A change to the http-chat adapter's runtime closure (its source, the contract package it imports, the generated validators, the workspace's manifests or lockfile) or to the reviewer's limits changes the adapter digest. In each case every record goes stale, and two new live runs are needed. Only the latest two runs of a route count: one passing run is not enough, two records of one seed are one run, and a failing run committed after passing ones unqualifies the route until two more pass.

**Residual (stated).** Each case runs once per run, and the bar assumes the reviewer's decisions are independent and identically distributed across cases. Under that assumption, a reviewer whose decisions ignore the case can pass the bar with a small chance per run: at most about 2.3% when it says no_go 61% of the time (Opus #306 M3). An order-dependent policy, one that ignores the prompt and answers by the request's place in the run, is outside that assumption (Astra #306 M5). The seeded order bounds it: answering no_go to the first four requests passes with probability 1/70 per run, and the best case-ignoring policy of any kind (no_go on exactly five requests) passes with 1/14, about 7.1%. Qualification takes two passing runs with different seeds, and each run's order is fresh. So a case-ignoring reviewer of any kind, stateful across runs included, qualifies with at most (1/14)² = 1/196, about 0.5%: this is the worst case. Under the i.i.d. assumption the figure is about 0.05%. The latest-two rule covers a failing run once it is committed, not a run that never is. The evidence is not signed: a hand-edited record whose evidence is made consistent is a forgery for the record's review to catch.

**Mutations.** `internal/qualify/testdata/mutations.py` runs each mutation of the harness alone on a clean copy of the repository and reports it killed or survived (`python3 internal/qualify/testdata/mutations.py`, from the repository root).
