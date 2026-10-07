// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// openSeats is a boundary wired with the seat handlers under grace, the
// seat free; every supervisor it names holds a seat grant.
func openSeats(t *testing.T, grace time.Duration) *boundary {
	b := openBoundaryGrace(t, grace, false)
	maps.Copy(b.az.Handlers, authority.SeatHandlers(grace))
	b.seatGrant(b.sup.Actor())
	return b
}

func (b *boundary) seatGrant(a types.ActorID) {
	g := b.supGrant
	g.ID, g.Actor, g.Projects, g.Actions = types.GrantID(newID(b.t, "grt")), a, types.ProjectScope{All: true}, []types.ActionKind{"seat.take", "seat.release"}
	b.seed(b.op, createGrant(g))
}

// grantedSupervisor is a new supervisor with a seat grant.
func (b *boundary) grantedSupervisor() identity.Principal {
	p := b.supervisor()
	b.seatGrant(p.Actor())
	return p
}

// seatCmd submits action under key, with input and expect, through the
// wired handler's logic.
func (b *boundary) seatCmd(p identity.Principal, action, key, input string, expect *eventstore.State) (eventstore.Committed, error) {
	c := p.Command(newID(b.t, "cmd"), action, key, []byte(input), nil)
	c.Expect = expect
	return b.s.Submit(context.Background(), c, b.az.Handlers[action].(authority.SeatHandler).Logic(c))
}

type seatResult struct {
	Seat       string      `json:"seat"`
	Transition string      `json:"transition"`
	ForceAfter *types.Time `json:"force_after"`
	Reason     string      `json:"reason"`
}

// must submits a seat command that commits with disposition d.
func (b *boundary) must(p identity.Principal, action, key string, d eventstore.Disposition) (eventstore.Committed, seatResult) {
	b.t.Helper()
	out, err := b.seatCmd(p, action, key, `{}`, nil)
	var r seatResult
	if err != nil || out.Disposition != d || json.Unmarshal(out.Result, &r) != nil {
		b.t.Fatalf("%s %s: %+v %v", action, key, out, err)
	}
	return out, r
}

// refusedAs: the command is committed as refused with reason, and its
// replay returns the same refusal.
func (b *boundary) refusedAs(p identity.Principal, action, key string, reason vocab.Reason) {
	b.t.Helper()
	first, r := b.must(p, action, key, eventstore.DispositionRefused)
	again, _ := b.must(p, action, key, eventstore.DispositionRefused)
	if r.Reason != reason.String() || !again.Replayed || again.CommandID != first.CommandID || first.FirstSeq != 0 {
		b.t.Errorf("%s %s: %+v, replay %+v, want %s", action, key, first, again, reason)
	}
}

// unrun: the command is refused before its logic, as check accepts, and
// nothing is appended.
func (b *boundary) unrun(name string, p identity.Principal, action, input string, check func(error) bool) {
	b.t.Helper()
	h := b.head()
	if out, err := b.seatCmd(p, action, newID(b.t, "cmd"), input, nil); !check(err) || out.CommandID != "" || b.head() != h {
		b.t.Errorf("%s: %+v %v", name, out, err)
	}
}

func (b *boundary) seatRecord() (json.RawMessage, string) {
	b.t.Helper()
	var rec json.RawMessage
	b.view(func(tx *eventstore.Tx) (err error) {
		rec, _, err = authority.SeatRecord(tx)
		return err
	})
	d, err := eventstore.ContentDigest(rec)
	if err != nil {
		b.t.Fatal(err)
	}
	return rec, d
}

