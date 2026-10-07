// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// seat proposes a seat transition.
func seat(tr types.SeatEventTransition, from, to *types.ActorID, forceAfter *time.Time) applyFn {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		p := types.SeatEvent{Transition: tr, From: from, To: to}
		if forceAfter != nil {
			at := types.Time(forceAfter.UTC().Format(time.RFC3339Nano))
			p.ForceAfter = &at
		}
		candidate, err := authority.NewSeatCandidate()
		if err != nil {
			return nil, err
		}
		return authority.NewSeatEvent(tx, candidate, p)
	}
}

// try is seed returning the writer's error instead of failing.
func (b *boundary) try(p identity.Principal, apply applyFn) error {
	cmd := newID(b.t, "cmd")
	_, err := b.s.SubmitApplied(context.Background(), p.Command(cmd, "test.seed", cmd, []byte(`{}`), nil), eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := apply(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

// supervisor enrols another supervisor and logs it in.
func (b *boundary) supervisor() identity.Principal {
	b.t.Helper()
	a := types.Actor{ID: types.ActorID(newID(b.t, "act")), Kind: types.ActorKindSupervisor, Name: "second", CreatedBy: b.op.Actor()}
	var tok identity.Secret
	b.seed(b.op, create(a))
	b.seed(b.op, issueTo(a.ID, b.op.Actor(), &tok))
	return b.login(tok)
}

func (b *boundary) holder() (types.ActorID, bool) {
	b.t.Helper()
	var id types.ActorID
	var held bool
	b.view(func(tx *eventstore.Tx) (err error) {
		id, held, err = authority.SeatHolder(tx)
		return err
	})
	return id, held
}

// rebuilds: the admitted history folds to the same projections again.
func (b *boundary) rebuilds() {
	b.t.Helper()
	if diffs, err := b.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		b.t.Errorf("the seat history does not rebuild: %v %v", diffs, err)
	}
}

func ref(id types.ActorID) *types.ActorID { return &id }

func TestSeatTakeAndRelease(t *testing.T) {
	b := openBoundary(t)
	sup, sup2, op := b.sup.Actor(), b.supervisor(), b.op.Actor()
	if h, held := b.holder(); !held || h != sup {
		t.Fatalf("the boundary's supervisor holds the seat: %s %t", h, held)
	}
	// One holder: a second take is refused, for the holder too.
	if b.try(sup2, seat(types.SeatEventTransitionSeatTaken, nil, ref(sup2.Actor()), nil)) == nil {
		t.Error("a take of a held seat")
	}
	if b.try(b.sup, seat(types.SeatEventTransitionSeatTaken, nil, ref(sup), nil)) == nil {
		t.Error("the holder taking it twice")
	}
	// Only the holder or an operator releases it.
	if b.try(sup2, seat(types.SeatEventTransitionSeatReleased, ref(sup), nil, nil)) == nil {
		t.Error("a release by a non-holder")
	}
	b.seed(b.op, seat(types.SeatEventTransitionSeatReleased, ref(sup), nil, nil))
	if _, held := b.holder(); held {
		t.Error("released by the operator, still held")
	}
	// An operator does not hold the seat; a supervisor takes it for itself only.
	if b.try(b.op, seat(types.SeatEventTransitionSeatTaken, nil, ref(op), nil)) == nil {
		t.Error("an operator took the seat")
	}
	if b.try(sup2, seat(types.SeatEventTransitionSeatTaken, nil, ref(sup), nil)) == nil {
		t.Error("a take for another actor")
	}
	b.seed(sup2, seat(types.SeatEventTransitionSeatTaken, nil, ref(sup2.Actor()), nil))
	b.seed(sup2, seat(types.SeatEventTransitionSeatReleased, ref(sup2.Actor()), nil, nil))
	b.rebuilds()
}

func TestSeatHandover(t *testing.T) {
	b := openBoundary(t)
	sup, sup2 := b.sup.Actor(), b.supervisor()
	to := sup2.Actor()
	for name, c := range map[string]struct {
		by       identity.Principal
		from, to types.ActorID
	}{
		"by a non-holder without a request": {sup2, sup, to},
		"not naming the holder":             {b.sup, to, to},
		"to an operator":                    {b.sup, sup, b.op.Actor()},
		"to the holder":                     {b.sup, sup, sup},
	} {
		if b.try(c.by, seat(types.SeatEventTransitionSeatHandedOver, &c.from, &c.to, nil)) == nil {
			t.Errorf("a handover %s", name)
		}
	}
	// Atomic: one event moves the seat, and the guard follows it.
	b.seed(b.sup, seat(types.SeatEventTransitionSeatHandedOver, ref(sup), &to, nil))
	if h, held := b.holder(); !held || h != to {
		t.Fatalf("after the handover: %s %t", h, held)
	}
	b.refusedUnrun("the former holder's write", b.noteCmd(b.sup, "x", prjA), isReason(vocab.ReasonSeatNotHeld))
	// An operator hands it back.
	b.seed(b.op, seat(types.SeatEventTransitionSeatHandedOver, &to, ref(sup), nil))
	b.rebuilds()
}

// Revoking the holder's actor frees the seat in the same fold; revoking a
// requester drops its request.
func TestSeatRevocation(t *testing.T) {
	b := openBoundary(t)
	sup, sup2, sup3 := b.sup.Actor(), b.supervisor(), b.supervisor()
	later := suiteNow().Add(time.Hour)
	b.seed(sup3, seat(types.SeatEventTransitionSeatHandoverRequested, ref(sup), ref(sup3.Actor()), &later))
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, sup, suiteNow()) })
	if _, held := b.holder(); held {
		t.Fatal("a revoked holder keeps the seat")
	}
	b.seed(sup2, seat(types.SeatEventTransitionSeatTaken, nil, ref(sup2.Actor()), nil))
	b.seed(sup3, seat(types.SeatEventTransitionSeatHandoverRequested, ref(sup2.Actor()), ref(sup3.Actor()), &later))
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, sup3.Actor(), suiteNow())
	})
	b.view(func(tx *eventstore.Tx) error {
		if _, pending, err := tx.Get(authority.SeatProjection, "request"); err != nil || pending {
			t.Errorf("a revoked requester's request stays: %t %v", pending, err)
		}
		return nil
	})
	b.rebuilds()
}

