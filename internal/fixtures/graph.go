// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// graphDemands is the number of goals that demand the build node at once.
const graphDemands = 4

// duplicateDemandsShareOneClaim: goals demanding the same node instance at
// once run it once: one claim, the others join it (13 [G10]). Each demand is
// a command through the real writer and the graph fold; the steps are the
// demands, and the log must hold one node.claimed for the node.
func duplicateDemandsShareOneClaim(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	s, done, err := storeWith(ctx, dir, graph.Kinds(), eventstore.ProjectEach(graph.Project))
	if err != nil {
		return nil, nil, err
	}
	defer done()
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
	const project, task = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9"), types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2np")
	node := func(kind types.NodeKind, inputs []types.InputSpec) (types.NodeRecorded, error) {
		id, err := ids.New("nod")
		return types.NodeRecorded{Type: "node_recorded", Task: task, Project: project, Node: types.Node{ID: types.NodeID(id), Kind: kind, DefinitionVersion: "1",
			Round: 1, Params: json.RawMessage(`{}`), Inputs: inputs, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}, err
	}

	// The plan the build takes, recorded, claimed and completed.
	plan, err := node(types.NodeKindPlan, []types.InputSpec{})
	rid, rerr := ids.New("res")
	if err = errors.Join(err, rerr); err != nil {
		return nil, nil, err
	}
	var planResult types.Result
	for _, fn := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.Record(plan) },
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Claim(tx, plan.Node.ID, map[string]graph.Resolved{}, 1)
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			key, err := graph.Key(plan.Node, map[string]graph.Resolved{}, 1)
			planResult = types.Result{ID: types.ResultID(rid), Computation: key.Key, Kind: types.ResultKindPlan, Value: []types.Ref{},
				Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
			if err != nil {
				return nil, err
			}
			return graph.RecordResult(tx, types.ResultRecorded{Node: plan.Node.ID, Result: planResult})
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Select(tx, plan.Node.ID, planResult.ID)
		},
	} {
		if err := do(fn); err != nil {
			return nil, nil, err
		}
	}
	build, err := node(types.NodeKindBuild, []types.InputSpec{{Name: "plan",
		Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: plan.Node.ID, ResultKind: types.ResultKindPlan}}}})
	if err == nil {
		err = do(func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.Record(build) })
	}
	if err != nil {
		return nil, nil, err
	}

	// The demands, at once.
	errs := make([]error, graphDemands)
	var wg sync.WaitGroup
	for i := range graphDemands {
		wg.Go(func() {
			errs[i] = do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return graph.Claim(tx, build.Node.ID, map[string]graph.Resolved{"plan": {Result: planResult.ID}}, 1)
			})
		})
	}
	wg.Wait()
	var o observe
	for _, err := range errs {
		if err := o.step(err); err != nil {
			return nil, nil, err
		}
	}
	claims := 0
	if err := s.View(ctx, func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(0, 1000)
		for _, e := range evs {
			if e.Kind == graph.NodeClaimed && e.Subject == string(build.Node.ID) {
				claims++
			}
		}
		return err
	}); err != nil {
		return nil, nil, err
	}
	o.check(claims == 1, "%d demands of one node made %d claims, want 1", graphDemands, claims)
	return o.steps, o.violations, nil
}
