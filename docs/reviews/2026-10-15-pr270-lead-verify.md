## Lead verification of #270 rounds 4 and 5 (2026-10-15)

Mythos's round-2 review of 8f2a20c was GO with changes. Under D64 the lead verifies the changes, and no further external round follows.

- **Round 4 (d859e60).**
  - **Mythos r2 H1, M1 and L6:** `pyPure` gives each statement an owner, and an owner can be an alias.
  - **The other Mythos r2 items:**
    - M6: the keyword arguments of an alias factory are conditions;
    - M2: the floor applies only directly under a basedir;
    - M3: star imports are resolved first, and keyword chain heads are skipped;
    - M4: `pythonpath` is read on normalised text, with rows for an inline table and a dotted key;
    - M5: def bodies are compared;
    - L1: a module that binds names at run time is unread;
    - L2: `pyShadow`'s loops check ctx.
  - **The three background flags on b5a634d,** answered in decision A's allowlist shape:
    - a star module is read only when every module-level statement is plain;
    - control dependence runs through the continuing headers;
    - a config with an ambiguous line break is unread.
  - 56 mutations, each turning its rows red.
- **Round 5 (8be232a).**
  - The builder ran main's `integrity_pyguard_test.go` against d859e60. Every main row still blocks; only two messages are reworded.
  - The allowlist escape `X = globals().update(...)` in a star module is fixed: the value of a plain assignment may hold no `(`.
- **Checks run by the lead.**
  - At d859e60, the lead re-ran its 27 mark probes (from the #267 lead verification) as conftest fixture bodies, plus the module-level parenthesised form, plus three new factory forms: a factory with no parameter, a caller above its factory, and a keyword argument. All 31 block.
  - `go vet`, `go test -race -run TestPy` and the full `go test ./internal/accept/` pass at d859e60.
  - At 8be232a, the diff from d859e60 is the 3-line `pyStatic` tightening and its row. `go vet` and the full accept tests pass.
- **Background flags after round 4.** Under the lead's "last chase" rule, the flags are classified as follows:
  - "control-regression ×2 / validator-bypass" on d859e60: not a regression against main (the builder's run of main's rows); the allowlist escape is fixed.
  - "validator-bypass (parser differential)" on 8be232a: in this PR's new shadow code, not in main's rules.

  Both go to the P14c-2a-reach residual in #226 entry 84, together with the builder's two factory residuals: a condition parameter named like a mark keyword, and a mark reached through `from pytest import mark as m`.
- **Merge rationale.**
  - No regression against main.
  - Every Opus and Mythos High is closed.
  - The remaining routes are stated in the PR body and owned by P14c-2a-reach (#226 entry 84).
  - P14b's fail-closed rule still blocks every Python change until P14c-2b.
- **Size:** +620/−67 non-test lines. This is one cohesive change, under the D41 ceiling.

Result: verified. #270 merges on green CI.
