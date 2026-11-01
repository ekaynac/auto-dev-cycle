# Stage 2: the report from the adc-dev session (2026-10-28)

> **Paths.** `stage2/…` paths name files in the lead's private stage-2 workspace (not published). TW, BR, T3R and C are reports in `ekaynac/atolye`. `#226` is this repository's tracking issue.

**For:** the auto-dev-cycle (ADC) lead and the operator.
**Plan:** the stage-2 test plan, revision 4 (the lead's private workspace, not published): §2 tests, §4 decision table, §5 order.
**Decisions:** R17/R18 (the lead's decision log, not published): ADC finishes stage 2, then stops. #226 entries 207–224.
**Updated 2026-10-29:** the operator closed T5 (§1a). Data: `stage2/t5/t5-goals.jsonl` and
`t5-summary.md`, collected 2026-10-29 16:02.
**Not repeated here:** atolye's reports. They are cited: `2026-10-27-three-way-report.md` (TW),
`2026-10-27-blind-review.md` (BR), `2026-10-27-t3-kutuphane-memory.md` (T3R). The coordination log is
`2026-10-27-coordination-adc-dev-atolye.md` (C).

> **Read this first: T5 and the arms did not run on the same jobs.**
> - **T5** measures the earlier supervisor on **the private product** goals.
> - **Arms A and C and T1** ran on **set G** (ADC's Go items) and **set P** (public Python/TypeScript repositories).
>   The private product is private and was never in the item set.
> - **Arm B** (the successor supervisor on the same items) never ran. Its first slice has one profile, for the private product, and parses only
>   pytest-style `FAILED`/`ERROR` lines; set G needs a Go profile and a `go test` parser
>   (a private comparison note, not published).
> - **So T5's cost per goal is not comparable with the arms' cost per accepted item.**
> - [INFERENCE] The private product goals are probably harder. In T5's 13 jobs, the median builder round is 66 Claude Code turns
>   (`builder_turns ÷ builder_rounds` per job, `t5-goals.jsonl`; the comparison report gives 67). T1 capped a whole
>   run at 50 steps. `F-2.8-audit` has taken 21 rounds so far.
> - **The same-jobs comparison is still needed:** arm B, or arm C on the private product items with hidden acceptance (§4).

## Conclusion

1. **Decision-table row: open between "OpenHands passes locally" and "only mini-swe-agent passes locally".** Both
   workers meet the acceptance rule. The #5510 part is **open**, not a fail (correction of 2026-10-29, below).
   - Both workers meet the acceptance rule on the comparable items: OpenHands 90 %, mini-swe-agent 83 % of the cloud
     reference (rule: ≥ 60 %).
   - Locally, OpenHands stopped on a text-only turn in 3 of 16 runs (rule: ≤ 1 in 10); in the cloud, 1 of 36. **But
     the local runs lacked a prompt line that every cloud run had** ("When you are done, call the finish tool."):
     adc-dev told atolye an incomplete prompt (C §6.22–6.23). atolye reruns the 20 OpenHands cells with the line in the
     next GPU window. If the stops fall to ≤ 2/20, the row becomes "OpenHands passes locally".
2. **T2: the chain front filter + agentgateway is adopted.** ADC's gateway is not extracted. Its rules stay as
   reference.
3. **T3: the memory path works with no human, but it did not help.** the memory service `#121` (a search size budget) comes
   first.
4. **T4 fails with both the rules and the model.** Qwen3.8-27B, zero-shot with a calibrated abstention threshold,
   auto-decided 0 % of test rounds at every bar from 80 % to 95 % (AUC 0.55). The orchestrator stays a capable
   model; T4 is re-run as the successor supervisor's decision log grows.
5. **T5 is closed: 5 of 5 goals done**, by the operator's rule (2026-10-29): the first 5 goals completed, in time
   order, from the successor supervisor's design-named list. Five goals cost 30 builder rounds, about 11 h wall to verified,
   $5.43 builder and $9.03–$10.41 supervisor (list-price estimates), with 0 operator messages. `F-2.8-audit`
   (release 0.3.20) is still open. See §1a, and the caveat above: these figures do not compare with the arms.
6. **ADC itself closed 0 of 3 items locally** (arm A). This supports R17/R18.

## 1. Results against the pass conditions

| Test | Pass condition (plan §2) | Result | Verdict | Evidence |
|---|---|---|---|---|
| Step 0 | none (sizing) | Parallel 4 is the knee: about 77 tok/s warm aggregate, about 12.5 runs per hour, 19 min model time per 40-turn run. Prefill (1.5–1.8k tok/s) is the bottleneck. Cold first token up to 148 s | done | `stage2/step0/step0-report.md`; C §6.1 |
| T1 cloud | reference only | 72 cells, 66 accepted. mini-swe-agent 36/36. OpenHands 30/36; all 6 rejections at the 50-turn or wall cap. 0 of 66 accepted patches flagged by the marker audit | done | `data/t1-cloud-{deepseek,qwen3.8}.csv`; #226 entry 220 |
| T1 local, mini-swe-agent | local acceptance ≥ 60 % of cloud | 10/12 = 83 % against 36/36 = 100 % → **83 %** | **pass** | §2 below |
| T1 local, OpenHands | local acceptance ≥ 60 % of cloud; #5510 stops ≤ 1 in 10 runs | acceptance 9/12 = 75 % against 30/36 = 83 % → **90 %**; #5510 stops **3/16 = 19 %**, in runs without the cloud prompt's finish line | **acceptance pass; #5510 open** (rerun with the same prompt pending) | §2 below; C §6.22–6.23 |
| T1 local, arm A (ADC) | not in the rule (ADC is not a candidate worker) | ADC closed 0/3. P-A accepted 2 of 3 work trees at the 60-min wrapper limit | — | `data/armA.jsonl`; `stage2/armA/README.md`; TW §3 |
| T2 | adopt agentgateway, or extract ADC's gateway | agentgateway alone 7/8 (check 3 fails). Front filter + agentgateway 8/8, then 9/9 after review | **adopted: front filter + agentgateway** | ``ekaynac/atolye` `docs/gateway.md`` "T2 results"; #226 entry 218 |
| T3 | works with no human; turns or reuse no worse | 18/18 runs headless, 0 infrastructure errors. Acceptance 8/9 with memory, 9/9 without. Input tokens +50 %. Turn-cap stops 8/9 against 5/9. Blind score 4.00 against 4.22 | **part 1 pass; part 2 fail** | T3R "Verdict"; BR "Verdict" 4 |
| T4 | reference accuracy ±5 points, ≥ 30 % auto-decided | Labels: 629 review rounds (354 accept, 275 repair); time-ordered split, 440 train, 189 test. **Rules:** at train precision ≥ 0.90, 0 % auto-decided; mined rules 28.6 % coverage at 57.4 % accuracy (about the base rate). **Model** (Qwen3.8-27B NVFP4, zero-shot, Platt-calibrated logprob or stated score, abstention threshold fitted on train): reference accuracy 1.0, so the bar is ≥ 95 % agreement on ≥ 30 % of rounds; **0 % of test rounds auto-decided at every bar from 80 % to 95 %**, both scores. AUC 0.545 (logprob) / 0.53 (stated). Agreement at full coverage 54.5 % / 57.7 %, against a 58 % accept base rate (110 of 189) | **fail** (rules and model) | `stage2/t4/T4-baseline.md` "Result"; `2026-10-28-t4-model-results.md`; #226 entries 217, 224 |
| T5 | none (the bar for T4) | **5 of 5 goals done** under the operator's rule: `F-2.8-fix-sources-api`, `F-2.8-polish-13-16`, `F-2.8-12`, `F-2.8-default-landing`, `1027-074049-0f23` (UI polish). `F-2.8-audit` open (partial row). These are the private product's goals, not the arms' items. Arm B was not run | **closed** (the earlier supervisor side; collected read-only by adc-dev) | §1a; `stage2/t5/t5-goals.jsonl`; `t5-summary.md` |

**adc-dev's own T2 run** (`stage2/gateway/T2-report.md`) found the same check-3 failure for agentgateway alone. Its
rule branch said "extract ADC's gateway". Revision 4 and #226 entry 218 replaced that branch with atolye's front
filter. The adc-dev run is kept as evidence only. It adds gaps that a deployment must configure: usage is 0 when a
client sends `include_usage:false`; budgets overshoot under concurrency (3.1× in check 6); revoking a key does not cut
an in-flight stream; there is no native key TTL.

## 1a. T5: the earlier supervisor's supervisor baseline

**The five goals** (operator's rule, 2026-10-29): the first 5 completed goals, in time order, from the successor supervisor's
design-named list.
- The 4 design-named goals that are done.
- The next completed goal in time order: `1027-074049-0f23`, "fix(frontend): session header, composer, empty
  and failure states, default profile" (verified, merged).
- `1026-155834-987d` (TrustCurve) started earlier. It was a trial outside the cutover list, so it is left out.
- `F-2.8-audit` (release 0.3.20) is a partial row: open since 2026-10-27 08:42; main is at 0.3.19.

All figures are from `stage2/t5/t5-goals.jsonl`. **Supervisor figures are estimates, not
bounds.** *Tagged* counts the messages that name the goal; *window* counts all supervisor messages while the goal was
open, split across the goals open then (`t5-summary.md` "How to read"). Supervisor tokens = input + cache read + cache
write. Both are priced at Anthropic list price; the subscription is not billed per token. Builder $ is the OpenRouter
key delta; the key is shared, so overlapped rounds can include other jobs' spend.

| Goal | Outcome | Jobs / rounds / reviews (GO) | Supervisor, tagged: in tok / out tok / msgs / $ | Supervisor, window: in tok / out tok / msgs / $ | Builder: in+cache tok / out tok / $ | Reviewer tok | Wall to verified / merged | Human |
|---|---|---|---|---|---|---|---|---|
| F-2.8-fix-sources-api | verified, merged | 1 / 1 / 1 (1) | 2.06M / 9.1k / 12 / $0.74 | 1.74M / 3.9k / 9.5 / $0.50 | 1.84M / 7k / $0.00 (1 round unpriced) | 31k | 0.30 h / 0.32 h | 0 msgs; minutes unrecorded |
| F-2.8-polish-13-16 | verified, merged | 3 / 9 / 9 (6) | 5.67M / 8.8k / 19.5 / $1.42 | 6.61M / 7.7k / 23 / $1.61 | 26.40M / 218k / $1.70 | 440k | 3.05 h / 3.07 h | 0 msgs; minutes unrecorded |
| F-2.8-12 | verified, merged | 2 / 14 / 15 (5) | 20.23M / 30.2k / 54.2 / $5.33 | 15.80M / 21.4k / 41.5 / $3.92 | 82.70M / 577k / $3.17 | 1.03M | 5.39 h / 5.39 h | 0 msgs; minutes unrecorded |
| F-2.8-default-landing | verified, merged | 1 / 2 / 2 (1) | 2.88M / 4.8k / 7.2 / $0.75 | 5.02M / 5.9k / 12.5 / $1.22 | 12.20M / 56k / $0.33 | 104k | 1.30 h / 1.50 h | 0 msgs; minutes unrecorded |
| 1027-074049-0f23 (UI polish) | verified, merged | 1 / 4 / 4 (2) | 3.37M / 2.9k / 7.2 / $0.79 | 13.42M / 12.0k / 29 / $3.17 | 10.05M / 63k / $0.23 | 209k | 1.02 h / 1.02 h | 0 msgs; minutes unrecorded |
| **Total, 5 goals** | **5 of 5 done** | **8 / 30 / 31 (15)** | **34.20M / 55.8k / 100.1 / $9.03** | **42.59M / 50.9k / 115.5 / $10.41** | **133.20M / 922k / $5.43** | **1.81M** | **11.06 h / 11.30 h** (sum) | **0 msgs; minutes unrecorded** |
| F-2.8-audit (partial, open) | 2 of 5 jobs verified; release 0.3.20 not cut | 5 / 21 / 21 (11) | 34.15M / 65.7k / 90.3 / $9.04 | 126.66M / 216.5k / 324.4 / $40.20 | 98.96M / 703k / $12.92 (12 rounds overlapped) | 933k | – (builder wall 13.6 h so far) | 26 operator msgs in window, 0 tagged; minutes unrecorded |

**Human time:** no goal records human minutes (`human.minutes` is null in every row). Only operator messages in the
orchestrator session are counted; Telegram replies and direct actions are not seen. the earlier supervisor backfilled estimated
`human_intervention` rows (the supervisor side's reply in `stage2/t5/` §3).

**Decisions by type** (`decisions_recorded` via `record_decision`; `decisions_implicit` from jobs and events):

| Goal | Recorded | Implicit |
|---|---|---|
| F-2.8-fix-sources-api | accept 1, goal_assign 1, merge 1 | delegate 1, publish 1 |
| F-2.8-polish-13-16 | goal_assign 3, merge 3 | continue_recheck 3, delegate 3, publish 3 |
| F-2.8-12 | accept 1, goal_assign 2, merge 2, re-plan 1 | continue_amend 1, continue_recheck 1, delegate 2, publish 2 |
| F-2.8-default-landing | goal_assign 1, merge 1 | delegate 1, publish 1 |
| 1027-074049-0f23 | goal_assign 1, merge 1 | delegate 1, publish 1 |
| **Total, 5 goals** | **19:** goal_assign 8, merge 8, accept 2, re-plan 1 | **21:** delegate 8, publish 8, continue_recheck 4, continue_amend 1 |
| F-2.8-audit (partial) | goal_assign 1, human_intervention 1, merge 3 | cancel 9, continue_amend 6, continue_recheck 5, delegate 5, publish 2 |

**T4 labels from the runtime log** (`stage2/t5/t5-labels.jsonl`, all work items since 2026-10-26): **244 rows.**
delegate 35, continue_amend 35, cancel 34, merge 30, continue_recheck 28, publish 26, goal_assign 23,
human_intervention 16, accept 5, goal_close 4, re-plan 3, goal_open 3, accept_or_fix 1, add_scope 1.
**No type reaches the plan's 200-label threshold**; the largest has 35.

## 2. The T1 rule, computed

**Comparable items:** the six items that both T1 cloud and arm C ran: `issue-170`, `adc-events-last-event-id`,
`adc-registry-origin-spelling`, `adc-graph-history-held`, `py-mini-feature-1`, `ts-mini-bugfix-1`. Set P has no cloud
cells, so it is left out. Per-item counts: TW §2.

| Worker | Local accepted (arm C, Qwen3.8-27B) | Cloud accepted | Local ÷ cloud | Rule ≥ 60 % |
|---|---|---|---|---|
| OpenHands | 9/12 = 75.0 % | deepseek 16/18 = 88.9 %; qwen3.8-flash 14/18 = 77.8 %; both 30/36 = 83.3 % | 84 % / 96 % / **90 %** | pass |
| mini-swe-agent | 10/12 = 83.3 % | deepseek 18/18; qwen3.8-flash 18/18; both 36/36 = 100 % | **83 %** | pass |

All local runs, set P included: OpenHands 11/16, mini-swe-agent 13/16 (TW "Verdict" 2). The ratio passes against
each cloud model alone too, so the choice of reference does not change the result.

**#5510 stops** (an OpenHands run that ends on a text-only turn). Classification rule: status `finished` and the last
event is a `MessageEvent`, not the finish tool (`stage2/harness/openhands/run_openhands.py:118-121`). The same rule
is applied to arm C's `last_agent_event` field.

| Cell | Runs | #5510 stops | Rate | Rule ≤ 1 in 10 |
|---|---|---|---|---|
| OpenHands, cloud (both models) | 36 | 1 (qwen3.8-flash; accepted) | 2.8 % | pass |
| OpenHands, local (arm C v2) | 16 | **3** (`ts-mini` r1, `issue-170` r1, `adc-events` r1; all accepted) | **18.8 %** | **open**: these runs lacked the finish line |

Sources: `data/t1-cloud-qwen3.8.csv` `stop_cause`; atolye's arm-C run record `arm-c-v2.jsonl`
`last_agent_event`.
- atolye's reports do not count #5510 stops. This count is adc-dev's, from atolye's rows.
- All three stops were accepted, so #5510 cost no acceptance here. The rule counts stops "with or without a recorded
  patch", so the result is still a fail.
- [INFERENCE] The margin is small: with 16 runs, one stop fewer (2/16 = 12.5 %) still fails, and two fewer passes.

**Decision row (plan §4): open.** With the stop count above it would be "only mini-swe-agent passes locally". But the
local OpenHands runs did not get the same prompt as the cloud runs: they lacked the line "When you are done, call the
finish tool." adc-dev gave atolye an incomplete prompt in C §6.6, and corrected it in C §6.22. atolye's PR #17 now
sends the same text. [INFERENCE] The missing line plausibly explains most of the gap.
- **If the rerun** (arm C's 20 OpenHands cells with the line, 50 turns) gives ≤ 2/20 stops: "OpenHands passes
  locally": OpenHands is the worker for every profile, and #5510 is a prompt matter for us, not an upstream report.
- **Otherwise:** "only mini-swe-agent passes locally": OpenHands for the cloud profiles, mini-swe-agent as the local
  worker, and #5510 goes upstream (`software-agent-sdk#5510`; R19).

This agrees with atolye's cost reading (TW "Verdict" 3: mini-swe-agent 0.48M against 1.05M input tokens per
accepted result) and its role reading (OpenHands where MCP tools, a browser or the Agent Server API are needed).

**Other rows:**
- **T2:** "agentgateway (configured)", with atolye's front filter in front.
- **T3:** the memory service's P1 order: `#121` (compact search results) first, then a T3 item that depends on a recorded
  decision (T3R §4).
- **T4:** "T4 fails": the orchestrator stays a capable model; re-run T4 as the log grows. The runtime labels from T5
  (`stage2/t5/t5-labels.jsonl`, 244 rows) are below the plan's 200-label threshold for every decision type (largest
  35), so the re-run waits for more log.
- **T5:** no row in the table; it is the bar for T4. §1a gives that bar.

## 3. Caveats

| Caveat | Effect | Source |
|---|---|---|
| **Sample size.** Local: 1–2 repeats per cell (16 runs per worker). Arm A: 1 run per item, 3 items. T3: 9 per condition. BR: 2–13 diffs per group | Every local rate has a wide interval. The #5510 verdict depends on one or two runs | TW §5.3 |
| **`vllm_exclusive` false.** An outside user was on vLLM all day | Arm C 1 of 32 rows exclusive; arm A 0 of 3; step 0 had a foreign 20k-token request. Prefix-cache shares are not reported | TW §5.1; step-0 report "Exclusivity" |
| **Prompt versions.** Arm C pilot (atolye prompts, 16 runs) is excluded. Arm C v2 uses T1-cloud's prompts (`prompt_version` `t1-cloud-prefix` for OpenHands, `t1-cloud:5011ca644369` for mini-swe-agent). Arm A uses ADC's own pipeline. T3 "with" adds one hint line | Local against cloud differs only in model and wall limit. A against C differs by design. mini-swe-agent's template forbids test edits, which lowers its blind score | C §6.7; TW §1; BR "Verdict" 3 |
| **Cloud reference model.** The plan named Evren `glm-5.3`. The operator replaced it with `qwen3.8-flash-next` and `deepseek-v4.1-flash` (glm-5.3 made no change in live runs 3–4) | The ratio passes against each model alone | #226 entries 211, 213 |
| **Caps hit.** 50-turn/step cap: 6 of 8 local rejections. OpenHands cloud: 16 of 36 runs at the turn cap, 2 at the wall cap. Wall limit 30 min cloud, 60 min local. T1-cloud's 12M tokens per provider per day stopped the matrix at 26 cells (resumed next day). Arm A: all 3 runs at the 60-min wrapper; the 14:45 cut-off dropped `adc-graph-history-held`; ADC's requested output plus prompt exceeded the 262,144-token context | Failures are mostly budget, not wrong code. The cap experiment (100 turns) is not run | TW "Verdict" 4; #226 entries 216, 219; C §6.1 14:48 |
| **Training contention after 18:02.** `track-a/train.py` resumed at about 18:02 with no marker. 9 of 16 runs per worker ran beside it (set P v2 from `humanize-parse-size`, and all r2 rows) | Verdicts and tokens stand. Wall times of those runs are excluded. No run reached the 60-min wall cap | C §6.10, §6.11; TW §5.2 |
| **P-A residual.** Solution code inside the test process can change the results (10-line probe) | Every accepted patch was audited for harness markers: 0 flagged in T1 cloud | #226 entry 220; obligations row "Opus #447 r3 Q1" |
| **Protected-path touches.** OpenHands touched paths outside `editable` or in `protected` in 6/16 local and 24/36 cloud runs (mini-swe-agent 1/16 and 1/36) | P-A restores them, so verdicts stand. It is a containment cost of OpenHands | TW §3; C §6.5 |
| **T4 model deviations.** The decider is Qwen3.8-27B on the existing vLLM, not Kev-4B/9B (a second model server is not approved). Zero-shot, no fine-tune. Decision-time input is reconstructed (the reviewed head can be a later fix commit). 582 of 629 diffs are truncated to a 6,144-token cap. 32.8 GPU-min, `vllm_exclusive` false; one invocation stopped on a foreign GPU process (the training) | A fine-tuned small model with full diffs may do better. [INFERENCE] With AUC 0.55 at zero shot, the gap to the bar is large | `2026-10-28-t4-model-results.md` "Deviations"; #226 entry 224 |
| **T5 is not on the arms' jobs.** T5 ran the private product goals; the arms and T1 ran sets G and P. Arm B never ran | T5's cost per goal does not compare with any arm's cost per accepted item. See the box at the top | `t5-summary.md`; TW "Verdict" 5; a private comparison note, not published |
| **T5 measures.** Human minutes are not recorded; the earlier supervisor backfilled estimated `human_intervention` rows. Supervisor cost has two estimators (tagged, window), neither a bound. Builder $ uses a shared OpenRouter key (overlapped rounds include other jobs; 1 round unpriced). The fifth goal comes from the operator's rule, not from the collector's design slot (its `t5_slot` is empty), so `t5-summary.md`'s "Done goals together (4)" line leaves it out | Supervisor and human cost per goal are approximate | `t5/t5-summary.md` "How to read"; the supervisor side's reply in `stage2/t5/` |

## 4. Still open

| Item | Owner | What it needs |
|---|---|---|
| **The same-jobs comparison (first for a successor):** arm B (the successor supervisor on sets G and P), or arm C on the private product items with hidden acceptance | atolye / the earlier supervisor | For arm B: a Go profile (not only the private product's) and a `go test` failure parser in the successor supervisor. For arm C on the private product: items with hidden acceptance written before any run, in P-A format |
| `F-2.8-audit` (release 0.3.20) | the earlier supervisor side | Outside T5 now; its partial cost is in §1a |
| T4 re-run as the decision log grows | the earlier supervisor side, with adc-dev's scripts (`stage2/t4/t4_baseline.py`, `stage2/t4/model/run_t4_model.py`) | ≥ 200 labels per decision type in the successor supervisor's log (now 244 rows in all; largest type 35) |
| Optional "A, tuned" (config only: smaller output budget, 2 builder slots) | nobody | The GPU slot was offered at 19:20 and not claimed; it is dropped with ADC's stop |
| Set P repeats (repeat 2, 8 cells) and the 100-turn cap experiment | atolye | The GPU, when the operator frees it. Start only after `nvidia-smi` shows no non-vLLM process (atolye PR #11) |
| The OpenHands #5510 rerun (20 cells, with the cloud prompt's finish line); upstream #5510 only if stops stay above 2/20 | atolye | The GPU window (D3) |
