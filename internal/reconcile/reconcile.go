// SPDX-License-Identifier: Apache-2.0

// Package reconcile is adcd's reconcile loop (M1 P22a-4; D59): a pure
// decision from a snapshot of the store to typed actions, each run in its
// own command of the system actor, which rechecks there what the decision
// read.
//   - Observe takes a Snapshot in one read transaction: the admitted task,
//     whether its approval is in force, and its runnable nodes; or the task
//     admission takes next. Each subject is read at its version (the seq of
//     its last event).
//   - Decide is pure. Admit the next task while none is admitted. Abandon
//     the admitted task once its approval is no longer in force (#226 entry
//     100: it can run nothing more). Evaluate the admitted task's runnable
//     nodes only when the snapshot allows evaluation (the launch capability
//     is active: until then a claimed node would wait for an attempt
//     nothing can start). Nothing else.
//   - Executor.Run runs one action in one command. The command expects each
//     subject at the version the snapshot read (the writer's expected-state
//     check), and its logic rechecks the authority in its own transaction:
//     AdmitTask the task's Ready state, approval and the admitted slot;
//     AbandonTask the revoked approval and that no lease of the task is
//     live; Evaluate that the task is still the admitted one with its
//     approval in force (authority.Tasks, Opus #309 L5: a refusal the loop
//     reads, never a fold error), then graph.Evaluate, whose claims adcd's
//     fold checks again (I22, I1). A changed subject is refused
//     stale_expected_state, appending nothing; the next pass decides again
//     from a fresh snapshot.
//   - Loop runs passes on one goroutine: at start (the resynchronisation of
//     a restart), every Every, and on a Nudge (after a pass that ran an
//     action, or a command adcd applied). A refused action is logged and the
//     pass goes on; nothing it decided carries over to the next pass.
package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// The system actions the loop's commands record (none is a registered
// action a principal may ask for).
const (
	AdmitAction    = "reconcile.admit"
	AbandonAction  = "reconcile.abandon"
	EvaluateAction = "reconcile.evaluate"
)

// Actions are the loop's system actions.
var Actions = []string{AdmitAction, AbandonAction, EvaluateAction}

// Versioned is a subject read at a version: the seq of its last event.
type Versioned struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// Snapshot is what Decide reads.
type Snapshot struct {
	// Admitted is the admitted task that is not done (ID "": none),
	// Revoked whether its approval is no longer in force, and Runnable its
	// runnable nodes.
	Admitted Versioned
	Revoked  bool
	Runnable []Versioned
	// Next is the task admission takes next (ID "": none, or one is
	// admitted).
	Next Versioned
	// Evaluates: the admitted task's nodes may be evaluated (launch active).
	Evaluates bool
}

// Action is one decision: Admit, Abandon or Evaluate.
type Action interface {
	action() string
	expected() []eventstore.Expectation
}

// Admit admits Task, read at its version, with its candidate template.
type Admit struct{ Task Versioned }

// Abandon ends Task, read at its version, abandoned: its approval is not in
// force.
type Abandon struct{ Task Versioned }

// Evaluate evaluates Task's Goals, each read at its version.
type Evaluate struct {
	Task  types.TaskID
	Goals []Versioned
}

func (Admit) action() string    { return AdmitAction }
func (Abandon) action() string  { return AbandonAction }
func (Evaluate) action() string { return EvaluateAction }

func (a Admit) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: a.Task.ID, Version: a.Task.Version}}
}

func (a Abandon) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: a.Task.ID, Version: a.Task.Version}}
}

func (e Evaluate) expected() []eventstore.Expectation {
	out := make([]eventstore.Expectation, len(e.Goals))
	for i, g := range e.Goals {
		out[i] = eventstore.Expectation{Subject: g.ID, Version: g.Version}
	}
	return out
}

// Observe is the store's Snapshot in tx; evaluates is the snapshot's
// Evaluates.
func Observe(tx *eventstore.Tx, evaluates bool) (Snapshot, error) {
	s := Snapshot{Evaluates: evaluates}
	admitted, ok, err := authority.AdmittedTask(tx)
	if err != nil || !ok {
		next, found, err := authority.NextAdmission(tx)
		if err == nil && found {
			s.Next.ID = string(next)
			s.Next.Version, err = tx.SubjectVersion(string(next))
		}
		return s, err
	}
	s.Admitted.ID = string(admitted)
	if s.Admitted.Version, err = tx.SubjectVersion(s.Admitted.ID); err != nil {
		return s, err
	}
	var revoked graph.ErrRevalidated
	if err := (authority.Tasks{}).Claimable(tx, admitted); errors.As(err, &revoked) {
		s.Revoked = true
	} else if err != nil {
		return s, err
	}
	nodes, err := graph.Nodes(tx)
	for _, n := range nodes {
		if err != nil {
			break
		}
		if n.Task == admitted && n.State.State == types.NodeStateStateRunnable {
			v := Versioned{ID: string(n.Node.ID)}
			v.Version, err = tx.SubjectVersion(v.ID)
			s.Runnable = append(s.Runnable, v)
		}
	}
	return s, err
}

