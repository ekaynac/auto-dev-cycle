// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/executors"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The tests' executors, in sorted order: exeA and exeB are claude, exeC is
// gpt, and the reviewers does not hold exeX.
const (
	exeA = "exe_01m3250v9xft08ssr4ygq6g2np"
	exeB = "exe_01m3250vkwffc979nzjw0g2sf9"
	exeC = "exe_01m3250vm7f6ktw39dszqnyddr"
	exeX = "exe_01m3250vnfeyrv67x6w0ddqr6a"
)

// reviewers is the tests' executor registry (the A1 fake).
var reviewers = must(executors.New(map[types.ExecutorID]string{exeA: "claude", exeB: "claude", exeC: "gpt"}))

// reviewParams are the ReviewNodeParams of the roster of exes.
func reviewParams(exes ...types.ExecutorID) json.RawMessage {
	roster, err := Roster(exes, reviewers)
	if err != nil {
		panic(err)
	}
	return must(json.Marshal(types.ReviewNodeParams{Subject: "candidate", Roster: roster}))
}

// requirementNode is a review_requirement node at revision rev, for a
// review of definition def by exes.
func (g graphStore) requirementNode(rev int64, def string, exes ...types.ExecutorID) types.NodeRecorded {
	r := g.newNode(types.NodeKindReviewRequirement)
	roster, err := Roster(exes, reviewers)
	if err != nil {
		g.t.Fatal(err)
	}
	r.Node.Inputs = []types.InputSpec{}
	r.Node.Params = must(json.Marshal(types.ReviewRequirement{Subject: "candidate", Roster: roster, DefinitionVersion: def, Revision: rev}))
	return r
}

// requirementOn records, claims and completes req, whose result's
// conclusions are concl (its params when nil).
func (g graphStore) completeRequirement(req types.NodeRecorded, concl json.RawMessage) (types.NodeID, types.ResultID, error) {
	n := g.record(req)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return claimAny(tx, n, map[string]Resolved{}, 1) })
	if concl == nil {
		concl = req.Node.Params
	}
	rid := types.ResultID(must(ids.New("res")))
	r := types.Result{ID: rid, Computation: g.state(n).Key.Key, Kind: types.ResultKindReviewRequirement, Value: []types.Ref{},
		Conclusions: &concl, Outcome: types.OutcomeFound, Reason: vocab.ReasonCompletedWithFindings, Detail: "required", Completed: true}
	err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: n, Result: r}, g.blobs)
	})
	if err == nil {
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, n, rid) })
	}
	return n, rid, err
}

