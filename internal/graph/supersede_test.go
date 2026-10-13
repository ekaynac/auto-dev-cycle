// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/executors"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
)

// required is a review's input taking req's requirement.
func required(req types.NodeID) types.InputSpec {
	return types.InputSpec{Name: "required", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: req, ResultKind: types.ResultKindReviewRequirement}}}
}

// superseding completes requirement revision 1 (exeA), records a review by
// exeA on the plan and it, claimed, and records revision 2 (exeB). It
// returns the review, revision 1's result and revision 2's node.
func (g graphStore) superseding() (review types.NodeID, res types.ResultID, next types.NodeID) {
	g.t.Helper()
	req, res, err := g.completeRequirement(g.requirementNode(1, "1", exeA), nil)
	if err != nil {
		g.t.Fatal(err)
	}
	r := g.newNode(types.NodeKindReview)
	r.Node.Inputs = append(r.Node.Inputs, required(req))
	review = g.record(r)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, review, map[string]Resolved{"plan": {Result: g.planRes}, "required": {Result: res}}, 0)
	})
	return review, res, g.record(g.requirementNode(2, "1", exeB))
}

func supersede(pred, req types.NodeID) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Supersede(tx, pred, req, reviewers) }
}

// A roster change is one supersession: the replacement keeps the review's
// round and inputs, with the next revision's roster and requirement, and
// takes its successor slot. The superseded review keeps its result as
// history and is never superseded, succeeded, committed or claimed again;
// its dependant waits, a review of the old revision is not claimed, and the
// replacement claims once its revision is selected. The log rebuilds.
func TestSupersede(t *testing.T) {
	g := openStore(t)
	env := g.env(gatesOn(treeA, demandA))
	review, res, next := g.superseding()
	other := g.newNode(types.NodeKindReview)
	other.Node.Inputs = append(slices.Clone(g.recordOf(review).Node.Inputs), g.planInput("again"))
	old := g.record(other) // another review of the old revision, not claimed yet
	g.must(commit(review, g.reviewResult(review), env))
	result := g.state(review).Result
	dependant := g.newNode(types.NodeKindClosure)
	dependant.Node.Inputs = append(dependant.Node.Inputs, types.InputSpec{Name: "review", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: review, ResultKind: types.ResultKindReview}}})
	d := g.record(dependant)

	g.must(supersede(review, next))
	st := g.state(review)
	if st.State != types.NodeStateStateSuperseded || st.SupersededBy == nil || st.Result == nil || *st.Result != *result {
		t.Fatalf("the superseded review: %+v", st)
	}
	rep := *st.SupersededBy
	was, now := g.recordOf(review).Node, g.recordOf(rep).Node
	if now.Round != was.Round || must(ParamsDigest(now.Params)) != must(ParamsDigest(reviewParams(exeB))) || now.Inputs[1].Source.Variant.(*types.ResultSource).Producer != next ||
		!reflect.DeepEqual(now.Inputs[0], was.Inputs[0]) {
		t.Errorf("the replacement: %+v", now)
	}
	if r := g.recordOf(rep); r.Predecessor != nil {
		t.Errorf("the replacement succeeds %s: it keeps its review's round and predecessor", *r.Predecessor)
	}
	if slot, _ := view[types.NodeID](g, SuccessorProjection, string(review)); slot != rep {
		t.Errorf("the successor slot is %s", slot)
	}
	var sup ErrSuperseded
	for name, fn := range map[string]logic{
		"supersede": supersede(review, next),
		"successor": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return Successor(tx, review, nil, owedanswers.Authority{})
		},
		"commit": commit(review, g.reviewResult(review), env),
	} {
		if err := g.do(fn); !errors.As(err, &sup) || sup.By != rep || sup.Reason().String() != "stale_expected_state" {
			t.Errorf("%s the superseded review: %v", name, err)
		}
	}
	g.must(evaluate(env, review, d))
	if g.state(review).State != types.NodeStateStateSuperseded || g.state(d).State != types.NodeStateStateWaiting {
		t.Error("the evaluator ran a superseded review, or served its result")
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, old, map[string]Resolved{"plan": {Result: g.planRes}, "required": {Result: res}, "again": {Result: g.planRes}}, 0)
	}); err == nil || !strings.Contains(err.Error(), "selected revision") {
		t.Errorf("a review of the old revision claimed: %v", err)
	}
	// The subject is at revision 2: another requirement of it, and
	// revision 1 again, are not selected; revision 2 is.
	for name, req := range map[string]types.NodeRecorded{"another roster": g.requirementNode(2, "1", exeC), "revision 1": g.requirementNode(1, "1", exeA)} {
		if _, _, err := g.completeRequirement(req, nil); err == nil || !strings.Contains(err.Error(), "revision") {
			t.Errorf("%s selected: %v", name, err)
		}
	}
	if _, err := g.completeRecorded(next, g.recordOf(next).Node.Params); err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(env, rep))
	if g.state(rep).State != types.NodeStateStateClaimed {
		t.Errorf("the replacement is %s", g.state(rep).State)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// edited is Supersede(pred, req) with its review.superseded payload edited.
func edited(pred, req types.NodeID, edit func(*types.ReviewSuperseded)) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := Supersede(tx, pred, req, reviewers)
		if err != nil {
			return nil, err
		}
		var e types.GraphEvent
		if err := json.Unmarshal(evs[len(evs)-1].Payload, &e); err != nil {
			return nil, err
		}
		p := e.Variant.(*types.ReviewSuperseded)
		edit(p)
		evs[len(evs)-1].Subject = string(p.Predecessor)
		evs[len(evs)-1].Payload, err = json.Marshal(e)
		return evs, err
	}
}

