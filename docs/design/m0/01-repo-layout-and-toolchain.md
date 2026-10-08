# 01 — Repository layout and toolchain

## Layout

```
auto-dev-cycle/
├── cmd/
│   ├── adcd/             controller daemon
│   ├── adc/              CLI (client of the controller API; also local dev subcommands)
│   └── adc-agent/        dial-out capacity agent (M3; M0 has only the binary skeleton)
├── internal/             Go, organised by domain, not by layer
│   ├── vocab/            outcome, reason registry, hold reasons (generated + hand-written helpers)
│   ├── eventstore/       append, read, projections, hash chain, migrations
│   ├── authority/        actors, tokens, grants, approvals, proposals, confirmation
│   ├── capacity/         sources, resource groups, leases, scheduler (M2), gateway (M1+)
│   ├── attempt/          attempts, fencing, checkpoints (AttemptRecord)
│   ├── receipt/          receipt envelope, predicate evaluation, applicability
│   ├── project/          profile loading and validation
│   ├── knowledge/        ContextManifest, refs, fingerprints, view assembly (M1+)
│   ├── cycle/            stage engine (M1)
│   ├── api/              controller HTTP API (handlers generated from the OpenAPI document)
│   ├── adapterhost/      spawns and speaks to executor adapters (JSON-RPC over stdio)
│   └── testguard/        in-process test layer (temp roots, fake clock) and fake registry; the boundary is the test sandbox (11)
├── schemas/              THE source of every shape (JSON Schema 2020-12) + openapi.yaml + vocab registries
│   ├── vocab/            outcomes.yaml, reasons.yaml, holds.yaml
│   ├── records/          event payloads, receipts, approvals, leases, attempts, manifests, …
│   ├── config/           project profile, usage profiles, source registry (reserved)
│   ├── adapter/          JSON-RPC method params/results/events
│   └── openapi.yaml      controller API
├── gen/                  generated code (committed; CI checks it is current)
│   ├── go/               Go types + validators
│   └── ts/               TypeScript types + validators (published as @adc/contract)
├── adapters/             pnpm workspace (TypeScript)
│   ├── contract/         @adc/contract: generated types, JSON-RPC framing, adapter SDK
│   ├── fake/             contract-faithful fake adapter (used by core tests)
│   ├── claude-code/      (M1)  codex/ opencode/ omp/ http-chat/ systemone/  (M1–M2)
│   └── conformance/      conformance kit runner
├── spec/knowledge/       shared semantic spec with InfraMedic (K10): schemas, invariants, fixture definitions
├── fixtures/t0/          T0 repos and fault fixtures (11)
├── docs/                 handoff, plan, design, reference (generated), adapters/AUTHORING.md, operating/
├── Makefile              the only entry point for humans and CI
└── LICENSE               Apache-2.0
```

Rules:
- `internal/` packages may not import a sibling's internals except through its exported API. One package owns each
  decision: authority decides authority, receipt decides receipt validity, and so on. A lint check forbids decision
  literals (e.g. reason codes as raw strings) outside `internal/vocab`.
- No `pkg/` until an external consumer exists. Everything is `internal/`.

## Languages and versions

| Part | Language | Version | Why |
|---|---|---|---|
| Core, CLI, agent | Go | 1.25+ (current stable at implementation time) | D2; static binaries, cross-compilation for the agent |
| Adapters | TypeScript on Node | Node 22 LTS, pnpm 10 | D2; harness SDKs are TS. Bun is optional later, not required |
| Schemas | JSON Schema 2020-12, OpenAPI 3.1 | — | Language-neutral; the shared spec with InfraMedic uses the same |

The Go toolchain is installed per user from the official tarball (`~/.local/go`), pinned in `go.mod` via the
`toolchain` directive. No system packages are required.

## Code generation (one source per contract)

