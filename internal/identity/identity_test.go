// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

type applyFn = func(*eventstore.Tx) ([]eventstore.Event, error)

type fixture struct {
	t     *testing.T
	dir   string // the state directory
	s     *eventstore.Store
	sys   types.Actor
	op    types.Actor // the first operator (bootstrap)
	token identity.Secret
}

func id(t *testing.T, prefix string) string {
	t.Helper()
	v, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// submitKey runs one command by actor, under an idempotency key.
func (f *fixture) submitKey(by types.ActorID, key string, apply applyFn) error {
	_, err := f.s.SubmitApplied(context.Background(), eventstore.Command{ID: id(f.t, "cmd"), Actor: string(by), Action: "test.command", IdempotencyKey: key, Input: []byte(`{}`)}, eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := apply(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

func (f *fixture) submit(by types.ActorID, apply applyFn) error {
	return f.submitKey(by, id(f.t, "cmd"), apply)
}

func (f *fixture) must(by types.ActorID, apply applyFn) {
	f.t.Helper()
	if err := f.submit(by, apply); err != nil {
		f.t.Fatal(err)
	}
}

// authenticate runs Authenticate in a view.
func (f *fixture) authenticate(bearer string) (types.Actor, error) {
	var a types.Actor
	var authErr error
	err := f.s.View(context.Background(), func(tx *eventstore.Tx) error {
		a, authErr = identity.Authenticate(tx, bearer)
		return nil
	})
	return a, errors.Join(err, authErr)
}

// projected reports whether a record is in a projection.
func (f *fixture) projected(projection, key string) bool {
	f.t.Helper()
	var found bool
	if err := f.s.View(context.Background(), func(tx *eventstore.Tx) error {
		_, ok, err := tx.Get(projection, key)
		found = ok
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return found
}

func actor(t *testing.T, kind types.ActorKind, createdBy types.ActorID) types.Actor {
	a := types.Actor{ID: types.ActorID(id(t, "act")), Kind: kind, Name: string(kind), CreatedBy: createdBy}
	if createdBy == "" {
		a.CreatedBy = a.ID
	}
	return a
}

func create(a types.Actor) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(a) }
}

func issue(holder, issuer types.ActorID, out *identity.Secret) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) {
		evs, secret, err := identity.IssueToken(holder, issuer, time.Now())
		if out != nil {
			*out = secret
		}
		return evs, err
	}
}

func revokeToken(bearer identity.Secret) applyFn {
	tokID, _, _ := strings.Cut(bearer.Reveal(), ".")
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeToken(tx, types.TokenID(tokID), time.Now())
	}
}

// open starts a store with the identity kinds and projector and runs the
// bootstrap: the system actor, the first operator, and its token.
func open(t *testing.T) *fixture {
	f := empty(t)
	f.sys = actor(t, types.ActorKindSystem, "")
	f.op = actor(t, types.ActorKindOperator, f.sys.ID)
	f.must(f.sys.ID, create(f.sys))
	f.must(f.sys.ID, create(f.op))
	f.must(f.sys.ID, issue(f.op.ID, f.sys.ID, &f.token))
	return f
}

// empty starts a store with no actors at all.
func empty(t *testing.T) *fixture {
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
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: identity.Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil }, Project: identity.Projector{}.Project}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &fixture{t: t, s: s, dir: dir}
}

