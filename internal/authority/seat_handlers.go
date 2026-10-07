// SPDX-License-Identifier: Apache-2.0

package authority

// The seat handlers (docs/design/m0/14 PR 6b; 08 "Supervisor seat"):
// seat.take and seat.release. Derive validates the scope and derives the
// seat as the command's object; Logic chooses the transition from the
// seat's current state and the caller, and SeatProjector admits it or the
// command fails. seat.take takes a free seat, asks for a held one (its
// force_after set here, never by the caller) and executes the caller's own
// request once it wins; only it is exempt from holding the seat.

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The seat actions' scope schemas, and the object type a proposal of one binds.
const (
	SeatTakeScope    = "api/scope/seat-take"
	SeatReleaseScope = "api/scope/seat-release"
	SeatObjectType   = "seat"
)

// SeatHandler is the handler of seat.take (Take) or seat.release.
type SeatHandler struct {
	Take bool
	// Grace overrides SeatMinGrace when positive (tests); it must be the
	// SeatProjector's, which refuses a shorter force_after.
	Grace time.Duration
}

// SeatHandlers are the seat actions' handlers, by action.
func SeatHandlers(grace time.Duration) map[string]Handler {
	return map[string]Handler{"seat.take": SeatHandler{Take: true, Grace: grace}, "seat.release": SeatHandler{Grace: grace}}
}

// Derive checks the scope against the action's schema, then derives the
// seat: its recorded id, or a candidate minted for this command, which
// Logic writes under (Tx.Bound), its record and its version
// (SeatVersion, which revocations move too). The seat is in no project and
// its transitions are in no change class.
func (h SeatHandler) Derive(tx *eventstore.Tx, _ types.Actor, input json.RawMessage) (Derived, error) {
	schema := SeatReleaseScope
	if h.Take {
		schema = SeatTakeScope
	}
	if err := contract.Validate(schema, input); err != nil {
		return Derived{}, err
	}
	candidate, err := NewSeatCandidate()
	if err != nil {
		return Derived{}, err
	}
	seat, err := SeatID(tx, candidate)
	return Derived{Object: seat, ObjectType: SeatObjectType, Record: SeatRecord, Version: SeatVersion, SeatExempt: h.Take}, err
}

// SeatRecord is the seat's record, its digest's definition (14 P3): its
// id, holder and pending request. It exists from the seat's first event.
func SeatRecord(tx *eventstore.Tx) (json.RawMessage, bool, error) {
	id, ok, err := loadJSON[string](tx, SeatProjection, "id")
	if err != nil || !ok {
		return nil, false, err
	}
	holder, held, err := SeatHolder(tx)
	req, pending, err2 := loadJSON[seatRequest](tx, SeatProjection, "request")
	if err := errors.Join(err, err2); err != nil {
		return nil, false, err
	}
	rec := struct {
		ID      string         `json:"id"`
		Holder  *types.ActorID `json:"holder"`
		Request *seatRequest   `json:"request"`
	}{ID: id}
	if held {
		rec.Holder = &holder
	}
	if pending {
		rec.Request = &req
	}
	b, err := json.Marshal(rec)
	return b, true, err
}

// Logic is the command's logic, which the request layer passes to Submit
// with c: the seat transition, under the bound seat id, or a refusal it
// records.
func (h SeatHandler) Logic(c eventstore.Command) eventstore.Decide {
	return func(tx *eventstore.Tx) (eventstore.Decision, error) {
		seat, bound := tx.Bound()
		if !bound {
			return eventstore.Decision{}, errors.New("authority: a seat command's logic runs only after its derivation")
		}
		by, err := liveActor(tx, types.ActorID(c.Actor))
		if err != nil {
			return eventstore.Decision{}, err
		}
		p, err := h.transition(tx, by)
		var r *Refusal
		if errors.As(err, &r) {
			b, err := json.Marshal(map[string]string{"outcome": "failed", "reason": r.Reason.String(), "detail": r.Detail})
			return eventstore.Decision{Disposition: eventstore.DispositionRefused, Result: b}, err
		}
		if err != nil {
			return eventstore.Decision{}, err
		}
		evs, err := NewSeatEvent(tx, seat, p)
		if err != nil {
			return eventstore.Decision{}, err
		}
		b, err := json.Marshal(map[string]any{"seat": seat, "transition": p.Transition, "force_after": p.ForceAfter})
		return eventstore.Decision{Disposition: eventstore.DispositionApplied, Events: evs, Result: b}, err
	}
}

// transition is the seat event the command asks for, or its refusal.
func (h SeatHandler) transition(tx *eventstore.Tx, by types.Actor) (types.SeatEvent, error) {
	holder, held, err := SeatHolder(tx)
	req, pending, err2 := loadJSON[seatRequest](tx, SeatProjection, "request")
	if err := errors.Join(err, err2); err != nil {
		return types.SeatEvent{}, err
	}
	grace := SeatProjector{MinGrace: h.Grace}.grace()
	now := tx.Now()
	switch {
	case !h.Take && !held:
		return types.SeatEvent{}, refusal(vocab.ReasonSeatNotHeld, "the seat is not held")
	case !h.Take: // by its holder or an operator; the guard refused any other supervisor
		return types.SeatEvent{Transition: types.SeatEventTransitionSeatReleased, From: &holder}, nil
	case by.Kind != types.ActorKindSupervisor:
		return types.SeatEvent{}, refusal(vocab.ReasonSeatUnavailable, "only a supervisor takes the seat")
	case !held:
		return types.SeatEvent{Transition: types.SeatEventTransitionSeatTaken, To: &by.ID}, nil
	case holder == by.ID:
		return types.SeatEvent{}, refusal(vocab.ReasonSeatUnavailable, "actor %s already holds the seat", by.ID)
	case pending && req.Event.To != nil && *req.Event.To == by.ID:
		if !forced(req, now.Format(time.RFC3339Nano), grace) {
			return types.SeatEvent{}, refusal(vocab.ReasonSeatUnavailable, "actor %s's handover request has not reached its force_after", by.ID)
		}
		return types.SeatEvent{Transition: types.SeatEventTransitionSeatHandedOver, From: &holder, To: &by.ID}, nil
	}
	// The request's recorded time is now (Tx.Now), so it wins at exactly
	// the grace after it.
	after := types.Time(now.Add(grace).Format(time.RFC3339Nano))
	return types.SeatEvent{Transition: types.SeatEventTransitionSeatHandoverRequested, From: &holder, To: &by.ID, ForceAfter: &after}, nil
}
