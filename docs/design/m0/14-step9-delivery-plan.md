# M0 step 9: delivery plan

Status: revision 4, 2026-10-06. Reviewed by Astra in three rounds (NO-GO, NO-GO, GO with changes; records in `reviews/2026-10-06-step9-plan-astra.md`); this revision applies the round-3 changes P1–P6.

Scope: **step-9 completion** of 04 §9 — the daemon, local API, CLI transport, parity harness and installed smoke. It is not complete M0. These M0 obligations are tracked separately, each with its own completion evidence and a tracking issue linked from issue #3:
- artifact graph (13);
- evidence nodes (12);
- knowledge-spec and legacy fixtures (10, 11);
- the `adc doctor` credential-isolation warning (04-authority:225).

## 1. Starting point (origin/main 18ecdd2)

`cmd/adcd` answers only `version`.

**Already present:**
- `eventstore`: Submit, Hooks, ProjectEach, the lock, verify, MarkPendingUncertain.
- `identity`: tokens, Authenticate, the bootstrap projection.
- `authority`: Decide, grants, ReadScopeOf, proposals, the WebAuthn verifier.
- `capacity`: Hook, CheckHistory, NewReservations.
- `api`: Events, ConnContext, redaction.
- `cli`: one command per action, with only `NoTransport`.
- `blobstore`, `adapterhost`.

**Missing:**
- a production Authorize hook and a command transaction boundary;
- action handlers;
- OpenAPI for events and reads;
- event kinds for proposals, seat, notes and token administration;
- sockets, the bootstrap flow and a client transport;
- the parity harness and the installed smoke.

## 2. Operator decisions (approved 2026-10-06)

- **R1. Private socket.** The private socket is 0600. The daemon refuses to start unless the state dir is 0700 and owned by the daemon uid.
- **R2. Optional listeners.** The shared socket and the HTTPS listener are off by default. An invalid explicit configuration fails startup.
- **R3. TLS.** Verified TLS only, from a configured CA or a pinned fingerprint. There is no insecure mode, no plaintext fallback and no credential-forwarding redirect.

  **Residual risk (PR 3b).** Remote callers reach the bearer check before they are authenticated, and each wrong bearer costs one Argon2id verification. The listener bounds this cost in four ways:
  - It has its own verification slots, capped in total and per remote address. Every limit counts one key: an IPv4 address (the same when IPv4-mapped), or an IPv6 /56 (6to4 included), a link-local one per interface. A connection without a remote address is refused.
  - It holds off an address after repeated failures, with a doubling wait of up to 30 s. A held-off address's new connections are closed at accept, before the TLS handshake. A 200 clears the address's failures once its last failure is 30 s old, so a caller with a live bearer cannot interleave successes with guesses.
  - It closes the connection after every refusal.
  - It never touches the sockets' slots.

  A connection that has not verified a bearer within 5 s of its accept is closed, whatever stage it is at. A connection that never presented a bearer counts as a failure for its address when it reaches that bound, or when it ends any other way after holding its slot for 1 s, so closing just before the bound does not avoid the failure. Even so, an attacker can keep the HTTPS slots busy and deny HTTPS service to others; the sockets are unaffected. It takes 32 addresses by default (128 connections, 4 per address). Each of them may hold its slots without a bearer three times; from then on each such connection doubles its hold-off, up to 30 s, so each address holds its slots for about 5 s in every 35 s. Only connections shorter than 1 s escape the count: holding slots with them takes several connections a second per address, which an L3/L4 connection-rate limiter should catch. Clients behind one NAT, one /56 or one link (over link-local addresses) share their limits and their hold-off. Failures are tracked for at most 4096 addresses at a time. Past that, the slots alone cap an untracked address, and a full table is swept for quiet addresses at most once a second. Deployments that expose the listener beyond a trusted network should put an L3/L4 firewall or connection-rate limiter in front of it. The listener trusts neither the PROXY protocol nor forwarded headers, so behind a reverse proxy every client shares the proxy's one address key (its slots, its verification and its hold-off); a reverse proxy in front must enforce per-client limits itself.
