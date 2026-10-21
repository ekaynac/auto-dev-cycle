// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// M1 P6 activates the answers capability. With the daemon running over a
// seeded store (owed inputs recorded by the test, one already answered by
// the operator), `adc holds answer`, run in-process (cli.Main) against the
// daemon's socket:
//   - records the operator's answer at --expect 1; a second answer at that
//     revision, and one at revision 0, are refused stale_expected_state;
//   - records the answer of a supervisor that owns the owed input, under a
//     grant covering input.answer, seated;
//   - refuses action_not_granted a supervisor answering an owed input it
//     does not own, and one naming an unknown id (no existence oracle);
//
// each refusal recording nothing.
func TestInputAnswerActive(t *testing.T) {
	var opOwed, supOwed types.OwedInputID
	var sup identity.Secret
	c := runCommanded(t, func(w *seeder, op types.ActorID) {
		node := heldNodeIn(t, w, op, graphProject)
		now := time.Now().UTC()
		s := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "answerer", CreatedBy: op}
		g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: s.ID, Projects: types.ProjectScope{All: true},
			Actions: []types.ActionKind{"seat.take", authority.InputAnswerAction}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{}},
			ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
			ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op}
		w.do(op, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(s) })
		w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, g) })
		w.do(op, func(*eventstore.Tx) ([]eventstore.Event, error) {
			evs, tok, err := identity.IssueToken(s.ID, op, now)
			sup = tok
			return evs, err
		})
		owe := func(owner types.ActorID) types.OwedInput {
			o := types.OwedInput{ID: types.OwedInputID(newID(t, "own")), Project: graphProject, RequestedBy: node, Owner: owner,
				Kind: types.OwedInputKindRuling, Question: "q", Subject: types.ObjectRef{Type: "node", ID: string(node), Version: new(int64)}, Revision: 1}
			w.do(op, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.RecordOwed(o) })
			return o
		}
		answered := owe(op)
		w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			scope := json.RawMessage(`{"owed_input":"` + string(answered.ID) + `","owed_revision":1,"answer":{"ruling":"yes"}}`)
			a := authority.AnswerApprovalOf(authority.OwedRecord{OwedInput: answered}, types.Actor{ID: op, Kind: types.ActorKindOperator}, nil, scope,
				types.ApprovalID(newID(t, "apr")), types.Time(tx.Now().Format(time.RFC3339Nano)))
			return authority.ApprovalEvent(a)
		})
		opOwed, supOwed = owe(op).ID, owe(s.ID).ID
	})
	answer := func(tok identity.Secret, owed types.OwedInputID, revision string) (int, string) {
		scope := `{"owed_input":"` + string(owed) + `","owed_revision":` + revision + `,"answer":{"ruling":"no"}}`
		code, out, errb := c.adc(tok.Reveal(), "holds", "answer", "--expect", "1", "--reason", "smoke", "--scope", scope)
		return code, out + errb
	}
	head := lastSeq(t, c.dir)
	if code, out := answer(c.op, opOwed, "1"); code != entry.ExitOK || !strings.Contains(out, "applied") || lastSeq(t, c.dir) == head {
		t.Fatalf("the operator's answer: %d %s", code, out)
	}
	head = lastSeq(t, c.dir)
	for _, at := range []string{"1", "0"} {
		if code, out := answer(c.op, opOwed, at); code != entry.ExitStale || !strings.Contains(out, "stale_expected_state") {
			t.Errorf("an answer at revision %s: %d %s", at, code, out)
		}
	}
	if code, out, errb := c.adc(sup.Reveal(), "seat", "take", "--expect", "0", "--reason", "r"); code != entry.ExitOK {
		t.Fatalf("seat take: %d %s %s", code, out, errb)
	}
	head = lastSeq(t, c.dir)
	for name, owed := range map[string]types.OwedInputID{"the operator's owed input": opOwed, "an unknown id": types.OwedInputID(newID(t, "own"))} {
		if code, out := answer(sup, owed, "1"); code != entry.ExitRefused || !strings.Contains(out, "action_not_granted") {
			t.Errorf("a supervisor answering %s: %d %s", name, code, out)
		}
	}
	if lastSeq(t, c.dir) != head {
		t.Error("a refused answer appended to the log")
	}
	if code, out := answer(sup, supOwed, "1"); code != entry.ExitOK || !strings.Contains(out, "applied") || lastSeq(t, c.dir) == head {
		t.Errorf("the owning supervisor's answer: %d %s", code, out)
	}
}

