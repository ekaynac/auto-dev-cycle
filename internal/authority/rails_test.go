// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
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
	ids := map[string]string{}
	for _, key := range []string{"one", "two"} {
		d := b.upload(b.op, "def test_"+key+"(): pass\n", suiteNow())
		if _, _, err := b.create(b.op, key, spec(prjA, "task "+key, "hidden/test.py", d)); err != nil {
			t.Fatal(err)
		}
		ids[key] = b.events(2)[0].Subject
	}
	one, two := ids["one"], ids["two"]
	if c := b.rails(one); c != (graph.Caps{AttemptsPerRound: 3, RoundsPerTask: 5, MergeOperationsPerGeneration: 2}) {
		t.Fatalf("the profile's caps: %+v", c)
	}
	if err := b.raise(b.op, one, graph.RailRounds, 7); err != nil {
		t.Fatal(err)
	}
	if c := b.rails(one); c.RoundsPerTask != 7 || c.AttemptsPerRound != 3 || b.rails(two).RoundsPerTask != 5 {
		t.Errorf("after the raise: %+v, the other task %+v", c, b.rails(two))
	}
	head := b.head()
	for name, c := range map[string]struct {
		p          identity.Principal
		task, rail string
		to         int
	}{
		"to the cap in force":  {b.op, one, graph.RailRounds, 7},
		"to the profile's cap": {b.op, two, graph.RailRounds, 5},
		"below it":             {b.op, one, graph.RailRounds, 6},
		"by a supervisor":      {b.sup, one, graph.RailAttempts, 9},
		"a USD rail":           {b.op, one, "per_day_usd", 9},
		"an unknown task":      {b.op, newID(t, "tsk"), graph.RailRounds, 9},
		"a cap of 1":           {b.op, two, graph.RailAttempts, 1},
	} {
		if err := b.raise(c.p, c.task, c.rail, c.to); err == nil || b.head() != head {
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
	// The fold, from the log alone: a raise not above the last is refused.
	lower := func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r := types.RailRaised{Task: types.TaskID(one), Project: prjA, Rail: graph.RailRounds, To: 6, By: b.op.Actor(),
			At: types.Time(tx.Now().Format(time.RFC3339Nano))}
		p := string(prjA)
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.TaskRailRaised, Subject: one, Project: &p, Payload: must(json.Marshal(r))}}, nil
	}
	if err := b.trySeed(b.op, lower); err == nil {
		t.Error("the fold took a lower raise")
	}
	// Nor a supervisor's raise, however high.
	bySup := func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r := types.RailRaised{Task: types.TaskID(two), Project: prjA, Rail: graph.RailAttempts, To: 9, By: b.sup.Actor(),
			At: types.Time(tx.Now().Format(time.RFC3339Nano))}
		p := string(prjA)
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.TaskRailRaised, Subject: two, Project: &p, Payload: must(json.Marshal(r))}}, nil
	}
	if err := b.trySeed(b.sup, bySup); err == nil || !strings.Contains(err.Error(), "a live operator raises a rail") {
		t.Errorf("the fold took a supervisor's raise: %v", err)
	}
	b.rebuilds()
}
