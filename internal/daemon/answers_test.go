// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// M1 P6 activates the answers capability: over a store holding an owed
// input (stored by P5's binary, an operator's answer to another), the
// installed `adc holds answer --expect 1` records the operator's answer;
// a second answer at that revision is refused stale_expected_state, and an
// answer at revision 0 too, each recording nothing.
func TestInputAnswerActive(t *testing.T) {
	var owed types.OwedInputID
	c := runCommanded(t, func(w *seeder, op types.ActorID) {
		node := heldNodeIn(t, w, op, graphProject)
		for i := range 2 {
			o := types.OwedInput{ID: types.OwedInputID(newID(t, "own")), Project: graphProject, RequestedBy: node, Owner: op,
				Kind: types.OwedInputKindRuling, Question: "q", Subject: types.ObjectRef{Type: "node", ID: string(node), Version: new(int64)}, Revision: 1}
			w.do(op, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.RecordOwed(o) })
			if owed = o.ID; i == 1 {
				break
			}
			w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				scope := json.RawMessage(`{"owed_input":"` + string(o.ID) + `","owed_revision":1,"answer":{"ruling":"yes"}}`)
				a := authority.AnswerApprovalOf(authority.OwedRecord{OwedInput: o}, types.Actor{ID: op, Kind: types.ActorKindOperator}, nil, scope,
					types.ApprovalID(newID(t, "apr")), types.Time(tx.Now().Format(time.RFC3339Nano)))
				return authority.ApprovalEvent(a)
			})
		}
	})
	answer := func(expect, revision string) (int, string) {
		scope := `{"owed_input":"` + string(owed) + `","owed_revision":` + revision + `,"answer":{"ruling":"no"}}`
		code, out, errb := c.adc(c.op.Reveal(), "holds", "answer", "--expect", expect, "--reason", "smoke", "--scope", scope)
		return code, out + errb
	}
	head := lastSeq(t, c.dir)
	if code, out := answer("1", "1"); code != entry.ExitOK || !strings.Contains(out, "applied") || lastSeq(t, c.dir) == head {
		t.Fatalf("holds answer: %d %s", code, out)
	}
	head = lastSeq(t, c.dir)
	for _, at := range []string{"1", "0"} {
		if code, out := answer("1", at); code != entry.ExitStale || !strings.Contains(out, "stale_expected_state") {
			t.Errorf("an answer at revision %s: %d %s", at, code, out)
		}
	}
	if lastSeq(t, c.dir) != head {
		t.Error("a refused answer appended to the log")
	}
	if _, ok := handlers()[authority.InputAnswerAction]; !ok {
		t.Error("input.answer has no handler in adcd")
	}
}

// Holds are read per project (#226 entry 13): a supervisor reading holds
// on project A sees A's node hold only, neither B's nor a lease hold (a
// lease records no project); the operator sees all three.
func TestHoldsReadProjectIsolation(t *testing.T) {
	const other = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sb2")
	var nodeA types.NodeID
	var reader identity.Secret
	c := runCommanded(t, func(w *seeder, op types.ActorID) {
		nodeA = heldNodeIn(t, w, op, graphProject)
		heldNodeIn(t, w, op, other)
		w.heldLease(op)
		now := time.Now().UTC()
		sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "holds reader", CreatedBy: op}
		g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{graphProject}},
			Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{"hold"}},
			ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
			ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op}
		w.do(op, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sup) })
		w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, g) })
		w.do(op, func(*eventstore.Tx) ([]eventstore.Event, error) {
			evs, s, err := identity.IssueToken(sup.ID, op, now)
			reader = s
			return evs, err
		})
	})
	read := func(tok identity.Secret) []types.HoldRead {
		code, out, errb := c.adc(tok.Reveal(), "holds", "list", "--json")
		var list struct{ Items []types.HoldRead }
		if code != entry.ExitOK || json.Unmarshal([]byte(out), &list) != nil {
			t.Fatalf("holds list: %d %s %s", code, out, errb)
		}
		return list.Items
	}
	if got := read(reader); len(got) != 1 || got[0].Subject != types.TypeID(nodeA) || *got[0].Project != graphProject {
		t.Errorf("the project A reader: %+v", got)
	}
	if got := read(c.op); len(got) != 3 {
		t.Errorf("the operator: %+v", got)
	}
}
