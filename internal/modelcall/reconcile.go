// SPDX-License-Identifier: Apache-2.0

package modelcall

// The model_call reconciler (docs/design/m1/00-slice-plan.md P20, its model_call half; 03 "External effects": query
// the provider's usage, otherwise keep the usage as an uncertain liability; Q8 "When usage cannot be queried"; X3).
//
// Decisions:
//   - One runner per effect: a Reconciler runs one settlement of an effect at a time. Another, asked while one runs,
//     does nothing and returns ErrRunning, so no query is asked twice and no transition races another. adcd holds
//     one Reconciler (P22a). The store's state machine refuses any transition from a settled state, and Reconciled's
//     command records a call's provider reconciliation once, so a second runner elsewhere settles nothing twice.
//   - Run end (Ended): the call's adapter run is over, so its effect leaves pending: completed (observed) when its
//     usage settles (ledger.StandingOf: a provider reconciliation, or a final the gateway's count bears out), else
//     uncertain, then reconciled as below. Once it has left pending, the gateway sends nothing more for the call
//     (Recorder.Count refuses a new floor).
//   - Reconciliation (Reconcile): a call whose usage settles is completed with no query. Otherwise, where its route
//     supports a usage query (Queries, by destination), the provider's answer is recorded as a
//     provider_reconciliation, the one report that releases an honest call the gateway's floors over-hold (an image,
//     a 4xx or 429 without usage, a response whose usage it could not read), and the effect completes. The query is
//     asked only once no request of the call is being served (gateway.Gateway.Serving), so the answer covers every
//     request sent for it. Without a query, with a request still served, with no usage at the provider for the call
//     (never read as zero), or with a failed query, the effect stays uncertain (still_uncertain), and its liability
//     stays where P12b keeps it: the larger of the lease's positive reservation, the deduplicated delta sum and the
//     gateway's count, never dropped and never zero.
//   - The reconciliation is durable before the effect completes, each in its own transaction. A crash between them
//     leaves the effect uncertain with its call settled, and the next run completes it with no second query.
//   - Late usage (#226 entry 59): a report on a call whose effect has left pending is expected. The ledger records it
//     and P12b's liability recomputes; the effect's state does not change. A completed effect whose call no longer
//     settles (a count recorded after the run end) is queried again, and its reconciliation releases the call.
//   - Inert: nothing in adcd constructs a Reconciler. P22a does, and authorises Reconciled.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// Reconciled is the command recording a call's provider reconciliation.
const Reconciled = "model_call.reconcile"

// What the reconciler observes, as recorded on the effect.
const (
	ObservedSettled  = "the call's usage settles"
	ObservedOpen     = "the run ended with the call's usage unsettled"
	ObservedQueried  = "the provider's usage is recorded"
	ObservedNoQuery  = "the route has no usage query: the liability is kept"
	ObservedServing  = "a request of the call is still served: the liability is kept"
	ObservedNotFound = "the provider holds no usage for the call: the liability is kept"
	ObservedFailed   = "the usage query failed: the liability is kept"
)

// Usage is what a provider reports a call used, in tokens.
type Usage struct{ In, Out, CacheRead, CacheWrite int64 }

// Query asks a route's provider for a call's usage, over every request sent for it. found is false when the provider
// holds no usage for the call: that is no evidence it used nothing.
type Query interface {
	Usage(ctx context.Context, call types.ModelCall) (u Usage, found bool, err error)
}

// Serving reports whether a request of a call is being served (gateway.Gateway.Serving).
type Serving interface {
	Serving(call types.CallID) bool
}

// ErrRunning: another runner is settling the effect.
var ErrRunning = errors.New("modelcall: the effect's runner is running")

// Reconciler settles model_call effects. Use one per store, by pointer.
type Reconciler struct {
	Store   *eventstore.Store
	Actor   string               // the controller's system actor
	Queries map[types.Slug]Query // by destination: the routes whose usage can be queried
	Gateway Serving              // nil: every call counts as served, so none is queried

	mu      sync.Mutex
	running map[types.CallID]bool
}

// Ended settles call's effect once its run is over: it leaves pending, completed or uncertain, and an uncertain one is
// reconciled. An effect already out of pending is reconciled as Reconcile does.
func (r *Reconciler) Ended(ctx context.Context, call types.CallID) (eventstore.Effect, error) {
	return r.run(ctx, call, true)
}

// Reconcile settles call's uncertain effect, and queries again for a completed one whose usage no longer settles. A
// pending effect is left alone: its run goes on, and Ended moves it.
func (r *Reconciler) Reconcile(ctx context.Context, call types.CallID) (eventstore.Effect, error) {
	return r.run(ctx, call, false)
}

