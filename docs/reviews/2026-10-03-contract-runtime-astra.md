# Contract runtime (PR #17): Astra reviews

Reviews of the contract runtime PR. Each section is the review as returned; file links are relative to the repository root.

## Review of #17 (Astra, 2026-10-03)

Verdict: NO-GO

One High finding blocks specification conformance. `make ci` passed locally, including sandbox self-tests and distribution builds; shellcheck was unavailable and skipped. All 72 spike cases also passed against the production runtime. The checkout remains unchanged.

**High**

1. **Production schema IDs still describe incomplete spike contracts.**  
   Files: `schemas/records/receipt.json:90`, `schemas/adapter/event.json:6`, `schemas/records/approval.json:13`.

   Against notes 04, 06 and 09:
   - Receipts reject the documented `object_revision` and `environment` subjects.
   - Adapter events support only `turn.start`, `coverage`, `upstream` and `heartbeat`; they reject `turn.end`, `call.sub`, deltas, tools, usage, compaction and errors.
   - Approvals permit only `plan.approve` and `input.answer`, excluding documented actions such as `grant.create` and `install.self`.

   Independent probes confirmed these rejections. The descriptions acknowledge the subsets, but that does not make them conforming production contracts. **Fix:** complete the documented variants and action-specific scopes, with positive fixtures for each; alternatively, retain these as explicitly experimental schemas outside the authoritative contract IDs.

**Medium**

2. **Unicode can be silently changed at the contract boundary.**  
   Files: `internal/contract/contract.go:100`, `internal/contract/contract.go:134`, `internal/contract/contract.go:207`.

   The raw UTF-8 check correctly rejects invalid bytes, but ASCII JSON containing `"\ud800"` passes validation and decodes as U+FFFD. Separately, `Encode` accepts a Go string containing byte `ff` and emits U+FFFD. Both behaviors were reproduced. Validating after the encoder substitutes characters cannot detect the original invalid string.

   **Fix:** reject unpaired surrogate escapes before token decoding and reject invalid Go strings before encoding. Add negative cases alongside valid surrogate-pair and literal-U+FFFD controls. This matters to the package’s stated rationale of preserving content for JCS identity.

3. **Strict decoding does not extend automatically into custom unmarshallers.**  
   File: `internal/contract/contract.go:193`.

   `DisallowUnknownFields` governs the outer decoder only. A nested `UnmarshalJSON` using ordinary `json.Unmarshal` successfully discarded a schema-valid `expected_state.digest` in a review probe. The committed test checks only an ordinary top-level struct missing fields.

   **Fix:** provide a strict decoding helper and require generated custom/union decoders to use it, as the codegen report requires. Test nested custom decoding and qualify the generic API’s guarantee for arbitrary caller-supplied types.

4. **Non-empty failure detail is not enforced.**  
   File: `schemas/records/receipt.json:40`.

   A receipt with `outcome: "failed"` and `detail: ""` passes validation, contrary to note 02’s requirement that every non-`empty` result carry non-empty detail.

   **Fix:** conditionally require `detail.minLength: 1` for non-`empty` outcomes. Test all four outcomes, preserving empty detail for `empty`.

5. **Scalar validation still stops short of the documented identifiers and times.**  
   Files: `schemas/common/defs.json:24`, `schemas/common/defs.json:29`, `schemas/adapter/event.json:31`.

   Validation accepts `2026-99-99T99:99:99Z`, a `cal_…` identifier as an approval ID, and `"banana"` as `reported_at`. The common definitions explicitly defer calendar, UUIDv7 and entity-prefix checks, but this runtime does not perform those deferred checks.

   **Fix:** connect semantic scalar validation to the production boundary, enforce entity-specific prefixes, and give `reported_at` the intended timestamp constraint. Add calendar, UUID-version, prefix and fractional-second cases.

**Low**

6. **Digest tests establish repeatability, but not coverage or the specified construction.**  
   Files: `schemas/schemas.go:19`, `schemas/schemas_test.go:12`.

   The current construction is deterministic and unambiguous: sorted records of `path + NUL + lowercase hex SHA256(file bytes) + LF`, hashed together. An independent calculation matched the binary’s digest. However, returning any constant well-formed digest would pass the existing digest test. Discovery also covers only one-level JSON files, whereas note 01 anticipates recursive JSON/YAML contracts.

   **Fix:** document the exact framing, test an independently constructed expected digest, and assert the supported file inventory. Expand discovery when broader schema paths become supported.

