// SPDX-License-Identifier: Apache-2.0

package identity

// A Principal is who sends a command, as the transport authenticated it
// (docs/design/m0/14 "Replay authentication"): the actor a bearer token
// resolved to and the listener the request came through. Only
// Authenticated makes one from a token, and it never makes the system
// actor's; only SystemPrincipal makes that, from the store's system claim,
// which the daemon takes at startup (eventstore.ClaimSystem). A command's actor is the principal's, never the
// caller's choice, and the writer rechecks the principal (Recheck) inside
// its transaction before anything else, a replay included.

import (
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// Listener is the transport a request came through, as the server that
// accepted it says (the listeners themselves come with the server). The
// zero value is no listener and authenticates nothing.
type Listener int

const (
	PrivateSocket Listener = iota + 1 // the daemon uid's 0600 socket
	SharedSocket                      // the optional group socket
	HTTPS                             // the optional verified-TLS listener
)

func (l Listener) valid() bool { return l >= PrivateSocket && l <= HTTPS }

// Principal is an authenticated sender. Its zero value is unusable: it
// names no actor and no listener, and Recheck refuses it.
type Principal struct {
	actor  types.ActorID
	token  types.TokenID // empty only for the system principal
	via    Listener
	system eventstore.SystemClaim // held only by the system principal
}

// Actor is the principal's actor.
func (p Principal) Actor() types.ActorID { return p.actor }

// Via is the listener the principal authenticated on.
func (p Principal) Via() Listener { return p.via }

// Token is the id of the token the principal presented (never its hash);
// empty for the system principal.
func (p Principal) Token() types.TokenID { return p.token }

// Live is the principal's live actor record in tx, rechecked as Recheck
// checks a command's principal.
func (p Principal) Live(tx *eventstore.Tx) (types.Actor, error) {
	return ActorOf(tx, p.Command("", "", "", nil, nil))
}

// PrincipalActor is the actor as the event store sees it.
func (p Principal) PrincipalActor() string { return string(p.actor) }

// Authenticated resolves a bearer presented on listener via to its
// principal, or fails with ErrUnauthenticated (as Authenticate does). The
// system actor holds no token, and is refused here even if one named it.
func Authenticated(tx *eventstore.Tx, bearer string, via Listener) (Principal, error) {
	if !via.valid() {
		return Principal{}, ErrUnauthenticated
	}
	a, tok, err := authenticate(tx, bearer)
	if err != nil {
		return Principal{}, err
	}
	if a.Kind == types.ActorKindSystem {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{actor: a.ID, token: tok, via: via}, nil
}

// SystemPrincipal is the system actor's principal, on the private socket:
// only from the store's system claim (which the daemon takes at startup),
// for the store's live system actor. It acts only on that store.
func SystemPrincipal(tx *eventstore.Tx, claim eventstore.SystemClaim, id types.ActorID) (Principal, error) {
	if !claim.Holds(tx) {
		return Principal{}, fmt.Errorf("identity: no system claim on this store: %w", ErrUnauthenticated)
	}
	a, err := liveActor(tx, id)
	if err != nil || a.Kind != types.ActorKindSystem {
		return Principal{}, fmt.Errorf("identity: %s is not the live system actor: %w", id, ErrUnauthenticated)
	}
	return Principal{actor: id, via: PrivateSocket, system: claim}, nil
}

// Command is a command of this principal: its actor is the principal's.
func (p Principal) Command(id, action, idempotencyKey string, input []byte, expected []eventstore.Expectation) eventstore.Command {
	return eventstore.Command{ID: id, Actor: string(p.actor), Action: action, IdempotencyKey: idempotencyKey,
		Input: input, Expected: expected, Principal: p}
}

// Recheck is the writer's Identify hook: inside the transaction, before
// the idempotency lookup, the command's principal must be one this package
// made, its actor the command's, and its token and actor still live (the
// system principal: its actor still the live system actor). Any failure is
// ErrUnauthenticated, unless the store could not be read.
func Recheck(tx *eventstore.Tx, c eventstore.Command) error {
	_, err := ActorOf(tx, c)
	return err
}

// ActorOf is Recheck returning the command's live actor record.
func ActorOf(tx *eventstore.Tx, c eventstore.Command) (types.Actor, error) {
	p, ok := c.Principal.(Principal)
	if !ok || !p.via.valid() || p.actor == "" || string(p.actor) != c.Actor {
		return types.Actor{}, ErrUnauthenticated
	}
	system := p.system != eventstore.SystemClaim{}
	switch {
	case system && !p.system.Holds(tx):
		return types.Actor{}, ErrUnauthenticated
	case !system:
		if err := tokenLive(tx, p.token, p.actor); err != nil {
			return types.Actor{}, err
		}
	}
	a, found, err := load[types.Actor](tx, ActorProjection, string(p.actor))
	switch {
	case err != nil:
		return types.Actor{}, err
	case !found || a.RevokedAt != nil || (a.Kind == types.ActorKindSystem) != system:
		return types.Actor{}, ErrUnauthenticated
	}
	return a, nil
}
