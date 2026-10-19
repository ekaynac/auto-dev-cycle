// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"encoding/json"
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
		"a go review":                     {other, g.reviewed(other, types.OutcomeEmpty)},
		"a review of another node":        {other, planReview},
		"a review of another build":       {other, noGo},
		"a repair of a review, not build": {noGo, noGo},
		"a review in another task":        {other, fn},
	} {
		if _, err := g.repair(c[0], c[1]); err == nil {
			t.Errorf("%s: repaired", name)
		}
	}
}
