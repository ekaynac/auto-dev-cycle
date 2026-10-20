# The Python guard's false-block baseline (P14c-2a-widen-b PR 1, D55)

Measured on 2026-10-20, on main `1c027aa` (P14c-2a-widen-a merged). This is the baseline that each widen-b relaxation is measured against (`docs/design/m1/00-slice-plan.md`, the P14c-2a-widen row; the lead's widen-b plan, whose decisions are quoted below where they matter). The Python guard stays inactive behind P14b's blanket `.py` block until P14c-2b, so these are the verdicts the guard *would* give. Opus reviewed the first version (`docs/reviews/2026-10-20-pr296-opus.md`); this version answers it.

## Method

- **Corpus:** `internal/accept/testdata/pycorpus.tsv` gives each repository's URL, tag and commit. These are the repositories Opus #266 measured: numpy, networkx, setuptools, Apache Arrow (pyarrow), sympy, TransformerEngine, and inframedic. No manifest of Opus's own 2,128 files survives, so these tags are new pins.
  - The six public repositories were cloned as `git clone --depth 1 --branch <tag> <url> <dir>`. The runner checks that each tag names the manifest's commit (`git rev-parse <tag>^{commit}`).
  - **inframedic is local-only.** Commit `828f682` is on a local branch that no public ref is known to contain. Its row is read in place from `$ADC_PY_CORPUS_INFRAMEDIC`, and it is skipped (logged) when that variable is unset. Every total below therefore has a **public** subtotal over the six public repositories, which anyone can reproduce.
  - Nothing is written into any repository: each one is cloned into a temporary directory, per repository (`t.Run`), before the guard reads it.
- **Runner:** `internal/accept/integrity_pycorpus_test.go`, built only with `-tags corpus`. `make lint` compiles it (`go vet -tags corpus ./internal/accept/`) but nothing runs it.

  ```
  ADC_PY_CORPUS_ROOT=<clones> [ADC_PY_CORPUS_INFRAMEDIC=<inframedic>] ADC_PY_CORPUS_OUT=<dir> \
    go test -tags corpus -run TestPyCorpus -timeout 2h ./internal/accept/
  ```

  It writes `summary.md` (the tables below) and `files.jsonl` (one record per file). The whole corpus takes about 80 s.
- **Per repository**, the runner reads the base as `python()` does: the configuration (`pyCustom`), the install reader, `pyBase.load` and the closure. It records:
  - the base verdict;
  - every configuration file's setting that makes it custom (not only the first);
  - the closure's first unread reason;
  - the full guard's verdict for one comment line added to the first judged file (`Integrity`, without P14b's blanket block).
- **Per file** the base closure reads (judged files included), it records each reason the file is not inert:
  - the risky signals (`pyBaseFile` fields);
  - the shape of each statement run at import that main's `pyAllowed` or widen-a's positive grammar rejects. It walks the statements as `pyBase.file` does: module level, class bodies and decorators, not def bodies or the exact `__main__` guard.

  The classifier is test code. It is checked against `pyBase.file`'s own impurity bit in both directions. The two disagreed on 0 files either way.
- **Per judged file**, it also records:
  - Opus #266's measure, `pyScan`'s import-time findings. That was 59% of 2,128 files on #266.
  - **The per-file probe:** one comment line added to the file, judged by the guard's per-change rules (the shadow rule, the scans, `pyReach`, the comparison of findings). The base-wide verdicts are set aside: the configuration, the install and `sys.path` readers, and the base and candidate closures. `pyReach` still resolves conditions through the base closure, so where the closure is unread, its "a module a condition reads is unread" repeats the closure's verdict. The tables count that block separately.

## Findings

