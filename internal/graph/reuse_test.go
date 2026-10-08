// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

func reuseOf(n types.NodeID, res types.ResultID) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Reuse(tx, n, map[string]Resolved{"plan": {Result: res}})
	}
}

// A memoizable instance refused its claim completes by reuse of the
// holder's selected result; it can be invalidated like a claimed node, and
// a dependant takes its result as its producer's.
func TestReuse(t *testing.T) {
	g := openStore(t)
	holder, r := g.gated("")
	b := g.record(g.nodeLikeGate(holder))
	var held ErrKeyClaimed
	if err := g.do(g.claimOf(b)); !errors.As(err, &held) || held.Node != holder {
		t.Fatalf("b's claim: %v", err)
	}
	g.must(reuseOf(b, g.planRes))
	st := g.state(b)
	if st.State != types.NodeStateStateCompleted || *st.Result != r.ID || *st.Key != *g.state(holder).Key || len(g.claimed(b).Inputs) != 1 {
		t.Fatalf("reused: %+v, claim %+v", st, g.claimed(b))
	}
	dependant := g.record(g.reviewNodeOn(b))
	if res, miss := g.resolve(dependant, gateOn(treeA, demandA), at); len(miss) != 1 || len(res) != 0 {
		t.Fatalf("the dependant of a receipt-less reuse: %v %+v", res, miss)
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, dependant, map[string]Resolved{"gate": {Result: r.ID}}, 0)
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, b) })
	if st := g.state(b); st.State != types.NodeStateStateClaimed || st.Key.Generation != 2 || st.Result != nil {
		t.Fatalf("the reused node invalidated: %+v", st)
	}
	diffs, err := g.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// nodeLikeGate is another gate with holder's params and inputs: its key.
func (g graphStore) nodeLikeGate(holder types.NodeID) types.NodeRecorded {
	r := g.newNode(types.NodeKindGate)
	r.Node.Params = g.newNodeLike(holder).Params
	return r
}

// reviewNodeOn is a review taking gate's result as its "gate" input.
func (g graphStore) reviewNodeOn(gate types.NodeID) types.NodeRecorded {
	r := g.nodeOn(types.NodeKindReview, gate)
	r.Node.Inputs[0].Name = "gate"
	r.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindGateResult
	return r
}

// The fold refuses a reuse its state does not derive.
func TestReuseRefusals(t *testing.T) {
	g := openStore(t)
	holder, _ := g.gated("")
	key := *g.state(holder).Key
	in := g.claimed(holder).Inputs
	b := g.record(g.nodeLikeGate(holder))
	held := g.record(g.nodeLikeGate(holder))
	g.must(raised(held, awaitingInput(t), nil))
	build := g.record(g.newNode(types.NodeKindBuild))
	busy := g.record(g.newNode(types.NodeKindGate)) // holds its key, claimed, not completed
	g.must(g.claimOf(busy))
	waiter := g.record(g.newNode(types.NodeKindGate))
	other, _ := g.gated("sha256:" + strings.Repeat("9", 64)) // another key
	elsewhere := g.nodeLikeGate(holder)
	elsewhere.Task = tskB
	far := g.record(elsewhere)
	reuse := func(subject string, project types.ProjectID, node, holder types.NodeID, k types.ComputationKey, items []types.ClaimedInput) logic {
		return forge(NodeReused, subject, project, &types.NodeReused{Type: "node_reused", Node: node, Holder: holder, Key: k, Inputs: items})
	}
	k2 := must(Key(g.newNodeLike(b), resolutions(in), 2))
	bk := must(Key(g.newNodeLike(build), resolutions(in), 1))
	for name, fn := range map[string]logic{
		"a reuse under another subject":    reuse(string(holder), prjID, b, holder, key, in),
		"a reuse under node.claimed":       forge(NodeClaimed, string(b), prjID, &types.NodeReused{Type: "node_reused", Node: b, Holder: holder, Key: key, Inputs: in}),
		"a reuse in another project":       reuse(string(b), prjB, b, holder, key, in),
		"a reuse of a held instance":       reuse(string(held), prjID, held, holder, key, in),
		"a reuse of a completed node":      reuse(string(holder), prjID, holder, holder, key, in),
		"a reuse of a build":               reuse(string(build), prjID, build, holder, bk, in),
		"a reuse at another generation":    reuse(string(b), prjID, b, holder, k2, in),
		"a reuse from other inputs":        reuse(string(b), prjID, b, holder, key, []types.ClaimedInput{{Name: "plan", Result: ptr(types.ResultID(resB))}}),
		"a reuse with no inputs":           reuse(string(b), prjID, b, holder, key, []types.ClaimedInput{}),
		"a reuse of a holder not holding":  reuse(string(b), prjID, b, other, key, in),
		"a reuse of an unfinished holder":  reuse(string(waiter), prjID, waiter, busy, *g.state(busy).Key, in),
		"a reuse of another task's holder": reuse(string(far), prjID, far, holder, key, in),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := g.do(reuseOf(far, g.planRes)); err == nil {
		t.Error("a reuse with no holder in the task")
	}
	// The inputs must still be selected: after the plan's invalidation, its
	// old result no longer serves a reuse, though the key still matches.
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, g.plan) })
	if err := g.do(reuse(string(b), prjID, b, holder, key, in)); err == nil || !strings.Contains(err.Error(), "graph: event") {
		t.Errorf("a reuse of unselected inputs: %v", err)
	}
}
