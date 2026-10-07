// SPDX-License-Identifier: Apache-2.0

package identity

// Actors and tokens are events. Command helpers propose them; the
// Projector, in the same writer transaction, is the one place that accepts
// or refuses them, from the projected records alone, so a rebuild folds to
// the same state. Authenticate resolves a bearer to its live actor.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// EventSchema is the payload schema of every identity event.
const EventSchema = "records/identity-event"

// Projections: the latest record of each actor and token, by id.
const (
	ActorProjection = "actors"
	TokenProjection = "tokens"
)

// The transitions (events are <transition>/v1).
const (
	ActorCreated = string(types.IdentityEventTransitionActorCreated)
	ActorRevoked = string(types.IdentityEventTransitionActorRevoked)
	TokenIssued  = string(types.IdentityEventTransitionTokenIssued)
	TokenRevoked = string(types.IdentityEventTransitionTokenRevoked)
)

var transitions = []string{ActorCreated, ActorRevoked, TokenIssued, TokenRevoked}

// Kinds registers the identity event kinds with the event store.
func Kinds() eventstore.Kinds {
	k := eventstore.Kinds{}
	for _, t := range transitions {
		k[t+"/v1"] = eventstore.Kind{Schema: EventSchema}
	}
	k[TokenRecoveredKind] = eventstore.Kind{Schema: TokenRecoveredSchema}
	k[BootstrapRenewedKind] = eventstore.Kind{Schema: BootstrapRenewSchema}
	k[BootstrapIssuedKind] = eventstore.Kind{Schema: BootstrapRenewSchema}
	return k
}

// ErrUnauthenticated is every authentication failure: a malformed bearer, an
// unknown or revoked token, a wrong secret, a missing or revoked actor. One
// error, so a caller learns nothing about which.
var ErrUnauthenticated = errors.New("identity: unauthenticated")

func load[T any](tx *eventstore.Tx, projection, id string) (T, bool, error) {
	var rec T
	raw, ok, err := tx.Get(projection, id)
	if err != nil || !ok {
		return rec, false, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, false, fmt.Errorf("identity: %s %s: %w", projection, id, err)
	}
	return rec, true, nil
}

func put(tx *eventstore.Tx, projection, id string, rec any) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return tx.Put(projection, id, raw)
}

func newEvent(subject string, p types.IdentityEvent) (eventstore.Event, error) {
	id, err := ids.New("evt")
	if err != nil {
		return eventstore.Event{}, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return eventstore.Event{}, err
	}
	return eventstore.Event{ID: id, Kind: string(p.Transition) + "/v1", Subject: subject, Payload: raw}, nil
}

func stamp(now time.Time) *types.Time {
	t := now.UTC().Format(time.RFC3339Nano)
	return &t
}

// CreateActor proposes a new actor.
func CreateActor(a types.Actor) ([]eventstore.Event, error) {
	e, err := newEvent(string(a.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionActorCreated, Actor: &a})
	return []eventstore.Event{e}, err
}

// RevokeActor proposes revoking an actor; its tokens stop authenticating.
func RevokeActor(tx *eventstore.Tx, id types.ActorID, now time.Time) ([]eventstore.Event, error) {
	a, ok, err := load[types.Actor](tx, ActorProjection, string(id))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("identity: no actor %s", id))
	}
	a.RevokedAt = stamp(now)
	e, err := newEvent(string(id), types.IdentityEvent{Transition: types.IdentityEventTransitionActorRevoked, Actor: &a})
	return []eventstore.Event{e}, err
}

// IssueToken proposes a new token for actor and returns its secret, which
// exists only here: the event carries the hash.
func IssueToken(actor, createdBy types.ActorID, now time.Time) ([]eventstore.Event, Secret, error) {
	i, err := Mint(actor, createdBy, now)
	if err != nil {
		return nil, Secret{}, err
	}
	evs, err := i.Events()
	return evs, i.secret, err
}

// Issued is a token minted ahead of the command that records it (14 N3):
// the request layer mints it outside the writer transaction, the command's
// logic records only Token (its public id and hash, through Events), and
// the plaintext goes to the live response only through Deliver. It never
// enters the store, and a replay neither delivers nor records a new one.
type Issued struct {
	Token  types.Token
	secret Secret
}

// Mint makes a token for actor, to be recorded by a command.
func Mint(actor, createdBy types.ActorID, now time.Time) (Issued, error) {
	t, secret, err := mint(actor, createdBy, now)
	return Issued{Token: t, secret: secret}, err
}

