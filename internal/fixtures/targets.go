// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/profile"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const targetGroup = types.ResourceGroupID("rg_01m3250vm7f6ktw39dszqnyddr")

// restoreFailureKeepsTargetExcluded: an evidence run on a target is
// collected, then the controller crashes before cleanup; the restore then
// fails, and the controller restarts again (13 [G5]; 05 "Evidence-target
// leases"; 11 intake). Across both restarts the run publishes nothing and
// the target stays excluded: a new job on it is refused. Once a restore is
// verified, the run settles with its draft and the target serves again.
func restoreFailureKeepsTargetExcluded(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	reg, err := vocab.Load()
	if err != nil {
		return nil, nil, err
	}
	limit := int64(1)
	r, err := capacity.NewReservations([]types.ResourceGroup{{ID: targetGroup, Kind: types.ResourceGroupKindEvidenceTarget,
		ReleaseRule: types.ResourceGroupReleaseRuleOnRestoreVerified, Members: []types.ExecutorID{},
		CapacityModel: types.ResourceGroupCapacityModel{Unit: types.CapacityUnitSlots, Limit: &limit, QuotaState: types.ResourceGroupCapacityModelQuotaStateKnown}}})
	if err != nil {
		return nil, nil, err
	}
	var s *eventstore.Store
	done := func() {}
	defer func() { done() }()
	restart := func() error { // a fresh controller over the same state
		done()
		var err error
		if s, done, err = storeWith(ctx, dir, lease.Kinds(), capacity.Hook(lease.Projector{Holds: reg.Holds, Units: oneTask{}}, r)); err != nil {
			done = func() {}
			return err
		}
		return do(ctx, s, lease.RecoverAfterRestart)
	}
	if err := restart(); err != nil {
		return nil, nil, err
	}
	run, attempt, err := targetLease()
	next, _, nerr := targetLease()
	if err = errors.Join(err, nerr); err != nil {
		return nil, nil, err
	}
	change := func(id types.LeaseID, e lease.LeaseEvent) error {
		return do(ctx, s, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			_, rev, err := leaseOf(tx, id)
			if err != nil {
				return nil, err
			}
			return lease.ChangeLease(tx, id, rev, e, reg.Holds)
		})
	}
	base := types.AttemptBase{Tree: "git:cccccccccccccccccccccccccccccccccccccccc", ContractRevision: "7"}
	for _, fn := range []func() error{
		func() error {
			return do(ctx, s, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, run) })
		},
		func() error {
			return change(run.ID, lease.LeaseEvent{Kind: lease.Granted, Targets: []types.ResourceGroupID{targetGroup}})
		},
		func() error {
			return do(ctx, s, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.StartAttempt(tx, run.ID, 2, attempt, base)
			})
		},
		func() error { // the evidence is collected; the run stops for cleanup
			return do(ctx, s, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.ChangeAttempt(tx, attempt, 1, lease.AttemptEvent{Kind: lease.StopRequested, StopReason: types.AttemptStopReasonValueDrain})
			})
		},
		func() error { return change(run.ID, lease.LeaseEvent{Kind: lease.Revoked, Reason: "collected"}) },
		func() error { return change(run.ID, lease.LeaseEvent{Kind: lease.RestoreStarted}) },
		restart, // the crash between collection and cleanup
		func() error {
			return do(ctx, s, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, next) })
		},
	} {
		if err := fn(); err != nil {
			return nil, nil, err
		}
	}
	draft := types.ResultID("res_01m3250vm7f6ktw39dszqnyddr")
	settle := lease.LeaseEvent{Kind: lease.Settled, Settlement: &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}, Published: &draft}}
	if change(run.ID, settle) == nil {
		violations = append(violations, "the run published before its restore was verified")
	}
	// The restore runs and fails.
	var targets sandbox.Targets
	targets.Fail(sandboxTarget, "the snapshot is gone")
	rerr := targets.Restore(sandboxTarget)
	if rerr == nil {
		return nil, append(violations, "the scripted restore failure did not fail"), nil
	}
	verified := types.ReceiptID("rcp_01m3250v57fhm9v1z9xd1wk8gw")
	hold := &types.LeaseHoldValue{Hold: lease.RestoreHold, SignalInstance: "restore:" + string(run.ID)}
	if err := change(run.ID, lease.LeaseEvent{Kind: lease.RestoreFailed, Reason: rerr.Error(), Hold: hold}); err != nil {
		return nil, nil, err
	}
	if err := restart(); err != nil {
		return nil, nil, err
	}
	grant := func() (Step, error) {
		err := change(next.ID, lease.LeaseEvent{Kind: lease.Granted, Targets: []types.ResourceGroupID{targetGroup}})
		var full *capacity.Exhausted
		if errors.As(err, &full) {
			return Step{vocab.CouldNotRun, full.Reason.String()}, nil
		}
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}, err
	}
	st, err := grant()
	if steps = append(steps, st); err != nil || st.Outcome == vocab.Empty {
		return steps, violations, err // the target was not excluded
	}
	targets.Heal(sandboxTarget)
	if err := targets.Restore(sandboxTarget); err != nil {
		return nil, nil, err
	}
	if err := change(run.ID, lease.LeaseEvent{Kind: lease.RestoreVerified, Receipt: &verified}); err != nil {
		return nil, nil, err
	}
	if err := change(run.ID, settle); err != nil {
		return nil, nil, err
	}
	st, err = grant()
	return append(steps, st), violations, err
}