// seat.take takes a free seat, asks for a held one with force_after the
// request's recorded time plus the grace, and executes the caller's own
// request once it wins; each command replays.
func TestSeatTakeHandler(t *testing.T) {
	const grace = 400 * time.Millisecond
	b := openSeats(t, grace)
	sup, sup2 := b.sup, b.grantedSupervisor()
	// The first take: the bound id, the event's subject and the recorded id are one.
	out, r := b.must(sup, "seat.take", "take-1", eventstore.DispositionApplied)
	b.view(func(tx *eventstore.Tx) error {
		recorded, err := authority.SeatID(tx, candidate(t))
		v, err2 := tx.SubjectVersion(r.Seat)
		if errors.Join(err, err2) != nil || recorded != r.Seat || v != out.LastSeq || r.Transition != "seat.taken" {
			t.Errorf("bound and emitted %s at %d, recorded %s, %+v", r.Seat, v, recorded, out)
		}
		return nil
	})
	if again, _ := b.must(sup, "seat.take", "take-1", eventstore.DispositionApplied); !again.Replayed || string(again.Result) != string(out.Result) {
		t.Errorf("the take's replay: %+v", again)
	}
	b.refusedAs(sup, "seat.take", "take-again", vocab.ReasonSeatUnavailable)
	b.refusedAs(b.op, "seat.take", "op-take", vocab.ReasonSeatUnavailable)
	b.unrun("an ungranted supervisor's take", b.supervisor(), "seat.take", `{}`, isReason(vocab.ReasonActionNotGranted))
	// A seatless supervisor asks; force_after is its request's recorded time plus the grace.
	req, rr := b.must(sup2, "seat.take", "ask", eventstore.DispositionApplied)
	var pending struct {
		Event types.SeatEvent `json:"event"`
		At    string          `json:"at"`
	}
	b.view(func(tx *eventstore.Tx) error {
		raw, _, err := tx.Get(authority.SeatProjection, "request")
		return errors.Join(err, json.Unmarshal(raw, &pending))
	})
	at, err := time.Parse(time.RFC3339Nano, pending.At)
	if err != nil || rr.Transition != "seat.handover_requested" || rr.ForceAfter == nil || *rr.ForceAfter != *pending.Event.ForceAfter ||
		string(*rr.ForceAfter) != at.Add(grace).Format(time.RFC3339Nano) || *pending.Event.To != sup2.Actor() {
		t.Fatalf("the request: %s, pending %+v %v", req.Result, pending, err)
	}
	b.refusedAs(sup2, "seat.take", "too-soon", vocab.ReasonSeatUnavailable)
	if again, _ := b.must(sup2, "seat.take", "ask", eventstore.DispositionApplied); !again.Replayed || again.LastSeq != req.LastSeq {
		t.Errorf("the request's replay: %+v", again)
	}
	time.Sleep(at.Add(grace).Sub(suiteNow()) + 50*time.Millisecond)
	if _, r := b.must(sup2, "seat.take", "force", eventstore.DispositionApplied); r.Transition != "seat.handed_over" {
		t.Errorf("forced: %+v", r)
	}
	if h, _ := b.holder(); h != sup2.Actor() {
		t.Errorf("after the forced handover: %s", h)
	}
	if again, _ := b.must(sup2, "seat.take", "force", eventstore.DispositionApplied); !again.Replayed {
		t.Errorf("the handover's replay: %+v", again)
	}
	b.rebuilds()
}

// seat.release: by the holder or an operator; a seatless supervisor is
// refused by the guard, before the logic runs.
func TestSeatReleaseHandler(t *testing.T) {
	b := openSeats(t, 0)
	sup, sup2 := b.sup, b.grantedSupervisor()
	b.must(sup, "seat.take", "take", eventstore.DispositionApplied)
	b.unrun("another supervisor's release", sup2, "seat.release", `{}`, isReason(vocab.ReasonSeatNotHeld))
	// A seatless supervisor still asks for the seat (seat.take is exempt).
	b.must(sup2, "seat.take", "ask", eventstore.DispositionApplied)
	out, r := b.must(sup, "seat.release", "release", eventstore.DispositionApplied)
	if r.Transition != "seat.released" {
		t.Errorf("the holder's release: %+v", r)
	}
	if again, _ := b.must(sup, "seat.release", "release", eventstore.DispositionApplied); !again.Replayed || again.LastSeq != out.LastSeq {
		t.Errorf("the release's replay: %+v", again)
	}
	if _, held := b.holder(); held {
		t.Error("released, still held")
	}
	if _, pending := b.pendingTo(); pending {
		t.Error("released, the request stays")
	}
	b.refusedAs(b.op, "seat.release", "free", vocab.ReasonSeatNotHeld)
	b.unrun("a release of a free seat by a supervisor", sup, "seat.release", `{}`, isReason(vocab.ReasonSeatNotHeld))
	b.must(sup2, "seat.take", "take", eventstore.DispositionApplied)
	b.must(b.op, "seat.release", "op-release", eventstore.DispositionApplied)
	if _, held := b.holder(); held {
		t.Error("released by an operator, still held")
	}
	b.rebuilds()
}

// The scope is checked against the action's schema before the derivation:
// force_after is never the caller's.
func TestSeatScopeSchema(t *testing.T) {
	b := openSeats(t, 0)
	isSchema := func(err error) bool { var e *contract.Error; return errors.As(err, &e) }
	for name, c := range map[string]struct{ action, input string }{
		"a take with force_after": {"seat.take", `{"force_after":"2026-10-07T00:00:00Z"}`},
		"a take for another":      {"seat.take", `{"to":"` + string(b.op.Actor()) + `"}`},
		"a release naming one":    {"seat.release", `{"holder":"` + string(b.sup.Actor()) + `"}`},
		"a scope not an object":   {"seat.take", `[]`},
	} {
		b.unrun(name, b.op, c.action, c.input, isSchema)
	}
	// Only seat.take (taking or asking for the seat) is exempt from holding it.
	b.view(func(tx *eventstore.Tx) error {
		for action, h := range authority.SeatHandlers(0) {
			d, err := h.Derive(tx, types.Actor{}, json.RawMessage(`{}`))
			if err != nil || d.SeatExempt != (action == "seat.take") || d.Project != nil || len(d.Classes) != 0 || d.Grantee != nil {
				t.Errorf("%s: %+v %v", action, d, err)
			}
		}
		return nil
	})
}

