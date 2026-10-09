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
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	ownC = "own_01m3250vm7f6ktw39dszqnyddr"
	ownD = "own_01m3250v57ft7aehkqwq0yrq3n"
)

// heldRound is a round 2 review of the plan under revision 1 (exeA), not
// claimed, answering ruling (ownID) and data (ownB), both asked by its
// round 1 and unanswered.
func (g graphStore) heldRound(review types.NodeID) types.NodeID {
	g.t.Helper()
	r := g.recordOf(review)
	r.Node.ID = g.newNode(types.NodeKindReview).Node.ID
	first := g.record(r)
	g.must(asked(ownID, first, nil))
	g.must(asked(ownB, first, nil))
	second, err := g.successor(first, owes("ruling", ownID), owes("data", ownB))
	if err != nil {
		g.t.Fatal(err)
	}
	return second
}

// on is a hold named name with signal; owners as the registry has them.
func on(t *testing.T, name, signal string) types.NodeHold {
	owner, ref := types.OwnerSystem, (*string)(nil)
	switch name {
	case "awaiting_input":
		owner, ref = types.OwnerGrantHolder, ptr(actID)
	case "budget_rail":
		owner = types.OwnerOperator
	}
	h := newHold(t, name, owner, ref)
	h.SignalInstance = signal
	return h
}

// moving is SupersedeMoving of pred to req with moves.
func moving(pred, req types.NodeID, moves Moves) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return SupersedeMoving(tx, pred, req, nil, reviewers, moves, owedanswers.Authority{})
	}
}

// supersession is the last review.superseded payload in the log.
func (g graphStore) supersession() (p types.ReviewSuperseded) {
	g.t.Helper()
	err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(0, 10000)
		for _, e := range evs {
			var ge types.GraphEvent
			if e.Kind == ReviewSuperseded && err == nil {
				err = json.Unmarshal(e.Payload, &ge)
				p = *ge.Variant.(*types.ReviewSuperseded)
			}
		}
		return err
	})
	if err != nil {
		g.t.Fatal(err)
	}
	return p
}

