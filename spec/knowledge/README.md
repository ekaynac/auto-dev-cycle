# Shared knowledge spec (draft)

**Version:** 0.1.0-draft.

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

- InfraMedic vendors a **pinned revision** of this directory and maps onto it in its own repository.
- The spec carries its own semantic version. A breaking change to a term, an invariant or a schema raises the major
  version; a new fixture or a clarification raises the minor or patch version.
- **A spec or fixture update never changes grants, accepted claims or policy by itself** (KA8). Adopting a new
  revision is an ordinary, reviewed change in the consuming repository.

## Status

Draft for M0: schemas, invariants and fixture definitions only. The checks behind each fixture arrive in the
milestone it names (M1, M2 or M4). `AttemptRecord` (M1), `FailureSignature` (M2) and `Claim` (after M2, K3) are named
in the vocabulary but have no schema yet.
