// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// owedMoved is the owed input the other question moves to.
const owedMoved = types.OwedInputID("own_01m3250vm7f6ktw39dszqnyddr")

// fulfilled claims the review_requirement node req, whose params are
// params, and selects its found result, its conclusions those params.
func (f *supersessionFix) fulfilled(req types.NodeID, params json.RawMessage) error {
	err := f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.ClaimReview(tx, req, map[string]graph.Resolved{}, 1, reviewers)
	})
	st, _, serr := f.node(req)
	rid, ierr := ids.New("res")
	if err = errors.Join(err, serr, ierr); err != nil || st.Key == nil {
		return errors.Join(err, errors.New("the requirement is not claimed"))
	}
	r := types.Result{ID: types.ResultID(rid), Computation: st.Key.Key, Kind: types.ResultKindReviewRequirement, Value: []types.Ref{}, Conclusions: &params,
		Outcome: types.OutcomeFound, Reason: vocab.ReasonCompletedWithFindings, Detail: "required", Completed: true}
	return f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, graph.Commit(tx, req, r, f.env(treeA, demandA))
	})
}

// asking records owed as asked by asker, answered by approval (nil: not yet).
func (f *supersessionFix) asking(owed types.OwedInputID, asker types.NodeID, approval *types.ApprovalID) error {
	return f.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
		return owedanswers.Record(owedanswers.Answer{Owed: owed, Project: gProject, Asker: asker, Owner: types.ActorID(f.actor), Revision: 1, Approval: approval})
	})
}