// A supersession transfers each wait its replacement still has, unchanged
// (id, since, owner, pages), and retires the others with their reason: an
// answer whose question moved, a member the roster drops. The replacement
// answers the moved question, is held until its waits clear, and then
// claims; the transferred page is not repeated, the predecessor holds
// nothing, and the log rebuilds.
func TestSupersedeHolds(t *testing.T) {
	g := openStore(t)
	env := g.env(gatesOn(treeA, demandA))
	review, _, next := g.superseding()
	second := g.heldRound(review)
	ruling, data := on(t, "awaiting_input", ownID+"@2"), on(t, "awaiting_input", ownB+"@2")
	removed, kept := on(t, "reviewer_unavailable", "registry:"+string(exeA)), on(t, "reviewer_unavailable", "registry:"+string(exeB))
	budget := on(t, "budget_rail", "raise")
	for _, h := range []types.NodeHold{ruling, data, removed, kept, budget} {
		g.must(raised(second, h, nil))
	}
	g.must(page(second, ruling.ID, types.OwnerGrantHolder, ptr(actID), "telegram"))
	before := g.holds(second)
	// The data question moves to ownC: round 1 no longer asks ownB.
	g.must(asked(ownB, review, nil))
	g.must(asked(ownC, *g.recordOf(second).Predecessor, nil))
	g.must(moving(second, next, Moves{"data": &types.AnswerSource{OwedInput: ownC, Revision: 2}}))

	p := g.supersession()
	rep := p.Replacement.ID
	if !slices.Equal(p.TransferredHolds, []types.HoldID{ruling.ID, kept.ID, budget.ID}) ||
		!slices.Equal(p.RetiredHolds, []types.RetiredHold{{Hold: data.ID, Reason: types.RetiredHoldReasonQuestionMoved}, {Hold: removed.ID, Reason: types.RetiredHoldReasonMemberRemoved}}) {
		t.Fatalf("the lists: %v %v", p.TransferredHolds, p.RetiredHolds)
	}
	want := []types.NodeHold{before[0], before[3], before[4]}
	if st := g.state(rep); st.State != types.NodeStateStateHeld || !reflect.DeepEqual(st.Holds, want) {
		t.Fatalf("the replacement: %+v, want holds %+v", st, want)
	}
	if st := g.state(second); st.State != types.NodeStateStateSuperseded || len(st.Holds) != 0 {
		t.Fatalf("the predecessor: %+v", st)
	}
	if by, _ := view[heldBy](g, HoldProjection, string(ruling.ID)); by.Node != rep {
		t.Errorf("the transferred hold's index names %s", by.Node)
	}
	if in := g.recordOf(rep).Node.Inputs[3]; in.Name != "data" || in.Source.Variant.(*types.AnswerSource).OwedInput != ownC {
		t.Errorf("the moved answer: %+v", in)
	}
	if err := g.do(page(rep, ruling.ID, types.OwnerGrantHolder, ptr(actID), "email")); err == nil {
		t.Error("a transferred hold's owner paged again")
	}
	if err := g.do(page(second, ruling.ID, types.OwnerGrantHolder, ptr(actID), "email")); err == nil {
		t.Error("the predecessor still holds a transferred hold")
	}
	g.must(answer(ownID, actID, ptr(types.ApprovalID(aprA))))
	g.must(answer(ownC, actID, ptr(types.ApprovalID(aprC))))
	if _, err := g.completeRecorded(next, g.recordOf(next).Node.Params); err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(env, rep))
	if st := g.state(rep); st.State != types.NodeStateStateHeld || len(st.Holds) != 2 {
		t.Fatalf("the answered replacement: %+v", st)
	}
	g.must(clear(rep, kept))
	g.must(clear(rep, budget))
	g.must(evaluate(env, rep))
	if st := g.state(rep); st.State != types.NodeStateStateClaimed {
		t.Errorf("the replacement is %s", st.State)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// A retry wait is of the predecessor's key, which its replacement does not
// hold: it retires as invalidated. A wait that matches none of the
// predecessor's answers or members transfers: an awaiting_input wait on an
// answer it does not declare, a reviewer_unavailable wait on an executor
// not in its roster, or on a signal that names no executor.
func TestSupersedeRetires(t *testing.T) {
	g := openStore(t)
	review, _, next := g.superseding()
	r := unrecorded(t, *g.state(review).Key)
	r.Kind, r.Outcome, r.Reason, r.Completed = types.ResultKindReview, types.OutcomeCouldNotRun, vocab.ReasonBrokerCreditExhausted, false
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: review, Result: r}, g.blobs)
	})
	retry := newHold(t, "", types.OwnerOperator, nil)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Retry(tx, review, r.ID, retry, nil) })
	g.must(supersede(review, next))
	if p := g.supersession(); len(p.TransferredHolds) != 0 || !slices.Equal(p.RetiredHolds, []types.RetiredHold{{Hold: retry.ID, Reason: types.RetiredHoldReasonInvalidated}}) {
		t.Fatalf("the retry wait: %v %v", p.TransferredHolds, p.RetiredHolds)
	}
	if st := g.state(*g.state(review).SupersededBy); st.State != types.NodeStateStateWaiting || len(st.Holds) != 0 {
		t.Errorf("the replacement: %+v", st)
	}

	second := g.heldRound(review)
	unmatched := []types.NodeHold{on(t, "awaiting_input", ownD+"@2"), on(t, "reviewer_unavailable", "registry:"+string(exeC)),
		on(t, "reviewer_unavailable", "probe")}
	for _, h := range unmatched {
		g.must(raised(second, h, nil))
	}
	g.must(supersede(second, next))
	if p := g.supersession(); !slices.Equal(p.TransferredHolds, holdIDs(unmatched)) || len(p.RetiredHolds) != 0 {
		t.Errorf("unmatched waits: %v %v", p.TransferredHolds, p.RetiredHolds)
	}
}

