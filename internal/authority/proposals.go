// SPDX-License-Identifier: Apache-2.0

package authority

// Proposal and approval persistence (docs/design/m0/14 PR 6a; 04
// "Proposals and confirmation", "Approval records"). A command authority
// proposes commits proposal.created/v1 with the proposed disposition, its
// logic never run (Authorizer.Authorize). ProposalProjector folds the
// events and the projections alone, so a rebuild reaches the same records:
//   - at most one pending (open, unexpired) proposal per binding digest;
//   - a pending proposal is invalidated in the fold when its proposer's
//     actor is revoked (actor_revoked), its grant is revoked
//     (authority_revoked) or its bound object has a new event
//     (stale_expected_state);
//   - an approval answers exactly one live proposal, every field it shares
//     with it copied from the proposal record (ApprovalOf);
//   - a proposal moves open → confirmed → applied, or open → expired, one
//     event each (confirm.go), or open → withdrawn (withdraw.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	// ProposalProjection holds each proposal under its id, the approval
	// answering it under approval/<proposal>, and each approval id, once,
	// under approval_id/<approval> (naming its proposal).
	ProposalProjection = "proposals"
	// PendingProjection indexes the pending proposals only: digest/<binding
	// digest>, proposer/<actor>/<id>, grant/<grant>/<id> and
	// object/<subject>/<id>, each naming the proposal.
	PendingProjection = "proposals_pending"
	// ProposalTTL is how long a proposal waits for its confirmation.
	ProposalTTL = 72 * time.Hour

	ProposalCreated     = "proposal.created/v1"
	ProposalInvalidated = "proposal.invalidated/v1"
	ApprovalRecorded    = "approval.recorded/v1"
)

// ProposalKinds registers the proposal and approval event kinds.
func ProposalKinds() eventstore.Kinds {
	k := eventstore.Kinds{ApprovalRecorded: {Schema: "records/approval"}}
	for _, kind := range []string{ProposalCreated, ProposalInvalidated, ProposalConfirmed, ProposalApplied, ProposalExpired, ProposalWithdrawn} {
		k[kind] = eventstore.Kind{Schema: "records/proposal"}
	}
	return k
}

// propose commits dec, a decision to propose c's operation as d derived
// it: a new proposal, or the pending one for the same exact operation,
// returned again without an event.
func (a Authorizer) propose(t *eventstore.Tx, c eventstore.Command, actor types.Actor, d Derived, dec Decision) error {
	if d.ObjectType == "" {
		return fmt.Errorf("authority: the %s handler derived no object type; a proposal binds one", c.Action)
	}
	if c.Action == WithdrawAction {
		return refusal(vocab.ReasonActionNotProposable, "%s is never proposed; an operator withdraws directly", c.Action)
	}
	state, err := StateOf(t, d)
	if err != nil {
		return err
	}
	id, err := ids.New("prp")
	if err != nil {
		return err
	}
	now := t.Now()
	obj := types.ObjectRef{Type: types.Name(d.ObjectType), ID: d.Object, Version: state.Version, Digest: state.Digest}
	p, err := NewProposal(types.ProposalID(id), dec, Bound{Action: c.Action, Project: d.Project, Object: obj, ExpectedState: state,
		Scope: c.Input, ProposedBy: actor.ID, Grantee: d.Grantee}, now, now.Add(ProposalTTL))
	if err != nil {
		return err
	}
	var evs []eventstore.Event
	if cur, pending, err := pendingFor(t, p.BindingDigest, p.CreatedAt); err != nil {
		return err
	} else if pending {
		p = cur
	} else if evs, err = proposalEvent(ProposalCreated, p); err != nil {
		return err
	}
	result, err := json.Marshal(map[string]any{"proposal": p.ID, "policy": p.Policy, "binding_digest": p.BindingDigest,
		"effective_change": EffectiveChangeOf(p)})
	if err != nil {
		return err
	}
	return t.Decided(eventstore.Decision{Disposition: eventstore.DispositionProposed, Events: evs, Result: result})
}

// EffectiveChangeOf is what p binds, as authority derived it (08: the
// CLI prints a proposal's effective change, not the request): its action,
// change classes, project, object and grantee, copied from the record,
// never from the command. Like a proposal read, it omits the object's
// content digest.
func EffectiveChangeOf(p types.Proposal) types.EffectiveChange {
	return types.EffectiveChange{Action: p.Action, ChangeClass: p.ChangeClass, Project: p.Project, Grantee: p.Grantee,
		Object: types.EffectiveChangeObject{Type: p.Object.Type, ID: p.Object.ID, Version: p.Object.Version}}
}

