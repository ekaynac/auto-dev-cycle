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

`schemas/vocab/actions.yaml` lists every mutating action kind. Authority does **not** trust the endpoint name. For
every command, it computes the **effective change**: the diff between the current and resulting policy-relevant state.
That state covers:
- data floor;
- routing requirement;
- allowed destinations;
- usage profile;
- protected paths and documents;
- paid-fallback flags;
- grants;
- enrolled agents;
- knowledge export.

It then classifies the command by the most sensitive change it contains. A profile adoption that widens destinations is
treated as `policy.widen_destinations`, whatever it was called.

Sensitive change classes and their supervisor confirmation defaults (D32, plus the knowledge actions from K5/KA1):

| Change class | Supervisor confirmation |
|---|---|
| `install.self` | `independent_review` |
| `policy.widen_data` · `policy.widen_destinations` · `policy.weaken_protection` (removes protected paths, loosens routing) | `human` |
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
  reason: string
  at: time
```

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
  binding_digest: sha256:…          # JCS digest of {action, change_class, project, object, expected_state, scope, proposed_by}
  status: open | confirmed | rejected | expired | superseded | invalidated
  expires_at: time

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
  requests a WebAuthn assertion whose challenge is `nonce ‖ binding_digest`. The server verifies, per the WebAuthn spec:
  - RP ID and origin;
  - challenge equality;
  - signature over authenticatorData + clientDataHash;
  - the user-presence flag (user verification required);
  - signature counter monotonicity;
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
- cannot approve their own adoption: the approval precedes and references the candidate, never sits inside it.

`knowledge.change_protected_document` accepts only the profile's `protected_documents` set.
`integration.change_topology` accepts only the profile's topology files. Other protected paths (canon) change only by
an operator-approved profile revision listing the exact files.

## Enforcement statement (D31)

Supervisor limits hold only if the supervisor process cannot read an operator token or the bootstrap secret. The docs
and `adc doctor` state this. `adc doctor` warns when a supervisor token is used from an OS user that can read the
operator token file or the controller's state directory.
