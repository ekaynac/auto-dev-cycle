// SPDX-License-Identifier: Apache-2.0

package authority

// The command framework's authorization (docs/design/m0/14 PR 2b): the
// writer's Authorize hook takes the command's actor from its rechecked
// principal, never from the request, and asks the action's Handler to
// derive, server side, what the command touches; Decide judges that. An
// action without a handler is refused, so a build wires actions one handler
// at a time and every other action stays closed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Derived is what a command touches, as its handler derives it from the
// validated input and the current projections, never from what the
// request claims.
type Derived struct {
	Object  string           // the subject the command changes
	Project *types.ProjectID // nil for an action outside any project
	Grantee *types.ActorID   // the grantee of a change that creates or extends a grant
	Classes []string         // the effective change's sensitive classes
	// Record is the object's current record and whether it exists (14 P3):
	// existence is the record's, and a digest expectation is checked
	// against its canonical bytes. Nil: the action takes no expected state.
	Record func(t *eventstore.Tx) (json.RawMessage, bool, error)
	// SeatExempt marks an action a supervisor performs without holding the
	// seat (taking or asking for it, 14 N4/P4). Unmarked, a supervisor's
	// command is a supervisor write and needs the seat (fail closed).
	SeatExempt bool
	// ObjectType is the object's record type, which a proposal binds
	// (types.ObjectRef); an action that may be proposed must set it.
	ObjectType string
}

// Handler derives one action's Derived. It runs inside the writer
// transaction, for the authenticated actor; an error refuses the command.
type Handler interface {
	Derive(t *eventstore.Tx, actor types.Actor, input json.RawMessage) (Derived, error)
}

// ErrActionNotWired refuses an action this build has no handler for.
var ErrActionNotWired = errors.New("authority: the action has no handler in this build; it is refused")

// Authorizer is the writer's Authorize hook over the wired handlers, by
// action.
type Authorizer struct {
	Registry *vocab.Registry
	Handlers map[string]Handler
	Now      func() time.Time
	// Reviews is where proposal.confirm finds review receipts; nil: every
	// confirmation is refused (NoReviewSource).
	Reviews Reviews
}

// Authorize decides c for its principal's live actor: the handler's
// derivation feeds Request, and only an allowed command passes.
func (a Authorizer) Authorize(t *eventstore.Tx, c eventstore.Command) error {
	actor, err := identity.ActorOf(t, c)
	if err != nil {
		return err
	}
	h, ok := a.Handlers[c.Action]
	if !ok || isNil(h) || a.Registry == nil || a.Now == nil {
		return fmt.Errorf("%w: %s", ErrActionNotWired, c.Action)
	}
	d, err := h.Derive(t, actor, c.Input)
	if err != nil {
		return err
	}
	if d.Object == "" {
		return fmt.Errorf("authority: the %s handler derived no object", c.Action)
	}
	// The writer checks the expected state against this derivation: the
	// handler runs once per command.
	if err := t.BindObject(eventstore.Object{Subject: d.Object, Record: d.Record}); err != nil {
		return err
	}
	grants, revoked, err := GrantsOf(t, actor.ID)
	if err != nil {
		return err
	}
	dec := Decide(a.Registry, Request{Actor: actor, Action: c.Action, Project: d.Project, Classes: d.Classes,
		Grants: grants, Revoked: revoked, Grantee: d.Grantee, Now: a.Now()})
	// 08 "Supervisor seat": a supervisor write needs the seat, checked here,
	// in the writer transaction; operators' and the system's commands do not.
	if dec.Disposition != Refuse && actor.Kind == types.ActorKindSupervisor && !d.SeatExempt {
		if err := holdsSeat(t, actor.ID); err != nil {
			return err
		}
	}
	switch dec.Disposition {
	case Allow:
		return nil
	case Propose:
		// A proposal is committed, not an error (14 H2 item 4): the command
		// records it with the proposed disposition and its logic never runs.
		return a.propose(t, c, actor, d, dec)
	}
	return &Refusal{Reason: dec.Reason, Detail: dec.Detail}
}

// isNil: no handler, or a typed nil one (which would panic, not refuse).
func isNil(h Handler) bool {
	if h == nil {
		return true
	}
	switch v := reflect.ValueOf(h); v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	}
	return false
}
