# 05 — Resource groups, leases, attempts, fencing, usage

M0 fixes the records and their state machines; the scheduler policy that fills them is M2. Implements plan §2.2 and
§2.5, Astra A3/A4 and M0 review [M6][M7].

## Resource groups

```yaml
ResourceGroup:
  id: rg_…
  kind: account_quota | broker_balance | gpu | concurrency | controller_gate | controller_integration | evidence_target
  capacity_model:
    unit: requests | tokens | usd | slots | gpu_seconds
    limit: number|null              # null = unknown
    window: duration|null
    reset_at: time|null
    quota_state: known | estimated | unknown
  release_rule: on_settle | on_attempt_end | on_window_reset | on_restore_verified   # how reserved amounts return [M7]; evidence targets use on_restore_verified (13) [D40]
  members: [exe_…]
```

- **Rate windows and credit balances are separate groups** (`account_quota` vs `broker_balance`). A provider can have
  rate headroom and no credit, or the reverse (nöbetçi Δ§4: a rate-window check passed on an account with no credit).
- **A lease on a `broker_balance` group is admitted by the ledger's affordability rule** (below, broker affordability). Otherwise it waits on `broker_credit_exhausted` (02).

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
  placement_checks: {data_floor, destinations, capability, independence, paid_authority, budget, fallback_scope}
