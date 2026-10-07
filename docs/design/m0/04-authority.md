# 04 — Authority: actors, tokens, grants, approvals, proposals

Implements plan §2.11, D23, D31, D32, Astra A8 and M0 review [M3][M4][M5][M12]. `internal/authority` is the only place
that decides whether an action is allowed. It runs **inside the writer transaction** (03).

## Actors and tokens

```yaml
Actor:
  id: act_…
  kind: operator | supervisor | system      # system = adcd's own components
  name: string                              # display only
  declared_agent: string|null               # self-declared label; audit only, never enforcement
  created_by: act_…
  revoked_at: time|null
```

- **Tokens** are opaque bearer secrets bound to one actor, stored hashed (argon2id), shown once at creation. The CLI
  reads its token from `ADC_TOKEN` or `~/.config/adc/token` (0600). Dashboard sessions resolve to an actor the same way.
- **Bootstrap [M5].**
  - On first start, `adcd` writes a one-time setup secret to `<state_dir>/bootstrap.secret` (0600, owner-only). It is
    never written to logs or stdout; the log says only where the file is.
  - The secret creates the first operator actor exactly once. Consumption is an event, so it survives restarts, and the
    file is deleted. It expires after 24 h.
  - Mechanics (14 R4): the file is created exclusively (`O_EXCL|O_NOFOLLOW`, 0600) through the held state-directory
    descriptor, and only its hash and its expiry, fixed then, are recorded (`identity.bootstrap_issued/v1`). A restart
    never renews or recreates a recorded secret. `POST /v1/bootstrap` is served only on the private socket, as its
    listener says. One writer transaction checks the secret, creates the first operator and its first token, and
    consumes the bootstrap; the file is deleted after the commit, and a leftover one never bootstraps again. A wrong,
    expired or spent secret is `failed/confirmation_refused`. `adc admin bootstrap` reads the secret from the file or
    stdin, never argv, and writes the token file.
  - Recovery: two offline commands, run as the daemon uid while the daemon is stopped. Each takes `adcd.lock` before
    the store opens and releases it after the store closes, so it is refused (`could_not_run/controller_already_running`)
    while `adcd` runs. Each runs every startup trust check (the state directory, the hash chain and `--trusted-head` when
    given, the capacity history, unread event kinds, the projection comparison), refuses on any failure as `adcd run`
    does. It never creates a store or any part of one: the lock is taken without creating its file, and the database
    and blob directories must already exist, verified read-only under the lock before the store opens. It commits one
    event as the system actor through its claim; no endpoint reaches it.
    Exit codes are the single table's (08): 0, 1 refused, 2 could not run, 64 usage.
    - `adcd recover operator-token --state-dir DIR --actor ID --out FILE [--revoke-others] [--force-overwrite]
      [--resource-groups FILE] [--trusted-head SEQ:sha256:HEX]` (14 R7), for a lost operator credential (or a
      bootstrap whose response or token-file write was lost: a retry replays without the token). It mints the token
      outside the transaction and records `identity.token_recovered/v1`, with `--revoke-others` revoking the operator's
      other live tokens in the same event. A target that is missing, revoked or not an operator is
      `failed/recipient_not_operator`. The secret goes only to `FILE`, written as the token file is (R5): its directory
      is the user's, not group- or other-writable and not itself a symlink; a temporary file is created
      `O_EXCL|O_NOFOLLOW` 0600, synced and renamed into place without replacing anything unless forced, so `FILE` is
      never followed. Before anything is recorded, an existing `FILE` is refused (exit 64) unless `--force-overwrite`,
      and one that is neither a regular file nor a symlink, or cannot be inspected, is refused even then. If the file
      cannot be written after the commit, recovering again with `--revoke-others` retires the undelivered token. It
      confirms no proposal, resurrects no actor and leaves the bootstrap alone.
    - `adcd bootstrap renew --state-dir DIR [--resource-groups FILE] [--trusted-head SEQ:sha256:HEX]` (14 R8), for an
      expired, unused secret or a recorded one whose file is gone. While the bootstrap is unconsumed it writes a fresh
      secret every time (never the current one) through the held state-directory descriptor, replacing the old file,
      and records `identity.bootstrap_renewed/v1` with a new 24 h expiry; the old secret no longer bootstraps. After
      consumption it is `failed/confirmation_refused`. Startup never renews.
  - All logs redact tokens, secrets and challenge values by type, not by pattern guessing.
