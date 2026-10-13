// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// displacedQuestionWaits: a replacement never runs without an answer its
// lineage still owes (13 "Moved questions"; plan 15 G6b-2, Astra on #213).
// Round 1 of a review asks ruling, unanswered, and other, answered; round
// 2 answers the ruling. A raw supersession of round 2 replaces the ruling's
// answer by the other's, though the ruling did not move. The steps:
//  1. the raw supersession, which the fold accepts (it reads no owed
//     inputs until M1);
//  2. the commit of the replacement, claimed raw on the other's answer:
//     refused as authority_revoked.
//
// The evaluator leaves the replacement unclaimed, and replay rebuilds the
// same state.
func displacedQuestionWaits(ctx context.Context, dir string) ([]Step, []string, error) {
	return displacedRun(ctx, dir, false)
}

// reuseDisplacedQuestion: nor does a replacement complete by reuse without
// an answer its lineage still owes (13 [G10] reuse; Astra on #216). As
// above, a raw supersession replaces the ruling's answer, and the
// replacement stays unclaimed. Its successor, with the same inputs and so
// the same key, completes once the ruling has moved out of the lineage;
// the ruling is then asked again, unanswered. The steps:
//  1. the raw supersession;
//  2. the reuse of the successor's result for the replacement: refused as
//     authority_revoked.
//
// The evaluator does not complete the replacement either, and replay
// rebuilds the same state.
func reuseDisplacedQuestion(ctx context.Context, dir string) ([]Step, []string, error) {
	return displacedRun(ctx, dir, true)
}

func displacedRun(ctx context.Context, dir string, reuse bool) (steps []Step, violations []string, err error) {
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
		err = errors.Join(f.asking(owedRuling, first, nil), f.asking(owedOther, first, ptrTo(approvalOther)))
	}
	if err == nil {
		second, err = f.successorOf(first, answerTo(owedRuling))
	}
	if err == nil {
		next, err = f.record(types.NodeKindReviewRequirement, two, []types.InputSpec{})
	}
	if err != nil {
		return nil, nil, err
	}
	var o observe
	o.steps = append(o.steps, supersessionStep(f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := graph.Supersede(tx, second, next, reviewers)
		if err != nil {
			return nil, err
		}
		var g types.GraphEvent
		last := &evs[len(evs)-1]
		if err := json.Unmarshal(last.Payload, &g); err != nil {
			return nil, err
		}
		p := g.Variant.(*types.ReviewSuperseded)
		p.Replacement.Inputs[2] = answerTo(owedOther) // the ruling's answer, raw: its question has not moved
		last.Payload, err = json.Marshal(g)
		return evs, err
	})))
	sups, err := f.supersessions()
	if err != nil || len(sups) != 1 {
		return nil, nil, errors.Join(err, errors.New("no supersession"))
	}
	rep, res := sups[0].Replacement.ID, types.ResultID("")
	if err = f.fulfilled(next, two); err == nil {
		var st types.NodeState
		st, _, err = f.node(next)
		if st.Result != nil {
			res = *st.Result
		}
	}
	env := f.env(treeA, demandA)
	if reuse {
		return reusedAfterMove(f, &o, first, rep, res, env)
	}
	if err == nil {
		err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{rep}, env)
		})
	}
	st, _, serr := f.node(rep)
	if err = errors.Join(err, serr); err != nil {
		return nil, nil, err
	}
	o.check(st.State != types.NodeStateStateClaimed, "the evaluator claimed the replacement while its lineage still asks the ruling")
	if st.State != types.NodeStateStateClaimed {
		err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.ClaimReview(tx, rep, map[string]graph.Resolved{"plan": {Result: f.planRes}, "required": {Result: res},
				"ruling": {Approval: approvalOther, Revision: 1}}, 0, reviewers)
		})
	}
	step, cerr := f.commit(rep, types.ResultKindReview, env)
	if err = errors.Join(err, cerr); err != nil {
		return nil, nil, err
	}
	o.steps = append(o.steps, step)
	diffs, err := f.s.RebuildProjections(ctx)
	if err != nil {
		return nil, nil, err
	}
	o.check(len(diffs) == 0, "replay reached another state: %+v", diffs)
	return o.steps, o.violations, nil
}

