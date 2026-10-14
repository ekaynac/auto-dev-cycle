// SPDX-License-Identifier: Apache-2.0

package modelcall

// The model_call reconciler (docs/design/m1/00-slice-plan.md P20, its model_call half; 03 "External effects": query
// the provider's usage, otherwise keep the usage as an uncertain liability; Q8; X3).
//
// Decisions:
//   - One runner per effect (one Reconciler per store, P22a): Ended waits for it while its context allows; Reconcile
//     returns ErrRunning. Queries are bounded (QueryDeadline). A reconciliation records only at the sequence it asked
//     for, and a transition to completed rechecks the standing in its own transaction.
//   - Ended moves the effect out of pending (Recorder.Count then refuses a new floor), and reconciles it from a read
//     taken after that fence.
//   - The provider's answer (Queries, by destination) is recorded as the next provider_reconciliation, and never
//     settles the call below what the gateway observed (ledger.bound): below it, the call settles there, flagged
//     (Standing.Below), and is asked again only after a later gateway count; an unchanged answer is not recorded.
//   - It is asked only once every request with a floor and no count is older than FloorGrace, by the floor's
//     recorded time: the gateway ends each request within gateway.RequestDeadline, in process or across a restart.
//   - Otherwise the effect stays uncertain and P12b keeps the liability: never dropped, never zero. Late usage (#226
//     entry 59) is expected. Inert: P22a constructs it and authorises Reconciled for the system actor only.

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

// QueryDeadline bounds one usage query.
const QueryDeadline = time.Minute

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
	running map[types.CallID]chan struct{} // closed when the call's runner ends
	// asked is, by call, how many gateway counts were recorded when the provider was last asked: a call settled
	// below the gateway's observation is asked again only after a later count (in memory: after a restart, once more).
	asked map[types.CallID]int
}

// Ended settles call's effect once its run is over, waiting for a runner already settling it while ctx allows. An
// effect out of pending is reconciled as Reconcile does.
func (r *Reconciler) Ended(ctx context.Context, call types.CallID) (eventstore.Effect, error) {
	return r.run(ctx, call, true)
}

// Reconcile settles call's uncertain effect, and asks again for a completed one whose usage no longer settles, or
// settles below the gateway's observation with a count newer than its last answer. A pending effect is left alone.
func (r *Reconciler) Reconcile(ctx context.Context, call types.CallID) (eventstore.Effect, error) {
	return r.run(ctx, call, false)
}

// view is what a run reads of a call.
type view struct {
	m         types.ModelCall
	e         eventstore.Effect
	st        ledger.Standing
	next      int64 // the sequence the next reconciliation takes
	young     bool  // a request with a floor and no count is younger than FloorGrace
	counts    int   // the gateway counts recorded
	recounted bool  // one was recorded since the provider was last asked
}

// run settles call's effect as its one runner: Ended waits for another, Reconcile does not (lock).
func (r *Reconciler) run(ctx context.Context, call types.CallID, ended bool) (eventstore.Effect, error) {
	unlock, err := r.lock(ctx, call, ended)
	if err != nil {
		return eventstore.Effect{}, err
	}
	defer unlock()
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
		if _, err = r.record(ctx, call, "uncertain", "", ObservedOpen); err == nil {
			v, err = r.read(ctx, call) // the fence is up: no floor commits after this read
		}
		if e = v.e; err != nil {
			return e, err
		}
	}
	ask := !v.st.Settled || v.st.Below && v.recounted
	if e.State != eventstore.EffectUncertain && (e.State != eventstore.EffectCompleted || !ask) {
		return e, nil
	}
	why := ObservedSettled
	if ask {
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

// read is call's binding, effect and standing, with its reconciliations, counts and floors.
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
		v.st = ledger.StandingOf(ms, call)
		_, v.next = latestOf(ms)
		at0, counts := now(), map[int64]bool{}
		for _, m := range ms {
			c := m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayCount
			counts[m.Sequence] = counts[m.Sequence] || c
			if c {
				v.counts++
			}
		}
		r.mu.Lock()
		v.recounted = v.counts > r.asked[call]
		r.mu.Unlock()
		for _, m := range ms {
			if m.SourceOfTruth != types.UsageMeasurementSourceOfTruthGatewayFloor || counts[m.Sequence] {
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

// query asks v's route for its call's usage, within QueryDeadline, and records the answer as the call's next provider
// reconciliation, returning what was observed: ObservedQueried once it is recorded, ObservedSettled when another
// runner recorded one meanwhile or the answer is the latest one's.
func (r *Reconciler) query(ctx context.Context, v view) (string, error) {
	q, ok := r.Queries[v.m.Destination]
	switch {
	case !ok:
		return ObservedNoQuery, nil
	case v.young:
		return ObservedYoung, nil
	}
	qctx, cancel := context.WithTimeout(ctx, QueryDeadline)
	defer cancel()
	u, found, err := q.Usage(qctx, v.m)
	switch {
	case err != nil:
		return ObservedFailed, err
	case !found:
		return ObservedNotFound, nil
	}
	r.mu.Lock()
	if r.asked == nil {
		r.asked = map[types.CallID]int{}
	}
	r.asked[v.m.Call] = v.counts
	r.mu.Unlock()
	recorded, err := r.reconciled(ctx, v.m, u, v.next)
	switch {
	case err != nil:
		return ObservedFailed, err
	case !recorded:
		return ObservedSettled, nil
	}
	return ObservedQueried, nil
}

// reconciled records u as m's provider reconciliation at sequence seq, superseding the earlier ones, and reports
// whether it did: not when another was recorded since the query was asked, nor when u is the latest one's figure.
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
			last, next := latestOf(ms)
			if recorded = err == nil && next == seq && (last == nil || *last != u); !recorded {
				return nil, json.RawMessage(`{}`), err
			}
			return []eventstore.Event{ev}, json.RawMessage(`{}`), nil
		}))
	return recorded && err == nil, err
}

// latestOf is the latest provider reconciliation among ms, and the sequence the next one takes: one past the highest.
func latestOf(ms []types.UsageMeasurement) (last *Usage, next int64) {
	for _, m := range ms {
		if m.Kind == types.UsageMeasurementKindProviderReconciliation && m.Sequence >= next {
			t := m.Tokens
			last, next = &Usage{int64(t.In), int64(t.Out), int64(t.CacheRead), int64(t.CacheWrite)}, m.Sequence+1
		}
	}
	return last, next
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

// lock makes the caller call's one runner, waiting for another while wait is set and ctx allows, else failing
// (ErrRunning) while one runs. The func it returns ends the run.
func (r *Reconciler) lock(ctx context.Context, call types.CallID, wait bool) (func(), error) {
	for {
		r.mu.Lock()
		if r.running == nil {
			r.running = map[types.CallID]chan struct{}{}
		}
		busy, held := r.running[call]
		if !held {
			done := make(chan struct{})
			r.running[call] = done
			r.mu.Unlock()
			return func() { r.mu.Lock(); delete(r.running, call); r.mu.Unlock(); close(done) }, nil
		}
		r.mu.Unlock()
		if !wait {
			return nil, fmt.Errorf("%w: %s", ErrRunning, call)
		}
		select {
		case <-busy:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
