# Shared knowledge spec (draft)

**Version:** 0.2.0-draft.

This is the semantic spec for knowledge that auto-dev-cycle shares with InfraMedic (K10): what the terms mean, what
must always hold, the record schemas, and fixtures both implementations must pass. It has one authoritative home:
this directory, in the auto-dev-cycle repository.

## What is in it

- [`vocabulary.md`](vocabulary.md): information classes, claim kinds, lifecycle dimensions, ref types, identity
  versus content digests, and restriction semantics.
- [`invariants.md`](invariants.md): the rules every implementation keeps.
- [`schemas/`](schemas/README.md): the record schemas. The JSON Schema files themselves live in
  [`schemas/records/`](../../schemas/records), the single source of every shape in this repository; this spec names
  them and adds nothing to them.
- [`fixtures/`](fixtures/README.md): fixture **definitions** (inputs and the expected outcome) and golden digest
  vectors. Each definition names the milestone that implements its check.

## Rules for consumers

- InfraMedic vendors a **pinned revision** of this directory **together with its schema dependency closure** at the
  same revision: `schemas/common/defs.json`, `schemas/records/receipt.json`, `schemas/records/ref.json`,
  `schemas/records/view-fingerprint.json`, `schemas/records/manifest-*.json`, `schemas/records/attempt-record.json`,
  `schemas/records/failure-signature.json` and `schemas/records/claim.json`, keeping the repository layout
  (`spec/knowledge/` and `schemas/` side by side) so every relative `$ref` and link resolves. It maps onto them in
  its own repository.
- The spec carries its own semantic version. A breaking change to a term, an invariant or a schema raises the major
  version; a new fixture or a clarification raises the minor or patch version.
- **A spec or fixture update never changes grants, accepted claims or policy by itself** (KA8). Adopting a new
  revision is an ordinary, reviewed change in the consuming repository.

## Status

Draft for M0: schemas, invariants and fixture definitions only. The checks behind each fixture arrive in the
milestone it names (M1, M2 or M4). Every record the spec names has a schema. `FailureSignature` (used from M2) and
`Claim` (unused until K3, after M2) are **draft** schemas: no code writes or reads them yet, they have no golden
vectors, and their shape may change before first use.
