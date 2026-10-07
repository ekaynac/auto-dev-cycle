// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// 14 PR 7a: agent.enrol and actor.revoke through the Authorizer and the
// writer.

func openAdmin(t *testing.T) *boundary {
	b := openBoundary(t)
	maps.Copy(b.az.Handlers, authority.AdminHandlers())
	return b
}

func enrolInput(id types.ActorID) []byte {
	return []byte(`{"actor":"` + string(id) + `","name":"agent","declared_agent":"harness-x"}`)
}

func (b *boundary) adminCmd(p identity.Principal, action, key string, in []byte, expect *eventstore.State) eventstore.Command {
	c := p.Command(newID(b.t, "cmd"), action, key, in, nil)
	c.Expect = expect
	return c
}

func (b *boundary) enrol(p identity.Principal, key string, in []byte, expect *eventstore.State) (eventstore.Committed, identity.Secret, bool, error) {
	return b.az.Enrol(context.Background(), b.s, b.adminCmd(p, authority.EnrolAction, key, in, expect))
}

func (b *boundary) revoke(p identity.Principal, key string, target types.ActorID) (eventstore.Committed, error) {
	return b.revokeExpecting(p, key, target, nil)
}

func (b *boundary) revokeExpecting(p identity.Principal, key string, target types.ActorID, expect *eventstore.State) (eventstore.Committed, error) {
	c := b.adminCmd(p, authority.RevokeActorAction, key, []byte(`{"actor":"`+string(target)+`"}`), expect)
	return b.s.Submit(context.Background(), c, b.az.RevokeActor(c))
}

// recordOf is a projection record.
func recordOf[T any](b *boundary, projection, key string) (v T) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) error {
		raw, ok, err := tx.Get(projection, key)
		if err == nil && !ok {
			err = errors.New("no record " + key)
		}
		if err == nil {
			err = json.Unmarshal(raw, &v)
		}
		return err
	})
	return v
}

func (b *boundary) authenticates(bearer string) (types.Actor, error) {
	var a types.Actor
	err := b.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		a, err = identity.Authenticate(tx, bearer)
		return err
	})
	return a, err
}

// An operator's enrolment creates the supervisor and its first token in one
// command; the secret reaches that call alone, never its result or a
// replay; the same actor under a new key, or against a stale expectation,
// is refused with nothing recorded.
func TestOperatorEnrols(t *testing.T) {
	b := openAdmin(t)
	mints := authority.CountMints(t)
	id := types.ActorID(newID(t, "act"))
	out, secret, ok, err := b.enrol(b.op, "k", enrolInput(id), version(0))
	if err != nil || out.Disposition != eventstore.DispositionApplied || out.Replayed || !ok || secret.Reveal() == "" {
		t.Fatalf("the enrolment: %+v %t %v", out, ok, err)
	}
	if out.LastSeq-out.FirstSeq != 1 {
		t.Errorf("the enrolment is not the actor and its token: %+v", out)
	}
	tokID, raw, _ := strings.Cut(secret.Reveal(), ".")
	if strings.Contains(string(out.Result), raw) || !strings.Contains(string(out.Result), tokID) {
		t.Errorf("the result %s", out.Result)
	}
	a, err := b.authenticates(secret.Reveal())
	if err != nil || a.ID != id || a.Kind != types.ActorKindSupervisor || a.CreatedBy != b.op.Actor() || a.Name != "agent" || a.DeclaredAgent == nil || *a.DeclaredAgent != "harness-x" {
		t.Fatalf("the enrolled actor: %+v %v", a, err)
	}
	if tok := recordOf[types.Token](b, identity.TokenProjection, tokID); tok.CreatedAt != b.recorded[tokID].At {
		t.Errorf("the token is created at %s, its event at %s", tok.CreatedAt, b.recorded[tokID].At)
	}
	h := b.head()
	again, secret2, ok, err := b.enrol(b.op, "k", enrolInput(id), version(0))
	if err != nil || !again.Replayed || again.CommandID != out.CommandID || ok || secret2.Reveal() != "" || b.head() != h {
		t.Errorf("the replay: %+v %t %v", again, ok, err)
	}
	// Neither the replay nor a non-operator's enrolment mints a token.
	if _, _, ok, err := b.enrol(b.sup, "s", enrolInput(types.ActorID(newID(t, "act"))), nil); !isReason(vocab.ReasonOperatorOnly)(err) || ok {
		t.Errorf("a supervisor's enrolment: %v", err)
	}
	if *mints != 1 {
		t.Errorf("%d mints for one enrolment", *mints)
	}
	if _, _, ok, err := b.enrol(b.op, "k2", enrolInput(id), nil); err == nil || ok || b.head() != h {
		t.Errorf("the same actor again: %t %v", ok, err)
	}
	other := types.ActorID(newID(t, "act"))
	if _, _, ok, err := b.enrol(b.op, "k3", enrolInput(other), version(1)); !errors.Is(err, eventstore.ErrStaleExpectedState) || ok || b.head() != h {
		t.Errorf("a stale expectation: %t %v", ok, err)
	}
	b.rebuilds()
}

