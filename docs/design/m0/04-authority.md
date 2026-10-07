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
      `failed/actor_revoked`. The secret goes only to `FILE`, written as the token file is (R5): its directory is the
      user's, not group- or other-writable and not itself a symlink; a temporary file is created `O_EXCL|O_NOFOLLOW`
      0600, synced and renamed into place, so `FILE` is never followed. An existing `FILE` is refused (exit 64) before anything
      is recorded unless `--force-overwrite`. It confirms no proposal, resurrects no actor and leaves the bootstrap alone.
    - `adcd bootstrap renew --state-dir DIR [--resource-groups FILE] [--trusted-head SEQ:sha256:HEX]` (14 R8), for an
      expired, unused secret or a recorded one whose file is gone. While the bootstrap is unconsumed it writes a fresh
      secret every time (never the current one) through the held state-directory descriptor, replacing the old file,
      and records `identity.bootstrap_renewed/v1` with a new 24 h expiry; the old secret no longer bootstraps. After
      consumption it is `failed/confirmation_refused`. Startup never renews.
  - All logs redact tokens, secrets and challenge values by type, not by pattern guessing.
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
  approval: apr_…
```

A supervisor cannot create, extend or widen grants (`human`, and never by the grantee).

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
