## External review of chunk 5b, K1 and K2 (Astra, 2026-10-23)
Verdict: NO-GO

Two High findings violate “the host wins” and can produce paid repairs. Review was read-only at `54935168`; no tests or executable probes were run.

1. **High — A teardown failure loses to a candidate refusal.** `internal/accept/integrity_proc.go:516`

   **Concrete input:** the base listing succeeds on `linux/amd64`; the candidate’s `golist` records `1`; removing that candidate listing’s sandbox directory fails with an I/O error. `boxList` explicitly joins the refusal with `ErrGuardRun` at `internal/accept/integrity_loader.go:195`.

   `listVerdict` sees `errRefused` and the successful base, classifies the joined error as a refusal, and returns no host error. The accept becomes `failed/test_integrity_violation`, allowing a paid repair despite the cleanup fault. Main’s `listDidNotRun` handling gave `ErrGuardRun` precedence, so this is also a regression.

   **Fix:** check `errors.Is(l.err, ErrGuardRun)` before the candidate-refusal branch. Add a combined-error row—successful base plus `errors.Join(refused(...), guardRun(...))`—and assert `could_not_run` with no repair.

2. **High — An earlier listing refusal masks the child’s own panic or deadline.** `internal/accept/integrity_proc.go:424`

   **Concrete input:** the base listing succeeds; the candidate listing records an attributable failure shortly before `ListEnd`; subsequent type checking reaches its deadline. `typeCheck` panics, and the new recovery in `integrity.go:103` returns no blocks and a guard error. The child consequently sends `erred=true`.

   `converse` processes `listRefusal != "" && len(blocks) == 0` **before** `c.erred`. It manufactures a refusal and returns a nil error, discarding the guard failure. The five-minute type-check deadline can precede the ten-minute whole-run deadline, so the outer timeout does not necessarily rescue this classification.

   **Fix:** process `c.erred` before synthesizing a listing-refusal block. Extend the existing process-service fixture with successful base listing, candidate refusal, then an errored child result with no blocks; assert a host error and no repair.

3. **Medium — Candidate-controlled package diagnostics can trigger `killedStep`.** `internal/accept/integrity_loader.go:299`

   **Concrete input, traced statically:** retain a valid main module, add:

   ```go
   require example.com/dep v1.0.0
   replace example.com/dep => "example.com/signal: killed" v1.0.0
   ```

   Then import an otherwise unresolved external package such as `example.com/missing`. `listable` accepts the versioned replacement: it checks replacement directories, but does not validate the replacement module path. Resolving the missing import requires the module graph; its missing-`go.sum` diagnostic names the replacement. With `go list -e`, that package error reaches the listing’s error column and contains `signal: killed`.

   Direct import-path injection is restricted by `listable`; candidate source filenames are not copied into this listing workspace. The package-diagnostic route remains.

   This produces retries and ultimately the visible `runner: failed` hold, never acceptance. It is therefore nonblocking under the stated bounded-and-visible rule, but prevents automatic repair of a candidate defect.

   **Fix:** parse and unquote the error column and recognize the complete pinned-toolchain termination diagnostic, such as the compiler executable followed by `: signal: killed`. Searching only the error column for the same substring is insufficient. Share that diagnostic predicate with `LocalLister`.

4. **Medium — The new file readers duplicate an existing bounded-read rule.** `internal/accept/integrity_loader.go:274`

   `goListStatus` and `listKilled` separately implement the no-follow, nonblocking, regular-file, bounded-read sequence already present in `internal/qualify/qualify.go:634` (`readRegular`).

   **Concrete input:** a regular `golist` containing `"0"` followed by fifteen spaces and trailing garbage. `goListStatus` parses only its first sixteen bytes as success; the existing reader rejects an oversized file. This illustrates divergence between implementations. I found no candidate route to writing `golist` directly.

   **Fix:** extract the existing reader into a shared internal helper, reuse it, and parse only a complete status file. Preserve the distinct listing-size classification explicitly.

The remaining requested checks:

- **Platforms and budgets:** exact-platform, same-run base matching is present. A missing base listing, failure on another platform, or candidate consumption of the shared budget causing a later base failure leads to a host error. The reverse-attribution residual is documented. Findings 1–2 are additional precedence failures, not those stated residuals.
- **Missing or forged status:** a missing `golist` becomes a host error. Candidate code is not executed by the listing container, and only module files are copied in; no direct candidate-write route was found. Finding 4 covers malformed suffix handling.
- **Repair lifecycle:** `OfLastRound`, the existing successor uniqueness check, and `roundRail` prevent repeated repairs of one round and execution beyond the rail. Refused-round gates and reviews wait without claims or leases. `CandidateGo`, `authority.endable`, and board column selection permit a later GO to finish the task and release admission.
- **Holds:** retry holds have a timed clearing path. The terminal `runner: failed` hold intentionally remains visible; revoking approval abandons the task. No additional unheld stuck-task path was identified.

Three kept-bit spot-checks were traced through production wiring and their test assertions:

| Bit | Production enforcement | Row inspected |
|---|---|---|
| I16 Consumption | `daemon/wiring.go:161` supplies rails; `graph/rails.go:180` holds the next build before claiming it | `TestRoundRail` |
| I10 Consumption | `daemon/place.go:280` records the intent in the launch transaction | `TestTheBuildersIntentIsRecordedAtLaunch`, `TestAForgedIntentIsRefused` |
| I12 Admission | `daemon/accept.go:136` installs `Integrity`; `daemon/gate.go:128` rebuilds and compares the accepted tree | `TestAGateRefusesATreeThatDoesNotRebuild` |

`TestTable` asserts exactly the four promised missing launch rows; launch remains inactive. The I12 guard is wired, but its host attribution needs findings 1–2 fixed.

**Reuse search:** I enumerated added declarations with `git diff --unified=0` and searched their names with `rg`, including test helpers. Purpose searches covered exit/status classification, bounded regular-file reads, listing attribution, repair verdicts, successor rounds, producer lookup, builder-safe rendering, and hold clearing. `AcceptRefused`, `roundReview`, `verdictOf`, and the listing helpers reuse the existing repair/rendering machinery; the concrete duplicate implementation found is finding 4. The process frame continues using its existing serializer and bit reader.