// Two calls with one key that both pass the early read (both mint): one
// applies and delivers; the other replays inside the writer and delivers
// nothing (Issued.Deliver).
func TestConcurrentEnrolDeliversOnce(t *testing.T) {
	b := openAdmin(t)
	in := enrolInput(types.ActorID(newID(t, "act")))
	start := make(chan struct{})
	var wg sync.WaitGroup
	delivered := make([]bool, 2)
	for i := range delivered {
		wg.Go(func() {
			<-start
			out, _, ok, err := b.enrol(b.op, "same", in, nil)
			if err != nil || out.Disposition != eventstore.DispositionApplied {
				t.Errorf("call %d: %+v %v", i, out, err)
			}
			delivered[i] = ok
		})
	}
	close(start)
	wg.Wait()
	if delivered[0] == delivered[1] {
		t.Errorf("deliveries: %v", delivered)
	}
}

// A replay that passes the pre-check (a key stored after it read) is the
// writer's: it mints, replays, and delivers nothing. Sequential, so no
// timing decides it.
func TestWriterReplayDeliversNothing(t *testing.T) {
	b := openAdmin(t)
	mints := authority.CountMints(t)
	in := enrolInput(types.ActorID(newID(t, "act")))
	first, _, ok, err := b.enrol(b.op, "k", in, nil)
	if err != nil || first.Disposition != eventstore.DispositionApplied || !ok {
		t.Fatalf("the enrolment: %+v %v", first, err)
	}
	authority.SkipStoredCheck(t)
	h := b.head()
	again, secret, ok, err := b.enrol(b.op, "k", in, nil)
	if err != nil || !again.Replayed || again.CommandID != first.CommandID || ok || secret.Reveal() != "" || b.head() != h {
		t.Errorf("the writer's replay: %+v %t %v", again, ok, err)
	}
	if *mints != 2 {
		t.Errorf("%d mints: the replay did not reach the writer", *mints)
	}
}

// The request layer and the logic serve their own action only.
func TestAdminLogicGuardsItsAction(t *testing.T) {
	b := openAdmin(t)
	mints := authority.CountMints(t)
	h := b.head()
	in := []byte(`{"actor":"` + string(b.sup.Actor()) + `"}`)
	if _, _, ok, err := b.az.Enrol(context.Background(), b.s, b.adminCmd(b.op, authority.RevokeActorAction, "e", enrolInput(types.ActorID(newID(t, "act"))), nil)); err == nil || ok {
		t.Errorf("Enrol for another action: %v", err)
	}
	seed := b.adminCmd(b.op, "test.seed", "r", in, nil) // the boundary authorizes test.seed without a handler
	if _, err := b.s.Submit(context.Background(), seed, b.az.RevokeActor(seed)); err == nil || !strings.Contains(err.Error(), "is not actor.revoke") {
		t.Errorf("RevokeActor for another action: %v", err)
	}
	if b.head() != h || *mints != 0 {
		t.Errorf("a guarded call wrote or minted (%d)", *mints)
	}
	if _, err := b.authenticates(b.supTok.Reveal()); err != nil {
		t.Error(err)
	}
}

