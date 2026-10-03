# 12 — Evidence nodes

Status: revision 3, 2026-10-03. Revision 1 came from the nöbetçi delta intake (A2). Revision 2 applied Astra's PR #5
review (`[PR5-#]`). **Revision 3 follows D40:** an evidence job is an **evidence node** in the artifact graph (13). Its
target is a leased resource (05), and its run is an attempt with fencing (05). The separate evidence state machine of
revision 2 is gone. This note keeps only what is specific to evidence.

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
  instrument: {name, version, digest}       # its InstrumentContract (06)
  grant: apr_…                              # authority for any live act; bound to the instrument digest
  target: tgt_…                             # leased as an evidence_target resource group
  demand: {key: sha256:…|null, requested_by: own_…|null}   # what the run must prove; part of the node key
  preconditions: [predicate]
  limits: {max_attempts_per_demand, max_attempts_per_subject, deadline, cooldown}
  restore: {required: bool, procedure: plugin-defined}
```

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

## What M0 delivers

The evidence node parameter schema, the attempt-key counting with the total cap, the precondition → hold mapping,
artifact classes, the `remote_process` effect kind, and fixtures:
- a new demand gets new attempts; the same demand does not;
- total-cap exhaustion;
- a precondition failure spends no attempt and recovers on its probe;
- an empty isolation setting is never published;
- a restricted artifact never reaches a prompt.

The "cannot occur" fixtures for exclusion, publication and fencing live with note 13.