1. **The D55 false-block rate on this corpus is 100% at repository level.**
   - Each repository is a released, pinned tree, and the edit only adds a comment line, so any block is a false block.
   - On all 7 repositories (6 of 6 public), that edit blocks.
   - Per judged file, the probe blocks 86.8% (3,167 of 3,647); public, 97.5% (2,942 of 3,016).
   - Leaving out the probe's block that repeats the closure's verdict, a per-file rule still blocks 40.5% (1,478 of 3,647); public, 41.5% (1,253 of 3,016). Most of these are strict findings: code run at import outside the allowlist (32.4%) and a dunder or allowlisted name bound at import (22.1%).
   - The repository-level rate is saturated: per-statement relaxations (the plan's PR-3 and PR-4) cannot move it. The per-file rates are the ones later PRs can move.
2. **Every repository's base is unread today, whatever its Python looks like.**
   - Six of seven bases are unread for their **pytest configuration** (`pyCustom`). TransformerEngine is unread for an unread `sys.path` entry. So on all seven, every Python change blocks before any per-file rule is consulted.
   - **One of those verdicts is a false block of main's own reader: networkx.**
     - Its only configuration file, `pyproject.toml`, has no pytest table.
     - Yet `pyConf` reads a TOML file whole whenever it mentions pytest anywhere, here in a test extra.
     - The classifier `Development Status :: 5 - Production/Stable` then yields the word `-`, which is read as an unknown pytest option.
     - Fixing the reader would not change networkx's verdict alone: its closure is unread too (`networkx/__init__.py`'s star import).
   - **setuptools and arrow hit the same defect but are custom anyway.**
     - setuptools: the classifier, again.
     - arrow: `python/pyproject.toml`'s setuptools-scm `--dirty`.
     - Their other files hold genuine settings: setuptools' `pytest.ini` has `norecursedirs` and more; arrow's `python/setup.cfg` has `[tool:pytest] addopts = --ignore=scripts`.
   - The remaining verdicts are genuine settings the guard does not model: `norecursedirs` (numpy, sympy) and `testpaths` (inframedic).
3. **The closure would make every base unread too.** With the configuration set aside, each repository's closure reaches at least one module that is not inert: 66–95% of judged files, and 67–92% of the files the closure reads. One such module makes the whole base unread, so no per-file relaxation can change a repository's verdict unless it clears every module in the closure.
4. **Opus #266's measure** (judged files with an import-time finding) is now **66.4%** (2,421 of 3,647); public, 67.4% (2,032 of 3,016). On #266 it was 59% of 2,128.
   - The judged set is larger: `pyTestDir` judges every file below a directory holding a conftest, and numpy, networkx and sympy keep a conftest at the package root, so their whole packages are judged.
   - 25.4% have a strict finding (one that never passes on the base); public, 25.9%.
