# Vocabulary

Terms of the shared knowledge spec. Where a term has a schema, the schema is normative and this page explains it.

## Information classes

Every piece of information belongs to exactly one class. The class decides who produces it, who may read it and how
long it lives.

| Class | Contents | Produced by | Read by |
|---|---|---|---|
| **Authoritative state** | receipts, approvals, attempts, findings, plan revisions, decisions | the controller | everyone, as regenerated status |
| **Regenerated view** | dossier, code index, digests, plan sidecar, canon excerpts | deterministic code from the tree and receipts | per-role assembly; regenerated per call, never stored as knowledge |
| **Ephemeral attempt continuity** | partial diff, failing tests, trail, untrusted builder notes, sealed at every builder exit | the attempt (notes) and the controller (seal) | builder continuations of the same project, task and contract revision only; never reviewers |
| **Supervisor continuity** | notes, open threads, handovers | the supervisor | the supervisor seat only, within its grant and projects; untrusted; never cycle roles |
| **Durable knowledge** | accepted decisions, conventions, verified lessons, scoped negative results | proposals from any role or code, accepted by policy | role views, by purpose |

## Claim kinds

Each kind has **one home**: lower layers refer to it by id and never restate it.

| Kind | Example | Home |
|---|---|---|
| observation | "test X failed on tree T" | a receipt |
| external fact | "library L ≥ 2.0 drops API A" | a `Claim` with a source pointer |
| inference or lesson | "this approach fails because …" | a `Claim` |
| decision or convention | an ADR, a naming rule | a protected repository file; the controller holds only the adoption record of a revision |
| procedure or mechanism | a test, a validator, a gate | code |

Competing interpretations are separate claims with an explicit dispute: one home does not mean one opinion.

## Lifecycle dimensions

Three independent dimensions. None of them grants authority.

- **Decision:** `unassessed`, `accepted`, `rejected` or `disputed`, each for one purpose. The state and its purpose
  live in a decision record that references the claim revision, never in the claim; with no decision record for a
  purpose, the claim is `unassessed` for it. Supersession is an explicit link.
- **Validity:** `current`, `stale`, `unknown` or `expired`, each with a reason. A claim declares the dependencies
  its validity rests on; a change to one makes it `stale` or `unknown` until revalidated (K6).
- **Retention:** `available`, `held` or `purged`; a purge leaves a tombstone.

## Refs

A **ref** ([`records/ref.json`](../../schemas/records/ref.json)) names one input exactly:

| Field | Meaning |
|---|---|
| `type` | `blob`, `git_tree`, `git_path`, `receipt`, `finding`, `approval`, `contract`, `claim`, `view` or `attempt_record` |
| `id` | a digest, a git id and path, or a controller id, by type |
| `revision` | e.g. an adopted contract revision; null when the type has none |
| `scope` | the project it belongs to (or null), and whether it is framework-scoped |
| `lineage` | the ids of the refs it was derived from, recorded by the controller |
| `restrictions` | the source restrictions it inherited through lineage |
| `content_digest` | of the bytes |
| `identity_digest` | of everything above |

### Identity versus content digests

- The **content digest** names the bytes.
- The **identity digest** names the ref: it covers the type, id, revision, scope, lineage, restrictions and content
  digest. Two refs with identical bytes but a different history have the same content digest and different
  identities.
- The identity preimage is the RFC 8785 (JCS) encoding of the ref in **canonical form** without its
  `identity_digest`. In canonical form every set (lineage, restrictions, each restriction's destinations) is sorted
  (strings by Unicode code point; restrictions by project, policy digest, floor, routing, then destinations), has no
  duplicates, and an empty set is `[]`. The digest is `sha256:` followed by the lowercase hex SHA-256 of the preimage.
- A **view fingerprint** ([`records/view-fingerprint.json`](../../schemas/records/view-fingerprint.json)) is the
  digest of `{"renderer": …, "inputs": [identity digests, in order]}`: identities, never content digests.

### Restriction semantics

A **restriction** records the data policy of one contributing project when the ref was derived from it: the
project, a digest of its adopted data policy, its floor (`own`, `contracted`, `free_unknown`), its routing
(`verified`, `reported_ok`) and its resolved allowed destinations.

- Every distinct snapshot is kept, several per project when the inputs came under different policy revisions.
- All restrictions apply together: a delivery must satisfy each one.
- An empty destination set means **no destination** may receive the ref.
- Restrictions record lineage. They never replace the live checks at each delivery (initial, lookup, fallback) of
  the receiving project's policy, of revocation and of export approval.

## ContextManifest

Three append-only events on one manifest id, for every model call:

1. **Intent** ([`records/manifest-intent.json`](../../schemas/records/manifest-intent.json)), committed before
   egress: the consumer, the inputs, the policy, what is included (in order) and what is omitted (with a reason),
   the budget, the rendered digest, the destination and the routing.
2. **Delivery** ([`records/manifest-delivery.json`](../../schemas/records/manifest-delivery.json)), one per turn:
   controller-mediated tool results with their handles, adapter declarations, compaction.
3. **Finalized** ([`records/manifest-finalized.json`](../../schemas/records/manifest-finalized.json)), on call end:
   the coverage and the delivery outcome.

**Coverage levels:** *assembled* (what the controller built), *delivered* (the initial prompt plus every
controller-mediated tool result, per turn) and *unobservable* (harness instructions, compaction, harness-internal
tools).

**Coverage derivation** (normative; the same rules are in
[`records/manifest-finalized.json`](../../schemas/records/manifest-finalized.json)):

1. `prompt` is the controller's own evidence of the initial prompt's egress: `delivered`, `not_sent` (refused or
   failed before egress: no turns, no delivery events) or `uncertain`.
2. The **delivered refs** are the included inputs when the prompt was delivered, plus every tool result of every
   delivery event. The **possibly delivered refs** are the included inputs of an uncertain prompt that no delivery
   event confirms. The two never overlap.
3. Every compaction mark of a delivery event is listed as unobservable for its turn, and every adapter
   declaration as declared-only for its turn: a declaration is never evidence. Either list may name more.
4. When the number of turns is known, every turn named anywhere lies in 1..turns.
5. The **status** is `unknown` when the number of turns is unknown or the prompt is uncertain; otherwise `complete`
   only when every turn 1..turns has exactly one delivery event and both lists are empty; otherwise `partial`.

A delivered tool result is a declared input of the intent, or a ref derived only from declared inputs: its lineage
is non-empty, names only declared input ids, and it keeps every restriction of every declared input each lineage
id names.

## Other records

- `AttemptRecord` ([`records/attempt-record.json`](../../schemas/records/attempt-record.json)): the schema of the
  existing checkpoint, bound to project, task, contract revision, fencing token and base tree.
- `FailureSignature` ([`records/failure-signature.json`](../../schemas/records/failure-signature.json), draft, used
  from M2): a derived, project-scoped aggregate (normalized failure and count, no prose).
- `Claim` ([`records/claim.json`](../../schemas/records/claim.json), draft, unused until K3): a revision of a typed
  claim with evidence refs, applicability, validity conditions, producer and inherited restrictions.
