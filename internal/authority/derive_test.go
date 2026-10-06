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
// names, its object the note it would add.
type noteHandler struct{}

func (noteHandler) Derive(_ *eventstore.Tx, _ types.Actor, input json.RawMessage) (authority.Derived, error) {
	var in struct {
		Project types.ProjectID `json:"project"`
		Object  string          `json:"object"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return authority.Derived{}, err
	}
	return authority.Derived{Object: in.Object, Project: &in.Project}, nil
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
}

func openBoundary(t *testing.T) *boundary {
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
	az := authority.Authorizer{Registry: reg, Handlers: map[string]authority.Handler{"note.add": noteHandler{}}, Now: func() time.Time { return t0 }}
	kinds := identity.Kinds()
	maps.Copy(kinds, authority.GrantKinds())
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: identity.Recheck, Project: eventstore.ProjectEach(identity.Projector{}.Project, authority.GrantProjector{}.Project),
			Authorize: func(tx *eventstore.Tx, c eventstore.Command) error {
				if c.Action == "test.seed" {
					return nil
				}
				return az.Authorize(tx, c)
			}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b := &boundary{t: t, s: s}
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
	b.view(func(tx *eventstore.Tx) (err error) { b.sys, err = identity.SystemPrincipal(tx, sys.ID); return err })
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
		for _, bearer := range []string{"", "tok_x.y", b.opTok.Reveal()[:len(b.opTok.Reveal())-2] + "AA"} {
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
