// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Projections: each lease and attempt with its revision, and each unit's
// current attempt.
const (
	LeaseProjection   = "leases"
	AttemptProjection = "attempts"
	UnitProjection    = "unit_attempts"
)

// Stale refuses a command that expected another revision of a record: the
// record changed since the caller read it (03 expected state).
type StaleRecord struct{ Detail string }

func (s *StaleRecord) Error() string {
	return "lease: " + vocab.ReasonStaleExpectedState.String() + ": " + s.Detail
}

type stored[T any] struct {
	Revision int64 `json:"revision"`
	Record   T     `json:"record"`
}

func load[T any](tx *eventstore.Tx, projection, id string) (T, int64, bool, error) {
	var s stored[T]
	raw, ok, err := tx.Get(projection, id)
	if err != nil || !ok {
		return s.Record, 0, false, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s.Record, 0, false, fmt.Errorf("lease: %s %s: %w", projection, id, err)
	}
	return s.Record, s.Revision, true, nil
}

func put[T any](tx *eventstore.Tx, projection, id string, revision int64, record T) error {
	raw, err := json.Marshal(stored[T]{revision, record})
	if err != nil {
		return err
	}
	return tx.Put(projection, id, raw)
}

// current loads a record a command expects at a revision.
func current[T any](tx *eventstore.Tx, projection, id string, expected int64) (T, error) {
	rec, rev, ok, err := load[T](tx, projection, id)
	switch {
	case err != nil:
		return rec, err
	case !ok:
		return rec, fmt.Errorf("lease: no %s record %s", projection, id)
	case rev != expected:
		return rec, &StaleRecord{fmt.Sprintf("%s %s is at revision %d, not %d", projection, id, rev, expected)}
	}
	return rec, nil
}

func newEvent(transition, subject, reason string, l *types.Lease, a *types.Attempt) (eventstore.Event, error) {
	id, err := ids.New("evt")
	if err != nil {
		return eventstore.Event{}, err
	}
	var r *string
	if reason != "" {
		r = &reason
	}
	payload, err := json.Marshal(struct {
		Transition string         `json:"transition"`
		Reason     *string        `json:"reason"`
		Lease      *types.Lease   `json:"lease"`
		Attempt    *types.Attempt `json:"attempt"`
	}{transition, r, l, a})
	if err != nil {
		return eventstore.Event{}, err
	}
	return eventstore.Event{ID: id, Kind: transition + "/v1", Subject: subject, Payload: payload}, nil
}

// RequestLease records a new lease, in the requested state. That the lease
// is new and requested is the projector's check (and the schema's that a
// requested lease carries nothing else), in one place.
func RequestLease(tx *eventstore.Tx, l types.Lease) ([]eventstore.Event, error) {
	e, err := newEvent(Requested, string(l.ID), "", &l, nil)
	return []eventstore.Event{e}, err
}

// ChangeLease applies one lease transition to the lease at the expected
// revision; the new record is derived by the state machine from the
// projection, never taken from the caller. (attempt.started goes through
// StartAttempt: its payload must carry the attempt, which the schema
// requires.)
func ChangeLease(tx *eventstore.Tx, id types.LeaseID, expected int64, e LeaseEvent, evidenceTarget bool, holds map[string]types.Hold) ([]eventstore.Event, error) {
	l, err := current[types.Lease](tx, LeaseProjection, string(id), expected)
	if err != nil {
		return nil, err
	}
	n, err := ApplyLease(l, e, evidenceTarget, holds)
	if err != nil {
		return nil, err
	}
	ev, err := newEvent(e.Kind, string(id), e.Reason, &n, nil)
	return []eventstore.Event{ev}, err
}

// StartAttempt starts a unit's first attempt (token 1) under a granted
// lease: one attempt.started event carries the new attempt and the lease,
// now active.
func StartAttempt(tx *eventstore.Tx, leaseID types.LeaseID, expected int64, id types.AttemptID, base types.AttemptBase) ([]eventstore.Event, error) {
	l, err := current[types.Lease](tx, LeaseProjection, string(leaseID), expected)
	if err != nil {
		return nil, err
	}
	if _, ok, err := tx.Get(UnitProjection, string(l.Unit)); err != nil || ok {
		return nil, errors.Join(err, fmt.Errorf("lease: unit %s already has an attempt; supersede it", l.Unit))
	}
	return started(l, Start(id, l.Unit, leaseID, base), nil)
}

// SupersedeAttempt retires the unit's sealed attempt and starts its
// replacement, with the next token, under a granted lease: attempt.superseded
// and attempt.started in one command (05).
func SupersedeAttempt(tx *eventstore.Tx, old types.AttemptID, oldExpected int64, leaseID types.LeaseID, leaseExpected int64, id types.AttemptID, base types.AttemptBase) ([]eventstore.Event, error) {
	a, err := current[types.Attempt](tx, AttemptProjection, string(old), oldExpected)
	if err != nil {
		return nil, err
	}
	l, err := current[types.Lease](tx, LeaseProjection, string(leaseID), leaseExpected)
	if err != nil {
		return nil, err
	}
	if l.Unit != a.Unit {
		return nil, fmt.Errorf("lease: lease %s is for unit %s, the attempt for %s", leaseID, l.Unit, a.Unit)
	}
	retired, next, err := Supersede(a, id, leaseID, base)
	if err != nil {
		return nil, err
	}
	return started(l, next, &retired)
}

func started(l types.Lease, next types.Attempt, retired *types.Attempt) ([]eventstore.Event, error) {
	active, err := ApplyLease(l, LeaseEvent{Kind: AttemptStarted, Attempt: &next.ID}, false, nil)
	if err != nil {
		return nil, err
	}
	var evs []eventstore.Event
	if retired != nil {
		e, err := newEvent(Superseded, string(retired.ID), "", nil, retired)
		if err != nil {
			return nil, err
		}
		evs = append(evs, e)
	}
	e, err := newEvent(AttemptStarted, string(next.ID), "", &active, &next)
	return append(evs, e), err
}

