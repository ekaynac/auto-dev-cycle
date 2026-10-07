// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// staleScope is applyingNotes whose scope schema no longer accepts the
// scope it once did.
type staleScope struct{ *applyingNotes }

func (h staleScope) Derive(*eventstore.Tx, types.Actor, json.RawMessage) (authority.Derived, error) {
	return authority.Derived{}, fmt.Errorf("%w: changed", authority.ErrInvalidScope)
}

// A stored proposal whose scope no longer derives is invalidated at apply,
// not answered as the confirmer's own invalid scope; nothing is applied.
func TestConfirmStaleStoredScope(t *testing.T) {
	k := openConfirm(t)
	p := k.propose(k.op.Actor())
	rcp := k.review(p, "claude")
	k.az.Handlers["note.add"] = staleScope{k.notes}
	o := k.committed(k.confirmCmd(k.sys, p.ID, rcp), eventstore.DispositionInvalidated)
	if o.Reason != vocab.ReasonProposalInvalidated.String() || k.notes.applied != 0 || k.proposal(p.ID).Status != types.ProposalStatusInvalidated {
		t.Fatalf("%+v, applied %d, %s", o, k.notes.applied, k.proposal(p.ID).Status)
	}
}