- **Enrolment and revocation (14 PR 7a).**
  - `agent.enrol {actor, name, declared_agent}` creates a supervisor and its first token in one command. The client
    mints the actor id, so a retry under the same key replays and the same id under a new key is refused. The request
    layer mints the token outside the transaction; only its hash is recorded, and the secret reaches only the live
    response of the call that applied it (14 N3). If that response is lost after the commit, a repeat replays without
    the token, so recovery is `actor.revoke` followed by enrolling under a new actor id.
  - A supervisor's `agent.enrol` is refused (`operator_only`), not proposed. Its `human` policy cannot be confirmed
    until the dashboard's WebAuthn is deployed (14 R6), so the proposal could only expire. Its token would also have
    no one to go to: at apply it would be minted by the confirming request and delivered only to the confirming
    operator's dashboard response, never to the proposer, since a supervisor holding a second identity defeats
    separation of duties and the one seat. That delivery path, and an `Applier` that receives the confirmer, arrive
    with WebAuthn.
  - `actor.revoke {actor}` revokes every live token of the actor, then the actor. The fold invalidates its pending
    proposals and frees its seat. Only an operator revokes (`operator_only`). The system actor and the last live
    operator are refused (`actor_protected`), because bootstrap is spent once and offline recovery (14 R7) serves only
    a live operator. A live operator is an unrevoked operator actor, whether or not it holds a live token: recovery
    issues it one, so token administration (7b) must not tighten this rule. The refusal is committed, so it is final
    under its idempotency key: a retry replays it, and the CLI (10a) uses a new key for a new attempt.
  - Minting costs one Argon2id hash (64 MiB). `Authorizer.Enrol` reads first and skips the mint for a caller who is not
    a live operator and for a stored key (a replay records no token). The command endpoint takes
    each mint through the listener's Argon2 slots (`api.Slots`), like a bearer verification, and answers the
    credential only in a fresh applied response (`issued_credential`).
  - Neither action is seat-exempt (only taking or asking for the seat is, 14 N4/P4); operators never need the seat.
- **Token administration (14 M4, PR 7b).** Three registered actions, each an operator's alone (`operator_only`, like
  actor administration); a supervisor's own revocation waits for a later build.
  - `token.issue {actor}` issues another token to a live operator or supervisor. `token.revoke {token}` revokes one
    token. `token.replace {token}` rotates one: a single command revokes it and issues its holder a new token, so the
    holder keeps its actor and grants (grants name the actor, never a token), the old bearer is refused, and its
    streams close on their next recheck (`identity.StillLive`).
  - Issuing works like enrolment (14 N3): the request layer mints the token outside the transaction, only its hash is
    recorded, and the secret reaches only the live response of the call that applied it. The early read skips the mint
    for a caller who is not a live operator, for a stored key and for a target that is not live; the writer rechecks
    all three. The result names the token (`{actor, token}`, and `replaced` for a rotation), never its secret.
  - A missing or revoked token, or a holder that is missing, revoked or the system actor, is a committed refusal
    (`token_not_live`), and so is a target that became live after the early read skipped the mint: nothing was minted
    to record, and a new key retries. Revoking an operator's last token is allowed: a live operator needs no live token (above), and
    offline recovery (14 R7) issues it another.
  - A lost response leaves a token nobody holds. A repeat under the same key replays the result, which names that
    token without its secret, and the operator revokes or rotates it.
  - The object is the actor for `token.issue` and the token for the other two, so `--expect` names the version the
    caller inspected. For `token.issue` that is the actor's version, which token events never advance: it binds the
    holder's record, not its set of tokens, so it does not detect a concurrent issue or revocation for that holder. No
    change class applies: operators are unrestricted (D31), and a supervisor is refused first.
  - An operator may issue a token to another operator, as well as to a supervisor: that is intended (D31), and each
    token's `created_by` records who issued it, for audit.
- `operator` tokens are unrestricted (D31).
- `supervisor` tokens act only under grants.

