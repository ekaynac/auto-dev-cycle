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
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// raise submits task.raise_rail by p with its logic.
func (b *tasks) raise(p identity.Principal, task, rail string, to int) error {
	in := must(json.Marshal(map[string]any{"task": task, "rail": rail, "to": to}))
	c := p.Command(newID(b.t, "cmd"), authority.TaskRaiseRailAction, newID(b.t, "cmd"), in, nil)
	_, err := b.s.Submit(context.Background(), c, authority.TaskRaiseRailHandler{}.Logic(c))
	return err
}

// raisedEvent is a raw task.rail_raised of task's rail to to, by by, in prjA.
func raisedEvent(t *testing.T, task, rail string, to int, by types.ActorID) func(*eventstore.Tx) ([]eventstore.Event, error) {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r := types.RailRaised{Task: types.TaskID(task), Project: prjA, Rail: types.RaisableRail(rail), To: &[]types.RailCap{types.RailCap(to)}[0], By: by, At: types.Time(tx.Now().Format(time.RFC3339Nano))}
		p := string(prjA)
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.TaskRailRaised, Subject: task, Project: &p, Payload: must(json.Marshal(r))}}, nil
	}
}

// rails is task's caps in force.
func (b *tasks) rails(task string) (c graph.Caps) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) (err error) { c, err = authority.Rails(tx, types.TaskID(task)); return err })
	return c
}

// TestTaskRaiseRail (P17a): a task's caps are its profile's (the fixture
// names 3/5/2); an operator raises one for that task only, above the cap
// in force; a raise not above it, a supervisor's, an unknown rail and an
// unknown task are refused; the fold refuses a raise not above the last.
func TestTaskRaiseRail(t *testing.T) {
	b := openTasks(t)
	b.az.Handlers[authority.TaskRaiseRailAction] = authority.TaskRaiseRailHandler{}
	one, two := b.newTask(b.op, prjA), b.newTask(b.op, prjA)
	if c := b.rails(one); c != (graph.Caps{AttemptsPerRound: 2, RoundsPerTask: 3, MergeOperationsPerGeneration: 2, PerTaskMicros: 5_000_000, PerDayMicros: 20_000_000}) { // py-mini's, not the defaults
		t.Fatalf("the profile's caps: %+v", c)
	}
	if err := b.raise(b.op, one, graph.RailRounds, 7); err != nil {
		t.Fatal(err)
	}
	if c := b.rails(one); c.RoundsPerTask != 7 || c.AttemptsPerRound != 2 || b.rails(two).RoundsPerTask != 3 {
		t.Errorf("after the raise: %+v, the other task %+v", c, b.rails(two))
	}
	head := b.head()
	for name, c := range map[string]struct {
		p          identity.Principal
		task, rail string
		to         int
	}{
		"to the cap in force":  {b.op, one, graph.RailRounds, 7},
		"to the profile's cap": {b.op, two, graph.RailRounds, 3},
		"below it":             {b.op, one, graph.RailRounds, 6},
		"by a supervisor":      {b.sup, one, graph.RailAttempts, 9},
		"a USD rail":           {b.op, one, "per_day_usd", 9},
		"an unknown task":      {b.op, newID(t, "tsk"), graph.RailRounds, 9},
		"a cap of 1":           {b.op, two, graph.RailAttempts, 1},
	} {
		err := b.raise(c.p, c.task, c.rail, c.to)
		// The logic refuses a raise not above the cap in force as stale, before the fold sees it.
		stale := strings.HasPrefix(name, "to the") || name == "below it"
		if err == nil || b.head() != head || stale && !errors.Is(err, eventstore.ErrStaleExpectedState) {
			t.Errorf("%s: raised (%v)", name, err)
		}
	}
	// A supervisor is refused at the derivation, as operator_only.
	b.view(func(tx *eventstore.Tx) error {
		in := must(json.Marshal(map[string]any{"task": one, "rail": graph.RailAttempts, "to": 9}))
		_, err := authority.TaskRaiseRailHandler{}.Derive(tx, types.Actor{ID: b.sup.Actor(), Kind: types.ActorKindSupervisor}, in)
		var r *authority.Refusal
		if !errors.As(err, &r) || r.Reason != vocab.ReasonOperatorOnly {
			t.Errorf("a supervisor's raise: %v", err)
		}
		return nil
	})
	// The fold, from the log alone: a raise not above the cap in force is
	// refused, the last raise's (7) or, with none, the profile's (3; Astra
	// #295 L2).
	for name, c := range map[string]struct {
		task string
		to   int
	}{"below the last raise": {one, 6}, "to the profile's cap": {two, 3}, "below the profile's cap": {two, 2}} {
		if err := b.trySeed(b.op, raisedEvent(t, c.task, graph.RailRounds, c.to, b.op.Actor())); err == nil || !strings.Contains(err.Error(), "above the cap in force") {
			t.Errorf("the fold took a raise %s: %v", name, err)
		}
	}
	// Nor a supervisor's raise, however high.
	if err := b.trySeed(b.sup, raisedEvent(t, two, graph.RailAttempts, 9, b.sup.Actor())); err == nil || !strings.Contains(err.Error(), "a live operator raises a rail") {
		t.Errorf("the fold took a supervisor's raise: %v", err)
	}
	b.rebuilds()
}

