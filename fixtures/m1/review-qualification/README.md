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

All eight cases apply cleanly and pass the visible gate (`go test ./...`), so the gate alone tells nothing apart. Each task has exactly one planted defect and one clean control (Opus #306 r2 H1; `TestSuite` counts them), so the requirement alone tells nothing apart either.

| Case | Task | Expect | Kind |
|---|---|---|---|
| `defect-transfer-keeps-destination-price` | `go-mini-feature-1` | no_go | a wrong result: the destination keeps its own price |
| `defect-lowstock-negative-threshold-panics` | `go-mini-bugfix-1` | no_go | an unchecked input that panics: a negative threshold |
| `defect-sku-package-test-weakened` | `go-mini-refactor-1` | no_go | a deleted assertion: the `"Ü-1"` case of `TestAddRejectsBadInput`, against "behaviour-preserving" |
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
  - a discriminating script's run is incomplete until `Record`; its one run does not qualify the route, and its second run, under another seed, does;
  - an all-go script and an all-no_go script do not ("under the bar");
  - the provider is sent exactly each case's prompt, in the seed's order;
  - each outcome's evidence is its run's, its prompt's and its route's;
  - the all-no_go run's defect reports and the all-go run's control reports, together, cannot be recorded as one run;
  - malformed answers are misses;
  - `Review` refuses a case out of the seed's order, a case reviewed again, another case's prompt, a run `Begin` did not start, and an adapter command other than `qualify.AdapterEntry` under the suite's root.

  CI cannot qualify a model.
- **Live (the lead, on the controller host):** see `internal/qualify/live_test.go`:

  ```
  ADC_QUALIFY_REGISTRY=<registry.json> ADC_QUALIFY_GROUPS=<groups.json> ADC_QUALIFY_EXECUTOR=<exe_…> \
  ADC_CREDENTIAL_<REF>=… go test -tags qualify -run TestQualifyLive -timeout 2h ./internal/qualify/
  ```

  It renders every prompt, then `Suite.Begin` writes `records/<time>-<executor>.json`, **incomplete**, before the first review: an aborted run leaves that record, and it fails qualification. The cases run in the order the run's random seed decides, and the record keeps the seed. Each case's id, expectation and decision are logged only after `Record` completes the record and writes its reports. A route needs two passing live runs, each with its own seed, so the run is made twice; it logs `Check`'s answer, and fails only when its own run is under the bar (that record is committed all the same). Every record, incomplete ones included, goes into the records PR; deleting one is for that PR's review to catch.

Any change here (a case, a patch, a probe, `suite.json`) changes the suite's digest. So does any change to a case's rendered prompt (its task's requirement or base, the template), which fails `TestPromptWithholds` until `suite.json` pins the new digest. A change to the http-chat adapter's runtime closure (its source, the contract package it imports, the generated validators, the workspace's manifests or lockfile) or to the reviewer's limits changes the adapter digest. In each case every record goes stale, and two new live runs are needed. Only the latest two runs of a route count, with every run at either one's time: one passing run is not enough, two records of one seed are one run, an incomplete record fails, and a failing run committed after passing ones unqualifies the route until two more pass.

**The committed records qualify this tree (Opus #426 M1).** `TestTheCommittedRecordsQualify` requires `qualify.Reviewers` to be nil, over `RecordsDir` of the repository's own tree, for every route the committed records hold. So a change that stales them (the adapter's sources, `MaxTurns`, `MaxOutputTokens`, the suite, the review prompt) fails CI in the PR that makes it. That PR must record the route again, twice; otherwise every later release would start with review inactive.

**What a record binds, and what it does not:**
- **Two adapter digests, two functions (Opus #426 L2).** A record's `adapter_digest` is `qualify.AdapterDigest`: the http-chat adapter's runtime sources (`adapterPaths`: its source, the contract package, the generated validators, the workspace's manifests and lockfile) and the reviewer's bounds (`MaxTurns`, `MaxOutputTokens`). The committed records give `sha256:61e7a8d4…4aba`. The image's label is `sandbox.AdapterDigest`: the listing of a release `lib/adc`'s adapter closure (`sandbox.AdapterClosure`: `node/bin/node`, the adapters, `gen/ts`, `node_modules`), which is `sha256:427c3994…da83` for main `f1638337`. They are different functions over different files and never match; compare a record only with `qualify.AdapterDigest`.
- **Another Node and other `node_modules` (Opus #426 L1).** The live qualification (`TestQualifyLive`) runs `http-chat` on the host's `node` (the one on `PATH`) and the workspace's dev install (`make ts-deps`). adcd runs the reviewer on the release's pinned Node (`scripts/node-pins`) and its production `node_modules`. `qualify.AdapterDigest` covers neither the Node binary nor `node_modules` (the lockfile pins the packages' versions only), and a record does not say which Node it ran on.
- **The endpoint, byte for byte (Mythos #426 L).** A route is its registry executor's identity, its endpoint compared as a string. The registry's `destination.endpoint` must stay byte-identical to the record's (`https://ollama.com/v1`): `https://ollama.com/v1/` or `https://OLLAMA.com/v1` is another route with no record, though the gateway reads it as the same origin. Changing it takes two new live runs.

**Residual (stated).** Each case runs once per run. For a reviewer that never reads the diff:
- **reading the requirement only, stateful** (it remembers the run's requests): at most **1/8 per run, 1/64 (about 1.6%) over the latest two runs**, the worst case for a reviewer that never reads the diff (one that reads its paths does better, below). This is an exhaustive count over all 8! orders (`TestBlindReviewers`). The sequence of tasks says nothing of which of a task's two requests is its defect, so no stateful rule does better.
- **reading the requirement only, stateless** (no_go with a probability per task): at most about 2.3% per run (0.61 on every task, found numerically), and about 0.05% over two runs. This is the i.i.d. figure, which assumes independent, identically distributed decisions (Opus #306 M3).
- **reading nothing, answering by the request's place in the run:** at most 1/14 per run, and 1/196 over two runs (Astra #306 M5).
- **reading only the paths in the diff:** at most **1/4 per run, 1/16 over two runs** (Mythos #306 L4). Only `defect-sku-package-test-weakened` touches an existing `_test.go` file, so that path alone decides the refactor task; the other three fall to the 1/8 rule. A control that edits an existing test would fail the sealed `TestQualProbeTestsUnchanged`, so this is stated, not balanced.

Re-running until two passes in a row qualifies the 1/8 reviewer after about 72 runs on average, unless every run's record is committed. The evidence is not signed: a hand-edited record whose evidence is made consistent is a forgery for the record's review to catch. The node binary, its version and its environment (`Review` checks only the adapter's entry point, not `Command[0]`, `NODE_OPTIONS` or an in-process adapter; Mythos #306 L2) and the route's Go half (gateway, adapter host, `review.Parse`/`Result`) are outside the adapter digest. A dishonest driver (a run time given to `Begin` below the latest two, a tree changed between `Begin` and `Record`, an outcome swapped before `Record`; Mythos #306 M1, L1, L3) is the same boundary as a hand-edited record; its code fixes are a follow-up.

**Mutations.** `internal/qualify/testdata/mutations.py` runs each mutation of the harness alone on a clean copy of the repository and reports it killed or survived (`python3 internal/qualify/testdata/mutations.py`, from the repository root).
