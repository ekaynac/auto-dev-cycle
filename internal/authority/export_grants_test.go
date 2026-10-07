// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// ApplyHuman is the path a human confirmation takes to apply proposal id
// once the dashboard's WebAuthn is deployed (14 R6), for tests only: the
// command's actor confirms, its approval naming the challenge and the event
// that consumed it, and the operation is revalidated and applied as
// Confirm's is. The assertion itself is not verified here. The confirmer
// is an operator: Separate refuses any other under a human policy, as
// Revalidate does again and the approval fold (recordApproval) a third time.
func (a Authorizer) ApplyHuman(c eventstore.Command, id types.ProposalID, challenge types.ChallengeID, consumed types.EventID) eventstore.Decide {
	return func(t *eventstore.Tx) (eventstore.Decision, error) {
		by, err := identity.ActorOf(t, c)
		if err != nil {
			return eventstore.Decision{}, err
		}
		p, err := storedProposal(t, id)
		if err == nil {
			err = open(p, t.Now())
		}
		if err == nil {
			err = Separate(p, by)
		}
		if err != nil {
			return eventstore.Decision{}, err
		}
		conf := types.ApprovalConfirmation{Challenge: &challenge, SatisfiedBy: []types.EventID{consumed}}
		return a.apply(t, confirmed(p), by, conf, "a human confirmed it", t.Now())
	}
}
