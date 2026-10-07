// SPDX-License-Identifier: Apache-2.0

package authority

// Enrolment and actor administration (docs/design/m0/14 PR 7a; 04 "Actors
// and tokens"): agent.enrol and actor.revoke. Each Derive validates its
// scope against the action's schema and derives the named actor as the
// command's object; the logic, which the request layer submits with the
// command, writes identity events that identity.Projector admits or
// refuses.
//
// Decisions:
//   - Operators only (operator_only). Only an operator revokes an actor
//     (the identity fold). A supervisor's enrolment would be a human-policy
//     proposal (14 H1; agent.enrol's class is human, which a grant cannot
//     lower), and a human confirmation is refused until the dashboard's
//     WebAuthn is deployed (14 R6), so in this build it could never be
//     applied, and its token could never be delivered: the confirmer's
//     response is a dashboard's, and the proposer must never receive it (a
//     supervisor holding a second identity would defeat separation of
//     duties and the one seat). It is refused rather than left as a proposal
//     that can only expire; neither handler is an Applier.
//   - The seat: neither action is exempt (only taking or asking for the
//     seat is, 14 N4/P4); operators never need it (08:80), and supervisors
//     are refused before the guard.
//   - agent.enrol's change class is its own (D32); it creates no grant, so
//     it has no grantee. actor.revoke only narrows authority: no class.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The admin actions, their scope schemas and the object type they bind.
const (
	EnrolAction       = "agent.enrol"
	RevokeActorAction = "actor.revoke"
	EnrolScope        = "api/scope/agent-enrol"
	RevokeActorScope  = "api/scope/actor-revoke"
	ActorObjectType   = "actor"
)

// AdminHandlers are the admin actions' handlers, by action.
func AdminHandlers() map[string]Handler {
	return map[string]Handler{EnrolAction: EnrolHandler{}, RevokeActorAction: RevokeActorHandler{}}
}

// mint is identity.Mint; tests count its calls.
var mint = identity.Mint

// hasCommand is eventstore.Tx.HasCommand, Enrol's pre-check; tests disable it.
var hasCommand = (*eventstore.Tx).HasCommand

// ActorRecord is actor id's record, its digest's definition (14 P3).
func ActorRecord(id types.ActorID) func(*eventstore.Tx) (json.RawMessage, bool, error) {
	return func(tx *eventstore.Tx) (json.RawMessage, bool, error) {
		return tx.Get(identity.ActorProjection, string(id))
	}
}

// enrolDeferred is why a supervisor does not enrol in this build.
const enrolDeferred = "a supervisor's enrolment needs a human confirmation, which this build does not take (14 R6)"

func operatorOnly(by types.Actor, action, why string) error {
	if by.Kind != types.ActorKindOperator {
		return refusal(vocab.ReasonOperatorOnly, "%s is an operator's in this build: %s", action, why)
	}
	return nil
}

// EnrolHandler is agent.enrol's handler.
type EnrolHandler struct{}

// Derive derives the supervisor actor the scope names.
func (EnrolHandler) Derive(_ *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	in, err := enrolScope(input)
	if err != nil {
		return Derived{}, err
	}
	if err := operatorOnly(by, EnrolAction, enrolDeferred); err != nil {
		return Derived{}, err
	}
	return Derived{Object: string(in.Actor), ObjectType: ActorObjectType, Classes: []string{EnrolAction}, Record: ActorRecord(in.Actor)}, nil
}

// Enrol is agent.enrol's request layer (14 H1, N3) for a caller that
// submits through it: EnrolRequest, Submit, then the delivery. The secret
// comes back only when this call freshly applied the enrolment.
func (a Authorizer) Enrol(ctx context.Context, s *eventstore.Store, c eventstore.Command) (eventstore.Committed, identity.Secret, bool, error) {
	decide, deliver, err := a.EnrolRequest(ctx, s, c)
	if err != nil {
		return eventstore.Committed{}, identity.Secret{}, false, err
	}
	out, err := s.Submit(ctx, c, decide)
	if err != nil {
		return out, identity.Secret{}, false, err
	}
	secret, ok := deliver(out)
	return out, secret, ok, nil
}

// EnrolRequest is agent.enrol's request layer before Submit
// (issueRequest): the new actor's first token, recorded with the actor.
func (Authorizer) EnrolRequest(ctx context.Context, s *eventstore.Store, c eventstore.Command) (eventstore.Decide, func(eventstore.Committed) (identity.Secret, bool), error) {
	if c.Action != EnrolAction {
		return nil, nil, fmt.Errorf("authority: %s is not %s", c.Action, EnrolAction)
	}
	in, err := enrolScope(c.Input)
	if err != nil {
		return nil, nil, err
	}
	holder := func(*eventstore.Tx) (types.ActorID, error) { return in.Actor, nil }
	return issueRequest(ctx, s, c, enrolDeferred, holder, func(i identity.Issued) eventstore.Decide { return enrolLogic(c, in, i) })
}