// Events are the token's identity.token_issued event: the hash, never the
// secret.
func (i Issued) Events() ([]eventstore.Event, error) {
	t := i.Token
	e, err := newEvent(string(t.ID), types.IdentityEvent{Transition: types.IdentityEventTransitionTokenIssued, Token: &t})
	return []eventstore.Event{e}, err
}

// Deliver returns the secret only for the call that freshly applied the
// command. Any other result (a replay, proposed, invalidated, refused)
// gets nothing, and the caller drops the secret.
func (i Issued) Deliver(c eventstore.Committed) (Secret, bool) {
	if c.Disposition != eventstore.DispositionApplied || c.Replayed {
		return Secret{}, false
	}
	return i.secret, true
}

// RevokeToken proposes revoking a token.
func RevokeToken(tx *eventstore.Tx, id types.TokenID, now time.Time) ([]eventstore.Event, error) {
	t, ok, err := load[types.Token](tx, TokenProjection, string(id))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("identity: no token %s", id))
	}
	t.RevokedAt = stamp(now)
	e, err := newEvent(string(id), types.IdentityEvent{Transition: types.IdentityEventTransitionTokenRevoked, Token: &t})
	return []eventstore.Event{e}, err
}

// BootstrapProjection records the one-time bootstrap (04 "Bootstrap"):
// under "operator" the first operator, which the system actor created, and
// under "token" its first token, which the system actor issued. The system
// actor does each once. Under BootstrapSecretKey, the latest renewal (R8).
const BootstrapProjection = "identity_bootstrap"

// Projector accepts identity events and keeps the projections; other kinds
// pass through untouched.
type Projector struct{}

// Project is the eventstore.Hooks.Project fold for identity events. The
// event's actor is the command's (the store refuses any other), so it is
// who did it: a record's created_by must be that actor, and what each kind
// of actor may do is checked here, from the projections alone.
func (Projector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ok, err := projectRecovery(tx, ev); ok {
			if err != nil {
				return fmt.Errorf("identity: event %s: %w", ev.ID, err)
			}
			continue
		}
		var p types.IdentityEvent
		if !isIdentityKind(ev.Kind) {
			continue
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return fmt.Errorf("identity: event %s: %w", ev.ID, err)
		}
		if string(p.Transition)+"/v1" != ev.Kind {
			return fmt.Errorf("identity: event %s: kind %s carries %s", ev.ID, ev.Kind, p.Transition)
		}
		if err := apply(tx, ev.Subject, types.ActorID(ev.Actor), p); err != nil {
			return fmt.Errorf("identity: event %s: %w", ev.ID, err)
		}
	}
	return nil
}

func isIdentityKind(kind string) bool {
	for _, t := range transitions {
		if kind == t+"/v1" {
			return true
		}
	}
	return false
}

// liveActor loads an actor that exists and is not revoked.
func liveActor(tx *eventstore.Tx, id types.ActorID) (types.Actor, error) {
	a, ok, err := load[types.Actor](tx, ActorProjection, string(id))
	switch {
	case err != nil:
		return a, err
	case !ok:
		return a, fmt.Errorf("no actor %s", id)
	case a.RevokedAt != nil:
		return a, fmt.Errorf("actor %s is revoked", id)
	}
	return a, nil
}

// bootstrapped reports whether the bootstrap step key has happened.
func bootstrapped(tx *eventstore.Tx, key string) (string, bool, error) {
	v, ok, err := load[string](tx, BootstrapProjection, key)
	return v, ok, err
}

