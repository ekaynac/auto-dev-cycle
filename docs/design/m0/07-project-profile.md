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
  settings: {}                      # per forge kind; reserved in v1 (an empty object) until a forge needs settings
source_tests:                       # the gate's workspace (13 "Source tests, package, runtime verification") [G8]
  dev_dependencies: true            # test tools and dev deps installed (npm devDependencies, composer require-dev, …)
  vcs_metadata: false               # true only if the test suite needs it (declared build-time fact)
package:                            # the deployable artifact; its bytes are hashed and exactly those bytes are deployed
  recipe: [argv]                    # e.g. `npm ci --omit=dev && npm run build`, `go build -o …`, `composer install --no-dev`
  outputs: [path]
runtime_verification:               # runtime-facing checks against the package artifact, with the target's runtime facts
  command: [argv]|null              # e.g. smoke tests, health checks, migration dry run
  facts_from: <environment name>    # gate.environments entry whose runtime facts apply
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
required_config:                    # (with the merge gate; not in the schema yet) intake B2; keys only, never values
  source: none | file               # a generated or command source is v1.x: its dependency closure cannot yet be confined
  file: path|null                   # file: a repo file listing the required keys (a JSON array of strings); protected
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

## Required configuration (intake B2)

A product that needs new deploy-time configuration says so in its own tree, and the merge asks the operator before
it deploys:
- **Declared, product-neutral, static.** `required_config` names a file in which the product lists its required
  keys. It is read as data, never executed, so nothing outside the file can change the list. Only key names are
  read, never values. A source computed by a command is deferred to v1.x, until its whole dependency closure can be
  confined.
- **The mechanism is trusted, the content is diffed.** The declaration mechanism (the field above) is the adopted
  profile's at the merge's commit, never the candidate's, and it is applied to both the pinned base and the
  prospective merge tree. Its `file` is a **protected path**, so a candidate cannot shrink its own inventory
  silently: any change to it (adding, removing, moving or deleting) is a **declaration change** that the operator
  answers (below).
- **Completeness is a review obligation.** No list proves that it names every key the code reads. The independent
  review of a candidate must check that configuration the diff newly reads appears in the candidate's declaration;
  an undeclared read is a blocking finding.
- **One operator question per generation.** A declaration change holds the merge on a hold **owned by the
  operator**, resumed by an approval. Only an **operator** actor's answering approval resolves it; a supervisor's
  `input.answer` grant does not discharge it. The question carries two parts:
  - **readiness**, for each added key: the operator states that every target environment provides it;
  - **integrity**, for the change itself, including removed keys: the operator accepts the new declaration.

  Removed keys need no readiness answer but are still part of the integrity answer. When a later generation's
  recomputation no longer has a key or change, the obsolete part of the question is retired, and the rest remains.
  The hold joins the registry (02) with the gate's implementation.
- **A merge input, bound and revalidated.** The comparison and its answer are inputs of the merge node in the queue
  generation (13). The answer binds the pinned base, the prospective tree, the declaration mechanism and its digest,
  the added-key set and the target environments. A new generation (base moved), a profile adoption or a target
  change recomputes the comparison, and an answer whose binding no longer matches does not apply. The merge
  effect revalidates it at commit time, like carry-over (13), under the mechanism adopted at that moment.
- **Never found after deploy.** A post-deploy verifier that discovers a missing key is too late; the diff is the rule.
- **Reading the declaration.** Whatever is not accepted below is `could_not_run` and keeps the merge held, never an
  empty list. It resumes when a later generation reads a declaration that is accepted.
  - **From the tree, by exact path.** `file` is an exact, repository-relative path. The declaration is the bytes of
    the **regular blob** at that path in each pinned Git tree, read from the object store, never from a working
    tree. It is read in the base and in the prospective tree.
  - **Entries.** Each parent component must be a directory (a Git tree entry), and the final entry a regular-file
    blob. A symlink or a submodule (gitlink) in either position is not accepted, so a reader never follows a link to
    an unprotected file. Attributes and filters (text conversion, LFS) are never applied: the committed blob is the
    declaration.
  - **One identity, case-sensitive.** The path compares byte for byte with no case folding, exactly as the
    protected-path matcher compares it, so the object read and the object protected are the same. A tree that also
    holds a path differing only by case from the declaration, or from one of its parents, is not accepted, because
    a case-insensitive checkout could resolve either.
  - **Shape, within bounds.** Accepted content is UTF-8 JSON: an array of unique, non-empty strings, each a
    configuration key. `null`, an object, a scalar, a non-string element and a duplicate are not accepted. The
    reader is bounded before it decodes:
    - at most 64 KiB of blob, checked from the object's size before reading;
    - no value nested inside the array;
    - at most 1,024 keys of at most 256 bytes each.

    Exceeding a bound is not accepted, without exhausting the reader. Only an accepted array reaches the comparison.
  - **First introduction.** A profile may name a declaration only if the file already exists, accepted, at the base
    branch's head. So a product introduces an empty or current declaration first, then adopts it.
    - Adoption validation records the base commit it read and the declaration's blob identity as preconditions of
      the adoption. Applying the adoption re-checks them, serialized with the merge queue of that base branch.
    - If the head moved, the declaration is revalidated at the new head, and adoption is refused if it no longer
      exists, accepted.
    - A pinned base that lacks a named declaration afterwards is unexpected and is not accepted.
- **Weakening the mechanism is weakening protection.** A profile adoption that disables the check (`source: none`),
  points it at another file or removes the file from the protected paths is classified `policy.weaken_protection`
  (04), so it needs a presence-verified human whoever proposes it. Its approval accepts the new mechanism, and it
  never substitutes for a pending configuration answer. A candidate merged while the mechanism changes is checked
  under the mechanism adopted at commit, applied to both trees: a stricter new mechanism can only add questions.

The profile field, the hold and the gate check land together, with the merge gate.

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