// force_after is never the requester's to shorten: under the default
// grace an immediate or short one is refused at the request.
func TestSeatForceAfterHasAFloor(t *testing.T) {
	b := openBoundary(t)
	sup, sup2 := b.sup.Actor(), b.supervisor()
	for _, d := range []time.Duration{-time.Minute, 0, time.Minute, authority.SeatMinGrace - time.Second} {
		at := suiteNow().Add(d)
		if b.try(sup2, seat(types.SeatEventTransitionSeatHandoverRequested, ref(sup), ref(sup2.Actor()), &at)) == nil {
			t.Errorf("force_after now%+v accepted", d)
		}
	}
	at := suiteNow().Add(authority.SeatMinGrace + time.Minute)
	b.seed(sup2, seat(types.SeatEventTransitionSeatHandoverRequested, ref(sup), ref(sup2.Actor()), &at))
	if b.try(sup2, seat(types.SeatEventTransitionSeatHandedOver, ref(sup), ref(sup2.Actor()), nil)) == nil {
		t.Error("forced before force_after")
	}
	b.rebuilds()
}

// A request wins only once its force_after (at least the grace after the
// request) has passed: refused before, applied after.
func TestSeatForcedHandover(t *testing.T) {
	const grace = 300 * time.Millisecond
	b := openBoundaryGrace(t, grace, true)
	sup, sup2 := b.sup.Actor(), b.supervisor()
	short := suiteNow().Add(grace / 3)
	if b.try(sup2, seat(types.SeatEventTransitionSeatHandoverRequested, ref(sup), ref(sup2.Actor()), &short)) == nil {
		t.Error("a force_after inside the grace")
	}
	at := suiteNow().Add(grace + 100*time.Millisecond)
	b.seed(sup2, seat(types.SeatEventTransitionSeatHandoverRequested, ref(sup), ref(sup2.Actor()), &at))
	force := seat(types.SeatEventTransitionSeatHandedOver, ref(sup), ref(sup2.Actor()), nil)
	if b.try(sup2, force) == nil {
		t.Fatal("forced inside the grace")
	}
	time.Sleep(at.Sub(suiteNow()) + 50*time.Millisecond)
	if err := b.try(sup2, force); err != nil {
		t.Fatalf("forced after force_after: %v", err)
	}
	if h, _ := b.holder(); h != sup2.Actor() {
		t.Errorf("the requester does not hold the seat: %s", h)
	}
	b.rebuilds()
}

