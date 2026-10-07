// SPDX-License-Identifier: Apache-2.0

package authority

// Withdrawal (docs/design/m0/14 PR 6d; 04 "Proposals and confirmation").
// proposal.withdraw closes an open, unexpired proposal as withdrawn, by its
// proposer or an operator, and drops it from the pending indexes, so the
// same exact operation may be proposed afresh; past its expiry it is closed
// as expired (refused), as a confirmation closes it. Its object is the
// proposal, in the proposal's project, under a grant covering the action
// like any other, and who may withdraw is checked in the derivation, before
// Decide, and again in the logic. It is never proposed
// (action_not_proposable): a policy that would make it a proposal refuses
// it. It is a write but no supervisor write to the project: it changes no
// target, so it needs no seat (08 "Supervisor seat"; a former holder can
// retract what it can no longer apply, 14 PR 6c).

import (
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

// WithdrawAction withdraws an open proposal; WithdrawScope is its scope.
const (
	WithdrawAction    = "proposal.withdraw"
	WithdrawScope     = "api/scope/proposal-withdraw"
	ProposalWithdrawn = "proposal.withdrawn/v1"
)

// WithdrawHandler derives proposal.withdraw.
type WithdrawHandler struct{}

// Derive is the Handler derivation of proposal.withdraw.
func (WithdrawHandler) Derive(t *eventstore.Tx, actor types.Actor, input json.RawMessage) (Derived, error) {
	in, err := withdrawInputOf(input)
	if err != nil {
		return Derived{}, err
	}
	p, err := storedProposal(t, in.Proposal)
	if err == nil {
		err = mayWithdraw(p, actor)
	}
	return Derived{Object: string(p.ID), ObjectType: "proposal", Project: p.Project, Record: ProposalRecord(p.ID), SeatExempt: true}, err
}

// ProposalRecord is proposal id's record, which --expect is checked against.
func ProposalRecord(id types.ProposalID) func(*eventstore.Tx) (json.RawMessage, bool, error) {
	return func(t *eventstore.Tx) (json.RawMessage, bool, error) { return t.Get(ProposalProjection, string(id)) }
}

func withdrawInputOf(raw json.RawMessage) (types.ProposalWithdrawScope, error) {
	in, err := contract.Decode[types.ProposalWithdrawScope](WithdrawScope, raw)
	if err != nil {
		return in, fmt.Errorf("%w: %s input: %w", ErrInvalidScope, WithdrawAction, err)
	}
	return in, nil
}

// mayWithdraw: the proposer or an operator. A grant covering
// proposal.withdraw covers its grantee's own proposals, never another's.
func mayWithdraw(p types.Proposal, by types.Actor) error {
	if by.ID != p.ProposedBy && by.Kind != types.ActorKindOperator {
		return refusal(vocab.ReasonActionNotGranted, "actor %s neither proposed %s nor is an operator", by.ID, p.ID)
	}
	return nil
}

// Withdraw is proposal.withdraw's logic for c, a command the Authorizer
// allowed. It rechecks who withdraws (mayWithdraw), as its derivation did,
// so it is safe to call on its own. A refusal that leaves the proposal as
// it is commits nothing.
func (a Authorizer) Withdraw(c eventstore.Command) eventstore.Decide {
	return func(t *eventstore.Tx) (eventstore.Decision, error) {
		var none eventstore.Decision
		if c.Action != WithdrawAction {
			return none, fmt.Errorf("authority: %s is not %s", c.Action, WithdrawAction)
		}
		in, err := withdrawInputOf(c.Input)
		if err != nil {
			return none, err
		}
		by, err := identity.ActorOf(t, c)
		if err != nil {
			return none, err
		}
		p, err := storedProposal(t, in.Proposal)
		if err == nil {
			err = mayWithdraw(p, by)
		}
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
		n := p
		n.Status = types.ProposalStatusWithdrawn
		evs, err := proposalEvent(ProposalWithdrawn, n)
		res, err2 := json.Marshal(map[string]any{"proposal": p.ID, "status": n.Status})
		return eventstore.Decision{Disposition: eventstore.DispositionApplied, Events: evs, Result: res}, errors.Join(err, err2)
	}
}

// withdrawProposal admits a proposal.withdrawn event: exactly the current
// open record withdrawn, before its expiry, by its proposer or an operator.
func withdrawProposal(tx *eventstore.Tx, ev eventstore.Event) error {
	var p types.Proposal
	if err := json.Unmarshal(ev.Payload, &p); err != nil || ev.Subject != string(p.ID) {
		return errors.Join(err, errors.New("a withdrawal names its proposal"))
	}
	cur, ok, err := loadJSON[types.Proposal](tx, ProposalProjection, string(p.ID))
	if err != nil || !ok {
		return errors.Join(err, errors.New("no such proposal"))
	}
	by, err := liveActor(tx, types.ActorID(ev.Actor))
	if err == nil {
		err = mayWithdraw(cur, by)
	}
	if err != nil {
		return err
	}
	want := cur
	want.Status = types.ProposalStatusWithdrawn
	if cur.Status != types.ProposalStatusOpen || !later(string(cur.ExpiresAt), ev.At) || !sameCanonical(want, p) {
		return fmt.Errorf("proposal %s is not withdrawn from %s at %s", p.ID, cur.Status, ev.At)
	}
	return settle(tx, want)
}
