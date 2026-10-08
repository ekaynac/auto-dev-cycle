# Schemas

The shared spec's records, by name. The JSON Schema 2020-12 files live in [`schemas/records/`](../../../schemas/records),
the single source of every shape in this repository; this page adds nothing to them.

| Record | Schema | Status |
|---|---|---|
| Ref | [`records/ref.json`](../../../schemas/records/ref.json) | M0 |
| ViewFingerprint | [`records/view-fingerprint.json`](../../../schemas/records/view-fingerprint.json) | M0 |
| ContextManifest intent | [`records/manifest-intent.json`](../../../schemas/records/manifest-intent.json) | M0 |
| ContextManifest delivery | [`records/manifest-delivery.json`](../../../schemas/records/manifest-delivery.json) | M0 |
| ContextManifest finalized | [`records/manifest-finalized.json`](../../../schemas/records/manifest-finalized.json) | M0 |
| AttemptRecord | [`records/attempt-record.json`](../../../schemas/records/attempt-record.json) | M0 (shared with 05) |
| FailureSignature | [`records/failure-signature.json`](../../../schemas/records/failure-signature.json) | draft; used from M2 |
| Claim | [`records/claim.json`](../../../schemas/records/claim.json) | draft; unused until K3 (after M2) |

The schemas refer to `schemas/common/defs.json`, `schemas/records/receipt.json` and `schemas/records/ref.json`;
vendor the whole closure at the same revision (see the [README](../README.md)). The digests these schemas define are
pinned by the golden vectors in [`../fixtures/golden.json`](../fixtures/golden.json). The two draft schemas have no
pinned digest and no golden vectors: their identity recipes are fixed with their golden vectors when first used.