// askedSuccessor records, through w, an operator, a plan node, an owed
// ruling it asks (never answered), and its successor declaring that answer;
// it returns the operator and the successor.
func askedSuccessor(t *testing.T, w *seeder) (types.ActorID, types.NodeID) {
	t.Helper()
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys.ID}
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(op) })
	w.sys, w.op = sys.ID, op.ID
	_, task := admittedTask(t, w, sys.ID) // a node is recorded only for an admitted task (P22a-3)
	asker := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	w.do(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Record(types.NodeRecorded{Task: task, Project: graphProject, Node: asker})
	})
	o := types.OwedInput{ID: types.OwedInputID(newID(t, "own")), Project: graphProject, RequestedBy: asker.ID, Owner: op.ID,
		Kind: types.OwedInputKindRuling, Question: "q", Subject: types.ObjectRef{Type: "node", ID: string(asker.ID), Version: new(int64)}, Revision: 1}
	w.do(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.RecordOwed(o) })
	var next types.NodeID
	w.do(op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		ask := types.InputSpec{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: o.ID, Revision: 1}}}
		evs, err := graph.Successor(tx, asker.ID, []types.InputSpec{ask}, authority.Answers{})
		if len(evs) > 0 {
			next = types.NodeID(evs[len(evs)-1].Subject)
		}
		return evs, err
	})
	return op.ID, next
}

// rawClaim claims next on an approval that answers nothing.
func rawClaim(t *testing.T, next types.NodeID) func(*eventstore.Tx) ([]eventstore.Event, error) {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, next, map[string]graph.Resolved{"ruling": {Approval: types.ApprovalID(newID(t, "apr")), Revision: 1}}, 0)
	}
}

// adcd folds with the checked graph fold (M1 P6, review H1): through the
// daemon's own projector, a raw claim on an answer never given is refused
// authority_revoked on append; and a log a raw writer (M0's fold) wrote
// with that claim is refused at start, its replay refused for the same
// reason. Wiring graph.ProjectM0 into the daemon turns both red.
func TestDaemonFoldChecksAnswers(t *testing.T) {
	w := seed(t, stateDir(t), daemonProjection(t))
	op, next := askedSuccessor(t, w)
	cmd := newID(t, "cmd")
	_, err := w.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: string(op), Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)},
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := rawClaim(t, next)(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	if err == nil || !strings.Contains(err.Error(), "authority_revoked") {
		t.Errorf("a raw claim through adcd's projector: %v", err)
	}
	w.done()

	dir := stateDir(t)
	raw := seed(t, dir, func(lease.Blobs, map[string]types.Hold) func(*eventstore.Tx, []eventstore.Event) error {
		return eventstore.ProjectEach(identity.Projector{}.Project, authority.ProposalProjector{}.Project, authority.ProjectProjector{}.Project,
			authority.TaskProjector{}.Project, authority.OwedProjector{}.Project, graph.ProjectM0) // the task admitted, as adcd folds it
	})
	op, next = askedSuccessor(t, raw)
	raw.do(op, rawClaim(t, next))
	raw.done()
	_, err = Start(context.Background(), Config{StateDir: dir})
	var se *StartError
	if !errors.As(err, &se) || se.Reason != vocab.ReasonAmbiguousState || !strings.Contains(fmt.Sprint(se.cause), "authority_revoked") {
		t.Errorf("a start over the raw claim: %v", err)
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
		nodeA = plannedIn(t, w, graphProject)
		nodeB := plannedIn(t, w, other) // A done, B admitted: then both held
		raiseOn(t, w, op, nodeA)
		raiseOn(t, w, op, nodeB)
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
