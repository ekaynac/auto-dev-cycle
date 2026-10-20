// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// railsRegistry is the stub builder at $1 and $4 per million input and
// output tokens, and its reviewer, in a 2,000,000-token quota
// (fixtures/m1/config's).
const (
	railsRegistry = `{"schema":"adc.executor-registry/v1","executors":[{"id":"exe_01m4tm99haemqt8es2qfbwj8pa","role":"builder","family":"stub-builder",` +
		`"adapter":"http-chat","model":"stub-builder-1","destination":{"id":"stub","kind":"direct_api","verification":"verified","trust_class":"contracted",` +
		`"endpoint":"http://127.0.0.1:18080/v1","retention_days":0},"credential_ref":"stub","group":"rg_01m4tm99haemss9tvzd438vq8j","cost_basis":"cash",` +
		`"price":{"input_usd_micros_per_mtok":1000000,"output_usd_micros_per_mtok":4000000}},` +
		`{"id":"exe_01m4tm99haemr8ajyf09v4ytzb","role":"reviewer","family":"stub-reviewer","adapter":"http-chat","model":"stub-reviewer-1",` +
		`"destination":{"id":"stub","kind":"direct_api","verification":"verified","trust_class":"contracted","endpoint":"http://127.0.0.1:18080/v1","retention_days":0},` +
		`"credential_ref":"stub","group":"rg_01m4tm99haemss9tvzd438vq8j","cost_basis":"cash","price":{"input_usd_micros_per_mtok":2000000,"output_usd_micros_per_mtok":8000000}}]}`
	railsGroups = `[{"id":"rg_01m4tm99haemss9tvzd438vq8j","kind":"account_quota","capacity_model":{"unit":"tokens","limit":2000000,"window":null,` +
		`"reset_at":null,"quota_state":"known"},"release_rule":"on_settle","members":["exe_01m4tm99haemqt8es2qfbwj8pa","exe_01m4tm99haemr8ajyf09v4ytzb"]}]`
	railsBuilder = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa")
	railsProject = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	railsTask    = types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2np")
	// railsReserved is a card's 1,000 tokens at the higher rate, in micros.
	railsReserved = 4000
)

// railsUSDLiability (P17b; #226 entry 69; fault 2: "a liability left out
// of the USD rail"): a task's call ends in a hostile zero final, so its
// lease settles with the call a liability (empty); the next lease would
// pass per_task_usd with it and is refused, could_not_run/
// budget_rail_reached, nothing placed; the node is held on budget_rail
// (empty); the task's rail is raised, which clears the hold (empty); and
// the lease is placed (empty).
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
	for _, ks := range []eventstore.Kinds{lease.Kinds(), ledger.Kinds(), modelcall.Kinds()} {
		maps.Copy(kinds, ks)
	}
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Effects: modelcall.Effects(),
		Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(
			capacity.Hook(lease.Projector{Blobs: blobs, Holds: vreg.Holds, Units: graph.Units{}}, r), ledger.Project, modelcall.Project,
			graph.Projector{Answers: authority.Answers{}}.Project)}})
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	actor, err := ids.New("act")
	if err != nil {
		return nil, nil, err
	}
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
	perTask := int64(2*railsReserved - 1)
	d := dispatch.Dispatcher{Registry: reg, Rails: func(*eventstore.Tx, types.TaskID) (graph.Caps, error) {
		return graph.Caps{AttemptsPerRound: 9, RoundsPerTask: 9, MergeOperationsPerGeneration: 9, PerTaskMicros: perTask, PerDayMicros: graph.NoUSDRail}, nil
	}}
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
		return dispatch.Card{Node: n, Project: railsProject, Image: "localhost/prepared@sha256:" + string(make64('a')), Tokens: 1000, Deadline: "2026-10-04T14:00:00Z",
			Policy:    types.ProjectProfileDataPolicy{Floor: "contracted", Routing: "verified", AllowedDestinations: []types.Slug{"stub"}},
			Footprint: types.LeaseFootprint{ContextTokens: 1, ExpectedTurns: 1}}
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
			return lease.SealMinimal(tx, a, 2, graph.Units{}, blobs, nil, types.ActorID(actor))
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
	// The raise: the rail above the spend, and the task's holds on it cleared
	// (task.raise_rail's logic: authority.TaskRaiseRailHandler).
	perTask = 3 * railsReserved
	if err := o.step(do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.ClearRail(tx, railsTask, graph.RailTaskUSD)
	})); err != nil {
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
