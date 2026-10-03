# M0 design notes — overview

Status: revision 2, 2026-10-02. Astra reviewed revision 1 (`reviews/2026-10-02-astra-m0-review.md`, GO with
changes); every finding is applied by operator ruling D37 and cited as `[M#]` in the notes. Branch
`feature/m0/design-notes`.

M0 is "Contracts & foundations" (plan §4). It ships no cycle behaviour. It fixes the **contracts every later
milestone builds on**, so that no decision is made twice (the nöbetçi failure class "one decision, two places") and so
that every later milestone has fakes to test against.

Inputs: `docs/plan/01-first-release-plan.md` (rev 3), `00-brainstorm-decisions.md` (D1–D36),
`02-supervisor-role.md`, `03-knowledge-layer.md` (rev 2), both Astra reviews.

## The notes

| # | Note | Fixes |
|---|---|---|
| 01 | `01-repo-layout-and-toolchain.md` | Repo tree, languages, build, CI, how schemas generate code |
| 02 | `02-vocabulary.md` | Outcome enum, reason-code registry, hold reasons with resume signals, id and time formats |
| 03 | `03-event-store.md` | SQLite event log, projections, single-writer rule, hash chain, migrations |
| 04 | `04-authority.md` | Actors, tokens, grants, approval records, proposals, confirmation policies, the protected-document operation |
| 05 | `05-leases-attempts-fencing.md` | Resource groups, leases, attempts, fencing tokens, usage settlement |
| 06 | `06-receipts.md` | The receipt envelope, instruments, inner predicates, applicability |
| 07 | `07-project-profile.md` | Project profile schema; usage-profile and source-registry config reserved |
| 08 | `08-controller-api-and-cli.md` | API shape, transports, events stream, CLI rules, dashboard parity |
| 09 | `09-executor-adapter-contract.md` | JSON-RPC protocol between the Go core and TS adapters; conformance kit. Draft of `docs/adapters/AUTHORING.md` |
| 10 | `10-context-manifest-and-knowledge-spec.md` | ContextManifest, refs and fingerprints, the shared knowledge spec draft (schemas, invariants, fixture definitions only) |
| 11 | `11-testing.md` | Test guard, contract-faithful fakes, T0 fixtures, mutation ("red-proof") rule |
| 12 | `12-evidence-jobs.md` | Evidence-job lifecycle: states, attempts by demand, preconditions, restore, receipt content policy, remote processes (added 2026-10-03 from the nöbetçi delta intake) |

## Cross-cutting rules (apply to every note)

1. **One source per contract.** Every persisted or exchanged shape is a JSON Schema file under `schemas/`. Go and
   TypeScript types are generated from it. CI fails on drift. Prompts that ask a model for structured output are
   generated from the same schema (nöbetçi lesson 1.3).
2. **Four outcomes, never booleans.** Every result carries an `outcome` plus a registered `reason` (02).
3. **Append-only truth.** State is events. Projections are rebuildable caches. Nothing is updated in place except a
   projection (03).
4. **Validation before any irreversible write.** Every command validates fully, then appends, then has effects
   (nöbetçi lesson 1.5).
5. **English only, schema-declared vocabulary.**
6. **No project literals in core.** Anything project-shaped comes from a profile (07).
7. **Refuse to run when unconfigured.**

## M0 exit check [M15]

Run against **installed artifacts** in a clean sandboxed prefix, never the source tree.

| Check | Command | Expected |
|---|---|---|
| CI green | `make ci` | lint, schema-drift check, sandboxed tests, contract fixtures, dist build, install check all pass |
| Clean install | `make dist && scripts/install-check.sh <prefix>` | `adcd`, `adc`, `adc-agent` install into an empty prefix; `adc version` reports the schema-set digest |
| Contract fixtures | `adc dev fixtures --contract` (installed binary) | every M0 contract fixture (11) yields its golden `(outcome, reason)` sequence through the **real** validators; each fixture's named mutation turns it red (checked in CI) |
| Containment | `make test-sandbox-selftest` | sentinel writes, credential env vars and network all fail inside the test sandbox |
| Schemas documented | `make schema-docs` | `docs/reference/schemas/*.md` regenerated with no diff |
| Vocabulary closed | `adc dev vocab check` | every reason code used is registered; every hold has a resume signal; every action has a confirmation default |
| Log integrity | `adc dev verify-log` on a fixture store | edit and truncation detected against the trusted head |

Prerequisites on the build host:
- Go toolchain (not installed on the controller host today; installed per user, see 01);
- Node 22 + pnpm (present);
- bubblewrap (or Docker/Podman as the sandbox fallback; Docker is present).

SQLite is not required (pure-Go driver).

## Resolved questions (D37, following Astra's recommendations)

| # | Question | Decision | Note |
|---|---|---|---|
| Q1 | Go module path | `github.com/ekaynac/auto-dev-cycle`. A rename at launch is an import and distribution migration, planned in M8 | 01 |
| Q2 | Id format | standard TypeID (UUIDv7 suffix). Event `seq` is the authoritative order | 02 |
| Q3 | Controller API transports | owner-only Unix socket by default; an explicit, separate shared socket for a supervisor under another OS user; verified TLS for remote clients | 08 |
| Q4 | Event hash chain | yes, with canonical encoding, genesis, full verification at startup and a trusted head in backups | 03 |

## Operator rulings specific to M0 (D37)

- Human confirmation is **WebAuthn on the dashboard only**. The pager notifies and links; it never confirms.
- Tests run **inside a sandbox** (bubblewrap, or a rootless container) where the real home does not exist.