func apply(tx *eventstore.Tx, subject string, by types.ActorID, p types.IdentityEvent) error {
	switch p.Transition {
	case types.IdentityEventTransitionActorCreated:
		return createActor(tx, subject, by, *p.Actor)
	case types.IdentityEventTransitionActorRevoked:
		a := *p.Actor
		cur, err := liveActor(tx, a.ID)
		if err != nil || subject != string(a.ID) {
			return errors.Join(err, fmt.Errorf("revoking actor %s under subject %s", a.ID, subject))
		}
		if cur.Kind == types.ActorKindSystem {
			return fmt.Errorf("actor %s is the controller's own; it is not revoked", a.ID)
		}
		if a.RevokedAt == nil || !sameExceptRevoked(cur, a) {
			return fmt.Errorf("actor %s: a revocation changes only revoked_at", a.ID)
		}
		if revoker, err := liveActor(tx, by); err != nil || revoker.Kind != types.ActorKindOperator {
			return errors.Join(err, fmt.Errorf("actor %s: only an operator revokes an actor", a.ID))
		}
		return put(tx, ActorProjection, string(a.ID), a)
	case types.IdentityEventTransitionTokenIssued:
		return issueToken(tx, subject, by, *p.Token)
	case types.IdentityEventTransitionTokenRevoked:
		t := *p.Token
		cur, ok, err := load[types.Token](tx, TokenProjection, string(t.ID))
		if err != nil || !ok || cur.RevokedAt != nil || subject != string(t.ID) {
			return errors.Join(err, fmt.Errorf("token %s is not a live token under subject %s", t.ID, subject))
		}
		if t.RevokedAt == nil || !sameExceptRevoked(cur, t) {
			return fmt.Errorf("token %s: a revocation changes only revoked_at", t.ID)
		}
		// An operator revokes any token; a holder revokes its own.
		if revoker, err := liveActor(tx, by); err != nil || (revoker.Kind != types.ActorKindOperator && by != cur.Actor) {
			return errors.Join(err, fmt.Errorf("token %s: revoked only by an operator or its holder", t.ID))
		}
		return put(tx, TokenProjection, string(t.ID), t)
	}
	return fmt.Errorf("unknown transition %q", p.Transition)
}

