// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// railsRegistry is the stub builder at $1 and $4 per million input and
// output tokens, and its reviewer, in a 2,000,000-token quota
// (fixtures/m1/config's); each attempt's prior is a 1,000-token turn, 2
// turns: a 2,000-token attempt_cap, one turn at least (#226 entries 103,
// 120).
const (
	railsRegistry = `{"schema":"adc.executor-registry/v1","executors":[{"id":"exe_01m4tm99haemqt8es2qfbwj8pa","role":"builder","family":"stub-builder",` +
		`"adapter":"http-chat","model":"stub-builder-1","destination":{"id":"stub","kind":"direct_api","verification":"verified","trust_class":"contracted",` +
		`"endpoint":"http://127.0.0.1:18080/v1","retention_days":0},"credential_ref":"stub","group":"rg_01m4tm99haemss9tvzd438vq8j","cost_basis":"cash",` +
		`"price":{"input_usd_micros_per_mtok":1000000,"output_usd_micros_per_mtok":4000000},"footprint_prior":{"context_tokens":1000,"expected_turns":2}},` +
		`{"id":"exe_01m4tm99haemr8ajyf09v4ytzb","role":"reviewer","family":"stub-reviewer","adapter":"http-chat","model":"stub-reviewer-1",` +
		`"destination":{"id":"stub","kind":"direct_api","verification":"verified","trust_class":"contracted","endpoint":"http://127.0.0.1:18080/v1","retention_days":0},` +
		`"credential_ref":"stub","group":"rg_01m4tm99haemss9tvzd438vq8j","cost_basis":"cash","price":{"input_usd_micros_per_mtok":2000000,"output_usd_micros_per_mtok":8000000},"footprint_prior":{"context_tokens":1000,"expected_turns":2}}]}`
	railsGroups = `[{"id":"rg_01m4tm99haemss9tvzd438vq8j","kind":"account_quota","capacity_model":{"unit":"tokens","limit":2000000,"window":null,` +
		`"reset_at":null,"quota_state":"known"},"release_rule":"on_settle","members":["exe_01m4tm99haemqt8es2qfbwj8pa","exe_01m4tm99haemr8ajyf09v4ytzb"]}]`
	// railsProfile is go-mini's profile, its per_task_usd $0.01 (10,000
	// micros): an unconstrained lease reserves 2,000 tokens, 8,000 micros at
	// the builder's $4; the 2,000 micros it leaves buy 500 tokens, less than
	// a turn.
	railsProfile = `{"schema":"adc.project-profile/v1","id":"prj_01m3250vkwffc979nzjw0g2sf9","name":"rails-usd","repo":{"url":"/var/lib/adc/remotes/go-mini.git","base_ref":"refs/heads/main"},"forge":{"kind":"git","capabilities":{"change_requests":false,"protected_branches":false,"required_checks":false},"settings":{}},"source_tests":{"dev_dependencies":true,"vcs_metadata":false},"package":{"recipe":[["go","build","-trimpath","-o","bin/","./..."]],"outputs":["bin"]},"runtime_verification":{"command":null,"facts_from":null},"toolchain":{"images":{"go":"ghcr.io/ekaynac/adc-prepared-go@sha256:4e8cf51b55c9115963f1b9485f6261e66dbbd5aa631b0e5142d13eec04f320c7"},"dependency_recipe":{"lockfiles":["go.mod"],"command":["go","mod","download"],"registries":["goproxy"]}},"gate":{"command":["go","test","-count=1","-json","./..."],"result_adapter":"go_test_json","baseline":{"kind":"measured","relock_requires":"approval"},"environments":[{"name":"ci","facts":{"vcs_metadata":false,"network":"none","runtime":{"language_version":"go1.27.1","os":"linux","arch":"arm64"}},"owner":"operator"}],"ownership_rules":"default"},"integration":{"plugin":"none","topology":null,"receipts_required":[]},"plugins":{"deploy":"none","records":"json_ledger"},"protected_paths":["go.mod","go.sum","go.work","go.work.sum","vendor/**"],"protected_documents":[],"data_policy":{"floor":"contracted","routing":"verified","allowed_destinations":[]},"usage_profile_override":null,"priority":100,"budget":{"rails":{"per_task_usd":"0.01","per_day_usd":"20"}},"retention":{"attempt_continuity_post_closure_ttl":"P7D","supervisor_notes_ttl":null,"blob_payload_ttl":null},"vocabulary":{"task_id_pattern":null,"branch_prefix":"adc/","language":"en"}}`
	railsBuilder = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa")
	railsProject = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
)