// Every scope is checked against its schema before anything is derived.
func TestAdminScopesValidated(t *testing.T) {
	b := openAdmin(t)
	id := newID(t, "act")
	isContract := func(err error) bool {
		var ce *contract.Error
		return errors.As(err, &ce)
	}
	for name, in := range map[string]string{
		"no name":            `{"actor":"` + id + `","declared_agent":null}`,
		"no declared agent":  `{"actor":"` + id + `","name":"a"}`,
		"an unknown field":   `{"actor":"` + id + `","name":"a","declared_agent":null,"kind":"operator"}`,
		"another id's kind":  `{"actor":"` + newID(t, "tok") + `","name":"a","declared_agent":null}`,
		"an empty name":      `{"actor":"` + id + `","name":"","declared_agent":null}`,
		"a revocation field": `{"actor":"` + id + `","name":"a","declared_agent":null,"revoked_at":null}`,
	} {
		b.refusedUnrun("enrol: "+name, b.adminCmd(b.op, authority.EnrolAction, newID(t, "cmd"), []byte(in), nil), isContract)
	}
	for name, in := range map[string]string{
		"no actor":         `{}`,
		"an unknown field": `{"actor":"` + string(b.sup.Actor()) + `","tokens":[]}`,
		"not an actor id":  `{"actor":"` + newID(t, "grt") + `"}`,
	} {
		b.refusedUnrun("revoke: "+name, b.adminCmd(b.op, authority.RevokeActorAction, newID(t, "cmd"), []byte(in), nil), isContract)
	}
}

// A supervisor neither enrols nor revokes, even granted both and holding
// the seat: its enrolment is refused, not proposed (no human confirmation
// in this build), and the logic never runs.
func TestSupervisorAdminRefused(t *testing.T) {
	b := openAdmin(t)
	g := b.supGrant
	g.ID, g.Projects, g.Actions = types.GrantID(newID(t, "grt")), types.ProjectScope{All: true}, []types.ActionKind{authority.EnrolAction, authority.RevokeActorAction}
	b.seed(b.op, createGrant(g))
	b.refusedUnrun("a supervisor's enrolment", b.adminCmd(b.sup, authority.EnrolAction, "e", enrolInput(types.ActorID(newID(t, "act"))), nil), isReason(vocab.ReasonOperatorOnly))
	b.refusedUnrun("the system's enrolment", b.adminCmd(b.sys, authority.EnrolAction, "se", enrolInput(types.ActorID(newID(t, "act"))), nil), isReason(vocab.ReasonOperatorOnly))
	if _, _, ok, err := b.enrol(b.sys, "se2", enrolInput(types.ActorID(newID(t, "act"))), nil); !isReason(vocab.ReasonOperatorOnly)(err) || ok {
		t.Errorf("the system's enrolment through Enrol: %v", err)
	}
	b.refusedUnrun("the system's revocation", b.adminCmd(b.sys, authority.RevokeActorAction, "sr", []byte(`{"actor":"`+string(b.sup.Actor())+`"}`), nil), isReason(vocab.ReasonOperatorOnly))
	other := b.supervisor()
	b.refusedUnrun("a supervisor's revocation", b.adminCmd(b.sup, authority.RevokeActorAction, "r", []byte(`{"actor":"`+string(other.Actor())+`"}`), nil), isReason(vocab.ReasonOperatorOnly))
	if _, err := b.authenticates(b.supTok.Reveal()); err != nil {
		t.Fatal(err)
	}
}