- **R4. Bootstrap.**
  - Reachable only over the private socket, decided by listener context.
  - The 24 h expiry is fixed when the secret is created.
  - The secret file is created securely and exclusively.
  - One transaction covers the secret check, the unconsumed check, the first operator, the first token and consumption.
  - Racing clients produce exactly one success.
  - The commit happens before the file is deleted.
  - Issued secrets are never persisted.
- **R5. Token file.**
  - Owner, regular-file type and 0600 are checked on the opened descriptor.
  - The parent directory is private.
  - The client never follows an attacker-chosen path.
  - The token is never logged.
- **R6. WebAuthn.** Deployment is deferred. The verifier and its fixtures stay, and the human confirmation endpoint is refused until deployment.
- **M3. Peer credentials and limits.**
  - The private socket and bootstrap accept only the daemon uid, and the client verifies the daemon uid before sending credentials.
  - Bearer authentication is required on every socket.
  - The shared socket is 0660, in an explicit run dir that the daemon controls, with an explicit group.
  - Stale sockets are cleaned up safely.
  - Bodies and headers are bounded, and concurrent Argon2 verifications are capped.
- **M4. Token administration.** Token issue, revoke and replace are registered actions. Rotation keeps the actor and its grants. The old token is rejected and its streams close.
- **R7. Offline operator-credential recovery.** `adcd recover operator-token --actor <id>`:
  - runs only as the daemon uid;
  - holds `adcd.lock` from before the store opens until after it closes;
  - works only against a store that passes every PR-1 trust check (chain, trusted head for restored state, projection comparison).

  It records a registered, versioned `identity.token_recovered/v1` event with a deterministic projector transition: the system is the issuer and an existing live operator is the recipient. Optional revocation of that operator's other tokens happens in the same event batch. The secret goes only to a 0600 file chosen by the caller.

  It refuses when:
  - the daemon is running;
  - the store fails any trust check;
  - the target is missing, revoked or not an operator.

  It never confirms proposals, resurrects actors or resets bootstrap. The ordinary issuance restriction (`identity.go:309`) and the bootstrap-consumed projection are unchanged.

**Approved by the operator on 2026-10-06, after the round-3 review:**
- **R8. Renewal of an expired, unconsumed bootstrap (P2).** `adcd bootstrap renew`:
  - has the same offline conditions as R7 (daemon uid, lock held, trusted store);
  - succeeds only while bootstrap is unconsumed;
  - writes a new secret with a new 24 h expiry and records a `identity.bootstrap_renewed/v1` event that invalidates the old secret.

  It is impossible after consumption, and an ordinary restart never renews the expiry. Step-9 completion requires an approved recovery path for both the lost-credential case (R7) and the expired-bootstrap case (R8).

## 3. Lead decisions

- **D1. Unwired actions.** Registered actions not wired in step 9 refuse with `could_not_run` and make no mutation; each is deferred to its owning milestone. An explicit supported/deferred matrix includes `review.request`. Effective-change and protected-change contract fixtures are preserved.
- **D2. Installed smoke.** It is additional coverage; every exit row of 00-overview:46-58 stays.
- **Resource groups.** Static, schema-validated startup configuration. Unsupported semantics fail closed.
- **Read scope.**
  - An explicit map from each resource to its domain and project.
  - Every record is authorized against one snapshot.
  - `whoami` is defined separately.
  - Blobs are reachable only through readable records that reference them.
- **H1. Enrolment.** An operator's enrolment is applied atomically (actor and token together). A supervisor's enrolment becomes a human-policy proposal. Remote agents are M3.
- **N1. Idempotency.**
  - Uniqueness stays `(actor, idempotency_key)`.
  - The stored input digest covers the canonical action, the complete request and the expectations.
  - The same key with a different action, body or expectation is refused with `idempotency_key_reused`; identical input replays.
  - Red-proofs: different input, cross-action, changed expectation.
