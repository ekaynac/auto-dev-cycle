// SPDX-License-Identifier: Apache-2.0

package authority

// grant.extend (docs/design/m0/14 N6/P6; 04 "Replacement"): an atomic
// replacement. One command revokes the old grant, creates a fresh one for
// the same grantee and records grant.replaced/v1, the lineage binding the
// old id to the complete new grant (and so to its approval); each event
// keeps its own subject, so both grants' versions advance. The command's
// object is the old grant, so a proposal is bound to its state. The class
// is grant.extend and the grantee the old grant's (no self-grant). As for
// grant.create, a supervisor is never told whether a grant exists or can
// be issued before Decide authorizes it: a grant it cannot replace (absent
// or revoked) is refused exactly as Decide refuses an uncovered one, and
// issuable runs only for an operator and in ApplyConfirmed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	GrantExtendAction = "grant.extend"
	GrantExtendScope  = "api/scope/grant-extend"
	// GrantReplaced is the lineage event kind; its subject is the old grant.
	GrantReplaced = "grant.replaced/v1"
	// GrantLineageProjection names, under each replaced grant's id, the
	// grant that replaced it.
	GrantLineageProjection = "grant_lineage"
	// GrantCommandProjection records, under <transition>/<command>/<grant>,
	// each command that created or revoked a grant.
	GrantCommandProjection = "grant_commands"
)

func commandKey(t types.GrantEventTransition, command string, id types.GrantID) string {
	return string(t) + "/" + command + "/" + string(id)
}

// byCommand: ev's command moved grant id by transition t.
func byCommand(tx *eventstore.Tx, t types.GrantEventTransition, id types.GrantID, ev eventstore.Event) (bool, error) {
	if ev.Command == nil {
		return false, nil
	}
	_, ok, err := tx.Get(GrantCommandProjection, commandKey(t, *ev.Command, id))
	return ok, err
}

// extendScope is a grant.extend scope: the new grant, but its grantee.
type extendScope struct {
	types.Grant
	Replaces types.GrantID `json:"replaces"`
}

func extendScopeOf(raw json.RawMessage) (extendScope, error) {
	in, err := contract.Decode[extendScope](GrantExtendScope, raw)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrInvalidScope, err)
	}
	return in, err
}

// liveGrant is grant id's record, and whether it is live (created and
// never revoked).
func liveGrant(tx *eventstore.Tx, id types.GrantID) (types.GrantEvent, bool, error) {
	k, ok, err := grantKeyOf(tx, id)
	if err != nil || !ok {
		return types.GrantEvent{}, false, err
	}
	e, ok, err := loadJSON[types.GrantEvent](tx, GrantProjection, k)
	return e, ok && e.Transition == types.GrantEventTransitionGrantCreated && e.RevokedAt == nil, err
}

// GrantExtendHandler is grant.extend's handler.
type GrantExtendHandler struct{}

// Derive derives the old grant as the object, its grantee, and the
// project of both grants (one project only when they name the same one).
func (GrantExtendHandler) Derive(tx *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	in, err := extendScopeOf(input)
	if err != nil {
		return Derived{}, err
	}
	if by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return Derived{}, operatorOnly(by, GrantExtendAction, "only an operator issues a grant")
	}
	old, live, err := liveGrant(tx, in.Replaces)
	switch {
	case err != nil:
		return Derived{}, err
	case !live && by.Kind == types.ActorKindSupervisor: // Decide's own words for an uncovered grant
		return Derived{}, refusal(vocab.ReasonActionNotGranted, "no grant in force covers %s for actor %s", GrantExtendAction, by.ID)
	case !live:
		return Derived{}, fmt.Errorf("%w: %s", ErrNoGrant, in.Replaces)
	}
	g := in.Grant
	g.Actor = old.Grant.Actor
	if by.Kind == types.ActorKindOperator { // applied at once: check now
		if err := issuable(tx, g); err != nil {
			return Derived{}, err
		}
	}
	project, was := GrantProject(g), GrantProject(old.Grant)
	if project == nil || was == nil || *project != *was {
		project = nil
	}
	return Derived{Object: string(in.Replaces), ObjectType: GrantObjectType, Project: project, Grantee: &old.Grant.Actor,
		Classes: []string{GrantExtendAction}, Record: GrantRecord(in.Replaces)}, nil
}

// Logic is an allowed grant.extend's logic: an operator replaces the grant
// at once, with no approval.
func (GrantExtendHandler) Logic(c eventstore.Command) eventstore.Decide {
	return eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		in, err := extendScopeOf(c.Input)
		if id, bound := tx.Bound(); err == nil && (!bound || id != string(in.Replaces)) {
			err = errors.New("authority: a grant replacement's logic runs only on its derived grant")
		}
		if err != nil {
			return nil, nil, err
		}
		return replaceGrant(tx, c.Input, types.ActorID(c.Actor), nil)
	})
}

