// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// openCancels is openTasks with task.cancel wired and the supervisor
// granted task.create and task.cancel on prjA.
func openCancels(t *testing.T) *tasks {
	b := openTasks(t)
	b.az.Handlers[authority.TaskCancelAction] = authority.TaskCancelHandler{}
	g := b.supGrant
	g.ID, g.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.TaskCreateAction, authority.TaskCancelAction}
	b.seed(b.op, createGrant(g))
	return b
}

// newTask creates a task on project as p and returns its id.
func (b *tasks) newTask(p identity.Principal, project types.ProjectID) string {
	b.t.Helper()
	key := newID(b.t, "cmd")
	out, _, err := b.create(p, key, spec(project, "t", "test.py", b.upload(p, key, suiteNow())))
	var res struct{ Task string }
	if err := errors.Join(err, json.Unmarshal(out.Result, &res)); err != nil {
		b.t.Fatal(err)
	}
	return res.Task
}

// cancel submits task.cancel of task as p under key; ran reports whether
// the logic ran.
func (b *tasks) cancel(p identity.Principal, key, task string) (out eventstore.Committed, ran bool, err error) {
	c := p.Command(newID(b.t, "cmd"), authority.TaskCancelAction, key, must(json.Marshal(map[string]string{"task": task, "reason": "no longer wanted"})), nil)
	decide := authority.CancelTask(c)
	out, err = b.s.Submit(context.Background(), c, func(tx *eventstore.Tx) (eventstore.Decision, error) {
		ran = true
		return decide(tx)
	})
	return out, ran, err
}

func (b *tasks) readyKey(task string) bool {
	b.t.Helper()
	var ok bool
	b.view(func(tx *eventstore.Tx) (err error) {
		_, ok, err = tx.Get(authority.TaskProjection, "ready/"+string(prjA)+"/"+task)
		return err
	})
	return ok
}

// An operator cancels a Ready task: one task.cancelled, the task cancelled
// and out of the Ready bound, so a full project takes another task; a
// second cancel is applied with no new event and the same result; the
// tasks rebuild.
func TestTaskCancel(t *testing.T) {
	b := openCancels(t)
	var ids []string
	for range authority.MaxReadyTasks {
		ids = append(ids, b.newTask(b.op, prjA))
	}
	out, ran, err := b.cancel(b.op, "c1", ids[0])
	if err != nil || !ran || out.Disposition != eventstore.DispositionApplied || out.LastSeq != out.FirstSeq {
		t.Fatalf("the cancel: %+v %v", out, err)
	}
	ev := b.events(1)[0]
	var x types.TaskCancelled
	if err := json.Unmarshal(ev.Payload, &x); err != nil || ev.Kind != authority.TaskCancelled || ev.Subject != ids[0] ||
		x.Reason != "no longer wanted" || x.By != b.op.Actor() || x.Project != prjA || contract.Validate("records/task-cancelled", ev.Payload) != nil {
		t.Fatalf("the event %+v %s: %v", ev, ev.Payload, err)
	}
	if r, ok := b.task(ids[0]); !ok || r.State != authority.TaskCancelledState || b.readyKey(ids[0]) {
		t.Fatalf("the cancelled task %+v (ready key kept: %t)", r, b.readyKey(ids[0]))
	}
	b.newTask(b.op, prjA) // a sixth task now fits
	h := b.head()
	again, ran, err := b.cancel(b.op, "c2", ids[0])
	if err != nil || !ran || again.Disposition != eventstore.DispositionApplied || b.head() != h || string(again.Result) != string(out.Result) {
		t.Fatalf("the second cancel: %+v %v (head %d, was %d)", again, err, b.head(), h)
	}
	b.rebuilds()
}

// A supervisor cancels its own unapproved task; it is refused
// (operator_only) on another's task and on its own task once an approval
// is in force, and the system is actor_kind_refused. An admitted or
// unknown task is stale_expected_state. None appends an event.
func TestTaskCancelRefusals(t *testing.T) {
	b := openCancels(t)
	mine, theirs := b.newTask(b.sup, prjA), b.newTask(b.op, prjA)
	if out, _, err := b.cancel(b.sup, "own", mine); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the supervisor's own task: %+v %v", out, err)
	}
	if r, _ := b.task(mine); r.State != authority.TaskCancelledState {
		t.Fatalf("the supervisor's task %+v", r)
	}
	approved := b.newTask(b.sup, prjA)
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { // its approval in force, as a recorded one
		return nil, approve(tx, approved, b.op.Actor())
	})
	admitted := b.newTask(b.op, prjA)
	if err := b.admitWith(b.sys, admitted, templateOf(t, admitted)); err != nil { // P22a-3's admission
		t.Fatal(err)
	}
	h := b.head()
	for name, c := range map[string]struct {
		p     identity.Principal
		task  string
		check func(error) bool
	}{
		"a supervisor on another's task":    {b.sup, theirs, isReason(vocab.ReasonOperatorOnly)},
		"a supervisor on its approved task": {b.sup, approved, isReason(vocab.ReasonOperatorOnly)},
		"the system":                        {b.sys, theirs, isReason(vocab.ReasonActorKindRefused)},
		"an admitted task":                  {b.op, admitted, func(err error) bool { return errors.Is(err, eventstore.ErrStaleExpectedState) }},
		"an unknown task":                   {b.op, newID(t, "tsk"), func(err error) bool { return errors.Is(err, eventstore.ErrStaleExpectedState) }},
		"a supervisor on an unknown task":   {b.sup, newID(t, "tsk"), func(err error) bool { return errors.Is(err, eventstore.ErrStaleExpectedState) }},
	} {
		if _, _, err := b.cancel(c.p, name, c.task); !c.check(err) || b.head() != h {
			t.Errorf("%s: %v", name, err)
		}
	}
	if r, _ := b.task(admitted); r.State != authority.TaskAdmitted {
		t.Errorf("the admitted task %+v", r)
	}
}