// --expect on the seat: version 0 before its first event, then its
// version and its record's digest.
func TestSeatExpect(t *testing.T) {
	b := openSeats(t, 0)
	sup2 := b.grantedSupervisor()
	zero := version(0)
	out, err := b.seatCmd(b.sup, "seat.take", "take", `{}`, zero)
	if err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("a first take expecting no seat: %+v %v", out, err)
	}
	rec, dg := b.seatRecord()
	staleBy := func(name string, p identity.Principal, action string, x *eventstore.State) {
		t.Helper()
		h := b.head()
		if out, err := b.seatCmd(p, action, newID(t, "cmd"), `{}`, x); !errors.Is(err, eventstore.ErrStaleExpectedState) || b.head() != h {
			t.Errorf("%s: %+v %v", name, out, err)
		}
	}
	staleBy("version 0 of an existing seat", sup2, "seat.take", zero)
	staleBy("an old version", sup2, "seat.take", version(out.LastSeq-1))
	staleBy("another digest", sup2, "seat.take", digestOf(contentDigest(t, `{"id":"x"}`)))
	ask, err := b.seatCmd(sup2, "seat.take", "ask", `{}`, both(out.LastSeq, dg))
	if err != nil || ask.Disposition != eventstore.DispositionApplied {
		t.Fatalf("a request at the inspected seat: %s %+v %v", rec, ask, err)
	}
	// The request changed the record: the old digest is stale, the new one current.
	staleBy("the digest before the request", b.sup, "seat.release", digestOf(dg))
	_, now := b.seatRecord()
	if out, err := b.seatCmd(b.sup, "seat.release", "release", `{}`, both(ask.LastSeq, now)); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("a release at the inspected seat: %+v %v", out, err)
	}
	b.rebuilds()
}

// One clock per command: under the production clock, a grant that tightens
// seat.take to human makes it propose, and the proposal's creation time is
// its event's recorded time, so the fold admits it. Its expected state is
// the seat's version, which the holder's revocation moved.
func TestSeatTakeProposedUnderATightenedGrant(t *testing.T) {
	b := openSeats(t, 0)
	sup2 := b.supervisor()
	b.must(b.sup, "seat.take", "take", eventstore.DispositionApplied)
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, b.sup.Actor(), suiteNow())
	})
	g := b.supGrant
	g.ID, g.Actor, g.Projects, g.Actions = types.GrantID(newID(t, "grt")), sup2.Actor(), types.ProjectScope{All: true}, []types.ActionKind{"seat.take"}
	g.ConfirmationOverrides = map[string]types.GrantConfirmationOverridesValue{"seat.take": types.GrantConfirmationOverridesValueHuman}
	b.seed(b.op, createGrant(g))
	out, err := b.seatCmd(sup2, "seat.take", "take", `{}`, nil)
	var r struct {
		Proposal types.ProposalID `json:"proposal"`
	}
	if err != nil || out.Disposition != eventstore.DispositionProposed || json.Unmarshal(out.Result, &r) != nil {
		t.Fatalf("a tightened seat.take: %+v %v", out, err)
	}
	p := b.proposal(r.Proposal)
	if p.Status != types.ProposalStatusOpen || p.Object.Type != authority.SeatObjectType || *p.ExpectedState.Version != b.seatVersion() ||
		*p.ExpectedState.Version != b.recorded[string(b.sup.Actor())].Seq ||
		p.Policy != types.ProposalPolicyHuman || string(p.CreatedAt) != b.recorded[string(p.ID)].At {
		t.Errorf("the seat proposal: %+v, recorded at %s", p, b.recorded[string(p.ID)].At)
	}
	if _, held := b.holder(); held {
		t.Error("a proposed take took the seat")
	}
	b.rebuilds()
}

func (b *boundary) seatVersion() int64 {
	b.t.Helper()
	var v int64
	b.view(func(tx *eventstore.Tx) (err error) {
		v, err = authority.SeatVersion(tx)
		return err
	})
	return v
}

