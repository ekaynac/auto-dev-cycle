# Code-generator spike: four schemas, one choice

Date: 2026-10-03. M0 step 2 (`docs/plan/04-development-workflow.md` §9). Implements the spike that
`docs/design/m0/01-repo-layout-and-toolchain.md` "Code generation" asks for. Revision 2 applies Astra's review of
PR #9 (`docs/reviews/2026-10-03-pr9-astra.md`).

## Question

Which tools turn `schemas/` into Go and TypeScript types and validators **faithfully**? Note 01 sets the criterion:
`oneOf`, `const`, `enum` and `additionalProperties: false` must be handled faithfully, on representative schemas
(receipt, approval, adapter event). The grant was added to cover typed maps and an array-or-literal union, which M0
needs too.

## Inputs

`schemas/` in this directory: draft 2020-12, `$id` under the module path (Q1), cross-file `$ref` into
`common/defs.json`.

| Shape | Where |
|---|---|
| `const`-discriminated `oneOf` | receipt `subject` (tree / proposal / call); adapter event (`event`) |
| nullable `oneOf [null, X]`, always required | receipt `subject.demand`, `head`, `valid_until`, `verdict`; approval `project`, `grant`, … |
| keyed `oneOf` (exactly one of two property sets) | approval `expected_state` (`version` xor `digest`) |
| array-or-literal `oneOf` | grant `projects` (`"*"` or a non-empty list); `reads.projects` (`"same"` or a list) |
| typed map | grant `confirmation_overrides` (`propertyNames` + `additionalProperties` schema) |
| open payload, validated by a selected schema | approval `scope` (selected by `action` with `if/then` in the same schema); predicate `payload` (selected at run time by the instrument contract) |
| `allOf` envelope + `unevaluatedProperties: false` | adapter event |
| bounded integers, `enum`, `pattern`, `uniqueItems`, `minItems` | versions, outcomes, digests, TypeIDs, lists |

**These are spike subsets, not the full contracts.** Left out: the receipt's `object_revision` and `environment`
subjects; 09's other adapter event kinds; the action registry beyond two actions. The scalar definitions check lexical
form only, and each description names the check that belongs to code (reason registration, calendar validity, TypeID
version and prefix). Note 02 lists 3-letter prefixes, but note 05 uses `rg_`. The pattern accepts 2–3 letters
until the vocabulary PR settles it.

## Results

