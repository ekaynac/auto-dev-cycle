// SPDX-License-Identifier: Apache-2.0

package authority

// Task cancel (backlog #226 entry 23; operator decision 2026-10-19):
// task.cancel ends a task still in Ready, terminally, with a reason.
//   - The operator cancels any task in Ready. A supervisor cancels only a
//     task it created that has no approval in force (operator_only
//     otherwise); any other actor kind is actor_kind_refused.
//   - An unknown task, or one not in Ready (an admitted task keeps the hold
//     and revoke path), is stale_expected_state; cancelling a cancelled
//     task again is applied with no new event.
//   - The fold sets the task's State to cancelled and removes its
//     ready/<project>/<task> key, so the Ready bound frees a place; a
//     cancelled task is never admitted. It refuses, from the log alone, a
//     cancel of an unknown task, of a task not in Ready, under another
//     subject or project, or by an actor the rules above refuse.
//   - Inert until the intake capability wires it, as task.create.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The cancel action, its scope schema, its event kind and the State of a
// cancelled task.
const (
	TaskCancelAction   = "task.cancel"
	TaskCancelScope    = "api/scope/task-cancel"
	TaskCancelled      = "task.cancelled/v1"
	TaskCancelledState = "cancelled"
)

// TaskCancelHandler derives task.cancel: the task is its object.
type TaskCancelHandler struct{}

func cancelScopeOf(raw json.RawMessage) (types.TaskCancelScope, error) {
	in, err := contract.Decode[types.TaskCancelScope](TaskCancelScope, raw)
	if err != nil {
		return in, fmt.Errorf("%w: %s input: %w", ErrInvalidScope, TaskCancelAction, err)
	}
	return in, nil
}

// mayCancel refuses by's cancel of t unless by is an operator, or the
// supervisor that created t with no approval in force.
func mayCancel(by types.Actor, t TaskRecord) error {
	switch by.Kind {
	case types.ActorKindOperator:
		return nil
	case types.ActorKindSupervisor:
		if t.CreatedBy != by.ID || t.Approval != nil && t.Approval.Status == ApprovalInForce {
			return refusal(vocab.ReasonOperatorOnly, "%s of task %s is the operator's: a supervisor cancels only its own unapproved task", TaskCancelAction, t.ID)
		}
		return nil
	}
	return refusal(vocab.ReasonActorKindRefused, "actor %s is a %s; only operators and supervisors cancel tasks", by.ID, by.Kind)
}

// Derive checks the scope, the task's existence and the actor's right to
// cancel it, and derives the task, in its project.
func (TaskCancelHandler) Derive(tx *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	if by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return Derived{}, mayCancel(by, TaskRecord{})
	}
	in, err := cancelScopeOf(input)
	if err != nil {
		return Derived{}, err
	}
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(in.Task))
	if err != nil || !ok {
		return Derived{}, errors.Join(err, fmt.Errorf("authority: %w: task %s is not recorded", eventstore.ErrStaleExpectedState, in.Task))
	}
	if err := mayCancel(by, t); err != nil {
		return Derived{}, err
	}
	return Derived{Object: string(in.Task), ObjectType: TaskObjectType, Project: &t.Project, Record: TaskRecordOf(in.Task)}, nil
}

// CancelTask is the logic for c, a command the Authorizer allowed: the
// cancel of a task in Ready; none again for a cancelled one.
func CancelTask(c eventstore.Command) eventstore.Decide {
	return eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		id, bound := tx.Bound()
		in, err := cancelScopeOf(c.Input)
		by, err2 := liveActor(tx, types.ActorID(c.Actor))
		if err = errors.Join(err, err2); err == nil && (c.Action != TaskCancelAction || !bound || id != string(in.Task)) {
			err = fmt.Errorf("authority: %s is no derived %s", c.Action, TaskCancelAction)
		}
		if err != nil {
			return nil, nil, err
		}
		t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(in.Task))
		if err != nil || !ok {
			return nil, nil, errors.Join(err, fmt.Errorf("authority: %w: task %s is not recorded", eventstore.ErrStaleExpectedState, in.Task))
		}
		res, err := json.Marshal(map[string]any{"task": t.ID, "state": TaskCancelledState})
		switch {
		case err != nil:
			return nil, nil, err
		case t.State == TaskCancelledState:
			return nil, res, nil
		case t.State != TaskReady:
			return nil, nil, fmt.Errorf("authority: %w: task %s is %s, not %s", eventstore.ErrStaleExpectedState, t.ID, t.State, TaskReady)
		}
		if err := mayCancel(by, t); err != nil {
			return nil, nil, err
		}
		x := types.TaskCancelled{Task: t.ID, Project: t.Project, Reason: in.Reason, By: by.ID, At: types.Time(tx.Now().Format(time.RFC3339Nano))}
		evs, err := event(TaskCancelled, t.ID, &t.Project, x)
		return evs, res, err
	})
}

// cancelTask admits ev, a cancel: of a recorded task in Ready, under its id
// and project, by its event's actor at its time, a live actor mayCancel
// allows. The task leaves the Ready bound.
func cancelTask(tx *eventstore.Tx, ev eventstore.Event) error {
	var x types.TaskCancelled
	if err := json.Unmarshal(ev.Payload, &x); err != nil {
		return err
	}
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(x.Task))
	if err != nil || !ok || ev.Subject != string(x.Task) || ev.Project == nil || *ev.Project != string(t.Project) || t.Project != x.Project ||
		string(x.By) != ev.Actor || x.At != types.Time(ev.At) || t.State != TaskReady {
		return errors.Join(err, errors.New("a cancel is of a recorded task in Ready, under its id and project, by its event's actor at its time"))
	}
	by, err := liveActor(tx, x.By)
	if err != nil {
		return err
	}
	if err := mayCancel(by, t); err != nil {
		return err
	}
	t.State = TaskCancelledState
	return errors.Join(putJSON(tx, TaskProjection, t.ID, t), tx.Delete(TaskProjection, "ready/"+string(t.Project)+"/"+t.ID))
}
