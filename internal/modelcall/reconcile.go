// SPDX-License-Identifier: Apache-2.0

package modelcall

// The model_call reconciler (docs/design/m1/00-slice-plan.md P20, its model_call half; 03 "External effects": query
// the provider's usage, otherwise keep the usage as an uncertain liability; Q8; X3).
//
// Decisions:
//   - One runner per effect: Ended waits for a running one (a run end is never lost); Reconcile, while one runs,
//     returns ErrRunning. adcd holds one Reconciler per store (P22a). A reconciliation is recorded only at the sequence
//     its query was asked for, and a transition to completed rechecks the standing in its own transaction, so another
//     runner settles nothing twice and no run completes on a stale read.
//   - Run end (Ended): the effect leaves pending, completed when the call's usage settles (ledger.StandingOf), else
//     uncertain and reconciled. The gateway then sends nothing more for the call (Recorder.Count refuses a new floor).
//   - Reconcile: where the route has a usage query (Queries, by destination), the provider's answer is recorded as the
//     call's next provider_reconciliation, and the effect completes. It releases what the gateway's floors over-hold (an
//     image, a 4xx or 429 without usage, unread usage), never the gateway's observed counts: below them the call settles
//     at the counts, flagged (Standing.Below), and is asked again, the later answer superseding.
//   - The answer covers every request sent: the provider is asked only while no request with a floor and no count is
//     younger than FloorGrace, by the floor's recorded time. The gateway ends every request within
//     gateway.RequestDeadline, and a request still being served has such a floor (its count follows within the
//     deadline), so this holds in process and after a restart alike, with no in-memory flag.
//   - Otherwise (no query, a request possibly in flight, no usage found, which is never read as zero, a failed
//     query) the effect stays uncertain, and P12b keeps the liability: never dropped and never zero.
//   - Late usage (#226 entry 59) is expected: the ledger records it and the liability recomputes, whatever the effect.
//   - Inert: P22a constructs the Reconciler and authorises Reconciled for the system actor only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// Reconciled is the command recording a call's provider reconciliation.
const Reconciled = ledger.ReconcileAction

// FloorGrace is how long after its floor a request with no count may still be in flight: the gateway's deadline, with
// a margin for the floor's commit before the send.
const FloorGrace = gateway.RequestDeadline + time.Minute

// What the reconciler observes, as recorded on the effect.
const (
	ObservedSettled  = "the call's usage settles"
	ObservedOpen     = "the run ended with the call's usage unsettled"
	ObservedQueried  = "the provider's usage is recorded"
	ObservedNoQuery  = "the route has no usage query: the liability is kept"
	ObservedYoung    = "a request of the call may still be in flight upstream: the liability is kept"
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

// ErrRunning: another runner is settling the effect.
var ErrRunning = errors.New("modelcall: the effect's runner is running")

var errUnsettled = errors.New("modelcall: the call's usage does not settle")

// Reconciler settles model_call effects. Use one per store, by pointer.
type Reconciler struct {
	Store   *eventstore.Store
	Actor   string               // the controller's system actor
	Queries map[types.Slug]Query // by destination: the routes whose usage can be queried
	Now     func() time.Time     // nil: time.Now

	mu      sync.Mutex
	done    *sync.Cond
	running map[types.CallID]bool
}

// Ended settles call's effect once its run is over, waiting for a runner already settling it. An effect out of
// pending is reconciled as Reconcile does.
func (r *Reconciler) Ended(ctx context.Context, call types.CallID) (eventstore.Effect, error) {
	r.lock(call, true)
	defer r.unlock(call)
	return r.run(ctx, call, true)
}

// Reconcile settles call's uncertain effect, and asks again for a completed one whose usage no longer settles, or
// settles below the gateway's counts. A pending effect is left alone: Ended moves it.
func (r *Reconciler) Reconcile(ctx context.Context, call types.CallID) (eventstore.Effect, error) {
	if !r.lock(call, false) {
		return eventstore.Effect{}, fmt.Errorf("%w: %s", ErrRunning, call)
	}
	defer r.unlock(call)
	return r.run(ctx, call, false)
}

// view is what a run reads of a call at its start.
type view struct {
	m      types.ModelCall
	e      eventstore.Effect
	st     ledger.Standing
	recons int64 // the provider reconciliations recorded
	young  bool  // a request with a floor and no count is younger than FloorGrace
}

func (r *Reconciler) run(ctx context.Context, call types.CallID, ended bool) (eventstore.Effect, error) {
	v, err := r.read(ctx, call)
	e := v.e
	switch {
	case err != nil || e.State == eventstore.EffectPending && !ended:
		return e, err
	case e.State == eventstore.EffectPending:
		if v.st.Settled {
			if done, err := r.record(ctx, call, "observed", "", ObservedSettled); !errors.Is(err, errUnsettled) {
				return done, err
			}
		}
		if e, err = r.record(ctx, call, "uncertain", "", ObservedOpen); err != nil {
			return e, err
		}
	}
	if e.State != eventstore.EffectUncertain && (e.State != eventstore.EffectCompleted || v.st.Settled && !v.st.Below) {
		return e, nil
	}
	why := ObservedSettled
	if !v.st.Settled || v.st.Below {
		var asked string
		if asked, err = r.query(ctx, v); asked == ObservedQueried || !v.st.Settled {
			why = asked
		}
	}
	if e.State != eventstore.EffectUncertain {
		return e, err
	}
	if why == ObservedSettled || why == ObservedQueried {
		done, rerr := r.record(ctx, call, "reconciled", "completed", why)
		if !errors.Is(rerr, errUnsettled) {
			return done, errors.Join(err, rerr)
		}
		why = ObservedOpen // the standing moved under it: it stays uncertain
	}
	if e.LastObservation == why {
		return e, err // nothing changed: nothing to record
	}
	e, rerr := r.record(ctx, call, "reconciled", "still_uncertain", why)
	return e, errors.Join(err, rerr)
}

// read is call's binding, effect and standing, with its reconciliations and whether a floor is young.
func (r *Reconciler) read(ctx context.Context, call types.CallID) (v view, err error) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	err = r.Store.View(ctx, func(tx *eventstore.Tx) error {
		var ok bool
		var ms []types.UsageMeasurement
		v.m, ok, err = Of(tx, call)
		if err == nil && !ok {
			err = fmt.Errorf("modelcall: %s is not bound", call)
		}
		if err == nil {
			v.e, err = tx.LoadEffect(EffectOf(call))
		}
		if err == nil {
			ms, err = ledger.Measurements(tx, call)
		}
		if err != nil {
			return err
		}
		v.st, v.recons = ledger.StandingOf(ms, call), reconciliations(ms)
		at0, counted := now(), map[int64]bool{}
		for _, m := range ms {
			counted[m.Sequence] = counted[m.Sequence] || m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayCount
		}
		for _, m := range ms {
			if m.SourceOfTruth != types.UsageMeasurementSourceOfTruthGatewayFloor || counted[m.Sequence] {
				continue
			}
			var at time.Time
			raw, ok, err := tx.Get(FloorProjection, floorKey(call, m.Sequence))
			if err == nil && ok {
				err = json.Unmarshal(raw, &at)
			}
			if err != nil {
				return err
			}
			v.young = v.young || at.IsZero() || at0.Sub(at) < FloorGrace // no recorded time: assume in flight
		}
		return nil
	})
	return v, err
}