## Grants

```yaml
Grant:
  id: grt_…
  actor: act_…                      # a supervisor actor
  projects: [prj_…] | "*"
  actions: [action_kind, …]         # write actions allowed
  reads: {projects: same|[prj_…], kinds: [resource_kind, …]}   # read scope [M5]
  valid_from: time
  expires_at: time                  # required
  confirmation_overrides: {action_kind: independent_review|human}   # may only tighten
  issued_by: act_…                  # an operator
  approval: apr_…|null              # the approval that confirmed its proposal; null when an operator issued it directly
```

A supervisor cannot create, extend or widen grants (`human`, and never by the grantee).

**Creation and revocation (14 PR 7c).**
- `grant.create` takes the grant without `valid_from`, `issued_by` or `approval`, under an id the client mints, so a
  retry replays and the same id under a new key, for any grantee, is refused. The controller sets `valid_from` to the
  time it is applied. Its class is its own and its grantee the scope's actor. Its project is the one project the grant
  names, in `projects` and listed read projects; several, or every project, make it installation-wide. It is refused
  (`grant_not_issuable`) when its id is taken, its expiry is not after the command's time or its grantee is not a live
  supervisor. Only a command applied at once (an operator's) is checked in its derivation; a supervisor's is checked
  only when its proposal is applied, where a failure invalidates the proposal (`proposal_invalidated`). A supervisor
  that may not create grants is refused its authorization whatever the id or grantee, so no refusal reveals a taken id
  or an actor's existence.
- An operator's is applied at once (D31), issued by the operator with a null `approval`: approvals under policy `none`
  are not persisted. A supervisor's own is refused (`self_grant_refused`); any other becomes a `human` proposal under
  the supervisor's grant, which no review confirms (14 R6). Applied after its confirmation, the grant is issued by the
  confirming operator and names the approval the same command records; the fold admits a grant naming an approval only
  when it is exactly the grant that approval's proposal asked for, by the approval's actor. The fold of
  `grant.created/v1` is thereby stricter than before this PR: every grant is valid from its issuing event's time (a
  grant is never backdated or future-dated), and an id used for any grantee is refused (each id is indexed,
  `grant_ids`).
- `grant.revoke {grant}` names a live grant (else `ErrNoGrant`, before any event) and is an operator's: the fold admits only a live operator's revocation, so a supervisor is
  refused (`operator_only`). The fold invalidates the pending proposals made under the grant
  (`authority_revoked`), and streams re-check it.
- Neither action is seat-exempt. Each binds the grant (record and version) for `--expect`.

**Replacement (14 PR 7d, N6/P6).** `grant.extend {replaces, id, projects, actions, reads, expires_at,
confirmation_overrides}` replaces the live grant `replaces` by a fresh grant `id` for the same grantee, never named by
the caller. One command commits `grant.revoked` (old id), `grant.created` (new id) and `grant.replaced/v1` (subject the
old id: `{replaces, grant}`, the complete new grant and so its approval), so both ids' versions advance. Its object is
the old grant: `--expect` and a proposal bind its state. The class is `grant.extend` and the grantee the old grant's, so a
supervisor's own grant is refused (`self_grant_refused`) and any other is a `human` proposal; the project is the one
both grants name, else installation-wide. Other kinds are refused (`operator_only`). The new grant's
`grant_not_issuable` checks run only for an operator and at application, where a failure invalidates the proposal.
What a supervisor learns about grant ids:
- Not authorized for the project: nothing. A grant it cannot replace (absent or revoked) is refused as
  `action_not_granted` with Decide's own detail ("no grant in force covers grant.extend for actor …"), identical by
  design to the refusal of an existing grant outside its scope, though "covers" there names no grant.
- Authorized to extend grants in a project: which grant ids there are live (a proposal, or the refusal above). That
  is the reach its grant gives it (14 P3), and accepted.
- Authorized but not holding the seat: an absent id is refused before Decide, a live one by the seat (`seat_not_held`)
  after it. The ids are unguessable, and the supervisor is already authorized for the project.

