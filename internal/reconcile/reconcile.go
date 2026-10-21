// SPDX-License-Identifier: Apache-2.0

// Package reconcile is adcd's reconcile loop (M1 P22a-4; D59): a pure
// decision from a snapshot of the store to typed actions, each run in its
// own command of the system actor, which rechecks there what the decision
// read.
//   - Observe takes a Snapshot in one read transaction: the admitted task,
//     whether its approval is in force, and its open nodes (runnable, or
//     waiting: an evaluation claims a node whose wait has ended); or the task
//     admission takes next. Each subject is read at its version (the seq of
//     its last event).
//   - Decide is pure. Admit the next task while none is admitted, when the
//     snapshot allows admission (the admission capability is active).
//     Abandon the admitted task once its approval is no longer in force
//     (#226 entry 100: it can run nothing more). Evaluate the admitted
//     task's open nodes only when the snapshot allows evaluation (the
//     launch capability is active: until then a claimed node would wait for
//     an attempt nothing can start). Nothing else.
//   - Executor.Run runs one action in one command. The command expects each
//     subject at the version the snapshot read (the writer's expected-state
//     check), and its logic rechecks the authority in its own transaction:
//     AdmitTask the task's Ready state, approval and the admitted slot;
//     AbandonTask the revoked approval and that nothing runs the task's
//     nodes; Evaluate that the task is still the admitted one with its
//     approval in force (authority.Tasks, Opus #309 L5: a refusal the loop
//     reads, never a fold error), then graph.Evaluate, whose claims adcd's
//     fold checks again (I22, I1). A changed subject is refused
//     stale_expected_state, appending nothing; the next pass decides again
//     from a fresh snapshot. An action whose logic emits no event commits
//     nothing (Opus #312 M1): it is not counted, so it never nudges a pass.
//   - Loop runs passes on one goroutine: at start (the resynchronisation of
//     a restart), every Every, and on a Nudge (after a pass that ran an
//     action, or a command adcd applied). A refused action is logged and the
//     pass goes on; nothing it decided carries over to the next pass. A
//     refusal the same as the last one logged is not logged again until
//     another, or a pass with none, comes between (Opus #312 L5). A panic in
//     a pass is recovered and returned as its error (Opus #312 M2): the pass
//     nudges nothing, so the timer paces the retries and a start pass never
//     takes adcd down.
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

// Gates are what the active capabilities let the loop do.
type Gates struct {
	// Admits: the next task may be admitted (admission active).
	Admits bool
	// Evaluates: the admitted task's nodes may be evaluated (launch active).
	Evaluates bool
}

// Snapshot is what Decide reads.
type Snapshot struct {
	Gates
	// Admitted is the admitted task that is not done (ID "": none),
	// Revoked whether its approval is no longer in force, and Open its
	// runnable and waiting nodes: a waiting node's producer may have
	// completed since, and nothing but an evaluation ends its wait.
	Admitted Versioned
	Revoked  bool
	Open     []Versioned
	// Next is the task admission takes next (ID "": none, or one is
	// admitted).
	Next Versioned
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

// Observe is the store's Snapshot in tx under g.
func Observe(tx *eventstore.Tx, g Gates) (Snapshot, error) {
	s := Snapshot{Gates: g}
	admitted, ok, err := authority.AdmittedTask(tx)
	if err != nil {
		return s, err
	}
	if !ok {
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
	nodes, err := graph.TaskNodes(tx, admitted) // the task's index, never the whole graph (Astra #316 M3)
	for _, n := range nodes {
		if err != nil {
			break
		}
		if st := n.State.State; n.Task == admitted && (st == types.NodeStateStateRunnable || st == types.NodeStateStateWaiting) {
			v := Versioned{ID: string(n.Node.ID)}
			v.Version, err = tx.SubjectVersion(v.ID)
			s.Open = append(s.Open, v)
		}
	}
	return s, err
}

// Decide is the actions s calls for, in order.
func Decide(s Snapshot) []Action {
	switch {
	case s.Admitted.ID == "" && s.Admits && s.Next.ID != "":
		return []Action{Admit{Task: s.Next}}
	case s.Admitted.ID != "" && s.Revoked:
		return []Action{Abandon{Task: s.Admitted}}
	case s.Admitted.ID != "" && s.Evaluates && len(s.Open) > 0:
		return []Action{Evaluate{Task: types.TaskID(s.Admitted.ID), Goals: slices.Clone(s.Open)}}
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

// errNothing is an action whose logic emits no event: its command commits
// nothing.
var errNothing = errors.New("reconcile: the action changes nothing")

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
			if err == nil && len(evs) == 0 {
				err = errNothing
			}
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
	Exec  Executor
	Gates Gates
	// Every is the period between passes (5 s in adcd).
	Every time.Duration
	Log   io.Writer
	nudge chan struct{}
	// last is the refusal logged last; "" after a pass with none.
	last string
}

// NewLoop is a loop over x under g.
func NewLoop(x Executor, g Gates, every time.Duration, log io.Writer) *Loop {
	return &Loop{Exec: x, Gates: g, Every: every, Log: log, nudge: make(chan struct{}, 1)}
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
			l.refused(err.Error())
		} else if ran > 0 && err == nil {
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
// were applied. A refused action is logged (but while ctx ends), a stale
// one or one that changes nothing silently: the next pass decides again. A
// panic in the observation or an action is recovered as the pass's error.
func (l *Loop) Pass(ctx context.Context) (ran int, err error) {
	var s Snapshot
	if err := recovered("the observation", func() error {
		return l.Exec.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
			s, err = Observe(tx, l.Gates)
			return err
		})
	}); err != nil {
		return 0, err
	}
	refusals := 0
	for _, a := range Decide(s) {
		var panicked *panicError
		switch err := recovered(a.action(), func() error { return l.Exec.Run(ctx, a) }); {
		case err == nil:
			ran++
		case errors.As(err, &panicked):
			return ran, err
		case errors.Is(err, eventstore.ErrStaleExpectedState), errors.Is(err, errNothing), ctx.Err() != nil:
		default:
			refusals++
			l.refused(fmt.Sprintf("%s refused: %v", a.action(), err))
		}
	}
	if refusals == 0 {
		l.last = ""
	}
	return ran, nil
}

// refused logs msg, unless it is the refusal logged last.
func (l *Loop) refused(msg string) {
	if msg != l.last {
		fmt.Fprintf(l.Log, "adcd: reconcile: %s\n", msg)
	}
	l.last = msg
}

// panicError is a panic recovered in a pass.
type panicError struct {
	what  string
	value any
}

func (p *panicError) Error() string { return fmt.Sprintf("%s panicked: %v", p.what, p.value) }

// recovered runs f, returning a panic in it as a panicError.
func recovered(what string, f func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &panicError{what: what, value: v}
		}
	}()
	return f()
}