// The guard: a supervisor write needs the seat; an operator's does not; an
// action marked SeatExempt is still authorized but needs no seat.
func TestSeatGuard(t *testing.T) {
	b := openBoundary(t)
	sup := b.sup.Actor()
	if out, err := b.note(b.noteCmd(b.sup, "held", prjA)); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the holder's write: %+v %v", out, err)
	}
	b.seed(b.sup, seat(types.SeatEventTransitionSeatReleased, ref(sup), nil, nil))
	b.refusedUnrun("a supervisor write without the seat", b.noteCmd(b.sup, "free", prjA), isReason(vocab.ReasonSeatNotHeld))
	// The guard covers a command Decide would propose, not only an allowed one.
	other := b.op.Actor()
	b.refusedUnrun("a seatless supervisor's proposal", b.derivedCmd(b.sup, []string{"grant.create"}, &other, false), isReason(vocab.ReasonSeatNotHeld))
	if out, err := b.note(b.noteCmd(b.op, "op", prjA)); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("an operator's write without the seat: %+v %v", out, err)
	}
	// seat.take: exempt, granted to sup only.
	g := b.supGrant
	g.ID, g.Projects, g.Actions = types.GrantID(newID(t, "grt")), types.ProjectScope{All: true}, []types.ActionKind{"seat.take"}
	b.seed(b.op, createGrant(g))
	take := func(p identity.Principal, exempt bool) eventstore.Command {
		b.az.Handlers["seat.take"] = &takeHandler{exempt: exempt, candidate: candidate(t)}
		key := newID(t, "cmd")
		return p.Command(key, "seat.take", key, []byte(`{}`), nil)
	}
	b.refusedUnrun("an unmarked seat action", take(b.sup, false), isReason(vocab.ReasonSeatNotHeld))
	b.refusedUnrun("an ungranted seat action", take(b.supervisor(), true), isReason(vocab.ReasonActionNotGranted))
	if out, err := b.s.Submit(context.Background(), take(b.sup, true), eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		evs, err := seat(types.SeatEventTransitionSeatTaken, nil, ref(sup), nil)(tx)
		return evs, json.RawMessage(`{}`), err
	})); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("seat.take without the seat: %+v %v", out, err)
	}
	if h, _ := b.holder(); h != sup {
		t.Errorf("after seat.take: %s", h)
	}
	b.rebuilds()
}

// takeHandler stands in for 6b's seat.take handler: candidate is the
// command's NewSeatCandidate, bound the object it derived.
type takeHandler struct {
	exempt           bool
	candidate, bound string
}

func (h *takeHandler) Derive(tx *eventstore.Tx, _ types.Actor, _ json.RawMessage) (authority.Derived, error) {
	seat, err := authority.SeatID(tx, h.candidate)
	h.bound = seat
	return authority.Derived{Object: seat, SeatExempt: h.exempt}, err
}

