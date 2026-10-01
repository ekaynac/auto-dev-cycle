# Review brief for Astra: M0 design notes

You are Astra, an independent reviewer (a different model family from the author, Claude Opus 5.5). This is a
read-only **design review**: do not modify any file.

## Background

The v1 plan is merged (PR #1). It went through your two reviews (plan, knowledge layer), and all findings were applied.
M0 "Contracts & foundations" ships no cycle behaviour. It fixes the contracts every later milestone builds on. Plan
§9: "Independent review of the M0 notes (Astra), then M0 implementation via PRs."

## Files

Under review (all under `/home/mega/auto-dev-cycle/docs`):
- `design/m0/00-overview.md` … `design/m0/11-testing.md` (12 notes)
- `adapters/AUTHORING.md` (draft)

Authoritative inputs the notes must implement faithfully:
- `plan/01-first-release-plan.md` (rev 3): especially §2.2 capacity, §2.3–2.4 adapters and host roles, §2.5 cycle and
  fencing, §2.10 sandbox, §2.11 authority, §2.15, §4 M0 row;
- `plan/00-brainstorm-decisions.md` (D1–D36);
- `plan/02-supervisor-role.md`;
- `plan/03-knowledge-layer.md` (rev 2; authoritative for knowledge semantics);
- your reviews: `plan/reviews/2026-10-01-astra-plan-review.md`, `plan/reviews/2026-10-02-astra-knowledge-review.md`.

Predecessor sources are read-only: `/home/mega/nobetci-dev`, `/home/mega/nobetci`. Do not open credential or env
files.

## Review focus

1. **Faithfulness.** Does each note implement what the plan and decisions say, without contradicting them or quietly
   dropping something? Are any of your applied findings (A1–A15, KA1–KA9) lost or weakened in the contracts?
2. **Contract soundness.** For each schema and state machine:
   - Are the invariants enforceable?
   - Do fail-open paths exist?
   - Is anything decided in two places?
   - Do the four outcomes hold everywhere?

   Look hard at:
   - the event store (single writer, intent-before-egress, idempotency/expected state, hash chain);
   - authority (grants, precedence, proposals, confirmation binding, the protected-document operation, bootstrap);
   - leases, fencing and settlement;
   - receipt outcome derivation and applicability;
   - adapter protocol and fencing;
   - ContextManifest coverage honesty.
3. **M0 scope.** Is M0 minimal and sufficient for M1 to start without redesign? What is missing that M1 needs? What
   is in M0 that belongs later?
4. **Testability.** Are the M0 exit checks runnable by a stranger? Do the fakes and T0 fixtures cover the contracts?
   Does each key test prove its red?
5. **Neutrality and portability.** Hidden assumptions about OS, language, forge or provider.
6. **Security.** Token handling, bootstrap, socket permissions, the confirmation channels (WebAuthn, pager code), the
   adapter blob exchange, host-side harness rules.
7. **Open questions Q1–Q4** in `00-overview.md`: give a recommendation for each.

## Output format

- A one-line verdict: **GO**, **GO with changes**, or **NO-GO** for starting M0 implementation from these notes.
- Findings ordered by severity (**Critical / High / Medium / Low**). For each: exact location (file + section), problem,
  why it matters, and a concrete change.
- Recommendations for Q1–Q4.
- Be terse. Block only on Critical/High.
