# M0 step-9 delivery plan: Astra reviews

Three rounds against plan revisions 1–3 (the reviewed revisions are kept in the lead session's scratch notes; revision 4 is `../14-step9-delivery-plan.md`). Line references point at the scratch plan file of that round.

## Review of the M0 step-9 plan and decisions (Astra, 2026-10-06)

Verdict: NO-GO

- **R1 — agree:** owner-only socket and private, daemon-owned state directory. Socket policy is already D37/Q3; ownership validation strengthens the existing lock checks.
- **R2 — agree:** shared socket and HTTPS require explicit opt-in; invalid explicit configuration must fail startup.
- **R3 — agree:** already decided by `docs/design/m0/08-controller-api-and-cli.md:12`; retain verified TLS without an insecure fallback.
- **R4 — change:** retain private-socket-only bootstrap, 24-hour expiry and deletion; specify transactional consumption, concurrent-client behavior, restart handling and lost-response recovery.
- **R5 — change:** also check ownership and regular-file identity through the opened descriptor; create/write credentials securely and never expose them through logs or persisted command responses.
- **R6 — change:** defer production WebAuthn deployment, while retaining the verifier and fake-authenticator contract tests. Human confirmation must remain unavailable until its complete security boundary exists.
- **D1 — change:** accept explicit unsupported-action refusals for the skeleton, but defer to each action’s owning milestone rather than promising everything in M1; preserve existing validator/fixture obligations.
- **D2 — change:** adopt the proposed smoke as additional coverage; specify installed fake-adapter dependencies and assertions, and retain the complete M0 exit checklist.
- **Open point: resource groups — agree:** static, validated startup configuration fits the current reservation implementation; reservations remain event-derived and unsupported group semantics fail closed.
- **Open point: read scope — change:** use explicit resource-to-domain/project mappings, not the first URL segment; preserve event-stream authorization semantics.
- **Open point: run directory/group — change:** require an explicit shared run directory and group initially; use a daemon-controlled, group-traversable directory and a connectable `0660` socket.

The blockers are H1–H2. The other findings should update acceptance criteria and sequencing; they do not block independent foundation work.

**High**

**H1. Proposal-only enrolment makes the planned installation unable to provision a supervisor.**

`.review-s9-plan.md:12` specifies `agent.enrol (proposal)`, while R6 defers the only human-confirmation channel. At this revision, `agent.enrol` specifically means “Enrol a supervisor actor and issue its token” (`schemas/vocab/actions.yaml:20`), not M3 remote-capacity-agent enrolment.

Operators are unrestricted under D31 (`docs/design/m0/04-authority.md:26`), and `authority.Decide` already allows this operator action directly (`internal/authority/decide.go:133`). Grants also require an existing live supervisor (`internal/authority/grants.go:139`). Consequently, proposal-only enrolment leaves no supported path from bootstrap to a usable supervisor, grants, seat ownership or scoped-read smoke tests.

**Fix:** implement operator enrolment as an applied, atomic actor-plus-token creation. An authorized supervisor’s enrolment request becomes a human-policy proposal. Keep remote agent enrolment in M3. Test the complete bootstrap → supervisor enrolment → grant → seat → scoped command/read flow without fixture seeding or direct database writes.

**H2. “Authorize = Decide” omits the production transaction contract needed by PRs 4–5.**

`.review-s9-plan.md:12–13` treats dispatch and proposal confirmation as small wiring steps, but the existing helpers do not supply the whole boundary:

- `authority.Request` expects server-computed effective-change classes and grantee identity (`internal/authority/decide.go:64`); it does not derive these from an HTTP request.
- The request schema leaves action-specific scope open, although its description requires a schema per action (`schemas/api/command-request.json:15`).
- The store’s expected-state mechanism supports subject versions, whereas the API also accepts digests (`internal/eventstore/writer.go:46`; `internal/cli/cli.go:142`).
- `Authorize` returns only an error, and errors roll back the transaction. A proposed result and a permanently invalidated proposal need committed events, not an authorization error (`internal/eventstore/writer.go:187`, `:290`; `internal/authority/proposal.go:137`).
- Grant persistence currently supports creation/revocation only; there is no extension transition (`internal/authority/grants.go:34`). Approval records and their authoritative linkage also need production persistence.

The design already requires effective-change classification, approval records and revalidation at application (`docs/design/m0/04-authority.md:55`, `:103`, `:192`).

**Fix:** add an explicit command-processing contract before exposing mutations. It must define:

1. Transport-authenticated identity, with current token/actor and grant checks at the writer boundary; public callers cannot select a system identity.
2. Schema-validated action inputs, server-derived object/project/grantee/classification, version **and** digest expectations.
3. Idempotency input containing the action as well as its body—otherwise identical bodies submitted to different command endpoints can collide.
4. Separate committed paths for applied approvals, proposals/challenges, and proposal invalidation.
5. Confirmation from authoritative receipts/provenance, exact-operation revalidation and atomic application; no client-supplied “GO” or policy decision.
6. Defined `grant.extend` semantics—either a checked extension event or atomic replacement, including its effect on existing grants, proposals and streams.

Also account for replay: `Submit` returns stored results before calling `Authorize` (`internal/eventstore/writer.go:265`). The HTTP boundary must authenticate replay requests; the hook alone cannot protect them.

**Medium**

**M1. Startup readiness needs a concrete projection and refusal policy.**

`.review-s9-plan.md:6` names verification and projector composition but does not specify how persisted projections become trusted. `RebuildProjections` compares against a temporary replay and always rolls back; it does not install rebuilt projections (`internal/eventstore/rebuild.go:23`). Hash verification alone does not validate projection contents.

**Fix:** make startup acceptance explicit:

- Validate configuration and filesystem ownership/permissions; acquire the lock before opening the store.
- Verify the complete chain; check a trusted head when supplied for restored state.
- Check capacity history on existing projections **before any replacement replay could manufacture capacity-admission history** (`internal/capacity/reserve.go:115`).
- Replay/compare all production projections and refuse on drift or ambiguity, unless an explicit repair operation installs verified replacements.
- Reject unsupported persisted event versions rather than silently ignoring them.
- Establish/load the persisted system actor, then perform required restart transitions; open listeners only after every readiness check succeeds.

This implements `docs/design/m0/03-event-store.md:100–112`. Test corrupt logs, projection drift, incompatible history, failed restart transitions and invalid listener configuration.

Static resource-group configuration is supported by `internal/capacity/reserve.go:44–83`. Schema-validate it before `NewReservations`; that constructor’s semantic checks are not full schema validation. Do not silently drop broker balances, restore-dependent groups or unsupported release rules.

**M2. Bootstrap needs crash and secret-delivery semantics, not just a file lifecycle.**

`.review-s9-plan.md:11`, `:23` preserve the adopted expiry/deletion rule (`docs/design/m0/04-authority.md:20–25`), but leave important transitions undefined.

The identity projector already enforces one-time first-operator and first-token creation (`internal/identity/identity.go:259`, `:304`). Build on that rather than maintaining an independent “bootstrapped” truth.

**Fix:** specify and test:

- First-start secret creation with secure exclusive file creation and an expiry that does not reset on restart.
- Private-listener identity enforced by server-side listener context, not a request header or claimed hostname.
- Secret validation, unconsumed-state check, first operator, first token and consumption committed atomically. Two clients racing with different idempotency keys produce exactly one successful bootstrap.
- Commit before deletion; a leftover file after a crash cannot authorize another bootstrap.
- A documented recovery path for expiry or a committed bootstrap whose response/token-file write was lost.

Keep issued secrets outside the store’s persisted result JSON: `Submit` saves that JSON (`internal/eventstore/writer.go:351`), while the identity contract stores only hashes and reveals secrets once (`internal/identity/token.go:51`; `internal/identity/identity.go:108`). Explicitly design this boundary for both bootstrap and enrolment.

**M3. Socket and credential defenses need acceptance criteria.**

R1–R3 substantially implement adopted policy, rather than introducing six wholly undecided security choices: see `docs/plan/00-brainstorm-decisions.md:77` and `docs/design/m0/08-controller-api-and-cli.md:10–12`.

However, “group-readable” in `.review-s9-plan.md:6` is insufficient for a usable Unix socket.

**Fix:**

- Use `0600` privately and `0660` for the configured shared group. The shared directory must permit traversal while preventing supervisors from replacing the socket.
- Validate ownership, directory/file type and unsafe path replacement; never unlink an arbitrary existing path while cleaning a stale socket.
- Add Unix peer-credential checks: same daemon UID for private/bootstrap connections; client verification of the expected daemon UID before sending credentials. Preserve bearer authentication on every socket. `api.ConnContext` currently records a connection solely for stream cutoff; it performs no peer authentication (`internal/api/conn.go:14`).
- For R5, check the opened token file’s ownership, type and permissions; create its private parent directory and write without following an attacker-selected destination.
- Bound request bodies, header reads and concurrent expensive token verification. Each well-formed token verification uses substantial Argon2 memory (`internal/identity/token.go:23`).
- Reject TLS misconfiguration, plaintext fallback and credential-forwarding redirects.

Peer checks are an additional defensive recommendation, not an existing D37 requirement. They do not make two processes sharing operator credentials separate security principals.

**M4. Token revocation/rotation is missing from the exposed administration surface.**

The code supports `IssueToken` and `RevokeToken` (`internal/identity/identity.go:108–127`), but `.review-s9-plan.md:12` exposes only actor revocation. The action registry likewise lacks token administration.

**Fix:** define authenticated token issue/revoke or replacement operations, with registered actions, policy, schemas and parity mappings. Rotation should replace a token without deleting its actor or grants, and revoke the previous token explicitly. Test old-token rejection and existing-stream closure.

Periodic automatic rotation is not required by the current design. A usable explicit replacement/recovery procedure is the missing defensive default.

**M5. The step-9 definition of done omits required API/CLI behavior.**

`.review-s9-plan.md:6` should additionally name:

- `--expect` enforcement and unknown-flag rejection; the CLI already implements explicit expectations and must retain them (`internal/cli/cli.go:64–87`; `docs/design/m0/08-controller-api-and-cli.md:61–67`).
- Human-policy output containing the proposal and effective change, plus the dashboard link. “Only the dashboard link” contradicts `docs/design/m0/08-controller-api-and-cli.md:65`.
- API build/version/schema-set reporting (`docs/design/m0/01-repo-layout-and-toolchain.md:105`).
- Revocation, expiry, narrowing, stalled readers and store-read failures over the assembled server, preserving the stream deadlines and `ConnContext` behavior (`docs/design/m0/08-controller-api-and-cli.md:27–43`).
- Redaction schemas/allowlists for new events and resource responses. New event payloads are withheld by default today (`internal/api/redact.go:14`).
- A tracked delivery decision for `adc doctor`’s credential-isolation warning (`docs/design/m0/04-authority.md:225`).

**Fix for the read-scope open point:** map resources explicitly to singular domain names and their authoritative project: `/v1/projects` → `project`, `/v1/grants` → `grant`, and so on. The first-segment rule currently applies to **event kinds**, not URL paths (`internal/authority/grants.go:243`). Authorize each returned record in the same snapshot; installation-wide records require the corresponding global scope. Blobs remain accessible only through readable referencing records (`docs/design/m0/04-authority.md:49–51`). Define `whoami` separately rather than accidentally inventing a `whoami` grant domain.

**M6. R6 is acceptable only as a deployment deferral with a closed confirmation endpoint.**

`.review-s9-plan.md:25` must not defer the WebAuthn security contract itself. D37 already fixes dashboard-only confirmation; RP/origin, credential ownership, user verification, counter handling and atomic challenge consumption are specified in `docs/design/m0/04-authority.md:176–198`.

**Fix:** retain the current real verifier and replay/expiry fixtures (`docs/design/m0/11-testing.md:81`). Keep human confirmation disabled while production RP configuration, credential persistence and dashboard session/CSRF protection are absent. Do not expose a fake-authenticator or bearer-only substitute.

`proposal.confirm` can implement independent-review confirmation in M0: that is exactly its registry description (`schemas/vocab/actions.yaml:15`). Human proposals remain pending until the actual dashboard capability arrives.

**M7. PR classifications, dependencies and size estimates need revision.**

The size limit counts **added plus deleted hand-written non-test code, including authored schemas**. The cohesive-package exception requires justification (`docs/plan/04-development-workflow.md:32–35`). The approximately 2,450-line total is not yet credible for the listed contracts.

| Proposed PR | Review classification and correction |
|---|---|
| 1. Daemon assembly | **Contract** for startup trust/refusal and authorization composition; a subsequent pure assembly refactor could be mechanical. |
| 2. Server/sockets | **Contract**: security configuration and transport boundaries are explicitly covered by §3. |
| 3. Bootstrap | **Contract**; separate lifecycle/secret-delivery contract from CLI integration if needed. |
| 4. Dispatch/Authorize | **Contract; likely split.** Separate command transaction boundary from identity/grant handlers. Include action schemas and approval persistence. |
| 5. Proposal/seat/note | **Contract; split now.** Proposal confirmation/revalidation, seat/handover, and notes are separate domains. |
| 6. Reads/OpenAPI | **Contract**; add each endpoint’s schema/OpenAPI description alongside its handler rather than leaving documented behavior behind. |
| 7. CLI transport | **Contract as proposed** because TLS trust and credential-file policy are security configuration. Separate stream/read presentation if that produces a genuinely mechanical PR. |
| 8. Parity/smoke | Mechanical only when using the existing containment boundary unchanged. Sandbox changes require **contract** review. |

The controlling classification is `docs/plan/04-development-workflow.md:48–56`.

**Dependency fixes:** move proposal event persistence ahead of any dispatch that returns `proposed`; settle the common principal/system-command boundary before bootstrap and dispatch proceed independently; establish seat semantics before enabling supervisor writes; implement reads after their authoritative projections exist. A supervisor must be able to request/take the seat without already holding it, while other writes retain the transactional seat check (`docs/design/m0/08-controller-api-and-cli.md:79–82`).

HTTPS server implementation currently has no explicit owner in the sequence. Assign it to the transport work or explicitly defer that optional listener.

**M8. The smoke is useful, but does not establish the complete M0 exit.**

`.review-s9-plan.md:29` gives “fake-driven T0 dry run” a reasonable concrete interpretation. It nevertheless needs command, fixture, expected results and prerequisites, as required by `docs/plan/01-first-release-plan.md:630`.

There is also a packaging prerequisite: `make dist` currently builds Go binaries only (`Makefile:69`); the fake adapter is TypeScript with workspace dependencies (`adapters/fake/package.json:8–19`). The installed-fixture sandbox mounts only the installed prefix (`scripts/installed-fixtures.sh:24`).

**Fix:** package the fake adapter, generated contract dependencies, required runtime and fixture data into a declared test installation. Run from that installation without source-tree imports or `go run`. Assert controller-derived output, fenced acceptance and recorded usage, rather than merely a successful fake process exit. Keep real task execution/delivery in M1 (`docs/design/m0/11-testing.md:65`).

Retain every exit row in `docs/design/m0/00-overview.md:46–58`: installed contract fixtures, mutation red-proofs, containment, generated schema documentation, vocabulary closure and trusted-head edit/truncation checks. Add transport smoke rather than replacing those checks.

Finally, label this plan **step-9 completion**, not complete M0 completion. The adopted M0 scope also includes:

- Artifact-graph schemas, computation, resolver/evaluator with atomic claiming, successor rounds and fixtures (`docs/design/m0/13-artifact-graph.md:237`).
- Evidence-node parameters, counting/caps, precondition mapping and fixtures (`docs/design/m0/12-evidence-jobs.md:71`).
- The remaining knowledge-spec and legacy-fixture obligations (`docs/design/m0/10-context-manifest-and-knowledge-spec.md:85`; `docs/design/m0/11-testing.md:102`).

Those need separate tracked completion evidence. They are not justification for expanding the daemon PR into cycle runtime.

**Low**

**L1. Make unsupported-action behavior and parity explicit.**

D1 is compatible with M0’s “no cycle behaviour” boundary (`docs/design/m0/00-overview.md:7`) if every registered action still has its endpoint and CLI/dashboard-stub mapping.

**Fix:** publish an explicit supported/deferred action matrix, including `review.request`, which D1’s examples omit. Return a registered capability-unavailable reason—preferably `could_not_run`—without confusing it with `action_unregistered` or an authorization denial. Preserve real effective-change and protected-change contract fixtures despite runtime deferral. Test that deferred endpoints produce no mutation.

The parity harness should exercise route dispatch and CLI endpoint selection, not merely compare identical generated lists. Dashboard controls may remain a manifest stub until M5 (`docs/design/m0/08-controller-api-and-cli.md:69–88`).

Reviewed at `18ecdd2`. No files modified; no tests executed for this read-only plan review.
## Review of the M0 step-9 plan, round 2 (Astra, 2026-10-06)

Verdict: NO-GO

H1 is resolved. H2’s missing work now has explicit owners and substantially correct dependencies, but PR 2a introduces two High-risk contract gaps below. R1–R6, M3 and M4 remain accepted; this review does not reopen those operator decisions.

| Round-1 finding | Status |
|---|---|
| **H1** | **Resolved.** Operator enrolment applies atomically; an authorized supervisor’s request becomes a human-policy proposal (`.review-s9-plan.md:21`). This matches `04-authority.md:26` and `decide.go:133`. |
| **H2** | **Partially resolved.** PRs 2a/2b establish the command boundary, and PR 5 precedes handlers returning `proposed`. Classification, expectations, authoritative approval linkage and revalidation are included. Close the PR 2a hazards below before considering this resolved. |
| **M1** | **Addressed.** Startup verification, capacity-history ordering, projection comparison/refusal, version rejection and listener readiness are explicit (`:18`, `:24`). |
| **M2** | **Partially addressed.** Atomic consumption, races, expiry and crash ordering are covered. Secret-delivery replay and an executable lost-token recovery procedure remain underspecified; see N3. |
| **M3** | **Addressed.** Socket modes, peer checks, controlled shared directory, descriptor checks, request bounds and Argon2 concurrency limits are explicit (`:6–12`). |
| **M4** | **Addressed at plan level.** Registered token administration, actor/grant preservation and stream cutoff are assigned (`:13`, `:31`). |
| **M5** | **Addressed.** Expectations, CLI output, version reporting, assembled-stream tests, resource authorization and redaction are named. The doctor warning is explicitly deferred (`:19`, `:28`, `:34–38`). |
| **M6** | **Addressed.** Deployment is deferred while the verifier/fixtures remain and human confirmation stays closed (`:11`, `:30`). |
| **M7** | **Partially addressed.** Classifications and major splits improve substantially. Seat/client dependencies and several oversized scopes remain; see N4–N5. |
| **M8** | **Mostly addressed.** Installed packaging, controller-derived assertions, preserved exit rows and step-9-only scope are explicit. Exact smoke invocation, fixture and prerequisites still need specification; see N7. |
| **L1** | **Addressed.** Supported/deferred matrix, `review.request`, capability refusal, no mutation and behavioral parity coverage are explicit (`:16`, `:36`). |

**High**

**N1. The revised idempotency formula changes the adopted uniqueness contract.**

At [`.review-s9-plan.md:26`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:26), “idempotency key = action + body digest + key” makes changed input select a different key. A retry with the same caller-supplied key but modified scope could execute a second mutation instead of returning `idempotency_key_reused`.

The adopted contract is **unique `(actor, idempotency_key)`**, with a separately compared input digest (`docs/design/m0/03-event-store.md:59`; `internal/eventstore/writer.go:270`).

**Fix:** preserve that uniqueness tuple. Compute the separate digest over the canonical action, complete request and expectations. Same actor/key with a different action, body or expectation must refuse; identical input must replay. If the formula was shorthand for this, replace it with the exact distinction. Retain the existing different-input red-proof and add cross-action/changed-expectation cases.

**N2. Returning committed refusals without an error requires an atomic migration of existing callers.**

At [`.review-s9-plan.md:26`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:26), `Submit` gains `proposed`, `invalidated` and `refused` outcomes, but the plan does not assign caller migration.

This has a concrete consequence: `internal/adapterhost/attempt.go:145` discards the outcome and proceeds when `err == nil`; line 150 enters the adapter run. Its helper returns `Store.Submit` directly at line 351. A committed refusal returned with nil error could therefore permit launch without an applied start transaction. Installed fixture callers also commonly inspect only the error.

**Fix:** make exhaustive caller migration part of PR 2a, or preserve an applied-only wrapper for existing internal callers. Only an explicitly applied outcome may advance execution; proposals/refusals/invalidation cannot create target effects. Add a refusal-before-start test proving no adapter launches.

Also distinguish internal disposition from HTTP disposition: `schemas/api/command-response.json:6` allows only applied/proposed/refused. A committed proposal invalidation should normally return a refused response with its registered reason, while retaining the invalidation event.

**Medium**

**N3. Secret stripping needs an explicit replay contract and usable recovery path.**

At [`.review-s9-plan.md:9`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:9) and `:26`, the first response contains a secret while its persisted/replayed result does not. This correctly protects credential storage, but it qualifies the “same result” promise in `docs/design/m0/03-event-store.md:59` and `schemas/api/command-response.json:13`.

**Fix:** define a stable persisted command result plus a typed, first-delivery-only credential field, explicitly absent on replay. Update the response schemas/documentation and CLI handling together. Never silently mint another token on replay.

Specify recovery for bootstrap, enrolment and replacement, including loss of the **only operator credential** after commit. “Use token replacement” alone cannot recover that case: issuance requires an operator, while the system’s bootstrap issuance is one-time (`internal/identity/identity.go:309`). Name the authorized recovery mechanism, preserve bootstrap consumption, and test commit-before-response and token-file-write failure.

Authenticated replay is correctly assigned to PR 2b. Acceptance should explicitly cover missing/invalid credentials, revoked token/actor, actor-bound lookup, and revocation between HTTP authentication and writer access. Place the promised writer-boundary identity recheck before returning a cached result; the current replay branch bypasses `Authorize` (`writer.go:265`). This need not re-execute the original mutation.

**N4. Two dependency edges remain unsafe or incomplete.**

At [`.review-s9-plan.md:29`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:29) and `:38`:

- **Seat enforcement follows supervisor-capable handlers.** PRs 5/6 precede PR 7, although supervisor writes require a transactional seat check (`docs/design/m0/08-controller-api-and-cli.md:80`). Move the seat guard/projection ahead of supervisor-handler activation, or explicitly keep those paths refused until PR 7. Taking/requesting the seat remains the exception.
- **Bootstrap CLI precedes its secure transport.** PR 4 includes a working CLI client, but Unix transport, daemon-UID verification and credential handling are assigned to PR 10. Extract that minimal client foundation before PR 4, or split server bootstrap from CLI integration.
- **PR 1’s system actor needs a narrow internal path.** “Refuse-all Authorize” cannot also reject the command that creates the initial system actor. Specify that trusted startup operation in PR 1 without introducing a public system-identity bypass.

The other principal dependencies are sound: transaction contract before mutations, proposal persistence before `proposed`, resource reads after their projections, and smoke last.

**N5. Several PR scopes still lack a credible ≤~400-line boundary.**

At [`.review-s9-plan.md:23`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:23), the stated cap is not supported by per-PR budgets. Likely splits, counting authored schemas and added **plus deleted** lines:

| PR | Split now |
|---|---|
| **2b** | Common authenticated-principal/derivation framework; deliver individual action schemas and derivation with their owning handlers. |
| **5** | Proposal/approval persistence and linkage; confirmation/revalidation/application and withdrawal. |
| **6** | Enrolment/actor administration; token administration; grant creation/revocation/replacement. |
| **9** | Read-authorization foundation, then resource families with their schemas/OpenAPI/redaction. |
| **10** | Secure transport/credential foundation; command integration; reads and SSE presentation. |

PRs 3 and 4 also warrant early line budgets because filesystem security, configuration and CLI integration are substantial.

Under `docs/plan/04-development-workflow.md:48`, PRs 1–10, including 3b, are correctly **contract** work. PR 11 is mechanical only if it adds assertions/mappings/generated output over behavior already established in contract PRs; schema, authority or design-policy changes make it contract. PR 12 is mechanical only while preserving the containment and adapter contracts unchanged. The cohesive-package exception requires an explicit justification (`:32`).

**N6. Grant replacement is consistent, but its lineage representation remains unassigned.**

At [`.review-s9-plan.md:20`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:20), atomic replacement agrees with 04’s revocation precedence and revalidation rules (`04-authority.md:147`, `:192`). It also fits the projector: revoke the exact original grant, then create a **fresh ID** under a live operator (`internal/authority/grants.go:127`, `:135`, `:147`).

However, existing grant and grant-event schemas have no superseding field and reject extra properties (`schemas/records/grant.json:7`; `grant-event.json:7`).

**Fix:** assign the typed lineage representation to the grant PR—such as schema-validated approval scope binding old/new IDs, or a compatible versioned event. Bind replacement to the old grant’s expected state and the complete new grant; retain `grant.extend` classification and the self-grant prohibition. Commit revocation, creation, approval linkage and proposal invalidation together.

Existing streams holding the old grant will **close**, even for a widening replacement, because `ReadScope.Holds` rejects any revoked held grant (`grants.go:213`). That is consistent with `08-controller-api-and-cli.md:39`; explicitly test reconnect under the replacement rather than implying seamless continuation.

**Low**

**N7. The installed smoke still needs its executable acceptance row.**

At [`.review-s9-plan.md:37`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:37), “one fixtures/t0 task” leaves the exact fixture and invocation open.

**Fix:** name the installed command, fixture, expected results and prerequisites as required by `docs/plan/01-first-release-plan.md:630`. Include both fake-adapter workspace dependencies (`adapters/fake/package.json:18`), runtime dependencies and the installed harness that invokes `internal/adapterhost`. Keep source-tree imports unavailable. Add concrete tracking references for the separately deferred M0 obligations and doctor warning.

Reviewed against `18ecdd2`. No files modified; no tests executed for this read-only plan review.
## Review of the M0 step-9 plan, round 3 (Astra, 2026-10-06)

Verdict: GO with changes

The round-2 High findings are resolved at plan level. No new Critical/High findings. R1–R6, M3 and M4 remain accepted; R7 is sound in principle but still requires the operator’s approval.

- **H2 — Substantially addressed.** Dispositions, authenticated identity, derivation, approval persistence and revalidation have owners. Explicit ownership of server-side digest expectations needs restoring.
- **M2 — Partially addressed.** Bootstrap consumption, races and delivery failures are covered. R7 addresses lost credentials after consumption, but cannot recover an expired bootstrap secret before an operator exists.
- **M7 — Partially addressed.** Classification and splitting improve substantially. Remaining sequencing and budget issues are below.
- **M8 — Addressed at plan level.** The installed smoke has a command, fixture, prerequisites and controller-derived assertions; other M0 obligations remain separately tracked.
- **N1 — Resolved.** `.review-s9-plan.md:23` preserves uniqueness on `(actor, idempotency_key)` and separately compares a digest covering canonical action, complete request and expectations. This matches `03-event-store.md:59` and `writer.go:270`; the existing writer already includes expectations in its digest at `:241`.
- **N2 — Resolved.** `.review-s9-plan.md:24` assigns exhaustive caller migration to PR 2a. The applied-only wrapper protects `adapterhost/attempt.go:145` and error-only fixture callers. Mapping internal `invalidated` to HTTP `refused`, while committing the invalidation event, preserves the response union.
- **N3 — Delivery and replay addressed; recovery conditional.** The typed first-delivery field, secret-free persisted result, schema/documentation updates and no-remint replay rule resolve the `command-response.json:13` mismatch. The writer identity recheck explicitly precedes cached-result delivery and covers the HTTP-to-writer revocation race. Completion depends on R7 or an approved alternative.
- **N4 — Original issues resolved.** Seat enforcement precedes later supervisor handlers; secure client transport precedes bootstrap CLI integration; startup has a private system-actor operation. That operation must still use validated, transactional event append and projection.
- **N5 — Improved, not closed.** The major splits and per-PR budgets are present; several scopes remain optimistic.
- **N6 — Addressed.** `grant.replaced/v1` provides typed lineage, binding the old grant, complete replacement and approval. Fresh identity, atomic revocation, proposal invalidation and stream reconnection are consistent with authority precedence. Clarify event subjects/versioning below.
- **N7 — Addressed at plan level.** `.review-s9-plan.md:50` names the installed invocation, task and prerequisites. The tracking-issue links promised at `:3` remain a pre-PR-1-merge deliverable.
- **R7 — Sound with implementation conditions; approval pending.** Daemon-UID recovery fits D31’s credential-isolation boundary. It does not replace D37’s dashboard WebAuthn confirmation. Refusal while the daemon holds the lock, against an unverified store, or for a revoked/non-operator actor is required and substantially stated already.

**Medium**

**P1. R7 needs a distinct replayable identity transition, not merely the startup bypass.**

[`.review-s9-plan.md:26`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:26) proposes issuance through the startup path. However, `internal/identity/identity.go:309` permits the system actor to issue only the first operator’s first token; `:224` also prevents ordinary system-issued token revocation. Bypassing `Authorize` does not bypass those projector checks.

**Fix:** make PR 4c own a registered, versioned recovery event and deterministic projector transition, including optional revocations atomically. Preserve the ordinary issuance restriction and bootstrap-consumed projection. Record the system as issuer and the existing live operator as recipient.

Define “verified store” as the applicable PR-1 trust checks, including projection comparison and trusted-head verification for restored state—not chain verification alone. Hold the same lock continuously from before opening through closing the store. Test daemon-running, projection drift, corrupt history, revoked/missing/non-operator targets, replay/restart and output-file failure. Recovery must never confirm proposals, resurrect actors or reset bootstrap.

**P2. Expiry before bootstrap consumption still strands the installation.**

[`.review-s9-plan.md:10`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:10) fixes expiry at creation, while R7 at `:26` requires an existing live operator. After 24 hours without bootstrap, neither path works. This is the remaining expiry case from M2, distinct from losing a committed response.

**Fix:** document an explicitly authorized recovery procedure for an expired, unconsumed installation. Any secret renewal must invalidate the old secret and remain impossible after consumption; ordinary restart must not renew expiry. Assign its acceptance tests to bootstrap. Also state that step-9 completion requires an approved credential-recovery path if R7 is declined.

**P3. Digest expectations have lost their explicit implementation owner.**

[`.review-s9-plan.md:35`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:35) assigns principal derivation, and `:48` assigns CLI `--expect`, but neither explicitly implements server-side digest comparison. `internal/eventstore/writer.go:46` and `:294` support only subject versions; `internal/cli/cli.go:142` already accepts digests. Including expectations in the idempotency digest does not enforce them.

**Fix:** assign transactional version **and** content-digest expectation checking to the command framework before handler activation. Each handler must supply its authoritative object/digest definition; enforce both when supplied. Include stale-digest and concurrent-change acceptance cases.

**P4. Seat handlers still precede their approval/proposal persistence dependency.**

[`.review-s9-plan.md:40`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:40) includes take/release/handover handlers before PR 6a. Seat commands are authority-gated actions, and grants can tighten their confirmation policy (`internal/authority/decide.go:173`). Exemption from already holding the seat does not exempt them from authorization, approvals or proposals.

**Fix:** place persistence-only PR 6a before seat-handler activation, while retaining the seat guard before other supervisor writes. For example: principal framework → proposal/approval persistence → seat → confirmation/application. Alternatively, land only the seat projection/guard early and activate handlers afterward.

**P5. Several budgets still need concrete file-level estimates or further splits.**

[`.review-s9-plan.md:39`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:39), `:42`, `:45` and `:47` remain the least convincing boundaries:

| PR | Remaining pressure | Suggested boundary |
|---|---|---|
| **4c, ~200** | Recovery CLI, new schema/projector transition, optional revocation and failure-safe credential delivery | Separate the recovery identity contract from CLI integration, or justify cohesive size. |
| **6b, ~350** | Receipt-backed confirmation, authoritative revalidation, application dispatch, invalidation and withdrawal | Keep withdrawal separate; budget the confirmation/application transaction explicitly. |
| **7c, ~400** | Three handlers and schemas, replacement lineage/projector changes, approval linkage and proposal invalidation | Create/revoke first; replacement second. |
| **9b, ~300** | Four resource families with response schemas, authorization, OpenAPI and redaction | Split by resource family after the shared foundation. |

These are estimates, not measured overruns. Apply the added-plus-deleted rule and documented cohesive-package exception from `docs/plan/04-development-workflow.md:32`; do not compress contracts to meet nominal budgets.

**Low**

**P6. Specify how replacement advances both grants’ expected-state versions.**

[`.review-s9-plan.md:30`](/tmp/claude-1000/-home-mega/d7e41cde-3883-4e5c-8d98-04b890c5f4c4/scratchpad/wt-s9plan/.review-s9-plan.md:30) does not say whether `grant.replaced/v1` supplements or replaces the existing revoke/create events. Each event has one subject, and `internal/eventstore/writer.go:179` derives versions from that subject. A sole replacement event cannot advance both subjects through that mechanism.

**Fix:** retain revoke/create events under their respective grant IDs and add the lineage event atomically, or explicitly define equivalent authoritative version tracking. Verify expected-state reads and projection rebuild for both IDs.

Reviewed against `18ecdd2`. No files modified; no tests executed for this read-only design review.