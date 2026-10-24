# go-mini in M1: checkpoint (a)'s stub project

go-mini is the small Go project that checkpoint (a)'s CI stub cases run on. The operator decided this on 2026-10-20 (#226 entry 94). It is T0's go-mini, `fixtures/t0/go-mini`: the repository (`repo/`, module `example.com/gomini`, package `inventory`), its tasks with their committed hidden tests and reference changes, and its well-formedness check (`check.sh`, run by `make fixtures-check`). M1 adds only its profile and the records here.

| File | What it holds |
|---|---|
| `../../profiles/go-mini.json` | The project profile. It holds:<ul><li>the remote `/var/lib/adc/remotes/go-mini.git`, built from T0's `repo/`;</li><li>go-mini's own pinned prepared Go image (see below);</li><li>the gate `go test -count=1 -json ./...`, read with `go_test_json`, offline.</li></ul>The count rails go in once P17a (#295) adds them to the profile schema. |
| `baseline.json` | The base's record, machine-readable, as #170's: command, image, tree and counts. Its `acceptance` block covers the stub task `go-mini-feature-1`. It gives the digests of the hidden test and the reference change, which are committed in T0's task directory, and what each gate run gave. |
| `baseline-report.json` | The gate's measured baseline (`gate.Baseline`): `gate.Measure` in the pinned image, on T0's `repo/` tree. Five tests pass, with no skip. |
| `baseline-stream.json` | The measured run's `go test -json` stream. `gate.TestGateGoMini` judges it against the baseline. |

**Stub task (`go-mini-feature-1`).** `Store.Transfer`. It was checked in the pinned image, in gate mode, with its hidden test overlaid:
- At the base, the package does not build, so the run is not green.
- With its reference change, all nine tests pass: the five ordinary ones and the four hidden ones.

**The pin.** The profile pins go-mini's own prepared image (#226 entries 97 and 194): `ghcr.io/ekaynac/adc-prepared-go-mini@sha256:66b8cd3a…268a31`, built from main `8adbed7a`, for **linux/arm64 only**: its list holds one platform, so a controller on linux/amd64 fails closed at image resolution (the image is not present, `wrong_owner`) until an amd64 instance is built and pinned. It is built by this repository's recipe, `scripts/prepare-go-image.sh build go-mini NAME`: the same base and toolchain as this repository's image, labelled `dev.adc.prepared` with go-mini's `go.mod`, so `sandbox.Image.CheckManifest` accepts it for go-mini. On linux/arm64 it resolves to the platform image `1053f9d45926…`; the instance digest is the registry's, not recorded here (Opus #427 L2).

**The adapter (P22a-5d).** The recipe now also bakes the builder's http-chat adapter under `/opt/adc/lib` (`make dist`'s closure, `sandbox.AdapterClosure`) and adds each platform's `sandbox.AdapterDigest` to the label's `adapter`; `Image.CheckAdapter` refuses an image whose adapter is not the installed release's. The pin above carries it: its adapter is the release of `8adbed7a`'s (arm64 `sha256:f2143fea…83aa`). The baseline was re-measured in it with `gate.Measure` (same tree, same five results, the same 24 stream events), and the stub task's acceptance re-checked in it: at the base no result, with the reference nine tests pass, the four hidden ones among them.

**Re-recording.** Re-record with `gate.Measure` in the pinned image whenever T0's `repo/` or the pin changes. The gate refuses a baseline of another command, platform image or base tree, and `TestGateGoMini` fails when `repo/`'s tree is no longer the baseline's.