// The roster's families are the reviewers's, sorted by executor; an
// executor of unknown family is never a member.
func TestRoster(t *testing.T) {
	got, err := Roster([]types.ExecutorID{exeC, exeA}, reviewers)
	want := []types.RosterMember{{Executor: exeA, Family: "claude"}, {Executor: exeC, Family: "gpt"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Roster = %v, %v", got, err)
	}
	var unknown ErrNotIndependent
	if _, err := Roster([]types.ExecutorID{exeA, exeX}, reviewers); !errors.As(err, &unknown) || unknown.Executor != exeX {
		t.Errorf("an unregistered executor: %v", err)
	}
	if _, err := Roster([]types.ExecutorID{exeA, exeA}, reviewers); err == nil {
		t.Error("an executor twice")
	}
}

// A review is recorded only with the reviewers's families: Record refuses
// it, and RecordReview refuses an asserted family, an unknown one, an
// unsorted roster and a requirement that is not one input.
func TestRecordReview(t *testing.T) {
	g := openStore(t)
	if _, err := RecordReview(g.newNode(types.NodeKindReview), reviewers); err != nil {
		t.Fatalf("a review of the reviewers's roster: %v", err)
	}
	if _, err := RecordReview(g.requirementNode(1, "1", exeA, exeC), reviewers); err != nil {
		t.Fatalf("a requirement of the reviewers's roster: %v", err)
	}
	for _, r := range []types.NodeRecorded{g.newNode(types.NodeKindReview), g.requirementNode(1, "1", exeA)} {
		if _, err := Record(r); err == nil {
			t.Errorf("Record recorded a valid %s node", r.Node.Kind)
		}
	}
	if _, err := RecordReview(g.newNode(types.NodeKindGate), reviewers); err == nil {
		t.Error("RecordReview recorded a gate")
	}
	roster := func(js string) func(*types.NodeRecorded) {
		return func(r *types.NodeRecorded) {
			r.Node.Params = json.RawMessage(`{"subject":"candidate","roster":[` + js + `]}`)
		}
	}
	member := func(e, f string) string { return `{"executor":"` + e + `","family":"` + f + `"}` }
	requirement := func(r *types.NodeRecorded) {
		r.Node.Inputs = append(r.Node.Inputs, types.InputSpec{Name: "required", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: g.plan, ResultKind: types.ResultKindReviewRequirement}}})
	}
	for name, change := range map[string]func(*types.NodeRecorded){
		"an asserted family":     roster(member(exeA, "gpt")),
		"an unknown executor":    roster(member(exeX, "claude")),
		"an unknown family":      roster(member(exeX, "unknown")),
		"an unsorted roster":     roster(member(exeC, "gpt") + "," + member(exeA, "claude")),
		"an executor twice":      roster(member(exeA, "claude") + "," + member(exeA, "claude")),
		"no reviewer":            roster(""),
		"two requirement inputs": func(r *types.NodeRecorded) { requirement(r); requirement(r); r.Node.Inputs[2].Name = "again" },
	} {
		r := g.newNode(types.NodeKindReview)
		change(&r)
		if _, err := RecordReview(r, reviewers); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
	withInput := g.requirementNode(1, "1", exeA)
	withInput.Node.Inputs = g.newNode(types.NodeKindGate).Node.Inputs
	if _, err := RecordReview(withInput, reviewers); err == nil {
		t.Error("a requirement node with an input")
	}
	// The fold, which reads no reviewers, refuses what it can tell.
	for name, r := range map[string]types.NodeRecorded{
		"an unknown family":  with(g.newNode(types.NodeKindReview), roster(member(exeX, "unknown"))),
		"an unsorted roster": with(g.newNode(types.NodeKindReview), roster(member(exeC, "gpt")+","+member(exeA, "claude"))),
		"a requirement input": with(g.requirementNode(1, "1", exeA), func(r *types.NodeRecorded) {
			r.Node.Inputs = g.newNode(types.NodeKindGate).Node.Inputs
		}),
	} {
		r.Type = "node_recorded"
		if err := g.do(forge(NodeRecorded, string(r.Node.ID), prjID, &r)); err == nil {
			t.Errorf("the fold recorded %s", name)
		}
	}
	// The fold takes a roster whose family is not the registry's, as it
	// reads no registry; the claim refuses it, by the evaluator too.
	for name, js := range map[string]string{"an asserted family": member(exeA, "gpt"), "an unregistered executor": member(exeX, "claude")} {
		r := with(g.newNode(types.NodeKindReview), roster(js))
		r.Type = "node_recorded"
		g.must(forge(NodeRecorded, string(r.Node.ID), prjID, &r))
		var refused ErrNotIndependent
		err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return ClaimReview(tx, r.Node.ID, map[string]Resolved{"plan": {Result: g.planRes}}, 0, reviewers)
		})
		if !errors.As(err, &refused) || refused.Reason() != vocab.ReasonReviewerNotIndependent {
			t.Errorf("%s: claimed: %v", name, err)
		}
		// Evaluated with another goal, it is held and the other is claimed.
		other := g.newNode(types.NodeKindReview)
		other.Node.DefinitionVersion = string(other.Node.ID)
		if err := g.do(evaluate(g.env(noRequirement), r.Node.ID, g.record(other))); err != nil {
			t.Errorf("%s: evaluated: %v", name, err)
		}
		if st := g.state(r.Node.ID); st.State != types.NodeStateStateHeld || len(st.Holds) != 1 || st.Holds[0].Hold != "reviewer_unavailable" {
			t.Errorf("%s: %+v", name, st)
		}
		if st := g.state(other.Node.ID); st.State != types.NodeStateStateClaimed {
			t.Errorf("%s: the other goal is %s", name, st.State)
		}
		if err := g.do(evaluate(g.env(noRequirement), r.Node.ID)); err != nil || len(g.state(r.Node.ID).Holds) != 1 {
			t.Errorf("%s: evaluated again: %v %+v", name, err, g.state(r.Node.ID))
		}
		if err := g.do(evaluate(Env{Need: noRequirement, Blobs: g.blobs}, g.record(g.newNode(types.NodeKindReview)))); err == nil {
			t.Errorf("%s: evaluated with no registry", name)
		}
	}
	plain := g.record(g.newNode(types.NodeKindReview))
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, plain, map[string]Resolved{"plan": {Result: g.planRes}}, 0)
	}); err == nil {
		t.Error("Claim claimed a review")
	}
}

