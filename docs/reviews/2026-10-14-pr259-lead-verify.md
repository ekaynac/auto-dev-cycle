## Lead verification of #259 round 4 (2026-10-14)

Astra's round-2 review of 670a8dc was GO with changes, with two Lows. Under D64 the lead verifies those fixes and no further external round follows. Verified at 61e0498.

- **L1 (Clock memory).**
  - **The change.** `Clock.evict` runs under `c.mu` on every note, every commit callback and every `young` call. It deletes only entries that are committed, have no pending submission, and are aged past `FloorGrace`.
  - **Behaviour is unchanged.** `young` already reads such an entry as old, and a missing entry also reads as old.
  - **No cross-key harm.** The failure callback's identity check (`c.at[key] == n`) stops an evicted entry's late callback from touching a new entry under the same key.
  - **Tests.** `TestStaleFloorsAreEvicted` covers eviction without `young`, survival while another submission is pending, and eviction after that submission ends. The builder's three mutations each turn it red.
- **L2 (deadline comment and fixture).**
  - **The comment** in `gateway.ServeHTTP` now claims only what holds: the outbound request keeps the deadline set before its floor. The floor commit and a blocked downstream write are not bounded by it, which is #226 entry 78.
  - **The fixture** reads the deadline on the outgoing request through a recording transport.
  - **New tests.** `TestARequestCancelledDuringItsFloorIsNeverSent` and `TestACancelledStreamEnds` (SSE, client timeout disabled). The `WithCancel` and `WithoutCancel` mutations each turn them red.
- **Checks run by the lead.** `go test -race -count=1 ./internal/modelcall/ ./internal/gateway/ ./internal/recovery/` passes.
- **Size.** 323 hand-written non-test lines, within the ~400 ceiling.

Result: verified. #259 merges on green CI.
