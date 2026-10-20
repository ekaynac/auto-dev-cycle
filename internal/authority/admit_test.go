// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// as submits what apply returns as p's command, with no Authorizer (the
// controller's own commands, and raw events the fold must refuse).
func (b *tasks) as(p identity.Principal, apply func(*eventstore.Tx) ([]eventstore.Event, error)) error {
	b.t.Helper()
	cmd := newID(b.t, "cmd")
	_, err := b.s.SubmitApplied(context.Background(), p.Command(cmd, "test.seed", cmd, []byte(`{}`), nil), eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := apply(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

// admitNext is the controller's admitting command: the next task, if any,
// and its candidate template.
func (b *tasks) admitNext() error {
	return b.as(b.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.Admit(tx, graph.M1Kinds) })
}

// admitWith admits task, decided before this command, with template.
func (b *tasks) admitWith(p identity.Principal, task string, template func(types.ProjectID) []types.NodeRecorded) error {
	return b.as(p, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r, _, err := tx.Get(authority.TaskProjection, task)
		var t authority.TaskRecord
		if err == nil && r != nil {
			err = json.Unmarshal(r, &t)
		}
		if err != nil {
			return nil, err
		}
		return authority.AdmitTask(tx, types.TaskID(task), template(t.Project), graph.M1Kinds)
	})
}

// templateOf is task's candidate template.
func templateOf(t *testing.T, task string) func(types.ProjectID) []types.NodeRecorded {
	return func(p types.ProjectID) []types.NodeRecorded {
		n, err := graph.Candidate(types.TaskID(task), p)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
}

func (b *tasks) admitted() (types.TaskID, bool) {
	b.t.Helper()
	var id types.TaskID
	var ok bool
	b.view(func(tx *eventstore.Tx) (err error) {
		id, ok, err = authority.NextAdmission(tx)
		return err
	})
	return id, ok
}

// P22a-3 (W11, W12; I22): the controller admits the oldest Ready task with
// its approval in force (an older unapproved task waits): task.admitted,
// then its candidate template (plan, build, accept, gate, no merge, no
// review while review is inactive), in one command. While it is admitted,
// held included, nothing else is; its task.done frees admission for the
// next. The tasks rebuild.
func TestAdmission(t *testing.T) {
	b := openCancels(t)
	unapproved := b.newTask(b.sup, prjA)
	first, second := b.newTask(b.op, prjA), b.newTask(b.op, prjB)
	if next, ok := b.admitted(); !ok || string(next) != first {
		t.Fatalf("the next admission: %s %t, want %s", next, ok, first)
	}
	h := b.head()
	if err := b.admitNext(); err != nil {
		t.Fatal(err)
	}
	evs := b.events(int(b.head() - h))
	kinds := []string{}
	for _, ev := range evs[1:] {
		var g struct{ Node types.Node }
		if err := json.Unmarshal(ev.Payload, &g); err != nil || ev.Kind != graph.NodeRecorded {
			t.Fatalf("a template event %s: %v", ev.Kind, err)
		}
		kinds = append(kinds, string(g.Node.Kind))
	}
	var x types.TaskAdmitted
	if err := json.Unmarshal(evs[0].Payload, &x); err != nil || evs[0].Kind != authority.TaskAdmittedEvent || string(x.Task) != first ||
		contract.Validate("records/task-admitted", evs[0].Payload) != nil || !slices.Equal(kinds, []string{"plan", "build", "accept", "gate"}) {
		t.Fatalf("the admitting command: %s %s, then %v: %v", evs[0].Kind, evs[0].Payload, kinds, err)
	}
	if r, _ := b.task(first); r.State != authority.TaskAdmitted || x.Approval != r.Approval.ID || b.readyKey(first) {
		t.Fatalf("the admitted task %+v", r)
	}
	// The admitted task's build held: it stays admitted, and nothing else is.
	var build types.NodeID
	b.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.Nodes(tx)
		for _, n := range nodes {
			if n.Task == types.TaskID(first) && n.Node.Kind == types.NodeKindBuild {
				build = n.Node.ID
			}
		}
		return err
	})
	hold, _ := ids.New("hld")
	if err := b.as(b.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Raise(tx, build, types.NodeHold{ID: types.HoldID(hold), Hold: "reviewer_unavailable", SignalInstance: "fix@1", Owner: types.OwnerSystem, Pages: []types.NodeHoldPagesItem{}}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	h = b.head()
	if _, ok := b.admitted(); ok || b.admitNext() != nil || b.head() != h {
		t.Fatalf("a second admission while %s is admitted and held", first)
	}
	if err := b.as(b.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.DoneTask(tx, types.TaskID(first))
	}); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.task(first); r.State != authority.TaskDoneState || contract.Validate("records/task-done", b.events(1)[0].Payload) != nil {
		t.Fatalf("the done task %+v", r)
	}
	if err := b.admitNext(); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{second: authority.TaskAdmitted, unapproved: authority.TaskReady} {
		if r, _ := b.task(id); r.State != want {
			t.Errorf("%s: %s, want %s", id, r.State, want)
		}
	}
	b.rebuilds()
}

// P22a-3 (I22 admission and fold; Opus #298's obligations): the admitting
// command re-checks what decided it. A task chosen and then cancelled is
// refused stale_expected_state, as is a cancelled task, one with no
// approval in force, and any task while another is admitted; a template
// the guard refuses (an evidence node, a merge node while merge is
// inactive) or a node of another task records nothing. The fold refuses,
// from the log alone, an admission by an operator, a second one, one of a
// cancelled task, and a done of a task not admitted.
func TestAdmissionRefusals(t *testing.T) {
	b := openCancels(t)
	chosen := b.newTask(b.op, prjA)
	if next, ok := b.admitted(); !ok || string(next) != chosen {
		t.Fatalf("the next admission: %s", next)
	}
	if _, _, err := b.cancel(b.op, "c", chosen); err != nil { // between the decision and the admitting command
		t.Fatal(err)
	}
	unapproved, ready := b.newTask(b.sup, prjA), b.newTask(b.op, prjA)
	kind := func(k types.NodeKind) func(types.ProjectID) []types.NodeRecorded {
		return func(p types.ProjectID) []types.NodeRecorded {
			n := templateOf(t, ready)(p)
			n[3].Node.Kind = k
			return n
		}
	}
	other := func(p types.ProjectID) []types.NodeRecorded {
		n := templateOf(t, ready)(p)
		n[2].Task = types.TaskID(newID(t, "tsk"))
		return n
	}
	h := b.head()
	stale := func(err error) bool { return errors.Is(err, eventstore.ErrStaleExpectedState) }
	for name, c := range map[string]struct {
		task     string
		template func(types.ProjectID) []types.NodeRecorded
		check    func(error) bool
	}{
		"chosen, then cancelled": {chosen, templateOf(t, chosen), stale},
		"no approval in force":   {unapproved, templateOf(t, unapproved), stale},
		"an evidence node":       {ready, kind(types.NodeKindEvidence), func(err error) bool { return err != nil && strings.Contains(err.Error(), "deferred surface") }},
		"a merge node":           {ready, kind(types.NodeKindMerge), func(err error) bool { return err != nil && strings.Contains(err.Error(), "deferred surface") }},
		"a node of another task": {ready, other, func(err error) bool { return err != nil && strings.Contains(err.Error(), "not of") }},
		"an unknown task":        {newID(t, "tsk"), templateOf(t, ready), stale},
	} {
		if err := b.admitWith(b.sys, c.task, c.template); !c.check(err) || b.head() != h {
			t.Errorf("%s: %v", name, err)
		}
	}
	if next, ok := b.admitted(); !ok || string(next) != ready {
		t.Fatalf("after the refusals the next is %s, want %s", next, ready)
	}
	if err := b.admitWith(b.op, ready, templateOf(t, ready)); err == nil || !strings.Contains(err.Error(), "not the controller's system actor") {
		t.Errorf("an operator's admission: %v", err)
	}
	if err := b.admitNext(); err != nil {
		t.Fatal(err)
	}
	another := b.newTask(b.op, prjB)
	h = b.head()
	if err := b.admitWith(b.sys, another, templateOf(t, another)); !stale(err) {
		t.Errorf("a second admission: %v", err)
	}
	// Raw events, past the command's checks: the fold refuses them.
	// Raw events, past the command's checks, each at its command's time:
	// the fold refuses them.
	raw := func(kind, task string, payload func(at types.Time) any) error {
		r, _ := b.task(task)
		project := string(r.Project)
		return b.as(b.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			pl, err := json.Marshal(payload(types.Time(tx.Now().Format(time.RFC3339Nano))))
			return []eventstore.Event{{ID: newID(t, "evt"), Kind: kind, Subject: task, Project: &project, Payload: pl}}, err
		})
	}
	second, _ := b.task(another)
	cancelledApproval := types.ApprovalID(newID(t, "apr"))
	if r, _ := b.task(chosen); r.Approval != nil {
		cancelledApproval = r.Approval.ID
	}
	for name, err := range map[string]error{
		"a second admission": raw(authority.TaskAdmittedEvent, another, func(at types.Time) any {
			return types.TaskAdmitted{Task: types.TaskID(another), Project: second.Project, Approval: second.Approval.ID, At: at}
		}),
		"a cancelled task": raw(authority.TaskAdmittedEvent, chosen, func(at types.Time) any {
			return types.TaskAdmitted{Task: types.TaskID(chosen), Project: prjA, Approval: cancelledApproval, At: at}
		}),
		"a done not admitted": raw(authority.TaskDoneEvent, another, func(at types.Time) any {
			return types.TaskDone{Task: types.TaskID(another), Project: second.Project, As: types.TaskDoneAsCandidate, At: at}
		}),
	} {
		if err == nil || b.head() != h {
			t.Errorf("the fold admitted %s", name)
		}
	}
	b.rebuilds()
}

