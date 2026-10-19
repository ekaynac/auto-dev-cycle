## Lead verification of #283 after Astra r5 "GO with changes" (2026-10-19)

Head e1a58c7. Astra round 5 (`2026-10-19-pr283-astra-r5.md`) gave GO with changes, with one Low (L1): when the budget runs out, a target could be counted without being hashed. Astra confirmed three things:
- the construction holds for completed prints;
- round 4's H1, M2 and L3 are closed;
- there is no regression against main.

Per D64, the lead verifies the change, with no second external round.

**Checks**
- **The diff since 7f199b2:** `typedPrint` now takes `charge` and `commit` callbacks.
  - A node's S targets are charged as they are resolved, but staged (`sTarget`).
  - They are committed (counts, `tuses`, `outTargetHook`) only after that node's print bytes are charged and hashed (`emit`, then `for _, t := range staged { if !ok { break } … }`).
  - If the hash fails, nothing from that node is committed.
  - Nothing else changed.
- **The new row** `TestOutTypedBodyBoundary` measures the boundary budget in the same run (no wall clock). It asserts no hook event from the failing node, and the types stop warning only. The builder showed that the mutation "commit before the hash" is caught by this row.
- **The lead's own probe** (throwaway, deleted): a swap through a generic wrapper, `type W[T any] struct{ P *T }` with `var x W[X]`, `var y W[Y]` and `closeA = x.P.Close`, `closeB = y.P.Close`. A separate file swaps `X = F`/`Y = R` (aliases of `os.File` and `os.Root`). The probe ran in an `init` (package `p`) and in a `TestMain` (package `q`), through `outGuarded`.
  - **Result:** typed entries `p/a.go changed bodies that use os (1: init body)` and `q/m_test.go changed bodies that use os (1: TestMain body)`. The on/off invariant held, and there were no blocks.
  - **A first, invalid version** (an embedded `*T` field, which Go refuses) was blocked by main as "does not type-check". The layer gave only its incomplete warnings, as designed.
- **`-race`** on `TestOutTypedBodies`, `TestOutTypedBodyCharge`, `TestOutTypedBodyBoundary`, `TestDecisionAOut3`, `TestOutTypedEntries` and `TestOutCheckHandOff` passed in 46.6 s.

**Verdict:** GO. L1 is closed, and no other code changed after Astra's GO with changes.

**Residual (stated in the PR body):**
- A type deeper than `typeStr`'s bound (256 nodes, depth 16) compares equal when its bounded prefix is equal.
- Any change by types or of text inside a body that uses S warns (a typed false warning). The S-only precision is cut to a later item.
- Dropping the declaration ordinal alone is not caught: it is redundant with the chained `FuncDecl` print.
- The target-emission mutations are caught by the charge row's hook checks, not by a swap row.