// supersessionMovesHolds: a held review's roster change carries its waits
// (13 "Replacement is a recorded supersession", "Holds", "Moved questions";
// plan 15 G6b-2). Round 2 of a review by exeA answers ruling and other,
// both asked by round 1. The evaluator holds it on both answers, the ruling
// wait is paged, and the registry holds it on exeA. The other question
// moves to another owed input, and the roster changes to exeB. The steps:
//  1. the supersession, which transfers the ruling wait with its id, since
//     and page, retires the other wait as question_moved and the exeA wait
//     as member_removed, and answers the moved question in place of the
//     old one;
//  2. once both answers are given and revision 2 is selected, the
//     replacement is evaluated, claimed and committed.
//
// The transferred wait's owner is not paged again, the predecessor holds
// nothing, and replay rebuilds the same state.
func supersessionMovesHolds(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	reg, err := vocab.Load()
	actor, aerr := ids.New("act")
	if err = errors.Join(err, aerr); err != nil {
		return nil, nil, err
	}
	f := &supersessionFix{graphFix: &graphFix{ctx: ctx, actor: actor}, dir: dir, holds: reg.Holds}
	if err := f.open(); err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, f.close()) }()
	f.plan, f.planRes, err = f.complete(types.NodeKindPlan, json.RawMessage(`{}`), []types.InputSpec{}, map[string]graph.Resolved{}, types.ResultKindPlan, []types.Ref{}, nil)
	one, err1 := requirementOf(1, exeA)
	two, err2 := requirementOf(2, exeB)
	if err = errors.Join(err, err1, err2); err != nil {
		return nil, nil, err
	}
	req, err := f.record(types.NodeKindReviewRequirement, one, []types.InputSpec{})
	var first, second, next types.NodeID
	if err == nil {
		err = f.fulfilled(req, one)
	}
	if err == nil {
		first, err = f.record(types.NodeKindReview, soleReviewer, append(on("plan", f.plan, types.ResultKindPlan), on("required", req, types.ResultKindReviewRequirement)...))
	}
	if err == nil {
		err = errors.Join(f.asking(owedRuling, first, nil), f.asking(owedOther, first, nil))
	}
	other := answerTo(owedOther)
	other.Name = "other"
	if err == nil {
		second, err = f.successorOf(first, answerTo(owedRuling), other)
	}
	env := f.env(treeA, demandA)
	evaluate := func(n types.NodeID) error {
		return f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Evaluate(tx, []types.NodeID{n}, env) })
	}
	if err == nil {
		err = evaluate(second) // held on both answers
	}
	st, _, serr := f.node(second)
	if err = errors.Join(err, serr); err != nil || len(st.Holds) != 2 {
		return nil, nil, errors.Join(err, errors.New("round 2 is not held on its two answers"))
	}
	ruling, waiting := st.Holds[0], st.Holds[1]
	if ruling.SignalInstance != string(owedRuling)+"@1" {
		ruling, waiting = waiting, ruling
	}
	hid, err := ids.New("hld")
	unavailable := types.NodeHold{ID: types.HoldID(hid), Hold: vocab.HoldReviewerUnavailable, SignalInstance: "registry:" + string(exeA), Owner: types.OwnerSystem, Pages: []types.NodeHoldPagesItem{}}
	for _, step := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Page(tx, second, ruling.ID, ruling.Owner, ruling.OwnerRef, ptrTo(types.Name("telegram")))
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Raise(tx, second, unavailable, nil) },
	} {
		if err == nil {
			err = f.do(step)
		}
	}
	if err == nil { // the other question moves: round 1 no longer asks it, and asks owedMoved
		err = errors.Join(f.asking(owedOther, f.plan, nil), f.asking(owedMoved, first, nil))
	}
	if err == nil {
		next, err = f.record(types.NodeKindReviewRequirement, two, []types.InputSpec{})
	}
	if st, _, err = f.node(second); err != nil {
		return nil, nil, err
	}
	var o observe
	o.steps = append(o.steps, supersessionStep(f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.SupersedeMoving(tx, second, next, nil, reviewers, graph.Moves{"other": {OwedInput: owedMoved, Revision: 1}}, owedanswers.Authority{})
	})))
	sups, err := f.supersessions()
	pre, _, perr := f.node(second)
	if err = errors.Join(err, perr); err != nil || len(sups) != 1 {
		return nil, nil, errors.Join(err, errors.New("no supersession"))
	}
	rep := sups[0].Replacement.ID
	now, _, err := f.node(rep)
	if err != nil {
		return nil, nil, err
	}
	o.check(slices.Equal(sups[0].TransferredHolds, []types.HoldID{ruling.ID}) && slices.Equal(sups[0].RetiredHolds, []types.RetiredHold{
		{Hold: waiting.ID, Reason: types.RetiredHoldReasonQuestionMoved}, {Hold: unavailable.ID, Reason: types.RetiredHoldReasonMemberRemoved}}),
		"the supersession transferred %v and retired %v", sups[0].TransferredHolds, sups[0].RetiredHolds)
	paged := slices.IndexFunc(st.Holds, func(h types.NodeHold) bool { return h.ID == ruling.ID && len(h.Pages) == 1 })
	o.check(len(st.Holds) == 3 && paged >= 0 && reflect.DeepEqual(now.Holds, st.Holds[paged:paged+1]),
		"the replacement holds %v, want the paged ruling wait %s unchanged", heldIDs(now.Holds), ruling.ID)
	o.check(pre.State == types.NodeStateStateSuperseded && len(pre.Holds) == 0, "the predecessor is %s with %d holds", pre.State, len(pre.Holds))
	o.check(f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Page(tx, rep, ruling.ID, ruling.Owner, ruling.OwnerRef, ptrTo(types.Name("email")))
	}) != nil, "the transferred wait's owner was paged again")

	err = errors.Join(f.asking(owedRuling, first, ptrTo(approvalOne)), f.asking(owedMoved, first, ptrTo(approvalOther)))
	if err == nil {
		err = f.fulfilled(next, two)
	}
	if err != nil {
		return nil, nil, err
	}
	var notOwed graph.ErrNotOwed
	switch err := evaluate(rep); {
	case errors.As(err, &notOwed):
		o.steps = append(o.steps, refusedStep)
	case err != nil:
		return nil, nil, err
	default:
		if now, _, err = f.node(rep); err != nil {
			return nil, nil, err
		}
		step := Step{vocab.Failed, string(now.State)} // not claimed: still held
		if now.State == types.NodeStateStateClaimed {
			if step, err = f.commit(rep, types.ResultKindReview, env); err != nil {
				return nil, nil, err
			}
		}
		o.steps = append(o.steps, step)
	}
	diffs, err := f.s.RebuildProjections(ctx)
	if err != nil {
		return nil, nil, err
	}
	o.check(len(diffs) == 0, "replay reached another state: %+v", diffs)
	return o.steps, o.violations, nil
}

func heldIDs(hs []types.NodeHold) (out []types.HoldID) {
	for _, h := range hs {
		out = append(out, h.ID)
	}
	return out
}
