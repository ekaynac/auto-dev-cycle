// SPDX-License-Identifier: Apache-2.0

package authority

// Confirmation and application (docs/design/m0/14 PR 6c; 04 "Proposals and
// confirmation"). proposal.confirm confirms an open independent_review
// proposal with a review receipt from an authoritative source (Reviews,
// 04:193; never a client-supplied GO, 14 H2 item 5), records the
// confirmer's approval server side (ApprovalOf), and in the same writer
// transaction re-decides the exact bound operation as of now (Revalidate)
// and applies it through its action's handler (Applier), the seat guard
// included. One command commits one of:
//   - applied: approval.recorded, proposal.confirmed, the operation's
//     events, proposal.applied;
//   - invalidated: proposal.invalidated, for a registered reason, nothing
//     applied;
//   - refused: proposal.expired, when it is confirmed past its expiry.
//
// Confirmation and application are one transaction, so a confirmed
// proposal is never left waiting: a confirmer revoked, or a policy
// tightened, before the confirm is met by the confirm itself (the writer's
// identity recheck, Revalidate). Human confirmation is refused until the
// dashboard's WebAuthn is deployed (14 R6), the KA1 presence check with it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	// ConfirmAction confirms a proposal and applies it.
	ConfirmAction = "proposal.confirm"

	ProposalConfirmed = "proposal.confirmed/v1"
	ProposalApplied   = "proposal.applied/v1"
	ProposalExpired   = "proposal.expired/v1"

	// HumanConfirmationDeferred is the fixed detail of a refused human
	// confirmation (confirmation_refused).
	HumanConfirmationDeferred = "a human confirmation is a WebAuthn assertion on the dashboard, which this build does not deploy (14 R6)"
	// NoReviewSource is the fixed detail of a confirmation refused for want
	// of an authoritative source of review receipts.
	NoReviewSource = "this build has no authoritative source of review receipts"
)

// Applier is a Handler whose action a confirmed proposal applies: Apply
// is the action's logic, run in the writer transaction once authority
// allowed it. Two actors meet here. Apply receives the proposer, whose
// operation it is; the command is the confirmer's. Apply leaves each
// event's Actor unset, and the writer stamps it with the command's actor,
// the confirmer (it refuses any other). Attribution to the proposer
// belongs in the payload or comes through the proposal (proposal.applied
// names it). A projector that reads the event's actor as who did it needs
// tests of the confirmed path when its action is wired as an Applier.
// Return a *Refusal only when the operation can no longer be applied: it
// invalidates the proposal. A plain error is a transient failure, which
// aborts the command and leaves the proposal open.
type Applier interface {
	Handler
	Apply(t *eventstore.Tx, actor types.Actor, input json.RawMessage) ([]eventstore.Event, json.RawMessage, error)
}

// ConfirmedApplier is an Applier whose operation records its confirmation:
// ApplyConfirmed also receives the confirmer and the id of the approval the
// same command records (a grant names its issuer and its approval, 04
// "Grants"). A handler implements exactly one of Applier and
// ConfirmedApplier; apply refuses one that implements both. Its refusals
// and errors mean what Apply's do.
type ConfirmedApplier interface {
	Handler
	ApplyConfirmed(t *eventstore.Tx, proposer, confirmer types.Actor, approval types.ApprovalID, input json.RawMessage) ([]eventstore.Event, json.RawMessage, error)
}

// Review is a review receipt as an authoritative source holds it: its
// instrument's contract and blobs, the model families it must be
// independent of (the proposer's and every contributor's, from recorded
// provenance; an unknown one as "unknown"), and the event that recorded it.
type Review struct {
	Receipt  types.Receipt
	Contract types.InstrumentContract
	Blobs    receipt.Blobs
	Excluded []string
	Event    types.EventID
}

// Reviews finds review receipt id for proposal p, in the writer
// transaction; an error refuses the confirmation.
type Reviews interface {
	Review(t *eventstore.Tx, id types.ReceiptID, p types.Proposal) (Review, error)
}

// ConfirmHandler derives proposal.confirm: its object is the proposal, in
// the proposal's project. A supervisor confirms under a grant for it and
// holding the seat, like any supervisor write.
type ConfirmHandler struct{}

// Derive is the Handler derivation of proposal.confirm.
func (ConfirmHandler) Derive(t *eventstore.Tx, _ types.Actor, input json.RawMessage) (Derived, error) {
	in, err := confirmInputOf(input)
	if err != nil {
		return Derived{}, err
	}
	p, err := storedProposal(t, in.Proposal)
	return Derived{Object: string(p.ID), ObjectType: "proposal", Project: p.Project, Record: ProposalRecord(p.ID)}, err
}

type confirmInput struct {
	Proposal types.ProposalID `json:"proposal"`
	Review   types.ReceiptID  `json:"review"`
}