// The bootstrap runs in its order only: the system actor first, by itself;
// then one operator and one token, by the system actor.
func TestBootstrapOrder(t *testing.T) {
	f := empty(t)
	stranger := types.ActorID(id(t, "act"))
	sys := actor(t, types.ActorKindSystem, "")
	sysByStranger := actor(t, types.ActorKindSystem, stranger)
	for name, c := range map[string]struct {
		by    types.ActorID
		apply applyFn
	}{
		"an operator before the system actor": {stranger, create(actor(t, types.ActorKindOperator, stranger))},
		"a system actor created by another":   {stranger, create(sysByStranger)},
	} {
		if err := f.submit(c.by, c.apply); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	f.must(sys.ID, create(sys))
	if err := f.submit(sys.ID, create(actor(t, types.ActorKindSupervisor, sys.ID))); err == nil {
		t.Error("the system actor created a supervisor: accepted")
	}
	op := actor(t, types.ActorKindOperator, sys.ID)
	f.must(sys.ID, create(op))
	// The system actor's one token is the first operator's, not another's.
	op2 := actor(t, types.ActorKindOperator, op.ID)
	f.must(op.ID, create(op2))
	if err := f.submit(sys.ID, issue(op2.ID, sys.ID, nil)); err == nil {
		t.Error("the system actor issued its token to another operator: accepted")
	}
	f.must(sys.ID, issue(op.ID, sys.ID, nil))
}

// The first operator's first token ends the bootstrap even when the
// operator issued it: the system actor issues nothing after it.
func TestBootstrapEndsWithTheFirstToken(t *testing.T) {
	f := empty(t)
	sys := actor(t, types.ActorKindSystem, "")
	op := actor(t, types.ActorKindOperator, sys.ID)
	f.must(sys.ID, create(sys))
	f.must(sys.ID, create(op))
	f.must(op.ID, issue(op.ID, op.ID, nil))
	if err := f.submit(sys.ID, issue(op.ID, sys.ID, nil)); err == nil {
		t.Error("the system actor issued a token after the operator's first: accepted")
	}
}

// wrongSecret changes one non-final character of a bearer's secret to
// another valid base64url character.
func wrongSecret(bearer string) string {
	b := []byte(bearer)
	i := strings.IndexByte(bearer, '.') + 5
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

func TestAuthenticate(t *testing.T) {
	f := open(t)
	a, err := f.authenticate(f.token.Reveal())
	if err != nil || a.ID != f.op.ID {
		t.Fatalf("%+v %v", a, err)
	}
	_, never, err := identity.IssueToken(f.op.ID, f.op.ID, time.Now()) // never recorded
	if err != nil {
		t.Fatal(err)
	}
	for name, bearer := range map[string]string{
		"a wrong secret":   wrongSecret(f.token.Reveal()),
		"an unknown token": never.Reveal(),
		"malformed":        "tok_x.y",
		"empty":            "",
	} {
		if _, err := f.authenticate(bearer); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A revoked token, then a revoked actor, stop authenticating.
	op2 := actor(t, types.ActorKindOperator, f.op.ID)
	var other identity.Secret
	f.must(f.op.ID, create(op2))
	f.must(f.op.ID, issue(op2.ID, f.op.ID, &other))
	f.must(op2.ID, revokeToken(f.token))
	if _, err := f.authenticate(f.token.Reveal()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("a revoked token: %v", err)
	}
	if _, err := f.authenticate(other.Reveal()); err != nil {
		t.Errorf("the other token: %v", err)
	}
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, op2.ID, time.Now())
	})
	if _, err := f.authenticate(other.Reveal()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("a revoked actor's token: %v", err)
	}
	// The projections are a fold: a rebuild derives the same.
	if diffs, err := f.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
}

// Every well-formed bearer costs exactly one hash, whatever its token's
// state; a malformed one costs none.
func TestAuthenticationWork(t *testing.T) {
	f := open(t)
	b := f.token.Reveal()
	sup := actor(t, types.ActorKindSupervisor, f.op.ID)
	var revoked, holderRevoked, damaged identity.Secret
	f.must(f.op.ID, create(sup))
	f.must(f.op.ID, issue(f.op.ID, f.op.ID, &revoked))
	f.must(f.op.ID, issue(sup.ID, f.op.ID, &holderRevoked))
	f.must(f.op.ID, issue(f.op.ID, f.op.ID, &damaged))
	f.must(f.op.ID, revokeToken(revoked))
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, sup.ID, time.Now())
	})
	// A stored hash no verification accepts, as damaged storage would leave it.
	damagedID, _, _ := strings.Cut(damaged.Reveal(), ".")
	f.must(f.sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		raw, ok, err := tx.Get(identity.TokenProjection, damagedID)
		if err != nil || !ok {
			return nil, errors.Join(err, errors.New("no token"))
		}
		var tk types.Token
		if err := json.Unmarshal(raw, &tk); err != nil {
			return nil, err
		}
		tk.Hash = "$argon2id$v=19$m=0,t=3,p=4$AAAA$AAAA"
		raw, _ = json.Marshal(tk)
		return nil, tx.Put(identity.TokenProjection, damagedID, raw)
	})
	_, never, _ := identity.IssueToken(f.op.ID, f.op.ID, time.Now())
	for name, c := range map[string]struct {
		bearer string
		hashes int64
		ok     bool
	}{
		"a valid token":         {b, 1, true},
		"a wrong secret":        {wrongSecret(b), 1, false},
		"an unknown token":      {never.Reveal(), 1, false},
		"a revoked token":       {revoked.Reveal(), 1, false},
		"a revoked holder":      {holderRevoked.Reveal(), 1, false},
		"a damaged stored hash": {damaged.Reveal(), 1, false},
		"a malformed bearer":    {"tok_x.y", 0, false},
	} {
		count, restore := identity.CountHashes()
		_, err := f.authenticate(c.bearer)
		restore()
		if count() != c.hashes || (err == nil) != c.ok {
			t.Errorf("%s: %d hashes, %v", name, count(), err)
		}
	}
}

