// SPDX-License-Identifier: Apache-2.0

// Package modelcall is the model_call effect kind (docs/design/m1/00-slice-plan.md P12a; 03 "External effects"; 05
// "Usage and settlement"): a model call's intent, durable before the gateway sends anything for it.
//
// Decisions:
//   - One effect per call (cal_), whose id is the call's suffix under eff_ (EffectOf). The call is the adapter's run
//     (09 `run`); the requests the gateway forwards for it are its sub-calls, sent under the one intent.
//   - The intent's request is the call's binding (records/model-call): call → attempt → lease → the lease's groups,
//     with the executor and the destination, which is also the intent's target. The command that intends the effect
//     also appends model_call.intended with the same binding, and a projection keeps it by call and by lease. Both
//     commit in one writer transaction; the store runs synchronous=FULL, so the binding is on disk when Submit
//     returns, and only then may the gateway send (Recorder implements gateway.Intents).
//   - The binding is checked inside the command against the lease's projection: active, and running the bound
//     attempt. The groups are the lease's, never the caller's.
//   - A call is bound once. Intending it again is a no-op only while its effect is still pending with the same
//     binding: a pending effect was intended since this start, as the start marks every pending effect uncertain. An
//     uncertain, completed or failed call is refused (ErrBound), so a call whose intent outlived a crash is never
//     sent again; and model_call is not idempotent, so the store never retries it either.
//   - Recovery reads the binding (OfLease): each call of a lease with its effect's current state, so an uncertain
//     call's charge stays on its groups (P12b keeps the liability; P20 reconciles).
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

	Projection      = "model_calls"       // the binding, by call
	LeaseProjection = "model_call_leases" // the binding, by lease/call
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

// ErrBound: the call is already bound, and not pending under the same
// binding: it is never sent again.
var ErrBound = errors.New("modelcall: the call is already bound")

// check: the request, a valid records/model-call, names the intent's target
// as its destination, with its groups sorted, so one binding has one digest.
func check(target string, request json.RawMessage) error {
	var m types.ModelCall
	if err := json.Unmarshal(request, &m); err != nil {
		return err
	}
	if m.Destination != target {
		return fmt.Errorf("modelcall: the destination %s is not the intent's target %s", m.Destination, target)
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
	m := types.ModelCall{Call: b.Call, Attempt: b.Attempt, Lease: b.Lease, Executor: l.Executor, Destination: destination, Groups: []types.ResourceGroupID{}}
	for _, r := range l.Reservations {
		if !slices.Contains(m.Groups, r.Group) {
			m.Groups = append(m.Groups, r.Group)
		}
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
	if err := tx.IntendEffect(EffectOf(b.Call), Kind, destination, raw); err != nil {
		return nil, err
	}
	id, err := ids.New("evt")
	return []eventstore.Event{{ID: id, Kind: Intended, Subject: string(b.Call), Payload: raw}}, err
}

func same(a, b types.ModelCall) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Project is an event store Project hook: it keeps each binding by call and
// by lease. A call is bound once; a second binding is refused.
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

var _ gateway.Intents = Recorder{}

func (r Recorder) Intend(ctx context.Context, b gateway.Binding, destination types.Slug) error {
	if r.Store == nil {
		return errors.New("modelcall: no store")
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
