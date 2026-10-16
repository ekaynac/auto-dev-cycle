## Lead verification of #279 round 3 (2026-10-16)

Astra's review of 2b33825 was GO with changes. It had no Critical or High finding and found no regression against main. Its one fix to make was M1: repeated star imports multiplied uncharged work. The lead verifies round 3 (eefb62a) and runs no second external round.

**Diff read** (`git diff 2b33825 eefb62a`, non-test +30/−3):
- **Deduplication:** `pyBase.file` records each star source once per `__init__.py`, through a set.
- **Cache:** `starCheck` caches its verdict on the file object (`starWhy`). Each `pyBase` has its own file map, the base or the candidate's, so the cache is per tree and per package.
- **Index:** a directory → entries map is built once per `pyBase` and charged to `r.work`. The competing-form scan now reads only the entries of `d`, `d/x` and `d/__pycache__`, which are exactly the places that `pyStarForm` matches.
- **Charge:** each scan charges its entries to `r.work` and checks `ctx`. Over the bound, the package is unread, so the change fails closed.

**Checks run by the lead at eefb62a:**
- `go vet ./internal/accept/` passes.
- The Python guard tests (`-run 'PyGuard|Python|Py'`) pass in 46 s.
- `-race` on `TestPyGuardStar*` passes.
- **The lead's own throwaway probe** (deleted after):
  - With the plain-star base, each competing form makes a `cfg` change block: `x/__init__.py`, `x/__init__.abi3.so`, `x.cpython-312-*.so`, `__pycache__/x.cpython-312.pyc` and `x.pyc`.
  - A candidate that adds a competing package `x/__init__.py` blocks.
  - A candidate that adds a second star blocks.
  - So the per-directory index finds every form the old full scan found, and the candidate is judged on its own files, not on the base's cached verdict.

**The new row:** `TestPyGuardStarWork` (2,000 repeated stars, 2,000 siblings, 100 conditions) passes within a bound of 50,000, and is unread at 3,000. It has no wall-clock bound.

**Result:** verified. #279 merges on green CI.
