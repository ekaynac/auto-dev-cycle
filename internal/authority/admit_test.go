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

// admitWith admits task, decided before this command, under allowed: its
// candidate template, as AdmitTask builds it.
func (b *tasks) admitWith(p identity.Principal, task string, allowed []types.NodeKind) error {
	return b.as(p, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.AdmitTask(tx, types.TaskID(task), allowed)
	})
}

// raw records a raw event of kind on task, its payload built at the
// command's time, as the system actor: past the command's checks, for the
// fold to refuse.
func (b *tasks) raw(kind, task string, payload func(at types.Time) any) error {
	r, _ := b.task(task)
	project := string(r.Project)
	return b.as(b.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		pl, err := json.Marshal(payload(types.Time(tx.Now().Format(time.RFC3339Nano))))
		return []eventstore.Event{{ID: newID(b.t, "evt"), Kind: kind, Subject: task, Project: &project, Payload: pl}}, err
	})
}

// nodeOf is task's node of kind.
func (b *tasks) nodeOf(task string, kind types.NodeKind) types.NodeID {
	var id types.NodeID
	b.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.Nodes(tx)
		for _, n := range nodes {
			if n.Task == types.TaskID(task) && n.Node.Kind == kind {
				id = n.Node.ID
			}
		}
		return err
	})
	return id
}