- **N2. Dispositions.**
  - The internal Submit outcome is applied, proposed, invalidated or refused, and every one is committed.
  - All existing callers move in the same PR. Internal callers (`adapterhost/attempt.go`, fixtures) go through an applied-only wrapper, so only an applied outcome may advance execution.
  - Regression test: a refusal committed before start launches no adapter.
  - HTTP mapping: invalidated becomes refused with its registered reason, and the invalidation event is kept.
- **N3. Credential delivery.**
  - The plaintext never enters the event store, whose persisted result is stable and free of secrets.
  - The request layer mints the token before `Submit`, outside the transaction (`identity.Mint`). The handler records only its public id and hash (`Issued.Events`). The mint is not part of the command input, so a retry under the same key replays.
  - After `Submit`, the caller gives the plaintext to the live response only when the disposition is applied and the result was not replayed (`Issued.Deliver`). Otherwise the plaintext is dropped.
  - The live response carries a typed `issued_credential` field only on first delivery; it is absent on replay. The schema, docs and CLI change together.
  - Replay never records or delivers a token.
  - Tests cover a lost response and a failed token-file write for bootstrap, enrolment and replacement.
- **Replay authentication.** A cached result is returned only after HTTP authentication and a writer-boundary identity recheck, which runs before the replay branch (`writer.go:265`).

  Acceptance cases:
  - missing or invalid credentials;
  - a revoked token;
  - a revoked actor;
  - another actor's key;
  - revocation between HTTP authentication and writer access.
- **P3. Expectations.** The command framework checks version and content-digest expectations inside the transaction, before any handler is activated. Each handler supplies its authoritative object and digest definition. Acceptance covers a stale digest and a concurrent change.
- **N4 and P4. Seat ordering.** The seat projection and guard land before any supervisor-write handler. Seat handlers activate after proposal and approval persistence, because seat actions are authority-gated and grants can tighten them (`decide.go:173`). Taking or requesting the seat does not require holding it, but still needs authorization.
- **N4. System actor.** The startup operation that creates the system actor is private, but it uses validated, transactional event append and projection. No public caller can select the system identity.
- **N6 and P6. Grant replacement.** `grant.extend` is an atomic replacement. One transaction holds:
  - `grant.revoked` under the old id;
  - `grant.created` under a fresh id;
  - a `grant.replaced/v1` lineage event that binds the old id, the complete new grant and the approval.

  Expected-state versions advance for both ids. The replacement is bound to the old grant's expected state. Classification stays `grant.extend`, and self-grants are prohibited. Proposals that reference the old grant are invalidated in the same transaction. Streams holding the old grant close, and a test covers reconnecting under the replacement.

## 4. PR sequence

Each PR is at most about 400 hand-written non-test lines, counting added plus deleted lines and including schemas. The budget is in brackets. Contracts are not compressed to meet a budget: an overrun is split, or justified under the cohesive-package rule (04:32).

1. **Daemon startup** [~400] (contract):
   - config and state-dir validation;
   - the lock before the store;
   - chain verification, plus a trusted head when supplied;
   - CheckHistory on existing projections before any replay;
   - the system actor (N4);
   - marking pending effects uncertain;
   - a readiness point;
   - an Authorize hook that refuses everything.

   This is PR #111, at 493 lines under the D41 cohesive-package justification.

   **1b. Startup projection and version checks** [~150] (contract): replay-and-compare of every projection, refusing on drift, and refusal of unsupported persisted event versions. Drafted alongside #111.
2. **Command boundary** (contract):
   - **2a. Dispositions and idempotency** [~300]: N1, N2 and the store side of N3.
   - **2b. Principal framework** [~300]: the transport-authenticated principal, replay authentication, and the derivation framework (object, project, grantee, class).
   - **2c. Expectations** [~200]: P3.
3. **Listeners** (contract):
   - **3. Server and Unix sockets** [~350]: R1, R2 and M3; readiness; ConnContext; `api.Events`; the version and schema-set endpoint; graceful shutdown; stream tests against the assembled server.
   - **3b. HTTPS listener** [~200]: R3.
