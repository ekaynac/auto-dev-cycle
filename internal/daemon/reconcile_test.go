// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
)

// reconciled is a seeder over adcd's projector, its system actor and an
// executor running as it, evaluation allowed (launch's, faked).
func reconciled(t *testing.T) (*seeder, types.ActorID, reconcile.Executor) {
	t.Helper()
	w := seed(t, stateDir(t), daemonProjection(t))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	claim, err := w.s.ClaimSystem()
	if err != nil {
		t.Fatal(err)
	}
	var p identity.Principal
	w.view(func(tx *eventstore.Tx) (err error) {
		p, err = identity.SystemPrincipal(tx, claim, sys.ID)
		return err
	})
	none := func(types.Node, types.InputSpec) (*receipt.Requirement, error) { return nil, nil }
	return w, sys.ID, reconcile.Executor{Store: w.s, System: p, Kinds: nodeKinds(), Env: graph.Env{Authority: authority.Answers{}, Need: none}}
}

// nodesOf are task's nodes by kind, with their states.
func nodesOf(w *seeder, task types.TaskID) map[types.NodeKind]graph.NodeView {
	out := map[types.NodeKind]graph.NodeView{}
	w.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.Nodes(tx)
		for _, n := range nodes {
			if n.Task == task {
				out[n.Node.Kind] = n
			}
		}
		return err
	})
	return out
}

// P22a-4 (W14; D59), on adcd's projector: a pass admits the Ready task with
// its template; the next evaluates its runnable plan, claiming it; the next
// finds nothing to do and appends nothing; an evaluation that changes
// nothing commits no command (Opus #312 M1). An action decided on a snapshot
// the store has moved past is refused stale_expected_state in its own
// command, appending nothing: an evaluation of a node claimed since, and
// an admission while another task holds the slot.
func TestReconcilePasses(t *testing.T) {
	w, sys, x := reconciled(t)
	_, first := readyTask(t, w, sys)
	var log bytes.Buffer
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, &log)
	ctx := context.Background()
	var stale reconcile.Versioned
	w.view(func(tx *eventstore.Tx) (err error) {
		stale.ID = string(first)
		stale.Version, err = tx.SubjectVersion(stale.ID)
		return err
	})
	if ran, err := l.Pass(ctx); err != nil || ran != 1 {
		t.Fatalf("the first pass: %d, %v", ran, err)
	}
	nodes := nodesOf(w, first)
	if len(nodes) != 4 {
		t.Fatalf("the admitted task's template: %v", nodes)
	}
	plan := reconcile.Versioned{ID: string(nodes[types.NodeKindPlan].Node.ID)}
	w.view(func(tx *eventstore.Tx) (err error) {
		plan.Version, err = tx.SubjectVersion(plan.ID)
		return err
	})
	if ran, err := l.Pass(ctx); err != nil || ran != 1 || nodesOf(w, first)[types.NodeKindPlan].State.State != types.NodeStateStateClaimed {
		t.Fatalf("the second pass: %d, %v; the plan %v", ran, err, nodesOf(w, first)[types.NodeKindPlan].State)
	}
	head := w.head()
	if ran, err := l.Pass(ctx); err != nil || ran != 0 || w.head() != head {
		t.Fatalf("a pass with nothing to do: %d, %v, head %d → %d", ran, err, head, w.head())
	}
	// Opus #312 M1: an evaluation that emits nothing (its goal, at its
	// current version, is claimed) commits nothing, not even its command.
	claimed := reconcile.Versioned{ID: plan.ID}
	w.view(func(tx *eventstore.Tx) (err error) {
		claimed.Version, err = tx.SubjectVersion(claimed.ID)
		return err
	})
	commands := count(t, w.dir, `SELECT count(*) FROM commands`)
	if err := x.Run(ctx, reconcile.Evaluate{Task: first, Goals: []reconcile.Versioned{claimed}}); err == nil || errors.Is(err, eventstore.ErrStaleExpectedState) ||
		w.head() != head || count(t, w.dir, `SELECT count(*) FROM commands`) != commands {
		t.Errorf("an evaluation that changes nothing: %v; head %d → %d", err, head, w.head())
	}
	_, second := readyTask(t, w, sys)
	var next reconcile.Versioned
	w.view(func(tx *eventstore.Tx) (err error) {
		next.ID = string(second)
		next.Version, err = tx.SubjectVersion(next.ID)
		return err
	})
	head = w.head()
	for name, a := range map[string]reconcile.Action{
		"an evaluation of a node claimed since":  reconcile.Evaluate{Task: first, Goals: []reconcile.Versioned{plan}},
		"an admission while another is admitted": reconcile.Admit{Task: next},
		"an admission of the admitted task":      reconcile.Admit{Task: stale},
	} {
		if err := x.Run(ctx, a); !errors.Is(err, eventstore.ErrStaleExpectedState) || w.head() != head {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An evaluation naming a task that is not the admitted one, its goal at
	// its current version, is refused: the command rechecks the task.
	build := reconcile.Versioned{ID: string(nodesOf(w, first)[types.NodeKindBuild].Node.ID)}
	w.view(func(tx *eventstore.Tx) (err error) {
		build.Version, err = tx.SubjectVersion(build.ID)
		return err
	})
	head = w.head()
	if err := x.Run(ctx, reconcile.Evaluate{Task: second, Goals: []reconcile.Versioned{build}}); !errors.Is(err, eventstore.ErrStaleExpectedState) || w.head() != head {
		t.Errorf("an evaluation of a task not admitted: %v", err)
	}
	if log.Len() != 0 {
		t.Errorf("the passes logged %q", log.String())
	}
}

// The loop resynchronises at start: a task left in Ready while another was
// admitted is admitted by Run's first pass once the slot is free (its
// task.done), with no tick (the period an hour); a Nudge runs a pass, and
// Run returns when its context ends.
func TestReconcileLoopRuns(t *testing.T) {
	w, sys, x := reconciled(t)
	_, first := readyTask(t, w, sys)
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.Admit(tx, nodeKinds()) })
	_, second := readyTask(t, w, sys)
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.DoneTask(tx, first) })
	var log bytes.Buffer
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true}, time.Hour, &log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx) }()
	admitted := func(task types.TaskID) bool {
		var got types.TaskID
		w.view(func(tx *eventstore.Tx) (err error) { got, _, err = authority.AdmittedTask(tx); return err })
		return got == task
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	waitFor("the start's pass to admit the waiting task", func() bool { return admitted(second) })
	_, third := readyTask(t, w, sys)
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.DoneTask(tx, second) })
	l.Nudge()
	waitFor("a nudged pass to admit the next task", func() bool { return admitted(third) })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	if len(nodesOf(w, third)) != 4 || nodesOf(w, third)[types.NodeKindPlan].State.State != types.NodeStateStateRunnable {
		t.Errorf("evaluation off: the admitted task's nodes %v", nodesOf(w, third))
	}
	if log.Len() != 0 {
		t.Errorf("the loop logged %q", log.String())
	}
}

