# M1 checkpoint exit record: template (P25)

The record of one checkpoint's exit, (a), (b) or (c) (`00-slice-plan.md` §2.1–2.5, §2.8). Copy it to `docs/reviews/<date>-m1-<checkpoint>-exit.md` and fill in every section. A section that does not apply says why, rather than being left out.

## 1. What was run

- **The build:** its commit and schema set (`adc version --json`), and the prefix (`make dist`, `scripts/install-check.sh --expect <schema set>`).
- **The host:** arch, OS, the sandbox runtime (Q1), and `git` and `timeout` versions.
- **The routes:** for the builder and the reviewer, the executor id, family, model and destination (Q2). For (a), also the reviewer's qualification record (P31: its file and digests).
- **The commands:** each live and stub command as run, verbatim (§2.3, §2.4 or §2.5 "Commands"), with its exit status.

## 2. Assertions

One row per assertion of the checkpoint's section, numbered as there:

| # | Assertion | Held | Evidence (the command, event seq or file that shows it) |
|---|---|---|---|

## 3. Throughout M1

The re-runs §2.1 names: the §2.6 faults the checkpoint's capabilities reach, the §2.7 hostile-repo cases, and the T0 regression. Give each case's `--expect` block result, and its positive control.

## 4. Measured failures

Every failed live run (§2.5 item 4): what failed, the run's `(outcome, reason)` or hold, and the backlog item it became.

## 5. The benchmark (§2.8)

- **The file:** `benchmarks/m1/<date>-<GOOS>-<GOARCH>-<checkpoint>.json` (`bench.Path`) (`records/benchmark-run`, written through `internal/bench.Record`).
- **The summary:** paste `bench.Summary` of that run.
- **The deltas:** against the previous checkpoint's run, paste `bench.Deltas(bench.Compare(prev, cur))`.
  - The deltas are reported, never gating.
  - Outstanding liabilities are listed as they are, never as zero.
  - In (c), rescues must be zero.

## 6. Residuals and decisions

The stated residuals and false blocks the exit leaves standing, each with its #226 entry or decision.
