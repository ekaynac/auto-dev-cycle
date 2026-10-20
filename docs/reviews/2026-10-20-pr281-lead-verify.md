## Lead verification of #281 after Astra r6 "GO with changes" (2026-10-20)

Head 3f5f703. Astra round 6 (`2026-10-19-pr281-astra-r6.md`) gave GO with changes. It found no High and no regression against main. Its findings:
- **M1:** a pytest mark called with one function runs that function at import;
- **M2:** dotted decorator roots caused an uncharged quadratic scan;
- **L1:** namespace variants were missed.

Per D64, the lead verifies the changes, with no further external round.

**Checks**
- **The diff since d419b57:**
  - A trusted pytest decorator called with exactly one positional argument and no keyword is not inert, unless that argument is a literal. `.with_args(...)` is left alone (M1).
  - Decorator-argument roots are deduplicated. Past 16 distinct roots the file is not inert. One charged, cancellable scan does every binding proof, cached per file content (M2).
  - Every ancestor directory is classified once, charged, by what answers to its own name. A reached namespace-only name or ancestor is unread, and the candidate blocks (L1).
  - Namespace portions are outside the stdlib rule, because PathFinder builds a namespace only when no regular module answers.
- **The builder's evidence:**
  - 190 mutations, all killed;
  - 123 of main's rows changed, none weaker. The stated false blocks and the work-bound row adjustment (50,000 → 52,000) are in the PR body.
- **The lead's own probe** (throwaway, deleted): four variants that the rows do not cover. All of them make the base unread:
  - `@pytest.fixture(park)`, with a repository function;
  - `from pytest import mark` and `@mark.usefixtures(park)`;
  - `@pytest.mark.slow(park)`;
  - `from ns import leaf`, with only `ns/leaf.py`.
- **`-race`:** `go test -count=1 -race -run TestPyGuard ./internal/accept/` passes (97 s).

**Verdict:** GO.

**Residual:** as stated in the PR body, for widen-b:
- a positive purity proof for callees;
- the precise write attribution;
- third-party shadowing;
- main's base `.py` × root index charging.

The Python guard stays inactive behind P14b's blanket `.py` block until P14c-2b. P15c (Python run isolation) is also a prerequisite of the Python profile.
