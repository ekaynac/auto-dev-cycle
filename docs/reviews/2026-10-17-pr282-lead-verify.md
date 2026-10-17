## Lead verification of #282 after Astra r4 "GO with changes" (2026-10-17)

Head a135cde. Astra round 4 (`2026-10-17-pr282-astra-r4.md`) gave GO with changes, with two Lows in the tests:
- **L1:** probe E bypassed the invariant.
- **L2:** `outSame` did not check that the suffix after main's report holds only typed entries.

Per D64, the lead verifies the changes, with no second external round.

**Checks**
- `git diff 936fa4b..a135cde -- ':!*_test.go' ':!docs/'` is empty: no production change after Astra r4.
- **L1:** `TestDecisionAOutRound6` probe E now goes through `outGuarded`, so the invariant and the base-equality checks apply, and its block assertion is kept.
- **L2:** `outReports` accepts main's report byte for byte, then only the enabled verdict's typed warnings, encoded as `Report()` encodes them, in order. `TestOutTypedEntries` adds two bounds where main's report already leaves text entries out. The builder showed with a throwaway test that Astra's counterexample (`[A,B]`, one truncated) now fails the helper.
- **The lead's own probe** (throwaway, deleted): one package, through `outGuarded`, with these mixes:
  - one binding with a typed signal (`w.Parse`) and a text signal (`flag.Parse`);
  - a generic S type argument (`G[*flag.FlagSet]`);
  - a method value (`f := w.Parse`);
  - an `init` body with a typed use only (cut to P14b-out-3b).

  **Result:**
  - The invariant held: blocks and non-`Typed` warnings were byte-identical with the types check on and off.
  - `y` and `g` were reported once, as text, with the typed signal of `y` deduplicated.
  - `mv` was reported as a separate typed entry.
  - The `init` change was blocked by main's own rule, as on main.
- **`-race` on the touched rows** passed in 49.9 s: `TestOutTypedEntries`, `TestDecisionAOutRound6`, `TestOutCheckHandOff`, `TestOutReportPriority` and `TestOutTypesApart`.

**Verdict:** GO. The two Lows are closed. No production code changed after Astra's GO with changes.

**Residual (stated in the PR body):**
- P14b-out-3b: the typed-only fingerprints of `init` and `TestMain` bodies, as the next item.
- MN1 is not caught by a row: the ordering holds by structure.
- MX7 is caught only by the test timeout.
- Opus #280 L2's two forms, print and println.
- L3: the shapes of a tolerated redeclaration.
- A dropped worker makes later runs' types check stop.
