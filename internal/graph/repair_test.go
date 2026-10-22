// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// reviewed is a review of build's candidate, claimed and committed with
// outcome o.
func (g graphStore) reviewed(build types.NodeID, o types.Outcome) types.NodeID {
	g.t.Helper()
	n := g.consumerOf(build, types.ResultKindPatch)
	env := g.env(noRequirement)
	g.must(evaluate(env, n))
	r := g.reviewResult(n)
	if o == types.OutcomeFound {
		r.Outcome, r.Reason, r.Detail = o, vocab.ReasonCompletedWithFindings, "the reviewer's decision is no_go"
	}
	g.must(commit(n, r, env))
	return n
}

// repair is Repair's events, applied, and the successor it recorded.
func (g graphStore) repair(build, review types.NodeID) (types.NodeRecorded, error) {
	var next types.NodeRecorded
	err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := Repair(tx, build, review, owedanswers.Authority{})
		if err == nil {
			err = json.Unmarshal(evs[len(evs)-1].Payload, &next)
		}
		return evs, err
	})
	return next, err
}

// TestRepair (P16b; 13 "Successor rounds"): a no_go review of a build's
// candidate makes round n+1 of the build, taking the review's findings and
// the candidate, which the evaluator claims; a go review, a review of
// another node and a repair of a non-build make none.
func TestRepair(t *testing.T) {
	g := openStore(t)
	build, _ := g.completedWith(types.NodeKindBuild, types.ResultKindPatch, []types.Ref{})
	noGo := g.reviewed(build, types.OutcomeFound)
	next, err := g.repair(build, noGo)
	if err != nil {
		t.Fatal(err)
	}
	in := next.Node.Inputs
	if next.Node.Round != 2 || next.Predecessor == nil || *next.Predecessor != build || len(in) != 3 ||
		in[1].Name != "findings-1" || in[1].Source.Variant.(*types.ResultSource).Producer != noGo ||
		in[2].Name != "candidate-1" || in[2].Source.Variant.(*types.ResultSource).Producer != build {
		t.Fatalf("the repair round: %+v", next)
	}
	g.must(evaluate(g.env(noRequirement), next.Node.ID))
	if st := g.state(next.Node.ID).State; st != types.NodeStateStateClaimed {
		t.Errorf("the repair round is %s", st)
	}
	// Round 2 completes and is reviewed no_go: round 3 takes findings-2
	// and candidate-2 after round 1's inputs.
	g.leaseOn(next.Node.ID, exeC)
	r2 := unrecorded(g.t, *g.state(next.Node.ID).Key)
	r2.Kind, r2.Outcome, r2.Reason, r2.Detail = types.ResultKindPatch, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	g.must(commit(next.Node.ID, r2, g.env(noRequirement)))
	noGo2 := g.reviewed(next.Node.ID, types.OutcomeFound)
	third, err := g.repair(next.Node.ID, noGo2)
	if in := third.Node.Inputs; err != nil || third.Node.Round != 3 || len(in) != 5 || in[3].Name != "findings-2" || in[4].Name != "candidate-2" ||
		in[4].Source.Variant.(*types.ResultSource).Producer != next.Node.ID {
		t.Errorf("the second repair: %+v %v", third, err)
	}
	// Astra #291 M2: a no_go review of P1, then the build invalidated and
	// completed with P2: the review's findings are about P1, so no repair
	// until P2 is reviewed.
	rerun, _ := g.completedWith(types.NodeKindBuild, types.ResultKindPatch, []types.Ref{})
	ofP1 := g.reviewed(rerun, types.OutcomeFound)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, rerun) })
	p2 := unrecorded(g.t, *g.state(rerun).Key)
	p2.Kind, p2.Outcome, p2.Reason, p2.Detail = types.ResultKindPatch, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	g.must(commit(rerun, p2, g.env(noRequirement)))
	if _, err := g.repair(rerun, ofP1); err == nil || !strings.Contains(err.Error(), "did not review its selected patch") {
		t.Errorf("a review of P1, the build at P2: %v", err)
	}
	if _, err := g.repair(rerun, g.reviewed(rerun, types.OutcomeFound)); err != nil {
		t.Errorf("a review of P2: %v", err)
	}
	other, otherRes := g.completedWith(types.NodeKindBuild, types.ResultKindPatch, []types.Ref{})
	planReview, _ := g.completedWith(types.NodeKindReview, types.ResultKindReview, []types.Ref{})
	// A no_go review of other's candidate recorded in another task.
	foreign := g.nodeOn(types.NodeKindReview, other)
	foreign.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindPatch
	foreign.Node.Inputs[0].Name = "candidate"
	foreign.Task = "tsk_01m3250v9xft08ssr4ygq6g2nq"
	fn := g.record(foreign)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, fn, map[string]Resolved{"candidate": {Result: otherRes.ID}}, 1)
	})
	fr := g.reviewResult(fn)
	fr.Outcome, fr.Reason, fr.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, "no_go"
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: fn, Result: fr}, g.blobs)
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, fn, fr.ID) })
	for name, c := range map[string][2]types.NodeID{
		"a go review":                                    {other, g.reviewed(other, types.OutcomeEmpty)},
		"a review of another node":                       {other, planReview},
		"a review of another build":                      {other, noGo},
		"a review that does not take the build (itself)": {noGo, noGo},
		"a second repair of the same round (a fork)":     {build, noGo},
		"a review in another task":                       {other, fn},
	} {
		if _, err := g.repair(c[0], c[1]); err == nil {
			t.Errorf("%s: repaired", name)
		}
	}
}

