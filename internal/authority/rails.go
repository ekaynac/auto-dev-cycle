// SPDX-License-Identifier: Apache-2.0

package authority

// The rails' raise (M1 slice plan P17a, P17b; §6 L3): task.raise_rail lets
// an operator raise one of a task's rails for that task, a count rail (to)
// or per_task_usd (to_usd, held in micros), and clears the task's
// budget_rail holds on that rail in the same command.
//   - A task's caps in force (Rails, a graph.Rails) are its project
//     profile's budget.rails, a default for each one absent
//     (graph.CapsOf), each raised to the task's latest raise of it.
//   - The raise must be above the cap in force; the fold admits a raise
//     only of a recorded task, by a live operator, at its event's time and
//     above any earlier raise of that rail (raises never lower a rail).
//   - per_day_usd is the project's, raised by a profile change only.
//   - adcd wires the handler while launch, its capability, is active (this
//     build, P22a).

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

// Rails is a task's rails in force (a graph.Rails): its project
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
	}{{graph.RailAttempts, &caps.AttemptsPerRound}, {graph.RailRounds, &caps.RoundsPerTask}, {graph.RailMergeOps, &caps.MergeOperationsPerGeneration},
		{graph.RailTaskUSD, &caps.PerTaskMicros}} {
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

// raisedTo is a raise's cap, as Rails holds it: a count rail's to, or
// per_task_usd's to_usd in micros; each rail takes exactly its own field.
func raisedTo(rail types.RaisableRail, to *types.RailCap, toUSD *types.USD) (int64, error) {
	switch {
	case rail == types.RaisableRailPerTaskUsd && toUSD != nil && to == nil:
		return graph.USDMicros(*toUSD)
	case rail != types.RaisableRailPerTaskUsd && to != nil && toUSD == nil:
		return *to, nil
	}
	return 0, fmt.Errorf("authority: a raise of %s takes %s alone", rail, map[bool]string{true: "to_usd", false: "to"}[rail == types.RaisableRailPerTaskUsd])
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
		to, err := raisedTo(in.Rail, in.To, in.ToUsd)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrInvalidScope, err)
		}
		caps, err := Rails(tx, in.Task)
		if err != nil {
			return nil, nil, err
		}
		if cur, _ := caps.Of(string(in.Rail)); to <= cur {
			return nil, nil, fmt.Errorf("authority: %w: %s's %s is %d, not below %d", eventstore.ErrStaleExpectedState, in.Task, in.Rail, cur, to)
		}
		t, _, err := loadJSON[TaskRecord](tx, TaskProjection, string(in.Task))
		if err != nil {
			return nil, nil, err
		}
		r := types.RailRaised{Task: in.Task, Project: t.Project, Rail: in.Rail, To: in.To, ToUsd: in.ToUsd, By: types.ActorID(c.Actor),
			At: types.Time(tx.Now().Format(time.RFC3339Nano))}
		evs, err := event(TaskRailRaised, string(in.Task), &t.Project, r)
		cleared, err2 := graph.ClearRail(tx, in.Task, string(in.Rail))
		res, err3 := json.Marshal(map[string]any{"task": in.Task, "rail": in.Rail, "to": to, "cleared": len(cleared)})
		return append(evs, cleared...), res, errors.Join(err, err2, err3)
	})
}

// railRaised admits ev, a raise: of a recorded task, under its id and in
// its project, by a live operator, at its event's time, above the cap in
// force (Rails: the profile's, or an earlier raise of that rail), on append
// and on replay alike (Astra #295 L2).
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
	to, err := raisedTo(r.Rail, r.To, r.ToUsd)
	if err != nil {
		return err
	}
	caps, err := Rails(tx, r.Task)
	if err != nil {
		return err
	}
	if cur, ok := caps.Of(string(r.Rail)); !ok || to <= cur {
		return fmt.Errorf("a raise of %s is above the cap in force (%d)", r.Rail, cur)
	}
	return errors.Join(putJSON(tx, TaskProjection, railKey(r.Task, string(r.Rail)), to),
		putJSON(tx, RaiseProjection, fmt.Sprintf("%s/%020d", r.Task, ev.Seq), Raise{Task: r.Task, Rail: string(r.Rail), Command: ev.Command}))
}

// RaiseProjection: "<task>/<seq>" → each raise of a task's rails, in log
// order, with the command that made it (the benchmark's rail raises, §2.8;
// adc dev bench record, P25): a fold of the raise alone.
const RaiseProjection = "task_rail_raises"

// Raise is one raise of a task's rail and its command: the budget_rail
// holds that command cleared are graph.RailCleared's.
type Raise struct {
	Task    types.TaskID `json:"task"`
	Rail    string       `json:"rail"`
	Command *string      `json:"command"`
}

// Raises are task's raises, in log order; a row of another task's raise
// under task's key is damage (Opus #432 r2 H1′).
func Raises(tx *eventstore.Tx, task types.TaskID) ([]Raise, error) {
	keys, err := tx.Keys(RaiseProjection, string(task)+"/")
	out := []Raise{}
	for _, k := range keys {
		r, _, lerr := loadJSON[Raise](tx, RaiseProjection, k)
		if err = errors.Join(err, lerr); err != nil {
			return nil, err
		}
		if r.Task != task {
			return nil, fmt.Errorf("authority: task %s's raise index names a raise of %q: %s", task, r.Task, k)
		}
		out = append(out, r)
	}
	return out, err
}