5. **The planned widenings 2–9 alone would clear about 4% of the judged files that are not inert:** 105 of 2,629 (4.0%); public, 97 of 2,214 (4.4%).
   - **This is a lower bound.** The estimate does not count widening 7's `parametrize` case (a pytest decorator with arguments outside the grammar). Counting it by name raises the public figure to 137 (6.2%). Also counting #266's plain-class bases (builtin exceptions, `unittest.TestCase`) raises it to 148 (6.7%) (Opus #296 L3). The conclusion holds.
   - **Report-only kinds:** 40.9% of those files hold at least one kind the lead kept report-only; public, 45.4%. These are #281's blunt rejections from rounds 5–8, which the lead's widen-b decisions of 2026-10-20 keep out of widen-b's relaxations: lambdas, comprehensions, `for`/`async` in expressions, dotted names, class bases, single-positional decorator calls, `\N{`, stars outside a proved `__init__`, class-body imports, private-mangled names and the stdlib-name shadow. Because class bases are among them, "plain class" now means a class with no base. That is narrower than #266's widening 2.
   - **The largest kinds**, as shares of the 4,400 files the closure reads (the "Why a file is not inert" table; shares of the 3,263 not-inert files read are higher, a call value being 44.8%):
     - a call value, 33%. `datetime(…)`, `re.compile(…)`, `frozenset(…)`, `Path(…)` and `import_module(…)` lead; only some are in widening 7's list;
     - a class with a base, 25%, which is report-only;
     - a dunder method, 24%, mostly `__init__`. Widening 2 allows it only in a plain class;
     - repository decorators, 17%, such as `nx._dispatchable`, `XFAIL` and `slow`;
     - def headers, 14%;
     - `@property`, 9.5%.

## What this means for widen-b's plan

- **The largest false-block driver is not in the plan:** the base-wide verdicts, configuration and closure alike, decide every repository. PR-5 (write attribution) and PR-6 (callee purity) narrow *which* conditions an impure module makes unread. They are therefore the steps that can change repository verdicts; the per-statement widenings (PR-3, PR-4) cannot do so alone.
- **Main's `pyConf` false block** (a TOML that mentions pytest anywhere is read whole) is a strict-to-precise fix outside the plan's items. The lead approved it as its own PR: the reader reads only pytest's statements, by TOML structure.
- **PR-7 re-runs this report.** The report-only kinds are measured there and not relaxed.

The tables below are the runner's `summary.md`, verbatim.

### Repositories

| Repository | Commit | Files | .py | Judged | Read by the closure | Judged not inert | Read not inert | Judged with a finding | …strict | Per-file probe blocks | Base | Why custom | The closure's first unread reason | Guard verdict on a comment line | Seconds |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| numpy | `2f7fe64b8b` | 2195 | 568 | 527 | 527 | 441 (83.7%) | 441 (83.7%) | 403 (76.5%) | 186 (35.3%) | 527 (100.0%) | custom pytest configuration | pytest.ini: the setting norecursedirs | the module doc/conftest.py writes [doctest . OutputChecker] at import | doc/conftest.py: unverifiable: a pytest configuration of the base moves collection off its defaults or is unread | 10.2 |
| networkx | `7fdddfa2ca` | 923 | 657 | 566 | 566 | 431 (76.1%) | 431 (76.1%) | 399 (70.5%) | 46 (8.1%) | 566 (100.0%) | custom pytest configuration | pyproject.toml: the option - | the module networkx/__init__.py has a star import other than from .x import * in a package __init__.py | networkx/__init__.py: unverifiable: a pytest configuration of the base moves collection off its defaults or is unread | 8.0 |
| setuptools | `441799f8b4` | 707 | 423 | 142 | 305 | 109 (76.8%) | 257 (84.3%) | 84 (59.2%) | 18 (12.7%) | 142 (100.0%) | custom pytest configuration | pyproject.toml: the option -; pytest.ini: the setting norecursedirs | the module conftest.py runs code at import outside the allowlist | conftest.py: unverifiable: a pytest configuration of the base moves collection off its defaults or is unread | 4.0 |
| arrow | `6a2e19a852` | 7749 | 221 | 182 | 182 | 150 (82.4%) | 150 (82.4%) | 138 (75.8%) | 80 (44.0%) | 108 (59.3%) | custom pytest configuration | python/pyproject.toml: the option --dirty; python/setup.cfg: the option --ignore | the module dev/archery/archery/benchmark/codec.py runs code at import outside the allowlist | csharp/test/Apache.Arrow.Compression.Tests/generate_resources.py: unverifiable: a pytest configuration of the base moves collection off its defaults or is unread | 5.8 |
| sympy | `2197797741` | 2057 | 1590 | 1536 | 1537 | 1023 (66.6%) | 1024 (66.6%) | 958 (62.4%) | 433 (28.2%) | 1536 (100.0%) | custom pytest configuration | pyproject.toml: the setting norecursedirs | the module bin/coverage_doctest.py imports or runs code dynamically | bin/ask_update.py: unverifiable: a pytest configuration of the base moves collection off its defaults or is unread | 29.4 |
| TransformerEngine | `e79d915aa0` | 422 | 183 | 63 | 159 | 60 (95.2%) | 146 (91.8%) | 50 (79.4%) | 17 (27.0%) | 63 (100.0%) | an install key or sys.path entry unread | – | the module examples/jax/encoder/test_model_parallel_encoder.py runs code at import outside the allowlist | examples/jax/encoder/test_model_parallel_encoder.py: unverifiable: the base adds sys.path entries the guard does not read (Mythos #270 M1) | 2.4 |
| inframedic (local-only) | `828f682c3b` | 1600 | 1163 | 631 | 1124 | 415 (65.8%) | 814 (72.4%) | 389 (61.6%) | 148 (23.5%) | 225 (35.7%) | custom pytest configuration | pyproject.toml: the setting testpaths | the module docs/notes/2026-09-04-1.3-review-repros/1.3-1-rr1/build46.py holds a lambda, a comprehension or a generator at import | docs/notes/2026-09-04-1.3-review-repros/1.3-1-rr1/build46.py: unverifiable: a pytest configuration of the base moves collection off its defaults or is unread | 19.3 |
| **all** | | 15653 | 4805 | 3647 | 4400 | 2629 (72.1%) | 3263 (74.2%) | 2421 (66.4%) | 928 (25.4%) | 3167 (86.8%) | | | | | |
| **public** | | 14053 | 3642 | 3016 | 3276 | 2214 (73.4%) | 2449 (74.8%) | 2032 (67.4%) | 780 (25.9%) | 2942 (97.5%) | | | | | |

### The comment-edit false-block rate (D55)

Every repository is a released, pinned tree, and the edit adds one comment line, so any block is a false block.

| Scope | Repositories whose comment edit blocks | Judged files the per-file probe blocks | …by a rule other than "a module a condition reads is unread" |
|---|---|---|---|
| all | 7 (100.0%) | 3167 (86.8%) | 1478 (40.5%) |
| public | 6 (100.0%) | 2942 (97.5%) | 1253 (41.5%) |

The per-file probe adds the line to each judged file and judges it with the base-wide verdicts set aside (the configuration, the install and sys.path readers, the base and candidate closures). pyReach still resolves conditions through the base closure, so where the closure is unread its "a module a condition reads is unread" repeats the closure's verdict. The probe's blocks, by kind (judged files, each once per kind):

| Block | All | Public |
|---|---|---|
| a module a condition reads is unread | 2830 (77.6%) | 2830 (93.8%) |
| code run at import outside the allowlist | 1180 (32.4%) | 1050 (34.8%) |
| a dunder or an allowlisted name bound at import | 806 (22.1%) | 682 (22.6%) |
| a dunder method or __class__ bound in a test file | 147 (4.0%) | 109 (3.6%) |
| code run at import in an f-string field | 22 (0.6%) | 22 (0.7%) |
| a file holding a pytest hook, hookimpl or pytest_plugins changed | 19 (0.5%) | 11 (0.4%) |
| a skipif or xfail condition that may be a string, code pytest runs | 10 (0.3%) | 10 (0.3%) |
| a package's import path, spec or loader bound at import | 6 (0.2%) | 6 (0.2%) |
| an import binding pytest, fixture, mock, patch or a builtin fixture's name otherwise | 3 (0.1%) | 3 (0.1%) |
| a skipif or xfail condition in sympy/testing/pytest.py reads mark, which this change rebinds | 2 (0.1%) | 2 (0.1%) |
| a skipif or xfail condition in setuptools/_distutils/tests/test_archive_util.py reads TempdirManager, which this change rebinds | 1 (0.0%) | 1 (0.0%) |
| not tokenizable as Python | 1 (0.0%) | 1 (0.0%) |

### Why a file is not inert

Each file counts once per kind. "Alone": the file's only kind, so relaxing that kind alone would make it inert. "Main too": files where main's allowlist (`pyAllowed`) also rejects a statement of that kind.

| Kind | Files read | Judged | Alone (read) | Alone (judged) | Main too |
|---|---|---|---|---|---|
| value: a call | 1463 (33.2%) | 1162 (31.9%) | 249 | 211 | 1454 |
| a class with a base or keyword | 1115 (25.3%) | 821 (22.5%) | 87 | 75 | 26 |
| a dunder bound (__init__, __repr__, __eq__, …) | 1075 (24.4%) | 797 (21.9%) | 77 | 72 | 69 |
| decorator: another | 763 (17.3%) | 716 (19.6%) | 297 | 296 | 763 |
| a def header (default or annotation outside the grammar) | 616 (14.0%) | 216 (5.9%) | 75 | 19 | 41 |
| decorator: @property | 416 (9.5%) | 293 (8.0%) | 0 | 0 | 416 |
| risky: a dynamic import or exec | 332 (7.5%) | 294 (8.1%) | 33 | 32 | 0 |
| an expression statement with a call | 318 (7.2%) | 282 (7.7%) | 42 | 40 | 318 |
| an expression statement | 311 (7.1%) | 88 (2.4%) | 0 | 0 | 311 |
| value: a dotted name | 306 (7.0%) | 203 (5.6%) | 20 | 20 | 0 |
| decorator: @classmethod | 296 (6.7%) | 255 (7.0%) | 36 | 36 | 296 |
| an assignment to a target other than one name | 286 (6.5%) | 275 (7.5%) | 47 | 47 | 258 |
| value: an operator or other expression | 264 (6.0%) | 201 (5.5%) | 16 | 12 | 29 |
| decorator: pytest's, with arguments outside the grammar | 258 (5.9%) | 258 (7.1%) | 37 | 37 | 168 |
| risky: holds a lambda, a comprehension or a generator at import | 246 (5.6%) | 191 (5.2%) | 0 | 0 | 0 |
| a compound statement (if) | 228 (5.2%) | 181 (5.0%) | 0 | 0 | 228 |
| decorator: @dataclass | 206 (4.7%) | 14 (0.4%) | 0 | 0 | 206 |
| decorator: @staticmethod | 199 (4.5%) | 129 (3.5%) | 4 | 4 | 199 |
| risky: a sys use beyond the allowlist | 187 (4.2%) | 165 (4.5%) | 15 | 15 | 0 |
| value: a subscript | 187 (4.2%) | 126 (3.5%) | 14 | 11 | 3 |
| risky: an attribute, subscript or del target | 152 (3.5%) | 139 (3.8%) | 0 | 0 | 0 |
| an annotation outside the grammar | 132 (3.0%) | 58 (1.6%) | 4 | 2 | 1 |
| a compound statement (try) | 118 (2.7%) | 95 (2.6%) | 0 | 0 | 118 |
| a compound statement (except) | 117 (2.7%) | 94 (2.6%) | 0 | 0 | 117 |
| a compound statement (else) | 112 (2.5%) | 87 (2.4%) | 0 | 0 | 105 |
| an augmented assignment | 110 (2.5%) | 102 (2.8%) | 1 | 0 | 110 |
| value: a comprehension or lambda | 99 (2.2%) | 69 (1.9%) | 0 | 0 | 88 |
| risky: a write word (setattr, __dict__, vars, globals, …) | 66 (1.5%) | 56 (1.5%) | 3 | 3 | 0 |
| a compound statement (for) | 62 (1.4%) | 57 (1.6%) | 0 | 0 | 62 |
| a chained assignment | 57 (1.3%) | 54 (1.5%) | 1 | 1 | 9 |
| decorator: @contextmanager | 46 (1.0%) | 23 (0.6%) | 4 | 2 | 46 |
| decorator: @contextlib.contextmanager | 43 (1.0%) | 31 (0.9%) | 0 | 0 | 43 |
| risky: has a star import other than from .x import * in a package __init__.py | 41 (0.9%) | 40 (1.1%) | 1 | 1 | 0 |
| an import outside the allowlist (star, or pytest/fixture rebound) | 39 (0.9%) | 38 (1.0%) | 0 | 0 | 38 |
| value: an f-string | 27 (0.6%) | 18 (0.5%) | 1 | 1 | 9 |
| a del statement | 25 (0.6%) | 23 (0.6%) | 0 | 0 | 25 |
| risky: names a character by \N{…}, whose decoding may import unicodedata | 25 (0.6%) | 25 (0.7%) | 0 | 0 | 0 |
| decorator: @NAME.setter/getter/deleter | 24 (0.5%) | 20 (0.5%) | 0 | 0 | 24 |
| a compound statement (elif) | 22 (0.5%) | 20 (0.5%) | 0 | 0 | 22 |
| a compound statement (with) | 21 (0.5%) | 20 (0.5%) | 0 | 0 | 21 |
| decorator: @abstractmethod | 20 (0.5%) | 14 (0.4%) | 0 | 0 | 20 |
| decorator: @functools.lru_cache | 20 (0.5%) | 12 (0.3%) | 0 | 0 | 20 |
| risky: a package star not proved | 20 (0.5%) | 17 (0.5%) | 10 | 9 | 0 |
| a assert statement | 18 (0.4%) | 15 (0.4%) | 1 | 0 | 18 |
| an f-string field that runs code | 18 (0.4%) | 15 (0.4%) | 1 | 1 | 8 |
| a raise statement | 17 (0.4%) | 15 (0.4%) | 1 | 1 | 17 |
| decorator: @abc.abstractmethod | 9 (0.2%) | 3 (0.1%) | 0 | 0 | 9 |
| decorator: mock's patch | 9 (0.2%) | 9 (0.2%) | 4 | 4 | 0 |
| decorator: @lru_cache | 8 (0.2%) | 6 (0.2%) | 1 | 1 | 8 |
| risky: imports in a class body or imports a class-private name | 4 (0.1%) | 4 (0.1%) | 0 | 0 | 0 |
| decorator: @functools.cache | 3 (0.1%) | 3 (0.1%) | 0 | 0 | 3 |
| a compound statement (finally) | 2 (0.0%) | 2 (0.1%) | 0 | 0 | 2 |
| a compound statement (match) | 1 (0.0%) | 0 (0.0%) | 0 | 0 | 1 |
| a compound statement (while) | 1 (0.0%) | 1 (0.0%) | 0 | 0 | 1 |
| decorator: @dataclasses.dataclass | 1 (0.0%) | 0 (0.0%) | 0 | 0 | 1 |
| risky: reads a decorator argument | 1 (0.0%) | 1 (0.0%) | 0 | 0 | 0 |
| risky: unread (encoding, tokens, size or a link) | 1 (0.0%) | 1 (0.0%) | 1 | 1 | 0 |

### The most common details, by kind

Files read by the closure, each once per detail.

**a class with a base or keyword:** `(Exception)` 115, `(ValueError)` 58, `(RuntimeError)` 47, `(Expr)` 43, `(Basic)` 37, `(Function)` 36, `(unittest.TestCase)` 33, `(util.F2PyTest)` 27, `(BaseModel)` 23, `(Command)` 23, `(NamedTuple)` 23, `(support.TempdirManager)` 20.

**a dunder bound (__init__, __repr__, __eq__, …):** `__init__` 720, `__new__` 205, `__repr__` 164, `__str__` 124, `__eq__` 119, `__call__` 103, `__getitem__` 86, `__mul__` 83, `__hash__` 78, `__add__` 74, `__rmul__` 64, `__iter__` 62.

**an expression statement with a call:** `print(…)` 40, `patch(…)` 25, `pytest.importorskip(…)` 22, `sys.path.insert(…)` 17, `p.write_text(…)` 10, `warnings.filterwarnings(…)` 8, `pytest.skip(…)` 7, `setattr(…)` 7, `sympy_deprecation_warning(…)` 7, `sys.exit(…)` 7, `register_primitive(…)` 6, `warnings.warn(…)` 6.

**decorator: another:** `nx._dispatchable` 227, `XFAIL` 97, `not_implemented_for` 91, `slow` 63, `public` 55, `cacheit` 47, `py_random_state` 37, `array_function_dispatch` 26, `set_module` 26, `doctest_depends_on` 24, `open_file` 15, `_both_exp_pow` 12.

**decorator: mock's patch:** `mock.patch` 4, `mock.patch.dict` 2, `patch` 2, `patch.object` 2.

**decorator: pytest's, with arguments outside the grammar:** `pytest.mark.parametrize` 205, `pytest.mark.skipif` 88, `pytest.mark.xfail` 14, `pytest.fixture` 9.

**value: a call:** `datetime(…)` 202, `re.compile(…)` 138, `frozenset(…)` 80, `Path(…)` 66, `import_module(…)` 64, `Symbol(…)` 55, `dict(…)` 48, `field(…)` 48, `np.array(…)` 43, `pytest.importorskip(…)` 38, `timedelta(…)` 38, `MatrixSymbol(…)` 29.

### Judged files' import-time findings (Opus #266's measure)

| Finding | Judged files |
|---|---|
| code run at import outside the allowlist | 2247 (61.6%) |
| a dunder or an allowlisted name bound at import | 806 (22.1%) |
| a dunder method or __class__ bound in a test file | 147 (4.0%) |
| a skipif or xfail condition that may be a string, code pytest runs | 94 (2.6%) |
| a star import | 57 (1.6%) |
| code run at import in an f-string field | 31 (0.9%) |
| a package's import path, spec or loader bound at import | 14 (0.4%) |
| an import binding pytest, fixture, mock, patch or a builtin fixture's name otherwise | 3 (0.1%) |

### What widen-b's planned widenings and its report-only kinds cover

"Covered": every kind of the file is one widenings 2–9 of Opus #266 would accept (approximately: by kind, and by callee or dunder name). "Report-only": a kind the lead kept out of widen-b's relaxations (#281 rounds 5–8).

| Scope | Not inert | Covered by widenings 2–9 | With a report-only kind | Only report-only kinds |
|---|---|---|---|---|
| judged files | 2629 | 105 (4.0%) | 1075 (40.9%) | 110 (4.2%) |
| files read | 3263 | 113 (3.5%) | 1420 (43.5%) | 125 (3.8%) |
| judged files, public | 2214 | 97 (4.4%) | 1005 (45.4%) | 100 (4.5%) |
| files read, public | 2449 | 101 (4.1%) | 1160 (47.4%) | 110 (4.5%) |

Classifier disagreements with `pyBase.file`: 0 files the classifier calls impure and the guard pure; 0 the guard calls impure and the classifier pure.
