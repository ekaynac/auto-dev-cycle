# 07 — Project profile (and reserved config schemas)

The profile is everything project-shaped. Core code reads nothing project-specific from anywhere else. Unknown keys
are refused. A project with no valid profile does not run (`could_not_run/project_unconfigured`).

## Location and authority

- The profile lives in the controller (registered with `adc project add --profile <file>`). Every change is a
  revision-bound action with an approval record. The adopted revision's digest is recorded.
- Authority classifies a profile change by its **effective change** (04): a revision that widens data policy or
  destinations, weakens protection or allows paid fallback is confirmed as that class, whatever the command was
  called [M3].
- Optionally a project repo may carry `.adc/profile.yaml` as the human-edited source. Adopting it is the same approval
  action. The controller never reads it implicitly at run time.

## Schema (summary; full JSON Schema in `schemas/config/project-profile.json`)

```yaml
schema: adc.project-profile/v1
id: prj_…
name: string
repo:
  url: string                       # any git URL
  base_ref: refs/heads/main         # parameterised, never assumed (A15)
forge:
  kind: git | github | gitlab
  capabilities:                     # declared, because plain git has none of them
    change_requests: bool
    protected_branches: bool
    required_checks: bool
  settings: {…}                     # per forge kind (schema per kind)
package:                            # the ship tree the gate runs on (13 "Test what you ship") [D40]
  recipe: [argv]|null               # build steps producing the deployable tree; default: export without VCS metadata
  includes_vcs_metadata: false      # tests needing VCS metadata, network etc. must declare it here as a fact
toolchain:
  images: {name: image_ref@digest}  # pinned by digest
  dependency_recipe:                # phase 1 of the sandbox (plan §2.10)
    lockfiles: [path]
    command: [argv]
    registries: [npm|pypi|packagist|goproxy|…]
gate:
  command: [argv]                   # run in phase 2, no network
  result_adapter: junit_xml | tap | go_test_json | custom:<plugin> | none
  baseline: {kind: measured, relock_requires: approval}
  environments:                     # target environments whose gate must hold before merge
    - name: string
      facts:                        # published to the builder brief and the reviewer (nöbetçi Δ§7)
        vcs_metadata: bool          # e.g. false for a deployed copy without .git
        network: none|loopback|full
        runtime: {language_version, os, arch}
      owner: string                 # who fixes environment faults (wrong_owner holds)
  ownership_rules: default          # gate-output classification (nöbetçi Δ§7): an ERROR is candidate-owned (builder
                                    # repair) only if ALL hold: a test node in a file the candidate added or changed vs
                                    # the merge-base; setup/teardown section; traceback passes through that file; NOT an
                                    # import error raised outside the changed files [PR5-9]; the error count is fully
                                    # accounted. Collection errors, untouched files, count mismatches, external import
                                    # errors and an unknown base are runner/environment faults (a hold)
integration:                        # phase 3; topology is controller-owned (A2)
  plugin: none | docker-compose
  topology:                         # pinned; changed only by integration.change_topology (04) [M12]
    files: [path]                   # protected
    adopted_revision: …
    digest: sha256:…                # of the adopted topology content
  receipts_required: [name]
plugins:
  deploy: none | script | docker-compose
  records: none | markdown_table | json_ledger   # delivery record format plugin
protected_paths: [glob]             # canon/topology/knowledge docs; changed only via the protected-document operation
protected_documents: [glob]         # subset that holds decisions/conventions (03-knowledge-layer K8)
data_policy:
  floor: own | contracted | free_unknown
  routing: verified | reported_ok
  allowed_destinations: [destination_id]   # empty = derive from usage profile
usage_profile_override: null | standard | open | on-prem   # may only narrow the installation profile without approval
priority: int
budget:
  rails: {per_task_usd, per_day_usd}        # rails, not budgets (nöbetçi CANON §3)
retention:                          # [M14], 03-knowledge-layer §10.3
  attempt_continuity_post_closure_ttl: duration   # starts when the task closes; pinned records (resumable) are never purged
  supervisor_notes_ttl: duration|null             # supervisor continuity is grant/project-scoped (02-supervisor-role §5)
  blob_payload_ttl: duration|null                 # payload purge leaves tombstones; accounting/history are never purged
vocabulary:
  task_id_pattern: regex|null       # e.g. product ledger grammar; default = framework ids
  branch_prefix: string             # e.g. "adc/"
  language: string                  # product language(s); framework vocabulary stays English
```

## Reserved schemas (defined in M0, filled in M2)

- `schemas/config/usage-profiles.json`: the three built-in profiles (`standard`, `open`, `on-prem`, D28), each with its
  recorded intention text. **Every usage setting must have a default per profile.** A schema test fails if a setting
  lacks one.
- `schemas/config/source-registry.json`: sources, executors, resource groups, destinations (with retention terms), and
  role preference/fallback chains with `paid_allowed` flags (D11).

## Validation

`adc project validate <file>` runs the schema, then semantic checks:
- images pinned by digest;
- protected globs compile;
- the base ref exists;
- the forge capabilities match the forge kind;
- the gate command is not empty;
- the data policy is within the installation's usage profile.

Validation runs before adoption, never after (nöbetçi lesson 1.5).