// 22a-7 (Main): the one last-round rule. LastBuildRound is the highest
// round of the task's build nodes; OfLastRound takes a review by its
// candidate's build's round, never its own (a repair's review is recorded
// in round 1); CandidateGo is a completed go review of the last round's
// candidate: an earlier round's go (a second review of round 1), a no_go
// in the last round, or a review not completed is no candidate.
func TestTheLastRoundsCandidate(t *testing.T) {
	empty, found := types.OutcomeEmpty, types.OutcomeFound
	view := func(id string, kind types.NodeKind, round int64, st types.NodeStateState, o *types.Outcome, patchOf string) NodeView {
		n := types.Node{ID: types.NodeID(id), Kind: kind, Round: round, Inputs: []types.InputSpec{}}
		if patchOf != "" {
			n.Inputs = append(n.Inputs, types.InputSpec{Name: "candidate", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result",
				Producer: types.NodeID(patchOf), ResultKind: types.ResultKindPatch}}})
		}
		return NodeView{Node: n, State: types.NodeState{State: st}, Outcome: o}
	}
	done := types.NodeStateStateCompleted
	b1, b2 := view("b1", types.NodeKindBuild, 1, done, &found, ""), view("b2", types.NodeKindBuild, 2, done, &found, "")
	for name, c := range map[string]struct {
		nodes []NodeView
		last  int64
		go_   bool
	}{
		"no build":                              {nil, 0, false},
		"round 1 go":                            {[]NodeView{b1, view("r1", types.NodeKindReview, 1, done, &empty, "b1")}, 1, true},
		"round 1 no_go":                         {[]NodeView{b1, view("r1", types.NodeKindReview, 1, done, &found, "b1")}, 1, false},
		"round 1 go, its review claimed":        {[]NodeView{b1, view("r1", types.NodeKindReview, 1, types.NodeStateStateClaimed, nil, "b1")}, 1, false},
		"round 2's review (in round 1) go":      {[]NodeView{b1, view("r1", types.NodeKindReview, 1, done, &found, "b1"), b2, view("r2", types.NodeKindReview, 1, done, &empty, "b2")}, 2, true},
		"round 1 go, round 2 built, unreviewed": {[]NodeView{b1, view("r1", types.NodeKindReview, 1, done, &empty, "b1"), b2}, 2, false},
		"round 2's review no_go":                {[]NodeView{b1, view("r1", types.NodeKindReview, 1, done, &found, "b1"), b2, view("r2", types.NodeKindReview, 1, done, &found, "b2")}, 2, false},
	} {
		if got := LastBuildRound(c.nodes); got != c.last {
			t.Errorf("%s: LastBuildRound %d, want %d", name, got, c.last)
		}
		if got := CandidateGo(c.nodes); got != c.go_ {
			t.Errorf("%s: CandidateGo %v, want %v", name, got, c.go_)
		}
	}
}