// P22a-3 (W12, W13; I22, I1 consumption): Tasks, the graph's claim
// authority, refuses a node of an unrecorded, Ready, cancelled or done
// task (not admitted), and of an admitted task whose approval was revoked
// (authority_revoked); it takes an admitted task with its approval in
// force.
func TestClaimable(t *testing.T) {
	b := openCancels(t)
	ready, cancelled := b.newTask(b.op, prjA), b.newTask(b.op, prjA)
	if _, _, err := b.cancel(b.op, "c", cancelled); err != nil {
		t.Fatal(err)
	}
	if err := b.admitNext(); err != nil {
		t.Fatal(err)
	}
	waiting := b.newTask(b.op, prjB)
	claimable := func(task string) error {
		var err error
		b.view(func(tx *eventstore.Tx) error { err = authority.Tasks{}.Claimable(tx, types.TaskID(task)); return nil })
		return err
	}
	notAdmitted := func(err error) bool { var e graph.ErrNotAdmitted; return errors.As(err, &e) }
	for name, c := range map[string]struct {
		task  string
		check func(error) bool
	}{
		"admitted":   {ready, func(err error) bool { return err == nil }},
		"in Ready":   {waiting, notAdmitted},
		"cancelled":  {cancelled, notAdmitted},
		"unrecorded": {newID(t, "tsk"), notAdmitted},
	} {
		if err := claimable(c.task); !c.check(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := b.as(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { // its approvals are revoked with it
		return identity.RevokeActor(tx, b.op.Actor(), suiteNow())
	}); err != nil {
		t.Fatal(err)
	}
	var revoked graph.ErrRevalidated
	if err := claimable(ready); !errors.As(err, &revoked) || revoked.Reason != vocab.ReasonAuthorityRevoked {
		t.Errorf("an admitted task, its approval revoked: %v", err)
	}
}

// A task whose approval was revoked (its operator revoked) is never
// admitted: the decision skips it, and the admitting command refuses it
// stale_expected_state, as the fold does.
func TestAdmissionRevokedApproval(t *testing.T) {
	b := openCancels(t)
	task := b.newTask(b.op, prjA)
	if err := b.as(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, b.op.Actor(), suiteNow())
	}); err != nil {
		t.Fatal(err)
	}
	if next, ok := b.admitted(); ok {
		t.Errorf("the next admission is %s, whose approval is revoked", next)
	}
	if err := b.admitWith(b.sys, task, templateOf(t, task)); !errors.Is(err, eventstore.ErrStaleExpectedState) {
		t.Errorf("admitting a task whose approval is revoked: %v", err)
	}
}