// Attempts are bound to nodes (M1 P8a): a unit is its node. A supersession
// stops its review's running attempt, derived from the review's unit, and
// lists exactly it. The fold refuses one leaving that attempt running,
// listed or not, and one listing nothing after stopping it. No attempt
// starts on the superseded review, or on a node not claimed. The log
// rebuilds.
func TestSupersedeStopsBoundAttempt(t *testing.T) {
	g := openStore(t)
	review, _, next := g.superseding()
	if n, ok := NodeOf(UnitOf(review)); !ok || n != review {
		t.Fatalf("the unit of %s is %s's", review, n)
	}
	if _, ok := NodeOf("lea_01m3250vm7f6ktw39dszqnyddr"); ok {
		t.Error("a lease id read as a unit")
	}
	a := g.runningOn(review)
	alone := func(edit func(*types.ReviewSuperseded)) logic { // the supersession without its stop
		inner := edited(review, next, edit)
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			evs, err := inner(tx)
			if err != nil {
				return nil, err
			}
			return evs[len(evs)-1:], nil
		}
	}
	for name, c := range map[string]struct {
		fn   logic
		want string
	}{
		"unlisted, left running":       {alone(func(p *types.ReviewSuperseded) { p.StoppedAttempts = []types.AttemptID{} }), "still runs"},
		"listed, left running":         {alone(func(*types.ReviewSuperseded) {}), "still runs"},
		"stopped, unlisted":            {edited(review, next, func(p *types.ReviewSuperseded) { p.StoppedAttempts = []types.AttemptID{} }), "lists"},
		"stopped, listed with another": {edited(review, next, func(p *types.ReviewSuperseded) { p.StoppedAttempts = append(p.StoppedAttempts, newAttempt()) }), "lists"},
	} {
		if err := g.do(c.fn); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	g.must(supersede(review, next))
	var at types.Attempt
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) { at, _, _, err = lease.AttemptOf(tx, a); return err }); err != nil {
		t.Fatal(err)
	}
	if by, _ := view[types.NodeID](g, PreemptedProjection, string(a)); at.Status != types.AttemptStatusStopping || *at.StopReason != types.AttemptStopReasonValuePreempt || by != review {
		t.Errorf("the review's attempt is %s, listed by %s", at.Status, by)
	}
	for node, want := range map[types.NodeID]string{review: "is superseded", next: "only on a claimed node"} {
		err := g.s.View(context.Background(), func(tx *eventstore.Tx) error { return Units{}.Startable(tx, UnitOf(node)) })
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("an attempt on %s: %v", node, err)
		}
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// The fold refuses a supersession its state does not derive, each for its
// own reason; once one is recorded, the same events again are ErrSuperseded.
func TestSupersededFold(t *testing.T) {
	g := openStore(t)
	review, _, next := g.superseding()
	requirement := func(change func(*types.NodeRecorded), rev int64, def string, exes ...types.ExecutorID) types.NodeID {
		r := g.requirementNode(rev, def, exes...)
		if change != nil {
			change(&r)
		}
		return g.record(r)
	}
	skipped := requirement(nil, 3, "1", exeB)
	another := requirement(nil, 2, "1", exeC)
	defTwo := requirement(nil, 2, "2", exeB)
	elsewhere := requirement(func(r *types.NodeRecorded) { r.Task = "tsk_01m3250vm7f6ktw39dszqnyddr" }, 2, "1", exeB)
	closure := requirement(func(r *types.NodeRecorded) {
		r.Node.Params = json.RawMessage(strings.Replace(string(r.Node.Params), "candidate", "closure", 1))
	}, 2, "1", exeB)
	input := func(i int, producer types.NodeID) func(*types.ReviewSuperseded) {
		return func(p *types.ReviewSuperseded) {
			p.Replacement.Inputs = slices.Clone(p.Replacement.Inputs)
			src := *p.Replacement.Inputs[i].Source.Variant.(*types.ResultSource)
			src.Producer = producer
			p.Replacement.Inputs[i].Source.Variant = &src
		}
	}
	for name, c := range map[string]struct {
		edit func(*types.ReviewSuperseded)
		want string
	}{
		"the same roster":    {func(p *types.ReviewSuperseded) { p.Replacement.Params = reviewParams(exeA) }, "another roster"},
		"another round":      {func(p *types.ReviewSuperseded) { p.Replacement.Round = 2 }, "another roster"},
		"another definition": {func(p *types.ReviewSuperseded) { p.Replacement.DefinitionVersion = "2"; input(1, defTwo)(p) }, "another roster"},
		"other requirements": {func(p *types.ReviewSuperseded) {
			p.Replacement.Requires.Resources = []types.ResourceGroupKind{types.ResourceGroupKindEvidenceTarget}
		}, "another roster"},
		"another subject": {func(p *types.ReviewSuperseded) {
			p.Replacement.Params = json.RawMessage(strings.Replace(string(p.Replacement.Params), "candidate", "closure", 1))
		}, "another roster"},
		"an input moved":         {input(0, next), "input 0"},
		"an input dropped":       {func(p *types.ReviewSuperseded) { p.Replacement.Inputs = p.Replacement.Inputs[1:] }, "another roster"},
		"the last input dropped": {func(p *types.ReviewSuperseded) { p.Replacement.Inputs = p.Replacement.Inputs[:1] }, "another roster"},
		"an input added": {func(p *types.ReviewSuperseded) {
			p.Replacement.Inputs = append(p.Replacement.Inputs, g.planInput("again"))
		}, "another roster"},
		"the requirement unmoved": {input(1, g.recordOf(review).Node.Inputs[1].Source.Variant.(*types.ResultSource).Producer), "next revision"},
		"a skipped revision":      {input(1, skipped), "next revision"},
		"another roster's":        {input(1, another), "next revision"},
		"another definition's":    {input(1, defTwo), "next revision"},
		"another subject's":       {input(1, closure), "next revision"},
		"both of another subject": {func(p *types.ReviewSuperseded) {
			p.Replacement.Params = json.RawMessage(strings.Replace(string(p.Replacement.Params), "candidate", "closure", 1))
			input(1, closure)(p)
		}, "another roster"},
		"a renamed requirement": {func(p *types.ReviewSuperseded) {
			p.Replacement.Inputs = slices.Clone(p.Replacement.Inputs)
			p.Replacement.Inputs[1].Name = "required2"
		}, "input 1"},
		"another task's":          {input(1, elsewhere), "of its task"},
		"a review as requirement": {input(1, review), "of its task"},
		"an attempt not stopped": {func(p *types.ReviewSuperseded) {
			p.StoppedAttempts = []types.AttemptID{"att_01m3250vnfeyrv67x6w0ddqr6a"}
		}, "lists"},
		"a transferred hold":       {func(p *types.ReviewSuperseded) { p.TransferredHolds = []types.HoldID{"hld_01m3250vnfeyrv67x6w0ddqr6a"} }, "each of the review's holds"},
		"a recorded replacement":   {func(p *types.ReviewSuperseded) { p.Replacement.ID = review }, "is recorded"},
		"of a requirement":         {func(p *types.ReviewSuperseded) { p.Predecessor = next }, "only a review"},
		"another kind replacement": {func(p *types.ReviewSuperseded) { p.Replacement.Kind = types.NodeKindCarryOver }, ""},
	} {
		if err := g.do(edited(review, next, c.edit)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	var again []eventstore.Event
	g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var err error
		again, err = Supersede(tx, review, next, reviewers)
		return nil, errors.Join(err, errors.New("kept, not recorded"))
	})
	g.must(edited(review, next, func(*types.ReviewSuperseded) {}))
	if g.state(review).State != types.NodeStateStateSuperseded {
		t.Fatal("the unedited supersession")
	}
	var sup ErrSuperseded
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return again, nil }); !errors.As(err, &sup) {
		t.Errorf("a second supersession's events: %v", err)
	}
}

