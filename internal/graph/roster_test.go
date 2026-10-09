// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
func reviewParams(exes ...types.ExecutorID) json.RawMessage { return reviewOf("candidate", exes...) }

// reviewOf are the ReviewNodeParams of a review of subject by exes. A
// test that wants a review of its own key gives it a subject of its own,
// whose requirement is its own too (one selected revision per subject).
func reviewOf(subject types.Name, exes ...types.ExecutorID) json.RawMessage {
	roster, err := Roster(exes, reviewers)
	if err != nil {
		panic(err)
	}
	return must(json.Marshal(types.ReviewNodeParams{Subject: subject, Roster: roster}))
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
	if concl == nil {
		concl = req.Node.Params
	}
	rid, err := g.completeRecorded(n, concl)
	return n, rid, err
}

// completeRecorded claims and completes the recorded requirement n with
// conclusions concl.
func (g graphStore) completeRecorded(n types.NodeID, concl json.RawMessage) (types.ResultID, error) {
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return claimAny(tx, n, map[string]Resolved{}, 1) })
	rid := types.ResultID(must(ids.New("res")))
	r := types.Result{ID: rid, Computation: g.state(n).Key.Key, Kind: types.ResultKindReviewRequirement, Value: []types.Ref{},
		Conclusions: &concl, Outcome: types.OutcomeFound, Reason: vocab.ReasonCompletedWithFindings, Detail: "required", Completed: true}
	err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: n, Result: r}, g.blobs)
	})
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, n, rid) })
	}
	return rid, err
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
		"two requirement inputs": func(r *types.NodeRecorded) { requirement(r); r.Node.Inputs[2].Name = "again" },
		"no requirement input":   func(r *types.NodeRecorded) { r.Node.Inputs = r.Node.Inputs[:1] },
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
		"an unknown family":    with(g.newNode(types.NodeKindReview), roster(member(exeX, "unknown"))),
		"an unsorted roster":   with(g.newNode(types.NodeKindReview), roster(member(exeC, "gpt")+","+member(exeA, "claude"))),
		"no requirement input": with(g.newNode(types.NodeKindReview), func(r *types.NodeRecorded) { r.Node.Inputs = r.Node.Inputs[:1] }),
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
		other.Node.Params = reviewOf(types.Name(other.Node.ID), exeA)
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
		again := g.state(r.Node.ID).Holds[0]
		again.ID = types.HoldID(must(ids.New("hld")))
		if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Raise(tx, r.Node.ID, again, nil) }); err == nil {
			t.Errorf("%s: the same hold raised twice", name)
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
// review binds the requirement of its roster and definition version, of
// its task, at its subject's selected revision there; a subject's revision
// is kept or moves to the next, nothing else.
func TestReviewRequirement(t *testing.T) {
	g := openStore(t)
	reqOf := func(subject types.Name, rev int64, def string, exes ...types.ExecutorID) types.NodeRecorded {
		r := g.requirementNode(rev, def, exes...)
		r.Node.Params = must(json.Marshal(types.ReviewRequirement{Subject: subject, Roster: must(Roster(exes, reviewers)), DefinitionVersion: def, Revision: rev}))
		return r
	}
	other := reqOf("pair", 1, "1", exeB).Node.Params
	if _, _, err := g.completeRequirement(reqOf("pair", 1, "1", exeA), other); err == nil {
		t.Error("a requirement result of another roster")
	}
	reviewOn := func(req types.NodeID, subject types.Name, exes ...types.ExecutorID) types.NodeID {
		r := g.newNode(types.NodeKindReview)
		r.Node.Params = reviewOf(subject, exes...)
		r.Node.Inputs = []types.InputSpec{r.Node.Inputs[0], requiredOn(req)}
		return g.record(r)
	}
	claim := func(review types.NodeID, res types.ResultID) error {
		return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return claimAny(tx, review, map[string]Resolved{"plan": {Result: g.planRes}, "required": {Result: res}}, 0)
		})
	}
	req, res, err := g.completeRequirement(reqOf("pair", 1, "1", exeA, exeC), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim(reviewOn(req, "pair", exeA, exeC), res); err != nil {
		t.Fatalf("a review of its requirement's roster: %v", err)
	}
	for name, exes := range map[string][]types.ExecutorID{"another member": {exeA, exeB}, "a member fewer": {exeA}} {
		if err := claim(reviewOn(req, "pair", exes...), res); err == nil || !strings.Contains(err.Error(), "roster and definition") {
			t.Errorf("%s: %v", name, err)
		}
	}
	carry := g.newNode(types.NodeKindIntegration) // a carry_over is the queue's (G7b)
	carry.Node.Inputs = append(carry.Node.Inputs, requiredOn(req))
	if err := claim(g.record(carry), res); err != nil {
		t.Errorf("an integration consumer of a requirement: %v", err)
	}
	// A subject has one selected revision in a task: another requirement
	// at revision 1, or a next one, is refused (only a supersession moves
	// it), so the older definition is another task's.
	for name, r := range map[string]types.NodeRecorded{"another requirement at the revision": reqOf("pair", 1, "1", exeC), "a second revision": reqOf("pair", 2, "1", exeC)} {
		if _, _, err := g.completeRequirement(r, nil); err == nil {
			t.Errorf("%s: selected without a supersession", name)
		}
	}
	olderReq := reqOf("pair", 2, "0", exeA, exeC)
	olderReq.Task = tskC
	older, oldRes, err := g.completeRequirement(olderReq, nil)
	// The roster changes: the review of revision 1 is superseded by one of
	// revision 2.
	first := reviewOn(req, "pair", exeA, exeC)
	next := g.record(reqOf("pair", 2, "1", exeB))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, next, map[string]Resolved{}, 1)
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Supersede(tx, first, next, nil, reviewers) })
	if err != nil {
		t.Fatal(err)
	}
	if err := claim(reviewOn(older, "pair", exeA, exeC), oldRes); err == nil {
		t.Error("a requirement of another definition version")
	}
	// A review of the old revision waits, and the fold refuses its claim.
	stale := reviewOn(req, "pair", exeA, exeB, exeC)
	if _, miss := g.resolve(stale, noRequirement, at); len(miss) != 1 || miss[0].Name != "required" || !strings.Contains(miss[0].Detail, "not its selected revision") {
		t.Errorf("a requirement past its subject's selected revision resolves: %+v", miss)
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		resolved := map[string]Resolved{"plan": {Result: g.planRes}, "required": {Result: res}}
		rec, err := loadNode(tx, stale)
		key, kerr := Key(rec.Recorded.Node, resolved, 1)
		if err = errors.Join(err, kerr); err != nil {
			return nil, err
		}
		return event(NodeClaimed, string(stale), prjID, &types.NodeClaimed{Type: "node_claimed", Node: stale, Key: key, Inputs: items(rec.Recorded.Node, resolved)})
	}); err == nil || !strings.Contains(err.Error(), "is not the selected review_requirement") {
		t.Errorf("a raw claim past its subject's selected revision: %v", err)
	}
	if _, _, err := g.completeRequirement(reqOf("pair", 1, "1", exeA, exeC), nil); err == nil {
		t.Error("a subject's revision went back")
	}
	elsewhere := reqOf("closure", 3, "1", exeA, exeC) // a subject's first selection is any revision
	closure, closureRes, err := g.completeRequirement(elsewhere, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim(reviewOn(closure, "pair", exeA, exeC), closureRes); err == nil || !strings.Contains(err.Error(), "subject") {
		t.Errorf("a requirement of another subject: %v", err)
	}
	inC := reqOf("task", 1, "1", exeA, exeC)
	inC.Task = tskC
	other3, otherRes, err := g.completeRequirement(inC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim(reviewOn(other3, "task", exeA, exeC), otherRes); err == nil || !strings.Contains(err.Error(), "is not the selected review_requirement") {
		t.Errorf("a requirement of another task: %v", err)
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
	// One not run keeps prior's requirement input: a roster change needs a
	// requirement of its own, whose next revision would leave prior's
	// unselected (requirement.go), and reuse is decided on the key alone.
	review := func(run bool, change func(*types.Node)) types.NodeID {
		rec := g.recordOf(prior)
		rec.Node.ID = g.newNode(types.NodeKindReview).Node.ID
		change(&rec.Node)
		if !run {
			g.must(func(*eventstore.Tx) ([]eventstore.Event, error) { return recordAny(rec) })
			return rec.Node.ID
		}
		n := g.record(rec)
		g.must(evaluate(env, n))
		return n
	}
	reusable := func(next types.NodeID, name types.Name) (got bool, err error) {
		err = g.do(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
			got, err = ReviewReusable(tx, prior, next, name, used, fresh, env)
			return nil, err
		})
		return got, err
	}
	same := review(true, func(*types.Node) {})
	for name, c := range map[string]struct {
		next types.NodeID
		want bool
	}{
		"the same roster":             {same, true},
		"a member of the same family": {review(false, func(n *types.Node) { n.Params = reviewParams(exeB) }), false},
		"a member added":              {review(false, func(n *types.Node) { n.Params = reviewParams(exeA, exeC) }), false},
		"another definition version":  {review(false, func(n *types.Node) { n.DefinitionVersion = "2" }), false},
		"another kind":                {review(true, func(n *types.Node) { n.Kind = types.NodeKindIntegration }), false},
		"an unclaimed review":         {review(false, func(*types.Node) {}), true},
		"another subject": {review(false, func(n *types.Node) {
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
	elsewhere := g.recordOf(prior)
	elsewhere.Node.ID, elsewhere.Task, elsewhere.Project = g.newNode(types.NodeKindReview).Node.ID, tskB, prjB
	g.must(func(*eventstore.Tx) ([]eventstore.Event, error) { return recordAny(elsewhere) })
	if got, err := reusable(elsewhere.Node.ID, "gate"); err != nil || got {
		t.Errorf("a review in another project: %v %v", got, err)
	}
	extra := review(false, func(n *types.Node) {
		n.Inputs = append(slices.Clone(n.Inputs), types.InputSpec{Name: "more", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: g.plan, ResultKind: types.ResultKindPlan}}})
	})
	otherPlan, _ := g.planOf(tskID, prjID)
	secondProducer := review(false, func(n *types.Node) {
		n.Inputs = slices.Clone(n.Inputs)
		n.Inputs[2].Source.Variant = &types.ResultSource{Type: "result", Producer: otherPlan, ResultKind: types.ResultKindPlan} // [gate, required, plan]
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

// A review is reusable only on its inputs as a claim resolves them now: not
// with its answer missing or stale, nor with a second input whose selected
// result is no longer applicable (an expired receipt).
func TestReviewReusableResolvesNow(t *testing.T) {
	const own = types.OwedInputID("own_01m3250vm7f6ktw39dszqnyddr")
	// setup is a review that ran on a gate, another gate and an answer, and
	// the gate's next result, which concludes the same.
	// The other gate's receipt is valid for an hour of the store's clock.
	setup := func() (g graphStore, clock *time.Time, reusable func() (bool, error)) {
		clock = ptr(time.Now().UTC())
		g = openStoreAt(t, func() time.Time { return *clock })
		env := g.env(gatesOn(treeA, demandA))
		gate, used := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
		until := clock.Add(time.Hour).Format(time.RFC3339)
		other, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &until))
		g.must(answer(own, actID, ptr(types.ApprovalID(aprA))))
		review := g.asking(takes("gate", gate), takes("other", other), owes("ruling", own))
		g.must(evaluate(env, review))
		g.must(commit(review, g.reviewResult(review), env))
		var fresh types.ResultID
		g.must(invalidate(gate))
		r := g.gateResult(gate, nil)
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return recordResult(tx, types.ResultRecorded{Node: gate, Result: r}, g.blobs)
		})
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, gate, r.ID) })
		fresh = r.ID
		reusable = func() (got bool, err error) {
			err = g.do(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
				got, err = ReviewReusable(tx, review, review, "gate", used.ID, fresh, env)
				return nil, err
			})
			return got, err
		}
		if got, err := reusable(); err != nil || !got {
			t.Fatalf("its inputs as they were: %v %v", got, err)
		}
		if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
			_, err := ReviewReusable(tx, review, review, "gate", used.ID, fresh, env)
			return err
		}); err == nil {
			t.Fatal("decided in a read transaction, which has no time")
		}
		return g, clock, reusable
	}
	g, _, reusable := setup()
	g.must(answer(own, actID, nil))
	if got, err := reusable(); err != nil || got {
		t.Errorf("its answer missing: %v %v", got, err)
	}
	g.must(answer(own, actID, ptr(types.ApprovalID(aprA))))
	if got, err := reusable(); err != nil || !got {
		t.Errorf("its answer given again: %v %v", got, err)
	}
	g.must(owedAnswer(own, func(a *owedanswers.Answer) { a.Revision, a.Approval = 3, ptr(types.ApprovalID(aprB)) }))
	if got, err := reusable(); err != nil || got {
		t.Errorf("its answer stale: %v %v", got, err)
	}
	// The clock passes the other receipt's validity; its result id stays.
	_, clock, reusable := setup()
	*clock = clock.Add(2 * time.Hour)
	if got, err := reusable(); err != nil || got {
		t.Errorf("a second input expired: %v %v", got, err)
	}
}