// createActor admits an actor: the system actor only as the first actor of
// all, creating itself; the first operator once, by the system actor; any
// other operator or supervisor only by a live operator.
func createActor(tx *eventstore.Tx, subject string, by types.ActorID, a types.Actor) error {
	if subject != string(a.ID) {
		return fmt.Errorf("subject %s is not actor %s", subject, a.ID)
	}
	if a.CreatedBy != by {
		return fmt.Errorf("actor %s: created_by %s is not the command's actor %s", a.ID, a.CreatedBy, by)
	}
	if _, ok, err := load[types.Actor](tx, ActorProjection, string(a.ID)); err != nil || ok {
		return errors.Join(err, fmt.Errorf("actor %s exists", a.ID))
	}
	if a.RevokedAt != nil {
		return fmt.Errorf("actor %s is created revoked", a.ID)
	}
	if a.Kind == types.ActorKindSystem {
		existing, err := tx.Keys(ActorProjection, "")
		if err != nil || len(existing) > 0 || by != a.ID {
			return errors.Join(err, fmt.Errorf("actor %s: the system actor is the first actor, created by itself", a.ID))
		}
		return put(tx, ActorProjection, string(a.ID), a)
	}
	creator, err := liveActor(tx, by)
	if err != nil {
		return fmt.Errorf("creator: %w", err)
	}
	switch {
	case creator.Kind == types.ActorKindOperator:
	case creator.Kind == types.ActorKindSystem && a.Kind == types.ActorKindOperator:
		if _, done, err := bootstrapped(tx, "operator"); err != nil || done {
			return errors.Join(err, fmt.Errorf("actor %s: the system actor creates only the first operator", a.ID))
		}
		if err := put(tx, BootstrapProjection, "operator", string(a.ID)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("actor %s: a %s does not create a %s", a.ID, creator.Kind, a.Kind)
	}
	return put(tx, ActorProjection, string(a.ID), a)
}

// issueToken admits a token of the issuance profile for a live operator or
// supervisor: by a live operator, or once by the system actor for the first
// operator.
func issueToken(tx *eventstore.Tx, subject string, by types.ActorID, t types.Token) error {
	if subject != string(t.ID) {
		return fmt.Errorf("subject %s is not token %s", subject, t.ID)
	}
	if t.CreatedBy != by {
		return fmt.Errorf("token %s: created_by %s is not the command's actor %s", t.ID, t.CreatedBy, by)
	}
	if _, ok, err := load[types.Token](tx, TokenProjection, string(t.ID)); err != nil || ok {
		return errors.Join(err, fmt.Errorf("token %s exists", t.ID))
	}
	if t.RevokedAt != nil {
		return fmt.Errorf("token %s is issued revoked", t.ID)
	}
	if !issuedProfile(t.Hash) {
		return fmt.Errorf("token %s: the hash is not of the issuance profile", t.ID)
	}
	holder, err := liveActor(tx, t.Actor)
	if err != nil {
		return fmt.Errorf("holder: %w", err)
	}
	if holder.Kind == types.ActorKindSystem {
		return fmt.Errorf("token %s: the system actor holds no bearer token", t.ID)
	}
	issuer, err := liveActor(tx, by)
	if err != nil {
		return fmt.Errorf("issuer: %w", err)
	}
	first, ok, err := bootstrapped(tx, "operator")
	_, done, err2 := bootstrapped(tx, "token")
	if err := errors.Join(err, err2); err != nil {
		return err
	}
	switch issuer.Kind {
	case types.ActorKindOperator:
	case types.ActorKindSystem:
		if !ok || first != string(t.Actor) || done {
			return fmt.Errorf("token %s: the system actor issues only the first operator's first token", t.ID)
		}
	default:
		return fmt.Errorf("token %s: a %s does not issue tokens", t.ID, issuer.Kind)
	}
	// The first operator's first token, whoever issues it, ends the
	// bootstrap: the system actor issues nothing after it.
	if ok && first == string(t.Actor) && !done {
		if err := put(tx, BootstrapProjection, "token", string(t.ID)); err != nil {
			return err
		}
	}
	return put(tx, TokenProjection, string(t.ID), t)
}

// sameExceptRevoked: a and b are equal apart from revoked_at.
func sameExceptRevoked[T types.Actor | types.Token](a, b T) bool {
	switch x := any(&a).(type) {
	case *types.Actor:
		x.RevokedAt = any(b).(types.Actor).RevokedAt
	case *types.Token:
		x.RevokedAt = any(b).(types.Token).RevokedAt
	}
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// decoy is a fixed hash of the issuance profile that no token has (its
// secret was discarded): verifying against it when a token is unknown, or
// its stored hash unusable, costs what verifying a wrong secret costs.
const decoy = "$argon2id$v=19$m=65536,t=3,p=4$Gf44i/lNBEgRytid3+XXjQ$I0iFpDw4ZkDQ+CLZ1NdkZBbfcYLoS0baKTIT3mow3Ho"

// Authenticate resolves a bearer string to its live actor, or fails with
// ErrUnauthenticated. It reads through tx (a view, or the writer's Tx). A
// well-formed bearer always costs exactly one hash: the token's, or the
// decoy's when the token is unknown or its stored hash unusable. The
// token's state is consulted only after that.
func Authenticate(tx *eventstore.Tx, bearer string) (types.Actor, error) {
	a, _, err := authenticate(tx, bearer)
	return a, err
}

// authenticate is Authenticate also returning the token's id.
func authenticate(tx *eventstore.Tx, bearer string) (types.Actor, types.TokenID, error) {
	id, secret, ok := parse(bearer)
	if !ok {
		return types.Actor{}, "", ErrUnauthenticated
	}
	t, found, err := load[types.Token](tx, TokenProjection, string(id))
	if err != nil {
		return types.Actor{}, "", err
	}
	_, usable := parsePHC(t.Hash)
	hash := t.Hash
	if !found || !usable {
		hash = decoy
	}
	if !verify(hash, secret) || !found || !usable || t.RevokedAt != nil {
		return types.Actor{}, "", ErrUnauthenticated
	}
	a, err := liveActor(tx, t.Actor)
	if err != nil {
		return types.Actor{}, "", ErrUnauthenticated
	}
	return a, id, nil
}

// StillLive re-checks, without hashing again, that a bearer Authenticate
// already accepted for actor is still live: its token is not revoked and
// its actor exists and is not revoked. Long-lived streams call it on every
// change (04 "Read authorization"). ErrUnauthenticated means authority was
// lost; any other error is the store's (reading or decoding a record).
func StillLive(tx *eventstore.Tx, bearer string, actor types.ActorID) error {
	id, _, ok := parse(bearer)
	if !ok {
		return ErrUnauthenticated
	}
	if err := tokenLive(tx, id, actor); err != nil {
		return err
	}
	a, found, err := load[types.Actor](tx, ActorProjection, string(actor))
	if err != nil {
		return err
	}
	if !found || a.RevokedAt != nil {
		return ErrUnauthenticated
	}
	return nil
}

// tokenLive: token id exists, is not revoked and is actor's.
func tokenLive(tx *eventstore.Tx, id types.TokenID, actor types.ActorID) error {
	t, found, err := load[types.Token](tx, TokenProjection, string(id))
	if err != nil {
		return err
	}
	if !found || t.RevokedAt != nil || t.Actor != actor {
		return ErrUnauthenticated
	}
	return nil
}
