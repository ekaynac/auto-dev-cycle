// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// evidenceRetriesOnItsProbe: a could_not_run evidence result retries on
// its probe without operator invalidation (13 [G2]). The first attempt
// fails its precondition; the retry wait on the precondition_failed probe
// clears when the probe passes, and the second attempt, on the same key
// and generation, completes clean.
func evidenceRetriesOnItsProbe(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	n, err := g.record(types.NodeKindEvidence, evidenceParams, on("plan", g.plan, types.ResultKindPlan))
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Claim(tx, n, map[string]graph.Resolved{"plan": {Result: g.planRes}}, 1)
		})
	}
	before, _, serr := g.node(n)
	rid, rerr := ids.New("res")
	hid, herr := ids.New("hld")
	if err = errors.Join(err, serr, rerr, herr); err != nil {
		return nil, nil, err
	}
	failed := types.Result{ID: types.ResultID(rid), Computation: before.Key.Key, Kind: types.ResultKindEvidence, Value: []types.Ref{},
		Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonPreconditionFailed, Detail: "the precondition failed", Completed: false}
	hold := types.NodeHold{ID: types.HoldID(hid), SignalInstance: "probe:" + rid, Owner: types.OwnerEnvironmentOwner, OwnerRef: ptrTo("staging-owner"), Pages: []types.NodeHoldPagesItem{}}
	for _, fn := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.RecordResult(tx, types.ResultRecorded{Node: n, Result: failed}, g.blobs)
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Retry(tx, n, failed.ID, hold, nil) },
	} {
		if err := g.do(fn); err != nil {
			return nil, nil, err
		}
	}
	var o observe
	o.steps = append(o.steps, Step{vocab.Outcome(failed.Outcome), failed.Reason.String()})
	// The probe passes. A refused clear leaves the node held.
	if g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Clear(tx, n, hold.ID, hold.SignalInstance)
	}) == nil {
		if _, err := g.selectResult(n, types.ResultKindEvidence, []types.Ref{}, ptrTo(json.RawMessage(`{"tests":"pass"}`))); err != nil {
			return nil, nil, err
		}
	}
	after, _, err := g.node(n)
	invalidations, cerr := g.count(graph.NodeInvalidated)
	if err = errors.Join(err, cerr); err != nil {
		return nil, nil, err
	}
	if after.State == types.NodeStateStateCompleted {
		o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
	} else {
		o.steps = append(o.steps, Step{vocab.Outcome(failed.Outcome), failed.Reason.String()})
	}
	if invalidations != 0 || *after.Key != *before.Key {
		o.violations = append(o.violations, "the retry invalidated the key")
	}
	return o.steps, o.violations, nil
}

// expiredReceiptRenews: an expired receipt triggers renewal without
// resetting counters (13 [G1], [G2]). The gate's receipt has expired, so
// its review's input is missing; the renewal reopens the same key, and a
// fresher result, at the same generation and under the same claim, serves
// the review.
func expiredReceiptRenews(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	expired, fresh := types.Time("2026-01-01T00:00:00Z"), types.Time("2099-01-01T00:00:00Z")
	gate, err := g.gateOf(treeA, demandA, &expired)
	var review types.NodeID
	if err == nil {
		review, err = g.reviewOf(gate)
	}
	if err != nil {
		return nil, nil, err
	}
	before, command, err := g.node(gate)
	if err != nil {
		return nil, nil, err
	}
	var o observe
	now := time.Now()
	st, err := g.resolved(review, treeA, demandA, now)
	if err != nil {
		return nil, nil, err
	}
	o.steps = append(o.steps, st)
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Renew(tx, gate, g.blobs) }); err != nil {
		return nil, nil, err
	}
	ref, conclusions, err := g.gateReceipt(treeA, demandA, &fresh)
	if err == nil {
		_, err = g.selectResult(gate, types.ResultKindGateResult, []types.Ref{ref}, &conclusions)
	}
	if err != nil {
		return nil, nil, err
	}
	if st, err = g.resolved(review, treeA, demandA, now); err != nil {
		return nil, nil, err
	}
	o.steps = append(o.steps, st)
	after, again, err := g.node(gate)
	if err != nil {
		return nil, nil, err
	}
	if *after.Key != *before.Key {
		o.violations = append(o.violations, "the renewal moved the key to another generation")
	}
	if command == nil || again == nil || *command != *again {
		o.violations = append(o.violations, "the renewal reset the claim")
	}
	return o.steps, o.violations, nil
}

// evidenceParams are a walk's EvidenceNodeParams (12) on the plan's result.
var evidenceParams = json.RawMessage(`{"instrument":{"name":"walk","version":"1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},` +
	`"grant":"apr_01m3250v0aer9ss2097svmefj8","target":"tgt_01m3250v0aer9ss2097svmefj8","subject_input":"plan","demand":{"key":null},` +
	`"limits":{"max_attempts_per_demand":2,"max_attempts_per_subject":2,"deadline":"PT1H","cooldown":"PT0S"},` +
	`"restore":{"required":false,"procedure":null}}`)