func confirmInputOf(raw json.RawMessage) (confirmInput, error) {
	var in confirmInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: %s input: %w", ErrInvalidScope, ConfirmAction, err)
	}
	if err := errors.Join(ids.Check(string(in.Proposal), "prp"), ids.Check(string(in.Review), "rcp")); err != nil {
		return in, fmt.Errorf("%w: %w", ErrInvalidScope, err)
	}
	return in, nil
}

func storedProposal(t *eventstore.Tx, id types.ProposalID) (types.Proposal, error) {
	p, ok, err := loadJSON[types.Proposal](t, ProposalProjection, string(id))
	if err == nil && !ok {
		err = refusal(vocab.ReasonProposalNotOpen, "no proposal %s", id)
	}
	return p, err
}

// Confirm is proposal.confirm's logic for c, a command the Authorizer
// allowed: the request layer submits it with c. Refusals that leave the
// proposal as it is (not open, a human policy, not separate, no applicable
// review, the seat, the handler) are errors and commit nothing.
func (a Authorizer) Confirm(c eventstore.Command) eventstore.Decide {
	return func(t *eventstore.Tx) (eventstore.Decision, error) {
		var none eventstore.Decision
		if c.Action != ConfirmAction {
			return none, fmt.Errorf("authority: %s is not %s", c.Action, ConfirmAction)
		}
		by, err := identity.ActorOf(t, c)
		if err != nil {
			return none, err
		}
		in, err := confirmInputOf(c.Input)
		if err != nil {
			return none, err
		}
		p, err := storedProposal(t, in.Proposal)
		if err != nil {
			return none, err
		}
		now := t.Now()
		if until, err := time.Parse(time.RFC3339Nano, string(p.ExpiresAt)); p.Status == types.ProposalStatusOpen && err == nil && !now.Before(until) {
			return closed(ProposalExpired, p, types.ProposalStatusExpired, vocab.ReasonProposalExpired, "proposal "+string(p.ID)+" expired at "+string(p.ExpiresAt))
		}
		if err := open(p, now); err != nil {
			return none, err
		}
		if p.Policy == types.ProposalPolicyHuman {
			return none, refusal(vocab.ReasonConfirmationRefused, "%s", HumanConfirmationDeferred)
		}
		if err := Separate(p, by); err != nil {
			return none, err
		}
		if a.Reviews == nil {
			return none, refusal(vocab.ReasonConfirmationRefused, "%s", NoReviewSource)
		}
		rv, err := a.Reviews.Review(t, in.Review, p)
		if err != nil {
			return none, err
		}
		if p, err = ConfirmByReview(p, rv.Receipt, rv.Contract, rv.Blobs, rv.Excluded, now); err != nil {
			return none, err
		}
		return a.apply(t, p, by, types.ApprovalConfirmation{SatisfiedBy: []types.EventID{rv.Event}}, "an independent review confirmed it", now)
	}
}

// apply re-decides the confirmed proposal p's operation for its proposer
// as of now and applies it, or invalidates p; by's approval records conf
// (its policy is p's) and reason.
func (a Authorizer) apply(t *eventstore.Tx, p types.Proposal, by types.Actor, conf types.ApprovalConfirmation, reason string, now time.Time) (eventstore.Decision, error) {
	var none eventstore.Decision
	proposer, err := liveActor(t, p.ProposedBy)
	if err != nil {
		return invalidated(t, p, types.ProposalInvalidatedReasonValueActorRevoked, err.Error())
	}
	h := a.Handlers[string(p.Action)]
	plain, ok := h.(Applier)
	named, ok2 := h.(ConfirmedApplier)
	if ok == ok2 || isNil(h) { // neither, or both: an Applier implements exactly one
		return none, fmt.Errorf("%w: %s", ErrActionNotWired, p.Action)
	}
	d, err := h.Derive(t, proposer, p.Scope)
	if errors.Is(err, ErrInvalidScope) { // the stored scope, not the confirmer's
		return invalidated(t, p, types.ProposalInvalidatedReasonValueProposalInvalidated, "the proposal's scope no longer satisfies its action")
	}
	if err != nil {
		return none, err
	}
	state, err := StateOf(t, d)
	if err != nil {
		return none, err
	}
	grants, revoked, err := GrantsOf(t, proposer.ID)
	if err != nil {
		return none, err
	}
	req := Request{Actor: proposer, Action: string(p.Action), Project: d.Project, Classes: d.Classes, Grants: grants, Revoked: revoked, Grantee: d.Grantee, Now: now}
	op := Bound{Object: types.ObjectRef{Type: types.Name(d.ObjectType), ID: d.Object, Version: state.Version, Digest: state.Digest}, Scope: p.Scope}
	if err := Revalidate(a.Registry, p, &by, req, op, state); err != nil {
		var r *Refusal
		if !errors.As(err, &r) {
			return none, err
		}
		reason := types.ProposalInvalidatedReasonValueProposalInvalidated
		if r.Reason == vocab.ReasonStaleExpectedState {
			reason = types.ProposalInvalidatedReasonValueStaleExpectedState
		}
		return invalidated(t, p, reason, r.Detail)
	}
	// The operation is the proposer's supervisor write, as of now.
	if proposer.Kind == types.ActorKindSupervisor && !d.SeatExempt {
		if err := holdsSeat(t, proposer.ID); err != nil {
			return none, err
		}
	}
	apr, err := ids.New("apr")
	if err != nil {
		return none, err
	}
	var effects []eventstore.Event
	var result json.RawMessage
	var r *Refusal
	if ok2 {
		effects, result, err = named.ApplyConfirmed(t, proposer, by, types.ApprovalID(apr), p.Scope)
	} else {
		effects, result, err = plain.Apply(t, proposer, p.Scope)
	}
	if errors.As(err, &r) { // the operation cannot be applied as of now
		return invalidated(t, p, types.ProposalInvalidatedReasonValueProposalInvalidated, r.Error())
	}
	if err != nil {
		return none, err
	}
	at := types.Time(now.UTC().Format(time.RFC3339Nano))
	approval := ApprovalOf(types.Approval{ID: types.ApprovalID(apr), Confirmation: conf,
		Answers: []types.OwedInputID{}, Supersedes: []types.ApprovalID{}, Reason: reason, At: at}, p, by)
	done := p
	done.Status = types.ProposalStatusApplied
	var evs []eventstore.Event
	for _, step := range []func() ([]eventstore.Event, error){
		func() ([]eventstore.Event, error) { return ApprovalEvent(approval) },
		func() ([]eventstore.Event, error) { return proposalEvent(ProposalConfirmed, p) },
		func() ([]eventstore.Event, error) { return effects, nil },
		func() ([]eventstore.Event, error) { return proposalEvent(ProposalApplied, done) },
	} {
		e, err := step()
		if err != nil {
			return none, err
		}
		evs = append(evs, e...)
	}
	res, err := json.Marshal(map[string]any{"proposal": p.ID, "approval": approval.ID, "result": result})
	return eventstore.Decision{Disposition: eventstore.DispositionApplied, Events: evs, Result: res}, err
}

