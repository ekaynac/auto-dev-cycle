## Lead verification of #288 after Opus "GO with changes" (2026-10-19)

Head 8022428. #288 is a small fix PR, so Opus is its only reviewer (04 §3). Opus (`2026-10-19-pr288-opus.md`) gave GO with changes:
- **H1:** the stall was not timed from an event;
- **H2:** a write with no deadline was not seen;
- **M1:** the load counts were unfinished.

**Checks**
- **The diff** (`internal/fixtures/stream.go` only):
  - `slowClient` records `stalledAt` when the first stalled write begins, and `endedAt` when the handler returns. The stall branch checks `endedAt - stalledAt <= Poll + frameSlack + lag` (H1).
  - A write with no deadline blocks on `<-c.done` (the request's end), so a server that sets no write deadline is held until the 10 s watchdog cuts it, and that is red (H2).
  - `lag` is the larger of `schedLag(5)` measured before and after `serve`, so the bound stays relative to this run.
- **The builder's evidence:**
  - Opus's two scratch defects are now red;
  - the named mutation is still red;
  - load after the fix: 0/50, 0/20 and 0/12. Before the fix, 14/14 `TestDevFixtures` failures had the CI signature, under the builder's own orphaned loops; the body states this.
- **The lead's own run:**
  - `go test -count=3 ./internal/cli/ -run TestDevFixtures` passed (17.8 s);
  - `go test -count=2 ./internal/fixtures/ -run TestFixturesAreGreen` passed (21.2 s).

**Verdict:** GO. This closes #226 entry 86. Entry 89 (`TestIntegrityDoS`'s fixed bound) stays open.