4. **Client transport and credentials** (contract):
   - **4a. Client transport foundation** [~300]: the Unix client with the daemon-uid check, the R5 token file and the codec.
   - **4b. Bootstrap** [~350]: R4 and `adc admin bootstrap`.
   - **4c. Recovery identity contract** [~200]: the R7 and R8 events and projector transitions.
   - **4d. Recovery commands** [~200]: `adcd recover operator-token` and `adcd bootstrap renew`.
5. **Seat projection and guard** [~200] (contract): seat events, the projection and the transactional guard. No handlers yet.
6. **Proposals** (contract):
   - **6a. Proposal and approval persistence** [~350].
   - **6b. Seat handlers** [~250]: take, release and handover, with per-action schemas.
   - **6c. Confirm and apply** [~350]: `proposal.confirm` (independent review), exact-operation revalidation, atomic application and invalidation. Human confirmation is refused (R6).
   - **6d. Withdraw** [~150].
7. **Identity and grant handlers** (contract):
   - **7a. Enrolment and actor administration** [~300]: H1 and `actor.revoke`.
   - **7b. Token administration** [~300]: M4.
   - **7c. Grant create and revoke** [~250].
   - **7d. Grant replacement** [~300]: N6 and P6.
8. **Notes** [~150] (contract).
9. **Reads** (contract):
   - **9a. Read-authorization foundation** [~250]: the resource map, snapshot authorization and `whoami`; apidoc emits `/v1/events`.
   - **9b–9e.** One PR per resource family (grants, proposals, seat and notes, projects and holds), each with its schema, OpenAPI and redaction allow-list.
10. **CLI** (contract):
    - **10a. Command integration** [~300]: `--expect`, unknown-flag rejection, exit codes, human-policy output (proposal, effective change and the dashboard link), `issued_credential`.
    - **10b. Reads and events** [~300]: `adc events --follow --json`, resuming by seq.
    - **10c. HTTPS client** [~200]: R3.
11. **Deferred-action matrix and parity harness** [~250] (mechanical): assertions, mappings and generated output only, over behavior established above. Any schema or authority change makes it a contract PR.
12. **Installed smoke** [~300] (mechanical while the containment and adapter contracts are unchanged; contract otherwise).
    - **Command:** `make install-check` runs `scripts/installed-smoke.sh` against the installed prefix, under bwrap, with no source tree mounted.
    - **Prerequisites:** `make dist` packages:
      - `adcd` and `adc`;
      - an installed harness binary that drives `internal/adapterhost`;
      - the fake adapter with its workspace dependencies and the Node runtime;
      - `fixtures/t0/py-mini`.
    - **Fixture:** `py-mini-feature-1`.
    - **Expected results:**
      1. Bootstrap succeeds once.
      2. The operator enrols a supervisor.
      3. A grant is created.
      4. The supervisor takes the seat.
      5. A scoped command applies, and a scoped read returns only granted records.
      6. Stale, refused and deferred commands produce their registered reasons and exit codes.
      7. `adc events --follow --json` shows these events in seq order.
      8. A second `adcd` exits with `could_not_run/controller_already_running`.
      9. The fake adapter run ends with `attempt.result_submitted` applied and a `usage.measured` record.

      Real task execution and delivery stay in M1.

**Dependencies:**
- **Backbone:** 1 → 1b → 2a → 2b → 2c → 3.
- **Credentials:** 3 → 4a → 4b → 4c → 4d.
- **Seat and proposals:** 2c → 5 → 6a → 6b → 6c → 6d.
- **Identity and grants:** 6c → 7a → 7b → 7c → 7d → 8.
- **Reads:** 9a after 3; 9b–9e after the projections they read.
- **CLI:** 10a after 4a and 6c; 10b after 9e; 10c after 3b.
- **Finish:** 11 after 10a–10c; 12 last.

**Size:** about 29 PRs and roughly 7,000 hand-written lines.
