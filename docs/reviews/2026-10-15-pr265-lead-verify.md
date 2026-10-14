## Lead verification of #265 rounds 4 and 5 (2026-10-15)

Mythos's round-2 review of cc44c85 was GO with changes. Under D64 the lead verifies the fixes, and no further external round follows. Verified at fdec10c.

- **M1, the type-error rule by platform (round 4).**
  - **The hole.** Round 4 ignored type errors in files that do not build on `typePlatform`. The lead found that this fails open: a forgery in an `x_amd64` file beside an amd64-only symbol is typed against arm64 export data, gets no type and no fact, and its error was ignored. The builder reproduced it at 57fb44b: accepted, 0 blocks.
  - **Round 5 reverts the relaxation.** Any error in an analysed file, other than a redeclaration across platforms' files, makes the package unverifiable.
  - **The cost.** The false block for packages with per-platform files is recorded for P14e-2 (#226 entry 85: per-platform type checking).
  - **Rows:** "a forgery typed away on the other platform" blocks; "files for each platform" blocks.
- **The flag-driven fixes (round 4).**
  - Redeclarations are matched on the whole message with an anchored regexp. Before, a substring match let `var _ = redeclared` through: a real fail-open.
  - Value-making reflection blocks when the base OR the candidate holds a Go test.
- **Reflect functions as values (round 5).** `set := reflect.Value.Set` hid value-making reflection: a real fail-open that forged end to end. Now a reflect function that is not called is a fact, and the makers are matched at their identifier, whether called or not.
  - The lead ran the mutation that drops the "not called" clause. The forgery still blocks through `New` and `Set`, which is the essential identifier match. The row goes red only through its expected `(ValueOf)` message, so that mutation discriminates the message, not the block.
- **Checks run by the lead.**
  - `go vet` and `go test ./internal/accept/` pass at fdec10c.
  - The `built`-lookup mutation (at 57fb44b) turned `TestIntegrityRealTypes` red, so the type-error path is live.
  - The builder's replay is unchanged: 6 of 10.
- **Merge rationale.** P14e-1 only ADDS blocks. Every P14b rule still runs unchanged, and type errors fail closed. So no gap that remains in P14e-1 can weaken the guard compared with main.
- **Background flags on fdec10c,** "fail-open on redeclaration" and "incomplete fix": not reproduced here. They go to P14e-2 (#226 entry 85), to be reproduced first.
  - The plausible route is a name declared in two build-exclusive files with different types: the redeclaration is exempt, and the second file's uses are typed against the first declaration.
  - Fix idea: exempt a redeclaration only when the two types are identical.
- **Size.** 795 non-test lines, under the ~800 ceiling (D41).

Result: verified. #265 merges on green CI.
