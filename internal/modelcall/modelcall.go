// SPDX-License-Identifier: Apache-2.0

// Package modelcall is the model_call effect kind (docs/design/m1/00-slice-plan.md P12a; 03 "External effects"; 05
// "Usage and settlement"): a model call's intent, durable before the gateway sends anything for it.
//
// Decisions:
//   - One effect per call (cal_), whose id is the call's suffix under eff_ (EffectOf). The call is the adapter's run
//     (09 `run`); the requests the gateway forwards for it are its sub-calls, sent under the one intent.
//   - The intent's request is the call's binding (records/model-call): call → attempt → lease → the lease's groups
//     (at least one), with the executor and the destination. The intent's target is the call itself, never the
//     shared destination, so I6's "one live operation per target" holds: calls to one destination are separate
//     targets, and a call is exclusive by its own id. The command that intends the effect also appends
//     model_call.intended with the same binding, and a projection keeps it by call and by lease. Both commit in one
//     writer transaction; the store runs synchronous=FULL, so the binding is on disk when Submit returns, and only
//     then may the gateway send (Recorder implements gateway.Intents). The fold admits the event only with its own
//     command's intent of the same request.
//   - The binding is checked inside the command against the lease and attempt projections: the lease is active on
//     the bound attempt, and that attempt is running, its unit's current attempt and the lease's (a stop leaves the
//     lease active on a stopping attempt). The groups are the lease's, never the caller's. The gateway checks the
//     same on every request (Reader), since a token's later requests skip the Recorder.
//   - A call is bound once. Intending it again is a no-op only while its effect is still pending under the same call,
//     attempt, lease and destination: a pending effect was intended since this start, because the Recorder refuses
//     to run on a store where MarkPendingUncertain has not run (eventstore.Store.RestartMarked). An
//     uncertain, completed or failed call is refused (ErrBound), so a call whose intent outlived a crash is never
//     sent again; and model_call is not idempotent, so the store never retries it either.
//   - Recovery reads the binding (OfLease): each call of a lease with its effect's current state, so an uncertain
//     call's charge stays on its groups (P12b keeps the liability; P20 reconciles).
//   - The Recorder also records the gateway's own count of each request it sent (gateway.Counts, P11b) as a usage
//     report of source gateway_count (gateway_floor when it read no usage), which an adapter's final must not fall
//     below to settle the call.
//   - Inert (§1's gating table): nothing in adcd registers the kind, the event or the projector; P22a does.
package modelcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

const (
	Kind     = "model_call"             // the effect kind
	Intended = "model_call.intended/v1" // the binding's event, subject the call
	Schema   = "records/model-call"
	Action   = "model_call.intend" // the Recorder's command
	Counted  = "model_call.count"  // the Recorder's command recording the gateway's count (P22a authorises both)

	// The binding's projections; internal/capacity/ledger, which reads them
	// and cannot import this package, holds the names.
	Projection      = ledger.BindingProjection      // the binding, by call
	LeaseProjection = ledger.LeaseBindingProjection // the binding, by lease/call
)

// EffectKind declares model_call: never retried (not idempotent),
// reconciled from the provider's usage or kept as a liability (03).
var EffectKind = eventstore.EffectKind{RequestSchema: Schema, Check: check}

// Effects registers the kind with the event store.
func Effects() eventstore.EffectKinds { return eventstore.EffectKinds{Kind: EffectKind} }

// Kinds registers Intended with the event store.
func Kinds() eventstore.Kinds { return eventstore.Kinds{Intended: {Schema: Schema}} }

// EffectOf is the id of call's model_call effect.
func EffectOf(call types.CallID) string { return "eff_" + strings.TrimPrefix(string(call), "cal_") }

// ErrNotRunning: the lease is not active running the bound attempt.
var ErrNotRunning = errors.New("modelcall: the lease is not running the bound attempt")

