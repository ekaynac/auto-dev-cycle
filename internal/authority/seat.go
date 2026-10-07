// SPDX-License-Identifier: Apache-2.0

package authority

// The supervisor seat (docs/plan/02-supervisor-role §5, docs/design/m0/08
// "Supervisor seat"): one per controller, held by at most one live
// supervisor, and only its holder performs supervisor writes (the guard in
// Authorizer.Authorize). Seat events are a fold over the events and the
// projections alone, like grants, so a rebuild reaches the same holder.
// Revoking the holder's actor frees the seat in the same fold.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// SeatEventSchema is the payload schema of every seat event.
const SeatEventSchema = "records/seat-event"

// SeatProjection holds the seat's id (key "id"), its holder (key
// "holder", an actor id), the pending handover request (key "request",
// a seatRequest) and its version (key "version", SeatVersion). The controller has one seat: its first event mints its
// id (sea_…), and every seat event after has that subject.
const SeatProjection = "seat"

// SeatMinGrace is the least time a forced handover leaves the holder to
// finish or park its action and write its handover note (02 §5). 08 lets
// the requester choose force-after but sets no floor; this is that floor.
// The /v1 seat events replay against it: a rebuild re-checks recorded
// history with it, so it is never edited silently. Changing it needs a
// versioned policy recorded with the events it applies to.
const SeatMinGrace = 15 * time.Minute

// seatRequest is a pending handover request and when it was recorded.
type seatRequest struct {
	Event types.SeatEvent `json:"event"`
	At    string          `json:"at"`
}

var seatTransitions = []types.SeatEventTransition{types.SeatEventTransitionSeatTaken, types.SeatEventTransitionSeatReleased,
	types.SeatEventTransitionSeatHandoverRequested, types.SeatEventTransitionSeatHandedOver}

// SeatKinds registers the seat event kinds with the event store.
func SeatKinds() eventstore.Kinds {
	k := eventstore.Kinds{}
	for _, t := range seatTransitions {
		k[string(t)+"/v1"] = eventstore.Kind{Schema: SeatEventSchema}
	}
	return k
}

// NewSeatCandidate mints the id a command gives the seat if it has none
// yet. Mint it once per command, before Submit, and pass the same value to
// the derivation and to NewSeatEvent: on a fresh seat, the object a
// command binds and the subject it writes are then one id.
func NewSeatCandidate() (string, error) { return ids.New("sea") }

// SeatID is the seat's id, the subject of its events: the recorded one,
// or candidate (a NewSeatCandidate id) before the seat's first event.
func SeatID(tx *eventstore.Tx, candidate string) (string, error) {
	id, ok, err := loadJSON[string](tx, SeatProjection, "id")
	if err != nil || ok {
		return id, err
	}
	return candidate, ids.Check(candidate, "sea")
}

// NewSeatEvent proposes the seat transition p, under SeatID(tx,
// candidate); SeatProjector admits it or refuses the command.
func NewSeatEvent(tx *eventstore.Tx, candidate string, p types.SeatEvent) ([]eventstore.Event, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	seat, err := SeatID(tx, candidate)
	if err != nil {
		return nil, err
	}
	id, err := ids.New("evt")
	if err != nil {
		return nil, err
	}
	return []eventstore.Event{{ID: id, Kind: string(p.Transition) + "/v1", Subject: seat, Payload: b}}, nil
}

// SeatHolder returns the seat's holder, if it is held.
func SeatHolder(tx *eventstore.Tx) (types.ActorID, bool, error) {
	return loadJSON[types.ActorID](tx, SeatProjection, "holder")
}

// holdsSeat is the guard: nil only when actor holds the seat.
func holdsSeat(tx *eventstore.Tx, actor types.ActorID) error {
	holder, held, err := SeatHolder(tx)
	switch {
	case err != nil:
		return err
	case !held || holder != actor:
		return refusal(vocab.ReasonSeatNotHeld, "actor %s does not hold the supervisor seat", actor)
	}
	return nil
}

// SeatProjector accepts seat events and frees the seat when its holder's
// actor is revoked; other kinds pass through. It runs after the identity
// projector (eventstore.ProjectEach), so it sees a revocation applied.
type SeatProjector struct {
	// MinGrace overrides SeatMinGrace when positive (tests); never shorter in production.
	MinGrace time.Duration
}

func (s SeatProjector) grace() time.Duration {
	if s.MinGrace > 0 {
		return s.MinGrace
	}
	return SeatMinGrace
}

// Project is the eventstore.Hooks.Project fold for the seat.
func (s SeatProjector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		var err error
		switch {
		case ev.Kind == identity.ActorRevoked+"/v1":
			var changed bool
			if changed, err = seatOnRevoked(tx, ev); err == nil && changed {
				err = putVersion(tx, ev)
			}
		case slices.ContainsFunc(seatTransitions, func(t types.SeatEventTransition) bool { return ev.Kind == string(t)+"/v1" }):
			var p types.SeatEvent
			if err = json.Unmarshal(ev.Payload, &p); err == nil {
				err = applySeat(tx, ev, p, s.grace())
			}
			if err == nil {
				err = putVersion(tx, ev)
			}
		}
		if err != nil {
			return fmt.Errorf("authority: event %s: seat: %w", ev.ID, err)
		}
	}
	return nil
}

// SeatVersion is the seat's version (14 P3): the seq of the last event
// that changed its holder or request, a seat event or an actor revocation
// alike, so an expectation sees every change; 0 before its first event.
func SeatVersion(tx *eventstore.Tx) (int64, error) {
	v, _, err := loadJSON[int64](tx, SeatProjection, "version")
	return v, err
}