// invalidated commits p's invalidation for reason; nothing is applied.
func invalidated(t *eventstore.Tx, p types.Proposal, reason types.ProposalInvalidatedReasonValue, detail string) (eventstore.Decision, error) {
	evs, err := InvalidateProposal(t, p.ID, reason)
	if err != nil {
		return eventstore.Decision{}, err
	}
	return failed(eventstore.DispositionInvalidated, evs, string(reason), detail)
}

// closed commits p closed as to (expired), refused for reason.
func closed(kind string, p types.Proposal, to types.ProposalStatus, reason vocab.Reason, detail string) (eventstore.Decision, error) {
	n, err := Close(p, to)
	if err != nil {
		return eventstore.Decision{}, err
	}
	evs, err := proposalEvent(kind, n)
	if err != nil {
		return eventstore.Decision{}, err
	}
	return failed(eventstore.DispositionRefused, evs, reason.String(), detail)
}

func failed(d eventstore.Disposition, evs []eventstore.Event, reason, detail string) (eventstore.Decision, error) {
	res, err := json.Marshal(map[string]string{"outcome": "failed", "reason": reason, "detail": detail})
	return eventstore.Decision{Disposition: d, Events: evs, Result: res}, err
}

// advanceProposal admits a proposal's confirmation, application or expiry:
// exactly the current record moved one step, confirmed only with its
// approval recorded and before its expiry, expired only at or after it.
func advanceProposal(tx *eventstore.Tx, ev eventstore.Event) error {
	var p types.Proposal
	if err := json.Unmarshal(ev.Payload, &p); err != nil || ev.Subject != string(p.ID) {
		return errors.Join(err, errors.New("a proposal's transition names its proposal"))
	}
	cur, ok, err := loadJSON[types.Proposal](tx, ProposalProjection, string(p.ID))
	if err != nil || !ok {
		return errors.Join(err, errors.New("no such proposal"))
	}
	from, to, timely := types.ProposalStatusOpen, types.ProposalStatusConfirmed, later(string(cur.ExpiresAt), ev.At)
	switch ev.Kind {
	case ProposalApplied:
		from, to, timely = types.ProposalStatusConfirmed, types.ProposalStatusApplied, true
	case ProposalExpired:
		to, timely = types.ProposalStatusExpired, !timely
	case ProposalConfirmed:
		_, approved, err := tx.Get(ProposalProjection, "approval/"+string(p.ID))
		if err != nil || !approved {
			return errors.Join(err, fmt.Errorf("proposal %s is confirmed without its approval", p.ID))
		}
	}
	want := cur
	want.Status = to
	if cur.Status != from || !timely || !sameCanonical(want, p) {
		return fmt.Errorf("proposal %s does not move from %s to %s at %s", p.ID, cur.Status, to, ev.At)
	}
	return settle(tx, want)
}
