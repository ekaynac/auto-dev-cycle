# Legacy incident goldens

Real incident artifacts from auto-dev-cycle's predecessor (codename nöbetçi), copied once, read-only, from its
`t/fixtures/` and anonymised. They are **data only**: no code, no tests. Each one records a failure that happened in
unattended operation, and says what auto-dev-cycle must yield for the same input
(`docs/design/m0/11-testing.md`, "Legacy incident goldens", intake item A11 in
`docs/research/2026-10-03-nobetci-delta-intake.md`).

`index.md` maps every row of the predecessor's incident→test index to the auto-dev-cycle fixture that will reproduce
it, or to the reason it does not apply.

## Contents

| Directory | Class | Artifacts |
|---|---|---|
| `f16-inline-fallback/` | primary reviewer out of credit; a fallback outside its role and mode takes the review packet | the run's budget, session, packet header, both usage records, the primary's rollout events |
| `f16-walk-demand/` | a reviewer's new walk demand with otherwise unchanged observations | two review packets (trimmed, see below) and three walk receipts |
| `edge-owned-errors/` | candidate-owned setup errors on a target copy without `.git` | the full pytest log of the edge gate |
| `closure-record-finding-r4/` | a closing-record finding routed to the builder | the review's findings file |

Not imported, with the reason:
- `obligation-inactive-golden.json`: a digest set for the obligation gate, which is not ported (plan §5).
- `evidence-profile-demo.json`: a demo configuration, not an incident; its successor is the project profile (note 07).
- `retro-roadmap-rows.md`: product roadmap rows. The class it served (row mapping at closure) has no design-note
  fixture yet (see `index.md`, row B20), and the content is product material.

## How to read `golden.json`

```json
{
  "source": "path in the predecessor's repository, relative to its root (anonymised)",
  "class": "the failure class, one line",
  "design_refs": ["docs/design/m0/…"],
  "expected": [{"outcome": "found|empty|could_not_run|failed", "reason": "snake_case"}],
  "reason_status": "registered|proposed",
  "notes": "which artifact each step applies to, and what is uncertain"
}
```

- `expected` is the ordered `(outcome, reason)` sequence that **auto-dev-cycle** must produce for these artifacts,
  derived from the design notes. It is not what the predecessor did: the artifacts record its failure.
- Outcomes are the four of `docs/design/m0/02-vocabulary.md`. A refusal (a command or placement check that is refused)
  is written as `failed` with its reason, as in note 03 (`failed/stale_expected_state`).
- `reason_status` is `registered` only if every reason in `expected` is named in note 02. Otherwise it is `proposed`:
  the vocabulary PR registers or renames those codes, and the goldens follow. `notes` says which steps are registered.
- A loader must first verify its own setup: the artifacts it reads have the shape the golden describes (the
  predecessor's rule "a test proves its own setup").

## Anonymisation rules applied

Applied by a one-shot script; the mapping itself is not kept.

- **Digests and commit ids** → clearly synthetic hex: `5eed` + a 4-digit counter, zero-padded to the original length.
  The same original maps to the same value everywhere, and a short prefix of a known digest maps to the prefix of its
  synthetic value, so equality and inequality between artifacts survive (e.g. the walk demanded by a review carries
  that review packet's digest prefix).
- **UUIDs** (sessions, calls, turns, runs) → `00000000-0000-4000-8000-<counter>`.
- **Hosts and addresses** → `host-a`; the SSH login on the lab host → `user@host-a`; the browser login subject →
  `user-a`; directory reader identities → `reader-a`, `reader-b`.
- **Paths** → `/home/user/…`; the predecessor's runtime tree → `/home/user/runtime`; the product checkout →
  `/home/user/product`; the target's copy → `/home/user/verify/product`.
- **Product** name and package → `product` / `Product`; a product storage module → `keystore`.
- **Task, claim and test ids** → `TASK-0001`…`TASK-0003`, `CLAIM-012`; claim-numbered test names lose the claim
  number (`test_ob_…_010_x` → `test_x`, file `test_release_proof.py`).
- **Timestamps** and epoch-based run ids are kept unchanged. Model and provider names are kept (they are public and
  the subject of the fallback golden).
- No credential, key or cookie value appeared in the imported artifacts. The words `token`/`tokens` that
  remain are usage counters (`input_tokens`), and `cookies: 1` is a count.
- No Turkish text remained in the imported artifacts after trimming; none of these goldens is about non-English
  requirements.

### Trimmed packets (`f16-walk-demand/packet-*.json`)

The two review packets were ~80 KB each, almost all product claim text. They are reduced to the fields the review
identity and receipt applicability depend on: candidate `head`/`base`/`tree`, `input_digest`, task id, both evidence
jobs (identity, status, launch record, the full `walk` block; the lab inventory dropped), the receipt rules and their
`problem`, operator rulings (digest, `answers`, time only; the ruling text dropped), the browser-walk vocabulary
(identity descriptions replaced), the review digest's round, previous event, open claims (id/status/owner),
controller projection and repair range, and the review continuation's previous head/tree/receipt. `input_digest` is
the predecessor's digest of the **untrimmed** packet, kept as an identity, not recomputable from the file.
