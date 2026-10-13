// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// ErrCommandsRefused is the writer's answer to every new command whose
// action is not wired: the Authorize hook rechecks the principal and asks
// the action's handler for its derivation, and an action without one fails
// closed (14 D1). (A repeat of a stored command, by the same live actor with
// the same key and input, still replays its stored result: the identity
// recheck, then the idempotency lookup, precede authorization, 03.)
var ErrCommandsRefused = authority.ErrActionNotWired

// hooks are the writer's hooks: the identity recheck, the authorizer over
// the wired handlers and the projector. proposal.confirm is wired without
// a source of review receipts, so every confirmation is refused
// (authority.NoReviewSource) until receipts are recorded.
func hooks(az authority.Authorizer, project func(*eventstore.Tx, []eventstore.Event) error) eventstore.Hooks {
	authorize := func(t *eventstore.Tx, c eventstore.Command) error {
		if slices.Contains([]string{bootstrapAction, issueAction, renewAction, recoverAction}, c.Action) {
			return authorizeSystem(t, c)
		}
		return az.Authorize(t, c)
	}
	return eventstore.Hooks{Identify: identity.Recheck, Authorize: authorize, Project: project}
}

// authorizer is the one authorizer over the wired handlers: the writer's
// Authorize hook and the request layer's logic (logics, issuers) share it.
func authorizer(reg *vocab.Registry) authority.Authorizer {
	return authority.Authorizer{Registry: reg, Handlers: handlers()}
}

// handlers are the wired actions' handlers: proposal.confirm,
// proposal.withdraw, note.add, the seat's, agent.enrol and actor.revoke
// (14 PR 7a), grant.create, grant.revoke and grant.extend (14 PR 7c, 7d),
// and token.issue, token.revoke and token.replace (14 M4, PR 7b). Every
// other registered action has none and is refused. project.create has a
// handler (authority.ProjectCreateHandler) that is not wired here: it stays
// deferred until P22a activates the intake capability (M1 slice plan §1).
func handlers() map[string]authority.Handler {
	hs := authority.SeatHandlers(0)
	maps.Copy(hs, authority.AdminHandlers())
	maps.Copy(hs, authority.TokenHandlers())
	hs[authority.ConfirmAction] = authority.ConfirmHandler{}
	hs[authority.WithdrawAction] = authority.WithdrawHandler{}
	hs[authority.NoteAction] = authority.NoteHandler{}
	hs[authority.GrantCreateAction] = authority.GrantCreateHandler{}
	hs[authority.GrantRevokeAction] = authority.GrantRevokeHandler{}
	hs[authority.GrantExtendAction] = authority.GrantExtendHandler{}
	return hs
}

// logic is a wired action's command logic, which the request layer passes
// to Submit with the command.
type logic func(eventstore.Command) eventstore.Decide

// issuer is the request layer of an action that issues a credential
// (14 N3): it mints outside the transaction and returns the logic to
// submit with the command and the delivery of what that commits.
type issuer func(context.Context, *eventstore.Store, eventstore.Command) (eventstore.Decide, func(eventstore.Committed) (identity.Secret, bool), error)

// issuers are the credential-issuing actions' request layers: agent.enrol,
// token.issue and token.replace.
func issuers(az authority.Authorizer) map[string]issuer {
	return map[string]issuer{authority.EnrolAction: az.EnrolRequest, authority.TokenIssueAction: az.TokenIssueRequest,
		authority.TokenReplaceAction: az.TokenReplaceRequest}
}

// logics are the wired actions' logic, by action: every action handlers
// wires has one, or an issuer (a test checks), and an action without
// either is refused before anything is submitted (14 D1).
func logics(az authority.Authorizer) map[string]logic {
	out := map[string]logic{authority.ConfirmAction: az.Confirm, authority.WithdrawAction: az.Withdraw, authority.NoteAction: authority.AddNote, authority.RevokeActorAction: az.RevokeActor,
		authority.GrantCreateAction: authority.GrantCreateHandler{}.Logic, authority.GrantRevokeAction: authority.GrantRevokeHandler{}.Logic,
		authority.GrantExtendAction: authority.GrantExtendHandler{}.Logic, authority.TokenRevokeAction: az.RevokeToken}
	for action, h := range az.Handlers {
		if s, ok := h.(authority.SeatHandler); ok {
			out[action] = s.Logic
		}
	}
	return out
}

// eventKinds merges the event kinds of every package the writer projects;
// a kind registered twice is refused rather than silently overwritten.
func eventKinds() (eventstore.Kinds, error) {
	all := eventstore.Kinds{}
	for _, ks := range []eventstore.Kinds{identity.Kinds(), authority.GrantKinds(), authority.SeatKinds(), authority.ProposalKinds(), authority.NoteKinds(), authority.ProjectKinds(), lease.Kinds(), ledger.Kinds()} {
		for k := range ks {
			if _, dup := all[k]; dup {
				return nil, fmt.Errorf("daemon: event kind %s is registered twice", k)
			}
		}
		maps.Copy(all, ks)
	}
	return all, nil
}

// projector is the writer's Project hook, composed as a rebuild replays
// it: each event goes through identity, then grants, the seat, proposals,
// notes, projects, leases with their capacity reservations, then the usage ledger,
// before the next event (eventstore.ProjectEach). A lease projector without Units admits
// no seal until units are wired (fail closed).
func projector(blobs lease.Blobs, holds map[string]types.Hold, r capacity.Reservations) func(*eventstore.Tx, []eventstore.Event) error {
	leases := lease.Projector{Blobs: blobs, Holds: holds}
	return eventstore.ProjectEach(
		identity.Projector{}.Project,
		authority.GrantProjector{}.Project,
		authority.SeatProjector{}.Project,
		authority.ProposalProjector{}.Project,
		authority.NoteProjector{}.Project,
		authority.ProjectProjector{}.Project,
		capacity.Hook(leases, r),
		ledger.Project,
	)
}
