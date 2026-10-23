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

// Opus #408 M1 (E1), L1: a review that is not completed is no candidate
// whatever its outcome (superseded after a go, or renewed and claimed);
// a stage's round is its candidate's.
func TestACandidateIsACompletedReview(t *testing.T) {
	empty := types.OutcomeEmpty
	patch := func(b string) types.InputSpec {
		return types.InputSpec{Name: "candidate", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: types.NodeID(b), ResultKind: types.ResultKindPatch}}}
	}
	tree := func(a string) types.InputSpec {
		return types.InputSpec{Name: "tree", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: types.NodeID(a), ResultKind: types.ResultKindTree}}}
	}
	v := func(id string, kind types.NodeKind, round int64, st types.NodeStateState, o *types.Outcome, in ...types.InputSpec) NodeView {
		return NodeView{Node: types.Node{ID: types.NodeID(id), Kind: kind, Round: round, Inputs: append([]types.InputSpec{}, in...)}, State: types.NodeState{State: st}, Outcome: o}
	}
	b1, b2 := v("b1", types.NodeKindBuild, 1, types.NodeStateStateCompleted, &empty), v("b2", types.NodeKindBuild, 2, types.NodeStateStateCompleted, &empty)
	for name, st := range map[string]types.NodeStateState{"superseded": types.NodeStateStateSuperseded, "claimed": types.NodeStateStateClaimed} {
		if CandidateGo([]NodeView{b1, v("r1", types.NodeKindReview, 1, st, &empty, patch("b1"))}) {
			t.Errorf("a %s review with a go outcome is a candidate", name)
		}
	}
	nodes := []NodeView{b1, b2, v("a2", types.NodeKindAccept, 1, types.NodeStateStateCompleted, &empty, patch("b2")),
		v("g2", types.NodeKindGate, 1, types.NodeStateStateRunnable, nil, tree("a2")), v("r2", types.NodeKindReview, 1, types.NodeStateStateWaiting, nil, patch("b2"))}
	for _, n := range nodes {
		if want := map[types.NodeID]int64{"b1": 1, "b2": 2, "a2": 2, "g2": 2, "r2": 2}[n.Node.ID]; CandidateRound(nodes, n.Node) != want {
			t.Errorf("%s's candidate round %d, want %d", n.Node.ID, CandidateRound(nodes, n.Node), want)
		}
	}
}

// #410 (K2), Opus: a refused round (its accept refused, so the build is
// repaired) leaves its gate and review waiting, never claimed: they are no
// candidate, and the next round's GO is the task's candidate.
func TestARefusedRoundsWaitingChecksAreNoCandidate(t *testing.T) {
	empty, found := types.OutcomeEmpty, types.OutcomeFound
	patch := func(b string) types.InputSpec {
		return types.InputSpec{Name: "candidate", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: types.NodeID(b), ResultKind: types.ResultKindPatch}}}
	}
	tree := func(a string) types.InputSpec {
		return types.InputSpec{Name: "tree", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: types.NodeID(a), ResultKind: types.ResultKindTree}}}
	}
	v := func(id string, kind types.NodeKind, round int64, st types.NodeStateState, o *types.Outcome, in ...types.InputSpec) NodeView {
		return NodeView{Node: types.Node{ID: types.NodeID(id), Kind: kind, Round: round, Inputs: append([]types.InputSpec{}, in...)}, State: types.NodeState{State: st}, Outcome: o}
	}
	done, waiting := types.NodeStateStateCompleted, types.NodeStateStateWaiting
	refused := []NodeView{v("b1", types.NodeKindBuild, 1, done, &found), v("a1", types.NodeKindAccept, 1, done, &found, patch("b1")),
		v("g1", types.NodeKindGate, 1, waiting, nil, tree("a1")), v("r1", types.NodeKindReview, 1, waiting, nil, patch("b1"))}
	if CandidateGo(refused) {
		t.Error("a refused round's waiting checks are a candidate")
	}
	next := append(refused, v("b2", types.NodeKindBuild, 2, done, &found), v("a2", types.NodeKindAccept, 1, done, &empty, patch("b2")),
		v("g2", types.NodeKindGate, 1, done, &empty, tree("a2")), v("r2", types.NodeKindReview, 1, done, &empty, patch("b2")))
	if !CandidateGo(next) || OfLastRound(next, next[3].Node) || !OfLastRound(next, next[7].Node) {
		t.Error("the round after a refused one: its GO is not the candidate")
	}
}

// #226 entry 164 (decision 2): a build's accept that refused the candidate
// (failed) repairs it as a no_go review does, its refusal findings-n; an
// accept that accepted it repairs nothing. Only the repair's build takes a
// refusal: any other consumer of an accept's tree (the gate) waits on it.
func TestARefusedAcceptRepairs(t *testing.T) {
	g := openStore(t)
	accepted := func(build types.NodeID, o types.Outcome) types.NodeID {
		a := g.nodeOn(types.NodeKindAccept, build)
		a.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindPatch
		a.Node.Inputs[0].Name = "patch"
		n := g.record(a)
		env := g.env(noRequirement)
		g.must(evaluate(env, n))
		r := unrecorded(g.t, *g.state(n).Key)
		r.Kind, r.Outcome, r.Reason, r.Detail = types.ResultKindTree, o, vocab.ReasonCompletedClean, ""
		if o == types.OutcomeFailed {
			r.Reason, r.Detail = vocab.ReasonProtectedPath, "accept refused the candidate: pyproject.toml"
		}
		g.must(commit(n, r, env))
		return n
	}
	build, _ := g.completedWith(types.NodeKindBuild, types.ResultKindPatch, []types.Ref{})
	refused := accepted(build, types.OutcomeFailed)
	next, err := g.repair(build, refused)
	if in := next.Node.Inputs; err != nil || next.Node.Round != 2 || len(in) != 3 || in[1].Name != "findings-1" ||
		in[1].Source.Variant.(*types.ResultSource).ResultKind != types.ResultKindTree || in[1].Source.Variant.(*types.ResultSource).Producer != refused {
		t.Fatalf("the repair of a refused candidate: %+v %v", next, err)
	}
	g.must(evaluate(g.env(noRequirement), next.Node.ID))
	if st := g.state(next.Node.ID).State; st != types.NodeStateStateClaimed {
		t.Errorf("the repair round on a refusal: %s", st)
	}
	gn := g.nodeOn(types.NodeKindGate, refused)
	gn.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindTree
	gn.Node.Inputs[0].Name = "tree"
	gate := g.record(gn)
	g.must(evaluate(g.env(noRequirement), gate))
	if st := g.state(gate); st.State == types.NodeStateStateClaimed || len(st.Holds) != 0 {
		t.Errorf("a gate over a refused tree: %s, holds %+v", st.State, st.Holds)
	}
	other, _ := g.completedWith(types.NodeKindBuild, types.ResultKindPatch, []types.Ref{})
	if _, err := g.repair(other, accepted(other, types.OutcomeEmpty)); err == nil || !strings.Contains(err.Error(), "accepted the candidate") {
		t.Errorf("an accepted candidate repaired: %v", err)
	}
}
