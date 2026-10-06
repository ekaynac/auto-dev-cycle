// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// noteHandler is the test-only handler: note.add on the project its input
// names, its object the note it would add, with the change classes and
// grantee the input names (a stand-in for an effective-change computation),
// and the note's record its projection (notesProjection).
type noteHandler struct{}

var errDerive = errors.New("the derivation failed")

// notes is the test projection of the notes, test.noted/v1 setting a
// note's record to the event's payload, test.unnoted/v1 deleting it.
const notes = "test_notes"

func notesProjection(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, e := range evs {
		if e.Kind == "test.noted/v1" {
			if err := tx.Put(notes, e.Subject, e.Payload); err != nil {
				return err
			}
		}
		if e.Kind == "test.unnoted/v1" {
			if err := tx.Delete(notes, e.Subject); err != nil {
				return err
			}
		}
	}
	return nil
}

func (*noteHandler) Derive(_ *eventstore.Tx, _ types.Actor, input json.RawMessage) (authority.Derived, error) {
	var in struct {
		Project types.ProjectID `json:"project"`
		Object  string          `json:"object"`
		Classes []string        `json:"classes"`
		Grantee *types.ActorID  `json:"grantee"`
		Fail    bool            `json:"fail"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return authority.Derived{}, err
	}
	if in.Fail {
		return authority.Derived{}, errDerive
	}
	return authority.Derived{Object: in.Object, Project: &in.Project, Classes: in.Classes, Grantee: in.Grantee,
		Record: func(tx *eventstore.Tx) (json.RawMessage, bool, error) { return tx.Get(notes, in.Object) }}, nil
}

// boundary is a store wired as the daemon wires it, the identity recheck
// and the Authorizer, with a note.add handler; seed commands
// (test.seed) skip the Authorizer, never the recheck.
type boundary struct {
	t             *testing.T
	s             *eventstore.Store
	sys, op, sup  identity.Principal
	opTok, supTok identity.Secret
	supGrant      types.Grant
	az            authority.Authorizer // what the store's Authorize hook runs
}

func openBoundary(t *testing.T) *boundary { return openBoundaryGrace(t, 0, true) }

// openBoundaryGrace: with the seat projector's grace overridden when
// positive; seated, the supervisor holds the seat.
func openBoundaryGrace(t *testing.T, grace time.Duration, seated bool) *boundary {
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Release() })
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	b := &boundary{t: t, az: authority.Authorizer{Registry: reg, Handlers: map[string]authority.Handler{"note.add": &noteHandler{}}, Now: func() time.Time { return t0 }}}
	kinds := identity.Kinds()
	maps.Copy(kinds, authority.GrantKinds())
	maps.Copy(kinds, authority.SeatKinds())
	kinds["test.noted/v1"] = eventstore.Kind{Schema: "common/defs"}
	kinds["test.unnoted/v1"] = eventstore.Kind{Schema: "common/defs"}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: identity.Recheck, Project: eventstore.ProjectEach(identity.Projector{}.Project, authority.GrantProjector{}.Project, authority.SeatProjector{MinGrace: grace}.Project, notesProjection),
			Authorize: func(tx *eventstore.Tx, c eventstore.Command) error {
				if c.Action == "test.seed" {
					return nil
				}
				return b.az.Authorize(tx, c)
			}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b.s = s
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	evs, err := identity.CreateActor(sys)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newID(t, "cmd")
	if _, err := s.Bootstrap(context.Background(), eventstore.Command{ID: cmd, Actor: string(sys.ID), Action: "test.seed", IdempotencyKey: cmd, Input: []byte(`{}`)}, evs[0]); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimSystem()
	if err != nil {
		t.Fatal(err)
	}
	b.view(func(tx *eventstore.Tx) (err error) {
		b.sys, err = identity.SystemPrincipal(tx, claim, sys.ID)
		return err
	})
	op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys.ID}
	sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op.ID}
	b.seed(b.sys, create(op))
	b.seed(b.sys, issueTo(op.ID, sys.ID, &b.opTok))
	b.op = b.login(b.opTok)
	b.seed(b.op, create(sup))
	b.seed(b.op, issueTo(sup.ID, op.ID, &b.supTok))
	b.supGrant = types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{prjA}},
		Actions: []types.ActionKind{"note.add"}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{}},
		ValidFrom: types.Time(t0.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(t0.Add(time.Hour).Format(time.RFC3339)),
		ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op.ID, Approval: types.ApprovalID(newID(t, "apr"))}
	b.seed(b.op, createGrant(b.supGrant))
	b.sup = b.login(b.supTok)
	if seated {
		b.seed(b.sup, seat(types.SeatEventTransitionSeatTaken, nil, &sup.ID, nil))
	}
	return b
}

func issueTo(holder, issuer types.ActorID, out *identity.Secret) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) {
		evs, secret, err := identity.IssueToken(holder, issuer, time.Now())
		*out = secret
		return evs, err
	}
}

func (b *boundary) view(fn func(*eventstore.Tx) error) {
	b.t.Helper()
	if err := b.s.View(context.Background(), fn); err != nil {
		b.t.Fatal(err)
	}
}

// login authenticates a bearer on the private socket.
func (b *boundary) login(tok identity.Secret) identity.Principal {
	b.t.Helper()
	var p identity.Principal
	b.view(func(tx *eventstore.Tx) (err error) {
		p, err = identity.Authenticated(tx, tok.Reveal(), identity.PrivateSocket)
		return err
	})
	return p
}

func (b *boundary) seed(p identity.Principal, apply applyFn) {
	b.t.Helper()
	cmd := newID(b.t, "cmd")
	if _, err := b.s.SubmitApplied(context.Background(), p.Command(cmd, "test.seed", cmd, []byte(`{}`), nil), eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := apply(tx)
			return evs, json.RawMessage(`{}`), err
		})); err != nil {
		b.t.Fatal(err)
	}
}

// note submits note.add on project under key, as c's principal, and
// returns what the writer committed.
func (b *boundary) note(c eventstore.Command) (eventstore.Committed, error) {
	return b.s.Submit(context.Background(), c, eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		return nil, json.RawMessage(`{"noted":true}`), nil
	}))
}

func noteInput(p types.ProjectID) []byte {
	return []byte(`{"project":"` + string(p) + `","object":"note-1"}`)
}

func (b *boundary) noteCmd(p identity.Principal, key string, project types.ProjectID) eventstore.Command {
	return p.Command(newID(b.t, "cmd"), "note.add", key, noteInput(project), nil)
}

func (b *boundary) head() int64 {
	b.t.Helper()
	h, err := b.s.Head(context.Background())
	if err != nil {
		b.t.Fatal(err)
	}
	return h.Seq
}

// The derivation feeds Decide: a supervisor's grant covers note.add on
// the project the handler derived and no other; an action without a
// handler, or a derivation without an object, is refused.
func TestDerivationFeedsDecide(t *testing.T) {
	b := openBoundary(t)
	if out, err := b.note(b.noteCmd(b.sup, "a", prjA)); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("a granted note: %+v %v", out, err)
	}
	var refusal *authority.Refusal
	if _, err := b.note(b.noteCmd(b.sup, "b", prjB)); !errors.As(err, &refusal) || refusal.Reason != vocab.ReasonActionNotGranted {
		t.Fatalf("a note outside the grant: %v", err)
	}
	if _, err := b.note(b.sup.Command(newID(t, "cmd"), "task.create", "c", noteInput(prjA), nil)); !errors.Is(err, authority.ErrActionNotWired) {
		t.Fatalf("an action without a handler: %v", err)
	}
	if _, err := b.note(b.sup.Command(newID(t, "cmd"), "note.add", "d", []byte(`{"project":"`+string(prjA)+`"}`), nil)); err == nil || !strings.Contains(err.Error(), "no object") {
		t.Fatalf("a derivation without an object: %v", err)
	}
}

// 14 "Replay authentication": every case is refused before the
// idempotency lookup, so nothing is applied and nothing replayed; a live
// actor still replays its own applied command.
func TestReplayAuthentication(t *testing.T) {
	b := openBoundary(t)
	applied, err := b.note(b.noteCmd(b.op, "k", prjA))
	if err != nil || applied.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the operator's note: %+v %v", applied, err)
	}
	again, err := b.note(b.noteCmd(b.op, "k", prjA))
	if err != nil || !again.Replayed || again.CommandID != applied.CommandID {
		t.Fatalf("the operator's own replay: %+v %v", again, err)
	}
	refused := func(name string, c eventstore.Command) {
		t.Helper()
		h := b.head()
		if out, err := b.note(c); !errors.Is(err, identity.ErrUnauthenticated) || out.Replayed {
			t.Errorf("%s: %+v %v", name, out, err)
		}
		if b.head() != h {
			t.Errorf("%s: appended", name)
		}
	}
	// Missing or invalid credentials make no principal; a command without one
	// is refused, even naming the operator.
	b.view(func(tx *eventstore.Tx) error {
		// The wrong secret differs from the operator's in exactly its first
		// character, changed to another valid one.
		tokID, secret, _ := strings.Cut(b.opTok.Reveal(), ".")
		other := "A"
		if secret[0] == 'A' {
			other = "B"
		}
		for _, bearer := range []string{"", "tok_x.y", tokID + "." + other + secret[1:]} {
			if _, err := identity.Authenticated(tx, bearer, identity.PrivateSocket); !errors.Is(err, identity.ErrUnauthenticated) {
				t.Errorf("a principal from a missing or invalid bearer: %v", err)
			}
		}
		return nil
	})
	bare := b.noteCmd(b.op, "k", prjA)
	bare.Principal = nil
	refused("no principal, the operator's key", bare)
	// Another actor's key: the supervisor cannot claim the operator's actor,
	// and under its own actor the key is its own, never the operator's result.
	forged := b.noteCmd(b.sup, "k", prjA)
	forged.Actor = string(b.op.Actor())
	refused("the supervisor naming the operator", forged)
	own, err := b.note(b.noteCmd(b.sup, "k", prjA))
	if err != nil || own.Replayed || own.CommandID == applied.CommandID {
		t.Fatalf("the supervisor's own key k: %+v %v", own, err)
	}
	// Revocation between authentication and writer access: the supervisor
	// authenticated (b.sup) before the revocation commits; its replay and a
	// new command are both refused.
	tokID, _, _ := strings.Cut(b.supTok.Reveal(), ".")
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeToken(tx, types.TokenID(tokID), time.Now())
	})
	refused("a revoked token's replay", b.noteCmd(b.sup, "k", prjA))
	refused("a revoked token's new command", b.noteCmd(b.sup, "new", prjA))
}

// A revoked actor's principal, taken before the revocation, is refused,
// its replays included.
func TestRevokedActorIsRefused(t *testing.T) {
	b := openBoundary(t)
	if _, err := b.note(b.noteCmd(b.sup, "k", prjA)); err != nil {
		t.Fatal(err)
	}
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, b.sup.Actor(), time.Now())
	})
	for _, key := range []string{"k", "new"} {
		if out, err := b.note(b.noteCmd(b.sup, key, prjA)); !errors.Is(err, identity.ErrUnauthenticated) || out.Replayed {
			t.Errorf("key %s: %+v %v", key, out, err)
		}
	}
}

// derivedCmd is note.add on project A whose derivation names classes and
// a grantee.
func (b *boundary) derivedCmd(p identity.Principal, classes []string, grantee *types.ActorID, fail bool) eventstore.Command {
	b.t.Helper()
	in, err := json.Marshal(map[string]any{"project": prjA, "object": "note-1", "classes": classes, "grantee": grantee, "fail": fail})
	if err != nil {
		b.t.Fatal(err)
	}
	key := newID(b.t, "cmd")
	return p.Command(key, "note.add", key, in, nil)
}

// refusedUnrun submits c and requires err to be what check accepts, with
// the command's logic never run and nothing appended.
func (b *boundary) refusedUnrun(name string, c eventstore.Command, check func(error) bool) {
	b.t.Helper()
	h, ran := b.head(), false
	_, err := b.s.Submit(context.Background(), c, eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		ran = true
		return nil, json.RawMessage(`{}`), nil
	}))
	if !check(err) || ran || b.head() != h {
		b.t.Errorf("%s: %v (logic ran: %t)", name, err, ran)
	}
}

func isReason(r vocab.Reason) func(error) bool {
	return func(err error) bool {
		var refusal *authority.Refusal
		return errors.As(err, &refusal) && refusal.Reason == r
	}
}

func is(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }

// The derived classes and grantee reach Decide, and a command Decide would
// propose is refused, its logic never run, until proposals are persisted.
func TestAuthorizerJudgesTheDerivedChange(t *testing.T) {
	b := openBoundary(t)
	self, other := b.sup.Actor(), b.op.Actor()
	// The supervisor's grant covers note.add; the derived class makes it a
	// grant change: to another actor it needs a human (a proposal), to
	// itself it is refused.
	b.refusedUnrun("a grant change for another", b.derivedCmd(b.sup, []string{"grant.create"}, &other, false), is(authority.ErrProposalsNotWired))
	b.refusedUnrun("a grant change for itself", b.derivedCmd(b.sup, []string{"grant.create"}, &self, false), isReason(vocab.ReasonSelfGrantRefused))
	// A class without a confirmation default is refused, for an operator too.
	b.refusedUnrun("an unknown class", b.derivedCmd(b.op, []string{"no_such.class"}, nil, false), isReason(vocab.ReasonChangeClassUnknown))
	// Without a sensitive class the same command applies.
	if out, err := b.note(b.derivedCmd(b.sup, nil, nil, false)); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the plain note: %+v %v", out, err)
	}
}

// A missing registry, clock or handler, a typed nil handler and a failed
// derivation each refuse, without a panic and without running the logic.
func TestAuthorizerFailsClosed(t *testing.T) {
	b := openBoundary(t)
	full := b.az
	for name, az := range map[string]authority.Authorizer{
		"no registry":         {Handlers: full.Handlers, Now: full.Now},
		"no clock":            {Registry: full.Registry, Handlers: full.Handlers},
		"a nil handler":       {Registry: full.Registry, Now: full.Now, Handlers: map[string]authority.Handler{"note.add": nil}},
		"a typed nil handler": {Registry: full.Registry, Now: full.Now, Handlers: map[string]authority.Handler{"note.add": (*noteHandler)(nil)}},
	} {
		b.az = az
		b.refusedUnrun(name, b.derivedCmd(b.op, nil, nil, false), is(authority.ErrActionNotWired))
	}
	b.az = full
	b.refusedUnrun("a failed derivation", b.derivedCmd(b.op, nil, nil, true), is(errDerive))
}