// issueRequest is the request layer of an action that issues a token
// (14 N3): it mints the token outside the writer transaction and returns
// the logic that records its hash, and the delivery, which yields the
// secret only for a fresh applied commit (identity.Issued.Deliver);
// otherwise the secret is dropped. A cheap read first skips the Argon2id
// mint for a caller who is not a live operator, for a key already stored
// (a replay records no token) and for no live holder (logic refuses it);
// the writer rechecks all three in its transaction.
func issueRequest(ctx context.Context, s *eventstore.Store, c eventstore.Command, why string, holder func(*eventstore.Tx) (types.ActorID, error),
	logic func(identity.Issued) eventstore.Decide) (eventstore.Decide, func(eventstore.Committed) (identity.Secret, bool), error) {
	nothing := func(eventstore.Committed) (identity.Secret, bool) { return identity.Secret{}, false }
	var stored bool
	var to types.ActorID
	if err := s.View(ctx, func(tx *eventstore.Tx) error {
		by, err := identity.ActorOf(tx, c)
		if err == nil {
			err = operatorOnly(by, c.Action, why)
		}
		if err == nil {
			stored, err = hasCommand(tx, c.Actor, c.IdempotencyKey)
		}
		if err == nil && !stored {
			to, err = holder(tx)
		}
		return err
	}); err != nil {
		return nil, nil, err
	}
	switch {
	case stored:
		return func(*eventstore.Tx) (eventstore.Decision, error) {
			return eventstore.Decision{}, errors.New("authority: a stored command only replays")
		}, nothing, nil
	case to == "":
		return logic(identity.Issued{}), nothing, nil
	}
	issued, err := mint(to, types.ActorID(c.Actor), time.Now())
	if err != nil {
		return nil, nil, err
	}
	return logic(issued), issued.Deliver, nil
}

// enrolScope and revokeScope decode a scope, an invalid one wrapping
// ErrInvalidScope.
func enrolScope(raw json.RawMessage) (types.AgentEnrolScope, error) {
	in, err := contract.Decode[types.AgentEnrolScope](EnrolScope, raw)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrInvalidScope, err)
	}
	return in, err
}

func revokeScope(raw json.RawMessage) (types.ActorRevokeScope, error) {
	in, err := contract.Decode[types.ActorRevokeScope](RevokeActorScope, raw)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrInvalidScope, err)
	}
	return in, err
}

// enrolLogic creates the supervisor and records its token's hash, created
// at the command's time.
func enrolLogic(c eventstore.Command, in types.AgentEnrolScope, issued identity.Issued) eventstore.Decide {
	return eventstore.Applies(func(t *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		evs, err := identity.CreateActor(types.Actor{ID: in.Actor, Kind: types.ActorKindSupervisor, Name: in.Name,
			DeclaredAgent: in.DeclaredAgent, CreatedBy: types.ActorID(c.Actor)})
		tok, err2 := recordIssued(t, c, in.Actor, issued)
		if err := errors.Join(err, err2); err != nil {
			return nil, nil, err
		}
		res, err := json.Marshal(map[string]any{"actor": in.Actor, "token": issued.Token.ID})
		return append(evs, tok...), res, err
	})
}

// RevokeActorHandler is actor.revoke's handler.
type RevokeActorHandler struct{}

// Derive derives the actor the scope names.
func (RevokeActorHandler) Derive(_ *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	in, err := revokeScope(input)
	if err != nil {
		return Derived{}, err
	}
	if err := operatorOnly(by, RevokeActorAction, "only an operator revokes an actor"); err != nil {
		return Derived{}, err
	}
	return Derived{Object: string(in.Actor), ObjectType: ActorObjectType, Record: ActorRecord(in.Actor)}, nil
}

// RevokeActor is actor.revoke's logic for c: every live token of the actor
// revoked, then the actor (so an operator revoking itself still revokes its
// tokens), all at the command's one time, or a committed refusal when the
// actor is protected. The fold invalidates the actor's pending proposals
// and frees its seat.
func (Authorizer) RevokeActor(c eventstore.Command) eventstore.Decide {
	return func(tx *eventstore.Tx) (eventstore.Decision, error) {
		var none eventstore.Decision
		if c.Action != RevokeActorAction {
			return none, fmt.Errorf("authority: %s is not %s", c.Action, RevokeActorAction)
		}
		now := tx.Now()
		in, err := revokeScope(c.Input)
		if err != nil {
			return none, err
		}
		target, err := liveActor(tx, in.Actor)
		if err != nil {
			return none, fmt.Errorf("authority: %s: %w", RevokeActorAction, err)
		}
		if err := protectedActor(tx, target); err != nil {
			return committed(err)
		}
		keys, err := tx.Keys(identity.TokenProjection, "")
		if err != nil {
			return none, err
		}
		var evs []eventstore.Event
		revoked := []types.TokenID{}
		for _, k := range keys {
			t, _, err := loadJSON[types.Token](tx, identity.TokenProjection, k)
			if err != nil {
				return none, err
			}
			if t.Actor != target.ID || t.RevokedAt != nil {
				continue
			}
			e, err := identity.RevokeToken(tx, t.ID, now)
			if err != nil {
				return none, err
			}
			evs, revoked = append(evs, e...), append(revoked, t.ID)
		}
		e, err := identity.RevokeActor(tx, target.ID, now)
		if err != nil {
			return none, err
		}
		res, err := json.Marshal(map[string]any{"actor": target.ID, "tokens": revoked})
		return eventstore.Decision{Disposition: eventstore.DispositionApplied, Events: append(evs, e...), Result: res}, err
	}
}

// protectedActor refuses the system actor, and an operator no other live
// operator would outlive: the bootstrap is spent once and offline recovery
// (14 R7) serves only a live operator, so nothing could administer the
// installation again.
func protectedActor(tx *eventstore.Tx, target types.Actor) error {
	switch target.Kind {
	case types.ActorKindSystem:
		return refusal(vocab.ReasonActorProtected, "actor %s is the controller's own", target.ID)
	case types.ActorKindOperator:
		keys, err := tx.Keys(identity.ActorProjection, "")
		if err != nil {
			return err
		}
		for _, k := range keys {
			a, _, err := loadJSON[types.Actor](tx, identity.ActorProjection, k)
			if err != nil {
				return err
			}
			if a.ID != target.ID && a.Kind == types.ActorKindOperator && a.RevokedAt == nil {
				return nil
			}
		}
		return refusal(vocab.ReasonActorProtected, "actor %s is the last live operator", target.ID)
	}
	return nil
}
