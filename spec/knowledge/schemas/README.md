# Schemas

The shared spec's records, by name. The JSON Schema 2020-12 files live in [`schemas/records/`](../../../schemas/records),
the single source of every shape in this repository; this page adds nothing to them.

| Record | Schema | Status |
|---|---|---|
| Ref | [`records/ref.json`](../../../schemas/records/ref.json) | M0 |
| ViewFingerprint | [`records/view-fingerprint.json`](../../../schemas/records/view-fingerprint.json) | M0 |
| ContextManifest intent | `records/manifest-intent.json` | M0 (in review) |
| ContextManifest delivery | `records/manifest-delivery.json` | M0 (in review) |
| ContextManifest finalized | `records/manifest-finalized.json` | M0 (in review) |
| AttemptRecord | — | M1 |
| FailureSignature | — | M2 |
| Claim | — | after M2 (K3) |

The digests these schemas define are pinned by the golden vectors in [`../fixtures/golden.json`](../fixtures/golden.json).