The fold admits a lineage only by a live operator, for the old grant revoked and the new grant created, for the same
grantee, by the same command (each grant event's command is recorded, `grant_commands`; never the time, which
consecutive commands may share), once per old grant and once per new grant (`grant_lineage` indexes both ids); a grant
naming a `grant.extend` approval only with that extension's old grant revoked by the same command. Revoking the old grant invalidates a pending extension
(its object moved) and the proposals made under the old grant, and closes the streams that hold it; a reader reconnects
under the replacement.

## Read authorization [M5]

- Every read (resources, blobs, notes, digests, events) is authorized server-side against the actor's grant read
  scope. Operators read everything.
- Blob reads are authorized through the referencing record's project, never by digest alone.
- **Long-lived streams (SSE) re-check authority** on every grant or actor change event. A revoked or expired grant
  closes the stream with `failed/authority_revoked`, and filtering is enforced server-side, never by the client.

## Action registry and effective-change classification [M3]

`schemas/vocab/actions.yaml` lists every mutating action kind a request can name. The exceptions are the controller's
system-only actions: `identity.bootstrap_issue` (the first start records the bootstrap secret), `identity.bootstrap`
(`POST /v1/bootstrap` spends it, where the setup secret, not a bearer, is the credential), and the offline recovery
commands' `identity.token_recover` and `identity.bootstrap_renew`. They are unregistered and have
no command endpoint of their own. The daemon's Authorize hook admits them for the system principal alone, never for a
bearer. Authority does **not** trust the endpoint name. For
every command, it computes the **effective change**: the diff between the current and resulting policy-relevant state.
That state covers:
- data floor;
- routing requirement;
- allowed destinations;
- source exclusions (below);
- usage profile;
- protected paths and documents;
- paid-fallback flags;
- grants;
- enrolled agents;
- knowledge export.

It then classifies the command by the most sensitive change it contains. A profile adoption that widens destinations is
treated as `policy.widen_destinations`, whatever it was called.

**Source exclusions are bound policy, not memory** (intake B4, `docs/research/2026-10-05-nobetci-phase1-close-intake.md`;
nöbetçi L1: a recovery path launched on a provider the operator had just excluded, because the exclusion was a
preference, not a switch).
- **Exclusions.** The installation keeps an **exclusion set** in its policy-relevant state: sources, executors or model
  families that no placement may use, each with the recorded action that set it.
- **Adding one** (`capacity.exclude_source`) only narrows routing. It needs a grant scoped to the installation (a
  project-only grant never suffices, because the set is installation-wide) and no further confirmation, so an
  exclusion takes effect at once.
- **Lifting one** (`capacity.include_source`) widens routing. The effective-change classification makes it
  `policy.widen_destinations`, with that class's D32 semantics: a supervisor's lift waits for a human confirmation,
  and an operator's own lift is that human decision.
- **One reader.** Placement (05) reads the effective exclusion set; no launch path has another source of routing
  truth.

Sensitive change classes and their supervisor confirmation defaults (D32, plus the knowledge actions from K5/KA1):

| Change class | Supervisor confirmation |
|---|---|
| `install.self` | `independent_review` |
| `policy.widen_data` · `policy.widen_destinations` · `policy.weaken_protection` (removes protected paths, loosens routing, disables or repoints the required-configuration check, 07) | `human` |
| `capacity.allow_paid_fallback` | `human` |
| `agent.enrol` | `human` |
| `grant.create` · `grant.extend` | `human` (never by the grantee) |
| `knowledge.accept_claim` (fact, lesson, negative result) | `independent_review` (settable to `human`) |
| `knowledge.export_framework_prose` (KA1) | `human`, **for every actor kind**: the only action where an operator token also needs a presence-verified confirmation, because acceptance never implies export |
| `knowledge.change_protected_document` | `independent_review`, plus the effective-change check below |
| `integration.change_topology` [M12] | `independent_review`, plus the effective-change check |
| anything else in the grant's `actions` | `none` |

## Approval records

