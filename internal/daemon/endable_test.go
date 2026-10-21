// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// rawDone is task's task.done as a candidate, built by hand: the fold
// alone decides it (append; replay runs the same fold).
func rawDone(task types.TaskID, project types.ProjectID) func(*eventstore.Tx) ([]eventstore.Event, error) {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		id, err := ids.New("evt")
		b, merr := json.Marshal(types.TaskDone{Task: task, Project: project, As: types.TaskDoneAsCandidate,
			At: types.Time(tx.Now().Format(time.RFC3339Nano))})
		p := string(project)
		return []eventstore.Event{{ID: id, Kind: authority.TaskDoneEvent, Subject: string(task), Project: &p, Payload: b}}, errors.Join(err, merr)
	}
}

// planResult commits plan's completed result, recorded with no attempt.
func planResult(t *testing.T, w *seeder, sys types.ActorID, plan types.NodeID) error {
	t.Helper()
	return w.try(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		nodes, err := graph.Nodes(tx)
		var key types.ComputationKey
		for _, n := range nodes {
			if err == nil && n.Node.ID == plan {
				key, err = graph.Key(n.Node, map[string]graph.Resolved{}, 1)
			}
		}
		if err != nil {
			return nil, err
		}
		r := types.Result{ID: types.ResultID(newID(t, "res")), Computation: key.Key, Kind: types.ResultKindPlan, Value: []types.Ref{},
			Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
		none := func(types.Node, types.InputSpec) (*receipt.Requirement, error) { return nil, nil }
		return nil, graph.Commit(tx, plan, r, graph.Env{Need: none, Authority: authority.Answers{}})
	})
}

// Astra #316 H1, through adcd's own fold: task A admitted, its plan
// claimed and an attempt started on it. While the attempt runs, a result
// recorded with no attempt is refused (a node an attempt runs records that
// attempt's result alone). Once the attempt is lost (not sealed, its lease
// still active), such a result completes the plan: no node of A is claimed
// or held, yet A is not done as a candidate, by the command (DoneTask) or
// by a raw task.done (the fold, on append and so on replay), while its
// lease is active, nor once the lease settled while the lost attempt is
// not sealed; once sealed, it is, and the projections rebuild unchanged.
func TestDoneWaitsForEveryAttemptAndLease(t *testing.T) {
	w, sys := withSystem(t)
	_, task := admittedTask(t, w, sys)
	plan := templateNodes(w, task)[types.NodeKindPlan]
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, plan, map[string]graph.Resolved{}, 1)
	})
	ls := w.lease(4)
	ls.Unit = graph.UnitOf(plan)
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, ls) })
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, ls.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
	})
	attempt := types.AttemptID(newID(t, "att"))
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, ls.ID, 2, attempt, types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"})
	})
	if err := planResult(t, w, sys, plan); err == nil || !strings.Contains(err.Error(), "is running") {
		t.Fatalf("a result with no attempt while the attempt runs: %v", err)
	}
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.LoseRunning(tx, attempt, types.AttemptLostReasonValueControllerRestart)
	})
	if err := planResult(t, w, sys, plan); err != nil {
		t.Fatalf("the plan's result once its attempt is lost: %v", err)
	}
	var project types.ProjectID
	w.view(func(tx *eventstore.Tx) (err error) { project, _, err = graph.TaskProject(tx, task); return err })
	refused := func(why string) {
		t.Helper()
		for name, err := range map[string]error{
			"the command": w.try(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.DoneTask(tx, task) }),
			"a raw done":  w.try(sys, rawDone(task, project)),
		} {
			if err == nil || !strings.Contains(err.Error(), "still runs") {
				t.Errorf("%s %s: %v", name, why, err)
			}
		}
	}
	refused("while the lease is active")
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, ls.ID, 3, lease.LeaseEvent{Kind: lease.Revoked, Reason: "authority_revoked"}, nil)
	})
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		st, err := capacity.Settlement(tx, ls.ID)
		if err != nil {
			return nil, err
		}
		return lease.ChangeLease(tx, ls.ID, 4, lease.LeaseEvent{Kind: lease.Settled, Settlement: &st}, nil)
	})
	refused("while the lost attempt is not sealed, its lease settled")
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.SealMinimal(tx, attempt, 2, graph.Units{Tasks: authority.Tasks{}}, w.blobs, nil, sys)
	})
	if err := w.try(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.DoneTask(tx, task) }); err != nil {
		t.Fatalf("the done once nothing runs: %v", err)
	}
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// Astra #316 M3: admission and a task's end read indexes, never the
// history: admitting the next task and ending it read the same number of
// projection rows whether 2 or 12 tasks were admitted and done before
// (counted, never timed).
func TestAdmissionWorkIsBounded(t *testing.T) {
	work := func(retained int) int64 {
		w, sys := withSystem(t)
		for range retained {
			_, task := admittedTask(t, w, sys)
			w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.DoneTask(tx, task) })
		}
		_, next := readyTask(t, w, sys)
		before := eventstore.ProjectionRowsRead()
		w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.Admit(tx, nodeKinds()) })
		w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.DoneTask(tx, next) })
		return eventstore.ProjectionRowsRead() - before
	}
	if few, many := work(2), work(12); few != many {
		t.Errorf("rows read to admit and end a task: %d after 2 tasks, %d after 12", few, many)
	}
}
