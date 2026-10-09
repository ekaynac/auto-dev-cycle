// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/executors"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The fixtures' executors: exeA and exeB are claude, exeC is gpt, and the
// registry does not hold exeX.
const (
	exeA = types.ExecutorID("exe_01m3250v9xft08ssr4ygq6g2np")
	exeB = types.ExecutorID("exe_01m3250vkwffc979nzjw0g2sf9")
	exeC = types.ExecutorID("exe_01m3250vm7f6ktw39dszqnyddr")
	exeX = types.ExecutorID("exe_01m3250vnfeyrv67x6w0ddqr6a")
)

// reviewers is the fixtures' executor registry (the fake of 11).
var reviewers, reviewersErr = executors.New(map[types.ExecutorID]string{exeA: "claude", exeB: "claude", exeC: "gpt"})

// rosterOf are the params of a review by exes, families from reviewers.
func rosterOf(exes ...types.ExecutorID) (json.RawMessage, error) {
	if reviewersErr != nil {
		return nil, reviewersErr
	}
	roster, err := graph.Roster(exes, reviewers)
	if err != nil {
		return nil, err
	}
	return json.Marshal(types.ReviewNodeParams{Subject: "candidate", Roster: roster})
}

// soleReviewer are the params of a review by exeA.
var soleReviewer = json.RawMessage(`{"subject":"candidate","roster":[{"executor":"` + string(exeA) + `","family":"claude"}]}`)

// otherReviewer are the params of another review, by exeB: of another
// subject, whose requirement is its own (one selected revision per
// subject, G7b-2).
var otherReviewer = json.RawMessage(`{"subject":"other","roster":[{"executor":"` + string(exeB) + `","family":"claude"}]}`)

// rosterChangeRunsReview: equal gate conclusions never reuse a review across
// a roster change (13 "Nothing reuses a review across rosters"; plan 15
// G6a). A review by exeA ran on a gate's result; the gate's next result
// concludes the same. A review of the same roster on the gate need not run;
// one whose member is another executor of the same family, or with a
// member added, runs. A roster naming an executor the registry does not
// hold is never recorded: unknown family is not independent (06 rule 3).
func rosterChangeRunsReview(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	env := g.env(treeA, demandA)
	ref, conclusions, err := g.gateReceipt(treeA, demandA, nil)
	var gate, prior types.NodeID
	var used, fresh types.ResultID
	if err == nil {
		gate, used, err = g.complete(types.NodeKindGate, json.RawMessage(`{}`), on("plan", g.plan, types.ResultKindPlan),
			map[string]graph.Resolved{"plan": {Result: g.planRes}}, types.ResultKindGateResult, []types.Ref{ref}, &conclusions)
	}
	if err == nil {
		prior, err = g.reviewOf(gate)
	}
	evaluate := func(n types.NodeID) error {
		return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Evaluate(tx, []types.NodeID{n}, env) })
	}
	if err == nil {
		err = evaluate(prior)
	}
	if err == nil {
		_, err = g.commit(prior, types.ResultKindReview, env)
	}
	if err == nil {
		ref, conclusions, err = g.gateReceipt(treeA, demandA, nil)
	}
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Invalidate(tx, gate) })
	}
	if err == nil {
		fresh, err = g.selectResult(gate, types.ResultKindGateResult, []types.Ref{ref}, &conclusions)
	}
	if err != nil {
		return nil, nil, err
	}
	// A subject has one selected requirement revision, and only a
	// supersession moves it (G6b-1): the other rosters review subjects of
	// their own, so their requirements are revisions of their own.
	for i, roster := range [][]types.ExecutorID{{exeA}, {exeB}, {exeA, exeC}} {
		params, err := rosterOf(roster...)
		if i > 0 && err == nil {
			params = json.RawMessage(strings.Replace(string(params), `"subject":"candidate"`, fmt.Sprintf(`"subject":"candidate-%d"`, i), 1))
		}
		var next types.NodeID
		if err == nil {
			next, err = g.record(types.NodeKindReview, params, on("gate", gate, types.ResultKindGateResult))
		}
		if err == nil {
			err = evaluate(next)
		}
		var same bool
		if err == nil {
			err = g.do(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
				same, err = graph.ReviewReusable(tx, prior, next, "gate", used, fresh, env)
				return nil, err
			})
		}
		if err != nil {
			return nil, nil, err
		}
		st := changedStep
		if same {
			st = Step{vocab.Empty, vocab.ReasonConclusionsUnchanged.String()}
		}
		steps = append(steps, st)
	}
	unknown := json.RawMessage(`{"subject":"candidate","roster":[{"executor":"` + string(exeX) + `","family":"claude"}]}`)
	st := cleanStep
	var unknownFamily graph.ErrNotIndependent
	if _, err := g.record(types.NodeKindReview, unknown, on("gate", gate, types.ResultKindGateResult)); errors.As(err, &unknownFamily) {
		st = Step{vocab.CouldNotRun, vocab.ReasonReviewerNotIndependent.String()}
	} else if err != nil {
		return nil, nil, err
	}
	return append(steps, st), nil, nil
}