func putVersion(tx *eventstore.Tx, ev eventstore.Event) error {
	b, err := json.Marshal(ev.Seq)
	if err != nil {
		return err
	}
	return tx.Put(SeatProjection, "version", b)
}

// seatOnRevoked: a revoked holder loses the seat, a revoked requester its
// request; changed reports either.
func seatOnRevoked(tx *eventstore.Tx, ev eventstore.Event) (bool, error) {
	var p types.IdentityEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil || p.Actor == nil {
		return false, errors.Join(err, errors.New("an actor revocation without its actor"))
	}
	holder, held, err := SeatHolder(tx)
	req, pending, err2 := loadJSON[seatRequest](tx, SeatProjection, "request")
	if err := errors.Join(err, err2); err != nil {
		return false, err
	}
	if held && holder == p.Actor.ID {
		return true, clearSeat(tx, true)
	}
	if pending && req.Event.To != nil && *req.Event.To == p.Actor.ID {
		return true, tx.Delete(SeatProjection, "request")
	}
	return false, nil
}

func clearSeat(tx *eventstore.Tx, holder bool) error {
	if holder {
		if err := tx.Delete(SeatProjection, "holder"); err != nil {
			return err
		}
	}
	return tx.Delete(SeatProjection, "request")
}

func applySeat(tx *eventstore.Tx, ev eventstore.Event, p types.SeatEvent, grace time.Duration) error {
	seat, known, err := loadJSON[string](tx, SeatProjection, "id")
	if err != nil {
		return err
	}
	if string(p.Transition)+"/v1" != ev.Kind || (known && ev.Subject != seat) || !strings.HasPrefix(ev.Subject, "sea_") {
		return fmt.Errorf("kind %s, subject %s carry %s", ev.Kind, ev.Subject, p.Transition)
	}
	if !known {
		b, _ := json.Marshal(ev.Subject)
		if err := tx.Put(SeatProjection, "id", b); err != nil {
			return err
		}
	}
	by, err := liveActor(tx, types.ActorID(ev.Actor))
	if err != nil {
		return err
	}
	holder, held, err := SeatHolder(tx)
	req, pending, err2 := loadJSON[seatRequest](tx, SeatProjection, "request")
	if err := errors.Join(err, err2); err != nil {
		return err
	}
	// Every transition but a take names the current holder as from.
	if p.Transition != types.SeatEventTransitionSeatTaken && (!held || p.From == nil || *p.From != holder) {
		return errors.New("the event does not name the seat's current holder")
	}
	switch p.Transition {
	case types.SeatEventTransitionSeatTaken:
		if held {
			return fmt.Errorf("the seat is held by %s; ask for a handover", holder)
		}
		if *p.To != by.ID || by.Kind != types.ActorKindSupervisor {
			return errors.New("a live supervisor takes the seat for itself")
		}
		return putHolder(tx, by.ID)
	case types.SeatEventTransitionSeatReleased:
		if by.ID != holder && by.Kind != types.ActorKindOperator {
			return errors.New("only the holder or an operator releases the seat")
		}
		return clearSeat(tx, true)
	case types.SeatEventTransitionSeatHandoverRequested:
		if *p.To != by.ID || by.Kind != types.ActorKindSupervisor || by.ID == holder {
			return errors.New("a live supervisor other than the holder asks for the seat for itself")
		}
		// The requester's force_after is never trusted: it leaves the holder
		// at least the grace from the request's recorded time. Both seat time
		// checks read only recorded event times, so a rebuild runs them alike.
		if p.ForceAfter != nil && !graced(ev.At, string(*p.ForceAfter), grace) {
			return fmt.Errorf("force_after leaves the holder less than %s", grace)
		}
		b, err := json.Marshal(seatRequest{Event: p, At: ev.At})
		if err != nil {
			return err
		}
		return tx.Put(SeatProjection, "request", b) // the latest request replaces any earlier one
	}
	// seat.handed_over: atomically from the holder to a live supervisor,
	// by the holder, an operator, or the requester once its request wins.
	to, err := liveActor(tx, *p.To)
	if err != nil || to.Kind != types.ActorKindSupervisor || to.ID == holder {
		return errors.Join(err, errors.New("the seat is handed to a live supervisor other than its holder"))
	}
	if by.ID != holder && by.Kind != types.ActorKindOperator && !(by.ID == to.ID && pending && *req.Event.To == to.ID && forced(req, ev.At, grace)) {
		return errors.New("only the holder, an operator or a requester past its force_after hands the seat over")
	}
	return putHolder(tx, to.ID)
}

// forced: req has a force_after that at (the forcing event's recorded
// time, so a rebuild decides alike) has reached, and that still leaves the
// holder the grace from the request's recorded time.
func forced(req seatRequest, at string, grace time.Duration) bool {
	if req.Event.ForceAfter == nil || !graced(req.At, string(*req.Event.ForceAfter), grace) {
		return false
	}
	return !later(string(*req.Event.ForceAfter), at)
}

// graced: after is at least grace past from.
func graced(from, after string, grace time.Duration) bool {
	f, err := time.Parse(time.RFC3339Nano, from)
	return err == nil && !later(f.Add(grace).Format(time.RFC3339Nano), after)
}

// later: a is after b; an unparsable time is later than anything.
func later(a, b string) bool {
	x, err1 := time.Parse(time.RFC3339Nano, a)
	y, err2 := time.Parse(time.RFC3339Nano, b)
	return err1 != nil || err2 != nil || x.After(y)
}

func putHolder(tx *eventstore.Tx, id types.ActorID) error {
	if err := clearSeat(tx, false); err != nil {
		return err
	}
	b, err := json.Marshal(id)
	if err != nil {
		return err
	}
	return tx.Put(SeatProjection, "holder", b)
}
