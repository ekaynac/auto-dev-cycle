# auto-dev-cycle: closing note (2026-10-29)

> **Paths.** `stage2/…` paths name files in the lead's private stage-2 workspace (not published). TW, BR, T3R and C are reports in `ekaynac/atolye`. `#226` is this repository's tracking issue.

**Status: stopped.** Stage 2 is complete; the operator closed T5 on 2026-10-29. ADC stopped on 2026-10-29 and no new
work starts on it (R17/R18). **No GPU rerun was done before the stop:** the OpenHands #5510 rerun (§1, §3) did not run,
so the #5510 part stays open. This note is in `docs/closing/`.
**Repository state:** `ekaynac/auto-dev-cycle`, origin/main `9ee8a40a`. All paths below are on that commit unless
marked otherwise. Read them with `git show 9ee8a40a:<path>`.

> **Caveat: T5 and the arms did not run on the same jobs.** T5 measured the earlier supervisor on **the private product** goals. Arms A and C
> and T1 ran on **set G** (ADC's Go items) and **set P** (public Python/TypeScript repositories); the private product is private
> and was never in the item set. **Arm B** (the successor supervisor on the same items) never ran: its first slice has only a
> profile for the private product and a pytest-style failure parser, and no Go profile
> (a private comparison note, not published). So T5's cost per goal is **not comparable** with
> the arms' cost per accepted item. [INFERENCE] The private product goals are probably harder: a median T5 builder round is
> 66–67 Claude Code turns, against T1's 50-step cap for a whole run, and `F-2.8-audit` has taken 21 rounds so far. The
> same-jobs comparison is the first thing a successor should run (§3).

## 1. Results

ADC reached checkpoint (a): its pipeline closed #170 as a reviewed candidate in 15.3 minutes on free cloud models,
after 5 live runs (#226 entry 212). In stage 2, ADC's own pipeline on the local Qwen3.8-27B closed 0 of 3 items:
each build asked for more output than the context allows, and a capacity stall held the builder for the rest of the
hour (#226 entry 219). On the same items, plain worker loops did better. In the cloud, mini-swe-agent accepted 36/36
and OpenHands 30/36. Locally, mini-swe-agent accepted 13/16 and OpenHands 11/16, at about one fourteenth of ADC's wall
time per accepted result (`2026-10-27-three-way-report.md` §3). Both workers meet the local acceptance rule. The
decision-table row is still open: OpenHands stopped on a text-only turn (#5510) in 3 of 16 local runs, but those runs
lacked the cloud prompt's "call the finish tool" line, and atolye reruns them with it. The gateway
chain front filter + agentgateway is adopted. The memory service works headless but did not help yet. T4 fails: neither
cheap rules nor Qwen3.8-27B (zero-shot, calibrated abstention) auto-decided any test round at a bar from 80 % to 95 %
(AUC 0.55; `2026-10-28-t4-model-results.md`, #226 entry 224), so the orchestrator stays a capable model and T4 is
re-run as the successor supervisor's decision log grows. T5 is closed by the operator's rule (the first 5 completed goals, in time
order, from the successor supervisor's design-named list): 5 of 5 done, with 30 builder rounds, about 11 h wall to verified,
$5.43 builder and $9.03–$10.41 supervisor (list-price estimates), and 0 operator messages; `F-2.8-audit` (release
0.3.20) is still open (`stage2/t5/t5-goals.jsonl`). These T5 figures do not compare with
the arms (see the caveat above). Full report: [`2026-10-29-stage2-report.md`](2026-10-29-stage2-report.md).

## 2. Salvaged parts

| Part | Paths (origin/main `9ee8a40a`) | How to reuse it |
|---|---|---|
| **P-A, the referee** (`adc dev referee`): judges any agent's patch against hidden tests; language-agnostic (Go, Python, TypeScript, PHP proven) | `internal/referee/{referee,results,work}.go`; CLI entry `internal/cli/cli.go:64` (`devReferee`), called from `cmd/adc/main.go`; schema `schemas/testing/referee-item.json`, doc `docs/reference/schemas/testing-referee-item.md`; items `fixtures/m1/referee-items/*.json` (16 T0 items) and `build-repos.sh` | Build `adc` from main (`make build` gives `bin/adc`). Write one item JSON per task: repo, base, pinned image, test command, results format, `editable`, `protected`, hidden acceptance, `hidden_tests`. Check each item: reference patch → `accepted`, empty patch → `rejected`. Treat `could_not_run` as infrastructure: judge again. Audit every accepted patch for harness markers (residual risk, see §4). atolye's sets G, P and T0 already use it (atolye's acceptance sets) |
| **The hidden-test overlay** (the gate's acceptance overlay for Go) | `internal/gate/acceptance.go` (`MaxOverlay`, `ErrAcceptance`), with `internal/gosel/gosel.go`; used by `internal/gate/gate.go` and `internal/daemon/gate.go`. P-A's language-agnostic overlay is in `internal/referee/work.go` | Reuse P-A's version for new work. The Go gate version shows how to refuse an acceptance without leaking its paths or names into logs. Hidden tests come from the acceptance files; the agent never sees them; a patch touching their paths is refused |
| **The gateway rules** (rules only; the service is not extracted) | `internal/gateway/{gateway,egress,count,capture,secrets,serve}.go`; `internal/gateway/hostile_test.go`; `internal/gateway/testdata/mutations.py` | A checklist for atolye's front filter + agentgateway: exact model pin with re-encoded body (`egress.go:114-143`); refuse redirects, dial only the registered origin (`egress.go:44-62`); redact echoed credentials, including JSON-escaped and write-split ones; count usage from the stream. Port the tests as cases, not the code |
| **The hostile-repository cases** | `fixtures/m1/cases/hostile-{gate-output,git-dir,instructions,model-switch,symlink,symlink-path,test-escape}/`; stub scenarios `fixtures/m1/stub-provider/scenarios/hostile-*.json`; controls `internal/fixtures/cycle_hostile_test.go`; schema `schemas/testing/cycle-case.json` | Run them against any worker sandbox: injected agent-instruction files (`AGENTS.md`, `CLAUDE.md`, `.cursorrules`), git-dir tricks (`.gitattributes`, `.gitmodules`), symlink escapes, a test that probes the environment, a model switch request. Each case states the expected refusal |
| **The review records, as T4 data** | `docs/reviews/` (507 files). Not on main: 19 review files and the external-review outputs, both in the lead's private workspace (not published) | The T4 labels are already extracted: `stage2/t4/labels.jsonl` (629 rounds) and `features.jsonl`; scripts `t4/t4_baseline.py` (rules; `--refresh` re-reads main and gh) and `t4/model/run_t4_model.py` (the model step). The earlier supervisor side re-runs both on its own decision log (`stage2/t5/t5-labels.jsonl`: 244 rows on 2026-10-29; no decision type has 200 yet, the largest has 35) |

**Stage-2 harnesses (outside the repository, kept as evidence):** `stage2/`:
`harness/` (mini-swe-agent `t1.yaml`, OpenHands runner), `t1/run_matrix.py`, `step0/`, `armA/`, `gateway/`, `t4/`,
`t5/` (`collect_t5.py`, read-only collector).

## 3. Not finished

- **The same-jobs comparison (run this first).** Arm B (the successor supervisor on sets G and P) needs a Go profile (not only the private product's)
  and a `go test` failure parser. The alternative is arm C on the private product items, with hidden acceptance written before
  any run, in P-A format. Until one of them runs, the earlier supervisor and the worker loops have no cost figures for the same jobs.
- **`F-2.8-audit`** (release 0.3.20): still open on the earlier supervisor's side; outside T5 now.
- **T4 re-runs:** wait for ≥ 200 labels per decision type in the successor supervisor's log.
- **The optional "A, tuned" pass** (config only): not run. It dies with ADC.
- **Set P repeat 2 and the 100-turn cap experiment**: atolye's, when the GPU is free.
- **ADC's own unattended operation:** never reached. Milestone M2 (OpenRouter, failover) was not started.

## 4. The open ledger

The lead's obligations ledger (in the private workspace, not published) has **52 rows** that end in `| open |`.

| Theme | Rows | Count | For a successor? |
|---|---|---|---|
| adcd daemon: placement, capacity, holds, rails, quota, reconcile, logging, review activation, board | Astra chunk 1 r3 (#226 148); #226 128; 5c-1 lead check; Opus 6b L3; #402; 6d shortcut; Opus #409 M1; Opus #410 L3; Opus #410/Build323; Mythos #408 r3 L8–L10; Astra #411 H1; Mythos #413 L1; Mythos #413 L2, L3 + record; Opus #414 L1, L2; I13 plan gap; Mythos #417 r2; #226 181 (K2b); Mythos #426; Opus #426 C2; Build307; #226 195; Opus #433; #226 210 | 23 | **Die with ADC.** One lesson survives from #226 210: a scheduler that reserves worst-case liability can stall with no visible hold, and a fixed output budget can exceed the context |
| Static analyzers and mutation tests | #226 122 (P14d-2); #226 129, 134 (P14d-w2); #370; #380 | 4 | Die |
| Live route, driver shell, secret and canary scans, stub harness | Opus #435 L3; Opus #435 r2 N1; #435 merge evidence; Opus #438; Opus #440 r2; Opus #441 L1; Opus #441 r2; Opus #441 r4 N2; #226 203 | 9 | Die. Lesson from #441 L1: register every encoded form of a canary (JSON-escaped included) before redacting |
| Fixtures, images, CI, #170 setup | #226 153; #226 172; #226 188; D1 fault-provider-4xx | 4 | Die |
| M2 features | #226 174 (OpenRouter); OmniRoute (#226 179) | 2 | Die. Provider pinning and failover belong to the gateway configuration now |
| Gateway rules | Opus #409 L2 (end-to-end gateway row); Astra #423 M1 (USD reservation per request); #226 193 (idle bound, call stop) | 3 | **Matter.** agentgateway also overshoots budgets under concurrency (3.1×) and does not cut in-flight streams on revoke (`stage2/gateway/T2-report.md`). Check them in atolye's stack |
| Review records not committed | Mythos #408 r3 record; Astra #411 re-review record | 2 | **Matter for T4 only.** The files are in the lead's private workspace and already in `labels.jsonl`; keep that workspace readable |
| Referee and hidden overlay | Opus #447 r3 Q1 (in-process result tampering); Opus #429 r8 L1 (quadratic `go/doc.Examples`, no deadline); Opus #446 A1 (acceptance outside the gate's packages) | 3 | **Matter.** #447 Q1 applies to P-A: keep the marker audit of every accepted patch. #429 L1 and #446 A1 apply if the Go overlay is reused |
| Stage 2 | #226 208 (run stage 2); stage2 t1 `run_matrix.py` hang after a token cap | 2 | #226 208 closes with this note (T5 closed 2026-10-29); T4 re-runs move to the earlier supervisor side. The `run_matrix.py` hang matters only if that runner is reused |
| **Total** | | **52** | 8 matter (gateway 3, review records 2, referee 3); 44 die with ADC (the two stage-2 rows close or move) |