// The fold refuses a raise not above the profile's cap on replay too
// (Astra #295 L2): a log that holds one, appended past a fold that let it
// by, does not rebuild.
func TestRailRaisedReplay(t *testing.T) {
	taskFold = func(tx *eventstore.Tx, evs []eventstore.Event) error {
		if !tx.Replaying() {
			evs = slices.DeleteFunc(slices.Clone(evs), func(e eventstore.Event) bool { return e.Kind == authority.TaskRailRaised })
		}
		return authority.TaskProjector{}.Project(tx, evs)
	}
	t.Cleanup(func() { taskFold = authority.TaskProjector{}.Project })
	b := openTasks(t)
	task := b.newTask(b.op, prjA)
	if err := b.trySeed(b.op, raisedEvent(t, task, graph.RailRounds, 2, b.op.Actor())); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.RebuildProjections(context.Background()); !errors.Is(err, eventstore.ErrReplayRefused) || !strings.Contains(err.Error(), "above the cap in force") {
		t.Errorf("the replay of a raise below the profile's cap: %v", err)
	}
}

// raiseWith submits task.raise_rail by p with in as its scope.
func (b *tasks) raiseWith(p identity.Principal, in map[string]any) error {
	c := p.Command(newID(b.t, "cmd"), authority.TaskRaiseRailAction, newID(b.t, "cmd"), must(json.Marshal(in)), nil)
	_, err := b.s.Submit(context.Background(), c, authority.TaskRaiseRailHandler{}.Logic(c))
	return err
}

// TestTaskRaiseUSDRail (P17b): the operator raises a task's per_task_usd
// (py-mini's $5) with to_usd, for that task only, to micros in force; a
// raise not above it, one with to, one with both, and per_day_usd (the
// project's, a profile change) are refused; the fold replays it.
func TestTaskRaiseUSDRail(t *testing.T) {
	b := openTasks(t)
	b.az.Handlers[authority.TaskRaiseRailAction] = authority.TaskRaiseRailHandler{}
	one, two := b.newTask(b.op, prjA), b.newTask(b.op, prjA)
	if err := b.raiseWith(b.op, map[string]any{"task": one, "rail": graph.RailTaskUSD, "to_usd": "7.50"}); err != nil {
		t.Fatal(err)
	}
	if c := b.rails(one); c.PerTaskMicros != 7_500_000 || c.PerDayMicros != 20_000_000 || b.rails(two).PerTaskMicros != 5_000_000 {
		t.Errorf("after the raise: %+v, the other task %+v", c, b.rails(two))
	}
	head := b.head()
	for name, in := range map[string]map[string]any{
		"to the cap in force": {"task": one, "rail": graph.RailTaskUSD, "to_usd": "7.5"},
		"below it":            {"task": one, "rail": graph.RailTaskUSD, "to_usd": "6"},
		"with to":             {"task": two, "rail": graph.RailTaskUSD, "to": 9},
		"with both":           {"task": two, "rail": graph.RailTaskUSD, "to": 9, "to_usd": "9"},
		"a count rail in USD": {"task": two, "rail": graph.RailRounds, "to_usd": "9"},
		"per_day_usd":         {"task": two, "rail": graph.RailDayUSD, "to_usd": "90"},
	} {
		err := b.raiseWith(b.op, in)
		// A cap that is not its rail's is an invalid scope, never a stale cap.
		invalid := strings.HasPrefix(name, "with") || name == "a count rail in USD" || name == "per_day_usd"
		if err == nil || b.head() != head || invalid != errors.Is(err, authority.ErrInvalidScope) {
			t.Errorf("%s: raised (%v)", name, err)
		}
	}
	b.rebuilds()
}
