# 03 — Event store

M0 review findings are cited as `[M#]` (`reviews/2026-10-02-astra-m0-review.md`).

## Shape

One SQLite database per controller (`<state_dir>/adc.db`), WAL mode, `synchronous=FULL`, opened by exactly one `adcd`
process. Pure-Go driver (no CGO), so release binaries stay static.

```
events (append-only)
  seq          INTEGER PRIMARY KEY      -- gapless, assigned inside the writer transaction
  id           TEXT UNIQUE NOT NULL     -- evt_…
  at           TEXT NOT NULL            -- event time (below), RFC 3339 UTC; never decreases with seq
  kind         TEXT NOT NULL            -- registered event kind + version, e.g. attempt.started/v1
  subject      TEXT NOT NULL            -- primary entity id
  project      TEXT                     -- prj_… or NULL for controller-wide events
  actor        TEXT NOT NULL            -- act_… (system actors included)
  command      TEXT                     -- id of the command that produced it
  causation    TEXT                     -- evt_… that caused this one
  schema_set   TEXT NOT NULL            -- digest of the writer's schema set
  payload      BLOB NOT NULL            -- canonical JSON (RFC 8785 JCS), validated against its schema
  prev_hash    TEXT NOT NULL            -- hash of the previous row; genesis = sha256 of the store's creation record
  hash         TEXT NOT NULL            -- sha256 over the canonical encoding of every column above

commands (one row per committed command; idempotency + results) [M1]
  id, actor, idempotency_key, input_digest, disposition, result_json, first_seq, last_seq
  -- disposition: applied | proposed | invalidated | refused (14 N2)
  UNIQUE(actor, idempotency_key)

effects (external side effects; durable identities) [M2]
  id (eff_…), kind, command, target, request_digest, state, attempts, last_observation, result_ref
```

Projections (`proj_*`) are derived, rebuildable caches, updated **in the same transaction** as the events that change
them. `adc dev rebuild-projections` replays the log into fresh tables and compares them with the live ones.

Large payloads live in a **content-addressed blob store** (`<state_dir>/blobs/sha256/ab/cd…`). A blob is written to a
temp file, fsynced, digest-verified and renamed into place **before** any event references it. Blobs are immutable.
Retention purges contents and leaves a tombstone event.

## The writer: the serialization boundary [M1]

All state-dependent decisions happen **inside one writer transaction**:
1. idempotency lookup;
2. authorization (`internal/authority`, against the current projections);
3. expected-state check;
4. event append;
5. projection update, which includes resource reservations;
6. command-result row.

Validation that needs no state (schema, syntax) may run before, on the caller's goroutine.

**Reservations are a projection** (05). The capacity projector runs after the lease projector in the same
transaction. On `lease.granted` it checks every reservation the lease names against its group, counting outstanding
reservations, and refuses the whole command if any lacks room (`could_not_run/capacity_reserved_full`). On `lease.settled` or `lease.released` it returns
them. Reserving from the proposed events, rather than from the command, makes reserved totals a fold over the log:
a rebuild recomputes them, and the limit check runs only when an event is admitted, never on replay.

- **Idempotency.** `(actor, idempotency_key)` is unique. `input_digest` covers the canonical action, the complete input
  and the expectations (14 N1). A repeat with the **same** digest returns the stored result and disposition. A repeat
  with a **different** action, input or expectation is refused with `failed/idempotency_key_reused`.
- **Dispositions.** The command's logic decides applied, proposed, invalidated or refused, and each is committed with
  its events (14 N2). Internal callers go on only through `SubmitApplied`, which turns any other disposition into an
  error. What the writer cannot decide (a reused key, an authorization refusal, a stale expectation) commits nothing.
- **Issued secrets.** A credential's plaintext never enters the store (14 N3). The request layer mints it before
  `Submit`, outside the writer transaction (`identity.Mint`). The command's logic records only the public id and the
  hash, and the stored result is stable and free of secrets. `Committed` reports its disposition and whether it was
  `Replayed`. The plaintext goes to the live response only from a fresh applied commit (`identity.Issued.Deliver`).
  Otherwise it is dropped, so a replay neither delivers nor records a new secret.
- **Expected state.** Commands acting on an object carry `expected` = the object's version (seq of its last event), its
  content digest, or both (14 P3). The object is the one the action's handler derives, once per command: the Authorize
  hook binds that derivation to the transaction (`Tx.BindObject`), and the writer checks the same object. The handler
  defines the object's record, which is required for any expectation. The record decides existence, and its canonical
  bytes give the digest (`eventstore.ContentDigest`, sha256). A missing record matches only version 0 without a digest;
  a present one never matches version 0; otherwise the version, and the digest when supplied, must match. An object
  whose record other subjects' events also change defines its own version, the seq of the last event that changed it.
  The seat does this, because revoking its holder's actor frees it. The writer
  enforces this after authorization and before the command's logic runs. A mismatch is refused with
  `failed/stale_expected_state` and commits nothing.