// Opus #312 L2: with evaluation on and the period an hour, the start's
// pass admits the Ready task and the loop's own nudge runs the pass that
// evaluates its plan, claiming it, with no nudge from outside.
func TestReconcileLoopEvaluatesAfterAdmission(t *testing.T) {
	w, sys, x := reconciled(t)
	_, task := readyTask(t, w, sys)
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	for deadline := time.Now().Add(10 * time.Second); nodesOf(w, task)[types.NodeKindPlan].State.State != types.NodeStateStateClaimed; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the plan after admission: %v", nodesOf(w, task)[types.NodeKindPlan].State)
		}
	}
}

// A waiting node is evaluated too: once the admitted task's plan, claimed
// by a pass, completes, the next pass claims its build, waiting on it until
// then; nothing but an evaluation ends a wait.
func TestReconcileEndsWaits(t *testing.T) {
	w, sys, x := reconciled(t)
	_, task := readyTask(t, w, sys)
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	ctx := context.Background()
	for _, pass := range []string{"the admission", "the plan's claim"} {
		if ran, err := l.Pass(ctx); err != nil || ran != 1 {
			t.Fatalf("%s: %d, %v", pass, ran, err)
		}
	}
	if s := nodesOf(w, task)[types.NodeKindBuild].State.State; s != types.NodeStateStateWaiting {
		t.Fatalf("the build before the plan completes: %s", s)
	}
	completePlan(t, w, sys, nodesOf(w, task)[types.NodeKindPlan].Node.ID)
	if ran, err := l.Pass(ctx); err != nil || ran != 1 || nodesOf(w, task)[types.NodeKindBuild].State.State != types.NodeStateStateClaimed {
		t.Fatalf("the pass after the plan completed: %d, %v; the build %v", ran, err, nodesOf(w, task)[types.NodeKindBuild].State)
	}
}

