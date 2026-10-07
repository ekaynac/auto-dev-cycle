// SPDX-License-Identifier: Apache-2.0

package authority

// Token administration (docs/design/m0/14 M4, PR 7b; 04 "Token
// administration"): token.issue, token.revoke and token.replace.
//
// Decisions:
//   - Operators only (operator_only), as actor administration (admin.go).
//     The identity fold already admits only an operator's issue; a holder's
//     own revocation is left for a later build.
//   - Rotation is token.replace: one command revokes the token and issues
//     its holder a new one, so the holder keeps its actor and its grants
//     (grants name the actor, never a token); the old bearer is refused and
//     its streams close (identity.StillLive).
//   - A live operator stays an unrevoked operator actor whether or not it
//     holds a live token (04 "Enrolment and revocation"): revoking an
//     operator's last token is allowed, and offline recovery (14 R7) issues
//     it another.
//   - A missing or revoked target is a committed refusal (token_not_live),
//     final under its idempotency key.
//   - No change class: operators are unrestricted (D31), and a supervisor
//     is refused before any class would matter.

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

// The token actions, their scope schemas and the object type a token binds.
const (
	TokenIssueAction   = "token.issue"
	TokenRevokeAction  = "token.revoke"
	TokenReplaceAction = "token.replace"
	TokenIssueScope    = "api/scope/token-issue"
	TokenRevokeScope   = "api/scope/token-revoke"
	TokenReplaceScope  = "api/scope/token-replace"
	TokenObjectType    = "token"
)

const tokenAdmin = "only an operator administers tokens"

// TokenHandlers are the token actions' handlers, by action.
func TokenHandlers() map[string]Handler {
	return map[string]Handler{TokenIssueAction: tokenHandler(TokenIssueAction),
		TokenRevokeAction: tokenHandler(TokenRevokeAction), TokenReplaceAction: tokenHandler(TokenReplaceAction)}
}

// TokenRecord is token id's record, its digest's definition (14 P3).
func TokenRecord(id types.TokenID) func(*eventstore.Tx) (json.RawMessage, bool, error) {
	return func(tx *eventstore.Tx) (json.RawMessage, bool, error) {
		return tx.Get(identity.TokenProjection, string(id))
	}
}

// tokenHandler is a token action's handler, by its action.
type tokenHandler string

// Derive derives the holder token.issue names, or the token the others do.
func (h tokenHandler) Derive(_ *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	if h == TokenIssueAction {
		in, err := decodeScope[types.TokenIssueScope](TokenIssueScope, input)
		if err == nil {
			err = operatorOnly(by, string(h), tokenAdmin)
		}
		return Derived{Object: string(in.Actor), ObjectType: ActorObjectType, Record: ActorRecord(in.Actor)}, err
	}
	id, err := tokenOf(string(h), input)
	if err == nil {
		err = operatorOnly(by, string(h), tokenAdmin)
	}
	return Derived{Object: string(id), ObjectType: TokenObjectType, Record: TokenRecord(id)}, err
}

// decodeScope decodes a scope, an invalid one wrapping ErrInvalidScope.
func decodeScope[T any](schema string, raw json.RawMessage) (T, error) {
	in, err := contract.Decode[T](schema, raw)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrInvalidScope, err)
	}
	return in, err
}

// tokenOf is the token token.revoke's or token.replace's scope names.
func tokenOf(action string, raw json.RawMessage) (types.TokenID, error) {
	switch action {
	case TokenRevokeAction:
		in, err := decodeScope[types.TokenRevokeScope](TokenRevokeScope, raw)
		return in.Token, err
	case TokenReplaceAction:
		in, err := decodeScope[types.TokenReplaceScope](TokenReplaceScope, raw)
		return in.Token, err
	}
	return "", fmt.Errorf("authority: %s names no token", action)
}

// TokenIssueRequest is token.issue's request layer (issueRequest): a new
// token for the scope's live operator or supervisor.
func (Authorizer) TokenIssueRequest(ctx context.Context, s *eventstore.Store, c eventstore.Command) (eventstore.Decide, func(eventstore.Committed) (identity.Secret, bool), error) {
	if c.Action != TokenIssueAction {
		return nil, nil, fmt.Errorf("authority: %s is not %s", c.Action, TokenIssueAction)
	}
	in, err := decodeScope[types.TokenIssueScope](TokenIssueScope, c.Input)
	if err != nil {
		return nil, nil, err
	}
	holder := func(tx *eventstore.Tx) (types.ActorID, error) { return orNone(liveHolder(tx, in.Actor)) }
	return issueRequest(ctx, s, c, tokenAdmin, holder, func(issued identity.Issued) eventstore.Decide {
		return func(tx *eventstore.Tx) (eventstore.Decision, error) {
			if _, err := liveHolder(tx, in.Actor); err != nil {
				return committed(err)
			}
			if err := minted(issued, string(in.Actor)); err != nil {
				return committed(err)
			}
			evs, err := recordIssued(tx, c, in.Actor, issued)
			return appliedWith(evs, err, map[string]any{"actor": in.Actor, "token": issued.Token.ID})
		}
	})
}