// ChangeAttempt applies one attempt transition (not started or superseded)
// to the attempt at the expected revision.
func ChangeAttempt(tx *eventstore.Tx, id types.AttemptID, expected int64, e AttemptEvent) ([]eventstore.Event, error) {
	a, err := current[types.Attempt](tx, AttemptProjection, string(id), expected)
	if err != nil {
		return nil, err
	}
	n, err := ApplyAttempt(a, e)
	if err != nil {
		return nil, err
	}
	ev, err := newEvent(e.Kind, string(id), "", nil, &n)
	return []eventstore.Event{ev}, err
}

// FenceOutput fences an output against the unit's current attempt (05).
func FenceOutput(tx *eventstore.Tx, unit types.UnitID, id types.AttemptID, token int64) error {
	raw, ok, err := tx.Get(UnitProjection, string(unit))
	if err != nil {
		return err
	}
	if !ok {
		return &Stale{fmt.Sprintf("unit %s has no attempt", unit)}
	}
	var cur types.AttemptID
	if err := json.Unmarshal(raw, &cur); err != nil {
		return err
	}
	a, _, ok, err := load[types.Attempt](tx, AttemptProjection, string(cur))
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("lease: unit %s's attempt %s is missing", unit, cur))
	}
	return Fence(a, id, token)
}

// Project folds lease and attempt events into the projections. It checks
// each event on its own (so a rebuild replays it the same way): the kind is
// its payload's transition, the subject is its record, and the record's
// state follows from the previous one by the state machines. A command can
// therefore not write an arbitrary record by appending raw events.
func Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		var p struct {
			Transition string         `json:"transition"`
			Lease      *types.Lease   `json:"lease"`
			Attempt    *types.Attempt `json:"attempt"`
		}
		if !isLeaseKind(ev.Kind) {
			continue
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return fmt.Errorf("lease: event %s: %w", ev.ID, err)
		}
		if ev.Kind != p.Transition+"/v1" {
			return fmt.Errorf("lease: event %s is a %s carrying %s", ev.ID, ev.Kind, p.Transition)
		}
		subject := ""
		if p.Attempt != nil {
			subject = string(p.Attempt.ID)
		} else if p.Lease != nil {
			subject = string(p.Lease.ID)
		}
		if ev.Subject != subject {
			return fmt.Errorf("lease: event %s's subject %s is not its record %s", ev.ID, ev.Subject, subject)
		}
		if p.Lease != nil {
			if err := projectLease(tx, p.Transition, *p.Lease); err != nil {
				return fmt.Errorf("lease: event %s: %w", ev.ID, err)
			}
		}
		if p.Attempt != nil {
			if err := projectAttempt(tx, p.Transition, *p.Attempt); err != nil {
				return fmt.Errorf("lease: event %s: %w", ev.ID, err)
			}
		}
	}
	return nil
}

func isLeaseKind(kind string) bool {
	for _, t := range transitions {
		if kind == t+"/v1" {
			return true
		}
	}
	return false
}

func projectLease(tx *eventstore.Tx, transition string, n types.Lease) error {
	prev, rev, ok, err := load[types.Lease](tx, LeaseProjection, string(n.ID))
	if err != nil {
		return err
	}
	switch {
	case !ok && (transition != Requested || n.State != types.LeaseStateRequested):
		return fmt.Errorf("lease %s does not exist", n.ID)
	case ok && transition == Requested && prev.State != types.LeaseStateWaiting:
		return fmt.Errorf("lease %s already exists", n.ID)
	case ok:
		step := leaseStep{prev.State, transition}
		want, okTable := leaseTable[step]
		if !okTable {
			if want, okTable = plainSettle[step]; !okTable {
				want, okTable = restoreTable[step]
			}
		}
		if !okTable || want != n.State || prev.Unit != n.Unit || prev.Executor != n.Executor {
			return fmt.Errorf("lease %s cannot go from %s by %s to %s", n.ID, prev.State, transition, n.State)
		}
	}
	return put(tx, LeaseProjection, string(n.ID), rev+1, n)
}

func projectAttempt(tx *eventstore.Tx, transition string, n types.Attempt) error {
	prev, rev, ok, err := load[types.Attempt](tx, AttemptProjection, string(n.ID))
	if err != nil {
		return err
	}
	switch {
	case transition == AttemptStarted:
		if ok || n.Status != types.AttemptStatusRunning || n.Publication != types.AttemptPublicationOpen {
			return fmt.Errorf("attempt %s cannot start as %s", n.ID, n.Status)
		}
		raw, err := json.Marshal(n.ID)
		if err != nil {
			return err
		}
		if err := tx.Put(UnitProjection, string(n.Unit), raw); err != nil {
			return err
		}
	case !ok:
		return fmt.Errorf("attempt %s does not exist", n.ID)
	case transition == Superseded:
		if prev.Status != types.AttemptStatusSealed || n.Status != types.AttemptStatusSuperseded {
			return fmt.Errorf("attempt %s cannot be superseded from %s", n.ID, prev.Status)
		}
	default:
		want, okTable := attemptTable[attemptStep{prev.Status, transition}]
		if !okTable || want != n.Status || n.FencingToken != prev.FencingToken || n.Unit != prev.Unit {
			return fmt.Errorf("attempt %s cannot go from %s by %s to %s", n.ID, prev.Status, transition, n.Status)
		}
	}
	return put(tx, AttemptProjection, string(n.ID), rev+1, n)
}