// StateOf is the derived object's current state: its version (0 when its
// record does not exist, 14 P3) and, when it has a record, its digest.
func StateOf(t *eventstore.Tx, d Derived) (types.ExpectedState, error) {
	var zero int64
	s := types.ExpectedState{Version: &zero}
	if d.Record != nil {
		rec, exists, err := d.Record(t)
		if err != nil || !exists {
			return s, err
		}
		dg, err := eventstore.ContentDigest(rec)
		if err != nil {
			return s, err
		}
		s.Digest = (*types.Digest)(&dg)
	}
	v, err := d.version(t)
	s.Version = &v
	return s, err
}

// InvalidateProposal is the proposal.invalidated event for the open or
// confirmed proposal id, for a command that finds it no longer holds (at
// apply, Authorizer.Confirm) and commits it with the invalidated disposition.
func InvalidateProposal(t *eventstore.Tx, id types.ProposalID, reason types.ProposalInvalidatedReasonValue) ([]eventstore.Event, error) {
	p, ok, err := loadJSON[types.Proposal](t, ProposalProjection, string(id))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("authority: no proposal %s", id))
	}
	if p, err = Invalidate(p, reason); err != nil {
		return nil, err
	}
	return proposalEvent(ProposalInvalidated, p)
}

// ApprovalOf is a with every field it shares with p copied from p, and its
// approver by: nothing a client sends decides what is approved or under
// which policy.
func ApprovalOf(a types.Approval, p types.Proposal, by types.Actor) types.Approval {
	n := a
	n.Proposal, n.Actor, n.ActorKind = &p.ID, by.ID, types.ApprovalActorKind(by.Kind)
	n.Action, n.ChangeClass, n.Project, n.Object, n.ExpectedState = p.Action, p.ChangeClass, p.Project, p.Object, p.ExpectedState
	n.Scope, n.Grant, n.Confirmation.Policy = p.Scope, p.Grant, types.ApprovalConfirmationPolicy(p.Policy)
	return n
}

// ApprovalEvent records approval a.
func ApprovalEvent(a types.Approval) ([]eventstore.Event, error) {
	return event(ApprovalRecorded, string(a.ID), a.Project, a)
}

func proposalEvent(kind string, p types.Proposal) ([]eventstore.Event, error) {
	return event(kind, string(p.ID), p.Project, p)
}

func event(kind, subject string, project *types.ProjectID, payload any) ([]eventstore.Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	id, err := ids.New("evt")
	ev := eventstore.Event{ID: id, Kind: kind, Subject: subject, Payload: b}
	if project != nil {
		ev.Project = (*string)(project)
	}
	return []eventstore.Event{ev}, err
}

// pendingFor is the pending proposal with this binding digest, if one is
// open and unexpired at at.
func pendingFor(t *eventstore.Tx, digest types.Digest, at types.Time) (types.Proposal, bool, error) {
	id, ok, err := loadJSON[types.ProposalID](t, PendingProjection, "digest/"+string(digest))
	if err != nil || !ok {
		return types.Proposal{}, false, err
	}
	p, ok, err := loadJSON[types.Proposal](t, ProposalProjection, string(id))
	if err == nil && !ok {
		err = fmt.Errorf("authority: the pending proposal %s has no record", id)
	}
	return p, err == nil && p.Status == types.ProposalStatusOpen && later(string(p.ExpiresAt), string(at)), err
}

// ProposalProjector folds proposal and approval events and the fold rules
// above; other kinds pass through. It runs after the identity and grant
// projectors (eventstore.ProjectEach), so it sees their revocations.
type ProposalProjector struct{}

// Project is the eventstore.Hooks.Project fold for proposals.
func (ProposalProjector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		var err error
		switch ev.Kind {
		case ProposalCreated:
			err = createProposal(tx, ev)
		case ProposalInvalidated:
			err = closeProposal(tx, ev)
		case ApprovalRecorded:
			err = recordApproval(tx, ev)
		case ProposalConfirmed, ProposalApplied, ProposalExpired:
			err = advanceProposal(tx, ev)
		case ProposalWithdrawn:
			err = withdrawProposal(tx, ev)
		case string(types.GrantEventTransitionGrantRevoked) + "/v1":
			var g types.GrantEvent
			if err = json.Unmarshal(ev.Payload, &g); err == nil {
				err = invalidateBy(tx, "grant/"+string(g.Grant.ID)+"/", types.ProposalInvalidatedReasonValueAuthorityRevoked)
			}
		case identity.ActorRevoked + "/v1":
			var a types.IdentityEvent
			if err = json.Unmarshal(ev.Payload, &a); err == nil && a.Actor == nil {
				err = errors.New("an actor revocation without its actor")
			}
			if err == nil {
				err = invalidateBy(tx, "proposer/"+string(a.Actor.ID)+"/", types.ProposalInvalidatedReasonValueActorRevoked)
			}
		}
		if err == nil {
			err = invalidateBy(tx, "object/"+ev.Subject+"/", types.ProposalInvalidatedReasonValueStaleExpectedState)
		}
		if err == nil {
			err = invalidateMovedSeat(tx, ev)
		}
		if err != nil {
			return fmt.Errorf("authority: event %s: proposals: %w", ev.ID, err)
		}
	}
	return nil
}

