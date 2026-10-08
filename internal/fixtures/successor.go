// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	owedRuling    = types.OwedInputID("own_01m3250vz2ezn81cgvbkcn17jd")
	owedOther     = types.OwedInputID("own_01m3250vnfeyrv67x6w0ddqr6a")
	approvalOne   = types.ApprovalID("apr_01m3250vz2ezn81cgvbkcn17jd")
	approvalOther = types.ApprovalID("apr_01m3250vnfeyrv67x6w0ddqr6a")
)

var (
	cleanStep   = Step{vocab.Empty, vocab.ReasonCompletedClean.String()}
	changedStep = Step{vocab.Found, vocab.ReasonReviewInputChanged.String()}
	refusedStep = Step{vocab.Failed, vocab.ReasonLineageUndeclared.String()}
)

// askedReview builds a gate and a review of it that owes a ruling, answered;
// another node owes another input.
func askedReview(g *graphFix) (review types.NodeID, err error) {
	gate, err := g.gateOf(treeA, demandA, nil)
	if err == nil {
		review, err = g.reviewOf(gate)
	}
	for _, a := range []owedanswers.Answer{
		{Owed: owedRuling, Project: gProject, Asker: review, Owner: types.ActorID(g.actor), Revision: 1, Approval: ptrTo(approvalOne)},
		{Owed: owedOther, Project: gProject, Asker: gate, Owner: types.ActorID(g.actor), Revision: 1, Approval: ptrTo(approvalOther)},
	} {
		if err == nil {
			err = g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return owedanswers.Record(a) })
		}
	}
	return review, err
}

// successorOf records pred's successor with added.
func (g *graphFix) successorOf(pred types.NodeID, added ...types.InputSpec) (next types.NodeID, err error) {
	err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := graph.Successor(tx, pred, added, owedanswers.Authority{})
		if len(evs) > 0 {
			next = types.NodeID(evs[len(evs)-1].Subject) // node.recorded comes last
		}
		return evs, err
	})
	return next, err
}

func answerTo(owed types.OwedInputID) types.InputSpec {
	return types.InputSpec{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: owed, Revision: 1}}}
}

// undeclaredRulingNotInContext: an undeclared ruling never enters a review's
// controller context (13 "What M0 delivers"; 10; 11:156). The review that
// owes a ruling, once answered, still renders only its declared inputs; a
// round 2 recorded with an answer to an owed input no ancestor asked for is
// refused by the evaluator; the successor that declares the answer its
// predecessor owed renders it.
func undeclaredRulingNotInContext(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	review, err := askedReview(g)
	var next, stray types.NodeID
	if err == nil {
		next, err = g.successorOf(review, answerTo(owedRuling))
	}
	if err == nil { // another review of the gate, and its round 2 answering what the gate owes
		var other types.NodeID
		other, err = g.record(types.NodeKindReview, json.RawMessage(`{"other":true}`), on("plan", g.plan, types.ResultKindPlan))
		if err == nil {
			stray, err = g.record(types.NodeKindReview, json.RawMessage(`{"other":true}`), append(on("plan", g.plan, types.ResultKindPlan), answerTo(owedOther)), other)
		}
	}
	evaluate := func(n types.NodeID) error {
		return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{n}, g.env(treeA, demandA))
		})
	}
	var o observe
	for _, n := range []types.NodeID{review, stray, next} {
		var notOwed graph.ErrNotOwed
		if err == nil {
			if err = evaluate(n); errors.As(err, &notOwed) {
				o.steps, err = append(o.steps, refusedStep), nil
				continue
			}
		}
		var refs []types.Ref
		if err == nil {
			err = g.s.View(ctx, func(tx *eventstore.Tx) (err error) {
				refs, err = graph.Context(tx, n, owedanswers.Authority{})
				return err
			})
		}
		if err != nil {
			return nil, nil, err
		}
		st := cleanStep
		if slices.ContainsFunc(refs, func(r types.Ref) bool { return r.Type == types.RefTypeApproval }) {
			st = changedStep // a ruling in the context
		}
		o.steps = append(o.steps, st)
	}
	return o.steps, o.violations, nil
}