// Opus #312 M2: a panic in an action is recovered. The task admitted and
// its plan completed, the build's evaluation reads an Env whose Need
// panics: Pass returns the panic as its error, appending nothing; Run logs
// it once however often it repeats, and returns when its context ends.
func TestReconcilePanicRecovered(t *testing.T) {
	w, sys, x := reconciled(t)
	_, task := readyTask(t, w, sys)
	ctx := context.Background()
	gates := reconcile.Gates{Admits: true}
	if ran, err := reconcile.NewLoop(x, gates, time.Hour, io.Discard).Pass(ctx); err != nil || ran != 1 {
		t.Fatalf("the admission: %d, %v", ran, err)
	}
	plan := nodesOf(w, task)[types.NodeKindPlan].Node.ID
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, plan, map[string]graph.Resolved{}, 1)
	})
	completePlan(t, w, sys, plan)
	x.Env.Need = func(types.Node, types.InputSpec) (*receipt.Requirement, error) { panic("a need that panics") }
	gates.Evaluates = true
	head := w.head()
	if ran, err := reconcile.NewLoop(x, gates, time.Hour, io.Discard).Pass(ctx); err == nil || !strings.Contains(err.Error(), "reconcile.evaluate panicked: a need that panics") ||
		ran != 0 || w.head() != head {
		t.Fatalf("a pass that panics: %d, %v", ran, err)
	}
	log := &logBuffer{}
	l := reconcile.NewLoop(x, gates, 20*time.Millisecond, log)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); l.Run(rctx) }()
	time.Sleep(200 * time.Millisecond) // several passes, each panicking
	cancel()
	<-done
	if got := log.String(); strings.Count(got, "panicked") != 1 || w.head() != head {
		t.Errorf("the loop over a panicking pass logged %q; head %d → %d", got, head, w.head())
	}
}

// #226 entry 100 and Opus #309 L5: once the admitted task's approval is
// revoked (its operator revoked), an evaluation of its runnable plan is
// refused authority_revoked by the command's own check, appending nothing;
// the next pass abandons the task (task.done, abandoned) and the slot
// frees; a pass after it finds nothing (the operator's other tasks were
// revoked with it).
func TestReconcileAbandons(t *testing.T) {
	w, sys, x := reconciled(t)
	op, task := readyTask(t, w, sys)
	readyTask(t, w, sys)
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	ctx := context.Background()
	if ran, err := l.Pass(ctx); err != nil || ran != 1 {
		t.Fatalf("the admitting pass: %d, %v", ran, err)
	}
	plan := reconcile.Versioned{ID: string(nodesOf(w, task)[types.NodeKindPlan].Node.ID)}
	w.view(func(tx *eventstore.Tx) (err error) {
		plan.Version, err = tx.SubjectVersion(plan.ID)
		return err
	})
	w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, op, time.Now()) })
	head := w.head()
	var revoked graph.ErrRevalidated
	// Refused by the command's own check, before any claim is built: not an
	// error of the fold's (Opus #309 L5).
	if err := x.Run(ctx, reconcile.Evaluate{Task: task, Goals: []reconcile.Versioned{plan}}); !errors.As(err, &revoked) || strings.Contains(err.Error(), graph.NodeClaimed) || w.head() != head {
		t.Errorf("an evaluation of a task whose approval is revoked: %v", err)
	}
	if ran, err := l.Pass(ctx); err != nil || ran != 1 {
		t.Fatalf("the abandoning pass: %d, %v", ran, err)
	}
	var r authority.TaskRecord
	w.view(func(tx *eventstore.Tx) error {
		raw, _, err := authority.TaskRecordOf(task)(tx)
		if err == nil {
			err = json.Unmarshal(raw, &r)
		}
		return err
	})
	if r.State != authority.TaskDoneState || r.DoneAs != string(types.TaskDoneAsAbandoned) {
		t.Fatalf("the abandoned task: %s %s", r.State, r.DoneAs)
	}
	head = w.head()
	if ran, err := l.Pass(ctx); err != nil || ran != 0 || w.head() != head {
		t.Errorf("a pass after the abandonment: %d, %v", ran, err)
	}
}
