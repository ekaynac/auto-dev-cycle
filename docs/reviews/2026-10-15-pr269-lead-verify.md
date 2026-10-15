## Lead verification of #269 rounds 3 and 4 (2026-10-15)

Mythos's external review of 52087d3 was GO with changes. Under D64 the lead verifies the changes, and no further external round follows.

- **Round 3 (ca1fcc3).** It carries Mythos H1 (a)–(c), M1, M2 and L2, and the three background flags on 52087d3. Two of those flags were real: text-layer counts traded across platforms, and build-tag differentials. The third was not a fail-open; its dead-condition class goes to P14d.
  - `go vet` and `go test ./internal/accept/` pass.
  - The rows for the TestMain platform split, the race twin, the `.(*testing.T)`-for-`"\x16"` trade, the `exec.Command` on one platform, the variadic pair, `gob.Register` and the alias DAG are present, and each has a mutation.
- **The lead's probes on round 3 found two problems.**
  - **A hang in `go/types` itself, on main too.** `validType` is exponential on a DAG of defined struct types: `type S0 struct{ A … H S1 }` … at depth 8 takes 24 s, and depth 14 did not finish in 13 min. The profile is in `go/types.(*Checker).validType0` and `Identical`. Interface chains are fast. `Config.Check` cannot be cancelled, so a candidate could hang the controller. This is a denial of service, not a forgery: the gate's own `go test` hangs too.
  - **A memo state drift.** The `mentions` memo, added in round 3, kept a result from the first of the two setup passes. A background flag on ca1fcc3 ("fail-open-state-drift") matches it. The builder reproduced it with three aliases: `A = struct{ x B }` used before `B = *testing.T` in a later file, then a candidate adding `C = struct{ y A }`. That candidate was accepted.
- **Round 4 (6c9c7c7).**
  - The memo is cleared before the second setup pass and after it. It has a row and a mutation.
  - Every type check runs under the guard's deadline, which is now set when the guard starts. Past the deadline, the check is abandoned and the change is unverifiable. On a done context, the guard returns the context's error, and `Accept` returns it, which refuses.
  - The depth-14 chain row gives "over the guard's deadline" in-process, with a 5 s deadline in the test.
- **Checks run by the lead at 6c9c7c7.**
  - `go vet` passes.
  - `go test -race -run TestIntegrityRealTypes ./internal/accept/` passes with no data race, including the abandoned-check row.
  - The full `go test ./internal/accept/` passes.
- **Residual.** An abandoned type check keeps a CPU until it ends, and a later background flag ("resource-exhaustion") names it. This is recorded in #226 entry 85, owned by P14e-2b: a killable type-check process (loader b), or a cap on live abandoned checks until then. Before #269, the same check ran with no bound at all, so round 4 only improves on main.
- **Merge rationale.**
  - The two relaxations, M2 (type arguments) and "declared twice", were reviewed by Opus and by Mythos, and they hold.
  - Opus's two end-to-end regressions are closed.
  - Every finding since then is closed or recorded with an owner in #226 entry 85.
  - #269 also fixes on main's P14b code two fatal stack overflows, the race/!race helper and constant pairs, and the TestMain race twin.
  - The replay is unchanged (6 of 10, with the same blocked counts).
- **Size:** 462 non-test lines, one cohesive change, under the D41 ceiling.

Result: verified. #269 merges on green CI.
