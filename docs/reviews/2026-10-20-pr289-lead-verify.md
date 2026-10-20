## Lead verification of #289 after Astra "GO with changes" (2026-10-20)

Head 98b1ffc. Astra (`2026-10-19-pr289-astra.md`) gave GO with changes. It found no High and no regression against main. Its findings:
- **M1:** the node cap was checked after the queue was allocated;
- **M2:** an ancestor that is also an input was counted twice;
- **L3:** the resolver's bounds;
- **L4:** no end-to-end `Load` test.

Per D64, the lead verifies the changes, with no further external round.

**Checks**
- **The diff since ab0a7ce:**
  - `schedule` counts each new id against the inclusive record bound BEFORE it enters `walked` or the queue (M1).
  - A `resolved` cache spans the intent's inputs. An input already walked as an ancestor is compared with its cached record (digest and lineage), and is not charged or expanded again (M2).
  - The acceptance check stays in `resolve`, on the id and on the content digest, so the cache cannot skip it.
  - `graph.Records` checks aggregate bounds (1M lineage entries, 64 MiB of ids) before any lineage is cloned (L3). The whole-result decode stays a P22a residual.
  - `TestIntentLoad` runs on a real store for both roles (L4).
- **The builder's evidence:** 23 mutations, each one caught.
- **The lead's own probe** (throwaway, deleted):
  1. The chain `head → leaf → acceptance`, supplied as `[leaf, head]` and as `[head, leaf]`, is refused with `ErrAcceptanceInput` in both orders, for both roles. The cache does not hide the acceptance ancestor.
  2. A diamond (`c → a, b`; `b → a`), supplied as `[c, b]`, is accepted with exactly one lookup per id.
  3. A duplicate input is refused by the manifest itself ("names input … twice"). This is by design.
- **`-race`:** `go test -count=1 -race ./internal/intent/... ./internal/graph/...` and `TestIntentLoad` pass.

**Verdict:** GO.

**Residual (stated in the PR body and in #226 entry 91):**
- the production calls, the fold enforcement and the reviewer-only declassification are P22a's;
- the whole-result decode;
- a builder may receive its own prior output.
