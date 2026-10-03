# 02 — Vocabulary: outcomes, reasons, holds, ids, times

## Outcome

`schemas/vocab/outcomes.yaml` defines exactly four values. Every result type in every schema carries one. There is no
boolean `ok`, `passed` or `success` field anywhere (lint-checked).

| Outcome | Meaning | Example |
|---|---|---|
| `found` | The check ran and found something: a change was produced, a defect exists, evidence is present | builder produced a patch; gate found failures |
| `empty` | The check ran completely and found nothing | gate ran, no deviations; no findings |
| `could_not_run` | The check did not run, or did not run completely, so nothing is known | provider quota, missing baseline, unreadable output |
| `failed` | The check ran and the subject failed a hard requirement, or the operation itself failed after starting | protected path touched; merge refused |

Rules:
- `could_not_run` is never read as `empty`. A consumer that needs `empty` and gets `could_not_run` must hold or retry,
  never pass. This is enforced in `internal/vocab` helpers (`RequireEmpty(result)` returns an error for
  `could_not_run`).
- Every non-`empty` result carries a non-empty `detail` string. Error text falls back to the error type name, never
  "" (nöbetçi lesson 1.1: an empty `AssertionError('')` was read as success).

### Disposition is not an outcome [M13]

A **command** to the controller first gets a *disposition*: `applied | proposed | refused`. The work it triggers later
produces results with the four outcomes. The two are never mixed in one field: a `proposed` command is not an outcome,
and a `refused` command carries its own `(outcome, reason)` explaining the refusal.

## Reason codes

`schemas/vocab/reasons.yaml` is the registry. Each entry:

```yaml
- code: quota_exhausted            # snake_case, unique, never reused for another meaning
  outcomes: [could_not_run]        # outcomes this reason may accompany
  domain: capacity                 # capacity | cycle | gate | review | authority | knowledge | forge | sandbox | adapter | ops
  description: The leased source reported its quota as exhausted.
  hold: quota_reset                # optional: the hold reason this leads to when it stops work (see below)
  introduced: 0.1.0
  deprecated: null                 # deprecated codes stay readable forever; writers may not emit them
```

Rules:
- Writers may emit only registered, non-deprecated codes. Readers accept every registered code ever issued.
- `routing_violation`, `quota_unknown`, `stale_attempt`, `protected_path`, `test_integrity_violation`,
  `evidence_inapplicable`, `broker_credit_exhausted`, `precondition_failed`, and similar are reason codes, not
  outcomes (Astra A15).
- **Outcomes come from observed facts** [D40, G3]. A build's outcome combines observed **completion** with the
  observed **diff** (13 "Facts, not claims"). No changes plus incomplete execution is `could_not_run`, never `empty`. Adapter and harness exits only fill the **reason** field, through one
  table (`internal/vocab/exitmap`). A 402 mid-run with edits is a sealed patch with reason `broker_credit_exhausted`,
  never "no changes" (nöbetçi Δ§14).
- `adc dev vocab check` scans the Go and TS code for reason literals and fails on unregistered ones.

## Hold reasons and resume signals

In the artifact graph (13), a node waiting on an internal input waits **structurally**: its signal is the input's
artifact, and no hold record is needed. The registry below names the **external** waits (approvals, quotas, human
acts, probes) [D40].

`schemas/vocab/holds.yaml`. A hold is a stop that must name what restarts it (nöbetçi PLANNING_CANON §16). Each entry:

```yaml
- hold: quota_reset
  signal: time                    # time | receipt | revision | approval | human_act | probe | config_change
  signal_detail: earliest known reset of the blocking resource group; if none known, the probe schedule applies
  owner: system                   # system | operator | supervisor | project_owner
  page_after: PT1H                # ISO-8601 duration before the pager is involved; null = never page
```

Rules:
- The watchdog and `adcd` refuse to start if any reachable code path can emit a hold not in the registry (checked at
  build time by the vocab check, and at runtime on emit).
- Every hold record carries the concrete signal instance (e.g. the reset timestamp, the approval id awaited).
- Pages are deduplicated by **event** (hold id + signal instance), never by channel (nöbetçi lesson: dedup by name
  ignored every receipt after the first).

Initial hold registry (M0 defines; later milestones add):

| Hold | Signal | Owner |
|---|---|---|
| `quota_reset` | time / probe | system |
| `source_invalid_config` | config_change | operator |
| `awaiting_approval` | approval | operator or supervisor (per grant) |
| `awaiting_confirmation` | approval (human or independent review per policy) | per confirmation policy |
| `awaiting_receipt` | receipt | system |
| `reviewer_unavailable` | probe (an independent reviewer family becomes available) | system |
| `wrong_owner` | human_act | the named owner (environment, access, tooling) |
| `maintenance_boundary` | revision (staged install lands) | system |
| `budget_rail` | approval (raise) | operator |
| `base_moved_conflict` | revision (rebased candidate) | system |
| `broker_credit_exhausted` | probe (balance probe passes) or human_act (operator top-up); never a time | operator |
| `awaiting_input` | approval that `answers` the owed input id (04) | operator or supervisor |
| `evidence_restore_pending` | lease settlement of the `evidence_target` group (restore verified, 13) | system, then operator |
| `precondition_failed` | probe (precondition passes) | the environment owner named in the profile |

**`page_after` defaults come from data** (nöbetçi stall profile, 382 terminals: long tails of 4–17 h sat on holds
whose signal was a human act or unwatched external state). Defaults:
- operator-owned holds: `PT1H`;
- system holds with a time signal: page when the signal is overdue by `PT30M`;
- `maintenance_boundary`: never page under `PT45M`.

Projects can override.

## Identifiers

Standard **TypeIDs** (jetify TypeID spec): `<prefix>_<26-char base32 encoding of a UUIDv7>`, lowercase (Q2). Ids are
roughly time-sortable, but the event store's `seq` is the authoritative order. Examples:

| Prefix | Entity |
|---|---|
| `prj` | project |
| `tsk` | task |
| `unt` | unit |
| `att` | attempt |
| `lea` | lease |
| `src` | source |
| `exe` | executor |
| `rcp` | receipt |
| `apr` | approval record |
| `grt` | grant |
| `prp` | proposal |
| `act` | actor |
| `cmf` | context manifest |
| `evt` | event |
| `eff` | external effect (03) |
| `cal` | model call (call ledger) |
| `chl` | confirmation challenge (04) |
| `own` | owed input (04) |
| `ejb` | evidence job (12) |
| `run` | evidence run (12) |
| `tgt` | evidence target (12) |
| `rg` | resource group (05) |
| `msr` | usage measurement (05) |
| `nod` | graph node (13) |
| `res` | graph result (13) |
| `cmd` | accepted command (03 `commands`) |

Ids are minted by the controller only. Adapters and agents never mint authoritative ids.

## Digests and times

- Content digests: `sha256:<64 hex>`. Git object ids are carried as `git:<sha1|sha256 hex>` with the algorithm explicit.
- Times: RFC 3339 in UTC with a `Z` suffix, from the controller clock (`time.Now().UTC()`), never from a model or an
  adapter's claim. Adapter-reported times are kept as `reported_at` alongside the controller's `observed_at`
  (nöbetçi lesson: write times from the clock, never from feel).
- Durations: ISO 8601 (`PT30M`).