// A command whose identity events are partly refused leaves nothing, and its
// key stays free; a batch that creates and issues at once rebuilds the same.
func TestRollbackAndBatches(t *testing.T) {
	f := open(t)
	sup := actor(t, types.ActorKindSupervisor, f.op.ID)
	both := func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		good, err := identity.CreateActor(sup)
		if err != nil {
			return nil, err
		}
		bad, err := identity.CreateActor(f.op) // exists
		return append(good, bad...), err
	}
	if err := f.submitKey(f.op.ID, "batch", both); err == nil {
		t.Fatal("a batch with a refused event applied")
	}
	if f.projected(identity.ActorProjection, string(sup.ID)) {
		t.Fatal("a refused batch left an actor")
	}
	if err := f.submitKey(f.op.ID, "batch", create(sup)); err != nil {
		t.Fatalf("the key of a refused command: %v", err)
	}
	sup2 := actor(t, types.ActorKindSupervisor, f.op.ID)
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		a, err := identity.CreateActor(sup2)
		if err != nil {
			return nil, err
		}
		tk, _, err := identity.IssueToken(sup2.ID, f.op.ID, time.Now())
		return append(a, tk...), err
	})
	if diffs, err := f.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
}

// The projector refuses every transition that does not follow from the
// projected records and who submits it.
func TestProjectorRefuses(t *testing.T) {
	f := open(t)
	now := time.Now()
	revoked := now.UTC().Format(time.RFC3339Nano)
	event := func(subject string, p types.IdentityEvent) applyFn {
		return func(*eventstore.Tx) ([]eventstore.Event, error) {
			raw, _ := json.Marshal(p)
			return []eventstore.Event{{ID: id(t, "evt"), Kind: string(p.Transition) + "/v1", Subject: subject, Payload: raw}}, nil
		}
	}
	created := func(a types.Actor) applyFn {
		return event(string(a.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionActorCreated, Actor: &a})
	}
	revokedActor := func(a types.Actor) applyFn {
		a.RevokedAt = &revoked
		return event(string(a.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionActorRevoked, Actor: &a})
	}
	sup := actor(t, types.ActorKindSupervisor, f.op.ID)
	op2 := actor(t, types.ActorKindOperator, f.op.ID)
	f.must(f.op.ID, create(sup))
	f.must(f.op.ID, create(op2))
	ghost := actor(t, types.ActorKindOperator, f.op.ID) // never created
	renamed := op2
	renamed.Name = "someone else"
	bornRevoked := actor(t, types.ActorKindSupervisor, f.op.ID)
	bornRevoked.RevokedAt = &revoked
	tok, _, _ := identity.IssueToken(f.op.ID, f.op.ID, now)
	var issued types.IdentityEvent
	if err := json.Unmarshal(tok[0].Payload, &issued); err != nil {
		t.Fatal(err)
	}
	tokenFor := func(holder, issuer types.ActorID, revokedAt *string) applyFn {
		tk := *issued.Token
		tk.ID, tk.Actor, tk.CreatedBy, tk.RevokedAt = types.TokenID(id(t, "tok")), holder, issuer, revokedAt
		return event(string(tk.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionTokenIssued, Token: &tk})
	}
	cheap := *issued.Token
	cheap.ID, cheap.Hash = types.TokenID(id(t, "tok")), identity.Hash("whatever", 1, 32, 4)
	unknownRevoked := *issued.Token
	unknownRevoked.RevokedAt = &revoked
	sys2 := actor(t, types.ActorKindSystem, "")
	sysByOp := actor(t, types.ActorKindSystem, f.op.ID)
	type step struct {
		by    types.ActorID
		apply applyFn
	}
	for name, c := range map[string]step{
		"an actor created twice":                      {f.op.ID, created(op2)},
		"created_by is not the command's actor":       {f.op.ID, created(actor(t, types.ActorKindSupervisor, op2.ID))},
		"a non-system actor creating itself":          {ghost.ID, created(actor(t, types.ActorKindOperator, ""))},
		"an actor created by an unknown actor":        {ghost.ID, created(actor(t, types.ActorKindSupervisor, ghost.ID))},
		"an actor created revoked":                    {f.op.ID, created(bornRevoked)},
		"another actor's subject":                     {f.op.ID, event(string(sup.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionActorCreated, Actor: &ghost})},
		"a second system actor":                       {sys2.ID, created(sys2)},
		"a system actor created by an operator":       {f.op.ID, created(sysByOp)},
		"the system actor creating a second operator": {f.sys.ID, created(actor(t, types.ActorKindOperator, f.sys.ID))},
		"the system actor creating a supervisor":      {f.sys.ID, created(actor(t, types.ActorKindSupervisor, f.sys.ID))},
		"a supervisor creating an operator":           {sup.ID, created(actor(t, types.ActorKindOperator, sup.ID))},
		"a supervisor creating a supervisor":          {sup.ID, created(actor(t, types.ActorKindSupervisor, sup.ID))},
		"revoking the system actor":                   {f.op.ID, revokedActor(f.sys)},
		"revoking an unknown actor":                   {f.op.ID, revokedActor(ghost)},
		"a revocation that renames":                   {f.op.ID, revokedActor(renamed)},
		"a revocation without a time":                 {f.op.ID, event(string(op2.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionActorRevoked, Actor: &op2})},
		"a supervisor revoking an actor":              {sup.ID, revokedActor(op2)},
		"a token for an unknown actor":                {f.op.ID, tokenFor(ghost.ID, f.op.ID, nil)},
		"a token issued by an unknown actor":          {ghost.ID, tokenFor(f.op.ID, ghost.ID, nil)},
		"a token whose created_by is not the issuer":  {f.op.ID, tokenFor(sup.ID, op2.ID, nil)},
		"a token issued revoked":                      {f.op.ID, tokenFor(f.op.ID, f.op.ID, &revoked)},
		"a token for the system actor":                {f.op.ID, tokenFor(f.sys.ID, f.op.ID, nil)},
		"the system actor's second token":             {f.sys.ID, tokenFor(f.op.ID, f.sys.ID, nil)},
		"the system actor issuing to another":         {f.sys.ID, tokenFor(op2.ID, f.sys.ID, nil)},
		"a supervisor issuing a token":                {sup.ID, tokenFor(sup.ID, sup.ID, nil)},
		"a hash of another profile":                   {f.op.ID, event(string(cheap.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionTokenIssued, Token: &cheap})},
		"a token under an actor's subject":            {f.op.ID, event(string(f.op.ID), issued)},
		"revoking an unknown token":                   {f.op.ID, event(string(unknownRevoked.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionTokenRevoked, Token: &unknownRevoked})},
		"a kind its payload does not carry": {f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
			fresh := actor(t, types.ActorKindSupervisor, f.op.ID)
			raw, _ := json.Marshal(types.IdentityEvent{Transition: types.IdentityEventTransitionActorCreated, Actor: &fresh})
			return []eventstore.Event{{ID: id(t, "evt"), Kind: identity.ActorRevoked + "/v1", Subject: string(fresh.ID), Payload: raw}}, nil
		}},
		"an event naming another actor": {f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
			fresh := actor(t, types.ActorKindSupervisor, f.sys.ID)
			evs, err := identity.CreateActor(fresh)
			evs[0].Actor = string(f.sys.ID)
			return evs, err
		}},
	} {
		if err := f.submit(c.by, c.apply); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Revoked actors issue, hold and create nothing, and are not revoked twice.
	gone := actor(t, types.ActorKindSupervisor, f.op.ID)
	goneOp := actor(t, types.ActorKindOperator, f.op.ID)
	f.must(f.op.ID, create(gone))
	f.must(f.op.ID, revokedActor(gone))
	f.must(f.op.ID, create(goneOp))
	f.must(f.op.ID, revokedActor(goneOp))
	for name, c := range map[string]step{
		"revoking an actor twice":                {f.op.ID, revokedActor(gone)},
		"a token for a revoked actor":            {f.op.ID, tokenFor(gone.ID, f.op.ID, nil)},
		"a token issued by a revoked actor":      {gone.ID, tokenFor(f.op.ID, gone.ID, nil)},
		"an actor created by a revoked one":      {gone.ID, created(actor(t, types.ActorKindSupervisor, gone.ID))},
		"an actor created by a revoked operator": {goneOp.ID, created(actor(t, types.ActorKindSupervisor, goneOp.ID))},
		"a token issued by a revoked operator":   {goneOp.ID, tokenFor(f.op.ID, goneOp.ID, nil)},
	} {
		if err := f.submit(c.by, c.apply); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A token is issued once and revoked once, changing only revoked_at, by
	// an operator or its holder: not by another supervisor.
	other := actor(t, types.ActorKindSupervisor, f.op.ID)
	f.must(f.op.ID, create(other))
	var supTok identity.Secret
	f.must(f.op.ID, issue(sup.ID, f.op.ID, &supTok))
	if err := f.submit(other.ID, revokeToken(supTok)); err == nil {
		t.Error("another supervisor revoked a token: accepted")
	}
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return tok, nil })
	if err := f.submit(f.op.ID, event(string(issued.Token.ID), issued)); err == nil {
		t.Error("a token issued twice: accepted")
	}
	moved := *issued.Token
	moved.RevokedAt, moved.Actor = &revoked, sup.ID
	if err := f.submit(f.op.ID, event(string(moved.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionTokenRevoked, Token: &moved})); err == nil {
		t.Error("a token revocation that moves the token: accepted")
	}
	f.must(sup.ID, revokeToken(supTok)) // the holder revokes its own
	if err := f.submit(f.op.ID, revokeToken(supTok)); err == nil {
		t.Error("revoking a token twice: accepted")
	}
}
