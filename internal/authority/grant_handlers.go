// SPDX-License-Identifier: Apache-2.0

package authority

// The grant handlers (docs/design/m0/14 PR 7c; 04 "Grants", "Creation and
// revocation", which records the decisions): grant.create and grant.revoke.
// Each Derive validates its scope and derives the grant as the command's
// object; the logic writes the grant event GrantProjector admits or
// refuses. grant.create's class is its own and its grantee the scope's
// actor; whether it can be issued (issuable) is checked only where it is
// applied, an operator's derivation and ApplyConfirmed, never before a
// supervisor's authorization (no existence oracle). grant.revoke is an
// operator's (operator_only). Neither is seat-exempt.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The grant actions, their scope schemas and the object type they bind.
const (
	GrantCreateAction = "grant.create"
	GrantRevokeAction = "grant.revoke"
	GrantCreateScope  = "api/scope/grant-create"
	GrantRevokeScope  = "api/scope/grant-revoke"
	GrantObjectType   = "grant"
	// GrantIDProjection names each grant's grantee under the grant's id, so
	// a grant is found, and its id is taken, whoever holds it.
	GrantIDProjection = "grant_ids"
)

// ErrNoGrant refuses a revocation of a grant that does not exist or is
// already revoked.
var ErrNoGrant = errors.New("authority: no live grant")

// GrantProject is a grant's project: the one project it names, in its
// projects and its listed read projects; naming several, or every project,
// makes it installation-wide (nil).
func GrantProject(g types.Grant) *types.ProjectID {
	if g.Projects.All {
		return nil
	}
	named := slices.Compact(slices.Sorted(slices.Values(slices.Concat(g.Projects.List, g.Reads.Projects.List))))
	if len(named) != 1 {
		return nil
	}
	return &named[0]
}

// grantKeyOf is grant id's projection key, under whichever grantee.
func grantKeyOf(tx *eventstore.Tx, id types.GrantID) (string, bool, error) {
	actor, ok, err := loadJSON[types.ActorID](tx, GrantIDProjection, string(id))
	return grantKey(actor, id), ok, err
}

// GrantRecord is grant id's record, its digest's definition (14 P3).
func GrantRecord(id types.GrantID) func(*eventstore.Tx) (json.RawMessage, bool, error) {
	return func(tx *eventstore.Tx) (json.RawMessage, bool, error) {
		k, ok, err := grantKeyOf(tx, id)
		if err != nil || !ok {
			return nil, false, err
		}
		return tx.Get(GrantProjection, k)
	}
}

// grantOf is the grant a grant.create scope asks for, valid from at,
// issued by by under approval.
func grantOf(scope json.RawMessage, at time.Time, by types.ActorID, approval *types.ApprovalID) (types.Grant, error) {
	g, err := contract.Decode[types.Grant](GrantCreateScope, scope)
	g.ValidFrom, g.IssuedBy, g.Approval = types.Time(at.UTC().Format(time.RFC3339Nano)), by, approval
	return g, err
}

// GrantCreateHandler is grant.create's handler.
type GrantCreateHandler struct{}

// Derive derives the grant the scope names, its grantee and its class.
func (GrantCreateHandler) Derive(tx *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	g, err := contract.Decode[types.Grant](GrantCreateScope, input)
	if err != nil {
		return Derived{}, fmt.Errorf("%w: %w", ErrInvalidScope, err)
	}
	switch by.Kind {
	case types.ActorKindOperator: // applied at once: check now
		if err := issuable(tx, g); err != nil {
			return Derived{}, err
		}
	case types.ActorKindSupervisor: // proposed or refused: checked at application
	default:
		return Derived{}, operatorOnly(by, GrantCreateAction, "only an operator issues a grant")
	}
	return Derived{Object: string(g.ID), ObjectType: GrantObjectType, Project: GrantProject(g), Grantee: &g.Actor,
		Classes: []string{GrantCreateAction}, Record: GrantRecord(g.ID)}, nil
}

// issuable refuses a grant that cannot be issued at the command's time.
func issuable(tx *eventstore.Tx, g types.Grant) error {
	_, taken, err := grantKeyOf(tx, g.ID)
	grantee, known, err2 := loadJSON[types.Actor](tx, identity.ActorProjection, string(g.Actor))
	if err := errors.Join(err, err2); err != nil {
		return err
	}
	until, err := time.Parse(time.RFC3339Nano, string(g.ExpiresAt))
	switch {
	case taken:
		return refusal(vocab.ReasonGrantNotIssuable, "grant id %s is taken", g.ID)
	case err != nil || !tx.Now().Before(until):
		return refusal(vocab.ReasonGrantNotIssuable, "grant %s expires at %s, not after now", g.ID, g.ExpiresAt)
	case !known || grantee.RevokedAt != nil || grantee.Kind != types.ActorKindSupervisor:
		return refusal(vocab.ReasonGrantNotIssuable, "grantee %s is not a live supervisor", g.Actor)
	}
	return nil
}

// Logic is an allowed grant.create's logic: an operator issues the grant,
// valid from the command's time, with no approval.
func (GrantCreateHandler) Logic(c eventstore.Command) eventstore.Decide {
	return eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		return createGrantAt(tx, c.Input, types.ActorID(c.Actor), nil)
	})
}