- `schemas/**/*.json|yaml` → Go types and validators (`gen/go`) and TypeScript types and validators (`gen/ts`).
  Chosen by the spike on receipt, approval, grant and adapter event (`docs/research/2026-10-03-codegen-spike/`):
  72 shared cases on both validators, adequacy checked by global keyword deletions, TS type assertions.
  - **Go validation:** `santhosh-tekuri/jsonschema/v6` over the **embedded** schema files.
  - **Go types:** a small in-repo generator for exactly the grammar in the spike report: strings and string enums,
    integers **bounded within ±(2^53−1)**, booleans, closed objects, nullable `oneOf [null, X]` (**always
    required**: null is explicit, absence invalid, also through `$ref` aliases), optional fields as pointers (an
    optional array or map too, so absence and an empty value stay distinct), `const`-discriminated `oneOf` as a
    sealed interface, keyed `oneOf` (each branch exactly one required property, nothing embedded), array-or-literal
    `oneOf`, typed maps (`propertyNames` a string schema), open payloads (`x-adc-open-payload`) as raw JSON with typed
    accessors for each selectable schema, `allOf` envelope embedding. Two validation-only forms keep the Go shape and
    are accepted: a `$ref` to a plain string schema refined by `pattern`, `minLength`, `maxLength` or `format` (the
    entity-specific ids), and `if`/`then`/`else` conditions, on an object or in its `allOf`, that only constrain
    properties the object declares with `const`, `enum`, `not`, lengths, `pattern` or bounds (e.g. "non-empty detail
    unless the outcome is `empty`"). Property names must be valid Go JSON tag names (no `,`, quotes or a lone `-`). A
    string with format `adc-reason` (the reason code) is generated as an opaque type, declared once in `$defs` as
    `ReasonCode` and reached by `$ref` only (inline or refined forms, other names and map keys are refused). No string
    can become one, null is never one, and it is read (registered) and written (registered, not deprecated) only
    through the vocabulary's check, however it is marshalled. That check is installed once, and only by the
    vocabulary package (its exact import path); any other caller is refused.
    **Any other shape is refused** at generation time, so nothing degrades to
    `interface{}`. Both third-party Go generators tried fail this.
  - **Decoding and encoding:** `Decode` validates the bytes, then decodes the same bytes strictly (unknown fields
    and trailing data rejected at every level; non-integral integer spellings refused); `Encode` validates its
    output. Types never stand in for validation.
  - **TypeScript:** `json-schema-to-typescript` types and Ajv 2020 standalone validators (CommonJS; Ajv is a
    runtime dependency of `@adc/contract`), compiled with `strict: true`, which doubles as the schema lint.
- `schemas/openapi.yaml` → Go server interfaces and the TS client used by the dashboard.
- Model structured-output schemas are derived from the same files (with a provider-dialect transform: e.g. OpenAI
  strict mode needs all fields required/nullable and no `oneOf`; nöbetçi lesson 2.2b). In M0 the transform is
  unit-tested offline. Live dialect probes against real providers start in M1/M2 as a separately authorised job, never
  inside `make ci` (11).
- Generated code is committed. `make gen` regenerates; CI runs `make gen` and fails on any diff.

## Build and CI

- `make ci` = `make lint gen-check test contract-fixtures dist install-check`. `make test` runs Go and TS tests
  **inside the test sandbox** (bubblewrap, or a rootless container as fallback; 11). It refuses to run unsandboxed.
- CI runs on GitHub Actions for this repository (the framework's own CI; projects using auto-dev-cycle may be on any
  forge).
- Release builds: `adcd`, `adc` and `adc-harness` (15 S1) for linux/amd64 and linux/arm64; `adc-agent` additionally for
  darwin and windows. Release builds are no longer Go binaries only (15 S2, D6): for the two linux platforms,
  `lib/adc` also carries the official Node 22 `bin/node` (pinned in `scripts/node-pins` from Node's signed
  `SHASUMS256.txt.asc`, verified at `make dist` and re-verified on the installed file), the fake adapter with its
  production dependencies (licences on an allowlist), `fixtures/t0/py-mini` (committed content, by `git archive HEAD`), the repository's
  `LICENSE` and `NOTICE`, and a `SHA256SUMS` that install-check verifies. No npm or corepack is packaged.
- Every build embeds version, commit and schema-set digest; `adc version` and the API report them, and every event
  records the writer's schema-set digest.

## Licence and headers

Apache-2.0 `LICENSE` and `NOTICE` at the root. SPDX headers (`// SPDX-License-Identifier: Apache-2.0`) in source files.
