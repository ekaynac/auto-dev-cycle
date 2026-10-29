# auto-dev-cycle: an attempt at an unattended development cycle (stopped)

auto-dev-cycle (ADC) was an attempt to build an unattended software-development cycle. The cycle was to take a
requirement, build a change with a model, check it with gates and an independent review, and deliver it to a git
repository with no human in the loop. It was to use model capacity from many sources: free tiers, subscriptions,
paid APIs and local GPUs.

## Status

**The project is stopped (2026-10-29) and archived.** No new work starts on it. Issues and pull requests are not
answered. The closing note is [`docs/closing/2026-10-29-closing-note.md`](docs/closing/2026-10-29-closing-note.md).

## What it tried to be

- A controller (`adcd`) and a CLI (`adc`) in Go. They place work on an *executor* (a harness, a model source and a
  model), and keep a record of each attempt.
- A capacity layer: a source registry, a scheduler with reservations, and a model gateway that checks destinations
  and counts usage.
- Gates that run tests in a sandbox, bind each result to an exact tree, and refuse when evidence is missing.
- Independent review before any merge.
- Project-agnostic: any git repository, any language.

The plan and the design are in [`docs/plan/`](docs/plan/) and [`docs/design/`](docs/design/). The handoff that started
the project is in [`docs/handoff/`](docs/handoff/). They are kept as the record. This README does not repeat them.

## What we learned: our mistakes, honestly

1. **We built a static pipeline, not a capable loop.** ADC fixed the steps (plan, build, gate, review, deliver) and
   gave each step a narrow contract. In stage 2, plain agent loops did better on the same items. Locally,
   mini-swe-agent accepted 13 of 16 and OpenHands 11 of 16. ADC's own pipeline closed 0 of 3, and was about 14 times
   slower per accepted result. A loop that lets a capable model choose its next step was the better base.
2. **Process and review cost grew larger than the system it replaced.** ADC has about 198,000 lines of Go and 507
   review records in [`docs/reviews/`](docs/reviews/). Most pull requests had an internal review and one or more
   external review rounds. Reviewer quotas stopped the work 4 times in 3 days. After about 1,365 commits, ADC did not
   reach unattended operation; milestone M2 was not started.
3. **We fixed instances where the class was the problem.** Reviews found one instance; the fix closed that
   instance; the next round found the next one. Examples: an index walk fixed for one hop only; cleanup inputs fixed
   handle by handle; shell isolation fixed variable by variable for three rounds; a fix round that wrote new copies
   of rules the repository already had. A bash driver's "never prints" promise took 9 internal and 5 external review
   rounds. The rule we learned: find the class, fix it once at the boundary, and use a typed language with tests for
   any promise of completeness.
4. **We did work per language.** The Go gate has its own test selection, its own file rules and its own hidden-test
   overlay (`internal/gosel/`, `internal/gate/acceptance.go`). Each Go detail (build tags, examples, imports) became
   new code and new findings. The later referee is language-agnostic and was proven on Go, Python, TypeScript and
   PHP with much less code.
5. **The local model stalled on capacity.** In stage 2 arm A, ADC on a local 27B model closed 0 of 3 items. Each
   build asked for more output than the context allows (prompt plus requested output was above 262,144 tokens). The
   scheduler reserved worst-case liability, so the builder stalled for the rest of the hour with no visible hold.
6. **Live runs failed because of configuration.** Checkpoint (a) needed 5 live runs before ADC closed one issue as a
   reviewed candidate (15.3 minutes on free cloud models). Earlier runs failed on route, model and driver
   configuration, not on the code under test; one configured model made no change in two live runs. The earlier
   system had the same class: a changed configuration hash failed a review, and a new script version silently
   orphaned a grant and lost 12 hours ([`docs/handoff/02-lessons-incidents-invariants.md`](docs/handoff/02-lessons-incidents-invariants.md)).
7. **Other lessons the records show:**
   - A test peer that runs in-process can hide faults that only the real container reaches. A path that only the
     real environment reaches needs a live end-to-end row before it is called done.
   - Many review rounds checked components, but none asked if the task's acceptance reached the gate at all. Trace
     each assertion from the operator's input to the check that uses it.
   - A heuristic added to fix "counted but never run" refused tests that do run. Under a fail-closed boundary, a rule
     must be a proof.
   - A rule moved into a shared package lost its old caller's timeout, and hung the gate.
   - On a shared host, one agent's `pkill` on a shared prefix killed other agents' runs. Kill only your own PIDs, and
     remove only what you created.
   - A reviewer can grade a fault low because it cannot build a real input. The severity class decides, not
     reachability by a probe.
   - Quota exhaustion and missing results were read as success in the earlier system. Every result needs an explicit
     outcome; there is no boolean "ok".

