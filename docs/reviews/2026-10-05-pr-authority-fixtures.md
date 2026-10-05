# Authority contract fixtures: review notes

This PR carries #87 Astra findings 3 and 4, which moved here with the authority fixtures when #87 was split (the review is in docs/reviews/2026-10-05-pr87-astra.md).

### Lead notes (applied)

- Finding 3 (profile widening tested only one sensitive class): `widened-profile-edit` now also adopts, under `profile.adopt`, a profile adding a destination, one emptying the destination list and one removing a protected document, beside the wider floor and the dropped usage override. Each widening must be proposed under the human policy carrying its class (`policy.widen_data`, `policy.widen_destinations`, `policy.weaken_protection`), and the review GO bound to that proposal is then refused by `ConfirmByReview`; the golden is `empty/completed_clean` then five `failed/confirmation_refused`. Controls: lowering the destination-widening default to independent review in `Decide` (in the working tree, reverted) turns it red with both destination steps confirmed and two policy assertions violated; `authority.inherited-usage-narrows` still turns it red on the dropped usage override.
- Finding 4 (no legitimate supervisor path): `protected-change-widens` asserts that a supervisor's benign protected-document change is `Propose` under `IndependentReview`. A proposal is a disposition, not an outcome (02), so it is an assertion, not a golden step; the golden is unchanged. Controls: refusing every supervisor protected change (in the working tree, reverted) turns it red on that assertion; `authority.protected-check-supervisors-only` still turns it red on the operator and system steps.

## Review of #88 (Astra, 2026-10-05)

Verdict: GO with changes. #87 findings 3 and 4 resolved; no Critical/High.

1. **Medium — No mixed topology-and-widening profile case** (`internal/fixtures/authority.go:91`). A classifier regression that returns only `ChangeTopology` whenever topology changes would pass every fixture and classifier test, letting a supervisor with `profile.adopt` bundle a wider floor with a topology change and get review confirmation. Fix: add one combined edit, assert both classes, `Propose/Human`, and the `ConfirmByReview` refusal.

### Lead notes on #88 round 1 (applied)

- Medium 1: `widened-profile-edit` adds a seventh step, a topology change bundled with a wider data floor. It must be proposed under human policy carrying both `integration.change_topology` and `policy.widen_data`, and its review GO is refused (`failed/confirmation_refused`). The cases now list every class they must carry. The regression above was put in temporarily (`ClassifyProfile` returning only `ChangeTopology` when the topology changes): the fixture failed at that step, proposed under independent review for `[integration.change_topology]`. It was then reverted.
