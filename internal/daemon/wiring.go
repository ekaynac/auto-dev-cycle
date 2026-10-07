// SPDX-License-Identifier: Apache-2.0

package daemon

import (
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
func hooks(reg *vocab.Registry, project func(*eventstore.Tx, []eventstore.Event) error) eventstore.Hooks {
	az := authority.Authorizer{Registry: reg, Handlers: handlers()}
	authorize := func(t *eventstore.Tx, c eventstore.Command) error {
		if slices.Contains([]string{bootstrapAction, issueAction, renewAction}, c.Action) {
			return authorizeSystem(t, c)
		}
		return az.Authorize(t, c)
	}
	return eventstore.Hooks{Identify: identity.Recheck, Authorize: authorize, Project: project}
}

// handlers are the wired actions' handlers: proposal.confirm and the
// seat's. Every other registered action has none and is refused.
func handlers() map[string]authority.Handler {
	hs := authority.SeatHandlers(0)
	hs[authority.ConfirmAction] = authority.ConfirmHandler{}
	return hs
}

// eventKinds merges the event kinds of every package the writer projects;
// a kind registered twice is refused rather than silently overwritten.
func eventKinds() (eventstore.Kinds, error) {
	all := eventstore.Kinds{}
	for _, ks := range []eventstore.Kinds{identity.Kinds(), authority.GrantKinds(), authority.SeatKinds(), authority.ProposalKinds(), lease.Kinds(), ledger.Kinds()} {
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
// it: each event goes through identity, then grants, the seat, proposals, leases with
// their capacity reservations, then the usage ledger, before the next
// event (eventstore.ProjectEach). A lease projector without Units admits
// no seal until units are wired (fail closed).
func projector(blobs lease.Blobs, holds map[string]types.Hold, r capacity.Reservations) func(*eventstore.Tx, []eventstore.Event) error {
	leases := lease.Projector{Blobs: blobs, Holds: holds}
	return eventstore.ProjectEach(
		identity.Projector{}.Project,
		authority.GrantProjector{}.Project,
		authority.SeatProjector{}.Project,
		authority.ProposalProjector{}.Project,
		capacity.Hook(leases, r),
		ledger.Project,
	)
}