// ErrNoGroups: the lease reserves on no group, so the call would charge
// nothing.
var ErrNoGroups = errors.New("modelcall: the lease reserves on no group")

// ErrNoPositiveReservation: a group of the lease is reserved at zero, so
// the call's liability, were it never measured, would have nothing to stand
// for it (internal/capacity).
var ErrNoPositiveReservation = errors.New("modelcall: the lease reserves zero on a group")

// ErrSecondCall: the lease, which runs one attempt, already binds another
// call. One call per attempt is what lets the lease's reservation stand for
// an unmeasured call (internal/capacity).
var ErrSecondCall = errors.New("modelcall: the lease already binds another call")

// ErrBound: the call is already bound, and not pending under the same
// binding: it is never sent again.
var ErrBound = errors.New("modelcall: the call is already bound")

// check: the request, a valid records/model-call, is the binding of the
// call the intent's target names, with its groups sorted, so one binding
// has one digest.
func check(target string, request json.RawMessage) error {
	var m types.ModelCall
	if err := json.Unmarshal(request, &m); err != nil {
		return err
	}
	if string(m.Call) != target {
		return fmt.Errorf("modelcall: the call %s is not the intent's target %s", m.Call, target)
	}
	if !slices.IsSorted(m.Groups) {
		return errors.New("modelcall: the groups are not sorted")
	}
	return nil
}

// Intend binds b's call and intends its effect, inside a command's Apply.
// It returns the events to append: none when the call is already pending
// under the same binding.
func Intend(tx *eventstore.Tx, b gateway.Binding, destination types.Slug) ([]eventstore.Event, error) {
	if err := ids.Check(string(b.Call), "cal"); err != nil {
		return nil, fmt.Errorf("modelcall: call: %w", err)
	}
	l, _, ok, err := lease.LeaseOf(tx, b.Lease)
	switch {
	case err != nil:
		return nil, err
	case !ok || l.State != types.LeaseStateActive || l.Attempt == nil || *l.Attempt != b.Attempt:
		return nil, fmt.Errorf("%w: %s, %s", ErrNotRunning, b.Lease, b.Attempt)
	}
	if err := running(tx, l, b.Attempt); err != nil {
		return nil, err
	}
	m := types.ModelCall{Call: b.Call, Attempt: b.Attempt, Lease: b.Lease, Executor: l.Executor, Destination: destination, Groups: []types.ResourceGroupID{}}
	for _, r := range l.Reservations {
		if r.Amount <= 0 {
			return nil, fmt.Errorf("%w: %s on %s", ErrNoPositiveReservation, b.Lease, r.Group)
		}
		if !slices.Contains(m.Groups, r.Group) {
			m.Groups = append(m.Groups, r.Group)
		}
	}
	if len(m.Groups) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoGroups, b.Lease)
	}
	slices.Sort(m.Groups)
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if prev, seen, err := Of(tx, b.Call); err != nil {
		return nil, err
	} else if seen {
		e, err := tx.LoadEffect(EffectOf(b.Call))
		if err != nil {
			return nil, err
		}
		if e.State != eventstore.EffectPending || !same(prev, m) {
			return nil, fmt.Errorf("%w: %s is %s", ErrBound, b.Call, e.State)
		}
		return nil, nil
	}
	if others, err := tx.Keys(LeaseProjection, string(b.Lease)+"/"); err != nil {
		return nil, err
	} else if len(others) > 0 {
		return nil, fmt.Errorf("%w: %s binds %s", ErrSecondCall, b.Lease, others[0])
	}
	if err := tx.IntendEffect(EffectOf(b.Call), Kind, string(b.Call), raw); err != nil {
		return nil, err
	}
	id, err := ids.New("evt")
	return []eventstore.Event{{ID: id, Kind: Intended, Subject: string(b.Call), Payload: raw}}, err
}