// approve marks task's approval in force by actor, as a recorded approval
// would (the fixture writes the projection directly).
func approve(tx *eventstore.Tx, task string, actor types.ActorID) error {
	return editTask(tx, task, func(r *authority.TaskRecord) {
		r.Approval = &authority.TaskApproval{ID: types.ApprovalID("apr_x"), Actor: actor, Digest: r.SpecDigest, Status: authority.ApprovalInForce}
	})
}

func editTask(tx *eventstore.Tx, task string, edit func(*authority.TaskRecord)) error {
	raw, _, err := tx.Get(authority.TaskProjection, task)
	var r authority.TaskRecord
	if err == nil {
		err = json.Unmarshal(raw, &r)
	}
	if err != nil {
		return err
	}
	edit(&r)
	return tx.Put(authority.TaskProjection, task, must(json.Marshal(r)))
}

// The fold refuses, from the log alone, a forged task.cancelled: of a task
// already cancelled, by a supervisor on an operator's approved task, under
// another subject, or by another actor than its event's; the right cancel
// is admitted (positive control).
func TestTaskCancelFoldRefusesForged(t *testing.T) {
	b := openCancels(t)
	approved, done := b.newTask(b.op, prjA), b.newTask(b.op, prjA)
	if _, _, err := b.cancel(b.op, "done", done); err != nil {
		t.Fatal(err)
	}
	// edit, when set, changes the forged payload or event (Opus #298 L2).
	forgeWith := func(task, subject string, by types.ActorID, edit func(*types.TaskCancelled, *eventstore.Event)) applyFn {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			p := string(prjA)
			x := types.TaskCancelled{Task: types.TaskID(task), Project: prjA, Reason: "forged", By: by, At: types.Time(tx.Now().Format(time.RFC3339Nano))}
			ev := eventstore.Event{ID: newID(t, "evt"), Kind: authority.TaskCancelled, Subject: subject, Project: &p}
			if edit != nil {
				edit(&x, &ev)
			}
			ev.Payload = must(json.Marshal(x))
			return []eventstore.Event{ev}, nil
		}
	}
	forge := func(task, subject string, by types.ActorID) applyFn { return forgeWith(task, subject, by, nil) }
	h := b.head()
	for name, c := range map[string]struct {
		p     identity.Principal
		apply applyFn
	}{
		"a cancelled task":                 {b.op, forge(done, done, b.op.Actor())},
		"a supervisor on an approved task": {b.sup, forge(approved, approved, b.sup.Actor())},
		"another subject":                  {b.op, forge(approved, done, b.op.Actor())},
		"another actor than the event's":   {b.sup, forge(approved, approved, b.op.Actor())},
		"an unknown task":                  {b.op, forge(newID(t, "tsk"), approved, b.op.Actor())},
		"another project in the payload":   {b.op, forgeWith(approved, approved, b.op.Actor(), func(x *types.TaskCancelled, _ *eventstore.Event) { x.Project = prjB })},
		"another project on the event": {b.op, forgeWith(approved, approved, b.op.Actor(), func(_ *types.TaskCancelled, ev *eventstore.Event) {
			p := string(prjB)
			ev.Project = &p
		})},
		"another time than the event's": {b.op, forgeWith(approved, approved, b.op.Actor(), func(x *types.TaskCancelled, _ *eventstore.Event) {
			x.At = types.Time(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano))
		})},
	} {
		if err := b.trySeed(c.p, c.apply); err == nil || b.head() != h {
			t.Errorf("%s: admitted: %v", name, err)
		}
	}
	b.seed(b.op, forge(approved, approved, b.op.Actor()))
	if r, _ := b.task(approved); r.State != authority.TaskCancelledState {
		t.Errorf("the operator's cancel: %+v", r)
	}
	b.rebuilds()
}

// task.cancelled/v1, as stored, must keep replaying: the frozen golden of
// the golden task's cancel is admitted and rebuilt.
func TestTaskCancelledGoldenReplays(t *testing.T) {
	task, err1 := os.ReadFile("testdata/task-created-v1.json")
	cancel, err2 := os.ReadFile("testdata/task-cancelled-v1.json")
	if err := errors.Join(err1, err2); err != nil {
		t.Fatal(err)
	}
	const id = "tsk_01m3250vkwffc979nzjw0gtak1"
	b := openCancels(t)
	for _, raw := range []string{string(task), string(cancel)} {
		kind := authority.TaskCreated
		if raw == string(cancel) {
			kind = authority.TaskCancelled
		}
		b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			fill := strings.NewReplacer("@ACTOR@", string(b.op.Actor()), "@AT@", tx.Now().Format(time.RFC3339Nano))
			p := string(prjA)
			return []eventstore.Event{{ID: newID(t, "evt"), Kind: kind, Subject: id, Project: &p, Payload: []byte(fill.Replace(raw))}}, nil
		})
	}
	if r, ok := b.task(id); !ok || r.State != authority.TaskCancelledState || b.readyKey(id) {
		t.Fatalf("the golden task %+v", r)
	}
	b.rebuilds()
}