// answerReachesOnlySuccessor: a ruling answers an owed input and re-keys the
// review, through its successor only (13 "Successor rounds"; 11:155, 156):
// the successor declaring the answer has another key than the review that
// asked, and a successor answering an input its predecessor did not owe is
// refused.
func answerReachesOnlySuccessor(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	review, err := askedReview(g)
	var next types.NodeID
	if err == nil {
		next, err = g.successorOf(review, answerTo(owedRuling))
	}
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{review, next}, g.env(treeA, demandA))
		})
	}
	asking, _, aerr := g.node(review)
	answered, _, nerr := g.node(next)
	if err = errors.Join(err, aerr, nerr); err != nil || asking.Key == nil || answered.Key == nil {
		return nil, nil, errors.Join(err, errors.New("the reviews are not claimed"))
	}
	var o observe
	o.steps = append(o.steps, cleanStep)
	if asking.Key.Key != answered.Key.Key {
		o.steps[0] = changedStep
	}
	st := cleanStep
	var notOwed graph.ErrNotOwed
	other := answerTo(owedOther)
	other.Name = "other"
	if _, err := g.successorOf(next, other); errors.As(err, &notOwed) { // round 3, answering what no ancestor owed
		st = refusedStep
	} else if err != nil {
		return nil, nil, err
	}
	return append(o.steps, st), o.violations, nil
}

// equalConclusionsReuseReview: an unchanged conclusions digest does not
// trigger re-review; a changed step outcome or changed restrictions do
// (13 "Reuse decisions use typed conclusions"; 11:167).
func equalConclusionsReuseReview(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	clean := pred("tests", vocab.Empty, vocab.ReasonCompletedClean)
	clean.Detail = ""
	findings := pred("tests", vocab.Found, vocab.ReasonCompletedWithFindings)
	restricted := []types.Restriction{{Project: gProject, PolicyDigest: instrumentDigest, Floor: types.RestrictionFloorOwn, Routing: types.RestrictionRoutingVerified, AllowedDestinations: []string{}}}
	env := g.env(treeA, demandA)
	// The review ran on the gate's first result; each later result follows
	// an invalidation of the gate.
	ref, conclusions, err := g.gateReceiptWith(treeA, demandA, nil, []types.Restriction{}, clean)
	var gate, review types.NodeID
	var used types.ResultID
	if err == nil {
		gate, used, err = g.complete(types.NodeKindGate, json.RawMessage(`{}`), on("plan", g.plan, types.ResultKindPlan),
			map[string]graph.Resolved{"plan": {Result: g.planRes}}, types.ResultKindGateResult, []types.Ref{ref}, &conclusions)
	}
	if err == nil {
		review, err = g.reviewOf(gate)
	}
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{review}, env)
		})
	}
	if err == nil {
		_, err = g.commit(review, types.ResultKindReview, env)
	}
	if err != nil {
		return nil, nil, err
	}
	var o observe
	for _, c := range []struct {
		p types.Predicate
		r []types.Restriction
	}{{clean, []types.Restriction{}}, {findings, []types.Restriction{}}, {clean, restricted}} {
		var fresh types.ResultID
		var same bool
		ref, conclusions, err := g.gateReceiptWith(treeA, demandA, nil, c.r, c.p)
		if err == nil {
			err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Invalidate(tx, gate) })
		}
		if err == nil {
			fresh, err = g.selectResult(gate, types.ResultKindGateResult, []types.Ref{ref}, &conclusions)
		}
		if err == nil {
			err = g.s.View(ctx, func(tx *eventstore.Tx) (err error) {
				same, err = graph.Equivalent(tx, review, "gate", used, fresh, g.blobs)
				return err
			})
		}
		if err != nil {
			return nil, nil, err
		}
		st := changedStep
		if same {
			st = Step{vocab.Empty, vocab.ReasonConclusionsUnchanged.String()}
		}
		o.steps = append(o.steps, st)
	}
	return o.steps, o.violations, nil
}