```

- **Inputs are not a placement concern** [D40]. A node is runnable only once all its inputs resolved to applicable artifacts (13). Missing or inapplicable inputs never reach placement; they make the node `waiting(input)`, and the producer is scheduled.
- **Broker affordability goes through the ledger** [PR5-10]: a `broker_balance` reservation is admitted only if
  (latest probed balance − outstanding reservations − uncertain liabilities) ≥ the reservation, with the probe within
  its TTL. A stale or unknown balance means wait (`broker_credit_exhausted` with a probe signal), never "assume
  affordable".
- **Exclusions hold on every path, up to launch** (04 "Source exclusions", intake B4).
  - The `destinations` check refuses any source, executor or family in the effective exclusion set.
  - Every launch is a lease through placement: a first placement, a retry, a recovery after a restart or a provider
    limit's reset, and every fallback step. So an exclusion binds all of them, and none needs to remember it.
  - **The launch boundary is the start of an attempt.** The transaction that records `attempt.started` re-reads the
    exclusion set and refuses to start on an excluded executor. That transaction is the launch authorization, and
    nothing is dispatched to an executor before it commits. A placement that passed is therefore not enough.
  - A lease granted but not yet started whose executor becomes excluded is refused at that boundary:
    `lease.start_refused{reason}` moves it to `releasing`, its unused reservations return at `released`, and no
    attempt exists. The controller then requests a replacement lease, which goes through placement. The event is in
    `schemas/records/lease-event.json` and `internal/lease`; the adapter host records it for a failed prelaunch
    capability check (09).
    Restart recovery starts nothing without passing the same boundary.
  - An attempt that has **started** before the exclusion is not cut by placement. Stopping it is a separate
    operator action (`attempt.stop_requested`).
- **No authorised route waits, never falls back.** When exclusions, the data floor, destinations or the fallback scope
  leave no admissible placement, the lease waits on the hold `no_authorised_route`. It never widens its own route: a
  wider route only comes from `capacity.include_source` or a destination change, each confirmed as 04 requires. The
  hold joins the registry (02) with its implementation, with these semantics:
  - **Signal.** The hold persists the revisions it was decided on: the exclusion set's, the source registry's, the
    installation's usage policy (its active usage profile and overrides) and the project profile's. A change to any
    of them re-runs the full placement. A source merely becoming healthy does not
    lift the hold while it stays excluded.
  - **Identity.** While still blocked after a re-run, the lease keeps the same hold, with its paging history.
  - **Owner and paging.** The operator owns it; it pages after 30 minutes and escalates like other operator-owned
    holds (02).
  - **Precedence.** The hold applies only when no route is admissible under policy. If an admissible route exists
    but is blocked by capacity (`quota_reset`, `broker_credit_exhausted`) or by `reviewer_unavailable`, that hold
    applies instead.
- **`fallback_scope`** (nöbetçi Δ§4): a fallback step is allowed only if the role's chain entry is authorised for this
  **role and mode** (e.g. "learning digest" ≠ "phase review"). The check runs in the placement path, not only in a
  record. Moving to another family re-runs `independence`.
- **A review member places only on its pinned executor** (13, intake B1). A review node's roster is in its key, so
  placement for a member must select exactly the executor the roster pins. A fallback that would change a member
  does not place under the old node: the controller records the supersession and leases for the replacement node.
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
| granted | `lease.start_refused{reason}` (the start boundary refused, no attempt) | releasing |
| active | `lease.completed` (attempt reached a terminal state) | settling |
| active | `lease.revoked{reason}` · `lease.preempted` · `lease.expired` | settling (the attempt is stopped first, see fencing) |
| settling | `lease.settled{usage_summary, liabilities}` | settled ✱ |
| releasing | `lease.released` | released ✱ |

### Evidence-target leases [G5]

For a lease that holds an `evidence_target` group, settlement carries a **restore obligation**:

| From | Event | To |
|---|---|---|
| settling | `lease.restore_started` | restoring |
| restoring | `lease.restore_verified{receipt}` | settling_verified |
| restoring | `lease.restore_failed{reason}` | restore_failed (target stays excluded; hold `evidence_restore_pending`, owner per profile, paged after `page_after`) |
| restore_failed | `lease.restore_verified{receipt}` (retry or operator act) | settling_verified |
| settling_verified | `lease.settled{…, published: res_…}` | settled ✱ |

- **Publication guard:** the run's accepted draft result is published by the **controller** in the same transaction
  as `lease.settled`, and only from `settling_verified`. Executor submissions after publication closed are refused
  by fencing. Publishing an already-accepted draft is a controller act, not a late submission.
- **Restart:** a lease found in `active`, `settling`, `restoring` or `restore_failed` stays in that state, and the
  target stays excluded. The recovery owner is `internal/evidence`, which resumes restore as an effect (03).
- **Wiring (15 G8).**
  - The `lease.granted` payload records the lease's `evidence_target` groups (`targets`). Capacity admits a grant only
    with exactly the configured targets. A rebuild reads them from the log, never from configuration.
  - `CheckGroups` refuses to run over groups that no longer match the recorded grants. It names the group that is gone
    or changed kind.
  - The lease projector holds a lease with a target to the restore table, so it settles only through a verified
    restore. Its slot returns at that settlement (`on_restore_verified`).
  - A failed restore keeps the slot reserved, so the target stays excluded across restarts and rebuilds, and a new
    lease on it is refused `capacity_reserved_full`.
  - A lease of no target never starts a restore.
- **Run id.** An evidence run is an attempt (D40). Its run id, the name a receipt gives it (06 `demand.run`), is the
  attempt's TypeID under the `run_` prefix (`evidence.RunOf`), so the pair needs no record of its own.

- **A refused grant leaves the lease `requested`.** When a group cannot hold its reservation, the grant is refused
  with `could_not_run/capacity_reserved_full` and nothing is reserved; the M2 scheduler then moves the lease to
  `waiting` on the hold `capacity_released`, whose signal is a new capacity revision of the blocking group (a
  settlement or release returning reservations to it). A group whose limit is unknown refuses with `quota_unknown`
  instead, and that lease waits on `quota_reset`.
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
| running | `attempt.result_submitted{result}` (valid token, admissible; the event carries the admitted result) | finishing | closed |
| running | `attempt.lost{reason: stream_dead|agent_disconnect|controller_restart}` | lost | closed |
| stopping · finishing · lost | `attempt.sealed{AttemptRecord}` | sealed | closed |
| sealed | `attempt.completed{outcome, reason}` | completed ✱ | closed |
| sealed (unit continues) | `attempt.superseded{by}` | superseded ✱ | closed |

Fencing rules:
- An output (result, patch, checkpoint contribution) is accepted only if **its attempt id and token exactly match** the
  unit's current attempt **and** that attempt's publication is `open` (finishing accepts only the one submitted result).
  Anything else is refused with `failed/stale_attempt` and recorded as `attempt.output_refused` (09).
- Stopping closes publication in the same transaction as the stop event, so a revoked or interrupted attempt can never
  publish, even before a replacement starts.
- Supersession and allocation of the replacement's token happen in **one transaction**: `attempt.superseded` for the
  old, `attempt.started` for the new.
- On controller restart, every `running` attempt becomes `lost` (publication closed) before anything else runs.
- An attempt whose adapter exited without an admissible result also becomes `lost` (`agent_disconnect`, or
  `stream_dead` after a heartbeat timeout; 09). `lost` is intermediate, not terminal: it closes publication and still
  seals and completes. When outputs were refused, their `attempt.output_refused` events explain it, not the reason.

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