// A review bound by reuse, or whose key another review reuses, is not
// superseded; nor is a review whose lineage has a
// later round, one with no requirement input, nor one whose replacement
// the registry no longer derives. Superseding a review never claimed
// leaves nothing for the evaluator to claim.
func TestSupersedeRefused(t *testing.T) {
	g := openStore(t)
	env := g.env(gatesOn(treeA, demandA))
	review, res, next := g.superseding()
	copyOf := func() types.NodeID {
		r := g.recordOf(review)
		r.Node.ID = g.newNode(types.NodeKindReview).Node.ID
		return g.record(r)
	}
	reuser, later, waiting := copyOf(), copyOf(), copyOf()
	unbound := g.newNode(types.NodeKindReview) // every review declares its requirement (G7b-2a)
	unbound.Node.Inputs = unbound.Node.Inputs[:1]
	if _, err := RecordReview(unbound, reviewers); err == nil || !strings.Contains(err.Error(), "review_requirement input") {
		t.Errorf("a review with no requirement: %v", err)
	}
	g.must(commit(review, g.reviewResult(review), env))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Reuse(tx, reuser, map[string]Resolved{"plan": {Result: g.planRes}, "required": {Result: res}}, owedanswers.Authority{})
	})
	if _, err := g.successor(later); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[types.NodeID]string{review: "reuse", reuser: "reuse", later: "successor"} {
		if err := g.do(supersede(n, next)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", want, err, want)
		}
	}
	noB := must(executors.New(map[types.ExecutorID]string{exeA: "claude"}))
	var notIndependent ErrNotIndependent
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Supersede(tx, waiting, next, noB) }); !errors.As(err, &notIndependent) {
		t.Errorf("a replacement the registry does not derive: %v", err)
	}
	// A review on revision 2, never selected, is not superseded to revision
	// 3: the subject is at revision 1.
	ahead := g.newNode(types.NodeKindReview)
	ahead.Node.Inputs = append(ahead.Node.Inputs, required(next))
	third := g.record(g.requirementNode(3, "1", exeC))
	if err := g.do(supersede(g.record(ahead), third)); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Errorf("a supersession skipping a revision: %v", err)
	}
	if err := g.do(supersede(waiting, g.plan)); err == nil {
		t.Error("a supersession to a plan")
	}
	g.must(supersede(waiting, next))
	g.must(evaluate(env, waiting))
	if g.state(waiting).State != types.NodeStateStateSuperseded {
		t.Errorf("the superseded waiting review is %s", g.state(waiting).State)
	}
}

