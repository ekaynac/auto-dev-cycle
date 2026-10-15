## Lead verification of #274 rounds 3 and 4 (2026-10-15)

Astra's round-2 review of 99edbd1 was GO with changes. Under D64 the lead verifies the changes, and no further external round follows. On the operator's instruction, Astra replaced the Opus pre-review for this PR only.

- **Round 3 (06c4a0b): Astra r2's change.**
  - The package-level `readPkgs` is replaced by a function-level `readFuncs`: `strings.Join`, `strings.Map`, `path/filepath.Base`, `path.Base`, `time.Date`, `time.Sleep`. `fmt.Append*` is dropped.
  - Rows: Astra's three `asn1.NullBytes` inputs (`utf8.EncodeRune`, `utf8.AppendRune`, `strconv.AppendInt`) and `errors.As(err, time.UTC)` from `init` block as writes. Main's "an inert value passed on" still passes.
  - The lead read the diff, and `go vet` and the full accept tests passed.
- **The lead's finding on round 3.** The `fmt` rule (`Contains(name, "rint")`) still allowed `Fprint*`, which writes its `io.Writer` argument.
- **Round 4 (fbc4680).**
  - `fmt` allows only `Sprint*`, `Print*` and `Errorf`. `Fprint*`, `Sscan*` and `Fscan*` are writers.
  - When `os.Args` is counted, `Fprint*` counts only its first argument, like `copy`'s destination, so `fmt.Fprintln(os.Stderr, os.Args)` passes.
  - Rows:
    - `fmt.Fprint(util.Buf, "x")` from `init` blocks (it also blocked on 06c4a0b, through older rules);
    - `fmt.Fprintln(os.Stderr, os.Args)` passes;
    - `fmt.Sscan(s, &os.Args[0])` counts as "os.Args assigned".
  - The diff from 06c4a0b is 23 added and 4 deleted lines.
- **Checks run by the lead at fbc4680.** `go vet ./internal/accept/` passes, and `go test ./internal/accept/` passes (104 s). The builder's replay is unchanged: 6 of 10 accepted, blocked 1/25/4/7.
- **Merge rationale.**
  - Astra's M1 regression is closed.
  - The package allowlist is replaced by an allowlist of functions.
  - The `fmt` writers are closed.
  - The relaxation for numeric type parameters was reviewed by Astra and holds.
  - The stated residual (decision A, false blocks only): unlisted read-only standard-library functions count as writers when given `os.Args` or another package's value from `init`.

Result: verified. #274 merges on green CI.