## What stage 2 measured

| Test | Result | Verdict |
|---|---|---|
| T1, mini-swe-agent, local against cloud | 83 % of the cloud reference (rule ≥ 60 %) | pass |
| T1, OpenHands, local against cloud | 90 % of the cloud reference; text-only stops (#5510) 3 of 16 | acceptance pass; #5510 **open** (no rerun before the stop) |
| T1, arm A (ADC itself), local | 0 of 3 items closed | — |
| T2, gateway | front filter + agentgateway 9/9 checks | adopted; ADC's gateway is not extracted |
| T3, memory | works with no human; did not improve results | part 1 pass, part 2 fail |
| T4, cheap decider (rules, or a local 27B model) | 0 % of test rounds auto-decided at every bar from 80 % to 95 % (AUC 0.55) | fail |
| T5, supervisor baseline | 5 of 5 goals done; not on the same jobs as the arms | closed |

The full report, with sources and caveats: [`docs/closing/2026-10-29-stage2-report.md`](docs/closing/2026-10-29-stage2-report.md).

## What is worth reusing

| Part | Paths | How to use it |
|---|---|---|
| **The referee**, `adc dev referee` | `internal/referee/`, `internal/cli/referee.go`; schema [`schemas/testing/referee-item.json`](schemas/testing/referee-item.json), doc [`docs/reference/schemas/testing-referee-item.md`](docs/reference/schemas/testing-referee-item.md); 16 example items in `fixtures/m1/referee-items/` | It judges any agent's patch against hidden tests, in a pinned container (Podman). Write one item JSON per task: repository, base, image, test command, results format, `editable`, `protected`, hidden acceptance and `hidden_tests`. Run `bin/adc dev referee --item ITEM.json --patch PATCH.diff --out VERDICT.json`. Exit 0 is accepted, 1 is rejected or an invalid item, 2 is `could_not_run` (an infrastructure fault: judge again). Check each item first: the reference patch gives `accepted`, an empty patch gives `rejected`. Audit every accepted patch for harness markers: code inside the test process can change the result |
| **The hidden-test overlay** | language-agnostic: `internal/referee/work.go`; Go gate: `internal/gate/acceptance.go` with `internal/gosel/` | The agent never sees the hidden tests; they are laid over the tree only at judgement, and a patch that touches their paths is refused. Use the referee's version for new work. The Go version shows how to refuse an acceptance without leaking its paths or names into logs |
| **The hostile-repository cases** | `fixtures/m1/cases/hostile-*/` (7 cases); stub scenarios `fixtures/m1/stub-provider/scenarios/hostile-*.json`; controls `internal/fixtures/cycle_hostile_test.go`; schema `schemas/testing/cycle-case.json` | Run them against any worker sandbox. They cover injected agent-instruction files, git-directory tricks (`.gitattributes`, `.gitmodules`), symlink escapes, a test that probes its environment, gate output that tries to steer the agent, and a request to switch models. Each case states the expected refusal |
| **The gateway rules** (rules, not the service) | `internal/gateway/` (`egress.go`, `secrets.go`, `count.go`); `internal/gateway/hostile_test.go`; `internal/gateway/testdata/mutations.py` | Use them as a checklist for any model gateway: pin the exact model and re-encode the body; refuse redirects and dial only the registered origin; redact echoed credentials, also JSON-escaped and split across writes; count usage from the stream. Port the tests as cases, not the code. Known gaps in agentgateway to configure: budgets overshoot under concurrency, and revoking a key does not cut an in-flight stream |
| **The review records, as data** | [`docs/reviews/`](docs/reviews/) (507 files) | Each record is one review round of one pull request, with severities and findings. Stage 2 extracted 629 rounds (354 accept, 275 repair) as labels for T4. Use them to train or test a review decider, or to study which fault classes recur |

## Where the work continues

The work continues in [`ekaynac/atolye`](https://github.com/ekaynac/atolye). The successor builds a stack centred on
OpenHands, with a model gateway in front, and uses the referee's item format for its acceptance sets.

## Build and test

These instructions still work on Linux. Requirements: `curl`, `jq`, `tar`, `sha256sum`, `git`, GNU make, bubblewrap,
and Podman for the referee. No system Go is needed.

```sh
make toolchain   # installs the Go toolchain pinned in go.mod into ~/.local/go (checksum-verified)
make build       # bin/adcd, bin/adc, bin/adc-harness, bin/adc-agent
make test        # all tests, inside the test sandbox (bubblewrap)
make ci          # everything CI runs
```

`make help` lists every target.

## Licence

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
