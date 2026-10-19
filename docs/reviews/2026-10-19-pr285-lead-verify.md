## Lead verification of #285 after Opus "GO with changes" (2026-10-19)

Opus (`2026-10-19-pr285-opus.md`) gave GO with changes: one Medium and two Lows.

- **Medium (local image state on the arm64 host).** This was real. The lead's two-arch pull-back left the list-digest name on the amd64 image. The fix is in the host state, with no change in the repository:
  - The lead ran a native `podman pull ghcr.io/ekaynac/adc-prepared-go@sha256:1d2fa05b…744b`, with no `--platform`.
  - `podman image inspect` of the pin now reports `e0abcae7… arm64 linux`.
  - The follow-up, that `sandbox.Host.Resolve` must refuse an image of the wrong architecture or OS, is #226 entry 88. Its owner is P15, which runs the gate on this pin first.
- **Low (#170 notes).** Fixed in this PR:
  - `fixtures/m1/issues/170/README.md:9` and the `note` of `baseline-report.json` now say "Provisional until P15 re-records it in the pinned image (`…@sha256:1d2fa05b…744b`)".
  - `go test ./internal/results/... ./internal/fixtures/...` passes; `internal/results` reads that file.
- **Low (slice plan status).** It stays as it is. The slice plan is updated at the next reconcile: the push is done (#285), and the package visibility is the operator's.

**Verdict:** GO.