// railsUSDLiability (P17b; #226 entry 69; fault 2: "a liability left out
// of the USD rail"): under the rails authority.Rails gives the task (its
// project profile's per_task_usd), a task's call ends in a hostile zero
// final, so its lease settles with the call a liability (empty); what the
// next lease could reserve under per_task_usd with it is less than one
// turn, so it is refused, could_not_run/
// budget_rail_reached, nothing placed; the node is held on budget_rail
// (empty); the operator's task.raise_rail (its handler's logic, and the
// task.rail_raised fold) raises the task's rail and clears the hold
// (empty); and the lease is placed (empty).
func railsUSDLiability(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	vreg, err := vocab.Load()
	if err != nil {
		return nil, nil, err
	}
	var doc types.ExecutorRegistry
	var groups []types.ResourceGroup
	if err := errors.Join(json.Unmarshal([]byte(railsRegistry), &doc), json.Unmarshal([]byte(railsGroups), &groups)); err != nil {
		return nil, nil, err
	}
	reg, err := registry.New(doc, groups)
	if err != nil {
		return nil, nil, err
	}
	r, err := capacity.NewReservations(groups)
	if err != nil {
		return nil, nil, err
	}
	state := filepath.Join(dir, "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	blobs, err := blobstore.Open(state)
	if err != nil {
		return nil, nil, err
	}
	kinds := graph.Kinds()
	for _, ks := range []eventstore.Kinds{identity.Kinds(), authority.ProjectKinds(), authority.TaskKinds(), lease.Kinds(), ledger.Kinds(), modelcall.Kinds()} {
		maps.Copy(kinds, ks)
	}
	tsk, err := ids.New("tsk")
	if err != nil {
		return nil, nil, err
	}
	railsTask := types.TaskID(tsk)
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	// The raise's object is its task, as the authorizer binds it.
	bind := func(tx *eventstore.Tx, c eventstore.Command) error {
		if c.Action != authority.TaskRaiseRailAction {
			return nil
		}
		return tx.BindObject(eventstore.Object{Subject: string(railsTask), Record: authority.TaskRecordOf(railsTask)})
	}
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Effects: modelcall.Effects(),
		Hooks: eventstore.Hooks{Identify: none, Authorize: bind, Project: eventstore.ProjectEach(identity.Projector{}.Project,
			authority.ProjectProjector{}.Project, authority.TaskProjector{}.Project,
			capacity.Hook(lease.Projector{Blobs: blobs, Holds: vreg.Holds, Units: graph.Units{}}, r), ledger.Project, modelcall.Project,
			graph.Projector{Answers: authority.Answers{}}.Project)}})
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	sys, err1 := ids.New("act")
	op, err2 := ids.New("act")
	if err := errors.Join(err1, err2); err != nil {
		return nil, nil, err
	}
	actor := op // the operator's commands
	do := func(fn func(*eventstore.Tx) ([]eventstore.Event, error)) error {
		cmd, err := ids.New("cmd")
		if err != nil {
			return err
		}
		_, err = s.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: actor, Action: "fixture.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(
			func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := fn(tx)
				return evs, json.RawMessage(`{}`), err
			}))
		return err
	}
	// The system actor, the operator, the project and the task, as their
	// commands record them.
	system := types.Actor{ID: types.ActorID(sys), Kind: types.ActorKindSystem, Name: "system", CreatedBy: types.ActorID(sys)}
	operator := types.Actor{ID: types.ActorID(op), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: types.ActorID(sys)}
	hidden := types.Attachment{Name: "inventory/hidden_test.go", Digest: types.Digest("sha256:" + string(make64('a')))}
	spec := types.TaskCreateScope{Project: railsProject, Title: "rails", Requirement: "spend", Attachments: []types.Attachment{hidden}}
	acceptance := []types.AcceptanceBlob{{Name: hidden.Name, Digest: hidden.Digest, Class: types.AcceptanceBlobClassRestricted,
		WithheldFrom: []types.AcceptanceBlobWithheldFromItem{types.AcceptanceBlobWithheldFromItemBuilder, types.AcceptanceBlobWithheldFromItemReviewer}}}
	actor = sys // the system actor records itself and the operator
	for _, a := range []types.Actor{system, operator} {
		if err := do(func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(a) }); err != nil {
			return nil, nil, err
		}
	}
	actor = op
	for _, setup := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(*eventstore.Tx) ([]eventstore.Event, error) {
			id, err := ids.New("evt")
			p := string(railsProject)
			return []eventstore.Event{{ID: id, Kind: authority.ProjectCreated, Subject: p, Project: &p, Payload: json.RawMessage(railsProfile)}}, err
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			raw, err := json.Marshal(spec)
			if err != nil {
				return nil, err
			}
			dg, err := eventstore.ContentDigest(raw)
			if err != nil {
				return nil, err
			}
			payload, err := json.Marshal(types.Task{ID: railsTask, Project: railsProject, Spec: spec, SpecDigest: types.Digest(dg), Acceptance: acceptance,
				CreatedBy: operator.ID, CreatedAt: types.Time(tx.Now().Format(time.RFC3339Nano))})
			id, err2 := ids.New("evt")
			p := string(railsProject)
			return []eventstore.Event{{ID: id, Kind: authority.TaskCreated, Subject: string(railsTask), Project: &p, Payload: payload}}, errors.Join(err, err2)
		},
	} {
		if err := do(setup); err != nil {
			return nil, nil, err
		}
	}
	d := dispatch.Dispatcher{Registry: reg, Rails: authority.Rails}
	node := func() (types.NodeID, error) {
		id, err := ids.New("nod")
		rec := types.NodeRecorded{Type: "node_recorded", Task: railsTask, Project: railsProject, Node: types.Node{ID: types.NodeID(id), Kind: types.NodeKindPlan,
			DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`), Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
		if err == nil {
			err = do(func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.Record(rec) })
		}
		if err == nil {
			err = do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return graph.Claim(tx, rec.Node.ID, map[string]graph.Resolved{}, 1)
			})
		}
		return rec.Node.ID, err
	}
	card := func(n types.NodeID) dispatch.Card {
		return dispatch.Card{Node: n, Project: railsProject, Image: "localhost/prepared@sha256:" + string(make64('a')), Deadline: "2026-10-04T14:00:00Z",
			Policy: types.ProjectProfileDataPolicy{Floor: "contracted", Routing: "verified", AllowedDestinations: []types.Slug{"stub"}}}
	}
	match := func(n types.NodeID) (types.LeaseID, *dispatch.Refused, error) {
		var id types.LeaseID
		err := do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			evs, l, err := d.Match(tx, card(n), railsBuilder)
			id = l
			return evs, err
		})
		var refused *dispatch.Refused
		if errors.As(err, &refused) {
			return "", refused, nil
		}
		return id, nil, err
	}
	// The first lease: one call, its final a hostile zero (the gateway
	// counted 300 in, 200 out), lost at a restart, then settled.
	first, err := node()
	if err != nil {
		return nil, nil, err
	}
	l, refused, err := match(first)
	if err != nil || refused != nil {
		return nil, nil, errors.Join(err, errors.New("the first lease was not placed"))
	}
	att, err1 := ids.New("att")
	call, err2 := ids.New("cal")
	if err := errors.Join(err1, err2); err != nil {
		return nil, nil, err
	}
	a, c := types.AttemptID(att), types.CallID(call)
	measured := func(kind types.UsageMeasurementKind, seq, in, out int64, src types.UsageMeasurementSourceOfTruth) func(*eventstore.Tx) ([]eventstore.Event, error) {
		return func(*eventstore.Tx) ([]eventstore.Event, error) {
			mid, err := ids.New("msr")
			if err != nil {
				return nil, err
			}
			ev, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(mid), Call: c, Attempt: a, Lease: l, Kind: kind, Sequence: seq,
				Tokens: types.UsageMeasurementTokens{In: in, Out: out}, Turns: 1, Milliseconds: 5, SourceOfTruth: src,
				Cost: types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true}})
			return []eventstore.Event{ev}, err
		}
	}
	for _, setup := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.StartNext(tx, l, 2, a, types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"})
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return modelcall.Intend(tx, gateway.Binding{Lease: l, Attempt: a, Call: c}, "stub")
		},
		measured(types.UsageMeasurementKindDelta, 1, 300, 200, types.UsageMeasurementSourceOfTruthGatewayCount),
		measured(types.UsageMeasurementKindFinal, 2, 0, 0, types.UsageMeasurementSourceOfTruthAdapterReport),
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.LoseRunning(tx, a, types.AttemptLostReasonValueControllerRestart)
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.SealMinimal(tx, a, 2, graph.Units{}, blobs, nil, operator.ID)
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, l, 3, lease.LeaseEvent{Kind: lease.Revoked, Reason: "restart"}, nil)
		},
	} {
		if err := do(setup); err != nil {
			return nil, nil, err
		}
	}
	var o observe
	var settled types.LeaseSettlementValue
	if err := o.step(do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		s, err := capacity.Settlement(tx, l)
		settled = s
		if err != nil {
			return nil, err
		}
		return lease.ChangeLease(tx, l, 4, lease.LeaseEvent{Kind: lease.Settled, Settlement: &s}, nil)
	})); err != nil {
		return nil, nil, err
	}
	o.check(len(settled.Liabilities) == 1 && settled.Liabilities[0] == c, "the hostile zero final's call is not a liability: %+v", settled)
	// The next lease, past per_task_usd with the liability.
	next, err := node()
	if err != nil {
		return nil, nil, err
	}
	id, refused, err := match(next)
	if err != nil {
		return nil, nil, err
	}
	if refused == nil {
		o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
		o.check(false, "the lease past per_task_usd was placed: %s", id)
		return o.steps, o.violations, nil
	}
	o.steps = append(o.steps, Step{vocab.Outcome(refused.Outcome), refused.Reason.String()})
	o.check(refused.Check == "budget" && refused.Hold != nil && refused.Hold.SignalInstance == graph.RailSignal(graph.RailTaskUSD),
		"the refusal names no per_task_usd hold: %+v", refused)
	if refused.Hold == nil {
		return o.steps, o.violations, nil
	}
	if err := o.step(do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Raise(tx, next, *refused.Hold, nil) })); err != nil {
		return nil, nil, err
	}
	// The operator raises the task's per_task_usd to $0.02, above the spend
	// and the next reservation: task.raise_rail's logic, which clears the
	// task's holds on that rail, and the task.rail_raised fold.
	raise, err := json.Marshal(map[string]any{"task": railsTask, "rail": graph.RailTaskUSD, "to_usd": "0.02"})
	if err != nil {
		return nil, nil, err
	}
	cmd, err := ids.New("cmd")
	if err != nil {
		return nil, nil, err
	}
	raiseCmd := eventstore.Command{ID: cmd, Actor: op, Action: authority.TaskRaiseRailAction, IdempotencyKey: cmd, Input: raise}
	_, err = s.SubmitApplied(ctx, raiseCmd, authority.TaskRaiseRailHandler{}.Logic(raiseCmd))
	if err := o.step(err); err != nil {
		return nil, nil, err
	}
	if err := s.View(ctx, func(tx *eventstore.Tx) error {
		caps, err := authority.Rails(tx, railsTask)
		o.check(err == nil && caps.PerTaskMicros == 20_000, "the raised rail in force: %d, %v", caps.PerTaskMicros, err)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	id, refused, err = match(next)
	if err != nil {
		return nil, nil, err
	}
	if refused != nil {
		o.steps = append(o.steps, Step{vocab.Outcome(refused.Outcome), refused.Reason.String()})
	} else {
		o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
	}
	o.check(refused == nil && id != "", "the raised rail did not place the lease: %+v", refused)
	return o.steps, o.violations, nil
}

// make64 is 64 bytes of b.
func make64(b byte) []byte {
	out := make([]byte, 64)
	for i := range out {
		out[i] = b
	}
	return out
}
