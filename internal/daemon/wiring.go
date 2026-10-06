// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"fmt"
	"maps"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// ErrCommandsRefused is the writer's answer to every new command until
// command dispatch is wired: it fails closed. (A repeat of a stored command,
// same actor, key and input, still replays its stored result: the writer's
// idempotency lookup precedes authorization, 03.)
var ErrCommandsRefused = errors.New("adcd: command dispatch is not wired in this build; every new command is refused")

func refuseAll(*eventstore.Tx, eventstore.Command) error { return ErrCommandsRefused }

// eventKinds merges the event kinds of every package the writer projects;
// a kind registered twice is refused rather than silently overwritten.
func eventKinds() (eventstore.Kinds, error) {
	all := eventstore.Kinds{}
	for _, ks := range []eventstore.Kinds{identity.Kinds(), authority.GrantKinds(), lease.Kinds(), ledger.Kinds()} {
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
// it: each event goes through identity, then grants, then leases with
// their capacity reservations, then the usage ledger, before the next
// event (eventstore.ProjectEach). A lease projector without Units admits
// no seal until units are wired (fail closed).
func projector(blobs lease.Blobs, holds map[string]types.Hold, r capacity.Reservations) func(*eventstore.Tx, []eventstore.Event) error {
	leases := lease.Projector{Blobs: blobs, Holds: holds}
	return eventstore.ProjectEach(
		identity.Projector{}.Project,
		authority.GrantProjector{}.Project,
		capacity.Hook(leases, r),
		ledger.Project,
	)
}