// Lineage follows supersession: the replacement's successor answers what
// the superseded review asked.
func TestSupersededLineage(t *testing.T) {
	g := openStore(t)
	review, _, next := g.superseding()
	g.must(asked(ownB, review, ptr(types.ApprovalID(aprB))))
	g.must(supersede(review, next))
	rep := *g.state(review).SupersededBy
	if _, err := g.successor(rep, answerAt("ruling", ownB, 2)); err != nil {
		t.Errorf("the replacement's successor answering its predecessor's question: %v", err)
	}
}

// Superseding a later round: the replacement keeps the round's predecessor,
// so its successor answers what round 1 asked.
func TestSupersedeSuccessor(t *testing.T) {
	g := openStore(t)
	first, _, next := g.superseding()
	g.must(asked(ownB, first, ptr(types.ApprovalID(aprB))))
	g.must(asked(ownID, first, ptr(types.ApprovalID(aprA))))
	second, err := g.successor(first, answerAt("ruling", ownB, 2))
	if err != nil {
		t.Fatal(err)
	}
	g.must(supersede(second, next))
	rep := *g.state(second).SupersededBy
	if r := g.recordOf(rep); r.Predecessor == nil || *r.Predecessor != first || r.Node.Round != 2 {
		t.Fatalf("the replacement of round 2: %+v", r)
	}
	if _, err := g.successor(rep, answerAt("data", ownID, 2)); err != nil {
		t.Errorf("the replacement's successor answering round 1's question: %v", err)
	}
}

