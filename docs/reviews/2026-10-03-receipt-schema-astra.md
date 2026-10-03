# Receipt schema (PR #18): Astra reviews

Reviews of the receipt and instrument-contract schema PR. Each section is the review as returned; file links are relative to the repository root.

## Review of #18 (Astra, 2026-10-03)

Verdict: GO with changes

No Critical or High findings.

1. **Medium — Predicate outcomes leave the 02/06 detail conflict unresolved.** [schemas/records/receipt.json:71](schemas/records/receipt.json). Note 02 requires non-empty detail for every non-`empty` result, but predicates cannot carry `detail`. Verified: a failed predicate without detail validates; adding detail fails `additionalProperties`. Original note 06 already omitted this field, so this is an unresolved specification gap rather than a newly changed decision. **Fix:** add predicate detail and its conditional requirement, amend note 06, and test all four predicate outcomes. `internal/receipt` cannot supply a field the schema forbids.

2. **Medium — Tests detect removal of the conditions but do not prove all outcomes remain representable.** [schemas/records_test.go:101](schemas/records_test.go). `failed` and `could_not_run` appear only in invalid receipts without detail. I restricted receipt outcomes to `empty|found`; the entire schema test package still passed. **Fix:** add valid cases for both missing outcomes, with detail, including review `no_go`; test `go` rejection for each non-empty outcome independently of the detail condition.

3. **Low — The nullable payload-schema amendment needs explicit semantics.** [docs/design/m0/06-receipts.md:22](docs/design/m0/06-receipts.md). Replacing `schema_ref` with a blob digest is a reasonable representation choice. Adding `null` leaves unspecified whether payloads are forbidden or simply unvalidated, while rule 7 requires typed facts validated against an instrument schema. **Fix:** define `null` as permitting no instrument-specific payload, and require `internal/receipt` to enforce that relationship. Document payload optionality explicitly. Allowing arbitrary unvalidated payloads would change the original intent.

4. **Low — Additional lexical restrictions are absent from the design-note amendments.** [schemas/records/receipt.json:55](schemas/records/receipt.json). The shared `Name` pattern rejects strings such as `GoTest`, although note 06 specifies instrument and predicate names as strings. Media types also acquire lowercase/no-parameter restrictions. These may be sensible conventions, but “exactly, no more and no less” is not yet documented. **Fix:** specify these conventions in the notes or relax the constraints; add representative boundary cases.

Otherwise, the envelope fields, eight kinds, five mutually exclusive subjects, demand bindings, entity-specific IDs and three outer conditional rules match amended note 06. The prefix additions, command identity, artifact name and conclusions digest are justified gap-fills. Named preconditions preserve the declared-check intent. No D# change was identified.

Completeness, referenced payload validation, evidence existence, applicability, independence and derivation correctly remain with `internal/receipt`. Required/optional name overlap also belongs there; it requires comparing instance values.

Validation: `make ci` passed locally, with shellcheck skipped because unavailable. Independently deleting each of the three conditional rules caused the expected tests to fail. Mutation checks used temporary copies; the checkout remains unchanged.