// TokenReplaceRequest is token.replace's request layer (issueRequest): the
// scope's live token revoked and its holder's new one recorded, in one
// command.
func (Authorizer) TokenReplaceRequest(ctx context.Context, s *eventstore.Store, c eventstore.Command) (eventstore.Decide, func(eventstore.Committed) (identity.Secret, bool), error) {
	if c.Action != TokenReplaceAction {
		return nil, nil, fmt.Errorf("authority: %s is not %s", c.Action, TokenReplaceAction)
	}
	id, err := tokenOf(c.Action, c.Input)
	if err != nil {
		return nil, nil, err
	}
	holder := func(tx *eventstore.Tx) (types.ActorID, error) {
		t, err := liveToken(tx, id)
		return orNone(t.Actor, err)
	}
	return issueRequest(ctx, s, c, tokenAdmin, holder, func(issued identity.Issued) eventstore.Decide {
		return func(tx *eventstore.Tx) (eventstore.Decision, error) {
			old, err := liveToken(tx, id)
			if err == nil {
				err = minted(issued, string(id))
			}
			if err != nil {
				return committed(err)
			}
			evs, err := identity.RevokeToken(tx, old.ID, tx.Now())
			tok, err2 := recordIssued(tx, c, old.Actor, issued)
			return appliedWith(append(evs, tok...), errors.Join(err, err2),
				map[string]any{"actor": old.Actor, "token": issued.Token.ID, "replaced": old.ID})
		}
	})
}

// RevokeToken is token.revoke's logic for c: the scope's live token
// revoked at the command's time, or a committed refusal.
func (Authorizer) RevokeToken(c eventstore.Command) eventstore.Decide {
	return func(tx *eventstore.Tx) (eventstore.Decision, error) {
		if c.Action != TokenRevokeAction {
			return eventstore.Decision{}, fmt.Errorf("authority: %s is not %s", c.Action, TokenRevokeAction)
		}
		id, err := tokenOf(c.Action, c.Input)
		if err != nil {
			return eventstore.Decision{}, err
		}
		t, err := liveToken(tx, id)
		if err != nil {
			return committed(err)
		}
		evs, err := identity.RevokeToken(tx, t.ID, tx.Now())
		return appliedWith(evs, err, map[string]any{"actor": t.Actor, "token": t.ID})
	}
}

// liveHolder is id when it is a live operator or supervisor, the refusal
// token_not_live otherwise.
func liveHolder(tx *eventstore.Tx, id types.ActorID) (types.ActorID, error) {
	a, ok, err := loadJSON[types.Actor](tx, identity.ActorProjection, string(id))
	switch {
	case err != nil:
		return "", err
	case !ok || a.RevokedAt != nil || a.Kind == types.ActorKindSystem:
		return "", refusal(vocab.ReasonTokenNotLive, "actor %s is not a live operator or supervisor", id)
	}
	return id, nil
}

// liveToken is token id when it and its holder are live, the refusal
// token_not_live otherwise.
func liveToken(tx *eventstore.Tx, id types.TokenID) (types.Token, error) {
	t, ok, err := loadJSON[types.Token](tx, identity.TokenProjection, string(id))
	switch {
	case err != nil:
		return t, err
	case !ok || t.RevokedAt != nil:
		return t, refusal(vocab.ReasonTokenNotLive, "token %s is missing or revoked", id)
	}
	_, err = liveHolder(tx, t.Actor)
	return t, err
}

// orNone is a request layer's holder: none ("") for a refusal, which the
// logic commits, so nothing is minted for it.
func orNone(id types.ActorID, err error) (types.ActorID, error) {
	var r *Refusal
	if errors.As(err, &r) {
		return "", nil
	}
	return id, err
}

// minted refuses a command whose request layer minted nothing because it
// read no live target, which has since become live (an enrolment, say):
// the refusal is committed like any token_not_live, and a new key retries.
func minted(issued identity.Issued, target string) error {
	if issued.Token.ID == "" {
		return refusal(vocab.ReasonTokenNotLive, "%s was not live when the command was read, so nothing was minted; retry under a new idempotency key", target)
	}
	return nil
}

// recordIssued is the token.issued event of a token minted for holder by
// the command's actor, created at the command's time; a token minted for
// anything else is never recorded.
func recordIssued(tx *eventstore.Tx, c eventstore.Command, holder types.ActorID, issued identity.Issued) ([]eventstore.Event, error) {
	if issued.Token.Actor != holder || issued.Token.CreatedBy != types.ActorID(c.Actor) {
		return nil, errors.New("authority: the token minted is not this command's")
	}
	issued.Token.CreatedAt = tx.Now().UTC().Format(time.RFC3339Nano)
	return issued.Events()
}

// committed is a refusal's committed decision; any other error is returned.
func committed(err error) (eventstore.Decision, error) {
	var r *Refusal
	if errors.As(err, &r) {
		return failed(eventstore.DispositionRefused, nil, r.Reason.String(), r.Detail)
	}
	return eventstore.Decision{}, err
}

// appliedWith is an applied decision of evs with result, unless err.
func appliedWith(evs []eventstore.Event, err error, result map[string]any) (eventstore.Decision, error) {
	if err != nil {
		return eventstore.Decision{}, err
	}
	res, err := json.Marshal(result)
	return eventstore.Decision{Disposition: eventstore.DispositionApplied, Events: evs, Result: res}, err
}