func with(r types.NodeRecorded, change func(*types.NodeRecorded)) types.NodeRecorded {
	change(&r)
	return r
}

// A review requirement's result records exactly its node's params, and a
// review binds the requirement of its roster and definition version.
func TestReviewRequirement(t *testing.T) {
	g := openStore(t)
	other := must(json.Marshal(types.ReviewRequirement{Subject: "candidate", Roster: must(Roster([]types.ExecutorID{exeB}, reviewers)), DefinitionVersion: "1", Revision: 1}))
	if _, _, err := g.completeRequirement(g.requirementNode(1, "1", exeA), other); err == nil {
		t.Error("a requirement result of another roster")
	}
	reviewOn := func(req types.NodeID, exes ...types.ExecutorID) types.NodeID {
		r := g.newNode(types.NodeKindReview)
		r.Node.Params = reviewParams(exes...)
		r.Node.Inputs = append(r.Node.Inputs, types.InputSpec{Name: "required", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: req, ResultKind: types.ResultKindReviewRequirement}}})
		return g.record(r)
	}
	claim := func(review types.NodeID, res types.ResultID) error {
		return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return claimAny(tx, review, map[string]Resolved{"plan": {Result: g.planRes}, "required": {Result: res}}, 0)
		})
	}
	req, res, err := g.completeRequirement(g.requirementNode(1, "1", exeA, exeC), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim(reviewOn(req, exeA, exeC), res); err != nil {
		t.Fatalf("a review of its requirement's roster: %v", err)
	}
	for name, exes := range map[string][]types.ExecutorID{"another member": {exeA, exeB}, "a member fewer": {exeA}} {
		if err := claim(reviewOn(req, exes...), res); err == nil || !strings.Contains(err.Error(), "roster and definition") {
			t.Errorf("%s: %v", name, err)
		}
	}
	older, oldRes, err := g.completeRequirement(g.requirementNode(2, "0", exeA, exeC), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim(reviewOn(older, exeA, exeC), oldRes); err == nil {
		t.Error("a requirement of another definition version")
	}
	elsewhere := g.requirementNode(3, "1", exeA, exeC)
	elsewhere.Node.Params = must(json.Marshal(types.ReviewRequirement{Subject: "closure", Roster: must(Roster([]types.ExecutorID{exeA, exeC}, reviewers)), DefinitionVersion: "1", Revision: 3}))
	closure, closureRes, err := g.completeRequirement(elsewhere, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim(reviewOn(closure, exeA, exeC), closureRes); err == nil || !strings.Contains(err.Error(), "subject") {
		t.Errorf("a requirement of another subject: %v", err)
	}
}

// A review is reusable only from a review of one non-input identity on the
// same producer: never across a roster change, a definition version, a
// kind or a generation.
func TestReviewReusable(t *testing.T) {
	g := openStore(t)
	gate, usedResult := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	used := usedResult.ID
	env := g.env(gatesOn(treeA, demandA))
	// prior takes the gate's result and, as a second input, the plan.
	withPlan := g.nodeOn(types.NodeKindReview, gate)
	withPlan.Node.Inputs[0].Name, withPlan.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = "gate", types.ResultKindGateResult
	withPlan.Node.Inputs = append(withPlan.Node.Inputs, types.InputSpec{Name: "plan", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: g.plan, ResultKind: types.ResultKindPlan}}})
	prior := g.record(withPlan)
	g.must(evaluate(env, prior))
	g.must(commit(prior, g.reviewResult(prior), env))
	// prior ran again at its second generation, on the same result.
	g.must(invalidate(prior))
	g.must(commit(prior, g.reviewResult(prior), env))
	// reselect makes the gate's next result one holding doc's receipt.
	reselect := func(doc map[string]any) types.ResultID {
		g.must(invalidate(gate))
		d := g.derived(doc)
		r := unrecorded(t, *g.state(gate).Key)
		r.Kind, r.Outcome, r.Reason, r.Detail = types.ResultKindGateResult, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
		r.Conclusions, r.Value = ptr(g.conclusions[d]), []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)}
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return recordResult(tx, types.ResultRecorded{Node: gate, Result: r}, g.blobs)
		})
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, gate, r.ID) })
		return r.ID
	}
	fresh := reselect(receiptDoc(treeSubject(treeA, demandA), nil)) // concludes the same
	// review records a node like prior, changed, and evaluates it if run.
	review := func(run bool, change func(*types.Node)) types.NodeID {
		rec := g.recordOf(prior)
		rec.Node.ID = g.newNode(types.NodeKindReview).Node.ID
		change(&rec.Node)
		n := g.record(rec)
		if run {
			g.must(evaluate(env, n))
		}
		return n
	}
	reusable := func(next types.NodeID, name types.Name) (got bool, err error) {
		err = g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
			got, err = ReviewReusable(tx, prior, next, name, used, fresh, g.blobs, owedanswers.Authority{})
			return err
		})
		return got, err
	}
	same := review(true, func(*types.Node) {})
	for name, c := range map[string]struct {
		next types.NodeID
		want bool
	}{
		"the same roster":             {same, true},
		"a member of the same family": {review(true, func(n *types.Node) { n.Params = reviewParams(exeB) }), false},
		"a member added":              {review(true, func(n *types.Node) { n.Params = reviewParams(exeA, exeC) }), false},
		"another definition version":  {review(true, func(n *types.Node) { n.DefinitionVersion = "2" }), false},
		"another kind":                {review(true, func(n *types.Node) { n.Kind = types.NodeKindCarryOver }), false},
		"an unclaimed review":         {review(false, func(*types.Node) {}), true},
		"another subject": {review(true, func(n *types.Node) {
			n.Params = must(json.Marshal(types.ReviewNodeParams{Subject: "closure", Roster: must(Roster([]types.ExecutorID{exeA}, reviewers))}))
		}), false},
	} {
		if got, err := reusable(c.next, "gate"); err != nil || got != c.want {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	plan := review(false, func(n *types.Node) {
		n.Inputs[0].Source.Variant = &types.ResultSource{Type: "result", Producer: g.plan, ResultKind: types.ResultKindGateResult}
	})
	evidence := review(false, func(n *types.Node) {
		n.Inputs[0].Source.Variant = &types.ResultSource{Type: "result", Producer: gate, ResultKind: types.ResultKindEvidence}
	})
	extra := review(false, func(n *types.Node) {
		n.Inputs = append(slices.Clone(n.Inputs), types.InputSpec{Name: "more", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: g.plan, ResultKind: types.ResultKindPlan}}})
	})
	otherPlan, _ := g.planOf(tskID, prjID)
	secondProducer := review(false, func(n *types.Node) {
		n.Inputs = slices.Clone(n.Inputs)
		n.Inputs[1].Source.Variant = &types.ResultSource{Type: "result", Producer: otherPlan, ResultKind: types.ResultKindPlan}
	})
	for name, c := range map[string]struct {
		next types.NodeID
		in   types.Name
	}{"another producer": {plan, "gate"}, "an input of another kind": {evidence, "gate"}, "no such input": {plan, "ruling"}, "an extra input": {extra, "gate"}, "another producer on a second input": {secondProducer, "gate"}} {
		if _, err := reusable(c.next, c.in); err == nil {
			t.Errorf("%s: compared", name)
		}
	}
	failing := receiptDoc(treeSubject(treeA, demandA), nil)
	failing["predicates"] = []any{map[string]any{"name": "tests", "outcome": "found", "reason": "completed_with_findings", "detail": "1 failed", "evidence": []any{}}}
	fresh = reselect(failing)
	if got, err := reusable(same, "gate"); err != nil || got {
		t.Errorf("a changed step outcome, the same roster: %v %v", got, err)
	}
	// The gate concludes the same again; then the plan, a second input,
	// is reselected.
	fresh = reselect(receiptDoc(treeSubject(treeA, demandA), nil))
	if got, err := reusable(same, "gate"); err != nil || !got {
		t.Fatalf("the same conclusions again: %v %v", got, err)
	}
	g.must(invalidate(g.plan))
	plan2 := unrecorded(t, *g.state(g.plan).Key)
	plan2.Kind, plan2.Outcome, plan2.Reason, plan2.Detail, plan2.Value = types.ResultKindPlan, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", []types.Ref{}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: g.plan, Result: plan2}, g.blobs)
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, g.plan, plan2.ID) })
	if got, err := reusable(same, "gate"); err != nil || got {
		t.Errorf("a second input reselected: %v %v", got, err)
	}
}