// ApplyConfirmed applies a confirmed grant.extend proposal: the new grant
// issuable (a refusal invalidates the proposal), issued by the confirmer
// under the approval it records. The old grant is live: a revocation
// invalidates the proposal in the fold (its object moved).
func (GrantExtendHandler) ApplyConfirmed(tx *eventstore.Tx, _, confirmer types.Actor, approval types.ApprovalID, input json.RawMessage) ([]eventstore.Event, json.RawMessage, error) {
	in, err := extendScopeOf(input) // unreachable: apply re-derived this scope
	old, live, err2 := liveGrant(tx, in.Replaces)
	if err2 == nil && !live {
		err2 = fmt.Errorf("%w: %s", ErrNoGrant, in.Replaces)
	}
	g := in.Grant
	g.Actor = old.Grant.Actor
	if err = errors.Join(err, err2); err == nil {
		err = issuable(tx, g)
	}
	if err != nil {
		return nil, nil, err
	}
	return replaceGrant(tx, input, confirmer.ID, &approval)
}

// replaceGrant revokes the scope's live grant and creates its replacement,
// with the lineage, all at the command's time.
func replaceGrant(tx *eventstore.Tx, input json.RawMessage, by types.ActorID, approval *types.ApprovalID) ([]eventstore.Event, json.RawMessage, error) {
	in, err := extendScopeOf(input)
	if err != nil {
		return nil, nil, err
	}
	old, live, err := liveGrant(tx, in.Replaces)
	if err == nil && !live {
		err = fmt.Errorf("%w: %s", ErrNoGrant, in.Replaces)
	}
	if err != nil {
		return nil, nil, err
	}
	g := in.Grant
	g.Actor, g.IssuedBy, g.Approval = old.Grant.Actor, by, approval
	g.ValidFrom = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
	revoked, err := RevokeGrant(tx, old.Grant.Actor, old.Grant.ID, tx.Now())
	created, err2 := CreateGrant(tx, g)
	lineage, err3 := event(GrantReplaced, string(old.Grant.ID), nil, types.GrantReplaced{Transition: "grant.replaced", Replaces: old.Grant.ID, Grant: g})
	if err := errors.Join(err, err2, err3); err != nil {
		return nil, nil, err
	}
	res, err := json.Marshal(map[string]any{"grant": g.ID, "replaces": old.Grant.ID, "valid_from": g.ValidFrom})
	return append(append(revoked, created...), lineage...), res, err
}

// revokedBy is grant id's record, which ev's command revoked.
func revokedBy(tx *eventstore.Tx, id types.GrantID, ev eventstore.Event) (types.Grant, error) {
	e, _, err := liveGrant(tx, id)
	by, err2 := byCommand(tx, types.GrantEventTransitionGrantRevoked, id, ev)
	if err = errors.Join(err, err2); err == nil && !by {
		err = fmt.Errorf("grant %s is not revoked by this command", id)
	}
	return e.Grant, err
}

// applyLineage admits a grant.replaced/v1 by a live operator: its old
// grant revoked and its new grant, exactly as recorded, created for the
// same grantee by the same command, under an approval of this replacement
// if any, once per grant.
func applyLineage(tx *eventstore.Tx, ev eventstore.Event) error {
	var l types.GrantReplaced
	if err := json.Unmarshal(ev.Payload, &l); err != nil || ev.Subject != string(l.Replaces) {
		return errors.Join(err, errors.New("a lineage is its old grant's"))
	}
	if by, err := liveActor(tx, types.ActorID(ev.Actor)); err != nil || by.Kind != types.ActorKindOperator {
		return errors.Join(err, errors.New("only a live operator records a lineage"))
	}
	old, err := revokedBy(tx, l.Replaces, ev)
	if err != nil {
		return err
	}
	cur, _, err := liveGrant(tx, l.Grant.ID)
	created, err2 := byCommand(tx, types.GrantEventTransitionGrantCreated, l.Grant.ID, ev)
	if err = errors.Join(err, err2); err != nil || !sameGrant(cur.Grant, l.Grant) || l.Grant.Actor != old.Actor || !created {
		return errors.Join(err, errors.New("its new grant is not created for the same grantee by this command"))
	}
	if l.Grant.Approval != nil {
		if a, err := approvalOf(tx, *l.Grant.Approval); err != nil || a.Action != GrantExtendAction || a.Object.ID != string(l.Replaces) {
			return errors.Join(err, errors.New("its approval is not of this replacement"))
		}
	}
	if _, done, err := tx.Get(GrantLineageProjection, string(l.Replaces)); err != nil || done {
		return errors.Join(err, errors.New("the grant is already replaced"))
	}
	return putJSON(tx, GrantLineageProjection, string(l.Replaces), l.Grant.ID)
}