// Decide is the actions s calls for, in order.
func Decide(s Snapshot) []Action {
	switch {
	case s.Admitted.ID == "" && s.Next.ID != "":
		return []Action{Admit{Task: s.Next}}
	case s.Admitted.ID != "" && s.Revoked:
		return []Action{Abandon{Task: s.Admitted}}
	case s.Admitted.ID != "" && s.Evaluates && len(s.Runnable) > 0:
		return []Action{Evaluate{Task: types.TaskID(s.Admitted.ID), Goals: slices.Clone(s.Runnable)}}
	}
	return nil
}

// Executor runs actions as the system actor.
type Executor struct {
	Store  *eventstore.Store
	System identity.Principal
	// Kinds are the node kinds a template may hold (adcd's).
	Kinds []types.NodeKind
	// Env is what evaluation reads.
	Env graph.Env
}

// Run runs a in one command; stale_expected_state when a subject it read
// has changed.
func (x Executor) Run(ctx context.Context, a Action) error {
	cmd, err := ids.New("cmd")
	input, merr := json.Marshal(a)
	if err = errors.Join(err, merr); err != nil {
		return err
	}
	var logic func(*eventstore.Tx) ([]eventstore.Event, error)
	switch a := a.(type) {
	case Admit:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return authority.AdmitTask(tx, types.TaskID(a.Task.ID), x.Kinds)
		}
	case Abandon:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return authority.AbandonTask(tx, types.TaskID(a.Task.ID))
		}
	case Evaluate:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) { return x.evaluate(tx, a) }
	default:
		return fmt.Errorf("reconcile: no action %T", a)
	}
	_, err = x.Store.SubmitApplied(ctx, x.System.Command(cmd, a.action(), cmd, input, a.expected()), eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := logic(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

// evaluate is the evaluation of a's goals, while a's task is the admitted
// one with its approval in force (a refusal, authority_revoked, rather than
// an error of the fold's); adcd's fold rechecks each claim's task.
func (x Executor) evaluate(tx *eventstore.Tx, a Evaluate) ([]eventstore.Event, error) {
	admitted, ok, err := authority.AdmittedTask(tx)
	if err != nil || !ok || admitted != a.Task {
		return nil, errors.Join(err, fmt.Errorf("reconcile: %w: task %s is no longer the admitted one", eventstore.ErrStaleExpectedState, a.Task))
	}
	if err := (authority.Tasks{}).Claimable(tx, a.Task); err != nil {
		return nil, err
	}
	goals := make([]types.NodeID, len(a.Goals))
	for i, g := range a.Goals {
		goals[i] = types.NodeID(g.ID)
	}
	return graph.Evaluate(tx, goals, x.Env)
}

// Loop runs reconcile passes on one goroutine.
type Loop struct {
	Exec Executor
	// Evaluates is each snapshot's Evaluates.
	Evaluates bool
	// Every is the period between passes (5 s in adcd).
	Every time.Duration
	Log   io.Writer
	nudge chan struct{}
}

// NewLoop is a loop over x.
func NewLoop(x Executor, evaluates bool, every time.Duration, log io.Writer) *Loop {
	return &Loop{Exec: x, Evaluates: evaluates, Every: every, Log: log, nudge: make(chan struct{}, 1)}
}

// Nudge asks for a pass now; it never blocks.
func (l *Loop) Nudge() {
	select {
	case l.nudge <- struct{}{}:
	default:
	}
}

// Run runs a pass now (the resynchronisation at start), then one every
// Every and on each Nudge, until ctx ends.
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(l.Every)
	defer t.Stop()
	for {
		if ran, err := l.Pass(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintf(l.Log, "adcd: reconcile: %v\n", err)
		} else if ran > 0 {
			l.Nudge() // what it changed may call for more
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.nudge:
		}
	}
}

// Pass observes the store, decides and runs each action; ran is how many
// were applied. A refused action is logged, a stale one silently: the next
// pass decides again.
func (l *Loop) Pass(ctx context.Context) (ran int, err error) {
	var s Snapshot
	if err := l.Exec.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
		s, err = Observe(tx, l.Evaluates)
		return err
	}); err != nil {
		return 0, err
	}
	for _, a := range Decide(s) {
		switch err := l.Exec.Run(ctx, a); {
		case err == nil:
			ran++
		case errors.Is(err, eventstore.ErrStaleExpectedState):
		default:
			fmt.Fprintf(l.Log, "adcd: reconcile: %s refused: %v\n", a.action(), err)
		}
	}
	return ran, nil
}
