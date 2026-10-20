// SPDX-License-Identifier: Apache-2.0

package authority

// The count rails' raise (M1 slice plan P17a; §6 L3): task.raise_rail lets
// an operator raise one of a task's count rails for that task, and clears
// the task's budget_rail holds on that rail in the same command.
//   - A task's caps in force (Rails, a graph.Rails) are its project
//     profile's budget.rails, a default for each one absent
//     (graph.CapsOf), each raised to the task's latest raise of it.
//   - The raise must be above the cap in force; the fold admits a raise
//     only of a recorded task, by a live operator, at its event's time and
//     above any earlier raise of that rail (raises never lower a rail).
//   - per_day_usd is the project's, raised by a profile change only; the
//     USD rails are P17b's.
//   - Inert until P22a: the handler is built and deferred, as task.create.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
)

// The raise action, its scope schema and its event kind.
const (
	TaskRaiseRailAction = "task.raise_rail"
	TaskRaiseRailScope  = "api/scope/task-raise-rail"
	TaskRailRaised      = "task.rail_raised/v1"
)

// railKey is a task's raise of rail in TaskProjection.
func railKey(task types.TaskID, rail string) string { return "rails/" + string(task) + "/" + rail }

// Rails is a task's count rails in force (a graph.Rails): its project
// profile's, raised by task.raise_rail.
func Rails(tx *eventstore.Tx, task types.TaskID) (graph.Caps, error) {
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(task))
	if err != nil || !ok {
		return graph.Caps{}, errors.Join(err, fmt.Errorf("authority: task %s has no record", task))
	}
	p, ok, err := loadJSON[ProjectRecord](tx, ProjectProjection, string(t.Project))
	var profile types.ProjectProfile
	if err == nil && ok {
		err = json.Unmarshal(p.Profile, &profile)
	}
	if err != nil || !ok {
		return graph.Caps{}, errors.Join(err, fmt.Errorf("authority: project %s has no record", t.Project))
	}
	caps := graph.CapsOf(profile.Budget.Rails)
	for _, f := range []struct {
		rail string
		to   *int64
	}{{graph.RailAttempts, &caps.AttemptsPerRound}, {graph.RailRounds, &caps.RoundsPerTask}, {graph.RailMergeOps, &caps.MergeOperationsPerGeneration}} {
		raised, ok, err := loadJSON[int64](tx, TaskProjection, railKey(task, f.rail))
		if err != nil {
			return graph.Caps{}, err
		}
		if ok {
			*f.to = max(*f.to, raised)
		}
	}
	return caps, nil
}

// TaskRaiseRailHandler derives task.raise_rail: the task is its object.
type TaskRaiseRailHandler struct{}

func raiseScopeOf(raw json.RawMessage) (types.TaskRaiseRailScope, error) {
	in, err := contract.Decode[types.TaskRaiseRailScope](TaskRaiseRailScope, raw)
	if err != nil {
		return in, fmt.Errorf("%w: %s input: %w", ErrInvalidScope, TaskRaiseRailAction, err)
	}
	return in, nil
}

// Derive checks the scope and the actor (an operator's), and derives the
// task, in its project.
func (TaskRaiseRailHandler) Derive(tx *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	in, err := raiseScopeOf(input)
	if err != nil {
		return Derived{}, err
	}
	if err := operatorOnly(by, TaskRaiseRailAction, "only the operator raises a rail"); err != nil {
		return Derived{}, err
	}
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(in.Task))
	if err != nil || !ok {
		return Derived{}, errors.Join(err, fmt.Errorf("%w: task %s is not recorded", eventstore.ErrStaleExpectedState, in.Task))
	}
	return Derived{Object: string(in.Task), ObjectType: TaskObjectType, Project: &t.Project, Record: TaskRecordOf(in.Task)}, nil
}

// Logic is an allowed task.raise_rail's logic: the raise, above the cap in
// force, then the task's holds on the rail cleared.
func (TaskRaiseRailHandler) Logic(c eventstore.Command) eventstore.Decide {
	return eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		in, err := raiseScopeOf(c.Input)
		if id, bound := tx.Bound(); err == nil && (!bound || id != string(in.Task)) {
			err = errors.New("authority: a rail's raise runs only on its derived task")
		}
		if err != nil {
			return nil, nil, err
		}
		caps, err := Rails(tx, in.Task)
		if err != nil {
			return nil, nil, err
		}
		if cur, _ := caps.Of(string(in.Rail)); int64(in.To) <= cur {
			return nil, nil, fmt.Errorf("authority: %w: %s's %s is %d, not below %d", eventstore.ErrStaleExpectedState, in.Task, in.Rail, cur, in.To)
		}
		t, _, err := loadJSON[TaskRecord](tx, TaskProjection, string(in.Task))
		if err != nil {
			return nil, nil, err
		}
		r := types.RailRaised{Task: in.Task, Project: t.Project, Rail: in.Rail, To: in.To, By: types.ActorID(c.Actor),
			At: types.Time(tx.Now().Format(time.RFC3339Nano))}
		evs, err := event(TaskRailRaised, string(in.Task), &t.Project, r)
		cleared, err2 := graph.ClearRail(tx, in.Task, string(in.Rail))
		res, err3 := json.Marshal(map[string]any{"task": in.Task, "rail": in.Rail, "to": in.To, "cleared": len(cleared)})
		return append(evs, cleared...), res, errors.Join(err, err2, err3)
	})
}

// railRaised admits ev, a raise: of a recorded task, under its id and in
// its project, by a live operator, at its event's time, above any earlier
// raise of that rail.
func railRaised(tx *eventstore.Tx, ev eventstore.Event) error {
	var r types.RailRaised
	if err := json.Unmarshal(ev.Payload, &r); err != nil {
		return err
	}
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(r.Task))
	if err != nil || !ok || ev.Subject != string(r.Task) || ev.Project == nil || *ev.Project != string(t.Project) || t.Project != r.Project ||
		string(r.By) != ev.Actor || r.At != types.Time(ev.At) {
		return errors.Join(err, errors.New("a raise is of a recorded task, under its id and project, by its event's actor at its time"))
	}
	by, err := liveActor(tx, r.By)
	if err != nil || by.Kind != types.ActorKindOperator {
		return errors.Join(err, errors.New("a live operator raises a rail"))
	}
	was, raised, err := loadJSON[int64](tx, TaskProjection, railKey(r.Task, string(r.Rail)))
	if err != nil || raised && int64(r.To) <= was {
		return errors.Join(err, fmt.Errorf("a raise of %s is above its earlier one (%d)", r.Rail, was))
	}
	return putJSON(tx, TaskProjection, railKey(r.Task, string(r.Rail)), int64(r.To))
}