// running: the attempt, under its active lease l, is running, its unit's
// current attempt, and l's. An active lease alone does not prove it: a stop
// leaves the lease active on a stopping attempt.
func running(tx *eventstore.Tx, l types.Lease, id types.AttemptID) error {
	a, current, ok, err := attemptOf(tx, id)
	switch {
	case err != nil:
		return err
	case !ok || !current || a.Status != types.AttemptStatusRunning || a.Lease != l.ID || a.Unit != l.Unit:
		return fmt.Errorf("%w: %s is not running", ErrNotRunning, id)
	}
	return nil
}

// attemptOf is the attempt and whether it is its unit's current one.
func attemptOf(tx *eventstore.Tx, id types.AttemptID) (types.Attempt, bool, bool, error) {
	a, _, ok, err := lease.AttemptOf(tx, id)
	if err != nil || !ok {
		return a, false, false, err
	}
	cur, has, err := lease.CurrentAttempt(tx, a.Unit)
	return a, has && cur == id, true, err
}

// Reader is the gateway's lease and attempt reader over the store's
// projections (gateway.Leases, gateway.Attempts).
type Reader struct{ Store *eventstore.Store }

var (
	_ gateway.Leases   = Reader{}
	_ gateway.Attempts = Reader{}
)

func (r Reader) Lease(ctx context.Context, id types.LeaseID) (l types.Lease, ok bool, err error) {
	err = r.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
		l, _, ok, err = lease.LeaseOf(tx, id)
		return err
	})
	return l, ok, err
}

func (r Reader) Attempt(ctx context.Context, id types.AttemptID) (a types.Attempt, current, ok bool, err error) {
	err = r.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
		a, current, ok, err = attemptOf(tx, id)
		return err
	})
	return a, current, ok, err
}

// same: the binding's identity, not its groups, which Intend re-derives
// from a lease whose reservations may have changed since.
func same(a, b types.ModelCall) bool {
	return a.Call == b.Call && a.Attempt == b.Attempt && a.Lease == b.Lease && a.Destination == b.Destination
}

// Project is an event store Project hook: it keeps each binding by call and
// by lease. A call is bound once; a second binding is refused, and so is a
// binding without its command's model_call intent of the same request.
func Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind != Intended {
			continue
		}
		var m types.ModelCall
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			return fmt.Errorf("modelcall: event %s: %w", ev.ID, err)
		}
		if ev.Subject != string(m.Call) {
			return fmt.Errorf("modelcall: event %s: subject %s is not its call %s", ev.ID, ev.Subject, m.Call)
		}
		if err := intended(tx, ev); err != nil {
			return fmt.Errorf("modelcall: event %s: %w", ev.ID, err)
		}
		if _, seen, err := tx.Get(Projection, string(m.Call)); err != nil {
			return err
		} else if seen {
			return fmt.Errorf("modelcall: event %s: %w: %s", ev.ID, ErrBound, m.Call)
		}
		if err := errors.Join(tx.Put(Projection, string(m.Call), ev.Payload),
			tx.Put(LeaseProjection, string(m.Lease)+"/"+string(m.Call), ev.Payload)); err != nil {
			return err
		}
	}
	return nil
}

// intended: ev has its call's model_call effect, intended by ev's command,
// on the call, with ev's payload as its request.
func intended(tx *eventstore.Tx, ev eventstore.Event) error {
	e, err := tx.LoadEffect(EffectOf(types.CallID(ev.Subject)))
	if err != nil {
		return err
	}
	d, err := eventstore.ContentDigest(ev.Payload)
	if err != nil {
		return err
	}
	if e.Kind != Kind || e.Target != ev.Subject || e.RequestDigest != d || ev.Command == nil || e.Command != *ev.Command {
		return errors.New("the binding is not its effect's request")
	}
	return nil
}

// Of is call's binding, if it is bound.
func Of(tx *eventstore.Tx, call types.CallID) (types.ModelCall, bool, error) {
	var m types.ModelCall
	raw, ok, err := tx.Get(Projection, string(call))
	if err != nil || !ok {
		return m, false, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, false, fmt.Errorf("modelcall: call %s: %w", call, err)
	}
	return m, true, nil
}