// invalidateMovedSeat: an event of another subject that moved the seat's
// version (its holder's or requester's revocation, SeatVersion) makes the
// pending proposals bound to the seat stale, as a seat event does. It runs
// after the seat projector (eventstore.ProjectEach).
func invalidateMovedSeat(tx *eventstore.Tx, ev eventstore.Event) error {
	seat, ok, err := loadJSON[string](tx, SeatProjection, "id")
	if err != nil || !ok || ev.Subject == seat {
		return err
	}
	if v, err := SeatVersion(tx); err != nil || v != ev.Seq {
		return err
	}
	return invalidateBy(tx, "object/"+seat+"/", types.ProposalInvalidatedReasonValueStaleExpectedState)
}

func createProposal(tx *eventstore.Tx, ev eventstore.Event) error {
	var p types.Proposal
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if ev.Subject != string(p.ID) || p.Status != types.ProposalStatusOpen || p.ProposedBy != types.ActorID(ev.Actor) {
		return errors.New("a proposal is created open, as its subject, by its proposer")
	}
	if _, err := liveActor(tx, p.ProposedBy); err != nil {
		return err
	}
	if got, err := BindingDigest(boundOf(p)); err != nil || got != p.BindingDigest {
		return errors.Join(err, errors.New("the proposal does not match its own binding digest"))
	}
	if !later(string(p.ExpiresAt), string(p.CreatedAt)) || later(string(p.CreatedAt), ev.At) {
		return errors.New("the proposal expires before it is created, or is created after its event")
	}
	if _, exists, err := tx.Get(ProposalProjection, string(p.ID)); err != nil || exists {
		return errors.Join(err, errors.New("the proposal exists"))
	}
	// Liveness is judged at the event's recorded time, never at a time the
	// payload claims. (With created_at at or before the event, as checked
	// above, a proposal live at the event is live at created_at too: this is
	// the authoritative reading, the check above its guard.)
	if cur, pending, err := pendingFor(tx, p.BindingDigest, types.Time(ev.At)); err != nil || pending {
		return errors.Join(err, fmt.Errorf("the same operation is pending as %s", cur.ID))
	}
	if err := putJSON(tx, ProposalProjection, string(p.ID), p); err != nil {
		return err
	}
	for _, k := range pendingKeys(p) {
		if err := putJSON(tx, PendingProjection, k, p.ID); err != nil {
			return err
		}
	}
	return nil
}

// closeProposal admits a proposal.invalidated event: exactly the current
// record invalidated for its reason.
func closeProposal(tx *eventstore.Tx, ev eventstore.Event) error {
	var p types.Proposal
	if err := json.Unmarshal(ev.Payload, &p); err != nil || p.InvalidatedReason == nil || ev.Subject != string(p.ID) {
		return errors.Join(err, errors.New("an invalidation names its proposal and its reason"))
	}
	cur, ok, err := loadJSON[types.Proposal](tx, ProposalProjection, string(p.ID))
	if err != nil || !ok {
		return errors.Join(err, errors.New("no such proposal"))
	}
	want, err := Invalidate(cur, *p.InvalidatedReason)
	if err != nil || !sameCanonical(want, p) {
		return errors.Join(err, errors.New("the invalidation changes more than the proposal's state"))
	}
	return settle(tx, want)
}