// The seat's version moves on every change of its holder or request,
// revocations included: a version-only expectation from before one is stale.
func TestSeatVersionCoversRevocation(t *testing.T) {
	b := openSeats(t, 0)
	sup2, sup3 := b.grantedSupervisor(), b.grantedSupervisor()
	revoke := func(a types.ActorID) {
		b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, a, suiteNow()) })
	}
	stale := func(name string, p identity.Principal, action string, v int64) {
		t.Helper()
		if out, err := b.seatCmd(p, action, newID(t, "cmd"), `{}`, version(v)); !errors.Is(err, eventstore.ErrStaleExpectedState) {
			t.Errorf("%s: %+v %v", name, out, err)
		}
	}
	applies := func(name string, p identity.Principal, action string, v int64) int64 {
		t.Helper()
		out, err := b.seatCmd(p, action, newID(t, "cmd"), `{}`, version(v))
		if err != nil || out.Disposition != eventstore.DispositionApplied || b.seatVersion() != out.LastSeq {
			t.Fatalf("%s: %+v %v (version %d)", name, out, err, b.seatVersion())
		}
		return out.LastSeq
	}
	// The holder's revocation frees the seat and moves its version.
	v := applies("the first take", b.sup, "seat.take", 0)
	revoke(b.sup.Actor())
	if b.seatVersion() <= v {
		t.Fatalf("the holder's revocation left the version at %d", b.seatVersion())
	}
	stale("a take expecting the seat before the holder's revocation", sup2, "seat.take", v)
	v = applies("a take at the current version", sup2, "seat.take", b.seatVersion())
	// The requester's revocation drops its request and moves the version.
	v = applies("a request", sup3, "seat.take", v)
	revoke(sup3.Actor())
	if b.seatVersion() <= v {
		t.Fatalf("the requester's revocation left the version at %d", b.seatVersion())
	}
	stale("a release expecting the seat before the requester's revocation", sup2, "seat.release", v)
	applies("a release at the current version", sup2, "seat.release", b.seatVersion())
	// A revocation that touches neither leaves the version.
	v = b.seatVersion()
	revoke(b.grantedSupervisor().Actor())
	if b.seatVersion() != v {
		t.Errorf("an unrelated revocation moved the version: %d -> %d", v, b.seatVersion())
	}
	b.rebuilds()
}

// The Authorizer decides at the command's time, the store clock's
// (Tx.Now), never its own: with the store two hours behind, the
// supervisor's grant (from an hour before t0) is not yet in force.
func TestAuthorizerDecidesAtTheCommandTime(t *testing.T) {
	storeShift = -2 * time.Hour
	t.Cleanup(func() { storeShift = 0 })
	b := openBoundary(t)
	b.refusedUnrun("a command before the grant's validity", b.noteCmd(b.sup, "early", prjA), isReason(vocab.ReasonActionNotGranted))
}

// A pending seat proposal binds the seat's version: a revocation that
// moves it (the requester's, the holder's) invalidates the proposal in the
// fold as stale, as a seat event would; an unrelated one leaves it open.
func TestSeatProposalInvalidatedByRevocation(t *testing.T) {
	b := openSeats(t, 0)
	sup2, sup3 := b.supervisor(), b.grantedSupervisor()
	g := b.supGrant
	g.ID, g.Actor, g.Projects, g.Actions = types.GrantID(newID(t, "grt")), sup2.Actor(), types.ProjectScope{All: true}, []types.ActionKind{"seat.take"}
	g.ConfirmationOverrides = map[string]types.GrantConfirmationOverridesValue{"seat.take": types.GrantConfirmationOverridesValueHuman}
	b.seed(b.op, createGrant(g))
	propose := func() types.ProposalID {
		t.Helper()
		out, err := b.seatCmd(sup2, "seat.take", newID(t, "cmd"), `{}`, nil)
		var r struct {
			Proposal types.ProposalID `json:"proposal"`
		}
		if err != nil || out.Disposition != eventstore.DispositionProposed || json.Unmarshal(out.Result, &r) != nil {
			t.Fatalf("a tightened seat.take: %+v %v", out, err)
		}
		return r.Proposal
	}
	revoke := func(a types.ActorID) {
		b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, a, suiteNow()) })
	}
	stale := func(name string, id types.ProposalID) {
		t.Helper()
		if p := b.proposal(id); p.Status != types.ProposalStatusInvalidated || *p.InvalidatedReason != types.ProposalInvalidatedReasonValueStaleExpectedState {
			t.Errorf("%s: %+v", name, p)
		}
	}
	b.must(b.sup, "seat.take", "take", eventstore.DispositionApplied)
	b.must(sup3, "seat.take", "ask", eventstore.DispositionApplied)
	p := propose()
	revoke(sup3.Actor())
	stale("after the requester's revocation", p)
	p = propose()
	revoke(b.grantedSupervisor().Actor())
	if got := b.proposal(p); got.Status != types.ProposalStatusOpen {
		t.Errorf("an unrelated revocation closed the proposal: %+v", got)
	}
	revoke(b.sup.Actor())
	stale("after the holder's revocation", p)
	b.rebuilds()
}
