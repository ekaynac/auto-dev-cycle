## Lead verification of #267 rounds 6 and 7 (2026-10-15)

Mythos's round-4 re-check of 76d9e77 was GO with changes. Under D64 the lead verifies the changes, and no further external round follows.

- **Round 6 (5f822a3): the two Mythos r4 items.**
  - **The alias of a partially applied mark:** the value is cut at its first `(`.
  - **The trailing `;`:** an empty statement is skipped before `pyBinds`.
  - The diff is as described, and `go vet` and the accept tests pass.
- **The lead's probes on round 6 found a gap within the promise "a call after the mark's call".** The lead ran 21 mark forms in a conftest fixture body through `f.guarded`, with a throwaway test that was deleted after the run.
  - `(pytest.mark.xfail(False))('x')`, its double-parenthesised form, the walrus form, and `m = (pytest.mark.xfail)` followed by `m(False)('x')` all passed.
  - At module level, `SKIP = (pytest.mark.xfail(False))('x')` only warned.
  - A background flag on 5f822a3 ("validator-bypass (parser differential)") matches these forms.
- **Round 7 (91f2ea8).**
  - **The "called again" check:** it skips closing `)` tokens after the mark's call, and before the mark's own `(`.
  - **Alias values:** they are stripped of enclosing parentheses and walrus targets, and a walrus target bound to a mark is an alias.
  - **Rows and mutations:** eight rows, each red before the fix, and five mutations.
- **The lead re-ran 27 forms at 91f2ea8,** the 21 above plus 6 new ones (parenthesised chains, `.with_args` in parentheses, nested walrus, and a tuple index). 22 block, including the module-level form, which now blocks on `money.py`.
- **Five forms stay silent in fixture bodies:**
  - `getattr(pytest.mark, 'xfail')(False)`;
  - a list of marks;
  - a `for` target over marks;
  - an alias of an alias;
  - a tuple index.

  Each passes the mark through a container, a call or a second name, which a token rule cannot follow. They are outside the PR's stated promise, and they are recorded in #226 entry 84, owned by P14c-2a3 (the closed body set). A second background flag on 91f2ea8 ("validator-bypass (parser differential)", with no details) is taken to be this class.
- **Accepted false block:** `foo(pytest.mark.xfail(False))(x)` (stated in the PR body).
- **Merge rationale.**
  - Against main, #267 only adds blocks: main has no `skipif`/`xfail` condition rule.
  - P14b's fail-closed rule still blocks every Python change until P14c-2b. So no remaining gap can weaken the guard compared with main.

  This follows the forgery stance (memory p14b-forgery-residual): known forms block, the residual is stated, and the remaining gaps go to the next item.
- `go vet` and `go test ./internal/accept/` pass at 91f2ea8. The builder's `make ci` passes.

Result: verified. #267 merges on green CI.
