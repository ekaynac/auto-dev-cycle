// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The fold refuses events its state does not derive: forged or replayed.
func TestFoldRefusals(t *testing.T) {
	g := openStore(t)
	aRec := g.newNode(types.NodeKindBuild)
	a := g.record(aRec)
	g.must(g.claimOf(a))
	key := *g.state(a).Key
	failed := g.result(a, false)                      // could_not_run
	other := g.record(g.newNode(types.NodeKindBuild)) // the same key, unclaimed
	in := g.claimed(a).Inputs
	claimOther := func(items []types.NodeClaimedInputsItem) *types.NodeClaimed {
		return &types.NodeClaimed{Type: "node_claimed", Node: other, Key: key, Inputs: items}
	}
	twin := g.record(g.newNode(types.NodeKindBuild)) // claimed, with a's key
	g.must(g.claimOf(twin))
	cRec := g.newNode(types.NodeKindBuild) // claimed, with a key of its own
	cRec.Node.Params = json.RawMessage(`{"task":"c"}`)
	c := g.record(cRec)
	g.must(g.claimOf(c))
	good := unrecorded(t, key) // selectable
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: a, Result: good})
	})
	odd := unrecorded(t, key)
	odd.Completed, odd.Outcome, odd.Reason, odd.Detail = true, types.OutcomeCouldNotRun, vocab.ReasonExecutorIncomplete, "a report that is not completion"
	g.must(forge(ResultRecorded, string(odd.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: odd}))
	ok := unrecorded(t, key)
	key2 := must(Key(aRec.Node, resolutions(in), 2))
	with := func(r types.NodeRecorded, f func(*types.NodeRecorded)) *types.NodeRecorded { f(&r); return &r }
	recordAs := func(project types.ProjectID, f func(*types.NodeRecorded)) logic {
		return func(*eventstore.Tx) ([]eventstore.Event, error) {
			r := with(g.newNode(types.NodeKindBuild), f)
			return event(NodeRecorded, string(r.Node.ID), project, r)
		}
	}
	foreignPred := g.newNode(types.NodeKindBuild)
	foreignPred.Task, foreignPred.Project = tskC, prjB
	fp := g.record(foreignPred)
	gate := g.record(g.newNode(types.NodeKindGate))
	g.must(g.claimOf(gate))
	for name, fn := range map[string]logic{
		// node.recorded
		"a node recorded twice":            forge(NodeRecorded, string(a), prjID, &aRec),
		"a node under another subject":     forge(NodeRecorded, string(other), prjID, with(g.newNode(types.NodeKindBuild), func(*types.NodeRecorded) {})),
		"a node in another project":        recordAs(prjB, func(*types.NodeRecorded) {}),
		"an unknown predecessor":           recordAs(prjID, func(r *types.NodeRecorded) { r.Predecessor, r.Node.Round = ptr(types.NodeID(nodNil)), 2 }),
		"a predecessor of another task":    recordAs(prjID, func(r *types.NodeRecorded) { r.Task, r.Predecessor, r.Node.Round = tskB, &a, 2 }),
		"a predecessor in another project": recordAs(prjID, func(r *types.NodeRecorded) { r.Predecessor, r.Node.Round = &fp, 2 }),
		"a predecessor of another kind":    recordAs(prjID, func(r *types.NodeRecorded) { r.Predecessor, r.Node.Round = &gate, 2 }),
		"a successor of the same round":    recordAs(prjID, func(r *types.NodeRecorded) { r.Predecessor = &a }),
		"a first node of round 2":          recordAs(prjID, func(r *types.NodeRecorded) { r.Node.Round = 2 }),
		"a recorded node under node.claimed": func(*eventstore.Tx) ([]eventstore.Event, error) {
			r := g.newNode(types.NodeKindBuild)
			return event(NodeClaimed, string(r.Node.ID), prjID, &r)
		},
		// node.claimed
		"a claim under node.recorded":      forge(NodeRecorded, string(other), prjID, claimOther(in)),
		"a claim under another subject":    forge(NodeClaimed, string(c), prjID, claimOther(in)),
		"a claim in another project":       forge(NodeClaimed, string(other), prjB, claimOther(in)),
		"a claim whose key is not its own": forge(NodeClaimed, string(other), prjID, &types.NodeClaimed{Type: "node_claimed", Node: other, Key: types.ComputationKey{Key: diff, Generation: 1}, Inputs: in}),
		"a claim from other inputs":        forge(NodeClaimed, string(other), prjID, claimOther([]types.NodeClaimedInputsItem{{Name: "plan", Result: ptr(types.ResultID(resB))}})),
		"a claim of a claimed node":        forge(NodeClaimed, string(a), prjID, &types.NodeClaimed{Type: "node_claimed", Node: a, Key: key2, Inputs: in}),
		"a claim with no inputs":           forge(NodeClaimed, string(other), prjID, claimOther([]types.NodeClaimedInputsItem{})),
		"a claim with an input twice":      forge(NodeClaimed, string(other), prjID, claimOther(append(in[:1:1], in[0]))),
		"a claim with an undeclared input": forge(NodeClaimed, string(other), prjID, claimOther(append(in[:1:1], types.NodeClaimedInputsItem{Name: "extra", Result: ptr(types.ResultID(resB))}))),
		"a claim input with both shapes": forge(NodeClaimed, string(other), prjID, claimOther([]types.NodeClaimedInputsItem{{Name: "plan", Result: in[0].Result,
			Approval: ptr(types.ApprovalID(aprA)), Revision: ptr(int64(2))}})),
		"a claimed gate key claimed again in the task": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := g.newNode(types.NodeKindGate)
			evs, err := Record(r)
			evc, err2 := event(NodeClaimed, string(r.Node.ID), prjID, &types.NodeClaimed{Type: "node_claimed", Node: r.Node.ID, Key: *g.state(gate).Key, Inputs: in})
			return append(evs, evc...), errors.Join(err, err2)
		},
		// result.recorded
		"a result under result.selected":     forge(ResultSelected, string(ok.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: ok}),
		"a result of an unclaimed node":      forge(ResultRecorded, string(ok.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: other, Result: ok}),
		"a result of another key":            forge(ResultRecorded, string(ok.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: withKey(ok, diff)}),
		"a result recorded twice":            forge(ResultRecorded, string(failed.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: failed}),
		"a result under another subject":     forge(ResultRecorded, string(a), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: ok}),
		"a result of another kind":           forge(ResultRecorded, string(ok.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: withKind(ok, types.ResultKindReview)}),
		"a result whose reason does not fit": forge(ResultRecorded, string(ok.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: withReason(ok, vocab.ReasonBrokerCreditExhausted)}),
		// result.selected
		"a selection under result.recorded":  forge(ResultRecorded, string(good.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: good.ID}),
		"a selection under another subject":  forge(ResultSelected, string(a), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: good.ID}),
		"a's result selected for c":          forge(ResultSelected, string(good.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: c, Result: good.ID}),
		"a's result selected for its twin":   forge(ResultSelected, string(good.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: twin, Result: good.ID}),
		"a could_not_run selected":           forge(ResultSelected, string(failed.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: failed.ID}),
		"a completed could_not_run selected": forge(ResultSelected, string(odd.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: odd.ID}),
		"an unrecorded result selected":      forge(ResultSelected, string(ok.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: ok.ID}),
		"a selection for an unclaimed node":  forge(ResultSelected, string(failed.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: other, Result: failed.ID}),
		// node.invalidated
		"an invalidation of an unclaimed node":  forge(NodeInvalidated, string(other), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: other, Key: key2}),
		"an invalidation to another generation": forge(NodeInvalidated, string(a), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: a, Key: must(Key(aRec.Node, resolutions(in), 3))}),
		"an invalidation under another subject": forge(NodeInvalidated, string(c), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: a, Key: key2}),
		"an invalidation under node.claimed":    forge(NodeClaimed, string(a), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: a, Key: key2}),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A selected node is completed: it selects nothing more and records no
	// further result.
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, a, good.ID) })
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, a, good.ID) }); err == nil {
		t.Error("a second selection was admitted")
	}
	if err := g.do(forge(ResultRecorded, string(ok.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: ok})); err == nil {
		t.Error("a completed node recorded a result")
	}
}