// Revoking a supervisor revokes it and every live token it holds in one
// command; the fold invalidates its pending proposal and frees its seat;
// its principal, replays included, and its bearers are refused.
func TestRevokeActor(t *testing.T) {
	b := openAdmin(t)
	sup := b.sup.Actor()
	var second, dead identity.Secret
	b.seed(b.op, issueTo(sup, b.op.Actor(), &second))
	b.seed(b.op, issueTo(sup, b.op.Actor(), &dead))
	deadID, _, _ := strings.Cut(dead.Reveal(), ".")
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeToken(tx, types.TokenID(deadID), suiteNow())
	})
	deadRec := recordOf[types.Token](b, identity.TokenProjection, deadID)
	_, p := b.submitProposal(b.proposeCmd(b.sup, "obj", b.op.Actor(), nil))
	if holder, held := b.holder(); !held || holder != sup {
		t.Fatalf("the seat: %s %t", holder, held)
	}
	if _, err := b.note(b.noteCmd(b.sup, "n", prjA)); err != nil {
		t.Fatal(err)
	}
	h := b.head()
	for name, expect := range map[string]*eventstore.State{"a stale version": version(1), "a stale digest": digestOf("sha256:" + strings.Repeat("0", 64))} {
		if _, err := b.revokeExpecting(b.op, newID(t, "cmd"), sup, expect); !errors.Is(err, eventstore.ErrStaleExpectedState) || b.head() != h {
			t.Errorf("%s: %v", name, err)
		}
	}
	out, err := b.revoke(b.op, "r", sup)
	if err != nil || out.Disposition != eventstore.DispositionApplied || out.LastSeq-out.FirstSeq != 2 {
		t.Fatalf("the revocation: %+v %v", out, err)
	}
	var res struct {
		Tokens []types.TokenID `json:"tokens"`
	}
	liveIDs := map[types.TokenID]bool{}
	for _, tok := range []identity.Secret{b.supTok, second} {
		id, _, _ := strings.Cut(tok.Reveal(), ".")
		liveIDs[types.TokenID(id)] = true
	}
	if err := json.Unmarshal(out.Result, &res); err != nil || len(res.Tokens) != 2 || !liveIDs[res.Tokens[0]] || !liveIDs[res.Tokens[1]] {
		t.Errorf("the result %s %v: it lists the live tokens only", out.Result, err)
	}
	if got := recordOf[types.Token](b, identity.TokenProjection, deadID); got.RevokedAt == nil || *got.RevokedAt != *deadRec.RevokedAt {
		t.Errorf("the already revoked token changed: %+v", got)
	}
	revokedAt := recordOf[types.Actor](b, identity.ActorProjection, string(sup)).RevokedAt
	for _, tok := range []identity.Secret{b.supTok, second} {
		if _, err := b.authenticates(tok.Reveal()); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("a revoked actor's bearer: %v", err)
		}
		id, _, _ := strings.Cut(tok.Reveal(), ".")
		b.view(func(tx *eventstore.Tx) error {
			raw, _, err := tx.Get(identity.TokenProjection, id)
			var rec types.Token
			if err == nil {
				err = json.Unmarshal(raw, &rec)
			}
			if rec.RevokedAt == nil || revokedAt == nil || *rec.RevokedAt != *revokedAt {
				t.Errorf("token %s is revoked at %v, the actor at %v", id, rec.RevokedAt, revokedAt)
			}
			// A stream's recheck closes it.
			if live := identity.StillLive(tx, tok.Reveal(), sup); !errors.Is(live, identity.ErrUnauthenticated) {
				t.Errorf("a stream's recheck: %v", live)
			}
			return err
		})
	}
	if got := b.proposal(p.ID); got.Status != types.ProposalStatusInvalidated || got.InvalidatedReason == nil ||
		*got.InvalidatedReason != types.ProposalInvalidatedReasonValueActorRevoked {
		t.Errorf("the pending proposal: %+v", got)
	}
	if _, held := b.holder(); held {
		t.Error("the revoked holder keeps the seat")
	}
	for _, key := range []string{"n", "new"} {
		if _, err := b.note(b.noteCmd(b.sup, key, prjA)); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("the revoked supervisor's %s: %v", key, err)
		}
	}
	h = b.head()
	if again, err := b.revoke(b.op, "r", sup); err != nil || !again.Replayed {
		t.Errorf("the replay: %+v %v", again, err)
	}
	if _, err := b.revoke(b.op, "r2", sup); err == nil || b.head() != h {
		t.Errorf("a second revocation: %v", err)
	}
	b.rebuilds()
}

// The system actor and the last live operator are never revoked: the
// refusal is committed with its reason and nothing else. An operator with
// another live operator beside it may revoke itself, its tokens first.
func TestRevokeProtected(t *testing.T) {
	b := openAdmin(t)
	sys := b.sys.Actor()
	refused := func(name string, by identity.Principal, target types.ActorID) {
		t.Helper()
		h := b.head()
		out, err := b.revoke(by, newID(t, "cmd"), target)
		var res struct {
			Reason string `json:"reason"`
		}
		if err != nil || out.Disposition != eventstore.DispositionRefused || json.Unmarshal(out.Result, &res) != nil ||
			res.Reason != vocab.ReasonActorProtected.String() || b.head() != h {
			t.Errorf("%s: %+v %v", name, out, err)
		}
	}
	refused("the system actor", b.op, sys)
	refused("the last operator, by itself", b.op, b.op.Actor())
	op2 := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "second", CreatedBy: b.op.Actor()}
	var tok2 identity.Secret
	b.seed(b.op, create(op2))
	b.seed(b.op, issueTo(op2.ID, b.op.Actor(), &tok2))
	second := b.login(tok2)
	if out, err := b.revoke(b.op, "self", b.op.Actor()); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("an operator revoking itself beside another: %+v %v", out, err)
	}
	if _, err := b.authenticates(b.opTok.Reveal()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("the revoked operator's bearer: %v", err)
	}
	refused("the last operator, by another", second, op2.ID)
	refused("the system actor, by the last operator", second, sys)
	b.rebuilds()
}