// A requirement of another task binds no review: the claim refuses it, and
// neither the review's requirement nor the replacement's may be another
// task's or of another subject. A review whose result a dependant claimed
// is not superseded (G6b-2, G7b-2), and no hold is raised on a superseded
// review. A listed attempt not the review's refuses the supersession.
func TestSupersedeBindings(t *testing.T) {
	g := openStore(t)
	env := g.env(gatesOn(treeA, demandA))
	review, _, next := g.superseding()
	other := g.requirementNode(1, "1", exeA)
	other.Task = "tsk_01m3250vm7f6ktw39dszqnyddr"
	foreignReq, foreignRes, err := g.completeRequirement(other, nil)
	if err != nil {
		t.Fatal(err)
	}
	closure := g.requirementNode(1, "1", exeA)
	closure.Node.Params = json.RawMessage(strings.Replace(string(closure.Node.Params), "candidate", "closure", 1))
	closureReq, _, err := g.completeRequirement(closure, nil)
	if err != nil {
		t.Fatal(err)
	}
	on := func(req types.NodeID) types.NodeID {
		r := g.newNode(types.NodeKindReview)
		r.Node.Inputs = append(r.Node.Inputs, required(req), g.planInput(types.Name(req)))
		return g.record(r)
	}
	foreign := on(foreignReq)
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, foreign, map[string]Resolved{"plan": {Result: g.planRes}, "required": {Result: foreignRes}, string(foreignReq): {Result: g.planRes}}, 0)
	}); err == nil || !strings.Contains(err.Error(), "in the task") {
		t.Errorf("a review claimed on another task's requirement: %v", err)
	}
	for name, n := range map[string]types.NodeID{"another task's": foreign, "another subject's": on(closureReq)} {
		if err := g.do(supersede(n, next)); err == nil || !strings.Contains(err.Error(), "requirement") {
			t.Errorf("a review on %s requirement superseded: %v", name, err)
		}
	}
	if err := g.do(edited(review, next, func(p *types.ReviewSuperseded) {
		p.StoppedAttempts = []types.AttemptID{"att_01m3250vnfeyrv67x6w0ddqr6a"}
	})); err == nil || !strings.Contains(err.Error(), "lists") {
		t.Errorf("a supersession listing an attempt not its review's: %v", err)
	}
	g.must(commit(review, g.reviewResult(review), env))
	dep := g.newNode(types.NodeKindIntegration)
	dep.Node.Inputs = append(dep.Node.Inputs, types.InputSpec{Name: "review", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: review, ResultKind: types.ResultKindReview}}})
	d := g.record(dep)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, d, map[string]Resolved{"plan": {Result: g.planRes}, "review": {Result: *g.state(review).Result}}, 0)
	})
	if err := g.do(supersede(review, next)); err == nil || !strings.Contains(err.Error(), "re-pointing a dependant") {
		t.Errorf("a review whose result a dependant claimed superseded: %v", err)
	}
	waiting := g.recordOf(review)
	waiting.Node.ID = g.newNode(types.NodeKindReview).Node.ID
	w := g.record(waiting)
	g.must(supersede(w, next))
	var sup ErrSuperseded
	if err := g.do(raised(w, awaitingInput(t), nil)); !errors.As(err, &sup) {
		t.Errorf("a hold raised on a superseded review: %v", err)
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		h := awaitingInput(t)
		h.Since = now(tx)
		return event(HoldRaised, string(w), prjID, &types.HoldRaised{Type: "hold_raised", Node: w, Hold: h, Result: nil})
	}); !errors.As(err, &sup) {
		t.Errorf("a raw hold on a superseded review: %v", err)
	}
}

// A reuse binds a review's requirement as a claim does: once another review
// of the subject is superseded to revision 2, a copy of a review completed
// under revision 1 is not reused, neither by the evaluator's fallback nor by
// a raw node.reused event, and the log still rebuilds.
func TestReuseBindsRevision(t *testing.T) {
	g := openStore(t)
	env := g.env(gatesOn(treeA, demandA))
	review, res, next := g.superseding()
	g.must(commit(review, g.reviewResult(review), env))
	copyOf := func(extra ...types.InputSpec) types.NodeID {
		r := g.recordOf(review)
		r.Node.ID, r.Node.Inputs = g.newNode(types.NodeKindReview).Node.ID, append(slices.Clone(r.Node.Inputs), extra...)
		return g.record(r)
	}
	g.must(supersede(copyOf(g.planInput("again")), next)) // the subject is at revision 2
	fresh := copyOf()
	if err := g.do(evaluate(env, fresh)); err != nil || g.state(fresh).State != types.NodeStateStateWaiting { // its requirement no longer serves (G7b-2a)
		t.Errorf("a review of revision 1 reused by the evaluator: %v, %s", err, g.state(fresh).State)
	}
	resolved := map[string]Resolved{"plan": {Result: g.planRes}, "required": {Result: res}}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		rec, err := loadNode(tx, fresh)
		if err != nil {
			return nil, err
		}
		key := *g.state(review).Key
		return event(NodeReused, string(fresh), prjID, &types.NodeReused{Type: "node_reused", Node: fresh, Holder: review, Key: key, Inputs: items(rec.Recorded.Node, resolved)})
	}); err == nil || !strings.Contains(err.Error(), "selected revision") {
		t.Errorf("a raw reuse of revision 1: %v", err)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}
