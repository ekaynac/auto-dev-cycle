## Lead verification of #263 round 3 (2026-10-14)

Mythos's review of 2ccde54 was GO with changes (H1, H2, M1, M2, L3). Under D64 the lead verifies the fixes, and no further external round follows. Verified at 4e750b8.

- **H1 (a rebound `__name__`).** A per-file flag `rebound` is set by any `__name__` token, outside an exact `pyMain` line, that is not a lone call argument. Once it is set, `pyMain` opens no scope. This is conservative: `x = __name__` also disables the guard scope. Rows cover `=`, `for`, the walrus and `from __main__ import __name__`. The `getLogger(__name__)` control stays clean.
- **H2 (`name=` read as a literal).**
  - `pyFixtureName` runs on any decorator line, and on any import-time line, that names `fixture`.
  - The `name` value must be one string token, read by `pyStr` (prefix stripped; f-strings, t-strings and escapes are not literals). It must be followed by `,` or `)`.
  - Any other value blocks as "a fixture name not a literal": a variable, concatenated literals, parentheses, or `**`. The rule fails closed.
  - `pyMain` uses `pyStr` too.
- **M1.** `capsys = …` at import blocks in a tested file, or on a line that names `fixture`.
- **M2.** A conftest may name `pytest` only as `import pytest` or `from pytest import …`. `import pytest, os` is now a false block, which is acceptable.
- **L3.** `:` is out of the word split, so `-W error::X` stays default. Comment lines in an `addopts` continuation are skipped.
- **Compaction.** It removed the empty `case k.kind != 'n':` from `pyKind`. In Go, a matching case with an empty body leaves the switch, and the later cases cannot match a quoted string or an operator token. So behaviour is unchanged.
- **Checks run by the lead.**
  - `go vet` and `go test ./internal/accept/ -run 'PyGuard|PyTokens'` pass.
  - The lead re-ran two of the builder's mutations. T1 (the `rebound` flag ignored) and T3 (the `name=` prefix kept) each turn `TestPyGuardBlocks` red. The file was restored byte for byte.
- **Size.** 443 hand-written non-test lines. This is accepted: about +40 lines of fixes, against about −25 lines of compaction.

Result: verified. #263 merges on green CI.
