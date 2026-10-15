## Lead verification of #278 (2026-10-16)

Astra's review of 653e1ca was GO with changes. Under D64 the lead checks the changes, and no second external round follows. The lead read `git diff 653e1ca cdbd8e1 -- docs/design/m1/00-slice-plan.md` against each finding.

1. **High, exhaustion (fixed).** §5.3 and the P14b-out-1 row now state the whole exception. Reaching the layer's bound warns, but a tested package that the layer did not finish gets an import-only scan of its changed or added Go files. A class-2 import, or a scan that cannot finish (a read failure, a parse failure or the scan's own bound), blocks. Main's findings keep their reporting priority.
2. **Medium, FileFinder order (fixed).** P14c-2a-star now gives the order: a package before a module, and in each search an extension, then `.py`, then `.pyc`. Any competing form makes the star not inert.
3. **Medium, dropped rows (fixed).**
   - P14c-2a-star lists the six background-review cases, each with its outcome.
   - P14c-2a-syspath lists three more rows, each with its outcome: Astra #272 r2's absolute join part (`/work/repo/vendor/lib`), `.parent` of an unsupported expression, and Opus #275 P2e.
4. **Medium, P14b-out-2 (fixed).** The row names Astra #276 r3 findings 4–7 with their inputs, and keeps the plain `func TestMain()` counterexample.
5. **Medium, widen's scope (fixed).** P14c-2a-widen now covers code that runs on import and values assigned at run time, with Opus #277 A02, A02b, A02c, A02d, G01 and G02 as its acceptance rows. P14c-2a-install stays off until those rows pass.
6. **Low, sizes and statuses (fixed).**
   - The sizes follow D44 (added plus deleted non-test lines): P14b-out-1 622 (round 9, open), P14c-2a 443 (#263), P14c-2a-allow 430 (#266), reach 377, roots 227, plain 107, and P14e-2b 447 (#271).
   - §5's sizes line names the convention.
7. **Low, the test-time rule (fixed).** The rule is about 150 s for an uncached standalone `go test -count=1 ./internal/accept/` on the reference host. A scaled fixture keeps its discriminating assertions and its mutation catches.

**Result:** verified. `make lint` passes (builder report). #278 waits for the operator's approval (plan PR rule) and green CI.
