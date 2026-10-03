# 12 — Evidence jobs

Status: added 2026-10-03 from the nöbetçi delta intake (`docs/research/2026-10-03-nobetci-delta-intake.md` A2; nöbetçi
Δ§2, Δ§5, Δ§6, Δ§8, G2, G10, G12). An **evidence job** is a controller-run producer of receipts that acts on something
outside the sandbox:
- an integration environment run;
- a deploy and its verification;
- a browser walk;
- a release proof (install from artifacts, migrate a copy, roll back);
- a live probe.

Units (05) cover model work; this note covers evidence producers. `internal/receipt` owns the receipts (06);
`internal/evidence` owns the jobs.

## Job definition

```yaml
EvidenceJob:
  id: ejb_…
  instrument: {name, version, digest}       # its InstrumentContract (06)
  grant: apr_…                              # authority for any live act; bound to the instrument digest
  subject: {type, …}                        # typed, as in receipts (06)
  demand:                                   # what this run must prove
    key: sha256:…                           # e.g. the digest of a reviewer's walk plan; null = the subject alone
    requested_by: rcp_…|evt_…|null          # the review/claim that demanded it (owed input, 04)
  preconditions: [predicate]                # checked before every run; see below
  limits:
    max_attempts_per_demand: int
    max_attempts_per_subject: int           # total cap, so a changing demand cannot loop forever
    deadline: duration                      # ONE overall deadline for the whole run
    cooldown: duration|null
  restore: {required: bool, procedure: plugin-defined}
```

## States

```
absent ──[launched]──▶ running ──[observed{receipt}]──▶ observed ──▶ (restore) ──▶ restored ✱
   ▲                     │
   │                     ├──[could_not_run{reason}]──▶ could_not_run ──▶ (restore) ──▶ restored ✱
   │                     ├──[interrupted]────────────▶ interrupted ──▶ (restore) ──▶ restored ✱ | restore_failed
   │                     └──[deadline]───────────────▶ interrupted
   └── a new run is admitted only if the previous run of this job reached `restored`
```

- **An unrestored run blocks.** If a previous run is `interrupted` or `restore_failed`, no new run starts and no
  receipt from this job counts as current until `restore` succeeds (nöbetçi OD#2; lab-deploy design F4–F8). The hold is
  `evidence_restore_pending`, owned by the system, then by the operator after `page_after`.
- Every transition is an event. A run is an **effect** (03) with its own reconciliation.

## Attempts, keyed by demand (Δ§2)

- Attempts are counted per `(subject, instrument digest, demand key)`. A new demand (e.g. the reviewer's new walk plan)
  gets its own attempts. Repeating the same demand does not.
- A total cap per subject bounds all demands together.
- Old attempt records are never rewritten.
- A run that ends `could_not_run` because a **precondition** failed is **not a spent attempt** (below).

## Preconditions (Δ§5, G12)

- The instrument contract declares environment preconditions:
  - name resolution;
  - reachability of hosts and services;
  - the identity provider up;
  - free disk and memory for the run;
  - configuration that must persist across reboots.
- They are checked immediately before each run.
- A failing precondition yields `could_not_run/precondition_failed` with a `wrong_owner` hold naming the environment
  owner (from the project profile). It does not consume an attempt.
- **A transient, externally held lock** (e.g. a package manager lock) is retried **inside the run's single deadline**,
  never as a new attempt (Δ§6).

## Receipt content policy (Δ§8, G10)

- **Predicate evidence is typed facts:** exit codes, digests and byte counts of outputs, versions matching strict
  patterns, integer schema numbers, row counts, artifact digests. Free text (script output, stderr, database content)
  enters a receipt **only as a digest**. Raw text, if kept, goes to the blob store with a restricted scope, never into
  prompts or the journal.
- **Isolation is proven, not assumed.** When a step must run offline or isolated, a probe runs inside the same
  wrapper. It must see only what is allowed and fail to reach an address the host can reach. An empty isolation
  setting is never `observed`.
- **No writes on production targets** unless the grant says so explicitly. Copies are taken by read-only mechanisms
  and size-capped. Capacity is checked first.
- **Consistency checks are part of the verdict** (e.g. a schema digest changes exactly when a migration reports a
  schema change; no table loses rows; a rollback names a different version).

## Remote processes (Δ§8)

- Remote steps run under recorded identities (their own session or process group, ids recorded outside the work
  directory).
- An effect kind `remote_process` (03) reconciles by "no process of the recorded sessions remains". It kills leftovers
  when a step returns.
- **Cleanup is proven:** a leftover copy, process or artifact blocks new runs and blocks `observed` until `restore`
  removes it.
- Every remote call gets only the time left in the run's single deadline. A stuck pipe cannot block past it.

## Applicability and re-keying

- An evidence job's receipt applies only to its typed subject and demand (06 rule 2).
- A new demand that produces an observation **re-keys** the review that asked for it: the demand key is part of the
  review's input fingerprint (10). A rerun of the same demand with the same conclusions does not re-key (Δ§3).

## What M0 delivers

The EvidenceJob schema, the state machine with its "unrestored blocks" rule, attempt counting by demand with the total
cap, the precondition → `wrong_owner` mapping, the `remote_process` effect kind, and contract fixtures:
- a demand change gets new attempts;
- the same demand does not;
- a precondition failure spends no attempt;
- an unrestored run blocks;
- free text in a predicate is refused;
- an empty isolation setting is never observed.

Real runs start with the integration plugin (M1 multi-service fixture) and the deploy plugins.