const sandboxTarget = types.EvidenceTargetID("tgt_01m3250v0aer9ss2097svmefj8")

// targetLease is a fresh lease of the evidence target's one slot.
func targetLease() (types.Lease, types.AttemptID, error) {
	l, a, err := freshLease()
	l.Reservations = []types.LeaseReservationsItem{{Group: targetGroup, Amount: 1, Unit: types.CapacityUnitSlots}}
	return l, a, err
}

func leaseOf(tx *eventstore.Tx, id types.LeaseID) (types.Lease, int64, error) {
	raw, ok, err := tx.Get(lease.LeaseProjection, string(id))
	var rec struct {
		Record   types.Lease `json:"record"`
		Revision int64       `json:"revision"`
	}
	if err == nil && ok {
		err = json.Unmarshal(raw, &rec)
	}
	return rec.Record, rec.Revision, err
}

// do submits one command whose logic is fn.
func do(ctx context.Context, s *eventstore.Store, fn func(*eventstore.Tx) ([]eventstore.Event, error)) error {
	cmd, err := ids.New("cmd")
	if err != nil {
		return err
	}
	_, err = s.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: "act_01m3250vkwffc979nzjw0g2sf9", Action: "fixture.command", IdempotencyKey: cmd, Input: []byte(`{}`)},
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := fn(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

// factsProfile's tests need VCS metadata; its production copy has none.
const factsProfile = `schema: adc.project-profile/v1
id: prj_01m3250vkwffc979nzjw0g2sf9
name: Example service
repo:
  url: https://github.com/example/service.git
  base_ref: refs/heads/main
forge:
  kind: github
  capabilities: {change_requests: true, protected_branches: true, required_checks: true}
  settings: {}
source_tests: {dev_dependencies: true, vcs_metadata: true}
package:
  recipe:
    - [go, build, -o, dist/service, ./cmd/service]
  outputs: [dist/service]
runtime_verification:
  command: [dist/service, --self-check]
  facts_from: production
toolchain:
  images:
    go: golang@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  dependency_recipe:
    lockfiles: [go.sum]
    command: [go, mod, download]
    registries: [goproxy]
gate:
  command: [go, test, -json, ./...]
  result_adapter: go_test_json
  baseline: {kind: measured, relock_requires: approval}
  environments:
    - name: production
      facts:
        vcs_metadata: false
        network: loopback
        runtime: {language_version: "1.27", os: linux, arch: amd64}
      owner: ops
  ownership_rules: default
integration:
  plugin: docker-compose
  topology:
    files: [deploy/compose.yaml]
    adopted_revision: git:cccccccccccccccccccccccccccccccccccccccc
    digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  receipts_required: [smoke]
plugins: {deploy: docker-compose, records: json_ledger}
protected_paths: [docs/decisions/**, deploy/*.yaml, CANON.md]
protected_documents: [docs/decisions/*.md]
data_policy:
  floor: contracted
  routing: verified
  allowed_destinations: []
usage_profile_override: null
priority: 10
budget:
  rails: {per_task_usd: "5", per_day_usd: "50.00"}
retention:
  attempt_continuity_post_closure_ttl: P30D
  supervisor_notes_ttl: null
  blob_payload_ttl: P90D
vocabulary:
  task_id_pattern: '^SVC-[0-9]+$'
  branch_prefix: adc/
  language: en
`

// gateAndRuntimeSeeTheirFacts: a gate on the source-test workspace and a
// runtime verification on the package artifact each run in a fake sandbox
// with their declared facts (13 [G8]; 07). The tests read VCS metadata, which
// the source-test workspace declares; the service's self-check reads it
// too, which the deployed package lacks: the gate passes and runtime
// verification finds the target-only failure before merge.
func gateAndRuntimeSeeTheirFacts(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	p, err := profile.Load([]byte(factsProfile))
	if err != nil {
		return nil, nil, err
	}
	on := func(kind types.NodeKind, rk types.ResultKind) types.Node {
		return types.Node{ID: "nod_01m3250v57fhm9v1z9xd1wk8gw", Kind: kind, Inputs: []types.InputSpec{{Name: "subject",
			Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: "nod_01m3250v57ft7aehkqwq0yrq3n", ResultKind: rk}}}}}
	}
	for _, n := range []types.Node{on(types.NodeKindGate, types.ResultKindTree), on(types.NodeKindRuntimeVerify, types.ResultKindPackage)} {
		declared, err := graph.DeclaredFacts(n, p)
		if err != nil {
			return nil, nil, err
		}
		seen := sandbox.Prepare(declared).Observe()
		if seen.Workspace != declared.Workspace || seen.VCSMetadata != declared.VCSMetadata || seen.DevDependencies != declared.DevDependencies ||
			seen.Network != declared.Network || (seen.Runtime == nil) != (declared.Runtime == nil) || seen.Runtime != nil && *seen.Runtime != *declared.Runtime {
			violations = append(violations, string(n.Kind)+" does not see its declared facts")
		}
		if seen.VCSMetadata { // the check reads .git
			steps = append(steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
		} else {
			steps = append(steps, Step{vocab.Found, vocab.ReasonCompletedWithFindings.String()})
		}
	}
	return steps, violations, nil
}
