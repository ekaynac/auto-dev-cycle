# 12 — Evidence jobs

Status: added 2026-10-03 from the nöbetçi delta intake (`docs/research/2026-10-03-nobetci-delta-intake.md` A2; nöbetçi
Δ§2, Δ§5, Δ§6, Δ§8, G2, G10, G12). Revised after Astra's PR #5 review (`docs/reviews/2026-10-03-pr5-astra.md`,
cited `[PR5-#]`).

An **evidence job** is a controller-run producer of receipts that acts on something outside the sandbox:
- an integration environment run;
- a deploy and its verification;
- a browser walk;
- a release proof;
- a live probe.

Units (05) cover model work; this note covers evidence producers. `internal/receipt` owns receipts and is the only
applicability checker (06); `internal/evidence` owns jobs and runs.

## Job and run

```yaml
EvidenceJob:
  id: ejb_…
  instrument: {name, version, digest}       # its InstrumentContract (06)
  grant: apr_…                              # authority for any live act; bound to the instrument digest
  target: tgt_…                             # the external thing acted on (an environment, a host, a deployment slot)
  subject: {type, …}                        # typed, as in receipts (06)
  demand:                                   # what this run must prove
    key: sha256:…                           # controller-computed, e.g. the digest of a reviewer's walk plan; null = subject alone
    requested_by: own_…|null                # the owed input (04) that demanded it
  preconditions: [predicate]
  limits:
    max_attempts_per_demand: int
    max_attempts_per_subject: int           # total cap across demands
    deadline: duration                      # ONE overall deadline per run
    cooldown: duration|null
  restore: {required: bool, procedure: plugin-defined}

EvidenceRun:
  id: run_…
  job: ejb_…
  fencing_token: int                        # per target; late results from a superseded run are refused [PR5-3]
  state: (below)
```

## States: collected is not published [PR5-3]

| From | Event | To | Receipt status |
|---|---|---|---|
| — | `run.launched` | running | — |
| running | `run.collected{draft_receipt}` | collected | **draft: never applicable** |
| running | `run.could_not_run{reason}` / `run.interrupted` / deadline | needs_restore | — |
| collected | `run.restore_verified` | published ✱ | **published: may be applicable** |
| collected · needs_restore | `run.restore_failed` | restore_failed | draft stays unpublished |
| needs_restore | `run.restore_verified` | closed ✱ | — (the outcome is recorded as `could_not_run`) |

- A receipt becomes applicable **only after verified cleanup/restore**. A crash between collection and cleanup leaves a
  draft that never counts; on restart the run is `needs_restore`.
- **Target exclusion.** While any run on a target is `running`, `collected`, `needs_restore` or `restore_failed`, the
  target is excluded:
  - no new run on that target starts, for **any** job, demand or instrument version;
  - no receipt from that target is published.

  The exclusion is persisted in the event store, not held in memory. Hold: `evidence_restore_pending`.
- Results from a run whose fencing token is no longer current are refused (`failed/stale_run`) and recorded.

## Attempts, keyed by demand (Δ§2)

- Attempts are counted per `(subject, instrument digest, demand key)`. A new demand gets its own attempts. The same
  demand does not.
- The total cap per subject bounds all demands together. When it is exhausted: `could_not_run/evidence_cap_exhausted`,
  with a `wrong_owner` hold for the project owner.
- Old attempt records are never rewritten.
- A run that ends `could_not_run` because a **precondition** failed is not a spent attempt.

## Preconditions (Δ§5, G12) [PR5-8]

- The instrument contract declares environment preconditions: name resolution, reachability, identity provider up,
  free disk and memory, reboot-persistent configuration.
- They are checked immediately before each run. A failure yields `could_not_run/precondition_failed` and the
  **`precondition_failed` hold** (02):
  - signal: the precondition probe passes;
  - the probe is retried on a bounded schedule, so recovery needs no manual release;
  - the page goes to the environment owner from the profile after `page_after`.
- Preconditions do not use the `wrong_owner` hold. That hold is reserved for faults with no probe-able signal.
- A transient, externally held lock is retried inside the run's single deadline, never as a new attempt (Δ§6).

## Receipt content (Δ§8, G10) [PR5-6]

- Predicate fields are typed facts validated by the instrument's payload schema (06 rule 7).
- Artifacts are classed by the instrument: `diagnostic` may reach builders and reviewers; `restricted` (live-system
  output, production data, remote stderr) never enters prompts or the journal.
- **Isolation is proven, not assumed.** When a step must run isolated, a probe inside the same wrapper must see only
  what is allowed and fail to reach an address the host can reach. An empty isolation setting is never `published`.
- **No writes on production targets** unless the grant says so explicitly. Copies are taken by read-only mechanisms,
  size-capped, with capacity checked first.
- Consistency checks are part of the verdict (e.g. a schema digest changes exactly when a migration reports a schema
  change; no table loses rows; a rollback names a different version).

## Remote processes (Δ§8)

- Remote steps run under recorded identities (session or process group, recorded outside the work directory).
- An effect kind `remote_process` (03) reconciles by "no process of the recorded sessions remains" and kills leftovers.
- Cleanup is part of `restore_verified`. A leftover keeps the target excluded.
- Every remote call gets only the time left in the run's single deadline.

## Applicability and re-review

- A published receipt carries the run's demand key and run id (06). A requirement that names a demand accepts only a
  receipt for that demand. The check is made by `internal/receipt`, never here [PR5-2].
- **Re-review** is decided by evidence equivalence (06 rule 2a), not by fingerprint identity:
  - a published receipt for a demand the reviewer made, whose `conclusions_digest` differs from what the reviewer saw,
    triggers a new review;
  - an identical `conclusions_digest` does not;
  - fingerprints (10) stay exact for manifests [PR5-5].

## What M0 delivers

The EvidenceJob and EvidenceRun schemas, the state machine with collected ≠ published, persistent target exclusion, run
fencing, demand-keyed attempts with the total cap, the precondition hold mapping, the `remote_process` effect kind,
artifact classes, and these contract fixtures:
- a crash between collection and cleanup leaves no applicable receipt;
- a new job on an excluded target is refused;
- a late result from a superseded run is refused;
- a new demand gets new attempts; the same demand does not;
- total-cap exhaustion across demands;
- a precondition failure spends no attempt and recovers on its probe;
- a receipt for another demand with identical conclusions does not apply;
- an empty isolation setting is never published.