```yaml
Approval:
  id: apr_…
  proposal: prp_…|null              # the one proposal it confirms; its shared fields are copied from it, never supplied
  actor: act_…
  actor_kind: operator|supervisor|system
  action: action_kind
  change_class: [class]             # computed by authority, never supplied
  project: prj_…|null
  object: {type, id, version, digest}
  expected_state: {version|digest}
  scope: {…}                        # schema per action
  grant: grt_…|null
  confirmation: {policy, challenge: chl_…|null, satisfied_by: [evt_…]}
  answers: [owed_input_id]          # owed inputs (reviewer demands) this approval answers; see below
  supersedes: [apr_…]               # explicit lineage
  question: string|null             # what the actor was asked (from the proposal or packet)
  answer_verbatim: string|null      # the actor's own words, when a human answered
  reason: string
  at: time
```

### Owed inputs (nöbetçi Δ§1, Δ§3; intake A1)

- When a reviewer or an evidence requirement needs something only an outside party can give (a ruling, lab data, a
  named identity), it records an **owed input**: `{id: own_…, requested_by, kind: ruling|data|receipt, question,
  subject}`. The dependent work holds on `awaiting_input`.
- Answering is its own action, `input.answer` [PR5-1]:
  - its scope carries `{owed_input: own_…, owed_revision, answer: typed payload}`;
  - `answers` is derived from the scope, so both are inside the action's binding digest (and the proposal digest when
    confirmation applies);
  - authority validates, in the writer transaction, that the responder may answer this kind for this project, that the
    owed input's subject and revision still match, and that the owed input is still open.

  An approval that merely mentions an id answers nothing.
- **Kinds are not interchangeable.** A `ruling` or `data` input can be answered by an approval. A `receipt` input
  (execution evidence) can be satisfied **only** by an applicable receipt (06), never by a ruling.
- The answer becomes an input of the next review's fingerprint (10), so a new answer re-keys the review. A review never
  receives answers for inputs it did not owe.
- **Delivery is structural** [D40]. An accepted answer is an `approval_answer` artifact. A review node that declared
  the owed input has it as an input (13), and nothing else can reach a model (10). There is no separate collector.

- **Transactional and idempotent** (03).
- **Precedence:** a revocation or narrower scope always wins over an earlier, wider one. Conflicting approvals in force
  make the projection fail closed (`ambiguous_state`).
- **Model output never acquires authority.** Plans, conventions and proposals are inert until an approval references
  their exact digest.

## Proposals and confirmation [M4]

When an action's policy is not `none`, the controller records a **proposal** and, for `human`, a **challenge**:

```yaml
Proposal:
  id: prp_…
  action, change_class, project, object, expected_state, scope
  proposed_by: act_…
  policy: independent_review | human
  grantee: act_…|null               # a grant change's grantee, as derived
  binding_digest: sha256:…          # JCS digest of {action, change_class, project, object, expected_state, scope, proposed_by[, grantee]}
  status: open | confirmed | applied | rejected | withdrawn | expired | superseded | invalidated   # open = pending
  invalidated_reason: reason|null   # registered: stale_expected_state, authority_revoked, actor_revoked, proposal_invalidated
  created_at: time
  expires_at: time                  # created_at + 72 h

Challenge:                          # human policy only
  id: chl_…
  proposal: prp_…
  binding_digest: sha256:…
  nonce: 32 random bytes            # fresh, single use
  allowed_confirmers: operator actors
  expires_at: time                  # short (default 15 min); re-issued on demand
  used_at: time|null
```

