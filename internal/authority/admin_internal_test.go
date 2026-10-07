// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// The enrolment's logic records only a token minted for this enrolment:
// for the scope's actor, by the command's actor. (The matching token is
// recorded in admin_test.go, through the writer.)
func TestEnrolLogicTakesOnlyItsToken(t *testing.T) {
	const op, sup, other = types.ActorID("act_01m3250vkwffc979nzjw0g2sa1"), types.ActorID("act_01m3250vkwffc979nzjw0g2sb2"), types.ActorID("act_01m3250vkwffc979nzjw0g2sc3")
	in := types.AgentEnrolScope{Actor: sup, Name: "agent"}
	c := eventstore.Command{Actor: string(op)}
	for name, m := range map[string][2]types.ActorID{"for another actor": {other, op}, "by another issuer": {sup, other}} {
		issued, err := identity.Mint(m[0], m[1], time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := enrolLogic(c, in, issued)(nil); err == nil {
			t.Errorf("a token %s was recorded", name)
		}
	}
}
