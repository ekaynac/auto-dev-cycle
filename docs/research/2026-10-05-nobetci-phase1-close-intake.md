# nöbetçi Phase 1 close intake (2026-10-03 → 05): what we take, and why

Date: 2026-10-05. Source: the nöbetçi supervisor's final supplement,
`/home/mega/nobetci-dev/docs/handoff/2026-10-05/09-phase1-close-lessons.md` (nobetci-dev commit 779a0ee, read-only),
cited as **L1..L8**. It supplements the reports filtered in `2026-10-03-nobetci-delta-intake.md`, whose criteria
apply unchanged:
- the lesson is general;
- it closes a failure class the plan or M0 notes leave open;
- its cost is a contract line or a fixture;
- it does not reopen an operator decision.

D40 ("design out, don't patch") applies: each accepted item says whether it is closed by construction or needs a
rule. This document proposes; the plan and note changes it names follow only once the operator accepts it.

## Accepted

| # | Item | Source | Where it lands | Closed by |
|---|---|---|---|---|
| B1 | **The reviewer roster is part of the review's identity.** The reviewers' executors and families are inputs to the review node's key. A change of who reviews is a new review node, never a hand-over inside an old one. A hold that waits on a removed reviewer is retired by that new node | L3 | m0/13 (review node inputs); m0/05 (independence is checked per node) | **Construction**: the graph key changes, so nothing can "continue" a round with different members. Our notes key reviews on conclusions and inputs, but say nothing about the roster |
| B2 | **New required deploy-time configuration is an operator question before merge.** The project profile names where the product declares its required configuration (a registry file or a command, product-neutral). The gate diffs it between base and candidate. A new required key holds the merge with an operator question listing the keys; it is never discovered by a post-deploy verifier | L4 | m0/07 (`deploy.required_config` in the profile); plan §2.13 deploy; a T0 fixture (a candidate adding a required key holds before merge) | **Rule plus fixture**: the diff is mechanical, the answer is the operator's |
| B3 | **Cited evidence must exist and run.** Every test id an acceptance row or a claim cites must have been collected in the gate run for this tree. An id never collected (a file nobody wrote) makes the row `could_not_run`, never a pass. This applies to every gate, not only the strict profile options | L5 | plan §2.6 (move the `test_ids` check from "strict options" to the one gate path); m0/06; a T0 fixture (a claim citing an unwritten test) | **Construction**: a pass row is computed from collected results, so a placeholder has nothing to pass on. Today it is a strict-profile option only |
| B4 | **An operator's exclusion of a provider is bound policy, not memory.** "Don't use X" is a typed authority action that changes the routing policy every placement reads (`destinations` / `fallback_scope`, 05). Recovery and fallback paths have no other route to launch. With no authorised route, the work holds; it never falls back | L1 | m0/04 (an authority action for a source exclusion with its approval); m0/05 placement checks; a T0 fixture (recovery after the exclusion does not launch on X) | **Mostly construction**: every launch is a lease through the same placement checks. The new piece is the typed exclusion and the fixture proving recovery honours it |
| B5 | **Tests use the shape of the live record.** Fixtures for record handling are built from captured live records (anonymised), not from the shape of the last incident. A killed call (no terminal record, no usage) is one of them | L2 | m0/11 (with A11's incident corpus); conformance kit (09) | **Data**: the corpus rule. The killed call itself is already closed. The exit map (`internal/vocab/exitmap`, #76) gives a process that ended without a result `adapter_protocol_error`, and the ledger keeps the call as an uncertain liability (05) |

## Already covered

| Source | Where |
|---|---|
| L2 dispositions with a cause (`provider_limit` / `wall_cut` / `killed_or_unknown`) | The exit map's precedence: capacity causes, the controller's stop request (a wall cut is `attempt.stop_requested{expire}`), then a protocol break (#76) |
| L7 credit and window limits are distinct | A4: separate rate and balance groups; `broker_credit_exhausted` vs `quota_exhausted` |
| L8 questions block monitoring | A10: questions are asynchronous decision packets; a liveness check comes first after an answer |

## Deferred

| Item | Source | When |
|---|---|---|
| Release tooling runs in its own declared environment (the product's locked dependencies) | L6 | With the release plugin (plan §2.13) and A6's environment facts |
| The F-1.5 coverage findings (D5 EX-3, D6 EX-2, D10) | 09 §F-1.5 | InfraMedic product work, not framework: they are the operator's Phase 2 intake |