- **Human confirmation is WebAuthn on the dashboard only** (operator ruling D37). The pager sends a notification and
  a link; it carries no confirmation code. The dashboard shows the proposal's human-readable effective change, then
  requests a WebAuthn assertion whose challenge is `nonce ‖ binding_digest`: the 32 nonce bytes followed by the 32 raw
  bytes of the digest (64 bytes, never its `sha256:` text). The server verifies, per the WebAuthn spec:
  - RP ID and origin;
  - challenge equality;
  - signature over authenticatorData + clientDataHash;
  - the user-presence flag (user verification required);
  - signature counter monotonicity (an authenticator without a counter, always 0, is accepted per the spec; the
    challenge's single use then stops a replay);
  - the credential belongs to an operator actor that is not revoked.

  The challenge is consumed atomically.
- **Independent-review confirmation:** a review receipt (06) whose typed subject is this proposal's `binding_digest`,
  whose recorded model families are independent of the proposer and of every contributor to the object, and whose
  mechanically derived verdict is GO. A GO for any other subject never applies.
- **Revalidation at apply.** In the same writer transaction that applies a confirmed proposal, authority re-checks:
  - the object's expected state;
  - that the proposer's grant is still in force and still covers the action;
  - that no tighter policy now applies;
  - that the confirmer is still authorized.

  Any change makes the proposal `invalidated`, never applied.
- An open proposal is an `awaiting_confirmation` hold on the dependent work only.
- **Persistence (14 PR 6a).** A command authority proposes commits `proposal.created/v1` with the `proposed`
  disposition and no target effect. The same exact operation (same binding digest) while one is pending returns the
  pending proposal, with no new event. The fold invalidates a pending proposal when its proposer's actor is revoked,
  its grant is revoked, or its object has a new event. An approval (`approval.recorded/v1`) answers exactly one live
  proposal, at most once, and is never by the proposer (except an operator's own presence check, KA1) or the grantee;
  under `human`, it is an operator's.
- **Confirmation and application (14 PR 6c).** `proposal.confirm {proposal, review}` confirms an open
  `independent_review` proposal with a review receipt the controller holds (looked up server side, never a client's
  GO). The confirmer's approval is built from the proposal record (`ApprovalOf`) and must be separate from it. In the
  same writer transaction the exact operation is re-derived for its proposer as of now and revalidated (object state,
  the proposal's own grant, proposer, policy, confirmer, the proposer's seat), then applied through its action's
  handler. One command commits `approval.recorded`, `proposal.confirmed`, the operation's events and
  `proposal.applied`; or `proposal.invalidated` with its registered reason (disposition invalidated); or, past its
  expiry, `proposal.expired` (refused). The operation's events carry the confirming command's actor; the proposal
  names the proposer. A `human` proposal, the KA1 presence check included, is refused (`confirmation_refused`) until
  the dashboard's WebAuthn is deployed (14 R6). Until review receipts are recorded, the daemon has no review source and
  refuses every confirmation.
- **Withdrawal (14 PR 6d).** `proposal.withdraw {proposal}` commits `proposal.withdrawn/v1` for an open, unexpired
  proposal, by its proposer or an operator, under a grant covering the action in the proposal's project. Anyone else is
  refused `action_not_granted` in the derivation, before authority decides, and again in the fold. A withdrawal is never
  itself proposed: a policy that would make it one refuses it (`action_not_proposable`). It drops the proposal from the
  pending indexes, so the same operation may be proposed again. At or past its expiry the proposal is closed as expired
  (refused). It changes no target, so it needs no seat.

## Protected changes: documents and topology [M3][M12]

Two narrow, separately registered actions. Both:
- bind the exact paths, the old and new digests, and the expected adopted revision;
- travel as a candidate through independent review and the merge queue. The approval authorises the accept stage to
  admit exactly those paths and digests and nothing else;
- record the new adopted revision on merge (`knowledge.adopted_revision`, `integration.adopted_topology`). The profile
  references topology by adopted revision **and digest**, never by path alone (07);
- **can never widen authority or data policy.** If the effective-change computation finds any widening in the new
  content, the action is refused, and the change must go through the dedicated widening action with its own
  confirmation;
- cannot approve their own adoption: the approval precedes and references the candidate, never sits inside it;
- **do not grant authority through prose.** Authority never derives from document text: ids named in prose are
  references, not grants. Authoritative identity references (which tasks, findings or grants exist and what they
  allow) change only through their typed actions. There is no lexical ban on naming ids in documents [PR5-7];
- the approval UI and CLI render the **exact bytes** being approved (old → new diff), so humans approve bytes, not
  intent.

`knowledge.change_protected_document` accepts only the profile's `protected_documents` set.
`integration.change_topology` accepts only the profile's topology files. Other protected paths (canon) change only by
an operator-approved profile revision listing the exact files.

## Enforcement statement (D31)

Supervisor limits hold only if the supervisor process cannot read an operator token or the bootstrap secret. The docs
and `adc doctor` state this. `adc doctor` warns when a supervisor token is used from an OS user that can read the
operator token file or the controller's state directory.