// ApplyConfirmed applies a confirmed grant.create proposal: the grant, if
// it can still be issued (a refusal invalidates the proposal), is issued
// by the confirmer under the approval this confirmation records.
func (GrantCreateHandler) ApplyConfirmed(tx *eventstore.Tx, _, confirmer types.Actor, approval types.ApprovalID, input json.RawMessage) ([]eventstore.Event, json.RawMessage, error) {
	g, err := contract.Decode[types.Grant](GrantCreateScope, input) // unreachable: apply re-derived this scope
	if err == nil {
		err = issuable(tx, g)
	}
	if err != nil {
		return nil, nil, err
	}
	return createGrantAt(tx, input, confirmer.ID, &approval)
}

func createGrantAt(tx *eventstore.Tx, input json.RawMessage, by types.ActorID, approval *types.ApprovalID) ([]eventstore.Event, json.RawMessage, error) {
	g, err := grantOf(input, tx.Now(), by, approval)
	if err != nil {
		return nil, nil, err
	}
	evs, err := CreateGrant(tx, g)
	if err != nil {
		return nil, nil, err
	}
	res, err := json.Marshal(map[string]any{"grant": g.ID, "valid_from": g.ValidFrom})
	return evs, res, err
}

// GrantRevokeHandler is grant.revoke's handler.
type GrantRevokeHandler struct{}

// Derive derives the grant the scope names, for an operator only.
func (GrantRevokeHandler) Derive(tx *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	in, err := contract.Decode[types.GrantRevokeScope](GrantRevokeScope, input)
	if err != nil {
		return Derived{}, fmt.Errorf("%w: %w", ErrInvalidScope, err)
	}
	if err := operatorOnly(by, GrantRevokeAction, "the fold admits only an operator's revocation, and no grant delegates it"); err != nil {
		return Derived{}, err
	}
	k, ok, err := grantKeyOf(tx, in.Grant)
	if err == nil && !ok {
		err = fmt.Errorf("%w: %s", ErrNoGrant, in.Grant)
	}
	if err != nil {
		return Derived{}, err
	}
	e, ok, err := loadJSON[types.GrantEvent](tx, GrantProjection, k)
	switch {
	case err != nil:
		return Derived{}, err
	case !ok:
		return Derived{}, fmt.Errorf("authority: grant %s is indexed without its record", in.Grant)
	case e.Transition != types.GrantEventTransitionGrantCreated || e.RevokedAt != nil:
		return Derived{}, fmt.Errorf("%w: %s is revoked", ErrNoGrant, in.Grant)
	}
	return Derived{Object: string(in.Grant), ObjectType: GrantObjectType, Project: GrantProject(e.Grant), Record: GrantRecord(in.Grant)}, nil
}

// Logic is grant.revoke's logic: the bound grant revoked at the command's
// time. Its check that the derivation ran is defence in depth: the writer
// runs Authorize, and with it Derive, before any logic.
func (GrantRevokeHandler) Logic(c eventstore.Command) eventstore.Decide {
	return eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		id, bound := tx.Bound()
		k, ok, err := grantKeyOf(tx, types.GrantID(id))
		if err == nil && (!bound || !ok) {
			err = errors.New("authority: a grant revocation's logic runs only after its derivation")
		}
		if err != nil {
			return nil, nil, err
		}
		actor, _, _ := strings.Cut(k, "/")
		evs, err := RevokeGrant(tx, types.ActorID(actor), types.GrantID(id), tx.Now())
		if err != nil {
			return nil, nil, err
		}
		res, err := json.Marshal(map[string]any{"grant": id, "revoked_at": tx.Now().UTC().Format(time.RFC3339Nano)})
		return evs, res, err
	})
}

// approvalOf is approval id's record. A missing index entry or approval
// record leaves a zero approval, whose id never matches: an error.
func approvalOf(tx *eventstore.Tx, id types.ApprovalID) (types.Approval, error) {
	prp, _, err := loadJSON[types.ProposalID](tx, ProposalProjection, "approval_id/"+string(id))
	var a types.Approval
	if err == nil {
		a, _, err = loadJSON[types.Approval](tx, ProposalProjection, "approval/"+string(prp))
	}
	if err == nil && a.ID != id {
		err = fmt.Errorf("approval %s is not recorded", id)
	}
	return a, err
}

// approvedGrant: a grant naming an approval is exactly the grant that
// approval's grant.create (or grant.extend, whose old grant this command
// revoked) proposal asked for, issued by its approver.
func approvedGrant(tx *eventstore.Tx, ev eventstore.Event, g types.Grant) error {
	a, err := approvalOf(tx, *g.Approval)
	if err != nil || a.Actor != types.ActorID(ev.Actor) || (a.Action != GrantCreateAction && a.Action != GrantExtendAction) {
		return errors.Join(err, errors.New("its approval is not its issuer's approval of a grant.create or grant.extend"))
	}
	var want types.Grant
	if a.Action == GrantExtendAction {
		in, err1 := extendScopeOf(a.Scope)
		old, err2 := revokedBy(tx, in.Replaces, ev)
		want, err = in.Grant, errors.Join(err1, err2)
		want.Actor = old.Actor
	} else {
		want, err = grantOf(a.Scope, time.Time{}, "", nil)
	}
	// valid_from is the fold's to check (applyGrant), as for every grant.
	want.ValidFrom, want.IssuedBy, want.Approval = g.ValidFrom, g.IssuedBy, g.Approval
	if err != nil || !sameGrant(want, g) {
		return errors.Join(err, errors.New("it is not the grant its approval approved"))
	}
	return nil
}