- **Event time.** An event is recorded at max(the store clock, the previous event's time), so recorded times never
  decrease with seq, even across a backward clock step. This applies to command and effect events alike, and the
  writer enforces it at append. A command reads the clock once, when its transaction begins (`Tx.Now`). Every time
  decision inside the command reads that one value: authorization, grant validity, proposal creation and the logic's
  own times (a seat request's `force_after`). Every event the command appends carries that value as its time. An event
  that names a time before the previous event's is refused. The clock is the store's (`Config.Now`, `time.Now` by
  default); the authorizer has none of its own. Folds read only recorded times, never the clock.
  Verification checks the rule: an unreadable or decreasing event time is a broken chain (`log_integrity_broken`).
  The bootstrap issues and consumes its secret by the command's time too.
- **Process exclusivity.** `adcd` takes an exclusive advisory lock (`flock`) on `<state_dir>/adcd.lock`. It records its
  pid and process start time, and refuses to open the database without the lock. A second daemon exits with
  `could_not_run/controller_already_running`. The lock is acquired before the database is opened and never created or
  truncated by probes (nöbetçi lesson: a probe truncated `cycle.lock`).
- **Tests:** concurrent conflicting approvals (exactly one applies); concurrent reservations of the last unit of a
  resource group (exactly one wins); a second daemon refused.

## External effects: intent, observation, reconciliation [M2]

Every external effect (model call, forge push/merge, deploy, notification) has a durable identity **before** it
happens:

```
planned ──[effect.intent committed]──▶ pending ──[effect.observed{result}]──▶ completed
                                          │
                                          ├──[crash / timeout / ambiguous reply]──▶ uncertain ──[reconcile]──▶ completed | failed_confirmed | still_uncertain
                                          └──[effect.failed{before_send proven}]──▶ failed_confirmed
```

- The intent is committed before the effect starts. If it cannot be committed, the effect does not happen.
- On restart, every `pending` effect becomes `uncertain`. Each effect kind declares a **reconciliation** procedure:
  - model call: query the provider's usage or request id, otherwise keep the usage as an uncertain liability (05);
  - forge push: compare the remote ref to the expected new head (done-probe);
  - merge: query the change-request state;
  - deploy: query the target's running version;
  - notification: query the outbox delivery id;
  - remote process (12): "no process of the recorded remote sessions remains", killing leftovers. Its intent's
    target is the evidence target, and its request (`records/remote-process-request.json`) records the identity alias
    and the session tags;
  - evidence run (12): its restore procedure, then the receipt.
- **Kept requests.** A kind that reconciles from its request declares `RequestBlob`. The caller publishes the
  request's RFC 8785 form as a blob before the intent, and the intent's `request_digest` references it; the store
  refuses the intent while the blob is absent. The blob is retained while its effect is pending or uncertain and
  until the target's lease settles; retention never purges it before then. A kind may declare a `RequestSchema`,
  which the request must satisfy, and a `Check` for what the schema cannot say (remote_process: the request's target
  is the intent's).
- **Retry versus reconcile.** An effect kind declares whether it is idempotent at the target (safe to retry with the
  same effect id) or must be reconciled first. Non-idempotent effects are never blindly retried. A command result
  returned on an idempotent retry includes its effects' current states, so unfinished work is visible, not lost.
- **Crash fixtures (M0 defines, M1 implements the runtime):** a crash after intent commit, a crash after send before
  observation, a crash after observation before projection. Each has an expected disposition.

## Rules

1. **Never update, never delete.** Corrections are new events. Counters, charges and attempts are never reset.
2. **Ambiguity fails closed.** A projection that finds two live records where one is allowed raises
   `could_not_run/ambiguous_state` rather than picking one. `adcd` refuses to start with the same reason on a state it
   cannot read as one history: a lease granted before capacity was wired, actors without exactly one system actor, or
   events without any actor.
3. **Hash chain [Q4].**
   - Canonical encoding (JCS) and a genesis record.
   - `adcd` verifies the **full chain** before building projections at startup (verification is linear; the cost is
     accepted).
   - `adc dev verify-log` does the same on demand.
   - Backups carry a trusted head (seq + hash) outside the database.

   The chain detects edits and truncation relative to a trusted head. It cannot detect a complete rewrite or a rollback
   by someone with write access to both; the docs say so.
4. **Schema evolution.** Payload schemas are versioned by kind. Readers handle every version ever written.
5. **Backups.** `adc admin backup`: SQLite online backup + blob manifest + trusted head. A restore verifies the chain
   against the head.

## What M0 delivers

The store, the single writer with in-transaction authorization and projection hooks (reservations are a projection,
above), the command and effect tables
with the effect state machine (runtime reconciliation is M1), the blob store with publish-before-reference, the
process lock, the hash chain with full verification, projection rebuild, and the tests above.
