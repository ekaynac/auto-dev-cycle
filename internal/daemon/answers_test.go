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

// M1 P5: adcd registers owed_input.recorded and projects owed inputs and
// their answers, but input.answer stays deferred until P6 activates the
// answers capability: a daemon over a store holding an owed input and its
// answer starts, and the installed `adc holds answer` is refused
// action_deferred, recording nothing.
func TestInputAnswerStaysDeferred(t *testing.T) {
	var owed types.OwedInputID
	c := runCommanded(t, func(w *seeder, op types.ActorID) {
		node := heldNodeIn(t, w, op, graphProject)
		o := types.OwedInput{ID: types.OwedInputID(newID(t, "own")), Project: graphProject, RequestedBy: node, Owner: op,
			Kind: types.OwedInputKindRuling, Question: "q", Subject: types.ObjectRef{Type: "node", ID: string(node), Version: new(int64)}, Revision: 1}
		owed = o.ID
		w.do(op, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.RecordOwed(o) })
		w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			scope := json.RawMessage(`{"owed_input":"` + string(o.ID) + `","owed_revision":1,"answer":{"ruling":"yes"}}`)
			a := authority.AnswerApprovalOf(authority.OwedRecord{OwedInput: o}, types.Actor{ID: op, Kind: types.ActorKindOperator}, nil, scope,
				types.ApprovalID(newID(t, "apr")), types.Time(tx.Now().Format(time.RFC3339Nano)))
			return authority.ApprovalEvent(a)
		})
	})
	head := lastSeq(t, c.dir)
	scope := `{"owed_input":"` + string(owed) + `","owed_revision":1,"answer":{"ruling":"no"}}`
	code, out, errb := c.adc(c.op.Reveal(), "holds", "answer", "--expect", "1", "--reason", "smoke", "--scope", scope)
	if code != entry.ExitCouldNotRun || !strings.Contains(out+errb, "action_deferred") {
		t.Fatalf("holds answer: %d %s %s", code, out, errb)
	}
	if lastSeq(t, c.dir) != head {
		t.Error("a deferred answer appended to the log")
	}
	if _, ok := handlers()[authority.InputAnswerAction]; ok {
		t.Error("input.answer has a handler in adcd")
	}
	kinds, err := eventKinds()
	if _, ok := kinds[authority.OwedInputRecorded]; err != nil || !ok {
		t.Errorf("owed_input.recorded is not registered: %v", err)
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
