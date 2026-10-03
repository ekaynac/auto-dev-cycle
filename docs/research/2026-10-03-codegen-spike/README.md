# Code-generator spike: three schemas, one choice

Date: 2026-10-03. M0 step 2 (`docs/plan/04-development-workflow.md` §9). Implements the spike that
`docs/design/m0/01-repo-layout-and-toolchain.md` "Code generation" asks for.

## Question

Which tools turn `schemas/` into Go and TypeScript types and validators **faithfully**? Note 01 sets the criterion:
`oneOf`, `const`, `enum` and `additionalProperties: false` must be handled faithfully, on three representative schemas
(receipt, approval, adapter event).

## Inputs

`schemas/` in this directory holds the spike schemas: draft 2020-12, `$id` under the module path (Q1),
cross-file `$ref` into `common/defs.json`. They were chosen to exercise every hard case:

| Case | Where |
|---|---|
| discriminated `oneOf` by a `const` property | receipt `subject` (tree / proposal / call); adapter event (`event`) |
| nullable `oneOf [null, X]` | receipt `demand`, `valid_until`, `verdict`; approval `project`, `grant`, … |
| keyed `oneOf` (exactly one of two property sets) | approval `expected_state` (`version` xor `digest`) |
| `allOf` composition + `unevaluatedProperties: false` | adapter event: a shared envelope + per-event fields |
| `enum`, `pattern`, `minimum`, `uniqueItems` | outcomes, digests, TypeIDs, times |

`cases.json` holds 16 instances (6 valid, 10 invalid) that each harness must classify identically.

## Results

| Tool | Side | Result |
|---|---|---|
| `santhosh-tekuri/jsonschema/v6` v6.0.3 (Apache-2.0) | Go validation | **16/16.** Full 2020-12 including `unevaluatedProperties`, cross-file `$ref` via `AddResource` |
| Ajv 8.20.0, 2020 dialect, standalone code (MIT) | TS validation | **16/16.** Standalone output must be CommonJS: the ESM output still `require()`s a runtime helper (`ucs2length`) |
| `json-schema-to-typescript` 16.0.0 (MIT) | TS types | **Faithful.** Discriminated unions with literal `const` types, nullable unions, enum literal unions, `allOf` as intersections |
| `atombender/go-jsonschema` (MIT) | Go types | **Fails.** Every `oneOf`, nullable or discriminated, and every `const` becomes `interface{}` |
| quicktype 26.0.0 | Go types | **Fails.** It merges each union into one struct where every variant field is optional (a tree subject can carry proposal fields), and **drops the `allOf` envelope fields** (`attempt`, `fencing_token`, `call`) |

Side finding: Ajv's `strict` mode caught a real schema defect in the first draft of the event schema: object
keywords without `"type": "object"`, and a `required` name defined only in the branches. Strict compilation is
worth keeping as a schema lint.

## Choice

- **TypeScript:** `json-schema-to-typescript` for types, Ajv 2020 standalone validators (CommonJS) for validation,
  compiled with `strict: true`, which also serves as the schema lint in `make gen-check`.
- **Go validation:** `santhosh-tekuri/jsonschema/v6`, compiled from the **embedded** schema files (`go:embed`), so
  the binary validates against exactly the bytes its schema-set digest names.
- **Go types:** a **small in-repo generator** (`gen/` tooling, as note 01 anticipated). No maintained Go generator
  handles `oneOf` and `const` faithfully. It supports exactly the shapes our schemas use and **refuses any other shape
  at generation time**, so a schema cannot silently degrade to `interface{}`:
  - object with `properties`, `required`, `additionalProperties: false` → struct; optional → pointer or `omitempty`;
  - string `enum` → named string type plus constants;
  - nullable `oneOf [null, X]` → `*X`;
  - discriminated `oneOf` (every branch an object with the same `const` property) → sealed interface plus one
    struct per branch, with an `UnmarshalJSON` that dispatches on the discriminator;
  - keyed `oneOf` (branches with disjoint single `required` keys) → struct with pointer fields; the validator enforces
    "exactly one";
  - `allOf` of a `$ref` envelope plus local properties → embedded struct;
  - `$ref` → named type; arrays → slices.
- **Decoding rule:** generated code never decodes unvalidated bytes. `Decode` validates against the schema first
  (santhosh), then decodes strictly (`DisallowUnknownFields`). The schema is the single source of truth, and the Go
  types are an ergonomic view of it.

## Rejected alternatives

- **Go types from a third-party generator plus runtime validation:** both candidates silently weaken the contract
  in the types. Callers would build invalid values that compile.
- **Go structs as the source, schemas generated from them** (e.g. `google/jsonschema-go`): that inverts "one source
  per contract". TypeScript and the shared knowledge spec (K10) need the schema as the source.
- **quicktype for both languages:** unfaithful on unions (above).

## Reproduce

```sh
cd ts && pnpm add json-schema-to-typescript@16.0.0 ajv@8.20.0 && node spike.mjs      # 16 × ok
cd ../go && go mod init spike && go get github.com/santhosh-tekuri/jsonschema/v6@v6.0.3 && go run spike.go  # 16 × ok
```

The spike schemas are evidence. The real `schemas/` tree starts in the generator PR and grows from these three.
