# T0 fixture repositories

Tiny projects the cycle is tested on (`docs/design/m0/11-testing.md` "T0 fixtures", plan §3). M0 defines them and
proves each task is well-formed. They run through the cycle for real from M1.

| Fixture | Language | Gate | Result adapter |
|---|---|---|---|
| `py-mini` | Python 3.12 | `pytest --junitxml` | `junit_xml` |
| `ts-mini` | TypeScript on Node 22 (type stripping, no dependencies) | `node --test --test-reporter=junit` | `junit_xml` |
| `go-mini` | Go | `go test -json` | `go_test_json` |
| `php-mini` | PHP 8.4 + PHPUnit | `phpunit --log-junit` | `junit_xml` |

`multi-service` (two services, a database and a bus) is added with the integration environment (M1).

## Layout (provisional format; M1 turns it into a schema when the cycle consumes it)

```
fixtures/t0/<name>/
  fixture.json            name, language, toolchain, dependency recipe, gate command, result adapter, results path
  repo/                   the project at its base state (committed as the fixture repo's first commit)
  tasks/<task-id>/
    task.json             the task (below)
    hidden/…              hidden acceptance tests, at the paths they take inside repo/
    visible/…             failing_then_green only: the failing visible test, copied into repo/ for this task
    reference.patch       a known-good solution: a `git diff` against repo/ (applies with `git apply`)
```

`task.json`:

```json
{
  "id": "py-mini-feature-1",
  "kind": "feature | bugfix | refactor | failing_then_green",
  "title": "one line",
  "requirement": "what the planner and builder are told, in plain English; never names the hidden tests",
  "acceptance": {"hidden_tests": ["tests/test_hidden_x.py"], "visible_tests_added": [], "visible_tests_must_pass": true},
  "expected": {"base_visible": "pass | fail", "base_hidden": "fail | pass", "after_all": "pass"}
}
```

## Well-formedness (checked when a fixture is added)

For every task, starting from a fresh copy of `repo/`:
0. `repo/` is green for every task. A failing visible test would turn every other task red, so a
   `failing_then_green` task ships its failing test in `tasks/<id>/visible/`, lists it in
   `acceptance.visible_tests_added`, and copies it into the repo before its base run.
1. **Base:** the visible tests give `expected.base_visible` (only `failing_then_green` starts with a failing visible
   test). With the hidden tests copied in, they give `expected.base_hidden`. This is `fail` for feature, bugfix and
   failing-then-green, so the hidden tests catch the missing behaviour. For refactor it is `fail` too, because its
   hidden tests check the new structure while the old behaviour keeps passing.
2. **After:** `git apply reference.patch`, copy the hidden tests in, and run the gate: everything passes.
3. **The patch never edits a test file.** Tasks are solved in code, never by changing tests (integrity guard, plan
   §2.5).
4. The gate runs **offline** after the dependency recipe, and emits per-test results in the declared adapter format.

Each fixture's `check.sh` runs 0–4 for all its tasks and exits non-zero on any deviation. Language tooling run from
the repository root must not pick up fixture files; each fixture fences itself off (e.g. go-mini's `tasks/go.mod`).