// query asks v's route for its call's usage and records the answer as the call's next provider reconciliation,
// returning what was observed: ObservedQueried once it is recorded, ObservedSettled when another runner recorded one
// meanwhile.
func (r *Reconciler) query(ctx context.Context, v view) (string, error) {
	q, ok := r.Queries[v.m.Destination]
	switch {
	case !ok:
		return ObservedNoQuery, nil
	case v.young:
		return ObservedYoung, nil
	}
	u, found, err := q.Usage(ctx, v.m)
	switch {
	case err != nil:
		return ObservedFailed, err
	case !found:
		return ObservedNotFound, nil
	}
	recorded, err := r.reconciled(ctx, v.m, u, v.recons)
	switch {
	case err != nil:
		return ObservedFailed, err
	case !recorded:
		return ObservedSettled, nil
	}
	return ObservedQueried, nil
}

// reconciled records u as m's provider reconciliation at sequence seq, superseding the earlier ones, and reports
// whether it did: not when another was recorded since the query was asked.
func (r *Reconciler) reconciled(ctx context.Context, m types.ModelCall, u Usage, seq int64) (bool, error) {
	msr, err := ids.New("msr")
	if err != nil {
		return false, err
	}
	ev, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(msr), Call: m.Call, Attempt: m.Attempt, Lease: m.Lease,
		Kind: types.UsageMeasurementKindProviderReconciliation, Sequence: seq,
		Tokens:        types.UsageMeasurementTokens{In: u.In, Out: u.Out, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite},
		Cost:          types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthProviderUsage})
	id, ierr := ids.New("cmd")
	if err = errors.Join(err, ierr); err != nil {
		return false, err
	}
	recorded := false
	_, err = r.Store.SubmitApplied(ctx, eventstore.Command{ID: id, Actor: r.Actor, Action: Reconciled, IdempotencyKey: id, Input: ev.Payload},
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			ms, err := ledger.Measurements(tx, m.Call)
			if recorded = err == nil && reconciliations(ms) == seq; !recorded {
				return nil, json.RawMessage(`{}`), err
			}
			return []eventstore.Event{ev}, json.RawMessage(`{}`), nil
		}))
	return recorded && err == nil, err
}

// reconciliations counts the provider reconciliations among ms.
func reconciliations(ms []types.UsageMeasurement) (n int64) {
	for _, m := range ms {
		if m.Kind == types.UsageMeasurementKindProviderReconciliation {
			n++
		}
	}
	return n
}

// record applies a transition to call's effect; one to completed only while the call's usage settles, rechecked in
// the transition's transaction (errUnsettled otherwise).
func (r *Reconciler) record(ctx context.Context, call types.CallID, kind, outcome, why string) (eventstore.Effect, error) {
	tr := eventstore.Transition{Kind: kind, Outcome: outcome, Observation: why}
	if kind == "observed" || outcome == "completed" {
		tr.Check = func(tx *eventstore.Tx) error {
			ms, err := ledger.Measurements(tx, call)
			if err == nil && !ledger.StandingOf(ms, call).Settled {
				err = errUnsettled
			}
			return err
		}
	}
	return r.Store.RecordEffect(ctx, r.Actor, EffectOf(call), tr)
}

// lock makes the caller call's one runner, waiting for another when wait is set, else failing while one runs.
func (r *Reconciler) lock(call types.CallID, wait bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done == nil {
		r.done, r.running = sync.NewCond(&r.mu), map[types.CallID]bool{}
	}
	for r.running[call] {
		if !wait {
			return false
		}
		r.done.Wait()
	}
	r.running[call] = true
	return true
}

func (r *Reconciler) unlock(call types.CallID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, call)
	r.done.Broadcast()
}
