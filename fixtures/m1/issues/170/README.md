# M1 first real target: issue #170, item 3

This is the first real target for M1's checkpoint (a), chosen under slice plan §2.2 and approved by the operator on 2026-10-13.

| File | What it holds |
|---|---|
| `task.json` | The `task.create` scope: project, title, an exact requirement, and the acceptance attachment by path and digest. |
| `baseline.json` | A provisional `go test -json ./internal/cli/...` record at the base, and the acceptance tests' expected results, as counts only. |
| `baseline-report.json` | The same run as the gate's base: `results.ParseGoTestJSON`'s own `Report` encoding, recorded in the prepared Go image (P32), with its command and the three tests that skip as root. Provisional until P15 re-records it in the pinned image (`ghcr.io/ekaynac/adc-prepared-go@sha256:1d2fa05bf24dc73dff179b0b08e6ee89cb82d8ac47b0c133cc578210fcce744b`). |

## Selection evidence (§2.2)

| Criterion | Evidence |
|---|---|
| 1. Useful and open | #170 is open. Item 3 is a finding from the Mythos review of #169: independent-review output prints no `requested:` line, so an operator cannot compare what was asked with what is bound. `at its digest` names a digest that is never shown. |
| 2. Bounded | One file in `internal/cli`, a package that already has tests, about 10–25 lines. It touches no schema, gen, spec, docs, `.github`, Makefile or test-sandbox script. `internal/cli` is not a contract package. Item 2 of #170 edits a schema and is excluded. Item 1 qualifies too; item 3 was chosen as the smaller, safer first run. |
| 3. Exact acceptance | Hidden Go tests that fail at the base and pass with a known-good reference change, both kept outside the repository (see below). Every assertion is stated in `task.json`'s requirement. |
| 4. Recorded baseline | Green at the base, offline: first on the host toolchain, then in P32's prepared Go image (`results.Report.Green`, with three root-only skips). Provisional until the image digest is pushed and the run is re-recorded in the pinned image. |
| 5. No dependency, network or gen | Output formatting only. `make gen` is not needed. |
| 6. Not D7, not a migration | #170 is not in 15's D7 table. It follows #169, which delivered a D7 item, but it is itself a review follow-up. It is not a migration. |

## Hidden tests and reference change: outside this repository

§2.2 keeps the hidden tests outside every builder input. Here the target is this repository, so a builder's clone of `main` would contain anything committed. For that reason the acceptance tests and the reference change are not committed. They are recorded by sha256 only, in `task.json` and `baseline.json`.

- **Where they are.** The lead keeps them in its own store. Only a reader with access to that store can check them against the digests.
- **How they are used.** The test file is uploaded as the task's acceptance blob (P2, P3) when the task is created after P22a, and is overlaid on the gate workspace only (§2.6).
- **Canary.** It holds an unpublished canary line for §2.3's confidentiality check.
- **Name collisions.** Its identifiers use a prefix that `task.json` reserves, so the builder's own code cannot collide with them.

## Re-recording

Re-record `baseline.json` in the pinned Go image once P32 merges. Re-record it again whenever the commit the task is created on differs from `base`: the package's tests depend on other packages, not only on `internal/cli`.