// Bound is a call's binding with its effect's current state.
type Bound struct {
	Call   types.ModelCall
	Effect eventstore.Effect
}

// OfLease is every call bound to lease, with its effect: what recovery reads
// to keep each call's charge on its groups.
func OfLease(tx *eventstore.Tx, l types.LeaseID) ([]Bound, error) {
	keys, err := tx.Keys(LeaseProjection, string(l)+"/")
	if err != nil {
		return nil, err
	}
	out := make([]Bound, 0, len(keys))
	for _, k := range keys {
		m, ok, err := Of(tx, types.CallID(strings.TrimPrefix(k, string(l)+"/")))
		if err == nil && !ok {
			err = fmt.Errorf("modelcall: %s is bound to lease %s but not by call", k, l)
		}
		if err != nil {
			return nil, err
		}
		e, err := tx.LoadEffect(EffectOf(m.Call))
		if err != nil {
			return nil, err
		}
		out = append(out, Bound{m, e})
	}
	return out, nil
}

// Recorder records a call's intent as its own command (gateway.Intents). It
// returns once the intent is committed, and so durable.
type Recorder struct {
	Store *eventstore.Store
	Actor string // the controller's system actor
}

var (
	_ gateway.Intents = Recorder{}
	_ gateway.Counts  = Recorder{}
)

func (r Recorder) Intend(ctx context.Context, b gateway.Binding, destination types.Slug) error {
	if r.Store == nil || !r.Store.RestartMarked() {
		return errors.New("modelcall: no store on which the restart rule has run")
	}
	id, err := ids.New("cmd")
	if err != nil {
		return err
	}
	input, err := json.Marshal(struct {
		Call        types.CallID    `json:"call"`
		Attempt     types.AttemptID `json:"attempt"`
		Lease       types.LeaseID   `json:"lease"`
		Destination types.Slug      `json:"destination"`
	}{b.Call, b.Attempt, b.Lease, destination})
	if err != nil {
		return err
	}
	_, err = r.Store.SubmitApplied(ctx, eventstore.Command{ID: id, Actor: r.Actor, Action: Action, IdempotencyKey: id, Input: input},
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := Intend(tx, b, destination)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

// Count records the gateway's count of one request it sent for b's call: a usage delta of source gateway_count (or
// gateway_floor, when the gateway read no usage), under the gateway's sequence for the call (internal/capacity/ledger
// keeps it apart from the adapter's reports). It returns once the report is committed. Usage is never fenced, so it
// needs no running attempt.
func (r Recorder) Count(ctx context.Context, b gateway.Binding, sequence int64, n gateway.Count) error {
	if r.Store == nil {
		return errors.New("modelcall: no store")
	}
	msr, err := ids.New("msr")
	if err != nil {
		return err
	}
	src := types.UsageMeasurementSourceOfTruthGatewayFloor // the upstream's usage unread: a floor only
	if n.Observed {
		src = types.UsageMeasurementSourceOfTruthGatewayCount
	}
	ev, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(msr), Call: b.Call, Attempt: b.Attempt, Lease: b.Lease,
		Kind: types.UsageMeasurementKindDelta, Sequence: sequence,
		Tokens:        types.UsageMeasurementTokens{In: n.In, Out: n.Out, CacheRead: n.CacheRead},
		Cost:          types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: src})
	if err != nil {
		return err
	}
	id, err := ids.New("cmd")
	if err != nil {
		return err
	}
	_, err = r.Store.SubmitApplied(ctx, eventstore.Command{ID: id, Actor: r.Actor, Action: Counted, IdempotencyKey: id, Input: ev.Payload},
		eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return []eventstore.Event{ev}, json.RawMessage(`{}`), nil
		}))
	return err
}
