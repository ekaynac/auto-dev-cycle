## Lead verification of #273 (2026-10-15)

Astra's external review of 75d9018 was GO with changes. Under D64 the lead applies and verifies the changes, and no second external round follows.

1. **P14d after P14e.** P14d now needs P14e-2c, as #226 entries 79 and 82 and #268 place it. The critical path reads `P14e-1 → P14e-2a → P14e-2b → P14e-2b2 → P14e-2c → P14d`, before P22a.
2. **The Python gates are qualified.**
   - The "Gate" sentences of P14c-2a-allow, P14c-2a2 and P14c-2a3 now say "a prerequisite of P22a's Python-profile activation only", which is operator answer "python gate".
   - The overview row "Build → accept → gate → qualified independent review" lists the P14e chain, and it lists P14c-1 to P14c-2b for a Python profile only.
   - P14c-2b's sentence ("P22a does not activate a Python profile before it lands") is kept unchanged.
3. **The Python chain is two branches** that join at P14c-2b:
   - P14c-2a-cond → P14c-2a-shadow → P14c-2a-reach → P14c-2a-widen;
   - P14c-2a2 → P14c-2a3.

   The text now matches the dependency table.
4. **Sizes as added plus deleted (04 §2):** P14c-2a-cond 197, and P14c-2a-shadow 687.

The lead re-read the four changed passages, and checked the graph again: it has no cycle.

**Approval:** the operator's answers of 2026-10-15, "loader b" and "python gate", given in the lead session at about 10:40 (UTC+3).

Result: verified. #273 merges on green CI.