// reusedAfterMove completes rep's successor while the ruling is not asked,
// asks it again, and records the step of reusing the successor's result
// for rep.
func reusedAfterMove(f *supersessionFix, o *observe, first, rep types.NodeID, res types.ResultID, env graph.Env) ([]Step, []string, error) {
	later, err := f.successorOf(rep)
	if err == nil {
		err = f.asking(owedRuling, f.plan, nil) // the ruling moves out of the lineage
	}
	if err == nil {
		err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{later}, env)
		})
	}
	var step Step
	if err == nil {
		step, err = f.commit(later, types.ResultKindReview, env)
	}
	if err == nil && step != cleanStep {
		err = errors.New("the successor did not complete")
	}
	if err == nil {
		err = f.asking(owedRuling, first, nil) // and back in, unanswered
	}
	if err != nil {
		return nil, nil, err
	}
	resolved := map[string]graph.Resolved{"plan": {Result: f.planRes}, "required": {Result: res}, "ruling": {Approval: approvalOther, Revision: 1}}
	err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Reuse(tx, rep, resolved, env.Authority)
	})
	var stale graph.ErrRevalidated
	switch {
	case err == nil:
		o.steps = append(o.steps, cleanStep)
	case errors.As(err, &stale):
		o.steps = append(o.steps, Step{vocab.Failed, stale.Reason.String()})
	default:
		return nil, nil, err
	}
	if err := f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Evaluate(tx, []types.NodeID{rep}, env)
	}); err != nil {
		return nil, nil, err
	}
	st, _, err := f.node(rep)
	if err != nil {
		return nil, nil, err
	}
	o.check(st.State != types.NodeStateStateCompleted, "the replacement completed while its lineage still asks the ruling")
	diffs, err := f.s.RebuildProjections(f.ctx)
	if err != nil {
		return nil, nil, err
	}
	o.check(len(diffs) == 0, "replay reached another state: %+v", diffs)
	return o.steps, o.violations, nil
}

// contextNeedsAuthority: a controller context is rendered only with the
// authority that reads its lineage's questions (13 "Successor rounds", 10;
// Astra on #218). Round 1 of a review asks the ruling, answered; round 2
// answers it and is claimed. The steps:
//  1. its context with no authority: refused as authority_revoked, never a
//     panic;
//  2. its context with the authority: rendered.
func contextNeedsAuthority(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
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
	one, oerr := requirementOf(1, exeA)
	req, rerr := f.record(types.NodeKindReviewRequirement, one, []types.InputSpec{})
	var first, second types.NodeID
	if err = errors.Join(err, oerr, rerr); err == nil {
		err = f.fulfilled(req, one)
	}
	if err == nil {
		first, err = f.record(types.NodeKindReview, soleReviewer, append(on("plan", f.plan, types.ResultKindPlan), on("required", req, types.ResultKindReviewRequirement)...))
	}
	if err == nil {
		err = f.asking(owedRuling, first, ptrTo(approvalOne))
	}
	if err == nil {
		second, err = f.successorOf(first, answerTo(owedRuling))
	}
	env := f.env(treeA, demandA)
	if err == nil {
		err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{second}, env)
		})
	}
	if err != nil {
		return nil, nil, err
	}
	var o observe
	for _, e := range []graph.Env{{}, env} {
		err := f.s.View(ctx, func(tx *eventstore.Tx) (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			_, err = graph.Context(tx, second, e)
			return err
		})
		var stale graph.ErrRevalidated
		switch {
		case err == nil:
			o.steps = append(o.steps, cleanStep)
		case errors.As(err, &stale):
			o.steps = append(o.steps, Step{vocab.Failed, stale.Reason.String()})
		default:
			o.check(false, "the context: %v", err)
		}
	}
	return o.steps, o.violations, nil
}