The committed tests exercise trailing documents, decimal/exponent spellings, `-0`, duplicate keys at the root, nested objects and array elements, invalid UTF-8 bytes, and output validation. They do not cover the Unicode/custom-decoder gaps above or establish full schema conformance.

The cleanup change at `scripts/test-sandbox-selftest.sh:62` passed review for the current Linux/GNU implementation: recursive `chmod` and removal did not follow nested file or directory symlinks. External decoy contents and permissions remained unchanged, including with a dangling symlink present.

No `docs/notes` directory exists; the codegen-choice evidence reviewed was `docs/research/2026-10-03-codegen-spike/README.md`.

## Second review of #17 (Astra, 2026-10-03)

Verdict: GO with changes

No Critical or High findings remain within the revised scope.

- **#1 High — Resolved by scope:** all four incomplete record schemas were removed; completing them belongs to their separate PRs.
- **#2 Medium — Partially resolved:** surrogate rejection and ordinary string/map-key UTF-8 checks work. The encode guarantee still has gaps described below.
- **#3 Medium — Resolved:** `UnmarshalStrict`, the qualified documentation, and the strict-versus-lax custom-decoder test address the finding.
- **#4 Medium — Removed from scope:** receipt detail enforcement remains a requirement for the future receipt schema.
- **#5 Medium — Resolved within scope:** calendar validity, entity prefixes, UUIDv7 version and variant are enforced through referenced definitions. `reported_at` moves with the event schema.
- **#6 Low — Resolved:** recursive JSON discovery, documented digest framing, an independent expected digest, and exact embedded inventory now have tests.

**New findings — Medium**

1. **The UTF-8 walk misses strings that the JSON encoder emits.**  
   `internal/contract/contract.go:380`

   `IsExported()` skips unexported anonymous structs entirely, although `encoding/json` emits their promoted exported fields. A probe embedding `reviewEmbedded struct { Note string }` with `Note: "\xff"` returned successful JSON containing U+FFFD. This reproduces the original identity-loss problem using default encoding rules.

   Separately, a custom `MarshalText` returning invalid UTF-8 also succeeds after replacement; checking the final JSON cannot detect that substitution.

   **Fix:** follow JSON’s anonymous-field promotion rules. Define and enforce the custom-marshaler boundary: validate text before substitution, or explicitly constrain/document supported custom marshalers. Add regression cases alongside the existing Unicode controls.

2. **The walk follows ignored fields and can recurse until process termination.**  
   `internal/contract/contract.go:357`, `internal/contract/contract.go:377–380`

   The assumption that successful marshaling has excluded cycles is incorrect. An exported `Parent *node` tagged `json:"-"` can point back to its containing node: JSON encoding succeeds, then `validStrings` follows the ignored pointer indefinitely. An isolated probe reproduced a fatal stack overflow with a reduced stack limit. An ignored field containing invalid UTF-8 also incorrectly rejects otherwise valid output.

   **Fix:** honor JSON field selection, including ignored fields, and make traversal cycle-safe. Account for custom marshalers that successfully encode cyclic backing structures. Test ignored invalid strings and ignored/self-referential pointers.

**Verification and test adequacy**

`make ci` passed locally, including sandbox self-tests and distribution builds. Shellcheck was unavailable and explicitly skipped.

I independently reproduced all five mutation failures: disabling surrogate rejection, format assertion, UUID version enforcement, UUID variant enforcement, or the encode UTF-8 walk makes existing tests fail. These establish that the checks are exercised; they do **not** establish the package comment’s full encode guarantee, as the additional probes demonstrate.

The checkout remains unchanged.
## Third review of #17 (Astra, 2026-10-03)

Verdict: GO

- **Second-review Medium #1 — Resolved:** invalid UTF-8 is rejected in promoted fields, `MarshalText` output (including map keys), `MarshalJSON` output and `RawMessage`. Valid Unicode survives unchanged.
- **Second-review Medium #2 — Resolved:** ignored invalid strings and ignored self-references encode successfully. Custom marshalers can safely encode cyclic backing structures; ordinary pointer, map and slice cycles return errors.

**New findings:** None.

Independent sandboxed probes confirmed byte-for-byte compatibility with the previous v1 encoder for contract documents and representative types: field order, `omitempty`, nil/empty slices and maps, `RawMessage`, sorted map keys, HTML characters and no trailing newline. Unpaired surrogate output remains rejected.

`make ci` passed at `3744c51`, including sandbox self-tests and distribution builds. Shellcheck was unavailable and explicitly skipped locally.

Changing only `AllowInvalidUTF8(false)` to `true` in a scratch copy made both committed encode tests fail as expected. The checkout remains unchanged.