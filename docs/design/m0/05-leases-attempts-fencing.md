# 05 — Resource groups, leases, attempts, fencing, usage

M0 fixes the records and their state machines; the scheduler policy that fills them is M2. Implements plan §2.2 and
§2.5, Astra A3/A4 and M0 review [M6][M7].

## Resource groups

```yaml
ResourceGroup:
  id: rg_…
  kind: account_quota | broker_balance | gpu | concurrency | controller_gate | controller_integration
  capacity_model:
    unit: requests | tokens | usd | slots | gpu_seconds
    limit: number|null              # null = unknown
    window: duration|null
    reset_at: time|null
    quota_state: known | estimated | unknown
  release_rule: on_settle | on_attempt_end | on_window_reset   # how reserved amounts return [M7]
  members: [exe_…]
```

- **Rate windows and credit balances are separate groups** (`account_quota` vs `broker_balance`). A provider can have
  rate headroom and no credit, or the reverse (nöbetçi Δ§4: a rate-window check passed on an account with no credit).
- **A lease on a `broker_balance` group requires a fresh balance probe** (within the group's probe TTL) showing at
  least the reservation. Otherwise the lease waits on `broker_credit_exhausted` (02).

## Lease

```yaml
Lease:
  id: lea_…
  unit: unt_…
  role: string
  executor: exe_…
  reservations: [{group: rg_…, amount, unit}]     # all-or-nothing, inside the writer transaction (03)
  priority: int
  footprint: {context_tokens, expected_turns, gpu_seconds?}
  deadline: time
  fallback_step: int
  placement_checks: {data_floor, destinations, capability, independence, paid_authority, budget, inputs_applicable, fallback_scope}
```

- **`inputs_applicable`** (nöbetçi Δ§13): a lease for a role that consumes evidence (reviewer, phase reviewer) is granted
  only when every input the role requires is applicable to the exact candidate tree: gate results, receipts, answered
  owed inputs. If one is missing, the controller schedules its producer first (`inputs_not_applicable` hold) and never
  sends the reviewer an inapplicable or missing input.
- **`fallback_scope`** (nöbetçi Δ§4): a fallback step is allowed only if the role's chain entry is authorised for this
  **role and mode** (e.g. "learning digest" ≠ "phase review"). The check runs in the placement path, not only in a
  record. Moving to another family re-runs `independence`.
- **Footprint is learned per executor**, not per role. On an identical review packet, nöbetçi measured 9× the input
  tokens for one reviewer family versus another. `footprint` starts from priors (the nöbetçi measurement export,
  intake A12) and is updated from settled usage.

Complete state machine. Every transition is an event, and terminal states are immutable:

| From | Event | To |
|---|---|---|
| — | `lease.requested` | requested |
| requested | `lease.waiting{hold, signal}` | waiting |
| waiting | `lease.requested` (signal fired) | requested |
| requested | `lease.granted` (all reservations made atomically) | granted |
| granted | `attempt.started` | active |
| granted | `lease.expired` (deadline, no attempt) | releasing |
| active | `lease.completed` (attempt reached a terminal state) | settling |
| active | `lease.revoked{reason}` · `lease.preempted` · `lease.expired` | settling (the attempt is stopped first, see fencing) |
| settling | `lease.settled{usage_summary, liabilities}` | settled ✱ |
| releasing | `lease.released` | released ✱ |

- **Reservations return** per their group's `release_rule` at `settled`/`released`. A provider charge still uncertain at
  settlement becomes an **uncertain liability** on the group's ledger. Liabilities reduce available budget until
  reconciled, so a lease can settle without waiting forever on billing [M7].
- `placement_checks` are re-evaluated and recorded at every fallback step (A3/A7).

## Attempt and fencing [M6]

```yaml
Attempt:
  id: att_…
  unit: unt_…
  lease: lea_…
  fencing_token: int                # strictly increasing per unit; minted only by the controller
  supersedes: att_…|null
  base: {tree: git:…, contract_revision: …}
  status: (see table)
  publication: open | closed        # whether outputs may still land
  checkpoint: AttemptRecord|null
```

| From | Event | To | Publication |
|---|---|---|---|
| — | `attempt.started` | running | open |
| running | `attempt.stop_requested{reason: revoke|preempt|expire|drain|cancel}` | stopping | **closed atomically with the event** |
| running | `attempt.result_submitted` (valid token, admissible) | finishing | closed |
| running | `attempt.lost{reason: stream_dead|agent_disconnect|controller_restart}` | lost | closed |
| stopping · finishing · lost | `attempt.sealed{AttemptRecord}` | sealed | closed |
| sealed | `attempt.completed{outcome, reason}` | completed ✱ | closed |
| sealed (unit continues) | `attempt.superseded{by}` | superseded ✱ | closed |

Fencing rules:
- An output (result, patch, checkpoint contribution) is accepted only if **its attempt id and token exactly match** the
  unit's current attempt **and** that attempt's publication is `open` (finishing accepts only the one submitted result).
  Anything else is refused with `failed/stale_attempt` and recorded.
- Stopping closes publication in the same transaction as the stop event, so a revoked or interrupted attempt can never
  publish, even before a replacement starts.
- Supersession and allocation of the replacement's token happen in **one transaction**: `attempt.superseded` for the
  old, `attempt.started` for the new.
- On controller restart, every `running` attempt becomes `lost` (publication closed) before anything else runs.

## AttemptRecord (the checkpoint schema; K2, KA6, KA8)

Sealed at every builder exit (completed, repair handoff, drain, abrupt loss, controller restart where the sandbox
evidence manifest is intact):

```yaml
AttemptRecord:
  attempt: att_…
  project: prj_…
  task: tsk_…
  contract_revision: …
  fencing_token: int
  base_tree: git:…
  partial_diff: blob sha256:…|null
  failing_tests: [test_id]
  trail: blob sha256:…
  builder_notes: blob sha256:…|null # untrusted data
  evidence_manifest: blob sha256:…
  seal: {reason: completed|repair_handoff|drain|abrupt_loss|host_restart, at, sealed_by: act_…}
  pinned: bool                      # true while the unit can still resume (retention never purges a pinned record) [M14]
```

Consumption: builder continuations of the same project, task and contract revision only.

## Usage and settlement [M7]

**Outputs are fenced; accounting is not.** Usage reports are accepted from any attempt, stale or not, because a stale
attempt may still have been billed.

```yaml
UsageMeasurement:
  id: msr_…
  call: cal_…                       # call-ledger id, recorded in the call's intent (03 effects)
  attempt: att_…
  lease: lea_…
  kind: delta | final | provider_reconciliation
  sequence: int                     # per call; deltas are numbered
  tokens: {in, out, cache_read, cache_write}
  turns: int
  seconds: number
  cost: {amount, currency, basis: cash|subscription_equivalent|free|local, estimate: bool}
  source_of_truth: provider_usage | adapter_report | gateway_count | none
```

Reconciliation, done in one place (`internal/capacity/ledger`):
- Deltas are deduplicated by `(call, sequence)`.
- A `final` for a call **replaces** the sum of its deltas when it is larger or equal. When smaller, the larger figure is
  kept and flagged `inconsistent_usage`; usage never goes down without evidence.
- A `provider_reconciliation` supersedes both and is the only thing that can lower a figure.
- A call with no `final` and no reconciliation stays an **uncertain liability** at its best estimate.
- Usage is per call, never a session total. Cancellation is never assumed to stop billing.
- The call ledger is the single accounting owner. Manifests and receipts reference `cal_…`, never amounts.

## What M0 delivers

Schemas, event kinds and transition validators for ResourceGroup, Lease, Attempt, AttemptRecord and UsageMeasurement;
the ledger's reconciliation function; a deterministic fake scheduler. Tests:
- reservations are all-or-nothing;
- every lease reaches `settled` or `released`;
- a revoked attempt's late result is refused;
- a stale attempt's usage is still counted;
- duplicate and late deltas never double count;
- a `final` lower than the deltas does not lower usage without reconciliation.
