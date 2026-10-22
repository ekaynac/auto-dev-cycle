// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"errors"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// noNeed: no input needs a receipt.
func noNeed(*eventstore.Tx, types.Node, types.InputSpec) (*receipt.Requirement, error) {
	return nil, nil
}

// gateNeed requires of a gate result input a go-test receipt about tree
// under demand; other inputs need none.
func gateNeed(tree string, demand types.Digest) graph.Requirements {
	return func(_ *eventstore.Tx, _ types.Node, in types.InputSpec) (*receipt.Requirement, error) {
		if src, ok := in.Source.Variant.(*types.ResultSource); !ok || src.ResultKind != types.ResultKindGateResult {
			return nil, nil
		}
		subject, err := subjectOf(tree, demand)
		return &receipt.Requirement{Kind: types.ReceiptKindGate, Instrument: types.InstrumentRef{Name: "go-test", Version: "1.0.0", Digest: instrumentDigest}, Subject: subject}, err
	}
}

// env evaluates under gateNeed, with the owed-answer fake's answers.
func (g *graphFix) env(tree string, demand types.Digest) graph.Env {
	return graph.Env{Need: gateNeed(tree, demand), Blobs: g.blobs, Authority: owedanswers.Authority{}, Families: reviewers}
}

// commit is the step of committing a clean result of kind for n under env:
// empty/completed_clean, or the re-validation's refusal.
func (g *graphFix) commit(n types.NodeID, kind types.ResultKind, env graph.Env) (Step, error) {
	st, _, err := g.node(n)
	rid, ierr := ids.New("res")
	if err = errors.Join(err, ierr); err != nil || st.Key == nil {
		return Step{}, errors.Join(err, errors.New("the node is not claimed"))
	}
	r := types.Result{ID: types.ResultID(rid), Computation: st.Key.Key, Kind: kind, Value: []types.Ref{},
		Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
	err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, graph.Commit(tx, n, r, env) })
	var stale graph.ErrRevalidated
	switch {
	case err == nil:
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}, nil
	case errors.As(err, &stale):
		o := vocab.CouldNotRun // a receipt that no longer applies
		if stale.Reason == vocab.ReasonAuthorityRevoked || stale.Reason == vocab.ReasonStaleExpectedState {
			o = vocab.Failed
		}
		return Step{o, stale.Reason.String()}, nil
	}
	return Step{}, err
}

// revokedApprovalBlocksCommit: an approval revoked between claim and commit
// blocks the commit (13 [G4]). A review owes a ruling: the evaluator holds
// it on awaiting_input, the answer clears the hold and claims it, and once
// the approval is revoked the commit is refused as authority_revoked.
func revokedApprovalBlocksCommit(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	const owed, approval = types.OwedInputID("own_01m3250vz2ezn81cgvbkcn17jd"), types.ApprovalID("apr_01m3250vz2ezn81cgvbkcn17jd")
	// Round 1 asks for the ruling; its round 2 successor declares the answer.
	asker, err := g.record(types.NodeKindReview, soleReviewer, on("plan", g.plan, types.ResultKindPlan))
	answer := func(approval *types.ApprovalID) error {
		return g.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
			return owedanswers.Record(owedanswers.Answer{Owed: owed, Project: gProject, Asker: asker, Owner: types.ActorID(g.actor), Revision: 1, Approval: approval})
		})
	}
	env := g.env(treeA, demandA)
	if err == nil {
		err = answer(nil)
	}
	var review types.NodeID
	if err == nil {
		review, err = g.successorOf(asker, types.InputSpec{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: owed, Revision: 1}}})
	}
	if err != nil {
		return nil, nil, err
	}
	evaluate := func() error {
		return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{review}, env)
		})
	}
	if err := evaluate(); err != nil {
		return nil, nil, err
	}
	if st, _, err := g.node(review); err != nil || len(st.Holds) != 1 || st.Holds[0].Hold != "awaiting_input" {
		return nil, nil, errors.Join(err, errors.New("the review does not await its ruling"))
	}
	if err := errors.Join(answer(ptrTo(approval)), evaluate()); err != nil {
		return nil, nil, err
	}
	var o observe
	if st, _, err := g.node(review); err != nil || st.State != types.NodeStateStateClaimed {
		return nil, nil, errors.Join(err, errors.New("the answered review is not claimed"))
	}
	o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
	if err := answer(nil); err != nil { // revoked between claim and commit
		return nil, nil, err
	}
	st, err := g.commit(review, types.ResultKindReview, env)
	if err != nil {
		return nil, nil, err
	}
	o.steps = append(o.steps, st)
	return o.steps, o.violations, nil
}