func candidate(t *testing.T) string {
	t.Helper()
	c, err := authority.NewSeatCandidate()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The first seat event: the object the command binds and the subject it
// writes are one id, which the seat keeps.
func TestSeatFirstTake(t *testing.T) {
	b := openBoundaryGrace(t, 0, false)
	sup := b.sup.Actor()
	// Before the first event, a candidate that is not a seat id is refused.
	b.view(func(tx *eventstore.Tx) error {
		if _, err := authority.SeatID(tx, newID(t, "evt")); err == nil {
			t.Error("an evt_ id as the seat's")
		}
		return nil
	})
	g := b.supGrant
	g.ID, g.Projects, g.Actions = types.GrantID(newID(t, "grt")), types.ProjectScope{All: true}, []types.ActionKind{"seat.take"}
	b.seed(b.op, createGrant(g))
	h := &takeHandler{exempt: true, candidate: candidate(t)}
	b.az.Handlers["seat.take"] = h
	key := newID(t, "cmd")
	var subject string
	out, err := b.s.Submit(context.Background(), b.sup.Command(key, "seat.take", key, []byte(`{}`), nil), eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := authority.NewSeatEvent(tx, h.candidate, types.SeatEvent{Transition: types.SeatEventTransitionSeatTaken, To: &sup})
			if len(evs) == 1 {
				subject = evs[0].Subject
			}
			return evs, json.RawMessage(`{}`), err
		}))
	if err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the first seat.take: %+v %v", out, err)
	}
	var recorded string
	b.view(func(tx *eventstore.Tx) (err error) {
		recorded, err = authority.SeatID(tx, candidate(t))
		return err
	})
	if h.bound == "" || h.bound != subject || subject != recorded || recorded != h.candidate {
		t.Errorf("bound %q, emitted %q, recorded %q, candidate %q", h.bound, subject, recorded, h.candidate)
	}
	b.rebuilds()
}

// pendingTo is the pending request's requester, if any.
func (b *boundary) pendingTo() (types.ActorID, bool) {
	b.t.Helper()
	var req struct {
		Event types.SeatEvent `json:"event"`
	}
	var pending bool
	b.view(func(tx *eventstore.Tx) error {
		raw, ok, err := tx.Get(authority.SeatProjection, "request")
		pending = ok
		if err != nil || !ok {
			return err
		}
		return json.Unmarshal(raw, &req)
	})
	if !pending || req.Event.To == nil {
		return "", false
	}
	return *req.Event.To, true
}

// A newer request replaces the older; only its own requester executes a
// matured request; any change of holder clears it, so a later force on it
// is stale and refused.
func TestSeatRequestLifecycle(t *testing.T) {
	const grace = 300 * time.Millisecond
	b := openBoundaryGrace(t, grace, true)
	sup, sup2, sup3 := b.sup.Actor(), b.supervisor(), b.supervisor()
	s2, s3 := sup2.Actor(), sup3.Actor()
	at := suiteNow().Add(grace + 100*time.Millisecond)
	b.seed(sup2, seat(types.SeatEventTransitionSeatHandoverRequested, ref(sup), &s2, &at))
	b.seed(sup3, seat(types.SeatEventTransitionSeatHandoverRequested, ref(sup), &s3, &at))
	if to, ok := b.pendingTo(); !ok || to != s3 {
		t.Fatalf("the newer request does not replace the older: %s %t", to, ok)
	}
	time.Sleep(at.Sub(suiteNow()) + 50*time.Millisecond)
	if b.try(sup2, seat(types.SeatEventTransitionSeatHandedOver, ref(sup), &s2, nil)) == nil {
		t.Error("a replaced request forced")
	}
	if b.try(sup2, seat(types.SeatEventTransitionSeatHandedOver, ref(sup), &s3, nil)) == nil {
		t.Error("a third supervisor executed another requester's matured handover")
	}
	b.seed(b.sup, seat(types.SeatEventTransitionSeatHandedOver, ref(sup), &s2, nil))
	if to, ok := b.pendingTo(); ok {
		t.Errorf("the request survives the handover: %s", to)
	}
	if b.try(sup3, seat(types.SeatEventTransitionSeatHandedOver, &s2, &s3, nil)) == nil {
		t.Error("a stale force after the holder changed")
	}
	if h, _ := b.holder(); h != s2 {
		t.Errorf("holder %s, want %s", h, s2)
	}
	b.rebuilds()
}