// The fold derives both lists: a hold missing, unknown, in both, out of
// order or retired for another reason is refused. A move that is not one
// is refused by the builder, and a replacement's answers by the fold unless
// each is its review's or replaced under its name by one answer to an owed
// input the review does not declare: a removal is refused until M1.
func TestSupersedeHoldsRefused(t *testing.T) {
	g := openStore(t)
	review, _, next := g.superseding()
	second := g.heldRound(review)
	ruling, removed, budget := on(t, "awaiting_input", ownID+"@2"), on(t, "reviewer_unavailable", "registry:"+string(exeA)), on(t, "budget_rail", "raise")
	for _, h := range []types.NodeHold{ruling, removed, budget} {
		g.must(raised(second, h, nil))
	}
	gone := types.RetiredHold{Hold: removed.ID, Reason: types.RetiredHoldReasonMemberRemoved}
	for name, edit := range map[string]func(*types.ReviewSuperseded){
		"a hold missing": func(p *types.ReviewSuperseded) { p.TransferredHolds = p.TransferredHolds[:1] },
		"no retirement":  func(p *types.ReviewSuperseded) { p.RetiredHolds = []types.RetiredHold{} },
		"an unknown hold": func(p *types.ReviewSuperseded) {
			p.TransferredHolds = append(p.TransferredHolds, "hld_01m3250vnfeyrv67x6w0ddqr6a")
		},
		"in both": func(p *types.ReviewSuperseded) { p.TransferredHolds = append(p.TransferredHolds, removed.ID) },
		"retired, transferred": func(p *types.ReviewSuperseded) {
			p.TransferredHolds, p.RetiredHolds = append(p.TransferredHolds, removed.ID), []types.RetiredHold{}
		},
		"out of order": func(p *types.ReviewSuperseded) { slices.Reverse(p.TransferredHolds) },
		"another reason": func(p *types.ReviewSuperseded) {
			p.RetiredHolds = []types.RetiredHold{{Hold: removed.ID, Reason: types.RetiredHoldReasonQuestionMoved}}
		},
		"a transfer retired": func(p *types.ReviewSuperseded) {
			p.TransferredHolds = []types.HoldID{ruling.ID}
			p.RetiredHolds = []types.RetiredHold{gone, {Hold: budget.ID, Reason: types.RetiredHoldReasonInvalidated}}
		},
	} {
		if err := g.do(edited(second, next, edit)); err == nil || !strings.Contains(err.Error(), "each of the review's holds") {
			t.Errorf("%s: %v", name, err)
		}
	}

	g.must(asked(ownB, review, nil))
	g.must(asked(ownC, *g.recordOf(second).Predecessor, nil))
	var notOwed ErrNotOwed
	for name, c := range map[string]struct {
		moves Moves
		want  string
	}{
		"a question not moved":       {Moves{"ruling": {OwedInput: ownC, Revision: 2}}, "not moved"},
		"a result input":             {Moves{"plan": {OwedInput: ownC, Revision: 2}}, "no answer input"},
		"the requirement input":      {Moves{"required": {OwedInput: ownC, Revision: 2}}, "no answer input"},
		"the requirement, removed":   {Moves{"required": nil}, "no answer input"},
		"a removal":                  {Moves{"data": nil}, "M1"},
		"no such input":              {Moves{"nope": {OwedInput: ownC, Revision: 2}}, "names no answer input"},
		"to an owed input not asked": {Moves{"data": {OwedInput: ownD, Revision: 2}}, ""},
		"to the same owed input":     {Moves{"data": {OwedInput: ownB, Revision: 2}}, ""},
		"to another revision":        {Moves{"data": {OwedInput: ownC, Revision: 3}}, ""},
	} {
		err := g.do(moving(second, next, c.moves))
		if c.want == "" && !errors.As(err, &notOwed) || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return SupersedeMoving(tx, second, next, nil, reviewers, Moves{"data": nil}, nil)
	}); err == nil || !strings.Contains(err.Error(), "authority") {
		t.Errorf("a move with no authority: %v", err)
	}
	answerTo := func(name types.Name, owed types.OwedInputID, rev int64) func(*types.ReviewSuperseded) {
		return func(p *types.ReviewSuperseded) {
			p.Replacement.Inputs = slices.Clone(p.Replacement.Inputs)
			i := slices.IndexFunc(p.Replacement.Inputs, func(in types.InputSpec) bool { return in.Name == "data" })
			p.Replacement.Inputs[i] = types.InputSpec{Name: name, Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: owed, Revision: rev}}}
		}
	}
	for name, edit := range map[string]func(*types.ReviewSuperseded){
		"a declared owed input": answerTo("data", ownID, 2),
		"a newer revision":      answerTo("data", ownB, 3),
		"renamed":               answerTo("data2", ownC, 2),
		"a declared owed input, its answer removed": func(p *types.ReviewSuperseded) {
			answerTo("data", ownID, 2)(p)
			p.Replacement.Inputs = slices.Delete(p.Replacement.Inputs, 2, 3) // ruling
		},
		"two answers to one owed input": func(p *types.ReviewSuperseded) {
			answerTo("data", ownC, 2)(p)
			p.Replacement.Inputs[2] = answerAt("ruling", ownC, 2)
		},
		"a result input removed": func(p *types.ReviewSuperseded) { p.Replacement.Inputs = p.Replacement.Inputs[1:] },
		"an input added": func(p *types.ReviewSuperseded) {
			p.Replacement.Inputs = append(slices.Clone(p.Replacement.Inputs), answerAt("more", ownC, 2))
		},
	} {
		if err := g.do(edited(second, next, edit)); err == nil || !strings.Contains(err.Error(), "input") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := g.do(edited(second, next, func(p *types.ReviewSuperseded) {
		p.Replacement.Inputs = slices.DeleteFunc(slices.Clone(p.Replacement.Inputs), func(in types.InputSpec) bool { return in.Name == "data" })
	})); err == nil || !strings.Contains(err.Error(), "another roster") {
		t.Errorf("a raw removal: %v", err)
	}
	// Two moves to one owed input, or a move to one another answer answers,
	// are refused by the builder.
	for name, moves := range map[string]Moves{
		"two moves to one":  {"data": {OwedInput: ownC, Revision: 2}, "ruling": {OwedInput: ownC, Revision: 2}},
		"to a declared one": {"data": {OwedInput: ownID, Revision: 2}},
	} {
		g.must(asked(ownID, review, nil))
		if name == "to a declared one" {
			g.must(asked(ownID, *g.recordOf(second).Predecessor, nil))
		}
		if err := g.do(moving(second, next, moves)); err == nil || !strings.Contains(err.Error(), "another answer") {
			t.Errorf("%s: %v", name, err)
		}
	}
	g.must(edited(second, next, answerTo("data", ownC, 2))) // a replaced answer, with the wait on ownB@2 retired
}
