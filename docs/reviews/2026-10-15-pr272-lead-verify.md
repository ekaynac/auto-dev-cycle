## Lead verification of #272 round 5 (2026-10-15)

Astra's round-3 review of the split, e05469c, was GO with changes. Under D64 the lead verifies the change, and no further external round follows.

- **The change (626fd7e).** It closes Astra r3's one Medium:
  - `follow`, the star-expansion loop and the token loop check the work bound and `ctx` before each resolution, expansion and token;
  - exhaustion propagates at once as an unread, over-the-bound block;
  - `follow` charges every attempt.

  The diff from e05469c is 46 added and 4 deleted lines, across `integrity_pyreach.go`, the test file and the review record.
- **The row "r5-M1 a star expansion bounded".** 10,000 star imports bind `Z`, a binding reads `Z` 10,000 times, and the work bound is 100,000. It gives "over the guard's work bound" inside the row's 10-second limit. At e05469c the same call took about 40 s.
- **The mutation.** All three checks were removed together, because each one alone limits the run to one token's expansion. That turns the row red at 39 s.
- **Background flags on e05469c.**
  - One is the same M1, now fixed.
  - "Control regression": the builder ran main's (f1aaa90) `integrity_pyguard_test.go` against this head. Every main row still blocks. S1 and S2 now block as unread instead of with the shadow message, which is the split's intent.
  - The other two are residuals for P14c-2a-reach and P14c-2a-roots, under the last-chase rule.
- **Checks run by the lead at 626fd7e.**
  - `go vet ./internal/accept/` passes.
  - `go test ./internal/accept/` passes (81 s).
  - `go test -race -run TestPy ./internal/accept/` passes, with no data race.
- **Merge rationale.**
  - Astra r3 confirmed the split: the root readers are removed, the roots and the floor are main's, and Astra r2's inputs block.
  - The last Medium is closed.
  - There is no regression against main.
  - P14b's fail-closed rule still blocks every Python change until P14c-2b.

Result: verified. #272 merges on green CI.
