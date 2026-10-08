# 12 — Evidence nodes

Status: revision 3, 2026-10-03. Revision 1 came from the nöbetçi delta intake (A2). Revision 2 applied Astra's PR #5
review (`[PR5-#]`). **Revision 3 follows D40:** an evidence job is an **evidence node** in the artifact graph (13). Its
target is a leased resource (05), and its run is an attempt with fencing (05). The separate evidence state machine of
revision 2 is gone. This note keeps only what is specific to evidence. Amended by E1 (15), 2026-10-08: the params
schema and the `remote_process` request.

An evidence node produces receipts by acting on something outside the sandbox:
- an integration environment;
- a deploy and its verification;
- a browser walk;
- a release proof;
- a live probe.

## How the general machinery covers it [D40]

| Concern | Covered by |
|---|---|
| Exclusive use of a target; nothing runs on an unrestored target | `evidence_target` resource group, capacity 1, release rule `on_restore_verified` (05, 13) |
| Collected ≠ published; a crash before cleanup publishes nothing | the controller publishes the accepted draft at lease settlement, which requires a persisted `restore_verified` (05 "Evidence-target leases") |
| Late results from a superseded run | attempt fencing (05) |
| Re-running only when needed; a new demand re-keys the review | computation keys, typed conclusions and successor reviews (13); `could_not_run` retries on its signal, never cached (13) |
| Applicability, including the demand | `internal/receipt` (06 rule 2) on input resolution (13) |
| External effects and crash recovery | effects (03), including `remote_process` |

## What is specific to evidence

### Node parameters

```yaml
EvidenceNode params:
  instrument: {name, version, digest}       # its InstrumentContract (06); its preconditions and artifact classes apply
  grant: apr_…                              # authority for any live act; bound to the instrument digest
  target: tgt_…                             # leased as an evidence_target resource group
  subject_input: name                       # the declared input whose result is the receipt's subject
  demand: {key: sha256:…|null}              # what the run must prove; the key is part of the node key
  limits: {max_attempts_per_demand, max_attempts_per_subject, deadline, cooldown}
  restore: {required: bool, procedure: plugin-defined}
```

The schema is `schemas/records/evidence-node-params.json`. `records/node` selects it for kind `evidence`, so every
field is in the params digest and so in the node's key (13).
- **The demand key is keyed; who asked is not.** The key is computed from what is demanded and the subject (legacy
  golden `f16-walk-demand`), and `null` is the plan's own evidence. One evidence node serves each `(subject,
  instrument digest, demand key)`. Every owed input that asks for it links to that node, and the link and the asker
  live with the owed input (G4b).
- **The preconditions and artifact classes are the instrument contract's** (06 rule 7), bound by the instrument
  digest. The params repeat neither.
- **The grant is never null**, because every run acts outside the sandbox. It exists before the node is recorded. The
  action kind that issues it is #187.
- **`subject_input`** names a declared result input. `internal/graph` checks that the input exists, and E2 checks its
  result against the contract's `subject_type`. In M0 the subject is a result of one of 13's result kinds (13:42), so it
  is a `tree` subject. `environment` and `call` subjects need a result kind that M1 brings with deploy.
- **Limits.** The deadline is never zero. `internal/graph` refuses a per-demand cap above the per-subject cap, and an
  evidence node that does not lease an `evidence_target`.
- **Restore.** It is the instrument contract's `restore_procedure` (06): a required restore names that procedure, and
  one not required means the contract has none. Remote-session cleanup still runs, and the lease still settles only
  after `lease.restore_verified` (05).

### Attempts keyed by demand (nöbetçi Δ§2)

- Attempts are counted per `(subject, instrument digest, demand key)`, with a total cap per subject across demands.
- Exhausting the cap yields `could_not_run/evidence_cap_exhausted` with a `wrong_owner` hold for the project owner.
- A precondition failure is not a spent attempt.

### Preconditions (nöbetçi Δ§5)

- Declared by the instrument and checked before each run: name resolution, reachability, identity provider up, free
  disk and memory, reboot-persistent configuration.
- A failure yields `could_not_run/precondition_failed` and the `precondition_failed` hold (02). It recovers when the
  probe passes, on a bounded probe schedule, and pages the environment owner after `page_after`.
- A transient external lock is retried inside the run's single deadline.

### Content (nöbetçi Δ§8) [PR5-6]

- Predicate fields are typed facts under the instrument's payload schema.
- Artifacts are classed `diagnostic` (may reach builders and reviewers) or `restricted` (live-system output,
  production data, remote stderr; never in prompts or the journal).
- **Isolation is proven** by a probe inside the same wrapper. An empty isolation setting is never published.
- **No production writes** unless the grant says so. Copies are read-only, size-capped and capacity-checked.
- Consistency checks are part of the verdict.

### Remote processes (nöbetçi Δ§8)

- Remote steps run under recorded identities.
- The `remote_process` effect (03) reconciles by "no process of the recorded sessions remains" and kills leftovers.
- Cleanup is part of restore. A leftover keeps the target's lease unsettled.
- Every remote call gets only the time left in the run's single deadline.
- The effect's request is `schemas/records/remote-process-request.json`. Reconciliation reads it, so it is kept: the
  caller publishes it as a blob before the intent, and the intent's `request_digest` references that blob (the effect
  kind is declared with `RequestBlob`, 03). It names:
  - the evidence target, which is also the intent's target;
  - the attempt, and the same attempt's run id. A receipt names the evidence attempt as `demand.run` (06, D40), so the
    request records both;
  - the identity, an alias of a recorded identity in the project profile, never the remote principal. Recovery
    resolves the alias through the profile when it reconciles, and an alias that no longer resolves leaves the effect
    uncertain. The profile has no identities field yet. M1 adds it (#191);
  - the session tags assigned before egress;
  - the end of the deadline.
- `remote_process` is not idempotent at its target, so it is never retried.
- The store validates the kept request (`evidence.RemoteProcessKind`, #190):
  - against `records/remote-process-request`;
  - that its target is the intent's;
  - that its run is its attempt's run (05).

  The restore procedure is the instrument contract's (06); `evidence.CheckRestore` holds the node's restore to it.

## What M0 delivers

The evidence node parameter schema, the attempt-key counting with the total cap, the precondition → hold mapping,
artifact classes, the `remote_process` effect kind, and fixtures:
- a new demand gets new attempts; the same demand does not;
- total-cap exhaustion;
- a precondition failure spends no attempt and recovers on its probe;
- an empty isolation setting is never published;
- a restricted artifact never reaches a prompt.

The "cannot occur" fixtures for exclusion, publication and fencing live with note 13.