// invalidateBy invalidates, for reason, every pending proposal indexed
// under prefix.
func invalidateBy(tx *eventstore.Tx, prefix string, reason types.ProposalInvalidatedReasonValue) error {
	keys, err := tx.Keys(PendingProjection, prefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		id := types.ProposalID(strings.TrimPrefix(k, prefix))
		if strings.Contains(string(id), "/") {
			continue // another subject that only starts like this one
		}
		p, ok, err := loadJSON[types.Proposal](tx, ProposalProjection, string(id))
		if err != nil || !ok {
			return errors.Join(err, fmt.Errorf("the pending proposal %s has no record", id))
		}
		n, err := Invalidate(p, reason)
		if err == nil {
			err = settle(tx, n)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// settle records a proposal that is no longer pending and drops its
// indexes (the digest one only while it still names this proposal).
func settle(tx *eventstore.Tx, p types.Proposal) error {
	if err := putJSON(tx, ProposalProjection, string(p.ID), p); err != nil {
		return err
	}
	for i, k := range pendingKeys(p) {
		if i == 0 {
			if id, _, err := loadJSON[types.ProposalID](tx, PendingProjection, k); err != nil || id != p.ID {
				continue
			}
		}
		if err := tx.Delete(PendingProjection, k); err != nil {
			return err
		}
	}
	return nil
}

// pendingKeys are p's index keys, its digest's first.
func pendingKeys(p types.Proposal) []string {
	keys := []string{"digest/" + string(p.BindingDigest), "proposer/" + string(p.ProposedBy) + "/" + string(p.ID),
		"object/" + p.Object.ID + "/" + string(p.ID)}
	if p.Grant != nil {
		keys = append(keys, "grant/"+string(*p.Grant)+"/"+string(p.ID))
	}
	return keys
}

// recordApproval admits an approval of one live proposal, by a live
// actor (an operator for a human policy), matching ApprovalOf exactly,
// and only the first one; one with no proposal only as an answer or a
// task's.
func recordApproval(tx *eventstore.Tx, ev eventstore.Event) error {
	var a types.Approval
	if err := json.Unmarshal(ev.Payload, &a); err != nil {
		return err
	}
	if ev.Subject != string(a.ID) || a.Actor != types.ActorID(ev.Actor) {
		return errors.New("an approval is its subject's, by its actor")
	}
	switch {
	case a.Proposal == nil && a.Action == InputAnswerAction: // an answer (answer.go)
		return recordAnswer(tx, ev, a)
	case a.Proposal == nil: // only a task's approval, in its creating command (task.go)
		return recordTaskApproval(tx, ev, a)
	}
	by, err := liveActor(tx, a.Actor)
	if err != nil {
		return err
	}
	p, ok, err := loadJSON[types.Proposal](tx, ProposalProjection, string(*a.Proposal))
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("no proposal %s", *a.Proposal)
	case p.Status != types.ProposalStatusOpen && p.Status != types.ProposalStatusConfirmed || !later(string(p.ExpiresAt), ev.At):
		return fmt.Errorf("proposal %s is %s or expired", p.ID, p.Status)
	case !sameCanonical(ApprovalOf(a, p, by), a):
		return fmt.Errorf("the approval does not match proposal %s", p.ID)
	}
	if err := Separate(p, by); err != nil {
		return err
	}
	key, id := "approval/"+string(p.ID), "approval_id/"+string(a.ID)
	if _, exists, err := tx.Get(ProposalProjection, key); err != nil || exists {
		return errors.Join(err, fmt.Errorf("proposal %s is already approved", p.ID))
	}
	if _, exists, err := tx.Get(ProposalProjection, id); err != nil || exists {
		return errors.Join(err, fmt.Errorf("approval %s is already recorded", a.ID))
	}
	if err := putJSON(tx, ProposalProjection, id, p.ID); err != nil {
		return err
	}
	return putJSON(tx, ProposalProjection, key, a)
}

// Separate is separation of duties between a proposal and its approver
// (02-supervisor-role:45, a supervisor never approves its own authority;
// 04:45 and :96, a grant change is never approved by its grantee; 04:190,
// a review is independent of the proposer; D37, a human confirmation is an
// operator's). The one approver who may be the proposer is an operator
// confirming its own human-policy proposal: the KA1 presence check
// (04:98), which adds a verified human, not authority (D31, 04:26).
func Separate(p types.Proposal, by types.Actor) error {
	ka1 := p.Policy == types.ProposalPolicyHuman && by.Kind == types.ActorKindOperator
	switch {
	case by.ID == p.ProposedBy && !ka1:
		return refusal(vocab.ReasonConfirmationRefused, "actor %s proposed %s and may not approve it", by.ID, p.ID)
	case p.Grantee != nil && by.ID == *p.Grantee:
		return refusal(vocab.ReasonConfirmationRefused, "actor %s is the grantee of %s and may not approve it", by.ID, p.ID)
	case p.Policy == types.ProposalPolicyHuman && by.Kind != types.ActorKindOperator:
		return refusal(vocab.ReasonConfirmationRefused, "only an operator approves %s under a human policy", p.ID)
	}
	return nil
}

func putJSON(tx *eventstore.Tx, projection, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Put(projection, key, b)
}

// sameCanonical: a and b have one canonical form.
func sameCanonical(a, b any) bool {
	digest := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		d, _ := eventstore.ContentDigest(raw)
		return d
	}
	x := digest(a)
	return x != "" && x == digest(b)
}