`cases.json`: 72 instances (27 valid, 45 invalid). They cover:
- every union variant;
- **both branches of every nullable field** (null and a valid value), plus an invalid value for each (receipt: `demand`, `head`, `valid_until`, `verdict`, `provenance.attempt`, `supersedes`; approval: `project`, `grant`, `confirmation.challenge`, `question`, `answer_verbatim`);
- the keyed union's neither and both forms;
- numeric bounds: the 2^53−1 boundary (at and above) for every adapter-event integer (`fencing_token`, each variant's `turn`) and for `expected_state.version`; other approval integers are checked above their maximum only through the global `maximum` deletion;
- duplicate items, scope dispatch, typed-map keys and values, and the array-or-literal forms.

**Adequacy is measured, not claimed, for global keyword deletions.** The Go harness recompiles the schemas with one
keyword removed everywhere, for each of 13 keywords (`additionalProperties`, `unevaluatedProperties`, `required`,
`const`, `enum`, `pattern`, `minimum`, `maximum`, `uniqueItems`, `minItems`, `propertyNames`, `allOf`, `oneOf`). Each
deletion flips at least one case. That detects the listed global deletions; per-occurrence coverage rests on the
case list above. "72/72" is about validation of these instances; it says nothing about keywords the schemas don't use.

| Tool (version) | Side | Result |
|---|---|---|
| `santhosh-tekuri/jsonschema/v6` v6.0.3 | Go validation | **72/72**, and all 13 global deletions detected |
| Ajv 8.20.0, 2020 dialect, standalone | TS validation | **72/72**. Strict compilation needs the `x-adc-open-payload` annotation registered |
| `json-schema-to-typescript` 16.0.0 + TypeScript 7.0.2 | TS types | **Faithful** under `types-check.ts`, 8 negative assertions plus narrowing (below) |
| `atombender/go-jsonschema` v0.24.1 | Go types | **Fails:** `type Subject interface{}`, `type Demand interface{}`, `Verdict interface{}`, `Head interface{}`, and the `const` discriminators `Type interface{}` |
| quicktype 26.0.0 | Go types | **Fails:** the event union becomes one struct with every variant field optional, and the `allOf` envelope (`attempt`, `fencing_token`, `call`) is missing entirely |

TS type assertions (`ts/types-check.ts`, `tsc` strict):
- exhaustive narrowing on `subject.type`;
- these must not compile: a field of another variant, an unknown discriminator, a missing envelope field, a partial
  demand, a scope without its required field, a literal other than `"*"`, an empty project list, and a weaker
  override value.

Making any of them valid makes `tsc` fail; that was checked by flipping one.

**TS limitations: accepted, and enforced by validation, not by types.** Three things compile in TypeScript although
Ajv rejects the documents. `types-check.ts` records each one as a probe that compiles on purpose, so a change in either
direction shows:
1. json-schema-to-typescript ignores `if/then`, so `Approval.scope` is typed as an open map. The per-action scope
   types are still emitted (`unreachableDefinitions: true`).
2. The keyed `expected_state` union does not exclude the other key (`{version, digest}` compiles).
3. Generated event types carry an open index signature, so another variant's field compiles.

TypeScript in this project only produces and consumes documents through the Ajv validators, so these gaps never reach
the wire. The same states are not excluded at compile time in Go either (both keyed pointers can be set; a scope `json.RawMessage` can hold anything): Go's guarantee is that `Decode` and `Encode` validate, so none of them is ever accepted or emitted. The difference from TypeScript is only that Go types are closed structs (no index signature) and offer typed scope accessors.

**Ajv side finding:** strict mode caught a real defect in the first event schema draft: object
keywords without `"type": "object"`, and a `required` name defined only in the branches. Strict compilation stays
on as a schema lint.

Reproduction commands (each tool, with the exact options) are under "Reproduce".

## Choice

The conclusion is limited to the candidates tested. Among them, **no Go type generator is faithful**, so:

- **TypeScript:** json-schema-to-typescript types (`unreachableDefinitions: true`); Ajv 2020 standalone validators,
  compiled `strict: true` with the generator annotations registered. Ajv is a **runtime dependency** of
  `@adc/contract`, because the CommonJS standalone output `require()`s Ajv runtime helpers. Step 2b smoke-tests the
  installed package from a consumer.
- **Go validation:** santhosh-tekuri/jsonschema v6 over the **embedded** schema files.
- **Go types:** a small in-repo generator that accepts **exactly** this grammar and refuses everything else at
  generation time:

  | Schema shape | Go representation |
  |---|---|
  | `string` (with `pattern`/`enum`/`format` as validation only) | `string`; a string `enum` → named type plus constants |
  | `integer` with `minimum` and `maximum` inside ±(2^53−1) | `int64`. **An integer without both bounds is refused**, so values stay exact in JavaScript |
  | `boolean` | `bool` |
  | `number` | refused in M0 (no float appears in any M0 contract) |
  | object, `additionalProperties: false` | struct; required → value; optional non-null → pointer with `omitempty` |
  | nullable `oneOf [null, X]` | `*X`. **The field must be required:** null means explicitly empty, absence is invalid. Optional-and-nullable is refused |
  | `const`-discriminated `oneOf` | sealed interface plus one struct per branch; decoding dispatches on the discriminator |
  | keyed `oneOf` (branches with disjoint single required keys) | struct with one pointer per key |
  | array-or-literal `oneOf [const, array]` | struct with a literal flag and a slice, plus custom encode/decode |
  | typed map (`additionalProperties: <schema>`, optional `propertyNames`) | `map[K]V` |
  | open payload (`"type": "object"` + `x-adc-open-payload`) | `json.RawMessage`, plus generated typed accessors for each selectable schema (e.g. per action) |
  | `allOf [$ref Envelope]` + local properties | embedded struct |
  | `$ref`, arrays | named type, slice |

- **Decoding and encoding contract** (implemented and round-trip tested in step 2b):
  1. `Decode` validates the bytes against the schema, then decodes **the same bytes** strictly. Every nested decoder,
     custom union decoders included, rejects unknown fields; trailing data after the document is rejected.
  2. Integers must be spelled as integers. `3.0` passes the schema but is refused by `Decode` as non-canonical, so
     Go and TypeScript agree on identity.
  3. `Encode` validates its own output before returning it. Constructed values (sealed interfaces, enum types, keyed
     pointer structs) can still be invalid in Go, so nothing reaches the wire unvalidated.
  4. Absent and null are never conflated, because of the "nullable implies required" rule above.

## Rejected alternatives

- **Third-party Go type generators plus runtime validation:** both candidates weaken the types, so callers would
  construct values that compile but are invalid. Validation would catch them late, at encode time.
- **Go structs as the source, schemas generated from them** (e.g. `google/jsonschema-go`): this inverts "one source
  per contract". TypeScript and the shared knowledge spec (K10) need the schema as the source.
- **quicktype for both languages:** unfaithful on unions and `allOf`.

## Reproduce

From a scratch copy of this directory (the commands create `package.json` and `go.mod`):

```sh
# TypeScript: types, validators, 72 cases, type assertions
cd ts && pnpm add json-schema-to-typescript@16.0.0 ajv@8.20.0 typescript@7.0.2 && node spike.mjs && npx tsc -p .
# Go: 72 cases + 13 global keyword deletions
cd ../go && go mod init spike && go get github.com/santhosh-tekuri/jsonschema/v6@v6.0.3 && go run spike.go
# Rejected Go generators
go install github.com/atombender/go-jsonschema@v0.24.1
go-jsonschema -p gen -t ../schemas/records/receipt.json -o /tmp/receipt.go    # grep 'interface{}'
npx quicktype@26.0.0 --src-lang schema --lang go --package gen --just-types-and-package \
  --additional-schema ../schemas/common/defs.json --src ../schemas/adapter/event.json   # no attempt/fencing_token/call
```

Transitive npm and Go dependencies are not locked here. The spike pins its direct tools, and the real pipeline in
step 2b gets lockfiles.

The spike schemas are evidence. The real `schemas/` tree starts in step 2b and grows from these four.