func (r *Reconciler) run(ctx context.Context, call types.CallID, ended bool) (eventstore.Effect, error) {
	if !r.claim(call) {
		return eventstore.Effect{}, fmt.Errorf("%w: %s", ErrRunning, call)
	}
	defer r.release(call)
	var m types.ModelCall
	var st ledger.Standing
	var e eventstore.Effect
	err := r.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
		var ok bool
		if m, ok, err = Of(tx, call); err == nil && !ok {
			err = fmt.Errorf("modelcall: %s is not bound", call)
		}
		if err == nil {
			e, err = tx.LoadEffect(EffectOf(call))
		}
		if err == nil {
			st, err = standingOf(tx, call)
		}
		return err
	})
	switch {
	case err != nil || e.State == eventstore.EffectPending && !ended:
		return e, err
	case e.State == eventstore.EffectPending && st.Settled:
		return r.record(ctx, e.ID, eventstore.Transition{Kind: "observed", Observation: ObservedSettled})
	case e.State == eventstore.EffectPending:
		if e, err = r.record(ctx, e.ID, eventstore.Transition{Kind: "uncertain", Observation: ObservedOpen}); err != nil {
			return e, err
		}
	}
	if e.State != eventstore.EffectUncertain && (e.State != eventstore.EffectCompleted || st.Settled) {
		return e, nil
	}
	why, err := ObservedSettled, error(nil)
	if !st.Settled {
		why, err = r.query(ctx, m)
	}
	switch {
	case e.State != eventstore.EffectUncertain:
		return e, err
	case why == ObservedSettled || why == ObservedQueried:
		return r.record(ctx, e.ID, eventstore.Transition{Kind: "reconciled", Outcome: "completed", Observation: why})
	case e.LastObservation == why:
		return e, err // nothing changed: nothing to record
	}
	e, rerr := r.record(ctx, e.ID, eventstore.Transition{Kind: "reconciled", Outcome: "still_uncertain", Observation: why})
	return e, errors.Join(err, rerr)
}

// query asks m's route for m's usage and records the answer as m's provider reconciliation, returning what was
// observed: ObservedQueried once it is recorded.
func (r *Reconciler) query(ctx context.Context, m types.ModelCall) (string, error) {
	q, ok := r.Queries[m.Destination]
	switch {
	case !ok:
		return ObservedNoQuery, nil
	case r.Gateway == nil || r.Gateway.Serving(m.Call):
		return ObservedServing, nil
	}
	u, found, err := q.Usage(ctx, m)
	switch {
	case err != nil:
		return ObservedFailed, err
	case !found:
		return ObservedNotFound, nil
	}
	if err := r.reconciled(ctx, m, u); err != nil {
		return ObservedFailed, err
	}
	return ObservedQueried, nil
}

// reconciled records u as m's provider reconciliation, unless one is recorded already. The effect must have left
// pending: a running call may send again.
func (r *Reconciler) reconciled(ctx context.Context, m types.ModelCall, u Usage) error {
	msr, err := ids.New("msr")
	if err != nil {
		return err
	}
	ev, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(msr), Call: m.Call, Attempt: m.Attempt, Lease: m.Lease,
		Kind:          types.UsageMeasurementKindProviderReconciliation,
		Tokens:        types.UsageMeasurementTokens{In: u.In, Out: u.Out, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite},
		Cost:          types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthProviderUsage})
	if err != nil {
		return err
	}
	id, err := ids.New("cmd")
	if err != nil {
		return err
	}
	_, err = r.Store.SubmitApplied(ctx, eventstore.Command{ID: id, Actor: r.Actor, Action: Reconciled, IdempotencyKey: id, Input: ev.Payload},
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			e, err := tx.LoadEffect(EffectOf(m.Call))
			if err == nil && e.State == eventstore.EffectPending {
				err = fmt.Errorf("%w: %s is still running", ErrNotPending, m.Call)
			}
			st, serr := standingOf(tx, m.Call)
			if err = errors.Join(err, serr); err != nil || st.Settled { // settled already: by another runner, or a final
				return nil, json.RawMessage(`{}`), err
			}
			return []eventstore.Event{ev}, json.RawMessage(`{}`), nil
		}))
	return err
}

// record applies tr to effect id.
func (r *Reconciler) record(ctx context.Context, id string, tr eventstore.Transition) (eventstore.Effect, error) {
	return r.Store.RecordEffect(ctx, r.Actor, id, tr)
}

// standingOf is call's standing from its recorded reports.
func standingOf(tx *eventstore.Tx, call types.CallID) (ledger.Standing, error) {
	ms, err := ledger.Measurements(tx, call)
	return ledger.StandingOf(ms, call), err
}

// claim makes the caller call's one runner, unless another is.
func (r *Reconciler) claim(call types.CallID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[call] {
		return false
	}
	if r.running == nil {
		r.running = map[types.CallID]bool{}
	}
	r.running[call] = true
	return true
}

func (r *Reconciler) release(call types.CallID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, call)
}