func (b *tasks) done(task string) error {
	return b.as(b.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.DoneTask(tx, types.TaskID(task)) })
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
// held included, nothing else is, and its done is refused while a node of
// it is held (Opus #309 M2); once the hold clears, its task.done frees
// admission for the next. The tasks rebuild.
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
	// The admitted task's build held: it stays admitted, nothing else is,
	// and it cannot be done.
	build := b.nodeOf(first, types.NodeKindBuild)
	hold := types.HoldID(newID(t, "hld"))
	if err := b.as(b.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Raise(tx, build, types.NodeHold{ID: hold, Hold: "reviewer_unavailable", SignalInstance: "fix@1", Owner: types.OwnerSystem, Pages: []types.NodeHoldPagesItem{}}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	h = b.head()
	if _, ok := b.admitted(); ok || b.admitNext() != nil || b.head() != h {
		t.Fatalf("a second admission while %s is admitted and held", first)
	}
	if err := b.done(first); !errors.Is(err, authority.ErrNotEndable) {
		t.Errorf("a done while the build is held: %v", err)
	}
	if err := b.raw(authority.TaskDoneEvent, first, func(at types.Time) any {
		return types.TaskDone{Task: types.TaskID(first), Project: prjA, As: types.TaskDoneAsCandidate, At: at}
	}); err == nil || b.head() != h {
		t.Errorf("the fold admitted a raw done while the build is held")
	}
	if err := b.as(b.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Clear(tx, build, hold, "fix@1") }); err != nil {
		t.Fatal(err)
	}
	if err := b.done(first); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.task(first); r.State != authority.TaskDoneState || r.DoneAs != string(types.TaskDoneAsCandidate) || contract.Validate("records/task-done", b.events(1)[0].Payload) != nil {
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

// P22a-3 (I22 admission and fold; Opus #298's obligations; Opus #309 M4):
// the admitting command re-checks what decided it. A task chosen and then
// cancelled is refused stale_expected_state, as is one with no approval in
// force, an unknown one, and any task while another is admitted; a
// template the guard refuses records nothing; an operator's admission is
// refused. The fold refuses, from the log alone, with the slot free, each
// raw admission that fails one check alone: of a cancelled task, naming
// another approval, at another time, under another project; and, the slot
// held, a second admission, a done of a task not admitted, and a
// candidate's done naming a reason.
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
	h := b.head()
	stale := func(err error) bool { return errors.Is(err, eventstore.ErrStaleExpectedState) }
	noGate := slices.DeleteFunc(slices.Clone(graph.M1Kinds), func(k types.NodeKind) bool { return k == types.NodeKindGate })
	for name, c := range map[string]struct {
		task    string
		allowed []types.NodeKind
		check   func(error) bool
	}{
		"chosen, then cancelled":   {chosen, graph.M1Kinds, stale},
		"no approval in force":     {unapproved, graph.M1Kinds, stale},
		"an unknown task":          {newID(t, "tsk"), graph.M1Kinds, stale},
		"a gate outside the kinds": {ready, noGate, func(err error) bool { return err != nil && strings.Contains(err.Error(), "deferred surface") }},
	} {
		if err := b.admitWith(b.sys, c.task, c.allowed); !c.check(err) || b.head() != h {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := b.admitWith(b.op, ready, graph.M1Kinds); err == nil || !strings.Contains(err.Error(), "not the controller's system actor") {
		t.Errorf("an operator's admission: %v", err)
	}
	cancelled, _ := b.task(chosen)
	r, _ := b.task(ready)
	for name, err := range map[string]error{
		"F1 a cancelled task": b.raw(authority.TaskAdmittedEvent, chosen, func(at types.Time) any {
			return types.TaskAdmitted{Task: types.TaskID(chosen), Project: prjA, Approval: cancelled.Approval.ID, At: at}
		}),
		"F2 another approval": b.raw(authority.TaskAdmittedEvent, ready, func(at types.Time) any {
			return types.TaskAdmitted{Task: types.TaskID(ready), Project: prjA, Approval: types.ApprovalID(newID(t, "apr")), At: at}
		}),
		"F3 another time": b.raw(authority.TaskAdmittedEvent, ready, func(at types.Time) any {
			earlier, _ := time.Parse(time.RFC3339Nano, string(at))
			return types.TaskAdmitted{Task: types.TaskID(ready), Project: prjA, Approval: r.Approval.ID, At: types.Time(earlier.Add(-time.Second).Format(time.RFC3339Nano))}
		}),
		"F4 another project": b.raw(authority.TaskAdmittedEvent, ready, func(at types.Time) any {
			return types.TaskAdmitted{Task: types.TaskID(ready), Project: prjB, Approval: r.Approval.ID, At: at}
		}),
	} {
		if err == nil || b.head() != h {
			t.Errorf("the fold admitted %s, the slot free", name)
		}
	}
	if err := b.admitNext(); err != nil {
		t.Fatal(err)
	}
	another := b.newTask(b.op, prjB)
	h = b.head()
	if err := b.admitWith(b.sys, another, graph.M1Kinds); !stale(err) {
		t.Errorf("a second admission: %v", err)
	}
	second, _ := b.task(another)
	reason := types.TaskDoneReasonValueAuthorityRevoked
	for name, err := range map[string]error{
		"a second admission": b.raw(authority.TaskAdmittedEvent, another, func(at types.Time) any {
			return types.TaskAdmitted{Task: types.TaskID(another), Project: second.Project, Approval: second.Approval.ID, At: at}
		}),
		"a done not admitted": b.raw(authority.TaskDoneEvent, another, func(at types.Time) any {
			return types.TaskDone{Task: types.TaskID(another), Project: second.Project, As: types.TaskDoneAsCandidate, At: at}
		}),
		"a candidate's done naming a reason": b.raw(authority.TaskDoneEvent, ready, func(at types.Time) any {
			return types.TaskDone{Task: types.TaskID(ready), Project: prjA, As: types.TaskDoneAsCandidate, Reason: &reason, At: at}
		}),
	} {
		if err == nil || b.head() != h {
			t.Errorf("the fold admitted %s", name)
		}
	}
	b.rebuilds()
}

// P22a-3 (W12, W13; I22, I1 consumption): Tasks, the graph's task
// authority, refuses a node of an unrecorded, Ready, cancelled or done
// task (not admitted), recorded or run, and a run of an admitted task's
// node once its approval is revoked (authority_revoked), though its nodes
// may still be recorded; it takes an admitted task with its approval in
// force.
func TestClaimable(t *testing.T) {
	b := openCancels(t)
	finished, ready, cancelled := b.newTask(b.op, prjA), b.newTask(b.op, prjA), b.newTask(b.op, prjA)
	if _, _, err := b.cancel(b.op, "c", cancelled); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(b.admitNext(), b.done(finished), b.admitNext()); err != nil {
		t.Fatal(err)
	}
	waiting := b.newTask(b.op, prjB)
	check := func(task string) (recordable, claimable error) {
		b.view(func(tx *eventstore.Tx) error {
			recordable, claimable = authority.Tasks{}.Recordable(tx, types.TaskID(task)), authority.Tasks{}.Claimable(tx, types.TaskID(task))
			return nil
		})
		return recordable, claimable
	}
	notAdmitted := func(err error) bool { var e graph.ErrNotAdmitted; return errors.As(err, &e) }
	for name, task := range map[string]string{"in Ready": waiting, "cancelled": cancelled, "done": finished, "unrecorded": newID(t, "tsk")} {
		if r, c := check(task); !notAdmitted(r) || !notAdmitted(c) {
			t.Errorf("%s: %v, %v", name, r, c)
		}
	}
	if r, c := check(ready); r != nil || c != nil {
		t.Errorf("admitted: %v, %v", r, c)
	}
	if err := b.as(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { // its approvals are revoked with it
		return identity.RevokeActor(tx, b.op.Actor(), suiteNow())
	}); err != nil {
		t.Fatal(err)
	}
	var revoked graph.ErrRevalidated
	if r, c := check(ready); r != nil || !errors.As(c, &revoked) || revoked.Reason != vocab.ReasonAuthorityRevoked {
		t.Errorf("an admitted task, its approval revoked: %v, %v", r, c)
	}
}

// #226 entry 100 (Opus #309 M3): an admitted task whose approval is no
// longer in force ends abandoned. Before the revocation it cannot be; after
// it, a raw abandonment naming no reason is refused, and AbandonTask
// records task.done as abandoned, authority_revoked: the slot frees. A
// Ready task whose approval was revoked with it is never admitted: the
// decision skips it, the admitting command refuses it, and so does the
// fold, the slot free (F1b).
func TestAdmissionRevokedApproval(t *testing.T) {
	b := openCancels(t)
	admitted, waiting := b.newTask(b.op, prjA), b.newTask(b.op, prjA)
	if err := b.admitNext(); err != nil {
		t.Fatal(err)
	}
	abandon := func() error {
		return b.as(b.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return authority.AbandonTask(tx, types.TaskID(admitted))
		})
	}
	if err := abandon(); !errors.Is(err, authority.ErrNotEndable) {
		t.Errorf("abandoning a task whose approval is in force: %v", err)
	}
	if err := b.as(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, b.op.Actor(), suiteNow())
	}); err != nil {
		t.Fatal(err)
	}
	h := b.head()
	if err := b.raw(authority.TaskDoneEvent, admitted, func(at types.Time) any {
		return types.TaskDone{Task: types.TaskID(admitted), Project: prjA, As: types.TaskDoneAsAbandoned, At: at}
	}); err == nil || b.head() != h {
		t.Error("the fold admitted an abandonment naming no reason")
	}
	if err := abandon(); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.task(admitted); r.State != authority.TaskDoneState || r.DoneAs != string(types.TaskDoneAsAbandoned) ||
		contract.Validate("records/task-done", b.events(1)[0].Payload) != nil {
		t.Fatalf("the abandoned task %+v", r)
	}
	if next, ok := b.admitted(); ok {
		t.Errorf("the next admission is %s, whose approval is revoked", next)
	}
	h = b.head()
	if err := b.admitWith(b.sys, waiting, graph.M1Kinds); !errors.Is(err, eventstore.ErrStaleExpectedState) {
		t.Errorf("admitting a task whose approval is revoked: %v", err)
	}
	w, _ := b.task(waiting)
	if err := b.raw(authority.TaskAdmittedEvent, waiting, func(at types.Time) any {
		return types.TaskAdmitted{Task: types.TaskID(waiting), Project: prjA, Approval: w.Approval.ID, At: at}
	}); err == nil || b.head() != h {
		t.Error("the fold admitted a task whose approval is revoked, the slot free")
	}
	b.rebuilds()
}
